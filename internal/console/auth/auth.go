package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/ratelimit"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Cookie and header names.
const (
	SessionCookie = "cryo_session"
	CSRFCookie    = "cryo_csrf"
	CSRFHeader    = "X-CSRF-Token"
	CSRFField     = "csrf_token"
)

// Role is a console role, ordered by privilege.
type Role int

// Roles (architecture §1.1): read_only < operator < admin < owner.
const (
	RoleNone Role = iota
	RoleReadOnly
	RoleOperator
	RoleAdmin
	RoleOwner
)

// ParseRole maps the admin_users.role values.
func ParseRole(s string) (Role, bool) {
	switch s {
	case "read_only":
		return RoleReadOnly, true
	case "operator":
		return RoleOperator, true
	case "admin":
		return RoleAdmin, true
	case "owner":
		return RoleOwner, true
	}
	return RoleNone, false
}

// String returns the stored role name.
func (r Role) String() string {
	switch r {
	case RoleReadOnly:
		return "read_only"
	case RoleOperator:
		return "operator"
	case RoleAdmin:
		return "admin"
	case RoleOwner:
		return "owner"
	}
	return "none"
}

// Short returns the architecture's abbreviation (RO, OP, AD, OW).
func (r Role) Short() string {
	switch r {
	case RoleReadOnly:
		return "RO"
	case RoleOperator:
		return "OP"
	case RoleAdmin:
		return "AD"
	case RoleOwner:
		return "OW"
	}
	return "-"
}

// Errors returned by Login and Authenticate.
var (
	// ErrInvalidCredentials is the only failure Login reports for an unknown
	// user, a wrong password, a disabled account and a locked account alike, so
	// the response is not an account-existence or lockout oracle. The distinction
	// lives in the audit row only.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrNoSession          = errors.New("auth: no valid session")
	// ErrMFAPending is Authenticate's answer for a session that has passed the
	// password step but not the MFA step. It wraps ErrNoSession: a pending
	// session is not a login and satisfies nothing but the MFA routes.
	ErrMFAPending = fmt.Errorf("%w: the MFA step is not complete", ErrNoSession)
)

// RateLimitedError is returned by Login when the source IP is over its limit.
type RateLimitedError struct{ RetryAfter time.Duration }

// Error implements error.
func (e *RateLimitedError) Error() string { return "auth: too many login attempts" }

// Principal is an authenticated admin for one request.
type Principal struct {
	Admin         store.Admin
	SessionID     string
	Role          Role
	Scope         store.Scope
	MFAVerifiedAt *time.Time
	// CSRF is the token this session's forms and htmx requests must carry.
	CSRF string
	// Pending is true for a session that has passed the password step only. It
	// reaches the MFA routes (RequirePending, RequireEnrollment) and nothing else.
	Pending bool
	// TokenID and TokenName are set when the request authenticated with an API
	// token (Authorization: Bearer) instead of a session; SessionID is then "".
	TokenID   string
	TokenName string
	// TokenScopes are the scopes the token holds: the gate stops a narrow one at its route.
	TokenScopes []string
}

// Enrolled reports whether the account has a TOTP enrollment.
func (p *Principal) Enrolled() bool { return p != nil && p.Admin.TOTPEnrolledAt != nil }

// ViaToken reports whether the request authenticated with an API token.
func (p *Principal) ViaToken() bool { return p != nil && p.TokenID != "" }

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal stored by Require.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok
}

// Manager owns login, the MFA step, sessions, API tokens, CSRF and the account
// operations of the account screens (one implementation behind the admin API
// and the UI). WebAuthn and OIDC are later milestones.
type Manager struct {
	// Master wraps the TOTP secrets (context ContextTOTP). Without it nobody can
	// enroll or pass the TOTP step, and login says so (fail loudly).
	Master *escrow.MasterKey
	// TOTPNow is the clock TOTP codes are checked against. nil means the wall
	// clock: an authenticator app runs on real time even when the console's
	// clock is frozen (fixtures mode). Tests inject one.
	TOTPNow func() time.Time
	// Issuer labels the account in the authenticator app ("CryoBackup").
	Issuer string

	Store     *store.Store
	Idle      time.Duration
	Absolute  time.Duration
	LockAfter int
	LockFor   time.Duration
	// PublicHost, when set, is an additional host an Origin may name (the
	// configured public_url host).
	PublicHost string
	Now        func() time.Time

	loginLimit *ratelimit.Bucket
	mu         sync.Mutex
	touched    map[string]time.Time

	nonceOnce sync.Once // login nonce key, see loginKey
	nonceKey  []byte

	mfa mfaState // pending enrollments, MFA attempt counts, reset links (mfa.go, resets.go)
}

