package auth

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// mfaHelper completes the MFA step of test logins: the first login of an
// account enrolls TOTP (and keeps the secret and the recovery codes), later
// ones send a TOTP code. Its TOTP clock moves 30 s per code, so consecutive
// logins never trip the replay guard by accident.
type mfaHelper struct {
	m       *Manager
	mu      sync.Mutex
	at      time.Time
	secrets map[string][]byte   // by username
	codes   map[string][]string // recovery codes, by username
}

func newMFAHelper(t *testing.T, m *Manager) *mfaHelper {
	mk, err := escrow.NewMasterKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h := &mfaHelper{m: m, at: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), secrets: map[string][]byte{}, codes: map[string][]string{}}
	m.Master = mk
	m.TOTPNow = h.now
	return h
}

func (h *mfaHelper) now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.at }

// next moves the TOTP clock to the next step and returns it.
func (h *mfaHelper) next() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.at = h.at.Add(TOTPPeriod)
	return h.at
}

func secretOf(t *testing.T, grouped string) []byte {
	t.Helper()
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(grouped, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *mfaHelper) complete(t *testing.T, res LoginResult) LoginResult {
	t.Helper()
	ctx := context.Background()
	p, err := h.m.PendingPrincipal(ctx, res.Token)
	if err != nil {
		t.Fatalf("pending principal: %v", err)
	}
	user := p.Admin.Username
	if !p.Enrolled() {
		en, err := h.m.BeginTOTP(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		secret := secretOf(t, en.Secret)
		codes, full, err := h.m.ConfirmTOTP(ctx, p, TOTPCode(secret, h.next()), "", "web", "192.0.2.1", "")
		if err != nil || full == nil || len(codes) != RecoveryCodeCount {
			t.Fatalf("enroll: %v %v %d", err, full, len(codes))
		}
		h.mu.Lock()
		h.secrets[user], h.codes[user] = secret, codes
		h.mu.Unlock()
		return *full
	}
	h.mu.Lock()
	secret := h.secrets[user]
	h.mu.Unlock()
	full, err := h.m.VerifyTOTPLogin(ctx, p, TOTPCode(secret, h.next()), "web", "192.0.2.1", "")
	if err != nil {
		t.Fatalf("totp login: %v", err)
	}
	return full
}

func auditCount(t *testing.T, st *store.Store, action string) int {
	t.Helper()
	l, err := st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

func allAudit(t *testing.T, st *store.Store) []store.AuditEvent {
	t.Helper()
	l, err := st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// RFC 6238 appendix B, SHA-1 rows, truncated to six digits.
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037", 20000000000: "353130"} {
		if got := TOTPCode(secret, time.Unix(unix, 0)); got != want {
			t.Errorf("T=%d: %s, want %s", unix, got, want)
		}
	}
}

// The window is the step before, the current one and the step after; two steps
// away is refused, and so is anything that is not six digits.
func TestTOTPWindow(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1_700_000_000, 0)
	cur := TOTPStep(now)
	for d, want := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		st, ok := matchTOTP(secret, TOTPCodeAt(secret, cur+d), now, -1)
		if ok != want || (ok && st != cur+d) {
			t.Errorf("offset %d: ok=%v step=%d", d, ok, st)
		}
	}
	code := TOTPCodeAt(secret, cur)
	if _, ok := matchTOTP(secret, code[:3]+" "+code[3:], now, -1); !ok {
		t.Error("a code typed with a space must match")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := matchTOTP(secret, bad, now, -1); ok {
			t.Errorf("%q matched", bad)
		}
	}
}

