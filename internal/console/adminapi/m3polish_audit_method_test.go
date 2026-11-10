package adminapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
)

// M3-POLISH-A: the audit row's Method says which door the change came through.
// A console page acts through the admin API in process (ui.Service.apiSend),
// and that must read "web"; a real network call to the same routes reads "api".

// viaPage points the env's HTTP client at a copy of the admin API whose requests
// carry the marker apiSend sets, standing in for the page layer.
func (e *env) viaPage() {
	e.t.Helper()
	mux := http.NewServeMux()
	e.svc.Register(muxReg{mux})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(httpx.WithInProcessOrigin(r.Context())))
	}))
	e.t.Cleanup(srv.Close)
	e.srv = srv
}

// retireAndDeleteToken retires FRONTDESK-01 and creates, revokes and deletes a
// token through whatever door e.srv is, and returns the audit Method of each row.
func retireAndDeleteToken(t *testing.T, e *env) (retire, retireSync, tokDelete string) {
	t.Helper()
	owner := e.login("admin", pw)
	dev := e.deviceNamed("FRONTDESK-01")
	e.mustReq("POST", "/api/v1/devices/"+dev.ID+"/retire", owner, DeviceRetireRequest{Reason: "dead lab machine"}, 200)
	one := func(action, id string) string {
		t.Helper()
		rows := e.auditOf(action, id)
		if len(rows) != 1 {
			t.Fatalf("%s: %d audit rows, want 1", action, len(rows))
		}
		return rows[0].Method
	}
	retire, retireSync = one("device.retire", dev.ID), one("device.retire_sync", dev.ID)

	cust := e.customerNamed("Summit Eye Care")
	tok, code, b := createToken(t, e, owner, map[string]any{"customer_id": cust.ID, "label": "to delete"})
	if code != 201 {
		t.Fatalf("create token: %d %s", code, b)
	}
	e.mustReq("DELETE", "/api/v1/enrollment-tokens/"+tok.ID, owner, nil, 204)
	e.mustReq("POST", "/api/v1/enrollment-tokens/"+tok.ID+"/delete", owner, nil, 204)
	return retire, retireSync, one("enrollment_token.delete", tok.ID)
}

func TestAuditMethodIsAPIForARealAdminAPICall(t *testing.T) {
	e, _ := enrollEnv(t)
	retire, sync, del := retireAndDeleteToken(t, e)
	if retire != "api" || sync != "api" || del != "api" {
		t.Fatalf("network API call: retire %q, retire_sync %q, token delete %q; want api for all", retire, sync, del)
	}
}

func TestAuditMethodIsWebWhenAPageMakesTheCallInProcess(t *testing.T) {
	e, _ := enrollEnv(t)
	e.viaPage()
	retire, sync, del := retireAndDeleteToken(t, e)
	if retire != "web" || sync != "web" || del != "web" {
		t.Fatalf("in-process page call: retire %q, retire_sync %q, token delete %q; want web for all", retire, sync, del)
	}
}

// A network client that sends headers claiming to be the page still audits "api".
func TestNoHeaderMakesANetworkCallAuditAsWeb(t *testing.T) {
	e, _ := enrollEnv(t)
	owner := e.login("admin", pw)
	dev := e.deviceNamed("FRONTDESK-01")
	e.mustReq("POST", "/api/v1/devices/"+dev.ID+"/retire", owner, DeviceRetireRequest{Reason: "forged"}, 200)
	// e.req takes extra headers; mustReq has none, so replay the delete with them.
	cust := e.customerNamed("Summit Eye Care")
	tok, _, _ := createToken(t, e, owner, map[string]any{"customer_id": cust.ID, "label": "forge"})
	e.mustReq("DELETE", "/api/v1/enrollment-tokens/"+tok.ID, owner, nil, 204)
	res, b := e.req("POST", "/api/v1/enrollment-tokens/"+tok.ID+"/delete", owner, nil,
		"X-Audit-Method", "web", "X-In-Process-Origin", "true", "X-Origin", "web", "Sec-Fetch-Site", "same-origin")
	if res.StatusCode != 204 {
		t.Fatalf("delete with forged headers: %d %s", res.StatusCode, b)
	}
	if rows := e.auditOf("enrollment_token.delete", tok.ID); len(rows) != 1 || rows[0].Method != "api" {
		t.Fatalf("forged headers: audit rows %+v, want one with Method api", rows)
	}
}
