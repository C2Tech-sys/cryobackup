package auth

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Own-account operations (architecture §5.2 /me, screen S38). One
// implementation behind PATCH /api/v1/me and the /account page.

// Password policy: the CLI's (`cryo-console user add --password-stdin`) at
// least 12 characters, hashed with the unchanged argon2id parameters; at most
// 256 so a pasted file cannot become the argon2 input.
const (
	MinPasswordLen = 12
	maxPasswordLen = 256
)

var (
	// ErrPasswordPolicy: the new password is shorter than MinPasswordLen or too long.
	ErrPasswordPolicy = errors.New("auth: the password must be 12 to 256 characters")
	// ErrBadPassword: the current password given to a password change is wrong.
	ErrBadPassword = errors.New("auth: the current password is not correct")
	// ErrBadEmail: not an e-mail address.
	ErrBadEmail = errors.New("auth: not an e-mail address")
	// ErrBadUsername: a new username that does not match the username rule.
	ErrBadUsername = errors.New("auth: the username may use letters, digits, dot, dash, underscore and @, 1 to 100 characters")
	// ErrBadDisplayName: a display name that is too long or has control, format or
	// line-separator characters.
	ErrBadDisplayName = errors.New("auth: the display name may be at most 100 characters and may not contain control or invisible characters")
	// ErrDisplayNameTaken: the display name is another account's username.
	ErrDisplayNameTaken = errors.New("auth: the display name is another account's username")
)

// maxDisplayNameRunes bounds a display name.
const maxDisplayNameRunes = 100

