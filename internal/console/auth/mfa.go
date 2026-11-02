package auth

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Errors of the MFA step and the account operations. Handlers map them to
// fixed sentences and problem codes; none carries a code, secret or path.
var (
	// ErrBadCode: the TOTP or recovery code is wrong, expired, already used, or
	// the account is locked. One error for all, like ErrInvalidCredentials.
	ErrBadCode = errors.New("auth: code incorrect or expired")
	// ErrNoEnrollment: a confirm without a begin (or the begin expired).
	ErrNoEnrollment = errors.New("auth: no TOTP enrollment is in progress")
	// ErrNotEnrolled: the operation needs a TOTP enrollment the account lacks.
	ErrNotEnrolled = errors.New("auth: the account has no TOTP enrollment")
	// ErrAlreadyEnrolled: a pending session tried to enroll an account that has
	// TOTP (it must pass the MFA step instead).
	ErrAlreadyEnrolled = errors.New("auth: the account already has TOTP")
	// ErrMasterKey: no console master key is loaded, so no TOTP secret can be
	// wrapped or unwrapped.
	ErrMasterKey = escrow.ErrMasterKeyUnavailable
	// ErrSessionRequired: the operation is refused for an API token.
	ErrSessionRequired = errors.New("auth: this operation needs a signed-in session")
	// ErrForbidden: the principal's role does not allow the operation.
	ErrForbidden = errors.New("auth: forbidden")
	// ErrSelf: an owner operation aimed at the owner's own account (use /account).
	ErrSelf = errors.New("auth: not allowed on your own account")
)

// maxPendingAttempts is how many wrong codes one pending session may send
// before it is deleted (the account lockout and the per-IP limiter apply too).
const maxPendingAttempts = 5

// pendingEnrollTTL is how long a begun TOTP enrollment waits for its confirm.
const pendingEnrollTTL = 15 * time.Minute

type pendingTOTP struct {
	adminID string
	secret  []byte
	expires time.Time
}

// mfaState is the in-memory part of the MFA step: begun enrollments (the
// secret lives here, unwrapped, until the confirm wraps and stores it), wrong
// code counts per pending session, and password-reset links (resets.go). A
// restart loses it, which only means "begin again" or "issue a new link".
type mfaState struct {
	mu       sync.Mutex
	enroll   map[string]pendingTOTP // by session id
	attempts map[string]int         // by session id
	resets   map[string]resetLink   // by sha256 of the link token
	touched  map[string]time.Time   // API token last-used writes, by token id
}

func (s *mfaState) init() {
	if s.enroll == nil {
		s.enroll, s.attempts, s.resets, s.touched = map[string]pendingTOTP{}, map[string]int{}, map[string]resetLink{}, map[string]time.Time{}
	}
}

func (m *Manager) totpNow() time.Time {
	if m.TOTPNow != nil {
		return m.TOTPNow()
	}
	return time.Now()
}

func (m *Manager) issuer() string {
	if m.Issuer != "" {
		return m.Issuer
	}
	return "CryoBackup"
}

// Actor is who did something, for the audit row.
type Actor struct {
	Type, ID, Label, Method, IP string
}

// ActorOf is the audit actor of a principal: the admin for a session, the API
// token for a Bearer request. method is "web" (UI) or "api".
func ActorOf(p *Principal, method, ip string) Actor {
	if p == nil {
		return Actor{Type: "system", Method: method, IP: ip}
	}
	if p.ViaToken() {
		return Actor{Type: "api_token", ID: p.TokenID, Label: p.Admin.Username, Method: "api", IP: ip}
	}
	return Actor{Type: "admin", ID: p.Admin.ID, Label: p.Admin.Username, Method: method, IP: ip}
}