func pendingOf(t *testing.T, e *env, user, pw string) (*Principal, LoginResult) {
	t.Helper()
	res, err := e.m.Login(context.Background(), user, pw, "192.0.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.m.PendingPrincipal(context.Background(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p, res
}

// Login cannot complete without MFA, on every path: the password step's
// session satisfies no gate but the MFA ones, API or UI, and Authenticate
// refuses it. Only the MFA step turns it into a session (with a new cookie).
func TestLoginCannotCompleteWithoutMFA(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	res, err := e.m.Login(ctx, "ow", "right horse", "192.0.2.1", "")
	if err != nil || !res.MFARequired || !res.EnrollRequired {
		t.Fatalf("password step: %v %+v", err, res)
	}
	if _, err := e.m.Authenticate(ctx, res.Token); !errors.Is(err, ErrMFAPending) || !errors.Is(err, ErrNoSession) {
		t.Fatalf("Authenticate(pending) = %v", err)
	}
	for _, min := range []Role{RoleReadOnly, RoleOperator, RoleAdmin, RoleOwner} {
		for _, gate := range []func(Role, Mode, http.Handler) http.Handler{e.m.Require, e.m.RequireSession} {
			w := httptest.NewRecorder()
			gate(min, ModeAPI, okHandler()).ServeHTTP(w, req("GET", "/api/v1/x", &res))
			if w.Code != 401 {
				t.Errorf("API gate %v with a pending session: %d", min, w.Code)
			}
			w = httptest.NewRecorder()
			gate(min, ModeUI, okHandler()).ServeHTTP(w, req("GET", "/x", &res))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != MFAPath {
				t.Errorf("UI gate %v with a pending session: %d %s", min, w.Code, w.Header().Get("Location"))
			}
		}
	}
	// The MFA gates admit it; RequireEnrollment admits it only while the account has no TOTP.
	for name, h := range map[string]http.Handler{"pending": e.m.RequirePending(ModeAPI, okHandler()), "enroll": e.m.RequireEnrollment(ModeAPI, okHandler())} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req("GET", "/x", &res))
		if w.Code != 200 {
			t.Errorf("%s gate: %d", name, w.Code)
		}
	}
	full := e.mfa.complete(t, res)
	if full.Token == res.Token {
		t.Fatal("completing MFA must issue a new session cookie")
	}
	if _, err := e.m.Authenticate(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("the pending cookie must die with the MFA step: %v", err)
	}
	if p, err := e.m.Authenticate(ctx, full.Token); err != nil || p.Pending || p.MFAVerifiedAt == nil {
		t.Fatalf("full session: %v %+v", err, p)
	}
	// A full session is not a pending one.
	w := httptest.NewRecorder()
	e.m.RequirePending(ModeAPI, okHandler()).ServeHTTP(w, req("GET", "/x", &full))
	if w.Code != 409 {
		t.Fatalf("pending gate with a full session: %d", w.Code)
	}
	// Second login of the enrolled account: the enrollment gate refuses its pending session.
	res2, _ := e.m.Login(ctx, "ow", "right horse", "192.0.2.1", "")
	if res2.EnrollRequired {
		t.Fatal("an enrolled account must not be offered enrollment")
	}
	w = httptest.NewRecorder()
	e.m.RequireEnrollment(ModeAPI, okHandler()).ServeHTTP(w, req("GET", "/x", &res2))
	if w.Code != 401 {
		t.Fatalf("enrollment gate with the pending session of an enrolled account: %d", w.Code)
	}
	p2, _ := e.m.PendingPrincipal(ctx, res2.Token)
	if _, err := e.m.BeginTOTP(ctx, p2); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("re-enrolling from a pending session: %v", err)
	}
	// A pending session older than PendingTTL is gone.
	e.clk.Advance(PendingTTL + time.Second)
	if _, err := e.m.PendingPrincipal(ctx, res2.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired pending session: %v", err)
	}
}

