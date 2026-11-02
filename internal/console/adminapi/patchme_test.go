package adminapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
)

// PATCH /api/v1/me is all or nothing: every field is validated before
// anything changes, and a bad e-mail beside a good new password leaves the
// password, the other sessions and the API tokens as they were (foreman
// verification notes, M2-W1A).
func TestPatchMeIsAtomic(t *testing.T) {
	e := newEnv(t)
	a := e.login("admin", "pw-for-tests-1")
	b := e.login("admin", "pw-for-tests-1")
	r, body := e.req("POST", "/api/v1/me/api-tokens", a, TokenCreateRequest{Name: "script", Scope: "read", Days: 30})
	var tc TokenCreateResponse
	if r.StatusCode != 201 || json.Unmarshal(body, &tc) != nil {
		t.Fatalf("token: %d %s", r.StatusCode, body)
	}
	before := len(auditAll(t, e))
	r, body = e.req("PATCH", "/api/v1/me", a, map[string]string{"current_password": "pw-for-tests-1", "new_password": "a brand new password", "email": "not an address"})
	if r.StatusCode != 400 {
		t.Fatalf("bad e-mail beside a good password: %d %s", r.StatusCode, body)
	}
	adm, _ := e.st.GetAdminByUsername(context.Background(), "admin")
	if ok, _ := auth.VerifyPassword(adm.PasswordHash, "pw-for-tests-1"); !ok {
		t.Fatal("the password changed although the request was refused")
	}
	if r, _ := e.req("GET", "/api/v1/me", b, nil); r.StatusCode != 200 {
		t.Fatalf("the other session ended although the request was refused: %d", r.StatusCode)
	}
	if r, _ := e.req("GET", "/api/v1/me", nil, nil, "Authorization", "Bearer "+tc.Token); r.StatusCode != 200 {
		t.Fatalf("the token was revoked although the request was refused: %d", r.StatusCode)
	}
	if n := len(auditAll(t, e)) - before; n != 0 {
		t.Fatalf("a refused PATCH wrote %d audit rows", n)
	}
	// Both fields valid: both change in one go, one audit row each.
	r, body = e.req("PATCH", "/api/v1/me", a, map[string]string{"current_password": "pw-for-tests-1", "new_password": "a brand new password", "email": "owner@example.invalid"})
	if r.StatusCode != 200 {
		t.Fatalf("valid PATCH: %d %s", r.StatusCode, body)
	}
	adm, _ = e.st.GetAdminByUsername(context.Background(), "admin")
	if ok, _ := auth.VerifyPassword(adm.PasswordHash, "a brand new password"); !ok || adm.Email != "owner@example.invalid" {
		t.Fatal("valid PATCH did not apply both fields")
	}
}
