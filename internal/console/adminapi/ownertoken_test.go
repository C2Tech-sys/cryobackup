package adminapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// An owner's API token, even with full scope, cannot create accounts, change
// roles or scopes, disable, enable or delete accounts, or reset another
// account's MFA or password: those mint or reset credentials and need a
// signed-in owner session (foreman review 2, M2-W1A). The cookie session of
// the same owner still can.
func TestOwnerCredentialRoutesRefuseAPITokens(t *testing.T) {
	e := newEnv(t)
	ow := e.login("admin", "pw-for-tests-1")
	r, b := e.req("POST", "/api/v1/me/api-tokens", ow, TokenCreateRequest{Name: "owner full", Scope: "full", Days: 30})
	var tc TokenCreateResponse
	if r.StatusCode != 201 || json.Unmarshal(b, &tc) != nil {
		t.Fatalf("token: %d %s", r.StatusCode, b)
	}
	victim, err := e.st.CreateAdmin(context.Background(), store.Admin{Username: "victim", Email: "v@example.invalid", Role: "read_only", PasswordHash: "!"})
	if err != nil {
		t.Fatal(err)
	}
	id := victim.ID
	routes := []struct {
		method, path string
		body         any
		ok           int
	}{
		{"POST", "/api/v1/admins", AccountRequest{Username: "minted", Email: "m@example.invalid", Role: "read_only"}, 201},
		{"PATCH", "/api/v1/admins/" + id, AccountRequest{Role: "operator"}, 200},
		{"POST", "/api/v1/admins/" + id + "/disable", nil, 204},
		{"POST", "/api/v1/admins/" + id + "/enable", nil, 204},
		{"POST", "/api/v1/admins/" + id + "/reset-mfa", nil, 204},
		{"POST", "/api/v1/admins/" + id + "/reset-password", nil, 200},
		{"DELETE", "/api/v1/admins/" + id, nil, 204},
	}
	before := len(auditAll(t, e))
	for _, rt := range routes {
		r, b := e.req(rt.method, rt.path, nil, rt.body, "Authorization", "Bearer "+tc.Token)
		if r.StatusCode != 403 || problemCode(t, r, b) != api.CodeForbidden {
			t.Errorf("%s %s with a full owner token: %d, want 403", rt.method, rt.path, r.StatusCode)
		}
	}
	if n := len(auditAll(t, e)) - before; n != 0 {
		t.Fatalf("refused token requests changed something: %d audit rows", n)
	}
	// Reading stays open to the token; the cookie session of the same owner can change.
	if r, _ := e.req("GET", "/api/v1/admins/"+id, nil, nil, "Authorization", "Bearer "+tc.Token); r.StatusCode != 200 {
		t.Fatalf("read with the token: %d", r.StatusCode)
	}
	for _, rt := range routes {
		if r, b := e.req(rt.method, rt.path, ow, rt.body); r.StatusCode != rt.ok {
			t.Errorf("%s %s with the owner session: %d %s", rt.method, rt.path, r.StatusCode, b)
		}
	}
}

func auditAll(t *testing.T, e *env) []store.AuditEvent {
	t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}
