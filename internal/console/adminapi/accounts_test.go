package adminapi

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// mfaState is the test side of the MFA step: a TOTP clock that moves one step
// per code (so consecutive logins never replay a code), and the secrets of the
// accounts it enrolled.
type mfaState struct {
	mu      sync.Mutex
	at      time.Time
	secrets map[string][]byte
	codes   map[string][]string
}

func newMFA(am *auth.Manager, mk *escrow.MasterKey) *mfaState {
	m := &mfaState{at: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), secrets: map[string][]byte{}, codes: map[string][]string{}}
	am.Master = mk
	am.TOTPNow = func() time.Time { m.mu.Lock(); defer m.mu.Unlock(); return m.at }
	return m
}

func (m *mfaState) code(user string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at = m.at.Add(auth.TOTPPeriod)
	return auth.TOTPCode(m.secrets[user], m.at)
}

func mergeCookies(old, fresh []*http.Cookie) []*http.Cookie {
	out := append([]*http.Cookie(nil), fresh...)
	for _, c := range old {
		dup := false
		for _, f := range fresh {
			dup = dup || f.Name == c.Name
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// completeMFA finishes the login the password step began: the forced
// enrollment through /me/totp/begin and /confirm for a new account, the TOTP
// step for an enrolled one. It returns the full session.
func (e *env) completeMFA(user string, s *session, loginBody []byte) *session {
	e.t.Helper()
	var lr LoginResponse
	_ = json.Unmarshal(loginBody, &lr)
	if !lr.MFARequired {
		e.t.Fatalf("the password step must require MFA: %s", loginBody)
	}
	if lr.MFAEnrollmentRequired {
		res, b := e.req("POST", "/api/v1/me/totp/begin", s, nil)
		var tb TOTPBeginResponse
		if res.StatusCode != 200 || json.Unmarshal(b, &tb) != nil || !strings.HasPrefix(tb.OTPAuthURI, "otpauth://totp/") {
			e.t.Fatalf("totp begin: %d %s", res.StatusCode, b)
		}
		secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(tb.Secret, " ", ""))
		if err != nil {
			e.t.Fatal(err)
		}
		e.mfa.mu.Lock()
		e.mfa.secrets[user] = secret
		e.mfa.mu.Unlock()
		res, b = e.req("POST", "/api/v1/me/totp/confirm", s, TOTPConfirmRequest{Code: e.mfa.code(user)})
		var rc RecoveryCodesResponse
		if res.StatusCode != 200 || json.Unmarshal(b, &rc) != nil || !rc.LoginComplete || len(rc.RecoveryCodes) != auth.RecoveryCodeCount {
			e.t.Fatalf("totp confirm: %d %s", res.StatusCode, b)
		}
		e.mfa.mu.Lock()
		e.mfa.codes[user] = rc.RecoveryCodes
		e.mfa.mu.Unlock()
		return &session{cookies: mergeCookies(s.cookies, res.Cookies()), csrf: rc.CSRFToken}
	}
	res, b := e.req("POST", "/api/v1/auth/mfa/totp", s, CodeRequest{Code: e.mfa.code(user)})
	if res.StatusCode != 200 {
		e.t.Fatalf("mfa totp: %d %s", res.StatusCode, b)
	}
	return &session{cookies: mergeCookies(s.cookies, res.Cookies()), csrf: csrfFrom(e.t, b)}
}

// passwordOnly is the password step alone (a pending session).
func (e *env) passwordOnly(user, pw string) (*session, LoginResponse) {
	e.t.Helper()
	res, body := e.req("POST", "/api/v1/auth/login", nil, LoginRequest{Username: user, Password: pw})
	if res.StatusCode != 200 {
		e.t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	var lr LoginResponse
	_ = json.Unmarshal(body, &lr)
	return &session{cookies: res.Cookies(), csrf: lr.CSRFToken}, lr
}

// TestAPIPasswordAloneIsNotALogin uses only the M0 routes, so it also runs
// against the code before M2-W1A, where it fails.
func TestAPIPasswordAloneIsNotALogin(t *testing.T) {
	e := newEnv(t)
	res, body := e.req("POST", "/api/v1/auth/login", nil, LoginRequest{Username: "admin", Password: "pw-for-tests-1"})
	if res.StatusCode != 200 {
		t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	s := &session{cookies: res.Cookies(), csrf: csrfFrom(t, body)}
	for _, p := range []string{"/api/v1/version", "/api/v1/devices"} {
		if r, b := e.req("GET", p, s, nil); r.StatusCode != 401 {
			t.Fatalf("%s with a password-only session: %d %s", p, r.StatusCode, b)
		}
	}
	if r, _ := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/restores", s, CreateRestoreRequest{SnapshotID: "x", Mode: "download_zip", Path: "/x"}); r.StatusCode != 401 {
		t.Fatalf("restore with a password-only session: %d", r.StatusCode)
	}
}

func TestAPIMFAStep(t *testing.T) {
	e := newEnv(t)
	full := e.login("admin", "pw-for-tests-1") // enrolls
	if r, _ := e.req("GET", "/api/v1/me", full, nil); r.StatusCode != 200 {
		t.Fatalf("me: %d", r.StatusCode)
	}
	s, lr := e.passwordOnly("admin", "pw-for-tests-1")
	if !lr.MFARequired || lr.MFAEnrollmentRequired {
		t.Fatalf("second login: %+v", lr)
	}
	if r, b := e.req("POST", "/api/v1/me/totp/begin", s, nil); r.StatusCode != 401 {
		t.Fatalf("enrollment from the pending session of an enrolled account: %d %s", r.StatusCode, b)
	}
	if r, b := e.req("POST", "/api/v1/auth/mfa/totp", s, CodeRequest{Code: "000000"}); r.StatusCode != 401 || problemCode(t, r, b) != api.CodeUnauthorized {
		t.Fatalf("wrong code: %d", r.StatusCode)
	}
	code := e.mfa.code("admin")
	r, b := e.req("POST", "/api/v1/auth/mfa/totp", s, CodeRequest{Code: code})
	if r.StatusCode != 200 {
		t.Fatalf("right code: %d %s", r.StatusCode, b)
	}
	var done LoginResponse
	_ = json.Unmarshal(b, &done)
	if done.MFARequired || done.CSRFToken == s.csrf {
		t.Fatalf("completed login: %+v", done)
	}
	// The same code again, on a fresh password step: replay.
	s2, _ := e.passwordOnly("admin", "pw-for-tests-1")
	if r, _ := e.req("POST", "/api/v1/auth/mfa/totp", s2, CodeRequest{Code: code}); r.StatusCode != 401 {
		t.Fatalf("replayed code: %d", r.StatusCode)
	}
	// A recovery code works once.
	rc := e.mfa.codes["admin"][0]
	if r, b := e.req("POST", "/api/v1/auth/mfa/recovery", s2, CodeRequest{Code: rc}); r.StatusCode != 200 || !strings.Contains(string(b), `"recovery_codes_left":9`) {
		t.Fatalf("recovery: %d %s", r.StatusCode, b)
	}
	s3, _ := e.passwordOnly("admin", "pw-for-tests-1")
	if r, _ := e.req("POST", "/api/v1/auth/mfa/recovery", s3, CodeRequest{Code: rc}); r.StatusCode != 401 {
		t.Fatalf("second use of a recovery code: %d", r.StatusCode)
	}
	// MFA routes refuse a full session.
	if r, _ := e.req("POST", "/api/v1/auth/mfa/totp", full, CodeRequest{Code: "123456"}); r.StatusCode != 409 {
		t.Fatalf("mfa step with a full session: %d", r.StatusCode)
	}
}

type roleSessions map[string]*session

// sessions logs in one account per role (the fixture admin is the owner).
func (e *env) sessionsByRole() roleSessions {
	e.addAdmin("ro", "read_only", nil)
	e.addAdmin("op", "operator", nil)
	e.addAdmin("ad", "admin", nil)
	return roleSessions{"RO": e.login("ro", "pw-for-tests-1"), "OP": e.login("op", "pw-for-tests-1"), "AD": e.login("ad", "pw-for-tests-1"),
		"OW": e.login("admin", "pw-for-tests-1")}
}

var roleRank = map[string]int{"RO": 1, "OP": 2, "AD": 3, "OW": 4, "SELF": 1}

// TestAccountRoutesRoleMatrix: every account route answers 403 to each role
// below its minimum (AD, OW) and never 403 to a role at or above it; SELF
// routes serve every role; PENDING routes refuse a full session.
func TestAccountRoutesRoleMatrix(t *testing.T) {
	e := newEnv(t)
	ss := e.sessionsByRole()
	target := e.st // a victim account for the {id} routes
	victim, err := target.CreateAdmin(context.Background(), store.Admin{Username: "victim", Email: "v@example.invalid", Role: "read_only", PasswordHash: "!"})
	if err != nil {
		t.Fatal(err)
	}
	for pattern, min := range AccountPatterns() {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.ReplaceAll(path, "{id}", victim.ID)
		for role, s := range ss {
			res, b := e.req(method, path, s, map[string]string{})
			switch {
			case min == "PENDING":
				if res.StatusCode != 409 {
					t.Errorf("%s as %s (full session): %d, want 409", pattern, role, res.StatusCode)
				}
			case roleRank[role] < roleRank[min]:
				if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
					t.Errorf("%s as %s: %d, want 403", pattern, role, res.StatusCode)
				}
			default:
				// Served: never 403. A 401 is the handler's "code incorrect" (the
				// routes that take a TOTP code), so the session must still be valid.
				if res.StatusCode == 403 {
					t.Errorf("%s as %s: 403, want the route to serve it", pattern, role)
				}
				if res.StatusCode == 401 {
					if me, _ := e.req("GET", "/api/v1/me", s, nil); me.StatusCode != 200 {
						t.Errorf("%s as %s: 401 from the gate, want the route to serve it", pattern, role)
					}
				}
			}
		}
	}
}

func TestAPITokensEndToEnd(t *testing.T) {
	e := newEnv(t)
	ow := e.login("admin", "pw-for-tests-1")
	mk := func(name, scope string) (string, TokenJSON) {
		r, b := e.req("POST", "/api/v1/me/api-tokens", ow, TokenCreateRequest{Name: name, Scope: scope, Days: 30})
		var out TokenCreateResponse
		if r.StatusCode != 201 || json.Unmarshal(b, &out) != nil || !strings.HasPrefix(out.Token, "cbk_") || out.APIToken.Prefix != out.Token[:8] {
			t.Fatalf("create %s: %d %s", name, r.StatusCode, b)
		}
		return out.Token, out.APIToken
	}
	readTok, _ := mk("reader", "read")
	fullTok, fullJSON := mk("full", "full")
	bearer := func(method, path, tok string, body any) (*http.Response, []byte) {
		return e.req(method, path, nil, body, "Authorization", "Bearer "+tok)
	}
	if r, b := bearer("GET", "/api/v1/me", readTok, nil); r.StatusCode != 200 || !strings.Contains(string(b), `"effective_role":"read_only"`) || !strings.Contains(string(b), `"via_token":true`) {
		t.Fatalf("me with a read token: %d %s", r.StatusCode, b)
	}
	if r, _ := bearer("GET", "/api/v1/admins", readTok, nil); r.StatusCode != 403 {
		t.Fatalf("admins with a read token: %d", r.StatusCode)
	}
	if r, _ := bearer("GET", "/api/v1/admins", fullTok, nil); r.StatusCode != 200 {
		t.Fatalf("admins with a full owner token: %d", r.StatusCode)
	}
	// A Bearer request needs no CSRF token and no Origin: a full owner token
	// reads one account without them. It cannot create one (session only).
	if r, b := bearer("GET", "/api/v1/admins", fullTok, nil); r.StatusCode != 200 || !strings.Contains(string(b), `"username":"admin"`) {
		t.Fatalf("list admins with a token: %d %s", r.StatusCode, b)
	}
	if r, _ := bearer("POST", "/api/v1/admins", fullTok, AccountRequest{Username: "viatoken", Email: "t@example.invalid", Role: "read_only"}); r.StatusCode != 403 {
		t.Fatalf("create admin with a token: %d", r.StatusCode)
	}
	// A token cannot mint tokens or change the password.
	if r, _ := bearer("POST", "/api/v1/me/api-tokens", fullTok, TokenCreateRequest{Name: "x", Scope: "read", Days: 7}); r.StatusCode != 403 {
		t.Fatalf("token minting a token: %d", r.StatusCode)
	}
	if r, _ := bearer("PATCH", "/api/v1/me", fullTok, map[string]string{"email": "x@example.invalid"}); r.StatusCode != 403 {
		t.Fatalf("token patching me: %d", r.StatusCode)
	}
	// The list shows prefixes, never secrets.
	_, lb := e.req("GET", "/api/v1/me/api-tokens", ow, nil)
	if strings.Contains(string(lb), readTok) || strings.Contains(string(lb), fullTok) || !strings.Contains(string(lb), fullJSON.Prefix) {
		t.Fatalf("token list leaks a secret or lacks the prefix: %s", lb)
	}
	// Revocation is immediate.
	if r, _ := e.req("DELETE", "/api/v1/me/api-tokens/"+fullJSON.ID, ow, nil); r.StatusCode != 204 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if r, _ := bearer("GET", "/api/v1/me", fullTok, nil); r.StatusCode != 401 {
		t.Fatalf("revoked token: %d", r.StatusCode)
	}
	// Expiry.
	e.clk.Advance(31 * 24 * time.Hour)
	if r, _ := bearer("GET", "/api/v1/me", readTok, nil); r.StatusCode != 401 {
		t.Fatalf("expired token: %d", r.StatusCode)
	}
}

func TestAPIAccountsAdministration(t *testing.T) {
	e := newEnv(t)
	ow := e.login("admin", "pw-for-tests-1")
	me := func() string {
		_, b := e.req("GET", "/api/v1/me", ow, nil)
		var m MeJSON
		_ = json.Unmarshal(b, &m)
		return m.ID
	}()
	audits := func(action string) int {
		l, _ := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action}, store.Page{Limit: 500})
		return len(l.Items)
	}
	r, b := e.req("POST", "/api/v1/admins", ow, AccountRequest{Username: "tech1", Email: "tech1@example.invalid", Role: "operator", CustomerScope: []string{"no-such-customer"}})
	if r.StatusCode != 400 {
		t.Fatalf("unknown customer in scope: %d %s", r.StatusCode, b)
	}
	cs, _ := e.st.ListCustomers(context.Background(), store.ScopeAll())
	r, b = e.req("POST", "/api/v1/admins", ow, AccountRequest{Username: "tech1", Email: "tech1@example.invalid", Role: "operator", CustomerScope: []string{cs[0].ID}})
	var created ResetLinkResponse
	if r.StatusCode != 201 || json.Unmarshal(b, &created) != nil || created.Admin == nil || len(created.Admin.CustomerScope) != 1 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	id := created.Admin.ID
	if r, _ := e.req("POST", "/api/v1/admins", ow, AccountRequest{Username: "tech1", Email: "x@example.invalid", Role: "operator"}); r.StatusCode != 409 {
		t.Fatalf("duplicate username: %d", r.StatusCode)
	}
	if r, _ := e.req("POST", "/api/v1/admins", ow, AccountRequest{Username: "boss", Email: "b@example.invalid", Role: "admin", CustomerScope: []string{cs[0].ID}}); r.StatusCode != 400 {
		t.Fatalf("a scoped admin: %d", r.StatusCode)
	}
	for _, step := range []struct {
		method, path, action string
		body                 any
		want                 int
	}{
		{"PATCH", "/api/v1/admins/" + id, "admin.update", AccountRequest{Role: "read_only"}, 200},
		{"POST", "/api/v1/admins/" + id + "/disable", "admin.disable", ReasonRequest{Reason: "on leave"}, 204},
		{"POST", "/api/v1/admins/" + id + "/enable", "admin.enable", nil, 204},
		{"POST", "/api/v1/admins/" + id + "/reset-mfa", "admin.reset_mfa", nil, 204},
		{"POST", "/api/v1/admins/" + id + "/reset-password", "admin.password_reset.issue", nil, 200},
		{"DELETE", "/api/v1/admins/" + id, "admin.delete", nil, 204},
	} {
		before := audits(step.action)
		if r, b := e.req(step.method, step.path, ow, step.body); r.StatusCode != step.want {
			t.Fatalf("%s %s: %d %s", step.method, step.path, r.StatusCode, b)
		}
		if n := audits(step.action) - before; n != 1 {
			t.Errorf("%s wrote %d audit rows", step.action, n)
		}
	}
	// The owner cannot act on itself here, and the last owner stays.
	for _, p := range []string{"/disable", "/reset-mfa", "/reset-password"} {
		if r, _ := e.req("POST", "/api/v1/admins/"+me+p, ow, nil); r.StatusCode != 409 {
			t.Errorf("%s on self: %d", p, r.StatusCode)
		}
	}
	if r, _ := e.req("DELETE", "/api/v1/admins/"+me, ow, nil); r.StatusCode != 409 {
		t.Fatalf("delete self: %d", r.StatusCode)
	}
	if r, _ := e.req("GET", "/api/v1/admins/"+id, ow, nil); r.StatusCode != 404 {
		t.Fatalf("deleted account: %d", r.StatusCode)
	}
}

