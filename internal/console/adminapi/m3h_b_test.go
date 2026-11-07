package adminapi

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-HARDEN B: each narrow token opens its one route and nothing else, read or write;
// the read and full tokens keep working as they did.
func TestNarrowTokenScopesOpenTheirOneRoute(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	mk := func(scope string) string {
		t.Helper()
		res, b := e.req("POST", "/api/v1/me/api-tokens", owner, TokenCreateRequest{Name: scope, Scope: scope, Days: 7})
		if res.StatusCode != 201 {
			t.Fatalf("token %s: %d %s", scope, res.StatusCode, b)
		}
		return decode[TokenCreateResponse](t, b).Token
	}
	for _, tc := range []struct {
		scope            string
		status, metrics  int
		fleet            int
		createCustomerOK bool
	}{
		{"status:read", 200, 403, 403, false},
		{"metrics:read", 403, 200, 403, false},
		{"read", 200, 200, 200, false},
		{"full", 200, 200, 200, true},
	} {
		tok := mk(tc.scope)
		get := func(path string) int {
			res, _ := e.req("GET", path, nil, nil, "Authorization", "Bearer "+tok)
			return res.StatusCode
		}
		if got := get("/api/v1/status"); got != tc.status {
			t.Errorf("%s: /api/v1/status = %d, want %d", tc.scope, got, tc.status)
		}
		if got := get("/metrics"); got != tc.metrics {
			t.Errorf("%s: /metrics = %d, want %d", tc.scope, got, tc.metrics)
		}
		if got := get("/api/v1/fleet/summary"); got != tc.fleet {
			t.Errorf("%s: /api/v1/fleet/summary = %d, want %d", tc.scope, got, tc.fleet)
		}
		res, _ := e.req("POST", "/api/v1/customers", nil, map[string]any{"name": "Scope " + tc.scope}, "Authorization", "Bearer "+tok)
		if got := res.StatusCode == 201; got != tc.createCustomerOK {
			t.Errorf("%s: POST /api/v1/customers = %d", tc.scope, res.StatusCode)
		}
	}
	if res, _ := e.req("POST", "/api/v1/me/api-tokens", owner, TokenCreateRequest{Name: "bad", Scope: "status:write", Days: 7}); res.StatusCode < 400 || res.StatusCode >= 500 {
		t.Errorf("an unknown scope answered %d", res.StatusCode)
	}
}

// M3-HARDEN B: the explain text prints the label, not what the agent reported.
func TestExplainPrintsTheSafeVersionLabel(t *testing.T) {
	d := state.DeviceState{}
	d.Detail.AgentVersion, d.Detail.ExpectedVersion = `C:\Users\bob\secret.docx`, "1.2.3"
	got := explainState(d, state.StateOutdated, time.Now())
	if strings.Contains(got, "bob") || !strings.Contains(got, "unknown") || !strings.Contains(got, "1.2.3") {
		t.Fatalf("explain = %q", got)
	}
}

// M3-HARDEN B: a retire whose reads met a busy slot and then ran out of time answers with what
// the reads said, sync_busy, as the page does: not check_timed_out.
func TestRetireAnswerNamesTheBusySlotAsThePageDoes(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	e.svc.SyncGateway, e.svc.GatewaySyncWait = func() {}, 200*time.Millisecond
	e.svc.CheckGatewayLogins = func(context.Context) jobs.LoginFileCheck {
		if calls.Add(1) == 1 {
			return jobs.LoginFileCheck{ReadClass: jobs.LoginFileBusy}
		}
		<-release // the next read outlasts the bound
		return jobs.LoginFileCheck{}
	}
	res, b := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/retire", e.login("admin", pw), DeviceRetireRequest{Reason: "r"})
	if res.StatusCode != 200 {
		t.Fatalf("retire: %d %s", res.StatusCode, b)
	}
	if got := decode[DeviceRetireResponse](t, b); got.GatewayLogin != GatewayLoginPending || got.GatewayLoginReason != jobs.LoginFileBusy {
		t.Fatalf("%+v, want pending with reason %s", got, jobs.LoginFileBusy)
	}
}