// NewManager builds a Manager. perMinPerIP is the login rate limit per source IP.
func NewManager(st *store.Store, idle, absolute time.Duration, perMinPerIP, lockAfter int, lockFor time.Duration, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{Store: st, Idle: idle, Absolute: absolute, LockAfter: lockAfter, LockFor: lockFor, Now: now,
		loginLimit: ratelimit.NewBucket(perMinPerIP, time.Minute, perMinPerIP, now), touched: map[string]time.Time{}}
}

// LoginResult is a successful password step (MFARequired true: Session is a
// pending session) or a completed MFA step (MFARequired false: a full session).
type LoginResult struct {
	Token       string // the cookie value; shown once, only its sha256 is stored
	CSRF        string
	Admin       store.Admin
	Session     store.Session
	MFARequired bool
	// EnrollRequired: the account has no TOTP yet, so the MFA step is the forced
	// enrollment (POST /api/v1/me/totp/begin and /confirm, or /login/mfa).
	EnrollRequired bool
}

// PendingTTL is how long a password-only (pending) session lives: long enough
// to open an authenticator app, short enough that a stolen pending cookie is
// worth little. A pending session older than this is no session at all.
const PendingTTL = 10 * time.Minute

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// CSRFToken derives the CSRF token of a session from its cookie value: an HMAC,
// so it needs no storage and cannot be computed without the cookie.
func CSRFToken(sessionToken string) string {
	m := hmac.New(sha256.New, []byte(sessionToken))
	m.Write([]byte("cryobackup/csrf/v1"))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (m *Manager) audit(ctx context.Context, action, result, severity, ip string, a *store.Admin, reason string) {
	ev := store.AuditEvent{ActorType: "system", Method: "web", SourceIP: ip, Action: action, ObjectType: "admin_session", Result: result, Severity: severity}
	if a != nil {
		ev.ActorType, ev.ActorID, ev.ActorLabel, ev.ObjectID = "admin", a.ID, a.Username, a.ID
	}
	if reason != "" {
		ev.Details = []byte(`{"reason":"` + reason + `"}`)
	}
	_, _ = m.Store.AppendAudit(ctx, ev)
}

// Login verifies username and password and starts a session. Every failure
// that is not a rate limit returns ErrInvalidCredentials (unknown user, wrong
// password, disabled and locked accounts are indistinguishable to the caller;
// the audit row carries the real reason), and each of those paths spends one
// argon2 verification so the time taken does not reveal which it was.
func (m *Manager) Login(ctx context.Context, username, password, ip, userAgent string) (LoginResult, error) {
	if ok, wait := m.loginLimit.Allow(ip); !ok {
		m.audit(ctx, "auth.login", "denied", "warning", ip, nil, "rate_limited")
		return LoginResult{}, &RateLimitedError{RetryAfter: wait}
	}
	a, err := m.Store.GetAdminByUsername(ctx, username)
	if err != nil {
		BurnPassword(password)
		if !errors.Is(err, store.ErrNotFound) {
			return LoginResult{}, err
		}
		m.audit(ctx, "auth.login", "denied", "warning", ip, nil, "unknown_user")
		return LoginResult{}, ErrInvalidCredentials
	}
	now := m.Now()
	if a.DisabledAt != nil {
		BurnPassword(password)
		m.audit(ctx, "auth.login", "denied", "warning", ip, &a, "disabled")
		return LoginResult{}, ErrInvalidCredentials
	}
	if a.LockedUntil != nil && a.LockedUntil.After(now) {
		BurnPassword(password)
		m.audit(ctx, "auth.login", "denied", "warning", ip, &a, "locked")
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, rehash := VerifyPassword(a.PasswordHash, password)
	if !ok {
		if a.PasswordHash == "" || a.PasswordHash == "!" {
			BurnPassword(password)
		}
		upd, err := m.Store.RecordLoginFailure(ctx, a.ID, m.LockAfter, m.LockFor)
		reason := "bad_password"
		if err == nil && upd.LockedUntil != nil && upd.LockedUntil.After(now) {
			reason = "locked_now"
		}
		m.audit(ctx, "auth.login", "denied", "warning", ip, &a, reason)
		return LoginResult{}, ErrInvalidCredentials
	}
	if rehash {
		if h, err := HashPassword(password); err == nil {
			_ = m.Store.UpdateAdminPassword(ctx, a.ID, h, a.MustChangePassword)
		}
	}
	// The password step yields a PENDING session only (no mfa_verified_at, ten
	// minutes): it can do the MFA step, or the forced enrollment for an account
	// without TOTP, and nothing else. The failure counter is NOT cleared here: it
	// counts password and code failures together and is cleared only when the MFA
	// step completes, so knowing the password does not buy fresh code guesses.
	tok, err := newSessionToken()
	if err != nil {
		return LoginResult{}, err
	}
	sess, err := m.Store.CreateSession(ctx, store.Session{AdminID: a.ID, TokenHash: hashToken(tok), SourceIP: ip, UserAgentHash: uaHashOf(userAgent),
		LastSeenAt: now, ExpiresAt: now.Add(PendingTTL), AbsoluteExpiresAt: now.Add(PendingTTL)})
	if err != nil {
		return LoginResult{}, err
	}
	m.audit(ctx, "auth.login", "ok", "info", ip, &a, "")
	return LoginResult{Token: tok, CSRF: CSRFToken(tok), Admin: a, Session: sess, MFARequired: true, EnrollRequired: a.TOTPEnrolledAt == nil}, nil
}

// newSessionToken is 32 random bytes in base64url: a session cookie value.
func newSessionToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func uaHashOf(userAgent string) string {
	if userAgent == "" {
		return ""
	}
	return hashToken(userAgent)
}

// Authenticate resolves a session cookie value to the Principal of a completed
// login. It fails with ErrNoSession for an unknown, expired or revoked session
// or a disabled account, and with ErrMFAPending (which wraps ErrNoSession) for
// a session that has not passed the MFA step. The idle expiry slides, written
// at most once a minute per session.
func (m *Manager) Authenticate(ctx context.Context, cookie string) (*Principal, error) {
	p, err := m.sessionPrincipal(ctx, cookie)
	if err != nil {
		return nil, err
	}
	if p.Pending {
		return nil, ErrMFAPending
	}
	return p, nil
}

// CheckSession answers what Authenticate answers and leaves the session as it
// found it: the idle expiry does not slide. It is for what a machine asks on its own
// (an open event stream checking its credential), which is not the operator doing
// anything. It writes nothing.
func (m *Manager) CheckSession(ctx context.Context, cookie string) (*Principal, error) {
	p, err := m.lookupSession(ctx, cookie)
	if err != nil {
		return nil, err
	}
	if p.Pending {
		return nil, ErrMFAPending
	}
	return p, nil
}

// sessionPrincipal is lookupSession for a request that counts as activity: a
// completed login's idle expiry slides, written at most once a minute per session.
func (m *Manager) sessionPrincipal(ctx context.Context, cookie string) (*Principal, error) {
	p, err := m.lookupSession(ctx, cookie)
	if err != nil || p.Pending {
		return p, err
	}
	now := m.Now()
	m.mu.Lock()
	last := m.touched[p.SessionID]
	touch := now.Sub(last) >= time.Minute
	if touch {
		m.touched[p.SessionID] = now
		if len(m.touched) > 4096 {
			for k, t := range m.touched {
				if now.Sub(t) > time.Hour {
					delete(m.touched, k)
				}
			}
		}
	}
	m.mu.Unlock()
	if touch {
		_ = m.Store.TouchSession(ctx, p.SessionID, m.Idle)
	}
	return p, nil
}

// lookupSession resolves a cookie to a Principal, pending sessions included
// (Pending set), and writes nothing. A pending session past PendingTTL is no
// session (a session written before MFA existed has no mfa_verified_at and dies
// here).
func (m *Manager) lookupSession(ctx context.Context, cookie string) (*Principal, error) {
	if cookie == "" || len(cookie) > 128 {
		return nil, ErrNoSession
	}
	sess, err := m.Store.GetSessionByHash(ctx, hashToken(cookie))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoSession
		}
		return nil, err
	}
	a, err := m.Store.GetAdmin(ctx, sess.AdminID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoSession
		}
		return nil, err
	}
	role, ok := ParseRole(a.Role)
	if a.DisabledAt != nil || !ok {
		return nil, ErrNoSession
	}
	now := m.Now()
	if sess.MFAVerifiedAt == nil {
		if now.Sub(sess.CreatedAt) > PendingTTL {
			return nil, ErrNoSession
		}
		return &Principal{Admin: a, SessionID: sess.ID, Role: role, Scope: a.Scope(), CSRF: CSRFToken(cookie), Pending: true}, nil
	}
	return &Principal{Admin: a, SessionID: sess.ID, Role: role, Scope: a.Scope(), MFAVerifiedAt: sess.MFAVerifiedAt, CSRF: CSRFToken(cookie)}, nil
}

