package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T6: device retire and enrollment-token delete through the admin API, with the
// real agent API and the real htpasswd job on the other side of the retire.

// agentCall is one request of the enrolled agent, with its device token; it
// returns the status.
func (a *agentSide) agentCall(method, route, token string, body any) int {
	a.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r, _ := http.NewRequest(method, a.srv.URL+route, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		a.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// enrollFull enrolls one machine and returns everything the agent was given.
func (a *agentSide) enrollFull(token, nonce, host string) (api.EnrollResponse, api.EnrollRequest) {
	a.t.Helper()
	req := api.EnrollRequest{Nonce: nonce, Hostname: host, OS: "windows", OSVersion: "Windows 11 Pro", Arch: api.ArchAMD64, DeviceClass: api.DeviceClassDesktop,
		HWFingerprint: strings.Repeat("ab", 16), AgentVersion: "1.4.2", ResticVersion: "0.19.1"}
	raw, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", a.srv.URL+api.RouteEnroll, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		a.t.Fatalf("enroll: %d", res.StatusCode)
	}
	var out api.EnrollResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		a.t.Fatal(err)
	}
	return out, req
}

// retireHtpasswd is the deployed gateway login file and the real job that renders it.
func retireHtpasswd(t *testing.T, e *env) (*jobs.HtpasswdSync, func() []gateway.Login) {
	t.Helper()
	mk, err := escrow.NewMasterKey(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	job := &jobs.HtpasswdSync{Store: e.st, Master: mk, Dir: t.TempDir()}
	read := func() []gateway.Login {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(job.Dir, gateway.FileName))
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatal("the job deployed no htpasswd file")
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _, logins, err := gateway.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		return logins
	}
	return job, read
}