// auditTx appends one audit row inside the caller's transaction. details is
// marshalled to JSON (fixed keys, never a secret, code or path).
func auditTx(tx *store.Tx, a Actor, action, objType, objID, result, severity string, details map[string]any) error {
	ev := store.AuditEvent{ActorType: a.Type, ActorID: a.ID, ActorLabel: a.Label, Method: a.Method, SourceIP: a.IP,
		Action: action, ObjectType: objType, ObjectID: objID, Result: result, Severity: severity}
	if len(details) > 0 {
		b, err := json.Marshal(details)
		if err != nil {
			return err
		}
		ev.Details = b
	}
	_, err := tx.AppendAudit(ev)
	return err
}

// unwrapTOTP returns the account's TOTP secret.
func (m *Manager) unwrapTOTP(a store.Admin) ([]byte, error) {
	if a.TOTPEnrolledAt == nil || len(a.TOTPSecretWrapped) == 0 {
		return nil, ErrNotEnrolled
	}
	if !m.Master.Loaded() {
		return nil, ErrMasterKey
	}
	return m.Master.Unwrap(ContextTOTP, a.ID, 1, a.TOTPSecretWrapped)
}

// newFullSession creates the session of a completed login inside tx and
// deletes the pending one: the cookie value changes when the login completes,
// so a pending cookie never becomes a full session.
func (m *Manager) newFullSession(tx *store.Tx, p *Principal, ip, userAgent string) (LoginResult, error) {
	tok, err := newSessionToken()
	if err != nil {
		return LoginResult{}, err
	}
	now := m.Now()
	sess, err := tx.InsertSession(store.Session{AdminID: p.Admin.ID, TokenHash: hashToken(tok), MFAVerifiedAt: &now, SourceIP: ip,
		UserAgentHash: uaHashOf(userAgent), LastSeenAt: now, ExpiresAt: now.Add(m.Idle), AbsoluteExpiresAt: now.Add(m.Absolute)})
	if err != nil {
		return LoginResult{}, err
	}
	if err := tx.DeleteSession(p.SessionID); err != nil {
		return LoginResult{}, err
	}
	if err := tx.ClearLoginFailures(p.Admin.ID, ip); err != nil {
		return LoginResult{}, err
	}
	a, err := tx.GetAdmin(p.Admin.ID)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{Token: tok, CSRF: CSRFToken(tok), Admin: a, Session: sess}, nil
}

// codeFailure records a wrong code: one audit row, the account's failure
// counter (which locks it after LockAfter), and for a pending session the
// per-session count (the session is deleted after maxPendingAttempts).
func (m *Manager) codeFailure(ctx context.Context, p *Principal, action, reason, method, ip string) {
	_ = m.Store.Write(ctx, func(tx *store.Tx) error {
		upd, err := tx.RecordLoginFailure(p.Admin.ID, m.LockAfter, m.LockFor)
		if err == nil && upd.LockedUntil != nil && upd.LockedUntil.After(m.Now()) && reason == "bad_code" {
			reason = "locked_now"
		}
		return auditTx(tx, Actor{Type: "admin", ID: p.Admin.ID, Label: p.Admin.Username, Method: method, IP: ip},
			action, "admin_session", p.Admin.ID, "denied", "warning", map[string]any{"reason": reason})
	})
	if !p.Pending {
		return
	}
	m.mfa.mu.Lock()
	m.mfa.init()
	m.mfa.attempts[p.SessionID]++
	n := m.mfa.attempts[p.SessionID]
	if n >= maxPendingAttempts {
		delete(m.mfa.attempts, p.SessionID)
		delete(m.mfa.enroll, p.SessionID)
	}
	m.mfa.mu.Unlock()
	if n >= maxPendingAttempts {
		_ = m.Store.DeleteSession(ctx, p.SessionID)
	}
}

