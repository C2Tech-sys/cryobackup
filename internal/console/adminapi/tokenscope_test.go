package adminapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// Credential management on /me is for a signed-in session only: neither a
// read-scope nor a full-scope API token may change the password or e-mail,
// enroll TOTP, regenerate recovery codes, revoke a session, or create or
// revoke a token (foreman review, M2-W1A).
func TestMutatingMeRoutesRefuseAPITokens(t *testing.T) {
	e := newEnv(t)
	ow := e.login("admin", "pw-for-tests-1")
	other := e.login("admin", "pw-for-tests-1")
	mk := func(name, scope string) (string, string) {
		r, b := e.req("POST", "/api/v1/me/api-tokens", ow, TokenCreateRequest{Name: name, Scope: scope, Days: 30})
		var out TokenCreateResponse
		if r.StatusCode != 201 || json.Unmarshal(b, &out) != nil {
			t.Fatalf("create %s: %d %s", name, r.StatusCode, b)
		}
		return out.Token, out.APIToken.ID
	}
	readTok, _ := mk("reader", "read")
	fullTok, _ := mk("full", "full")
	_, victimTok := mk("victim", "read")
	_, sb := e.req("GET", "/api/v1/me/sessions", other, nil)
	var sl SessionsResponse
	_ = json.Unmarshal(sb, &sl)
	otherID := ""
	for _, x := range sl.Sessions {
		if x.Current {
			otherID = x.ID
		}
	}
	if otherID == "" {
		t.Fatal("no current session listed")
	}
	routes := []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/api/v1/me", map[string]string{"email": "x@example.invalid"}},
		{"POST", "/api/v1/me/totp/begin", nil},
		{"POST", "/api/v1/me/totp/confirm", TOTPConfirmRequest{Code: "123456"}},
		{"POST", "/api/v1/me/recovery-codes", CodeRequest{Code: "123456"}},
		{"DELETE", "/api/v1/me/sessions/" + otherID, nil},
		{"POST", "/api/v1/me/api-tokens", TokenCreateRequest{Name: "minted", Scope: "read", Days: 7}},
		{"DELETE", "/api/v1/me/api-tokens/" + victimTok, nil},
	}
	for name, tok := range map[string]string{"read": readTok, "full": fullTok} {
		for _, rt := range routes {
			r, b := e.req(rt.method, rt.path, nil, rt.body, "Authorization", "Bearer "+tok)
			if r.StatusCode != 403 || problemCode(t, r, b) != api.CodeForbidden {
				t.Errorf("%s %s with a %s token: %d, want 403", rt.method, rt.path, name, r.StatusCode)
			}
		}
	}
	// Nothing changed: the other session and the victim token still work.
	if r, _ := e.req("GET", "/api/v1/me", other, nil); r.StatusCode != 200 {
		t.Fatalf("the other session was revoked by a token: %d", r.StatusCode)
	}
	_, lb := e.req("GET", "/api/v1/me/api-tokens", ow, nil)
	if strings.Count(string(lb), `"state":"active"`) != 3 || strings.Contains(string(lb), `"name":"minted"`) {
		t.Fatalf("a token was revoked or minted by a token: %s", lb)
	}
}