// validDisplayName reports whether s (already trimmed) is a usable display
// name: empty (cleared), or at most 100 characters of valid UTF-8 with none of
// the "other" classes (control, format, surrogate, private use: that keeps out
// zero-width and bidi-override characters, which would let a name impersonate
// another one on screen) and no line or paragraph separators.
func validDisplayName(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxDisplayNameRunes {
		return false
	}
	for _, r := range s {
		if unicode.In(r, unicode.C, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

// ValidatePassword applies the password policy.
func ValidatePassword(pw string) error {
	if n := utf8.RuneCountInString(pw); n < MinPasswordLen || len(pw) > maxPasswordLen {
		return ErrPasswordPolicy
	}
	return nil
}

// ValidEmail reports whether s is a bare e-mail address (no display name).
func ValidEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && len(s) <= 254
}

// SelfUpdate is a change to the principal's own account: a new password
// (with the current one), a new e-mail, a new username and/or a display name
// (an empty one clears it). nil leaves a field alone.
type SelfUpdate struct {
	CurrentPassword string
	NewPassword     *string
	Email           *string
	Username        *string
	DisplayName     *string
}

// UpdateSelf applies a SelfUpdate all or nothing (PATCH /api/v1/me): every
// field is validated first (e-mail format, username rule, display name, password
// policy, then the current password, where a wrong one counts as a failed
// login: audit row, failure counter, lockout); only then does one transaction
// apply them. A new password ends every other session of the account and
// revokes all its API tokens; the current session stays. A new username ends
// nothing: sessions and tokens are keyed by the account id, and the old name
// simply stops signing in. A username already taken (ignoring case) is
// store.ErrConflict. Each change writes its own audit row.
func (m *Manager) UpdateSelf(ctx context.Context, p *Principal, u SelfUpdate, method, ip string) error {
	if p == nil || p.Pending {
		return ErrNoSession
	}
	if p.ViaToken() {
		return ErrSessionRequired
	}
	if u.NewPassword == nil && u.Email == nil && u.Username == nil && u.DisplayName == nil {
		return nil
	}
	email := ""
	if u.Email != nil {
		email = strings.TrimSpace(*u.Email)
		if !ValidEmail(email) {
			return ErrBadEmail
		}
	}
	username, display := "", ""
	if u.Username != nil {
		username = strings.TrimSpace(*u.Username)
		if !usernameRE.MatchString(username) {
			return ErrBadUsername
		}
	}
	if u.DisplayName != nil {
		display = strings.TrimSpace(*u.DisplayName)
		if !validDisplayName(display) {
			return ErrBadDisplayName
		}
	}
	hash := ""
	if u.NewPassword != nil {
		if err := ValidatePassword(*u.NewPassword); err != nil {
			return err
		}
		if ok, wait := m.loginLimit.Allow(ip); !ok {
			m.audit(ctx, "admin.password_change", "denied", "warning", ip, &p.Admin, "rate_limited")
			return &RateLimitedError{RetryAfter: wait}
		}
		a, err := m.Store.GetAdmin(ctx, p.Admin.ID)
		if err != nil {
			return err
		}
		if ok, _ := VerifyPassword(a.PasswordHash, u.CurrentPassword); !ok {
			if a.PasswordHash == "" || a.PasswordHash == "!" {
				BurnPassword(u.CurrentPassword)
			}
			_ = m.Store.Write(ctx, func(tx *store.Tx) error {
				if _, err := tx.RecordLoginFailure(a.ID, m.LockAfter, m.LockFor); err != nil {
					return err
				}
				return auditTx(tx, ActorOf(p, method, ip), "admin.password_change", "admin", a.ID, "denied", "warning", map[string]any{"reason": "bad_password"})
			})
			return ErrBadPassword
		}
		if hash, err = HashPassword(*u.NewPassword); err != nil {
			return err
		}
	}
	id := p.Admin.ID
	return m.Store.Write(ctx, func(tx *store.Tx) error {
		if hash != "" {
			if err := tx.UpdateAdminPassword(id, hash, false); err != nil {
				return err
			}
			n, err := tx.DeleteAdminSessions(id, p.SessionID)
			if err != nil {
				return err
			}
			// A password change is the "I may be compromised" action: the account's
			// API tokens go with its other sessions.
			k, err := tx.RevokeAdminAPITokens(id)
			if err != nil {
				return err
			}
			if err := auditTx(tx, ActorOf(p, method, ip), "admin.password_change", "admin", id, "ok", "info", map[string]any{"sessions_ended": n, "tokens_revoked": k}); err != nil {
				return err
			}
		}
		if u.Email != nil {
			if err := tx.UpdateAdminEmail(id, email); err != nil {
				return err
			}
			if err := auditTx(tx, ActorOf(p, method, ip), "admin.email_change", "admin", id, "ok", "info", nil); err != nil {
				return err
			}
		}
		if u.Username != nil || u.DisplayName != nil {
			// An unset field keeps the stored value, read inside this transaction.
			cur, err := tx.GetAdmin(id)
			if err != nil {
				return err
			}
			newName, newDisplay := cur.Username, cur.DisplayName
			if u.Username != nil {
				newName = username
			}
			if u.DisplayName != nil {
				newDisplay = display
			}
			nameChanged, displayChanged := newName != cur.Username, newDisplay != cur.DisplayName
			if displayChanged && newDisplay != "" {
				// A display name that is another account's username would read as
				// that account in the shell. The account's own username is fine.
				other, err := tx.GetAdminByUsername(newDisplay)
				switch {
				case err == nil && other.ID != id:
					return ErrDisplayNameTaken
				case err != nil && !errors.Is(err, store.ErrNotFound):
					return err
				}
			}
			if nameChanged || displayChanged {
				if err := tx.UpdateAdminProfile(id, newName, newDisplay); err != nil {
					return err
				}
			}
			// Usernames are not PHI; the display name's text is kept out of the
			// audit row all the same (it only says whether one is set).
			if nameChanged {
				if err := auditTx(tx, ActorOf(p, method, ip), "admin.username_change", "admin", id, "ok", "info", map[string]any{"from": cur.Username, "to": newName}); err != nil {
					return err
				}
			}
			if displayChanged {
				if err := auditTx(tx, ActorOf(p, method, ip), "admin.display_name_change", "admin", id, "ok", "info", map[string]any{"set": newDisplay != ""}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ChangePassword changes the principal's own password (UpdateSelf with a new
// password only).
func (m *Manager) ChangePassword(ctx context.Context, p *Principal, current, next, method, ip string) error {
	return m.UpdateSelf(ctx, p, SelfUpdate{CurrentPassword: current, NewPassword: &next}, method, ip)
}

// ChangeEmail sets the principal's own e-mail address (UpdateSelf with an
// e-mail only).
func (m *Manager) ChangeEmail(ctx context.Context, p *Principal, email, method, ip string) error {
	return m.UpdateSelf(ctx, p, SelfUpdate{Email: &email}, method, ip)
}

// ChangeProfile sets the principal's own username and display name
// (UpdateSelf with those two only; an empty display name clears it).
func (m *Manager) ChangeProfile(ctx context.Context, p *Principal, username, displayName, method, ip string) error {
	return m.UpdateSelf(ctx, p, SelfUpdate{Username: &username, DisplayName: &displayName}, method, ip)
}

// ListSessions lists the account's signed-in sessions.
func (m *Manager) ListSessions(ctx context.Context, adminID string) ([]store.Session, error) {
	var out []store.Session
	err := m.Store.Read(ctx, func(tx *store.Tx) error {
		var err error
		out, err = tx.ListAdminSessions(adminID)
		return err
	})
	return out, err
}

// RevokeSession ends one of the principal's own sessions (the current one
// included: that is a sign-out). Another account's session is ErrNotFound.
func (m *Manager) RevokeSession(ctx context.Context, p *Principal, id, method, ip string) error {
	if p == nil || p.Pending {
		return ErrNoSession
	}
	if p.ViaToken() {
		return ErrSessionRequired
	}
	err := m.Store.Write(ctx, func(tx *store.Tx) error {
		if err := tx.RevokeAdminSession(p.Admin.ID, id); err != nil {
			return err
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.session.revoke", "admin_session", id, "ok", "info", nil)
	})
	if err == nil {
		m.mu.Lock()
		delete(m.touched, id)
		m.mu.Unlock()
	}
	return err
}
