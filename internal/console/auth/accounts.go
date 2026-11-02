package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Account administration (architecture §5.2 "Admin accounts", screen S32).
// Listing is AD; every change is OW in a signed-in session (never an API
// token). One implementation behind /api/v1/admins
// and /settings/accounts. Every change and its audit row commit in one
// transaction (an unauditable change is not made). An owner never changes,
// disables, deletes or resets their own account here (ErrSelf: /account is
// for that), and the last enabled owner can be neither demoted, disabled nor
// deleted (store.ErrLastOwner).

// ErrAccountInvalid: the username, e-mail, role or customer scope is not valid.
var ErrAccountInvalid = errors.New("auth: the account fields are not valid")

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,99}$`)

// AccountInput is a create or an update of role and scope. Scope nil means
// all customers; only operator and read_only may be limited to customers.
type AccountInput struct {
	Username string
	Email    string
	Role     string
	Scope    []string
}

// requireOwner admits a signed-in owner session. An API token is refused
// (ErrSessionRequired) whatever its scope: account changes mint or reset other
// accounts' credentials and change roles, which a script must not do.
func (m *Manager) requireOwner(p *Principal) error {
	if p == nil || p.Pending {
		return ErrNoSession
	}
	if p.ViaToken() {
		return ErrSessionRequired
	}
	if p.Role < RoleOwner {
		return ErrForbidden
	}
	return nil
}

// validScope checks the role/scope pair and that every customer id exists.
func (m *Manager) validScope(ctx context.Context, role string, scope []string) error {
	if _, ok := ParseRole(role); !ok {
		return ErrAccountInvalid
	}
	if scope == nil {
		return nil
	}
	if role != "operator" && role != "read_only" || len(scope) == 0 {
		return ErrAccountInvalid
	}
	cs, err := m.Store.ListCustomers(ctx, store.ScopeAll())
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, c := range cs {
		known[c.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range scope {
		if !known[id] || seen[id] {
			return ErrAccountInvalid
		}
		seen[id] = true
	}
	return nil
}

// ListAccounts returns every account (AD and up).
func (m *Manager) ListAccounts(ctx context.Context, p *Principal) ([]store.Admin, error) {
	if p == nil || p.Pending {
		return nil, ErrNoSession
	}
	if p.Role < RoleAdmin {
		return nil, ErrForbidden
	}
	return m.Store.ListAdmins(ctx)
}

// GetAccount returns one account (AD and up).
func (m *Manager) GetAccount(ctx context.Context, p *Principal, id string) (store.Admin, error) {
	if p == nil || p.Pending {
		return store.Admin{}, ErrNoSession
	}
	if p.Role < RoleAdmin {
		return store.Admin{}, ErrForbidden
	}
	return m.Store.GetAdmin(ctx, id)
}

// CreateAccount creates an account with an unusable password and returns a
// one-time password-reset link for it (shown once): the new user sets their
// own password through the link and enrolls TOTP at the first login.
func (m *Manager) CreateAccount(ctx context.Context, p *Principal, in AccountInput, method, ip string) (store.Admin, ResetLink, error) {
	if err := m.requireOwner(p); err != nil {
		return store.Admin{}, ResetLink{}, err
	}
	in.Username, in.Email = strings.TrimSpace(in.Username), strings.TrimSpace(in.Email)
	if !usernameRE.MatchString(in.Username) || !ValidEmail(in.Email) {
		return store.Admin{}, ResetLink{}, ErrAccountInvalid
	}
	if err := m.validScope(ctx, in.Role, in.Scope); err != nil {
		return store.Admin{}, ResetLink{}, err
	}
	var made store.Admin
	err := m.Store.Write(ctx, func(tx *store.Tx) error {
		var err error
		made, err = tx.CreateAdmin(store.Admin{Username: in.Username, Email: in.Email, Role: in.Role, CustomerScope: in.Scope, PasswordHash: "!"})
		if err != nil {
			return err
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.create", "admin", made.ID, "ok", "info",
			map[string]any{"role": in.Role, "scoped": in.Scope != nil, "customers": len(in.Scope), "reset_link": true})
	})
	if err != nil {
		return store.Admin{}, ResetLink{}, err
	}
	return made, m.newResetLink(made.ID), nil
}

// UpdateAccount changes another account's role and customer scope. The change
// applies to that account's next request (roles are read per request).
func (m *Manager) UpdateAccount(ctx context.Context, p *Principal, id, role string, scope []string, method, ip string) (store.Admin, error) {
	if err := m.requireOwner(p); err != nil {
		return store.Admin{}, err
	}
	if id == p.Admin.ID {
		return store.Admin{}, ErrSelf
	}
	if err := m.validScope(ctx, role, scope); err != nil {
		return store.Admin{}, err
	}
	var after store.Admin
	err := m.Store.Write(ctx, func(tx *store.Tx) error {
		before, a, err := tx.UpdateAdminRoleScope(id, role, scope)
		if err != nil {
			return err
		}
		after = a
		return auditTx(tx, ActorOf(p, method, ip), "admin.update", "admin", id, "ok", "info",
			map[string]any{"role_from": before.Role, "role_to": a.Role, "scoped": scope != nil, "customers": len(scope)})
	})
	return after, err
}

// cleanReason trims an operator's free-text reason to one line of at most 200
// characters for the audit row.
func cleanReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

func reasonDetails(reason string) map[string]any {
	if r := cleanReason(reason); r != "" {
		return map[string]any{"reason": r}
	}
	return nil
}

// DisableAccount disables another account: its sessions end and its API
// tokens are revoked at once.
func (m *Manager) DisableAccount(ctx context.Context, p *Principal, id, reason, method, ip string) error {
	if err := m.requireOwner(p); err != nil {
		return err
	}
	if id == p.Admin.ID {
		return ErrSelf
	}
	return m.Store.Write(ctx, func(tx *store.Tx) error {
		if err := tx.DisableAdmin(id); err != nil {
			return err
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.disable", "admin", id, "ok", "warning", reasonDetails(reason))
	})
}

// EnableAccount re-enables another account (and lifts any login lock).
func (m *Manager) EnableAccount(ctx context.Context, p *Principal, id, method, ip string) error {
	if err := m.requireOwner(p); err != nil {
		return err
	}
	if id == p.Admin.ID {
		return ErrSelf
	}
	return m.Store.Write(ctx, func(tx *store.Tx) error {
		if err := tx.EnableAdmin(id); err != nil {
			return err
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.enable", "admin", id, "ok", "info", nil)
	})
}

// DeleteAccount deletes another account (never the last enabled owner).
func (m *Manager) DeleteAccount(ctx context.Context, p *Principal, id, method, ip string) error {
	if err := m.requireOwner(p); err != nil {
		return err
	}
	if id == p.Admin.ID {
		return ErrSelf
	}
	return m.Store.Write(ctx, func(tx *store.Tx) error {
		a, err := tx.DeleteAdmin(id)
		if err != nil {
			return err
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.delete", "admin", id, "ok", "warning", map[string]any{"username": a.Username, "role": a.Role})
	})
}

// ResetAccountMFA is the break-glass for a lost authenticator: the account's
// TOTP enrollment and recovery codes are removed, its sessions end and its API
// tokens are revoked, so its next login goes through enrollment again.
func (m *Manager) ResetAccountMFA(ctx context.Context, p *Principal, id, reason, method, ip string) error {
	if err := m.requireOwner(p); err != nil {
		return err
	}
	if id == p.Admin.ID {
		return ErrSelf
	}
	return m.Store.Write(ctx, func(tx *store.Tx) error {
		return ResetMFATx(tx, ActorOf(p, method, ip), id, reason)
	})
}

// ResetMFATx is the reset-mfa change and its audit row inside tx; the CLI's
// `user reset-mfa` uses it with a cli actor.
func ResetMFATx(tx *store.Tx, actor Actor, id, reason string) error {
	if err := tx.ClearAdminMFA(id); err != nil {
		return err
	}
	k, err := tx.RevokeAdminAPITokens(id)
	if err != nil {
		return err
	}
	d := reasonDetails(reason)
	if d == nil {
		d = map[string]any{}
	}
	d["tokens_revoked"] = k
	return auditTx(tx, actor, "admin.reset_mfa", "admin", id, "ok", "critical", d)
}

// ResetAccountPassword issues a one-time password-reset link for another
// account (shown once). The old password keeps working until the link is used.
func (m *Manager) ResetAccountPassword(ctx context.Context, p *Principal, id, method, ip string) (ResetLink, error) {
	if err := m.requireOwner(p); err != nil {
		return ResetLink{}, err
	}
	if id == p.Admin.ID {
		return ResetLink{}, ErrSelf
	}
	err := m.Store.Write(ctx, func(tx *store.Tx) error {
		a, err := tx.GetAdmin(id)
		if err != nil {
			return err
		}
		if a.DisabledAt != nil {
			return store.ErrConflict
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.password_reset.issue", "admin", id, "ok", "warning", nil)
	})
	if err != nil {
		return ResetLink{}, err
	}
	return m.newResetLink(id), nil
}