// EndSession is what a sign-out does: it deletes the session (Logout) and leaves
// a trace, like a login does. The audit row (auth.logout) is best effort and
// never blocks the sign-out: the operator can always leave, and a session that
// cannot be deleted is the error the caller sees. method is "web" or "api".
func (m *Manager) EndSession(ctx context.Context, p *Principal, method, ip string) error {
	if err := m.Logout(ctx, p); err != nil {
		return err
	}
	if p == nil || p.SessionID == "" {
		return nil
	}
	ev := store.AuditEvent{ActorType: "admin", ActorID: p.Admin.ID, ActorLabel: p.Admin.Username, Method: method, SourceIP: ip,
		Action: "auth.logout", ObjectType: "admin_session", ObjectID: p.Admin.ID, Result: "ok", Severity: "info"}
	_, _ = m.Store.AppendAudit(ctx, ev)
	return nil
}

// Logout deletes the session behind the cookie value.
func (m *Manager) Logout(ctx context.Context, p *Principal) error {
	if p == nil {
		return nil
	}
	m.mu.Lock()
	delete(m.touched, p.SessionID)
	m.mu.Unlock()
	return m.Store.DeleteSession(ctx, p.SessionID)
}

// secureConn reports whether the request that sets (or clears) a cookie arrived
// over TLS: directly (r.TLS, the primary rule) or through a TLS front on the same
// host or a private/mesh address that says so with X-Forwarded-Proto: https. The
// header is honoured only when the direct peer (r.RemoteAddr) is loopback or
// private (config.IsLoopbackOrPrivate); from any other peer it is ignored, so
// the open internet cannot steer the flag. Of a comma-separated chain the LAST
// value is used, the hop the trusted proxy appended. The admin listener is plain
// HTTP by design (bind it to loopback or a private address), and a browser never
// sends a Secure cookie back over http://, so the flag has to follow the
// connection or login would succeed and every following request would be
// anonymous.
func secureConn(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !config.IsLoopbackOrPrivate(r.RemoteAddr) {
		return false
	}
	v := r.Header.Get("X-Forwarded-Proto")
	if i := strings.LastIndexByte(v, ','); i >= 0 {
		v = v[i+1:]
	}
	return strings.EqualFold(strings.TrimSpace(v), "https")
}

