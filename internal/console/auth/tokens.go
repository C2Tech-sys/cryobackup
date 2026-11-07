package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// API tokens (architecture §5.2 /me/api-tokens, feature row r101).
//
// A token is "cbk_" followed by 32 random bytes in base64url (47 characters).
// Only its SHA-256 is stored; the first 8 characters ("cbk_" and four more)
// are kept as the prefix the list shows. A token belongs to one account and
// acts with at most that account's role, always inside the account's customer
// scope, and only while the account is enabled. Scopes, in words:
//
//	read         the read-only role, whatever the account's role is
//	full         the account's own role
//	status:read  GET /api/v1/status and nothing else, at the read-only role
//	metrics:read GET /metrics and nothing else, at the read-only role
//
// Every token expires (7, 30, 90 or 365 days); expiry and revocation are
// checked on every request (no cache), and the last use is recorded at most
// once a minute. Disabling the account, changing or resetting its password
// and resetting its MFA revoke all its tokens. A token never manages
// credentials: creating or revoking tokens, revoking sessions, the password,
// the e-mail, TOTP and recovery codes need a signed-in session.
const (
	TokenPrefix     = "cbk_"
	tokenPrefixShow = 8
	ScopeRead       = "read"
	ScopeFull       = "full"
	// ScopeStatusRead and ScopeMetricsRead are for an external monitor (architecture
	// §5.2): each opens one route. A new route is closed to them until it is listed
	// in tokenScopeAllows.
	ScopeStatusRead  = "status:read"
	ScopeMetricsRead = "metrics:read"
)

// tokenScopeAllows is where a narrow token stops. A token holding read or full, or one
// with no scope on record (it has always acted read-only), is judged by its role alone.
func tokenScopeAllows(scopes []string, r *http.Request) bool {
	if len(scopes) == 0 || slices.Contains(scopes, ScopeRead) || slices.Contains(scopes, ScopeFull) {
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/status":
		return slices.Contains(scopes, ScopeStatusRead)
	case "/metrics":
		return slices.Contains(scopes, ScopeMetricsRead)
	}
	return false
}

// TokenLifetimes are the expiry choices, in days.
var TokenLifetimes = []int{7, 30, 90, 365}

// ErrTokenInvalid: the token request is malformed (name, scope or lifetime).
var ErrTokenInvalid = errors.New("auth: the token name, scope or lifetime is not valid")

// TokenRole is the role a token with these scopes acts with for an account of
// role accountRole: never more than the account's.
func TokenRole(scopes []string, accountRole Role) Role {
	if slices.Contains(scopes, ScopeFull) {
		return accountRole
	}
	return min(RoleReadOnly, accountRole)
}

// AuthenticateBearer resolves an API token to a Principal. Unknown, revoked and
// expired tokens and tokens of a disabled account are ErrNoSession.
func (m *Manager) AuthenticateBearer(ctx context.Context, raw string) (*Principal, error) {
	if !strings.HasPrefix(raw, TokenPrefix) || len(raw) < 20 || len(raw) > 128 {
		return nil, ErrNoSession
	}
	h := hashToken(raw)
	var tok store.APIToken
	var a store.Admin
	err := m.Store.Read(ctx, func(tx *store.Tx) error {
		var err error
		if tok, err = tx.GetLiveAPITokenByHash(h); err != nil {
			return err
		}
		a, err = tx.GetAdmin(tok.AdminID)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoSession
		}
		return nil, err
	}
	// The lookup was by hash; compare again in constant time so the decision
	// never rests on the database's string comparison.
	if subtle.ConstantTimeCompare([]byte(tok.TokenHash), []byte(h)) != 1 {
		return nil, ErrNoSession
	}
	role, ok := ParseRole(a.Role)
	if !ok || a.DisabledAt != nil {
		return nil, ErrNoSession
	}
	now := m.Now()
	m.mfa.mu.Lock()
	m.mfa.init()
	touch := now.Sub(m.mfa.touched[tok.ID]) >= time.Minute
	if touch {
		m.mfa.touched[tok.ID] = now
	}
	m.mfa.mu.Unlock()
	if touch {
		_ = m.Store.Write(ctx, func(tx *store.Tx) error { return tx.TouchAPIToken(tok.ID) })
	}
	return &Principal{Admin: a, Role: TokenRole(tok.Scopes, role), Scope: a.Scope(), TokenID: tok.ID, TokenName: tok.Name, TokenScopes: tok.Scopes}, nil
}

// CreateAPIToken creates a token for the principal's own account and returns
// the secret (shown once) with the stored row. Only a signed-in session may
// create one (a token cannot mint tokens). name: 1 to 100 characters, unique
// among the account's active tokens; scope "read" or "full"; days one of
// TokenLifetimes.
func (m *Manager) CreateAPIToken(ctx context.Context, p *Principal, name, scope string, days int, method, ip string) (string, store.APIToken, error) {
	if p == nil || p.Pending {
		return "", store.APIToken{}, ErrNoSession
	}
	if p.ViaToken() {
		return "", store.APIToken{}, ErrSessionRequired
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 || strings.ContainsAny(name, "\r\n\t") ||
		!slices.Contains([]string{ScopeRead, ScopeFull, ScopeStatusRead, ScopeMetricsRead}, scope) || !slices.Contains(TokenLifetimes, days) {
		return "", store.APIToken{}, ErrTokenInvalid
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", store.APIToken{}, err
	}
	secret := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	exp := m.Now().Add(time.Duration(days) * 24 * time.Hour)
	var out store.APIToken
	err := m.Store.Write(ctx, func(tx *store.Tx) error {
		var err error
		out, err = tx.CreateAPIToken(store.APIToken{AdminID: p.Admin.ID, Name: name, Scopes: []string{scope},
			TokenPrefix: secret[:tokenPrefixShow], TokenHash: hashToken(secret), ExpiresAt: &exp})
		if err != nil {
			return err
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.api_token.create", "api_token", out.ID, "ok", "info",
			map[string]any{"scope": scope, "days": days})
	})
	if err != nil {
		return "", store.APIToken{}, err
	}
	return secret, out, nil
}

// ListAPITokens lists the account's tokens (prefix only, never a secret).
func (m *Manager) ListAPITokens(ctx context.Context, adminID string) ([]store.APIToken, error) {
	var out []store.APIToken
	err := m.Store.Read(ctx, func(tx *store.Tx) error {
		var err error
		out, err = tx.ListAPITokens(adminID)
		return err
	})
	return out, err
}

// RevokeAPIToken revokes one of the principal's own tokens; it stops working
// on the next request. Another account's token is ErrNotFound. Credential
// management is for a signed-in session: an API token cannot revoke tokens.
func (m *Manager) RevokeAPIToken(ctx context.Context, p *Principal, id, method, ip string) error {
	if p == nil || p.Pending {
		return ErrNoSession
	}
	if p.ViaToken() {
		return ErrSessionRequired
	}
	return m.Store.Write(ctx, func(tx *store.Tx) error {
		if _, err := tx.RevokeAPIToken(p.Admin.ID, id); err != nil {
			return err
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.api_token.revoke", "api_token", id, "ok", "info", nil)
	})
}