// codePreflight is shared by every code check: the per-IP limiter, a fresh
// read of the account (disabled is no session; locked is a bad code).
func (m *Manager) codePreflight(ctx context.Context, p *Principal, action, method, ip string) (store.Admin, error) {
	if ok, wait := m.loginLimit.Allow(ip); !ok {
		m.audit(ctx, action, "denied", "warning", ip, &p.Admin, "rate_limited")
		return store.Admin{}, &RateLimitedError{RetryAfter: wait}
	}
	a, err := m.Store.GetAdmin(ctx, p.Admin.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return a, ErrNoSession
		}
		return a, err
	}
	if a.DisabledAt != nil {
		return a, ErrNoSession
	}
	if a.LockedUntil != nil && a.LockedUntil.After(m.Now()) {
		m.audit(ctx, action, "denied", "warning", ip, &a, "locked")
		return a, ErrBadCode
	}
	return a, nil
}

// checkTOTP verifies a TOTP code of an enrolled account inside tx, with the
// replay guard: nil only when the code matches a step later than the last used
// one AND this transaction recorded that step. errCodeMismatch for a wrong or
// expired code, errCodeReplayed for a code (or step) already used.
func (m *Manager) checkTOTP(tx *store.Tx, a store.Admin, code string) error {
	secret, err := m.unwrapTOTP(a)
	if err != nil {
		return err
	}
	step, ok := matchTOTP(secret, code, m.totpNow(), -1)
	if !ok {
		return errCodeMismatch
	}
	// The guard is the UPDATE (atomic against a concurrent use), not the row
	// read before it: a step at or before the recorded one is refused.
	advanced, err := tx.AdvanceTOTPStep(a.ID, step)
	if err != nil {
		return err
	}
	if !advanced {
		return errCodeReplayed
	}
	return nil
}

// errCodeMismatch and errCodeReplayed roll a code check's transaction back;
// callers record the failure in a transaction of its own and answer ErrBadCode.
var (
	errCodeMismatch = errors.New("auth: code mismatch")
	errCodeReplayed = errors.New("auth: code already used")
)

// failReason is the audit reason of a failed code check.
func failReason(err error, base string) string {
	if errors.Is(err, errCodeReplayed) {
		return "replayed_code"
	}
	return base
}

func isCodeFailure(err error) bool {
	return errors.Is(err, errCodeMismatch) || errors.Is(err, errCodeReplayed)
}

// recoveryVerifier checks a code against one stored hash; tests replace it
// with a slow stub to prove the hashing holds no write lock.
var recoveryVerifier = verifyRecoveryCode

// matchRecovery finds which unused recovery code of the account a presented
// code is, OUTSIDE any write transaction: the hashes are read in a read
// transaction and verified (argon2id, every one of them, no early exit, so the
// time does not tell which matched) with no database lock held, because up to
// ten argon2id verifications would otherwise stall every console write (agent
// check-ins included) behind the single write lock. It returns the row id or
// "". The caller consumes the row with useRecovery inside its short write.
func (m *Manager) matchRecovery(ctx context.Context, adminID, code string) (string, error) {
	if len(normalizeRecovery(code)) != 10 {
		return "", nil
	}
	var codes []store.RecoveryCode
	if err := m.Store.Read(ctx, func(tx *store.Tx) error {
		var err error
		codes, err = tx.UnusedRecoveryCodes(adminID)
		return err
	}); err != nil {
		return "", err
	}
	match := ""
	for _, c := range codes {
		if recoveryVerifier(adminID, code, c.CodeHash) {
			match = c.ID
		}
	}
	return match, nil
}

// useRecovery marks the matched code used inside tx. UseRecoveryCode updates
// only while used_at IS NULL, so of two requests that matched the same code
// outside the transaction exactly one consumes it; the loser (and an empty
// match) is a failed attempt: errCodeMismatch.
func useRecovery(tx *store.Tx, id string) error {
	if id == "" {
		return errCodeMismatch
	}
	ok, err := tx.UseRecoveryCode(id)
	if err != nil {
		return err
	}
	if !ok {
		return errCodeMismatch
	}
	return nil
}

