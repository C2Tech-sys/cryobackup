package adminapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/agentapi"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
)

// The tokens an admin creates through the API are the tokens the agent enroll
// path honours: use cap, expiry and revocation are enforced by POST /enroll, not
// only shown in the list.

type agentSide struct {
	t   *testing.T
	srv *httptest.Server
	n   int
}

func newAgentSide(t *testing.T, e *env) *agentSide {
	t.Helper()
	mk, err := escrow.NewMasterKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := &agentapi.Service{Store: e.st, Keys: &escrow.Keys{Master: mk, Store: e.st}, Backend: e.be, GatewayURL: "https://gateway.example.invalid:9443", Clock: e.clk.Now, HtpasswdSync: func() {}}
	mux := http.NewServeMux()
	svc.Register(muxReg{mux})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &agentSide{t: t, srv: srv}
}

// enroll presents the token as an installer would and returns the HTTP status.
func (a *agentSide) enroll(token string) int {
	a.t.Helper()
	a.n++
	req := api.EnrollRequest{Nonce: strings.Repeat(fmt.Sprintf("%02x", a.n), 16), Hostname: fmt.Sprintf("ENROLL-PC-%02d", a.n), OS: "windows", OSVersion: "Windows 11 Pro",
		Arch: api.ArchAMD64, DeviceClass: api.DeviceClassDesktop, HWFingerprint: strings.Repeat("ab", 16), AgentVersion: "1.4.2", ResticVersion: "0.19.1"}
	raw, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", a.srv.URL+api.RouteEnroll, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		a.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestAgentEnrollHonoursUsesExpiryAndRevocationOfConsoleTokens(t *testing.T) {
	e, _ := enrollEnv(t)
	s := e.login("admin", pw)
	agent := newAgentSide(t, e)
	cust := e.customerNamed("Northwind Dental")
	status := func(id string) EnrollTokenResponse {
		_, b := e.get("/api/v1/enrollment-tokens/"+id, s)
		return decode[EnrollTokenResponse](t, b)
	}

	// Use cap: two enrollments, the third refused, the token reads used up.
	capped, code, b := createToken(t, e, s, map[string]any{"customer_id": cust.ID, "max_uses": 2, "expires_in": "1h"})
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	for i := 1; i <= 2; i++ {
		if c := agent.enroll(capped.Token); c != 200 {
			t.Fatalf("enrollment %d with a live token: %d", i, c)
		}
	}
	if c := agent.enroll(capped.Token); c == 200 {
		t.Fatal("a third device enrolled with a token capped at two")
	}
	if got := status(capped.ID); got.Status != "used_up" || got.UsedCount != 2 || got.UsesLeft == nil || *got.UsesLeft != 0 || got.Devices != 2 || got.LastUsedAt == nil {
		t.Fatalf("after two enrollments: %+v", got.EnrollTokenJSON)
	}

	// Expiry: live, then past its time.
	timed, _, _ := createToken(t, e, s, map[string]any{"customer_id": cust.ID, "expires_in": "1h"})
	e.clk.Advance(59 * time.Minute)
	if c := agent.enroll(timed.Token); c != 200 {
		t.Fatalf("enrollment one minute before expiry: %d", c)
	}
	e.clk.Advance(2 * time.Minute)
	if c := agent.enroll(timed.Token); c == 200 {
		t.Fatal("an expired token enrolled a device")
	}
	if got := status(timed.ID); got.Status != "expired" || got.Devices != 1 {
		t.Fatalf("after expiry: %+v", got.EnrollTokenJSON)
	}

	// Revocation: the token worked, is revoked through the API, and stops at once.
	rev, _, _ := createToken(t, e, s, map[string]any{"customer_id": cust.ID})
	if c := agent.enroll(rev.Token); c != 200 {
		t.Fatalf("enrollment before revoke: %d", c)
	}
	if res, b := e.req("DELETE", "/api/v1/enrollment-tokens/"+rev.ID, s, nil); res.StatusCode != 204 {
		t.Fatalf("revoke: %d %s", res.StatusCode, b)
	}
	if c := agent.enroll(rev.Token); c == 200 {
		t.Fatal("a revoked token enrolled a device")
	}
	if got := status(rev.ID); got.Status != "revoked" || got.Devices != 1 || got.UsedCount != 1 {
		t.Fatalf("after revoke: %+v (the enrolled device stays, the use count stays)", got.EnrollTokenJSON)
	}
}