// SetCookies writes the session cookie (HttpOnly, SameSite=Strict, Secure when
// the request came over TLS, see secureConn) and the readable CSRF cookie for
// double submit.
func (m *Manager) SetCookies(w http.ResponseWriter, r *http.Request, res LoginResult) {
	exp := res.Session.AbsoluteExpiresAt
	secure := secureConn(r)
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: res.Token, Path: "/", Expires: exp, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: CSRFCookie, Value: res.CSRF, Path: "/", Expires: exp, HttpOnly: false, Secure: secure, SameSite: http.SameSiteStrictMode})
}

// ClearCookies expires both cookies with the same attributes SetCookies used for
// a request over the same kind of connection.
func ClearCookies(w http.ResponseWriter, r *http.Request) {
	secure := secureConn(r)
	for _, n := range []string{SessionCookie, CSRFCookie} {
		http.SetCookie(w, &http.Cookie{Name: n, Value: "", Path: "/", MaxAge: -1, HttpOnly: n == SessionCookie, Secure: secure, SameSite: http.SameSiteStrictMode})
	}
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// CheckCSRF enforces the unsafe-request rules: the origin rule (csrfOriginOK:
// an Origin or Referer the browser sent must name this host; a browser that
// sends neither is judged on the token alone), and a CSRF token in the
// X-CSRF-Token header or the csrf_token form field that equals the token derived
// from the session and the value of the CSRF cookie (double submit).
func (m *Manager) CheckCSRF(r *http.Request, p *Principal) error {
	if safeMethod(r.Method) {
		return nil
	}
	if !m.csrfOriginOK(r) {
		return errors.New("auth: cross-origin request")
	}
	tok := r.Header.Get(CSRFHeader)
	if tok == "" {
		tok = r.PostFormValue(CSRFField)
	}
	if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(p.CSRF)) != 1 {
		return errors.New("auth: bad csrf token")
	}
	c, err := r.Cookie(CSRFCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(tok)) != 1 {
		return errors.New("auth: csrf cookie mismatch")
	}
	return nil
}