// VerifyTOTPLogin completes a pending login with a TOTP code. On success the
// pending session is replaced by a full one (new cookie value) and one audit
// row records it. A wrong, expired or replayed code is ErrBadCode.
func (m *Manager) VerifyTOTPLogin(ctx context.Context, p *Principal, code, method, ip, userAgent string) (LoginResult, error) {
	if p == nil || !p.Pending {
		return LoginResult{}, ErrNoSession
	}
	a, err := m.codePreflight(ctx, p, "auth.mfa", method, ip)
	if err != nil {
		return LoginResult{}, err
	}
	if a.TOTPEnrolledAt == nil {
		return LoginResult{}, ErrNotEnrolled
	}
	var res LoginResult
	err = m.Store.Write(ctx, func(tx *store.Tx) error {
		if err := m.checkTOTP(tx, a, code); err != nil {
			return err
		}
		var err error
		res, err = m.newFullSession(tx, p, ip, userAgent)
		if err != nil {
			return err
		}
		return auditTx(tx, Actor{Type: "admin", ID: a.ID, Label: a.Username, Method: method, IP: ip}, "auth.mfa", "admin_session", a.ID, "ok", "info",
			map[string]any{"factor": "totp"})
	})
	if isCodeFailure(err) {
		m.codeFailure(ctx, p, "auth.mfa", failReason(err, "bad_code"), method, ip)
		return LoginResult{}, ErrBadCode
	}
	if err != nil {
		return LoginResult{}, err
	}
	m.forgetPending(p.SessionID)
	return res, nil
}

// VerifyRecoveryLogin completes a pending login with a single-use recovery
// code. The audit row is a warning and says how many codes are left.
func (m *Manager) VerifyRecoveryLogin(ctx context.Context, p *Principal, code, method, ip, userAgent string) (LoginResult, int, error) {
	if p == nil || !p.Pending {
		return LoginResult{}, 0, ErrNoSession
	}
	a, err := m.codePreflight(ctx, p, "auth.mfa", method, ip)
	if err != nil {
		return LoginResult{}, 0, err
	}
	match, err := m.matchRecovery(ctx, a.ID, code)
	if err != nil {
		return LoginResult{}, 0, err
	}
	var res LoginResult
	left := 0
	err = m.Store.Write(ctx, func(tx *store.Tx) error {
		if err := useRecovery(tx, match); err != nil {
			return err
		}
		var err error
		res, err = m.newFullSession(tx, p, ip, userAgent)
		if err != nil {
			return err
		}
		left, _, err = tx.RecoveryCodeCounts(a.ID)
		if err != nil {
			return err
		}
		return auditTx(tx, Actor{Type: "admin", ID: a.ID, Label: a.Username, Method: method, IP: ip}, "auth.mfa", "admin_session", a.ID, "ok", "warning",
			map[string]any{"factor": "recovery_code", "recovery_codes_left": left})
	})
	if errors.Is(err, errCodeMismatch) {
		m.codeFailure(ctx, p, "auth.mfa", "bad_recovery_code", method, ip)
		return LoginResult{}, 0, ErrBadCode
	}
	if err != nil {
		return LoginResult{}, 0, err
	}
	m.forgetPending(p.SessionID)
	return res, left, nil
}

func (m *Manager) forgetPending(sessionID string) {
	m.mfa.mu.Lock()
	m.mfa.init()
	delete(m.mfa.attempts, sessionID)
	delete(m.mfa.enroll, sessionID)
	m.mfa.mu.Unlock()
}

// TOTPEnrollment is a begun enrollment as the page shows it. Secret and URI
// are the secret: rendered once, served no-store, never logged.
type TOTPEnrollment struct {
	Secret  string // base32, grouped in fours
	URI     string // otpauth://
	Expires time.Time
}