// A session written before MFA existed (no mfa_verified_at) is not a login.
func TestLegacySessionWithoutMFAIsNoSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.addAdmin(t, "old", "owner", "right horse")
	tok := "legacy-cookie-value-0123456789abcdef"
	now := e.clk.Now()
	if _, err := e.st.CreateSession(ctx, store.Session{AdminID: a.ID, TokenHash: hashToken(tok), LastSeenAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(11 * time.Hour), AbsoluteExpiresAt: now.Add(6 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(PendingTTL + time.Minute)
	if _, err := e.m.Authenticate(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("legacy session: %v", err)
	}
	if _, err := e.m.PendingPrincipal(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("legacy session as pending: %v", err)
	}
}

// The same code cannot be used twice, nor an older one inside the window.
func TestTOTPReplayIsRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	login(t, e, "ow", "right horse") // enrolls
	secret := e.mfa.secrets["ow"]
	at := e.mfa.next()
	code := TOTPCode(secret, at)
	p, _ := pendingOf(t, e, "ow", "right horse")
	if _, err := e.m.VerifyTOTPLogin(ctx, p, code, "web", "192.0.2.1", ""); err != nil {
		t.Fatal(err)
	}
	p, _ = pendingOf(t, e, "ow", "right horse")
	if _, err := e.m.VerifyTOTPLogin(ctx, p, code, "web", "192.0.2.1", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("replayed code: %v", err)
	}
	// The previous step's code is inside the window but older than the used step.
	p, _ = pendingOf(t, e, "ow", "right horse")
	if _, err := e.m.VerifyTOTPLogin(ctx, p, TOTPCode(secret, at.Add(-TOTPPeriod)), "web", "192.0.2.1", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("older code: %v", err)
	}
	reasons := map[string]int{}
	for _, ev := range allAudit(t, e.st) {
		if ev.Action == "auth.mfa" && ev.Result == "denied" {
			var d struct{ Reason string }
			_ = json.Unmarshal(ev.Details, &d)
			reasons[d.Reason]++
		}
	}
	if reasons["replayed_code"] != 2 {
		t.Fatalf("replays must be audited as replayed_code: %v", reasons)
	}
	// The next step works.
	p, _ = pendingOf(t, e, "ow", "right horse")
	if _, err := e.m.VerifyTOTPLogin(ctx, p, TOTPCode(secret, e.mfa.next()), "web", "192.0.2.1", ""); err != nil {
		t.Fatalf("next step: %v", err)
	}
}

func TestRecoveryCodeIsSingleUseAndWorksWithoutTheMasterKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	login(t, e, "ow", "right horse")
	code := e.mfa.codes["ow"][3]
	e.m.Master = nil // banner C12: TOTP cannot be checked, recovery codes still work
	p, _ := pendingOf(t, e, "ow", "right horse")
	if _, err := e.m.VerifyTOTPLogin(ctx, p, "123456", "web", "192.0.2.1", ""); !errors.Is(err, ErrMasterKey) {
		t.Fatalf("TOTP without the master key: %v", err)
	}
	p, _ = pendingOf(t, e, "ow", "right horse")
	res, left, err := e.m.VerifyRecoveryLogin(ctx, p, strings.ToUpper(code), "web", "192.0.2.1", "")
	if err != nil || left != RecoveryCodeCount-1 || res.Token == "" {
		t.Fatalf("recovery login: %v left=%d", err, left)
	}
	p, _ = pendingOf(t, e, "ow", "right horse")
	if _, _, err := e.m.VerifyRecoveryLogin(ctx, p, code, "web", "192.0.2.1", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("second use of a recovery code: %v", err)
	}
	// Another account's code is not this account's code.
	e.addAdmin(t, "op", "operator", "right horse")
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		return tx.ReplaceRecoveryCodes(mustAdmin(t, e, "op").ID, []string{HashRecoveryCode(mustAdmin(t, e, "op").ID, "aaaaa-bbbbb")})
	}); err != nil {
		t.Fatal(err)
	}
	p, _ = pendingOf(t, e, "ow", "right horse")
	if _, _, err := e.m.VerifyRecoveryLogin(ctx, p, "aaaaa-bbbbb", "web", "192.0.2.1", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a code of another account: %v", err)
	}
}

