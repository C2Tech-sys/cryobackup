package auth

import (
	"context"
	"errors"
	"net/mail"
	"strings"
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
)

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
// (with the current one) and/or a new e-mail. nil leaves a field alone.
type SelfUpdate struct {
	CurrentPassword string
	NewPassword     *string
	Email           *string
}

// UpdateSelf applies a SelfUpdate all or nothing (PATCH /api/v1/me): every
// field is validated first (e-mail format, password policy, then the current
// password, where a wrong one counts as a failed login: audit row, failure
// counter, lockout); only then does one transaction apply them. A new password
// ends every other session of the account and revokes all its API tokens; the
// current session stays. Each change writes its own audit row.
func (m *Manager) UpdateSelf(ctx context.Context, p *Principal, u SelfUpdate, method, ip string) error {
	if p == nil || p.Pending {
		return ErrNoSession
	}
	if p.ViaToken() {
		return ErrSessionRequired
	}
	if u.NewPassword == nil && u.Email == nil {
		return nil
	}
	email := ""
	if u.Email != nil {
		email = strings.TrimSpace(*u.Email)
		if !ValidEmail(email) {
			return ErrBadEmail
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