// BeginTOTP starts (or, within its lifetime, shows again) a TOTP enrollment for
// the principal's session. A pending session may begin only when its account
// has no TOTP (the forced enrollment); a full session may begin a re-enrollment,
// which its confirm must authorise with a current code. Needs the master key,
// since the confirm wraps the secret.
func (m *Manager) BeginTOTP(ctx context.Context, p *Principal) (TOTPEnrollment, error) {
	if p == nil {
		return TOTPEnrollment{}, ErrNoSession
	}
	if p.ViaToken() {
		return TOTPEnrollment{}, ErrSessionRequired
	}
	if p.Pending && p.Enrolled() {
		return TOTPEnrollment{}, ErrAlreadyEnrolled
	}
	if !m.Master.Loaded() {
		return TOTPEnrollment{}, ErrMasterKey
	}
	now := m.Now()
	m.mfa.mu.Lock()
	m.mfa.init()
	pe, ok := m.mfa.enroll[p.SessionID]
	if !ok || pe.adminID != p.Admin.ID || now.After(pe.expires) {
		secret, err := NewTOTPSecret()
		if err != nil {
			m.mfa.mu.Unlock()
			return TOTPEnrollment{}, err
		}
		pe = pendingTOTP{adminID: p.Admin.ID, secret: secret, expires: now.Add(pendingEnrollTTL)}
		m.mfa.enroll[p.SessionID] = pe
	}
	m.mfa.mu.Unlock()
	return TOTPEnrollment{Secret: TOTPSecretGroups(pe.secret), URI: OTPAuthURI(m.issuer(), p.Admin.Username, pe.secret), Expires: pe.expires}, nil
}

// ConfirmTOTP finishes an enrollment begun by BeginTOTP on the same session:
// code must be valid for the new secret. For an account that already has TOTP
// (re-enrollment from /account) current must be a valid code of the OLD secret
// or an unused recovery code (a fresh MFA check). The secret is wrapped under
// the master key and stored, ten new recovery codes replace any old ones and
// are returned once, and one audit row is written. For a pending session the
// login completes in the same transaction and the result carries the new full
// session (nil otherwise).
func (m *Manager) ConfirmTOTP(ctx context.Context, p *Principal, code, current, method, ip, userAgent string) ([]string, *LoginResult, error) {
	if p == nil {
		return nil, nil, ErrNoSession
	}
	if p.ViaToken() {
		return nil, nil, ErrSessionRequired
	}
	m.mfa.mu.Lock()
	m.mfa.init()
	pe, ok := m.mfa.enroll[p.SessionID]
	m.mfa.mu.Unlock()
	if !ok || pe.adminID != p.Admin.ID || m.Now().After(pe.expires) {
		return nil, nil, ErrNoEnrollment
	}
	a, err := m.codePreflight(ctx, p, "admin.mfa_enroll", method, ip)
	if err != nil {
		return nil, nil, err
	}
	if p.Pending && a.TOTPEnrolledAt != nil {
		return nil, nil, ErrAlreadyEnrolled
	}
	step, ok := matchTOTP(pe.secret, code, m.totpNow(), -1)
	if !ok {
		m.codeFailure(ctx, p, "admin.mfa_enroll", "bad_code", method, ip)
		return nil, nil, ErrBadCode
	}
	if !m.Master.Loaded() {
		return nil, nil, ErrMasterKey
	}
	wrapped, err := m.Master.Wrap(ContextTOTP, a.ID, 1, pe.secret)
	if err != nil {
		return nil, nil, err
	}
	codes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = HashRecoveryCode(a.ID, c)
	}
	// A recovery code given as the current factor of a re-enrollment is
	// verified here, before the write transaction (see matchRecovery).
	recoveryMatch := ""
	if a.TOTPEnrolledAt != nil && !p.Pending {
		if recoveryMatch, err = m.matchRecovery(ctx, a.ID, current); err != nil {
			return nil, nil, err
		}
	}
	var res *LoginResult
	reenroll := false
	err = m.Store.Write(ctx, func(tx *store.Tx) error {
		// The enrollment state is decided here, inside the write transaction (one
		// writer at a time), not from the read above: of two concurrent
		// confirms only the first sees "not enrolled".
		cur, err := tx.GetAdmin(a.ID)
		if err != nil {
			return err
		}
		reenroll = cur.TOTPEnrolledAt != nil
		if reenroll && p.Pending {
			return ErrAlreadyEnrolled
		}
		if reenroll {
			if err := m.checkTOTP(tx, cur, current); err != nil {
				if !isCodeFailure(err) {
					return err
				}
				// Not a current TOTP code: a recovery code matched before the
				// transaction (recoveryMatch) may stand in for it.
				if rerr := useRecovery(tx, recoveryMatch); rerr != nil {
					if errors.Is(rerr, errCodeMismatch) {
						return err
					}
					return rerr
				}
			}
		}
		if err := tx.SetAdminTOTP(a.ID, wrapped, step); err != nil {
			return err
		}
		if err := tx.ReplaceRecoveryCodes(a.ID, hashes); err != nil {
			return err
		}
		details := map[string]any{"reenroll": reenroll, "recovery_codes": len(codes)}
		if p.Pending {
			r, err := m.newFullSession(tx, p, ip, userAgent)
			if err != nil {
				return err
			}
			res = &r
			details["login"] = true
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.mfa_enroll", "admin", a.ID, "ok", "info", details)
	})
	if isCodeFailure(err) {
		m.codeFailure(ctx, p, "admin.mfa_enroll", failReason(err, "bad_current_code"), method, ip)
		return nil, nil, ErrBadCode
	}
	if err != nil {
		return nil, nil, err
	}
	m.forgetPending(p.SessionID)
	return codes, res, nil
}

