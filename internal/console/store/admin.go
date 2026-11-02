package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Admin is an operator account. Admin accounts are console-wide data, not
// customer-owned, so these methods take no Scope; the Scope an account carries
// is exposed by Admin.Scope and passed to every scoped call by the handler layer.
type Admin struct {
	ID          string
	Username    string
	Email       string
	DisplayName string
	Role        string // owner | admin | operator | read_only
	// CustomerScope is the customer ids an operator or read_only account is
	// limited to; nil means all customers (SQL NULL). Only those two roles may be scoped.
	CustomerScope     []string
	PasswordHash      string // argon2id PHC string, produced by the caller; "" for OIDC-only
	TOTPSecretWrapped []byte
	TOTPEnrolledAt    *time.Time
	// TOTPLastStep is the RFC 6238 step of the last TOTP code the account used
	// (migration 0004): a code is accepted only for a later step.
	TOTPLastStep       *int64
	OIDCSubject        string
	IPAllowlist        json.RawMessage // JSON array or nil
	MustChangePassword bool
	FailedLoginCount   int
	LockedUntil        *time.Time
	LastLoginAt        *time.Time
	LastLoginIP        string
	DisabledAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Scope converts the account's customer_scope into the Scope its calls carry.
func (a Admin) Scope() Scope {
	if a.CustomerScope == nil {
		return ScopeAll()
	}
	return ScopeOf(a.CustomerScope...)
}

const adminCols = `id, username, email, display_name, role, customer_scope, password_hash, totp_secret_wrapped, totp_enrolled_at,
	oidc_subject, ip_allowlist, must_change_password, failed_login_count, locked_until, last_login_at, last_login_ip,
	disabled_at, created_at, updated_at, totp_last_step`

func scanAdmin(sc scanner) (Admin, error) {
	var a Admin
	var display, scopeJSON, pw, oidc, ipl, ip sql.NullString
	var totp []byte
	var totpAt, locked, lastLogin, disabled, lastStep sql.NullInt64
	var must int
	var c, u int64
	err := sc.Scan(&a.ID, &a.Username, &a.Email, &display, &a.Role, &scopeJSON, &pw, &totp, &totpAt, &oidc, &ipl, &must,
		&a.FailedLoginCount, &locked, &lastLogin, &ip, &disabled, &c, &u, &lastStep)
	if err != nil {
		return Admin{}, err
	}
	a.DisplayName, a.PasswordHash, a.OIDCSubject, a.LastLoginIP = display.String, pw.String, oidc.String, ip.String
	a.TOTPSecretWrapped, a.TOTPEnrolledAt = totp, ptrTime(totpAt)
	a.IPAllowlist = rawOf(ipl)
	a.MustChangePassword = must != 0
	a.LockedUntil, a.LastLoginAt, a.DisabledAt = ptrTime(locked), ptrTime(lastLogin), ptrTime(disabled)
	a.CreatedAt, a.UpdatedAt = timeOf(c), timeOf(u)
	a.TOTPLastStep = ptrInt64(lastStep)
	if scopeJSON.Valid {
		var ids []string
		if err := json.Unmarshal([]byte(scopeJSON.String), &ids); err != nil {
			return Admin{}, invalidf("admin %s has a malformed customer_scope", a.ID)
		}
		if ids == nil {
			ids = []string{}
		}
		a.CustomerScope = ids
	}
	return a, nil
}

// CreateAdmin inserts an account; ID and timestamps are assigned. The caller
// supplies the password hash (argon2id) and any wrapped TOTP secret. The
// schema's rules (a password or OIDC subject; only operator/read_only may be
// scoped; TOTP secret iff enrolled) surface as ErrInvalid.
func (s *Store) CreateAdmin(ctx context.Context, a Admin) (Admin, error) {
	var out Admin
	err := s.Write(ctx, func(tx *Tx) error {
		var err error
		out, err = tx.CreateAdmin(a)
		return err
	})
	return out, err
}

// CreateAdmin is the in-transaction form, so the accounts screen can create an
// account and its audit row in one transaction.
func (tx *Tx) CreateAdmin(a Admin) (Admin, error) {
	var scopeArg any
	if a.CustomerScope != nil {
		b, err := json.Marshal(a.CustomerScope)
		if err != nil {
			return Admin{}, err
		}
		scopeArg = string(b)
	}
	ipl, err := nullJSON(a.IPAllowlist)
	if err != nil {
		return Admin{}, err
	}
	a.ID = tx.id()
	now := tx.nowMS()
	_, err = tx.Exec(`INSERT INTO admin_users (`+adminCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Username, a.Email, nullStr(a.DisplayName), a.Role, scopeArg, nullStr(a.PasswordHash),
		nullBytes(a.TOTPSecretWrapped), nullTime(a.TOTPEnrolledAt), nullStr(a.OIDCSubject), ipl,
		boolInt(a.MustChangePassword), a.FailedLoginCount, nullTime(a.LockedUntil), nullTime(a.LastLoginAt),
		nullStr(a.LastLoginIP), nullTime(a.DisabledAt), now, now, nullInt64(a.TOTPLastStep))
	if err != nil {
		return Admin{}, err
	}
	return tx.GetAdmin(a.ID)
}

// GetAdmin is the in-transaction form of Store.GetAdmin.
func (tx *Tx) GetAdmin(id string) (Admin, error) {
	return one(tx, `SELECT `+adminCols+` FROM admin_users WHERE id = ?`, []any{id}, scanAdmin)
}

// GetAdmin returns an account by id or ErrNotFound.
func (s *Store) GetAdmin(ctx context.Context, id string) (Admin, error) {
	var out Admin
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+adminCols+` FROM admin_users WHERE id = ?`, []any{id}, scanAdmin)
		return err
	})
	return out, err
}