// HeadersContradict reports whether the request carries evidence that it did NOT
// come from this site. Evidence precedence (FIX23b): a login nonce or a session
// CSRF token may only make up for MISSING header evidence, never override
// evidence that points elsewhere. Contradicting: an Origin other than "null"
// that does not name this host (or the public host); a Referer that does not
// name this host or cannot be parsed; Sec-Fetch-Site of anything but
// same-origin or none (cross-site and same-site, even beside a matching Origin).
func (m *Manager) HeadersContradict(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return true
	}
	names := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && u.Host != "" && (u.Host == r.Host || (m.PublicHost != "" && u.Host == m.PublicHost))
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" && !names(o) {
		return true
	}
	if ref := r.Header.Get("Referer"); ref != "" && !names(ref) {
		return true
	}
	return false
}

// csrfOriginOK is the origin rule of CheckCSRF on a session request. The
// session-bound token (HMAC of the session cookie, equal to the readable cookie)
// is the defence; the header check is belt and braces whenever the browser sends
// one. Refused first: anything HeadersContradict reports. Accepted then:
// originOK (Sec-Fetch-Site same-origin, or an Origin or Referer naming this
// host); or a request that offers no evidence at all, that is no Sec-Fetch-Site,
// no Referer and an Origin that is absent or exactly "null" (a privacy-hardened
// browser strips them, FIX23).
func (m *Manager) csrfOriginOK(r *http.Request) bool {
	if m.HeadersContradict(r) {
		return false
	}
	if m.originOK(r) {
		return true
	}
	if r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Referer") != "" {
		return false
	}
	o := r.Header.Get("Origin")
	return o == "" || o == "null"
}

// originOK reports whether an unsafe request came from this site. A request the
// browser marks Sec-Fetch-Site: same-origin is accepted first: that header is a
// forbidden header name, so a page cannot set or forge it, and it holds even when
// the Referrer-Policy makes the browser serialise Origin as "null". Anything else
// (cross-site, same-site, none, or no header at all) falls back to the Origin
// rule, then the Referer rule; an Origin of "null" with no Referer is refused.
func (m *Manager) originOK(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		return true
	}
	allowed := func(host string) bool {
		return host != "" && (host == r.Host || (m.PublicHost != "" && host == m.PublicHost))
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		return err == nil && allowed(u.Host)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && allowed(u.Host)
	}
	return false
}

// Mode selects how Require answers an unauthenticated or forbidden request.
type Mode int

const (
	// ModeUI redirects to /login (303) and renders 403 as a plain page.
	ModeUI Mode = iota
	// ModeAPI answers with RFC 9457 problem+json (401, 403).
	ModeAPI
)

// LoginOriginOK is the CSRF rule for the login endpoints, which have no session
// to bind a token to: a request the browser marks Sec-Fetch-Site: same-origin is
// allowed; otherwise a request that names an Origin must name this host (or the
// configured public host); a request with no Origin (a script) is allowed, since
// a browser always sends one on a cross-site POST.
func (m *Manager) LoginOriginOK(r *http.Request) bool {
	if r.Header.Get("Origin") == "" && r.Header.Get("Referer") == "" {
		return true
	}
	return m.originOK(r)
}