// The ticket's acceptance: a retired device's credentials stop working against the
// real agent API and in the generated htpasswd, and the retire signals that sync.
func TestRetireCutsOffTheRealAgentAPIAndTheGeneratedHtpasswd(t *testing.T) {
	e, _ := enrollEnv(t)
	owner := e.login("admin", pw)
	agent := newAgentSide(t, e)
	cust := e.customerNamed("Summit Eye Care")
	tok, code, b := createToken(t, e, owner, map[string]any{"customer_id": cust.ID, "expires_in": "1h"})
	if code != 201 {
		t.Fatalf("create token: %d %s", code, b)
	}
	nonce := strings.Repeat("c1", 16)
	enrolled, _ := agent.enrollFull(tok.Token, nonce, "RETIRE-ME-01")
	device := string(enrolled.DeviceToken)
	hb := api.HeartbeatRequest{AgentVersion: "1.4.2", ResticVersion: "0.19.1", UptimeS: 60, DeviceTime: e.clk.Now(), ServiceState: api.ServiceIdle, HWFingerprint: strings.Repeat("ab", 16)}

	// Before: the same calls work, and the login is in the file with the password the agent holds.
	if c := agent.agentCall("POST", api.RouteHeartbeat, device, hb); c != 200 {
		t.Fatalf("heartbeat before the retire: %d", c)
	}
	if c := agent.agentCall("GET", api.RoutePolicy, device, nil); c != 200 {
		t.Fatalf("policy before the retire: %d", c)
	}
	job, read := retireHtpasswd(t, e)
	ctx := context.Background()
	if err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var hash string
	for _, l := range read() {
		if l.Username == enrolled.DeviceID {
			hash = l.Hash
		}
	}
	if hash == "" || !gateway.CheckPassword(hash, string(enrolled.Gateway.Password)) {
		t.Fatalf("before the retire the login must be in the file and accept the agent's password (hash %q)", hash)
	}

	// The real runner runs the real job, so the wait sees the run it records, as in serve.
	runner := jobs.NewRunner(e.st, "m3t6-test", logx.New(io.Discard, logx.LevelError))
	runner.Add(job.Job())
	synced := 0
	e.svc.SyncGateway = func() {
		synced++
		if _, err := runner.RunOnce(ctx, jobs.NameHtpasswdSync); err != nil {
			t.Error(err)
		}
	}
	res, body := e.req("POST", "/api/v1/devices/"+enrolled.DeviceID+"/retire", owner, DeviceRetireRequest{Reason: "dead lab machine"})
	if res.StatusCode != 200 {
		t.Fatalf("retire: %d %s", res.StatusCode, body)
	}
	got := decode[DeviceRetireResponse](t, body)
	if !got.Retired || got.State != "RETIRED" || !got.Changed || got.TokensRevoked != 1 || got.LoginsRevoked != 1 || got.GatewayLogin != GatewayLoginRemoved {
		t.Fatalf("retire answered %+v", got)
	}
	if synced != 1 {
		t.Errorf("the gateway sync was signalled %d times, want once", synced)
	}

	// After: every agent call refuses the token, at once and with the same 401.
	for _, c := range []struct {
		name, method, route string
		body                any
	}{
		{"heartbeat", "POST", api.RouteHeartbeat, hb},
		{"policy", "GET", api.RoutePolicy, nil},
		{"commands", "GET", api.RouteCommands, nil},
	} {
		if s := agent.agentCall(c.method, c.route, device, c.body); s != 401 {
			t.Errorf("%s with the retired device's token: %d, want 401", c.name, s)
		}
	}
	// The enrollment nonce cannot bring it back either (DH17: "device was retired").
	if c := agent.enrollWithNonce(tok.Token, nonce, "RETIRE-ME-01"); c == 200 {
		t.Error("an enrollment replay brought the retired device's credentials back")
	}

	// The real job dropped the login from the real file before the answer; the probe login and the others stay.
	after := read()
	probe := false
	for _, l := range after {
		if l.Username == enrolled.DeviceID {
			t.Error("the retired device's login is still in the generated htpasswd")
		}
		if gateway.CheckPassword(l.Hash, string(enrolled.Gateway.Password)) {
			t.Error("the retired device's password still opens a line of the generated htpasswd")
		}
		probe = probe || l.Username == gateway.ProbeUsername
	}
	if !probe || len(after) < 2 {
		t.Errorf("the file should keep the probe and the other logins: %d lines, probe %v", len(after), probe)
	}

	// Idempotent: the same state, no second signal, and no second audit row.
	res, body = e.req("POST", "/api/v1/devices/"+enrolled.DeviceID+"/retire", owner, DeviceRetireRequest{Reason: "again"})
	if res.StatusCode != 200 {
		t.Fatalf("second retire: %d %s", res.StatusCode, body)
	}
	again := decode[DeviceRetireResponse](t, body)
	if again.Changed || !again.Retired || again.State != "RETIRED" || again.Reason != "dead lab machine" || !again.RetiredAt.Equal(*got.RetiredAt) || again.GatewayLogin != GatewayLoginRemoved {
		t.Errorf("second retire: %+v", again)
	}
	if synced != 1 {
		t.Errorf("a repeated retire signalled the sync again (%d)", synced)
	}
	l, err := e.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "device.retire"}, store.Page{Limit: 10})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("device.retire audit rows = %d, %v; want exactly one", len(l.Items), err)
	}
}

// enrollWithNonce repeats an enrollment and returns the status.
func (a *agentSide) enrollWithNonce(token, nonce, host string) int {
	a.t.Helper()
	req := api.EnrollRequest{Nonce: nonce, Hostname: host, OS: "windows", OSVersion: "Windows 11 Pro", Arch: api.ArchAMD64, DeviceClass: api.DeviceClassDesktop,
		HWFingerprint: strings.Repeat("ab", 16), AgentVersion: "1.4.2", ResticVersion: "0.19.1"}
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

// Role gate (§5.2 "AD"): below admin 403, scoped or not (the schema has no scoped
// admin, so the store's scope matrix, not this route, covers another customer's
// device), and nothing changes.
func TestRetireNeedsTheAdminRole(t *testing.T) {
	e := newEnv(t)
	other := e.customerNamed("Northwind Dental")
	e.addAdmin("u-ro", "read_only", nil)
	e.addAdmin("u-op", "operator", nil)
	e.addAdmin("u-scoped", "operator", []string{other.ID})
	dev := e.deviceNamed("FRONTDESK-02")
	body := DeviceRetireRequest{Reason: "r"}
	for _, u := range []string{"u-ro", "u-op", "u-scoped"} {
		res, b := e.req("POST", "/api/v1/devices/"+dev.ID+"/retire", e.login(u, pw), body)
		if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
			t.Errorf("%s: %d %s, want 403", u, res.StatusCode, b)
		}
	}
	if d := e.deviceNamed("FRONTDESK-02"); d.RetiredAt != nil {
		t.Fatal("a refused call retired the device")
	}
	res, b := e.req("POST", "/api/v1/devices/"+dev.ID+"/retire", e.login("admin", pw), DeviceRetireRequest{Reason: " "})
	if res.StatusCode != 400 {
		t.Errorf("an empty reason: %d %s, want 400", res.StatusCode, b)
	}
	res, b = e.req("POST", "/api/v1/devices/"+dev.ID+"/retire", e.login("admin", pw), DeviceRetireRequest{Reason: `moved to C:\Users\frank`})
	if res.StatusCode != 400 || bytes.Contains(b, []byte("frank")) {
		t.Errorf("a path in the reason: %d %s, want 400 that does not repeat it", res.StatusCode, b)
	}
}