// GetAdminByUsername returns an account by username (case-insensitive) or ErrNotFound.
func (s *Store) GetAdminByUsername(ctx context.Context, username string) (Admin, error) {
	var out Admin
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+adminCols+` FROM admin_users WHERE username = ?`, []any{username}, scanAdmin)
		return err
	})
	return out, err
}

// ListAdmins returns every account ordered by username.
func (s *Store) ListAdmins(ctx context.Context) ([]Admin, error) {
	var out []Admin
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT `+adminCols+` FROM admin_users ORDER BY username COLLATE NOCASE`, nil, scanAdmin)
		return err
	})
	return out, err
}

// UpdateAdminPassword stores a new password hash, sets must_change_password as
// given, and clears the lockout (a successful reset ends a lock).
func (s *Store) UpdateAdminPassword(ctx context.Context, id, passwordHash string, mustChange bool) error {
	if passwordHash == "" {
		return invalidf("password hash is required")
	}
	return s.Write(ctx, func(tx *Tx) error { return tx.UpdateAdminPassword(id, passwordHash, mustChange) })
}

// UpdateAdminPassword is the in-transaction form, so a caller can audit the
// change in the same transaction (cryo-console user passwd).
func (tx *Tx) UpdateAdminPassword(id, passwordHash string, mustChange bool) error {
	if passwordHash == "" {
		return invalidf("password hash is required")
	}
	res, err := tx.Exec(`UPDATE admin_users SET password_hash = ?, must_change_password = ?, failed_login_count = 0,
		locked_until = NULL, updated_at = ? WHERE id = ?`, passwordHash, boolInt(mustChange), tx.nowMS(), id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// RecordLoginFailure counts a failed login and locks the account for lockFor
// once the count reaches lockAfter (lockAfter <= 0 never locks). It returns the
// updated account so the caller can tell the lock fired.
func (s *Store) RecordLoginFailure(ctx context.Context, id string, lockAfter int, lockFor time.Duration) (Admin, error) {
	var out Admin
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`UPDATE admin_users SET failed_login_count = failed_login_count + 1,
			locked_until = CASE WHEN ? > 0 AND failed_login_count + 1 >= ? THEN ? ELSE locked_until END,
			updated_at = ? WHERE id = ?`, lockAfter, lockAfter, now+lockFor.Milliseconds(), now, id)
		if err != nil {
			return err
		}
		if err := requireRow(res); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+adminCols+` FROM admin_users WHERE id = ?`, []any{id}, scanAdmin)
		return err
	})
	return out, err
}

// ClearLoginFailures records a successful login: failures and lock cleared,
// last_login_at set to now, last_login_ip to ip ("" leaves it NULL).
func (s *Store) ClearLoginFailures(ctx context.Context, id, ip string) error {
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`UPDATE admin_users SET failed_login_count = 0, locked_until = NULL, last_login_at = ?,
			last_login_ip = ?, updated_at = ? WHERE id = ?`, now, nullStr(ip), now, id)
		if err != nil {
			return err
		}
		return requireRow(res)
	})
}

// DisableAdmin disables the account, ends its sessions and revokes its API
// tokens in one transaction (the `user disable` command). Disabling a disabled
// account keeps its original disabled_at. ErrNotFound for an unknown id.
func (s *Store) DisableAdmin(ctx context.Context, id string) error {
	return s.Write(ctx, func(tx *Tx) error { return tx.DisableAdmin(id) })
}

// requireRow maps "no row changed" to ErrNotFound.
func requireRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

// Session is a console login session (ADR D5: cookie + MFA). TokenHash is the
// lower-case SHA-256 hex of the cookie value; the cookie itself is never stored.
type Session struct {
	ID                string
	AdminID           string
	TokenHash         string
	MFAVerifiedAt     *time.Time
	SourceIP          string
	UserAgentHash     string
	LastSeenAt        time.Time
	ExpiresAt         time.Time // idle timeout
	AbsoluteExpiresAt time.Time // hard cap
	RevokedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const sessionCols = `id, admin_id, token_hash, mfa_verified_at, source_ip, user_agent_hash, last_seen_at, expires_at,
	absolute_expires_at, revoked_at, created_at, updated_at`

func scanSession(sc scanner) (Session, error) {
	var x Session
	var mfa, revoked sql.NullInt64
	var ip, ua sql.NullString
	var seen, exp, abs, c, u int64
	if err := sc.Scan(&x.ID, &x.AdminID, &x.TokenHash, &mfa, &ip, &ua, &seen, &exp, &abs, &revoked, &c, &u); err != nil {
		return Session{}, err
	}
	x.MFAVerifiedAt, x.SourceIP, x.UserAgentHash = ptrTime(mfa), ip.String, ua.String
	x.LastSeenAt, x.ExpiresAt, x.AbsoluteExpiresAt = timeOf(seen), timeOf(exp), timeOf(abs)
	x.RevokedAt, x.CreatedAt, x.UpdatedAt = ptrTime(revoked), timeOf(c), timeOf(u)
	return x, nil
}

// CreateSession inserts a session. LastSeenAt defaults to now; ExpiresAt and
// AbsoluteExpiresAt are required (the caller owns the timeout policy).
func (s *Store) CreateSession(ctx context.Context, x Session) (Session, error) {
	var out Session
	err := s.Write(ctx, func(tx *Tx) error {
		x.ID = tx.id()
		now := tx.nowMS()
		seen := now
		if !x.LastSeenAt.IsZero() {
			seen = msOf(x.LastSeenAt)
		}
		_, err := tx.Exec(`INSERT INTO sessions (`+sessionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			x.ID, x.AdminID, x.TokenHash, nullTime(x.MFAVerifiedAt), nullStr(x.SourceIP), nullStr(x.UserAgentHash), seen,
			msOf(x.ExpiresAt), msOf(x.AbsoluteExpiresAt), nullTime(x.RevokedAt), now, now)
		if err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, []any{x.ID}, scanSession)
		return err
	})
	return out, err
}

// GetSessionByHash returns the live session for a cookie hash. A session that is
// revoked, past its idle expiry or past its absolute expiry is ErrNotFound, so a
// caller cannot forget to check.
func (s *Store) GetSessionByHash(ctx context.Context, tokenHash string) (Session, error) {
	var out Session
	err := s.Read(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		var err error
		out, err = one(tx, `SELECT `+sessionCols+` FROM sessions
			WHERE token_hash = ? AND revoked_at IS NULL AND expires_at > ? AND absolute_expires_at > ?`,
			[]any{tokenHash, now, now}, scanSession)
		return err
	})
	return out, err
}

// TouchSession records activity: last_seen_at = now and the idle expiry slides to
// now+idle, never beyond the absolute cap.
func (s *Store) TouchSession(ctx context.Context, id string, idle time.Duration) error {
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`UPDATE sessions SET last_seen_at = ?, expires_at = MIN(?, absolute_expires_at), updated_at = ?
			WHERE id = ? AND revoked_at IS NULL`, now, now+idle.Milliseconds(), now, id)
		if err != nil {
			return err
		}
		return requireRow(res)
	})
}

// MarkSessionMFA stamps a fresh MFA verification (the gate for key reveal and
// destructive actions).
func (s *Store) MarkSessionMFA(ctx context.Context, id string) error {
	return s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`UPDATE sessions SET mfa_verified_at = ?, updated_at = ? WHERE id = ? AND revoked_at IS NULL`, now, now, id)
		if err != nil {
			return err
		}
		return requireRow(res)
	})
}

// DeleteSession removes a session (logout). Deleting a missing session is not an error.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`DELETE FROM sessions WHERE id = ?`, id)
		return err
	})
}

// DeleteExpiredSessions removes sessions that are expired or revoked (the
// session-cleanup job) and returns how many.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *Tx) error {
		now := tx.nowMS()
		res, err := tx.Exec(`DELETE FROM sessions WHERE expires_at <= ? OR absolute_expires_at <= ? OR revoked_at IS NOT NULL`, now, now)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
