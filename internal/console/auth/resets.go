package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// One-time password-reset links (architecture §5.2 POST /admins/{id}/reset-password).
//
// The link is /login/reset?token=<32 random bytes, base64url>. Only the token's
// SHA-256 is kept, in memory, with the account id and an expiry of 24 hours; a
// new link for the same account replaces the old one, and using a link
// consumes it. Like the restore download tokens (architecture §13), links live
// in memory: a console restart invalidates every outstanding link and the owner
// issues a new one. Access logs record route patterns only, so the token never
// reaches a log. Setting a password through a link ends every session and
// revokes every API token of the account; the next login still needs the TOTP
// step (or enrollment).

// ResetPath is the UI route of the reset form.
const ResetPath = "/login/reset"

// ResetTTL is how long a reset link stays valid.
const ResetTTL = 24 * time.Hour

// ErrResetInvalid: the link is unknown, used, expired or replaced, or its
// account is gone or disabled.
var ErrResetInvalid = errors.New("auth: the reset link is not valid")

// ResetLink is a link shown once to the owner who issued it.
type ResetLink struct {
	Token   string
	Path    string // ResetPath + "?token=" + Token
	Expires time.Time
}

type resetLink struct {
	adminID string
	expires time.Time
}

func (m *Manager) newResetLink(adminID string) ResetLink {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ResetLink{}
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	exp := m.Now().Add(ResetTTL)
	m.mfa.mu.Lock()
	m.mfa.init()
	for h, l := range m.mfa.resets {
		if l.adminID == adminID || m.Now().After(l.expires) {
			delete(m.mfa.resets, h)
		}
	}
	m.mfa.resets[hashToken(tok)] = resetLink{adminID: adminID, expires: exp}
	m.mfa.mu.Unlock()
	return ResetLink{Token: tok, Path: ResetPath + "?token=" + tok, Expires: exp}
}

// ResetLinkAccount returns the account a valid link belongs to, without
// consuming it (the form shows whose password is being set).
func (m *Manager) ResetLinkAccount(ctx context.Context, token string) (store.Admin, error) {
	if token == "" || len(token) > 128 {
		return store.Admin{}, ErrResetInvalid
	}
	m.mfa.mu.Lock()
	m.mfa.init()
	l, ok := m.mfa.resets[hashToken(token)]
	m.mfa.mu.Unlock()
	if !ok || m.Now().After(l.expires) {
		return store.Admin{}, ErrResetInvalid
	}
	a, err := m.Store.GetAdmin(ctx, l.adminID)
	if err != nil || a.DisabledAt != nil {
		return store.Admin{}, ErrResetInvalid
	}
	return a, nil
}

// CompletePasswordReset sets a new password through a link: the link is
// consumed only when the password passes the policy, every session of the
// account ends, and one audit row (actor: the account) records it.
func (m *Manager) CompletePasswordReset(ctx context.Context, token, password, ip string) (store.Admin, error) {
	if ok, wait := m.loginLimit.Allow(ip); !ok {
		return store.Admin{}, &RateLimitedError{RetryAfter: wait}
	}
	a, err := m.ResetLinkAccount(ctx, token)
	if err != nil {
		return store.Admin{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return store.Admin{}, err
	}
	h, err := HashPassword(password)
	if err != nil {
		return store.Admin{}, err
	}
	key := hashToken(token)
	m.mfa.mu.Lock()
	l, ok := m.mfa.resets[key]
	if ok {
		delete(m.mfa.resets, key)
	}
	m.mfa.mu.Unlock()
	if !ok || l.adminID != a.ID {
		return store.Admin{}, ErrResetInvalid
	}
	err = m.Store.Write(ctx, func(tx *store.Tx) error {
		if err := tx.UpdateAdminPassword(a.ID, h, false); err != nil {
			return err
		}
		n, err := tx.DeleteAdminSessions(a.ID, "")
		if err != nil {
			return err
		}
		k, err := tx.RevokeAdminAPITokens(a.ID)
		if err != nil {
			return err
		}
		return auditTx(tx, Actor{Type: "admin", ID: a.ID, Label: a.Username, Method: "web", IP: ip}, "admin.password_reset.complete", "admin", a.ID,
			"ok", "warning", map[string]any{"sessions_ended": n, "tokens_revoked": k})
	})
	if err != nil {
		// The change was not made; give the link back so the user can retry.
		m.mfa.mu.Lock()
		m.mfa.resets[key] = l
		m.mfa.mu.Unlock()
		return store.Admin{}, err
	}
	return a, nil
}