func mustAdmin(t *testing.T, e *env, user string) store.Admin {
	t.Helper()
	a, err := e.st.GetAdminByUsername(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The TOTP secret is stored only wrapped, and no database text (audit details
// included) carries it or any recovery code.
func TestSecretNeverInTheDatabaseInClear(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	login(t, e, "ow", "right horse")
	secret := e.mfa.secrets["ow"]
	a := mustAdmin(t, e, "ow")
	if len(a.TOTPSecretWrapped) == 0 || bytes.Contains(a.TOTPSecretWrapped, secret) {
		t.Fatal("the secret must be stored wrapped")
	}
	got, err := e.m.Master.Unwrap(ContextTOTP, a.ID, 1, a.TOTPSecretWrapped)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("unwrap: %v", err)
	}
	needles := []string{TOTPSecretText(secret), strings.ToLower(TOTPSecretText(secret))}
	needles = append(needles, e.mfa.codes["ow"]...)
	var dump strings.Builder
	if err := e.st.Read(ctx, func(tx *store.Tx) error {
		for _, q := range []string{`SELECT coalesce(details,'') FROM audit_events`, `SELECT code_hash FROM admin_recovery_codes`,
			`SELECT coalesce(email,'') || coalesce(password_hash,'') || coalesce(hex(totp_secret_wrapped),'') FROM admin_users`} {
			rows, err := tx.Query(q)
			if err != nil {
				return err
			}
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					rows.Close()
					return err
				}
				dump.WriteString(v + "\n")
			}
			rows.Close()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range needles {
		if strings.Contains(dump.String(), n) {
			t.Fatalf("%q found in the database in clear", n)
		}
	}
}

// Wrong codes count toward the account lockout (LockAfter 3 here), the
// lockout refuses even the right code, and five wrong codes end a pending session.
func TestWrongCodesLockTheAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	login(t, e, "ow", "right horse")
	p, _ := pendingOf(t, e, "ow", "right horse")
	for i := 0; i < 3; i++ {
		if _, err := e.m.VerifyTOTPLogin(ctx, p, "000000", "web", "192.0.2.1", ""); !errors.Is(err, ErrBadCode) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	right := TOTPCode(e.mfa.secrets["ow"], e.mfa.next())
	if _, err := e.m.VerifyTOTPLogin(ctx, p, right, "web", "192.0.2.1", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a locked account must refuse the right code: %v", err)
	}
	e.clk.Advance(16 * time.Minute)
	// The lock lifted, but the pending session expired with it: log in again.
	if _, err := e.m.Login(ctx, "ow", "right horse", "192.0.2.1", ""); err != nil {
		t.Fatal(err)
	}

	// Five wrong codes end the pending session (lockout off for this part).
	e.m.LockAfter = 0
	p, res := pendingOf(t, e, "ow", "right horse")
	for i := 0; i < maxPendingAttempts; i++ {
		_, _ = e.m.VerifyTOTPLogin(ctx, p, "000000", "web", "192.0.2.1", "")
	}
	if _, err := e.m.PendingPrincipal(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("pending session after %d wrong codes: %v", maxPendingAttempts, err)
	}
}

func TestRegenerateRecoveryCodesNeedsAFreshCode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	full := login(t, e, "ow", "right horse")
	p, _ := e.m.Authenticate(ctx, full.Token)
	if _, err := e.m.RegenerateRecoveryCodes(ctx, p, "000000", "web", "192.0.2.1"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("without a valid code: %v", err)
	}
	old := e.mfa.codes["ow"]
	codes, err := e.m.RegenerateRecoveryCodes(ctx, p, TOTPCode(e.mfa.secrets["ow"], e.mfa.next()), "web", "192.0.2.1")
	if err != nil || len(codes) != RecoveryCodeCount {
		t.Fatalf("regenerate: %v", err)
	}
	pp, _ := pendingOf(t, e, "ow", "right horse")
	if _, _, err := e.m.VerifyRecoveryLogin(ctx, pp, old[0], "web", "192.0.2.1", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("an old code after regeneration: %v", err)
	}
	if _, _, err := e.m.VerifyRecoveryLogin(ctx, pp, codes[0], "web", "192.0.2.1", ""); err != nil {
		t.Fatalf("a new code: %v", err)
	}
}

func TestReEnrollNeedsTheCurrentFactor(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	full := login(t, e, "ow", "right horse")
	p, _ := e.m.Authenticate(ctx, full.Token)
	en, err := e.m.BeginTOTP(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	newSecret := secretOf(t, en.Secret)
	if _, _, err := e.m.ConfirmTOTP(ctx, p, TOTPCode(newSecret, e.mfa.next()), "000000", "web", "192.0.2.1", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("re-enroll without the current factor: %v", err)
	}
	if a := mustAdmin(t, e, "ow"); bytes.Contains(a.TOTPSecretWrapped, newSecret) {
		t.Fatal("impossible")
	}
	codes, res, err := e.m.ConfirmTOTP(ctx, p, TOTPCode(newSecret, e.mfa.next()), e.mfa.codes["ow"][0], "web", "192.0.2.1", "")
	if err != nil || res != nil || len(codes) != RecoveryCodeCount {
		t.Fatalf("re-enroll with a recovery code: %v %v", err, res)
	}
	e.mfa.secrets["ow"] = newSecret
	login(t, e, "ow", "right horse") // the new secret works
}

func TestAPITokenScopeExpiryAndRevocation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	full := login(t, e, "ow", "right horse")
	p, _ := e.m.Authenticate(ctx, full.Token)
	readTok, rt, err := e.m.CreateAPIToken(ctx, p, "reader", ScopeRead, 7, "web", "192.0.2.1")
	if err != nil || !strings.HasPrefix(readTok, TokenPrefix) || rt.TokenPrefix != readTok[:8] || rt.TokenHash == readTok {
		t.Fatalf("create: %v %+v", err, rt)
	}
	fullTok, ft, _ := e.m.CreateAPIToken(ctx, p, "admin", ScopeFull, 30, "web", "192.0.2.1")
	if _, _, err := e.m.CreateAPIToken(ctx, p, "reader", ScopeRead, 7, "web", "192.0.2.1"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate active name: %v", err)
	}
	for _, bad := range []struct {
		name, scope string
		days        int
	}{{"", ScopeRead, 7}, {"x", "admin", 7}, {"x", ScopeRead, 3}, {strings.Repeat("n", 101), ScopeRead, 7}} {
		if _, _, err := e.m.CreateAPIToken(ctx, p, bad.name, bad.scope, bad.days, "web", ""); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	bearer := func(tok string, min Role, method string, mode Mode) int {
		r := httptest.NewRequest(method, "/api/v1/x", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		e.m.Require(min, mode, okHandler()).ServeHTTP(w, r)
		return w.Code
	}
	// Scope: read is read-only whatever the account; full is the account's role. No CSRF for Bearer.
	if c := bearer(readTok, RoleReadOnly, "POST", ModeAPI); c != 200 {
		t.Fatalf("read token on an RO route: %d", c)
	}
	if c := bearer(readTok, RoleOperator, "POST", ModeAPI); c != 403 {
		t.Fatalf("read token on an OP route: %d", c)
	}
	if c := bearer(fullTok, RoleOwner, "POST", ModeAPI); c != 200 {
		t.Fatalf("full token of an owner on an OW route: %d", c)
	}
	// Bearer is refused on UI routes and on session-only API routes.
	if c := bearer(fullTok, RoleReadOnly, "GET", ModeUI); c != 403 {
		t.Fatalf("Bearer on a UI route: %d", c)
	}
	r := httptest.NewRequest("POST", "/api/v1/me/api-tokens", nil)
	r.Header.Set("Authorization", "Bearer "+fullTok)
	w := httptest.NewRecorder()
	e.m.RequireSession(RoleReadOnly, ModeAPI, okHandler()).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("Bearer on a session-only route: %d", w.Code)
	}
	tp, _ := e.m.AuthenticateBearer(ctx, fullTok)
	if _, _, err := e.m.CreateAPIToken(ctx, tp, "minted", ScopeFull, 7, "api", ""); !errors.Is(err, ErrSessionRequired) {
		t.Fatalf("a token minting a token: %v", err)
	}
	// Unknown and malformed tokens.
	for _, tok := range []string{"cbk_" + strings.Repeat("A", 43), "nope", readTok + "x"} {
		if c := bearer(tok, RoleReadOnly, "GET", ModeAPI); c != 401 {
			t.Errorf("token %q: %d", tok[:6], c)
		}
	}
	// Last use is recorded.
	ts, _ := e.m.ListAPITokens(ctx, p.Admin.ID)
	used := 0
	for _, x := range ts {
		if x.LastUsedAt != nil {
			used++
		}
	}
	if used != 2 {
		t.Fatalf("last_used_at recorded on %d tokens, want 2", used)
	}
	// Revocation is immediate.
	if err := e.m.RevokeAPIToken(ctx, p, ft.ID, "web", ""); err != nil {
		t.Fatal(err)
	}
	if c := bearer(fullTok, RoleReadOnly, "GET", ModeAPI); c != 401 {
		t.Fatalf("revoked token: %d", c)
	}
	// Expiry: the 7-day reader dies after 7 days.
	e.clk.Advance(7*24*time.Hour + time.Second)
	if c := bearer(readTok, RoleReadOnly, "GET", ModeAPI); c != 401 {
		t.Fatalf("expired token: %d", c)
	}
	// Another account cannot revoke this account's token.
	e.addAdmin(t, "op", "operator", "right horse")
	op := login(t, e, "op", "right horse")
	opp, _ := e.m.Authenticate(ctx, op.Token)
	if err := e.m.RevokeAPIToken(ctx, opp, rt.ID, "web", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoking another account's token: %v", err)
	}
}

func TestDisablingAnAccountRevokesTokensAndSessions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	e.addAdmin(t, "op", "operator", "right horse")
	owp, _ := e.m.Authenticate(ctx, login(t, e, "ow", "right horse").Token)
	opRes := login(t, e, "op", "right horse")
	opp, _ := e.m.Authenticate(ctx, opRes.Token)
	tok, _, err := e.m.CreateAPIToken(ctx, opp, "op script", ScopeFull, 30, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.DisableAccount(ctx, owp, opp.Admin.ID, "left the company", "web", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Authenticate(ctx, opRes.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session of a disabled account: %v", err)
	}
	if _, err := e.m.AuthenticateBearer(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("token of a disabled account: %v", err)
	}
	// Re-enabling does not bring the token back.
	if err := e.m.EnableAccount(ctx, owp, opp.Admin.ID, "web", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AuthenticateBearer(ctx, tok); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a revoked token after enable: %v", err)
	}
}

func TestLastOwnerIsProtected(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	e.addAdmin(t, "ow2", "owner", "right horse")
	p, _ := e.m.Authenticate(ctx, login(t, e, "ow", "right horse").Token)
	ow2 := mustAdmin(t, e, "ow2")
	// Self is refused; another owner may be changed while one stays.
	if err := e.m.DisableAccount(ctx, p, p.Admin.ID, "", "web", ""); !errors.Is(err, ErrSelf) {
		t.Fatalf("self disable: %v", err)
	}
	if _, err := e.m.UpdateAccount(ctx, p, ow2.ID, "admin", nil, "web", ""); err != nil {
		t.Fatalf("demote the second owner: %v", err)
	}
	if _, err := e.m.UpdateAccount(ctx, p, ow2.ID, "owner", nil, "web", ""); err != nil {
		t.Fatal(err)
	}
	// Now make ow2 the only enabled owner from its own side and try to remove it.
	p2, _ := e.m.Authenticate(ctx, login(t, e, "ow2", "right horse").Token)
	if err := e.m.DisableAccount(ctx, p2, p.Admin.ID, "", "web", ""); err != nil {
		t.Fatalf("disable the first owner: %v", err)
	}
	if err := e.st.Write(ctx, func(tx *store.Tx) error { return tx.DisableAdmin(ow2.ID) }); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("disable the last owner: %v", err)
	}
	if err := e.st.Write(ctx, func(tx *store.Tx) error { _, err := tx.DeleteAdmin(ow2.ID); return err }); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("delete the last owner: %v", err)
	}
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, _, err := tx.UpdateAdminRoleScope(ow2.ID, "admin", nil)
		return err
	}); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("demote the last owner: %v", err)
	}
	if err := e.st.DisableAdmin(ctx, ow2.ID); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("CLI disable of the last owner: %v", err)
	}
}