// final_backup retires nothing: it queues the backup, because a backup needs the
// credentials a retire takes away.
func TestRetireFinalBackupQueuesABackupAndDoesNotRetire(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	dev := e.deviceNamed("FRONTDESK-02")
	ctx := context.Background()
	res, b := e.req("POST", "/api/v1/devices/"+dev.ID+"/retire", owner, DeviceRetireRequest{Reason: "replaced", FinalBackup: true})
	if res.StatusCode != 202 {
		t.Fatalf("final backup: %d %s", res.StatusCode, b)
	}
	out := decode[DeviceRetireResponse](t, b)
	if out.Retired || out.State != "" || out.FinalBackupCommandID == "" || out.Changed {
		t.Fatalf("answer: %+v", out)
	}
	if d := e.deviceNamed("FRONTDESK-02"); d.RetiredAt != nil {
		t.Fatal("final_backup retired the device")
	}
	cmds, err := e.st.ListCommands(ctx, store.ScopeAll(), dev.ID, store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cmds.Items {
		found = found || (c.ID == out.FinalBackupCommandID && c.Type == store.CommandBackupNow && c.Status == "queued")
	}
	if !found {
		t.Fatalf("no queued backup_now %s: %+v", out.FinalBackupCommandID, cmds.Items)
	}
	// A second ask joins the backup in flight.
	_, b = e.req("POST", "/api/v1/devices/"+dev.ID+"/retire", owner, DeviceRetireRequest{Reason: "replaced", FinalBackup: true})
	if again := decode[DeviceRetireResponse](t, b); again.FinalBackupCommandID != out.FinalBackupCommandID {
		t.Errorf("a second final backup queued another command: %+v", again)
	}
	// Then the retire proper cancels what the machine has not collected.
	res, b = e.req("POST", "/api/v1/devices/"+dev.ID+"/retire", owner, DeviceRetireRequest{Reason: "replaced"})
	if got := decode[DeviceRetireResponse](t, b); res.StatusCode != 200 || got.CommandsCancelled != 1 {
		t.Fatalf("retire: %d %s", res.StatusCode, b)
	}
	// On a retired device final_backup answers the state, and queues nothing.
	res, b = e.req("POST", "/api/v1/devices/"+dev.ID+"/retire", owner, DeviceRetireRequest{Reason: "x", FinalBackup: true})
	if got := decode[DeviceRetireResponse](t, b); res.StatusCode != 200 || !got.Retired || got.Changed || got.FinalBackupCommandID != "" {
		t.Fatalf("final backup on a retired device: %d %s", res.StatusCode, b)
	}
	// A parked device takes no backup: 409, with the reason.
	parked := e.deviceNamed("LEGAL-PC-01")
	if _, err := e.st.PauseDeviceAudited(ctx, store.ScopeAll(), parked.ID, store.PauseRequest{Reason: "moving"}, store.AuditActor{Username: "t", Method: "api"}); err != nil {
		t.Fatal(err)
	}
	res, b = e.req("POST", "/api/v1/devices/"+parked.ID+"/retire", owner, DeviceRetireRequest{Reason: "x", FinalBackup: true})
	if res.StatusCode != 409 || !bytes.Contains(b, []byte("device_parked")) {
		t.Errorf("final backup on a parked device: %d %s", res.StatusCode, b)
	}
}

// §4.2 row 1 and §4.3, on the API's own answers: a retired device leaves every
// alarm and every denominator (B14, B15; D10 for the health percentage).
func TestRetiredDeviceRaisesNoAlarmAndLeavesEveryDenominator_B14_B15_D10(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	summary := func() FleetSummaryResponse {
		t.Helper()
		return decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", owner))
	}
	before := summary()
	// A device that is alarming now: the board's first critical device.
	var target string
	for _, g := range decode[AlarmsResponse](t, mustGet(e, "/api/v1/alarms", owner)).Groups {
		for _, a := range g.Alarms {
			if a.DeviceID != "" && a.Severity == "critical" && target == "" {
				target = a.DeviceID
			}
		}
	}
	if target == "" {
		t.Fatal("fixture: no critical device to retire")
	}
	if res, b := e.req("POST", "/api/v1/devices/"+target+"/retire", owner, DeviceRetireRequest{Reason: "dead lab machine"}); res.StatusCode != 200 {
		t.Fatalf("retire: %d %s", res.StatusCode, b)
	}
	after := summary()
	if after.Devices.Total != before.Devices.Total-1 {
		t.Errorf("B15: device total %d -> %d, want one fewer", before.Devices.Total, after.Devices.Total)
	}
	if after.Devices.Critical != before.Devices.Critical-1 {
		t.Errorf("B14: critical %d -> %d, want one fewer", before.Devices.Critical, after.Devices.Critical)
	}
	if after.States["RETIRED"] != before.States["RETIRED"] {
		t.Errorf("D10: the states map counts retired devices: %v", after.States)
	}
	for _, path := range []string{"/api/v1/alarms", "/api/v1/fleet/overdue", "/api/v1/fleet/unseen"} {
		if b := mustGet(e, path, owner); bytes.Contains(b, []byte(target)) {
			t.Errorf("B14: %s still lists the retired device: %s", path, b)
		}
	}
	// Its own verdict says so.
	st := decode[DeviceStateResponse](t, mustGet(e, "/api/v1/devices/"+target+"/state", owner))
	if st.State != "RETIRED" || st.Severity != "none" {
		t.Errorf("B14: state %s severity %s", st.State, st.Severity)
	}
	// Pause on a retired device stays refused (M3-T3).
	if res, b := e.req("POST", "/api/v1/devices/"+target+"/pause", owner, map[string]any{"reason": "x", "indefinite": true}); res.StatusCode != 409 {
		t.Errorf("pausing a retired device: %d %s", res.StatusCode, b)
	}
}

// Silence is an alarm: retiring an expected customer's last device reads MISSING,
// critical, on the API and in the alarm list (D02, §4.5, architecture §9 "zero
// active devices is judged on non-retired devices").
func TestRetiringTheLastDeviceOfAnExpectedCustomerReadsMissing_D02(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	ctx := context.Background()
	summit := e.customerNamed("Summit Eye Care")
	expected := true
	if _, _, err := e.st.UpdateCustomerAudited(ctx, store.ScopeAll(), summit.ID, store.CustomerPatch{Expected: &expected}, store.AuditActor{Username: "t", Method: "api"}); err != nil {
		t.Fatal(err)
	}
	devs, err := e.st.ListDevices(ctx, store.ScopeOf(summit.ID), store.ListDevicesOptions{Page: store.Page{Limit: 100}})
	if err != nil || len(devs.Items) < 1 {
		t.Fatalf("devices of the customer: %d, %v", len(devs.Items), err)
	}
	health := func() CustomerHealthResponse {
		t.Helper()
		return decode[CustomerHealthResponse](t, mustGet(e, "/api/v1/customers/"+summit.ID+"/health", owner))
	}
	retiredBefore := health().Retired
	for i, d := range devs.Items {
		if h := health(); h.Missing {
			t.Fatalf("MISSING with %d devices left", len(devs.Items)-i)
		}
		if res, b := e.req("POST", "/api/v1/devices/"+d.ID+"/retire", owner, DeviceRetireRequest{Reason: "site closed"}); res.StatusCode != 200 {
			t.Fatalf("retire: %d %s", res.StatusCode, b)
		}
	}
	h := health()
	if !h.Missing || h.Retired != retiredBefore+len(devs.Items) {
		t.Fatalf("after retiring every device: missing %v, retired %d", h.Missing, h.Retired)
	}
	if b := mustGet(e, "/api/v1/alarms", owner); !bytes.Contains(b, []byte(summit.ID)) {
		t.Errorf("the MISSING customer raises no alarm: %s", b)
	}
	sum := decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", owner))
	if sum.CustomerRing.Missing < 1 {
		t.Errorf("the customer ring does not count it as missing: %+v", sum.CustomerRing)
	}
}

// ---- token delete ----

func TestDeleteTokenThroughTheAPI(t *testing.T) {
	e, _ := enrollEnv(t)
	owner := e.login("admin", pw)
	cust := e.customerNamed("Summit Eye Care")
	agent := newAgentSide(t, e)
	tok, code, b := createToken(t, e, owner, map[string]any{"customer_id": cust.ID, "label": "old rollout", "expires_in": "1h"})
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	enrolled, _ := agent.enrollFull(tok.Token, strings.Repeat("d1", 16), "VIA-TOKEN-01")
	del := "/api/v1/enrollment-tokens/" + tok.ID + "/delete"

	// Active: refused, with the reason, and nothing changes.
	res, body := e.req("POST", del, owner, nil)
	if res.StatusCode != 409 || !bytes.Contains(body, []byte("token_live")) {
		t.Fatalf("deleting an active token: %d %s", res.StatusCode, body)
	}
	// Roles.
	e.addAdmin("u-op", "operator", nil)
	if res, b := e.req("POST", del, e.login("u-op", pw), nil); res.StatusCode != 403 {
		t.Errorf("as an operator: %d %s", res.StatusCode, b)
	}

	e.mustReq("DELETE", "/api/v1/enrollment-tokens/"+tok.ID, owner, nil, 204)
	e.mustReq("POST", del, owner, nil, 204)
	e.mustReq("POST", del, owner, nil, 204) // idempotent

	list := decode[EnrollTokenListResponse](t, mustGet(e, "/api/v1/enrollment-tokens?customer="+cust.ID, owner))
	for _, x := range list.Tokens {
		if x.ID == tok.ID {
			t.Error("the deleted token is still listed")
		}
	}
	// A direct read still finds it: the device's enrolled-via link has somewhere to point.
	got := decode[EnrollTokenResponse](t, mustGet(e, "/api/v1/enrollment-tokens/"+tok.ID, owner))
	if got.DeletedAt == nil || got.Devices != 1 {
		t.Errorf("direct read: %+v", got.EnrollTokenJSON)
	}
	d, err := e.st.GetDevice(context.Background(), store.ScopeAll(), enrolled.DeviceID)
	if err != nil || d.EnrollmentTokenID != tok.ID {
		t.Errorf("the device's enrolled-via link: %q, %v", d.EnrollmentTokenID, err)
	}
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "enrollment_token.delete"}, store.Page{Limit: 10})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("delete audit rows = %d, %v", len(l.Items), err)
	}
	// An expired token needs no revoke first.
	exp, code, b := createToken(t, e, owner, map[string]any{"customer_id": cust.ID, "expires_in": "1h"})
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	e.clk.Advance(2 * time.Hour)
	e.mustReq("POST", "/api/v1/enrollment-tokens/"+exp.ID+"/delete", owner, nil, 204)
}