// RegenerateRecoveryCodes replaces the account's recovery codes after a fresh
// MFA check (a valid, unused TOTP code in the same request) and returns the new
// codes once.
func (m *Manager) RegenerateRecoveryCodes(ctx context.Context, p *Principal, code, method, ip string) ([]string, error) {
	if p == nil || p.Pending {
		return nil, ErrNoSession
	}
	if p.ViaToken() {
		return nil, ErrSessionRequired
	}
	a, err := m.codePreflight(ctx, p, "admin.recovery_codes", method, ip)
	if err != nil {
		return nil, err
	}
	if a.TOTPEnrolledAt == nil {
		return nil, ErrNotEnrolled
	}
	codes, err := NewRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = HashRecoveryCode(a.ID, c)
	}
	err = m.Store.Write(ctx, func(tx *store.Tx) error {
		if err := m.checkTOTP(tx, a, code); err != nil {
			return err
		}
		if err := tx.ReplaceRecoveryCodes(a.ID, hashes); err != nil {
			return err
		}
		return auditTx(tx, ActorOf(p, method, ip), "admin.recovery_codes", "admin", a.ID, "ok", "info", map[string]any{"recovery_codes": len(codes)})
	})
	if isCodeFailure(err) {
		m.codeFailure(ctx, p, "admin.recovery_codes", failReason(err, "bad_code"), method, ip)
		return nil, ErrBadCode
	}
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// MFAStatus is what /account shows about the account's second factor.
type MFAStatus struct {
	Enrolled      bool
	EnrolledAt    *time.Time
	RecoveryLeft  int
	RecoveryTotal int
}

// MFAStatusOf reads the account's MFA status.
func (m *Manager) MFAStatusOf(ctx context.Context, adminID string) (MFAStatus, error) {
	var st MFAStatus
	err := m.Store.Read(ctx, func(tx *store.Tx) error {
		a, err := tx.GetAdmin(adminID)
		if err != nil {
			return err
		}
		st.Enrolled, st.EnrolledAt = a.TOTPEnrolledAt != nil, a.TOTPEnrolledAt
		st.RecoveryLeft, st.RecoveryTotal, err = tx.RecoveryCodeCounts(adminID)
		return err
	})
	return st, err
}