// Every account mutation writes exactly one audit row, and a non-owner is refused.
func TestEveryMutationWritesOneAuditRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	e.addAdmin(t, "ad", "admin", "right horse")
	fullRes := login(t, e, "ow", "right horse")
	p, _ := e.m.Authenticate(ctx, fullRes.Token)
	adp, _ := e.m.Authenticate(ctx, login(t, e, "ad", "right horse").Token)
	var made store.Admin
	var tokID string
	steps := []struct {
		name, action string
		do           func() error
	}{
		{"create", "admin.create", func() error {
			var err error
			made, _, err = e.m.CreateAccount(ctx, p, AccountInput{Username: "newbie", Email: "newbie@example.invalid", Role: "read_only"}, "web", "")
			return err
		}},
		{"update", "admin.update", func() error { _, err := e.m.UpdateAccount(ctx, p, made.ID, "operator", nil, "web", ""); return err }},
		{"disable", "admin.disable", func() error { return e.m.DisableAccount(ctx, p, made.ID, "test", "web", "") }},
		{"enable", "admin.enable", func() error { return e.m.EnableAccount(ctx, p, made.ID, "web", "") }},
		{"reset mfa", "admin.reset_mfa", func() error { return e.m.ResetAccountMFA(ctx, p, made.ID, "lost phone", "web", "") }},
		{"reset password", "admin.password_reset.issue", func() error { _, err := e.m.ResetAccountPassword(ctx, p, made.ID, "web", ""); return err }},
		{"email", "admin.email_change", func() error { return e.m.ChangeEmail(ctx, p, "owner@example.invalid", "web", "") }},
		{"token create", "admin.api_token.create", func() error {
			_, t, err := e.m.CreateAPIToken(ctx, p, "t1", ScopeRead, 7, "web", "")
			tokID = t.ID
			return err
		}},
		{"token revoke", "admin.api_token.revoke", func() error { return e.m.RevokeAPIToken(ctx, p, tokID, "web", "") }},
		{"codes", "admin.recovery_codes", func() error {
			_, err := e.m.RegenerateRecoveryCodes(ctx, p, TOTPCode(e.mfa.secrets["ow"], e.mfa.next()), "web", "")
			return err
		}},
		{"session revoke", "admin.session.revoke", func() error {
			other := login(t, e, "ow", "right horse")
			before := auditCount(t, e.st, "admin.session.revoke")
			_ = before
			op, _ := e.m.Authenticate(ctx, other.Token)
			return e.m.RevokeSession(ctx, p, op.SessionID, "web", "")
		}},
		{"password", "admin.password_change", func() error { return e.m.ChangePassword(ctx, p, "right horse", "a much longer password", "web", "") }},
		{"delete", "admin.delete", func() error { return e.m.DeleteAccount(ctx, p, made.ID, "web", "") }},
	}
	for _, s := range steps {
		before := auditCount(t, e.st, s.action)
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := auditCount(t, e.st, s.action) - before; got != 1 {
			t.Errorf("%s wrote %d %s rows, want 1", s.name, got, s.action)
		}
	}
	// An admin may list but not change accounts.
	if _, err := e.m.ListAccounts(ctx, adp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.m.CreateAccount(ctx, adp, AccountInput{Username: "x", Email: "x@example.invalid", Role: "read_only"}, "web", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin creating an account: %v", err)
	}
}