// M1: anything but a passing run after the retire reads pending, with its reason, within the bound.
func TestRetireAnswersPendingUnlessASyncPassedAfterIt(t *testing.T) {
	retireOf := func(t *testing.T, e *env, host string) DeviceRetireResponse {
		t.Helper()
		res, b := e.req("POST", "/api/v1/devices/"+e.deviceNamed(host).ID+"/retire", e.login("admin", pw), DeviceRetireRequest{Reason: "r"})
		if res.StatusCode != 200 {
			t.Fatalf("retire: %d %s", res.StatusCode, b)
		}
		return decode[DeviceRetireResponse](t, b)
	}
	record := func(t *testing.T, e *env, status, class string) {
		t.Helper()
		if err := e.st.RecordJobRun(context.Background(), store.JobHtpasswdSync, store.JobRun{Status: status, ErrorClass: class, StartedAt: e.clk.Now(), FinishedAt: e.clk.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("no sync wired", func(t *testing.T) {
		e := newEnv(t)
		if got := retireOf(t, e, "FRONTDESK-02"); got.GatewayLogin != GatewayLoginPending || got.GatewayLoginReason != gatewaySyncNotWired {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("the job never ran", func(t *testing.T) {
		e := newEnv(t)
		e.svc.SyncGateway, e.svc.GatewaySyncWait = func() {}, 60*time.Millisecond
		if got := retireOf(t, e, "FRONTDESK-02"); got.GatewayLogin != GatewayLoginPending || got.GatewayLoginReason != state.LoginNotChecked {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a pass from before the retire proves nothing", func(t *testing.T) {
		e := newEnv(t)
		record(t, e, "ok", "")
		e.clk.Advance(time.Minute)
		e.svc.SyncGateway, e.svc.GatewaySyncWait = func() {}, 60*time.Millisecond
		if got := retireOf(t, e, "FRONTDESK-02"); got.GatewayLogin != GatewayLoginPending || got.GatewayLoginReason != state.LoginNoSyncSinceRetire {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a failed run answers at once with its class", func(t *testing.T) {
		e := newEnv(t)
		e.svc.GatewaySyncWait = time.Minute // the test fails on time if it waits this out
		e.svc.SyncGateway = func() { record(t, e, "error", "io") }
		began := time.Now()
		if got := retireOf(t, e, "FRONTDESK-02"); got.GatewayLogin != GatewayLoginPending || got.GatewayLoginReason != "io" {
			t.Fatalf("%+v", got)
		}
		if took := time.Since(began); took > 30*time.Second {
			t.Errorf("it waited %v for a run that had already failed", took)
		}
	})
	t.Run("a passing run answers removed", func(t *testing.T) {
		e := newEnv(t)
		e.svc.SyncGateway = func() { record(t, e, "ok", "") }
		if got := retireOf(t, e, "FRONTDESK-02"); got.GatewayLogin != GatewayLoginRemoved || got.GatewayLoginReason != "" {
			t.Fatalf("%+v", got)
		}
	})
}

// L1: final_backup keeps the reason and holds replaced_by to the retire's rule.
func TestRetireFinalBackupValidatesTheReplacementAndKeepsTheReason(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	dev := e.deviceNamed("FRONTDESK-02")
	path := "/api/v1/devices/" + dev.ID + "/retire"
	for name, repl := range map[string]string{"bogus": "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", "another customer": e.deviceNamed("LEGAL-PC-01").ID, "itself": dev.ID} {
		res, b := e.req("POST", path, owner, DeviceRetireRequest{Reason: "r", FinalBackup: true, ReplacedBy: repl})
		if res.StatusCode != 400 || !bytes.Contains(b, []byte("replaced_by")) {
			t.Errorf("%s: %d %s, want 400 on replaced_by", name, res.StatusCode, b)
		}
	}
	ctx := context.Background()
	if cmds, err := e.st.ListCommands(ctx, store.ScopeAll(), dev.ID, store.Page{Limit: 50}); err != nil {
		t.Fatal(err)
	} else {
		for _, c := range cmds.Items {
			if c.Type == store.CommandBackupNow && c.Status == "queued" {
				t.Fatal("a refused final backup queued a command")
			}
		}
	}
	good := e.deviceNamed("DR-OFFICE-PC").ID
	res, b := e.req("POST", path, owner, DeviceRetireRequest{Reason: "replaced next week", FinalBackup: true, ReplacedBy: good})
	if res.StatusCode != 202 {
		t.Fatalf("final backup: %d %s", res.StatusCode, b)
	}
	if out := decode[DeviceRetireResponse](t, b); out.Reason != "replaced next week" || out.ReplacedBy != good {
		t.Errorf("answer: %+v", out)
	}
	l, err := e.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "command.queue"}, store.Page{Limit: 5})
	if err != nil || len(l.Items) == 0 || !strings.Contains(string(l.Items[0].Details), "replaced next week") {
		t.Errorf("the command's audit row does not keep the reason: %v %+v", err, l.Items)
	}
}

// L7: a repeat with an empty reason answers the state, not 400.
func TestRetireRepeatWithAnEmptyReasonAnswersTheState(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	path := "/api/v1/devices/" + e.deviceNamed("FRONTDESK-02").ID + "/retire"
	if res, b := e.req("POST", path, owner, DeviceRetireRequest{Reason: " "}); res.StatusCode != 400 {
		t.Fatalf("a first retire with no reason: %d %s", res.StatusCode, b)
	}
	e.mustReq("POST", path, owner, DeviceRetireRequest{Reason: "first"}, 200)
	for _, final := range []bool{false, true} {
		res, b := e.req("POST", path, owner, DeviceRetireRequest{FinalBackup: final})
		if res.StatusCode != 200 {
			t.Fatalf("repeat (final backup %v): %d %s", final, res.StatusCode, b)
		}
		if got := decode[DeviceRetireResponse](t, b); got.Changed || !got.Retired || got.Reason != "first" {
			t.Errorf("repeat: %+v", got)
		}
	}
}