func TestAPIMeSessionsAndPassword(t *testing.T) {
	e := newEnv(t)
	a := e.login("admin", "pw-for-tests-1")
	b := e.login("admin", "pw-for-tests-1")
	r, body := e.req("GET", "/api/v1/me/sessions", a, nil)
	var sl SessionsResponse
	if r.StatusCode != 200 || json.Unmarshal(body, &sl) != nil || len(sl.Sessions) != 2 {
		t.Fatalf("sessions: %d %s", r.StatusCode, body)
	}
	if r, _ := e.req("PATCH", "/api/v1/me", a, map[string]string{"current_password": "nope", "new_password": "another long password"}); r.StatusCode != 401 {
		t.Fatalf("wrong current password: %d", r.StatusCode)
	}
	if r, _ := e.req("PATCH", "/api/v1/me", a, map[string]string{"current_password": "pw-for-tests-1", "new_password": "short"}); r.StatusCode != 400 {
		t.Fatalf("short password: %d", r.StatusCode)
	}
	if r, b := e.req("PATCH", "/api/v1/me", a, map[string]string{"current_password": "pw-for-tests-1", "new_password": "another long password"}); r.StatusCode != 200 {
		t.Fatalf("change password: %d %s", r.StatusCode, b)
	}
	if r, _ := e.req("GET", "/api/v1/me", b, nil); r.StatusCode != 401 {
		t.Fatalf("other session after a password change: %d", r.StatusCode)
	}
	if r, _ := e.req("GET", "/api/v1/me", a, nil); r.StatusCode != 200 {
		t.Fatalf("this session after a password change: %d", r.StatusCode)
	}
}