func TestChangePasswordPolicyAndSessions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	a := login(t, e, "ow", "right horse")
	b := login(t, e, "ow", "right horse")
	p, _ := e.m.Authenticate(ctx, a.Token)
	if err := e.m.ChangePassword(ctx, p, "wrong", "a much longer password", "web", ""); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("wrong current: %v", err)
	}
	if err := e.m.ChangePassword(ctx, p, "right horse", "short", "web", ""); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("short: %v", err)
	}
	if err := e.m.ChangePassword(ctx, p, "right horse", "a much longer password", "web", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Authenticate(ctx, a.Token); err != nil {
		t.Fatalf("the current session must stay: %v", err)
	}
	if _, err := e.m.Authenticate(ctx, b.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("other sessions must end: %v", err)
	}
	if ok, _ := VerifyPassword(mustAdmin(t, e, "ow").PasswordHash, "a much longer password"); !ok {
		t.Fatal("new password not stored")
	}
}

func TestPasswordResetLinkIsSingleUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	p, _ := e.m.Authenticate(ctx, login(t, e, "ow", "right horse").Token)
	made, link, err := e.m.CreateAccount(ctx, p, AccountInput{Username: "newbie", Email: "n@example.invalid", Role: "operator"}, "web", "")
	if err != nil || !strings.HasPrefix(link.Path, ResetPath+"?token=") {
		t.Fatalf("create: %v %+v", err, link)
	}
	if _, err := e.m.Login(ctx, "newbie", "anything at all", "192.0.2.1", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("a new account has no usable password until the link is used")
	}
	if _, err := e.m.CompletePasswordReset(ctx, link.Token, "short", ""); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("policy: %v", err)
	}
	if _, err := e.m.CompletePasswordReset(ctx, link.Token, "newbie password 1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CompletePasswordReset(ctx, link.Token, "newbie password 2", ""); !errors.Is(err, ErrResetInvalid) {
		t.Fatalf("second use: %v", err)
	}
	res, err := e.m.Login(ctx, "newbie", "newbie password 1", "192.0.2.1", "")
	if err != nil || !res.EnrollRequired {
		t.Fatalf("login after reset: %v %+v", err, res)
	}
	// A newer link replaces an older one; links expire.
	l1, _ := e.m.ResetAccountPassword(ctx, p, made.ID, "web", "")
	l2, _ := e.m.ResetAccountPassword(ctx, p, made.ID, "web", "")
	if _, err := e.m.ResetLinkAccount(ctx, l1.Token); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("the older link must be replaced")
	}
	e.clk.Advance(ResetTTL + time.Second)
	if _, err := e.m.ResetLinkAccount(ctx, l2.Token); !errors.Is(err, ErrResetInvalid) {
		t.Fatal("links expire")
	}
}

func TestResetMFATakesTheAccountBackToEnrollment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ow", "owner", "right horse")
	e.addAdmin(t, "op", "operator", "right horse")
	p, _ := e.m.Authenticate(ctx, login(t, e, "ow", "right horse").Token)
	opRes := login(t, e, "op", "right horse")
	if err := e.m.ResetAccountMFA(ctx, p, mustAdmin(t, e, "op").ID, "lost phone", "api", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Authenticate(ctx, opRes.Token); !errors.Is(err, ErrNoSession) {
		t.Fatal("reset-mfa must end the account's sessions")
	}
	res, err := e.m.Login(ctx, "op", "right horse", "192.0.2.1", "")
	if err != nil || !res.EnrollRequired {
		t.Fatalf("after reset-mfa the next login enrolls: %v %+v", err, res)
	}
	if left, total := 0, 0; true {
		_ = e.st.Read(ctx, func(tx *store.Tx) error {
			left, total, err = tx.RecoveryCodeCounts(mustAdmin(t, e, "op").ID)
			return err
		})
		if left != 0 || total != 0 {
			t.Fatalf("recovery codes survive reset-mfa: %d/%d", left, total)
		}
	}
}
