package adminapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T3: the admin read API of the state engine, pause and resume, saved views,
// /status, /metrics, the job table and the event stream.

// m3Reads are the read routes that need only a signed-in account and a device
// or customer from the fixtures.
func (e *env) m3Reads() map[string]string {
	summit := e.customerNamed("Summit Eye Care")
	blue := e.customerNamed("Blue Ridge Accounting")
	return map[string]string{
		"fleet summary":     "/api/v1/fleet/summary",
		"fleet summary (c)": "/api/v1/fleet/summary?customer=" + summit.ID,
		"fleet banners":     "/api/v1/fleet/banners",
		"fleet overdue":     "/api/v1/fleet/overdue",
		"fleet unseen":      "/api/v1/fleet/unseen",
		"fleet ten-day":     "/api/v1/fleet/ten-day",
		"fleet ten-day (c)": "/api/v1/fleet/ten-day?customer=" + summit.ID,
		"alarms":            "/api/v1/alarms",
		"customer health":   "/api/v1/customers/" + summit.ID + "/health",
		"missing customer":  "/api/v1/customers/" + blue.ID + "/health",
		"device state":      "/api/v1/devices/" + e.dev.ID + "/state",
		"device heartbeats": "/api/v1/devices/" + e.dev.ID + "/heartbeats",
		"device ten-day":    "/api/v1/devices/" + e.dev.ID + "/ten-day",
		"runs":              "/api/v1/runs",
		"views":             "/api/v1/views",
		"status":            "/api/v1/status",
		"system health":     "/api/v1/system/health",
		"system jobs":       "/api/v1/system/jobs",
	}
}

func TestM3EveryReadRouteAnswersInFixturesMode(t *testing.T) { // route ids F05 F06 F13 F14 F15 F18 F19 F20
	e := newEnv(t)
	owner := e.login("admin", pw)
	for name, path := range e.m3Reads() {
		code, body := e.get(path, owner)
		if code != 200 {
			t.Errorf("%s: %d %s", name, code, body)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Errorf("%s: not JSON: %v", name, err)
			continue
		}
		if _, ok := m["computed_at"]; !ok {
			t.Errorf("%s: no computed_at (the freshness stamp, architecture 4.6)", name)
		}
	}
	// The run detail needs a run id.
	l := e.getJSON("/api/v1/runs?limit=5", owner)
	runs := l["runs"].([]any)
	if len(runs) == 0 {
		t.Fatal("the fixtures have no runs")
	}
	if code, body := e.get("/api/v1/runs/"+runs[0].(map[string]any)["id"].(string), owner); code != 200 {
		t.Fatalf("run detail: %d %s", code, body)
	}
	// Prometheus text.
	res, body := e.req("GET", "/metrics", owner, nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != MetricsContentType || !strings.Contains(string(body), `cryobackup_devices{state="OK"}`) {
		t.Fatalf("metrics: %d %s %.200s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	// Unknown ids are 404, never an empty 200.
	for _, p := range []string{"/api/v1/devices/nope/state", "/api/v1/devices/nope/heartbeats", "/api/v1/devices/nope/ten-day", "/api/v1/runs/nope", "/api/v1/customers/nope/health"} {
		if code, _ := e.get(p, owner); code != 404 {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
	// A customer filter outside the data is 404 too, not an empty fleet.
	if code, _ := e.get("/api/v1/fleet/summary?customer=nope", owner); code != 404 {
		t.Errorf("summary of an unknown customer = %d", code)
	}
}

func (e *env) getJSON(path string, s *session) map[string]any {
	e.t.Helper()
	code, body := e.get(path, s)
	if code != 200 {
		e.t.Fatalf("GET %s: %d %s", path, code, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func TestM3TheAPIAndTheDeviceListCannotDisagree(t *testing.T) { // one FleetView behind every route
	e := newEnv(t)
	owner := e.login("admin", pw)
	_, lb := e.get("/api/v1/devices?include_retired=true&limit=500", owner)
	list := decode[DeviceListResponse](t, lb)
	st := decode[StatusResponse](t, mustGet(e, "/api/v1/status", owner))
	if len(st.Devices) != len(list.Devices) {
		t.Fatalf("/status lists %d devices, /devices %d", len(st.Devices), len(list.Devices))
	}
	byID := map[string]DeviceJSON{}
	for _, d := range list.Devices {
		byID[d.ID] = d
	}
	counts := map[string]int{}
	for _, d := range st.Devices {
		want := byID[d.ID]
		if d.State != want.State || d.Severity != want.Severity {
			t.Errorf("%s: /status says %s/%s, /devices %s/%s", d.Hostname, d.State, d.Severity, want.State, want.Severity)
		}
		if d.State != string(state.StateRetired) {
			counts[d.State]++
		}
	}
	sum := decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", owner))
	for stt, n := range counts {
		if sum.States[stt] != n {
			t.Errorf("summary counts %d %s devices, /status %d", sum.States[stt], stt, n)
		}
	}
	// The explain route agrees device by device (a sample of every state in the fixtures).
	seen := map[string]bool{}
	for _, d := range list.Devices {
		if seen[d.State] {
			continue
		}
		seen[d.State] = true
		ds := decode[DeviceStateResponse](t, mustGet(e, "/api/v1/devices/"+d.ID+"/state", owner))
		if ds.State != d.State || ds.Severity != d.Severity || len(ds.Explain) == 0 || ds.Explain[0].State != d.State {
			t.Errorf("%s: explain says %+v, list %s/%s", d.Hostname, ds.State, d.State, d.Severity)
		}
		for _, x := range ds.Explain {
			if x.Text == "" || strings.Contains(x.Text, `\`) {
				t.Errorf("%s: explain line %+v is empty or looks like a path", d.Hostname, x)
			}
		}
	}
	if len(seen) < 12 {
		t.Fatalf("only %d states in the fixtures to compare", len(seen))
	}
}

func mustGet(e *env, path string, s *session) []byte {
	e.t.Helper()
	code, b := e.get(path, s)
	if code != 200 {
		e.t.Fatalf("GET %s: %d %s", path, code, b)
	}
	return b
}

func TestM3SummaryOverdueUnseenAndAlarmsReadTheEngine(t *testing.T) { // F06 F14
	e := newEnv(t)
	owner := e.login("admin", pw)
	sum := decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", owner))
	if !sum.Complete || sum.AllClear || sum.Devices.Total == 0 || sum.Devices.Critical == 0 || sum.Devices.WithinSLAPct == nil {
		t.Fatalf("summary of the fixtures: %+v", sum)
	}
	if sum.CustomerRing.Missing != 1 || sum.CustomerRing.ByStatus["missing"] != 1 {
		t.Errorf("the fixtures have one MISSING customer: %+v", sum.CustomerRing)
	}
	if sum.Freshness.Enrolled != sum.Devices.Total || sum.Freshness.InView > sum.Freshness.Enrolled {
		t.Errorf("freshness %+v vs devices %+v", sum.Freshness, sum.Devices)
	}
	if got := sum.AgeRing.Under24h + sum.AgeRing.Within + sum.AgeRing.Beyond + sum.AgeRing.Parked; got != sum.Devices.Total {
		t.Errorf("the age ring covers %d of %d devices", got, sum.Devices.Total)
	}

	od := decode[FleetOverdueResponse](t, mustGet(e, "/api/v1/fleet/overdue", owner))
	if od.Total != sum.Overdue || od.Total == 0 {
		t.Fatalf("overdue list %d, summary %d", od.Total, sum.Overdue)
	}
	for _, d := range od.Devices {
		if len(d.Chips) == 0 {
			t.Errorf("%s is on the overdue list with no reason chip", d.Hostname)
		}
		for _, c := range d.Chips {
			if c != "STALE" && c != "MISSED" && c != "BLANK" {
				t.Errorf("%s: chip %s", d.Hostname, c)
			}
		}
	}
	if od.Devices[0].LastSuccessAgeS != nil {
		// never-succeeded devices (BLANK) come first
		for _, d := range od.Devices {
			if d.LastSuccessAgeS == nil {
				t.Errorf("%s (no success ever) is listed after devices that did succeed", d.Hostname)
			}
		}
	}

	un := decode[FleetUnseenResponse](t, mustGet(e, "/api/v1/fleet/unseen", owner))
	if un.Devices != sum.Unseen || un.Devices == 0 {
		t.Fatalf("unseen: %d devices, summary %d", un.Devices, sum.Unseen)
	}

	al := decode[AlarmsResponse](t, mustGet(e, "/api/v1/alarms", owner))
	if al.Total == 0 || len(al.Groups) == 0 {
		t.Fatalf("alarms: %+v", al)
	}
	foundMissing := false
	for _, g := range al.Groups {
		for _, a := range g.Alarms {
			if a.Kind == "customer" && g.Status == "missing" {
				foundMissing = true
			}
			if a.Severity == "none" || len(a.Categories) == 0 {
				t.Errorf("group %s lists a non-alarm %+v", g.Customer, a)
			}
		}
	}
	if !foundMissing {
		t.Error("the MISSING customer is not in the alarms (an expected customer with no devices is critical)")
	}
	crit := decode[AlarmsResponse](t, mustGet(e, "/api/v1/alarms?severity=critical", owner))
	if crit.Total == 0 || crit.Total >= al.Total {
		t.Errorf("critical-only alarms: %d of %d", crit.Total, al.Total)
	}
	if code, _ := e.get("/api/v1/alarms?severity=loud", owner); code != 400 {
		t.Errorf("unknown severity = %d", code)
	}

	bn := decode[FleetBannersResponse](t, mustGet(e, "/api/v1/fleet/banners", owner))
	if bn.AllClear || len(bn.Banners) == 0 {
		t.Errorf("a console with no master key loaded in the test env and a MISSING customer is not all clear: %+v", bn)
	}
	miss := false
	for _, eb := range bn.EntityBanner {
		miss = miss || eb.Kind == "missing"
	}
	if !miss {
		t.Error("no entity banner for the MISSING customer")
	}

	blue := e.customerNamed("Blue Ridge Accounting")
	ch := decode[CustomerHealthResponse](t, mustGet(e, "/api/v1/customers/"+blue.ID+"/health", owner))
	if !ch.Missing || ch.Rollup == nil || ch.Rollup.Status != "missing" || ch.Rollup.HealthPct != nil {
		t.Errorf("the MISSING customer: %+v", ch)
	}
	summit := e.customerNamed("Summit Eye Care")
	cs := decode[CustomerHealthResponse](t, mustGet(e, "/api/v1/customers/"+summit.ID+"/health", owner))
	if cs.Missing || len(cs.TenDay) != state.StripDays || cs.Rollup == nil || cs.Rollup.Counts["STALE"] == 0 {
		t.Errorf("Summit health: %+v", cs)
	}
}

func TestM3TenDayStripsFollowTheEngine(t *testing.T) { // F05 F13
	e := newEnv(t)
	owner := e.login("admin", pw)
	d := decode[DeviceTenDayResponse](t, mustGet(e, "/api/v1/devices/"+e.dev.ID+"/ten-day", owner))
	if len(d.Days) != state.StripDays || d.State != "OK" || d.Zone == "" {
		t.Fatalf("device strip: %+v", d)
	}
	for _, c := range d.Days {
		switch c.Kind {
		case "success", "mixed", "missed", "failed", "none":
		default:
			t.Errorf("cell kind %q", c.Kind)
		}
	}
	summit := e.customerNamed("Summit Eye Care")
	g := decode[FleetTenDayResponse](t, mustGet(e, "/api/v1/fleet/ten-day?customer="+summit.ID, owner))
	if len(g.Customers) != 1 || len(g.Customers[0].Devices) == 0 || len(g.Customers[0].Days) != state.StripDays {
		t.Fatalf("customer grid: %+v", g)
	}
	all := decode[FleetTenDayResponse](t, mustGet(e, "/api/v1/fleet/ten-day", owner))
	if len(all.Customers) < 6 {
		t.Fatalf("fleet grid has %d customers", len(all.Customers))
	}
	for _, c := range all.Customers {
		if len(c.Devices) != 0 {
			t.Errorf("%s: the fleet grid lists device rows without being asked", c.Name)
		}
		if c.Status == "missing" && len(c.Days) != 0 {
			t.Errorf("a MISSING customer has no devices and so no strip: %+v", c.Days)
		}
	}
	withDev := decode[FleetTenDayResponse](t, mustGet(e, "/api/v1/fleet/ten-day?devices=true", owner))
	n := 0
	for _, c := range withDev.Customers {
		n += len(c.Devices)
	}
	if n == 0 {
		t.Error("devices=true lists no device rows")
	}
}

func TestM3HeartbeatRing(t *testing.T) { // F13 F14 F18
	e := newEnv(t)
	owner := e.login("admin", pw)
	for i := 0; i < 3; i++ {
		e.clk.Advance(time.Minute)
		if err := e.st.AgentUpsertPresence(context.Background(), e.dev.ID, store.Presence{ReceivedAt: e.clk.Now(), ServiceState: "idle", AgentVersion: "1.4.2"}); err != nil {
			t.Fatal(err)
		}
	}
	hb := decode[DeviceHeartbeatsResponse](t, mustGet(e, "/api/v1/devices/"+e.dev.ID+"/heartbeats?range=1h", owner))
	if hb.Count != len(hb.Heartbeats) || hb.Count < 3 || hb.Truncated {
		t.Fatalf("ring: %+v", hb)
	}
	for i := 1; i < len(hb.Heartbeats); i++ {
		if hb.Heartbeats[i].ReceivedAt.After(hb.Heartbeats[i-1].ReceivedAt) {
			t.Fatal("heartbeats are not newest first")
		}
	}
	hb = decode[DeviceHeartbeatsResponse](t, mustGet(e, "/api/v1/devices/"+e.dev.ID+"/heartbeats?limit=2", owner))
	if hb.Count != 2 || !hb.Truncated {
		t.Errorf("limit 2: %+v", hb)
	}
	for _, bad := range []string{"range=week", "limit=0", "limit=99999", "limit=x"} {
		if code, _ := e.get("/api/v1/devices/"+e.dev.ID+"/heartbeats?"+bad, owner); code != 400 {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
}

func TestM3RoleMatrix(t *testing.T) {
	e := newEnv(t)
	roles := map[string]*session{}
	for _, r := range []string{"read_only", "operator", "admin", "owner"} {
		e.addAdmin("m3-"+r, r, nil)
		roles[r] = e.login("m3-"+r, pw)
	}
	runner := jobs.NewRunner(e.st, "matrix", nil)
	runner.Add(jobs.Job{Name: "matrix-job", Every: time.Hour, Run: func(context.Context) error { return nil }})
	e.svc.Jobs = runner
	t.Cleanup(jobBackground.Wait)
	target := e.firstStorageTarget()
	n := 0
	order := []string{"read_only", "operator", "admin", "owner"}
	type call struct {
		name, method, path string
		body               func() any
		min                string
	}
	var calls []call
	for name, p := range e.m3Reads() {
		min := "read_only"
		if name == "system jobs" {
			min = "admin"
		}
		calls = append(calls, call{name, "GET", p, nil, min})
	}
	calls = append(calls,
		call{"metrics", "GET", "/metrics", nil, "read_only"},
		call{"pause", "POST", "/api/v1/devices/" + e.held.ID + "/pause", func() any { return DevicePauseRequest{Reason: "matrix", DurationHours: ptr(1)} }, "operator"},
		call{"resume", "POST", "/api/v1/devices/" + e.held.ID + "/resume", nil, "operator"},
		call{"create view", "POST", "/api/v1/views", func() any { n++; return CreateViewRequest{Name: "matrix " + strings.Repeat("x", n), Entity: "devices"} }, "read_only"},
		call{"run job", "POST", "/api/v1/system/jobs/matrix-job/run", nil, "admin"},
		call{"probe", "POST", "/api/v1/storage-targets/" + target + "/probe", nil, "admin"},
	)
	for _, c := range calls {
		for i, role := range order {
			var body any
			if c.body != nil {
				body = c.body()
			}
			res, b := e.req(c.method, c.path, roles[role], body)
			allowed := i >= indexOf(order, c.min)
			switch {
			case allowed && res.StatusCode >= 300:
				t.Errorf("%s as %s: want success, got %d %s", c.name, role, res.StatusCode, b)
			case !allowed && (res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden):
				t.Errorf("%s as %s: want 403 forbidden, got %d %s", c.name, role, res.StatusCode, b)
			}
			if c.name == "run job" && allowed {
				jobBackground.Wait() // the lease is free again before the next role runs it
			}
		}
	}
	// Nobody anonymous reads anything.
	for _, p := range []string{"/api/v1/fleet/summary", "/api/v1/status", "/metrics", "/api/v1/events", "/api/v1/system/health"} {
		if res, _ := e.req("GET", p, nil, nil); res.StatusCode != 401 {
			t.Errorf("anonymous GET %s = %d, want 401", p, res.StatusCode)
		}
	}
}

func (e *env) firstStorageTarget() string {
	e.t.Helper()
	ts, err := e.st.ListStorageTargets(context.Background())
	if err != nil || len(ts) == 0 {
		e.t.Fatalf("storage targets: %v", err)
	}
	return ts[0].ID
}

func TestM3ACustomerScopedAccountSeesOnlyItsCustomer(t *testing.T) { // §3.5
	e := newEnv(t)
	nw := e.customerNamed("Northwind Dental")
	summit := e.customerNamed("Summit Eye Care")
	e.addAdmin("m3-scoped", "operator", []string{nw.ID})
	sc := e.login("m3-scoped", pw)

	sum := decode[FleetSummaryResponse](t, mustGet(e, "/api/v1/fleet/summary", sc))
	if sum.Devices.Total != 3 || sum.CustomerRing.Total != 1 {
		t.Errorf("scoped summary: %+v / %+v", sum.Devices, sum.CustomerRing)
	}
	st := decode[StatusResponse](t, mustGet(e, "/api/v1/status", sc))
	for _, d := range st.Devices {
		if d.CustomerID != nw.ID {
			t.Errorf("/status shows %s of another customer", d.Hostname)
		}
	}
	for _, c := range st.Customers {
		if c.ID != nw.ID {
			t.Errorf("/status shows customer %s", c.Name)
		}
	}
	runs := decode[RunListResponse](t, mustGet(e, "/api/v1/runs?limit=500", sc))
	for _, r := range runs.Runs {
		if r.CustomerID != nw.ID {
			t.Errorf("/runs shows a run of %s", r.Customer)
		}
	}
	if runs.Of >= decode[RunListResponse](t, mustGet(e, "/api/v1/runs?limit=1", e.login("admin", pw))).Of {
		t.Error("the scoped total is not smaller than the whole")
	}
	other := e.openRun(e.deviceNamed("EXAM-ROOM-3").ID)
	for name, p := range map[string]string{
		"state":      "/api/v1/devices/" + e.dev.ID + "/state",
		"heartbeats": "/api/v1/devices/" + e.dev.ID + "/heartbeats",
		"ten-day":    "/api/v1/devices/" + e.dev.ID + "/ten-day",
		"health":     "/api/v1/customers/" + summit.ID + "/health",
		"run":        "/api/v1/runs/" + other.ID,
		"summary":    "/api/v1/fleet/summary?customer=" + summit.ID,
		"unseen":     "/api/v1/fleet/unseen?customer=" + summit.ID,
	} {
		if code, b := e.get(p, sc); code != 404 {
			t.Errorf("%s of another customer = %d %s, want 404", name, code, b)
		}
	}
	if res, _ := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/pause", sc, DevicePauseRequest{Reason: "x", Indefinite: true}); res.StatusCode != 404 {
		t.Errorf("pausing another customer's device = %d, want 404", res.StatusCode)
	}
	al := decode[AlarmsResponse](t, mustGet(e, "/api/v1/alarms", sc))
	for _, g := range al.Groups {
		if g.CustomerID != nw.ID {
			t.Errorf("alarms of %s shown to an account scoped to Northwind", g.Customer)
		}
	}
	// A scoped account cannot share a view, and cannot see a shared view naming another customer.
	if res, _ := e.req("POST", "/api/v1/views", sc, CreateViewRequest{Name: "mine", Entity: "devices", Shared: true}); res.StatusCode != 403 {
		t.Errorf("scoped share = %d, want 403", res.StatusCode)
	}
	owner := e.login("admin", pw)
	if res, b := e.req("POST", "/api/v1/views", owner, CreateViewRequest{Name: "summit only", Entity: "devices", Shared: true, Filter: json.RawMessage(`{"customer_id":"` + summit.ID + `"}`)}); res.StatusCode != 201 {
		t.Fatalf("shared view: %d %s", res.StatusCode, b)
	}
	if res, b := e.req("POST", "/api/v1/views", owner, CreateViewRequest{Name: "everyone", Entity: "devices", Shared: true}); res.StatusCode != 201 {
		t.Fatalf("shared view: %d %s", res.StatusCode, b)
	}
	vl := decode[ViewListResponse](t, mustGet(e, "/api/v1/views", sc))
	if len(vl.Views) != 1 || vl.Views[0].Name != "everyone" {
		t.Errorf("the scoped account sees %+v", vl.Views)
	}
}

func TestM3PauseAndResume(t *testing.T) { // F14, DH11
	e := newEnv(t)
	op := func() *session { e.addAdmin("m3-op", "operator", nil); return e.login("m3-op", pw) }()
	wakes, mu := e.wakes()
	dev := e.deviceNamed("EXAM-ROOM-3") // has an open (stuck) backup run
	open := e.openRun(dev.ID)

	known := map[string]bool{} // commands the fixtures already queued
	for _, c := range e.commandsOf(dev.ID) {
		known[c.ID] = true
	}
	before := e.audits()
	res, b := e.req("POST", "/api/v1/devices/"+dev.ID+"/pause", op, DevicePauseRequest{Reason: "replacing the disk", DurationHours: ptr(4)})
	if res.StatusCode != 200 {
		t.Fatalf("pause: %d %s", res.StatusCode, b)
	}
	pr := decode[DevicePauseResponse](t, b)
	if !pr.Paused || !pr.Changed || pr.Indefinite || pr.PausedUntil == nil || !pr.PausedUntil.Equal(e.clk.Now().Add(4*time.Hour)) || pr.Reason != "replacing the disk" {
		t.Fatalf("pause response: %+v", pr)
	}
	if pr.CancelledRunID != open.ID || pr.QueuedCommands != 2 {
		t.Fatalf("DH11: pausing must cancel the running run %s: %+v", open.ID, pr)
	}
	if got := e.audits() - before; got != 1 {
		t.Fatalf("pause wrote %d audit rows, want 1", got)
	}
	mu.Lock()
	if len(*wakes) != 1 || (*wakes)[0] != dev.ID {
		t.Errorf("the device's command poll was not woken: %v", *wakes)
	}
	mu.Unlock()
	types := map[string]int{}
	for _, c := range e.commandsOf(dev.ID) {
		if known[c.ID] {
			continue
		}
		types[c.Type]++
		if c.Type == "cancel" && !strings.Contains(string(c.Params), open.ID) {
			t.Errorf("cancel params %s", c.Params)
		}
	}
	if len(types) != 2 || types["cancel"] != 1 || types["refresh_policy"] != 1 {
		t.Errorf("commands queued by the pause: %v", types)
	}
	// The engine now says PAUSED, through the state route.
	ds := decode[DeviceStateResponse](t, mustGet(e, "/api/v1/devices/"+dev.ID+"/state", op))
	if ds.State != "PAUSED" || ds.Inputs.PauseReason != "replacing the disk" || ds.Inputs.PausedUntil == nil {
		t.Errorf("state after pause: %+v", ds)
	}
	// The same request again is a no-op.
	pr = decode[DevicePauseResponse](t, mustBody(e, "POST", "/api/v1/devices/"+dev.ID+"/pause", op, DevicePauseRequest{Reason: "replacing the disk", Until: pr.PausedUntil}))
	if pr.Changed || e.audits()-before != 1 {
		t.Errorf("a repeated pause changed something: %+v", pr)
	}
	// Resume ends it; a second resume is a no-op.
	rr := decode[DevicePauseResponse](t, mustBody(e, "POST", "/api/v1/devices/"+dev.ID+"/resume", op, nil))
	if rr.Paused || !rr.Changed {
		t.Errorf("resume: %+v", rr)
	}
	rr = decode[DevicePauseResponse](t, mustBody(e, "POST", "/api/v1/devices/"+dev.ID+"/resume", op, nil))
	if rr.Changed || e.audits()-before != 2 {
		t.Errorf("second resume: %+v, audit rows %d", rr, e.audits()-before)
	}
	// Indefinite and by date.
	pr = decode[DevicePauseResponse](t, mustBody(e, "POST", "/api/v1/devices/"+e.dev.ID+"/pause", op, DevicePauseRequest{Reason: "closed for renovation", Indefinite: true}))
	if !pr.Paused || !pr.Indefinite || pr.PausedUntil != nil {
		t.Errorf("indefinite: %+v", pr)
	}
	until := e.clk.Now().Add(72 * time.Hour)
	pr = decode[DevicePauseResponse](t, mustBody(e, "POST", "/api/v1/devices/"+e.dev.ID+"/pause", op, DevicePauseRequest{Reason: "closed until Monday", Until: &until}))
	if pr.Indefinite || pr.PausedUntil == nil || !pr.PausedUntil.Equal(until) {
		t.Errorf("by date: %+v", pr)
	}

	// Refusals: reason required, exactly one duration form, the listed hours only, a date in the future, no paths.
	soon, past := e.clk.Now().Add(10*time.Second), e.clk.Now().Add(-time.Hour)
	for name, body := range map[string]any{
		"no reason":       DevicePauseRequest{DurationHours: ptr(1)},
		"no duration":     DevicePauseRequest{Reason: "x"},
		"two forms":       DevicePauseRequest{Reason: "x", DurationHours: ptr(1), Indefinite: true},
		"3 hours":         DevicePauseRequest{Reason: "x", DurationHours: ptr(3)},
		"past":            DevicePauseRequest{Reason: "x", Until: &past},
		"under a minute":  DevicePauseRequest{Reason: "x", Until: &soon},
		"a path":          DevicePauseRequest{Reason: `C:\Users\someone\file.docx is locked`, DurationHours: ptr(1)},
		"unknown field":   map[string]any{"reason": "x", "hours": 1},
		"an empty object": map[string]any{},
	} {
		res, b := e.req("POST", "/api/v1/devices/"+e.held.ID+"/pause", op, body)
		if res.StatusCode != 400 {
			t.Errorf("%s: %d %s, want 400", name, res.StatusCode, b)
		}
	}
	if got := decode[DeviceStateResponse](t, mustGet(e, "/api/v1/devices/"+e.held.ID+"/state", op)); got.State == "PAUSED" {
		t.Error("a refused pause parked the device")
	}
	// A retired device cannot be paused.
	old := e.deviceNamed("OLD-RECEPTION-PC")
	if res, b := e.req("POST", "/api/v1/devices/"+old.ID+"/pause", op, DevicePauseRequest{Reason: "x", Indefinite: true}); res.StatusCode != 409 {
		t.Errorf("pausing a retired device = %d %s, want 409", res.StatusCode, b)
	}
}

func mustBody(e *env, method, path string, s *session, body any) []byte {
	e.t.Helper()
	res, b := e.req(method, path, s, body)
	if res.StatusCode != 200 {
		e.t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	return b
}

func TestM3SavedViewsThroughTheAPI(t *testing.T) { // F06
	e := newEnv(t)
	owner := e.login("admin", pw)
	e.addAdmin("m3-ro", "read_only", nil)
	e.addAdmin("m3-ro2", "read_only", nil)
	ro, ro2 := e.login("m3-ro", pw), e.login("m3-ro2", pw)

	res, b := e.req("POST", "/api/v1/views", ro, CreateViewRequest{Name: "Stale devices", Entity: "devices", Filter: json.RawMessage(`{"state":["STALE"]}`), Columns: json.RawMessage(`["hostname","state"]`), Sort: json.RawMessage(`[{"column":"hostname","dir":"asc"}]`)})
	if res.StatusCode != 201 || res.Header.Get("Location") == "" {
		t.Fatalf("create: %d %s", res.StatusCode, b)
	}
	mine := decode[ViewResponse](t, b)
	if !mine.Mine || mine.Shared || mine.Owner != "m3-ro" || string(mine.Filter) != `{"state":["STALE"]}` {
		t.Fatalf("created view: %+v", mine)
	}
	// Private to its owner.
	if vl := decode[ViewListResponse](t, mustGet(e, "/api/v1/views", ro2)); len(vl.Views) != 0 {
		t.Errorf("another account sees a private view: %+v", vl.Views)
	}
	if code, _ := e.get("/api/v1/views?entity=bogus", ro); code != 400 {
		t.Errorf("unknown entity = %d", code)
	}
	// A duplicate name is a conflict; bad shapes are 400 with the field named.
	if res, _ := e.req("POST", "/api/v1/views", ro, CreateViewRequest{Name: "Stale devices", Entity: "devices"}); res.StatusCode != 409 {
		t.Errorf("duplicate = %d", res.StatusCode)
	}
	for name, body := range map[string]CreateViewRequest{
		"no name":      {Entity: "devices"},
		"a long name":  {Name: strings.Repeat("n", 101), Entity: "devices"},
		"bad entity":   {Name: "x", Entity: "audit"},
		"filter array": {Name: "x", Entity: "devices", Filter: json.RawMessage(`[1]`)},
		"columns obj":  {Name: "x", Entity: "devices", Columns: json.RawMessage(`{}`)},
	} {
		res, b := e.req("POST", "/api/v1/views", ro, body)
		if res.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, res.StatusCode, b)
		}
	}
	// Sharing: an all-customer account may; then everyone sees it, read-only to non-owners.
	res, b = e.req("PATCH", "/api/v1/views/"+mine.ID, ro, PatchViewRequest{Shared: ptr(true)})
	if res.StatusCode != 200 || !decode[ViewResponse](t, b).Shared {
		t.Fatalf("share: %d %s", res.StatusCode, b)
	}
	if vl := decode[ViewListResponse](t, mustGet(e, "/api/v1/views?entity=devices", ro2)); len(vl.Views) != 1 || vl.Views[0].Mine {
		t.Errorf("the shared view as another account: %+v", vl.Views)
	}
	n := "Renamed by someone else"
	if res, _ := e.req("PATCH", "/api/v1/views/"+mine.ID, ro2, PatchViewRequest{Name: &n}); res.StatusCode != 403 {
		t.Errorf("a non-owner editing = %d, want 403", res.StatusCode)
	}
	if res, _ := e.req("DELETE", "/api/v1/views/"+mine.ID, ro2, nil); res.StatusCode != 403 {
		t.Errorf("a non-owner deleting = %d, want 403", res.StatusCode)
	}
	// An admin may do both; a no-op patch says so.
	res, b = e.req("PATCH", "/api/v1/views/"+mine.ID, owner, PatchViewRequest{Name: &n})
	if res.StatusCode != 200 || decode[ViewResponse](t, b).Name != n {
		t.Fatalf("admin edit: %d %s", res.StatusCode, b)
	}
	if got := decode[ViewResponse](t, mustBody(e, "PATCH", "/api/v1/views/"+mine.ID, owner, PatchViewRequest{Name: &n})); got.Changed {
		t.Error("a patch that changed nothing says it did")
	}
	if res, _ := e.req("DELETE", "/api/v1/views/"+mine.ID, owner, nil); res.StatusCode != 204 {
		t.Errorf("admin delete = %d", res.StatusCode)
	}
	for _, code := range []int{404} {
		if res, _ := e.req("DELETE", "/api/v1/views/"+mine.ID, owner, nil); res.StatusCode != code {
			t.Errorf("deleting twice = %d, want %d", res.StatusCode, code)
		}
	}
}

func TestM3RunSearchAndDetailArePathFree(t *testing.T) { // F06 F18
	e := newEnv(t)
	owner := e.login("admin", pw)
	ctx := context.Background()
	// A run whose stats carry a path-looking text value: the detail keeps numbers only.
	fin := e.clk.Now().Add(-time.Minute)
	in := store.RunInput{ID: "01J9Y4K3A0B1C2D3E4F5G6H7K1", Kind: "backup", Trigger: "manual", StartedAt: fin.Add(-5 * time.Minute), FinishedAt: &fin, Outcome: "failed",
		ErrorClass: "permission", ErrorCounts: map[string]int{"permission": 7, "vss": 2}, Stats: json.RawMessage(`{"total_files_processed":900,"failed_files":9,"note":"C:\\Users\\x\\secret.docx","nested":{"a":1}}`)}
	if res, err := e.st.AgentIngestRuns(ctx, e.dev.ID, []store.RunInput{in}, e.clk.Now()); err != nil || res[0].Status != store.IngestCreated {
		t.Fatalf("ingest: %v %+v", err, res)
	}
	d := decode[RunDetailResponse](t, mustGet(e, "/api/v1/runs/"+in.ID, owner))
	if d.Outcome != "failed" || d.DeviceID != e.dev.ID || d.Hostname != "DR-OFFICE-PC" || d.Customer != "Summit Eye Care" || d.DurationMS == nil || *d.DurationMS != 5*60*1000 {
		t.Fatalf("detail: %+v", d)
	}
	if len(d.ErrorClasses) != 2 || d.ErrorClasses[0].Class != "permission" || d.ErrorClasses[0].Count != 7 {
		t.Errorf("error classes: %+v", d.ErrorClasses)
	}
	if d.Stats["total_files_processed"] != 900 || len(d.Stats) != 2 {
		t.Errorf("stats %v: only the numeric counters may pass", d.Stats)
	}
	_, raw := e.get("/api/v1/runs/"+in.ID, owner)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), `\\`) {
		t.Errorf("the run detail carries a path: %s", raw)
	}

	// The clause builder.
	count := func(q string) int {
		t.Helper()
		return decode[RunListResponse](t, mustGet(e, "/api/v1/runs?"+q, owner)).Total
	}
	all := count("limit=1")
	if all < 10 {
		t.Fatalf("the fixtures have %d runs", all)
	}
	if n := count("status=failed&device=" + e.dev.ID); n != 1 {
		t.Errorf("failed runs of the device: %d", n)
	}
	if n := count("status=failed,running"); n < 2 {
		t.Errorf("failed or running: %d", n)
	}
	if n := count("kind=prune,forget,check") + count("kind=backup"); n < 1 || n > all {
		t.Errorf("kinds: %d", n)
	}
	if n := count("customer=" + e.customerNamed("Northwind Dental").ID); n == 0 || n >= all {
		t.Errorf("customer clause: %d of %d", n, all)
	}
	since := fin.Add(-6 * time.Minute).Format(time.RFC3339) // the run started 5 minutes before fin
	if n := count("since=" + since); n < 1 || n >= all {
		t.Errorf("date clause: %d of %d", n, all)
	}
	l := decode[RunListResponse](t, mustGet(e, "/api/v1/runs?limit=3", owner))
	if len(l.Runs) != 3 || !l.Truncated || l.NextCursor != "o3" || l.Of != all {
		t.Errorf("paging: %+v", l)
	}
	l2 := decode[RunListResponse](t, mustGet(e, "/api/v1/runs?limit=3&cursor=o3", owner))
	if l2.Runs[0].ID == l.Runs[0].ID || l2.Runs[0].StartedAt.After(l.Runs[2].StartedAt) {
		t.Errorf("page two overlaps page one or is out of order")
	}
	for _, bad := range []string{"status=exploded", "kind=nap", "since=yesterday", "limit=0", "cursor=7", "until=2020-01-01T00:00:00Z&since=2021-01-01T00:00:00Z"} {
		if code, _ := e.get("/api/v1/runs?"+bad, owner); code != 400 {
			t.Errorf("runs?%s = %d, want 400", bad, code)
		}
	}
}

// ---- jobs, system health, probe ----

func TestM3JobTableAndRunNow(t *testing.T) { // F19
	e := newEnv(t)
	owner := e.login("admin", pw)
	t.Cleanup(jobBackground.Wait)
	ran := make(chan struct{}, 4)
	runner := jobs.NewRunner(e.st, "test-holder", nil)
	runner.Add(jobs.Job{Name: "test-job", Every: time.Hour, Run: func(context.Context) error { ran <- struct{}{}; return nil }})

	// Not wired: the table lists job_state rows only, and run-now is 503, loudly.
	if res, b := e.req("POST", "/api/v1/system/jobs/test-job/run", owner, nil); res.StatusCode != 503 || problemCode(t, res, b) != api.CodeUnavailable {
		t.Fatalf("run-now without a runner = %d %s", res.StatusCode, b)
	}
	e.svc.Jobs = runner
	jl := decode[JobListResponse](t, mustGet(e, "/api/v1/system/jobs", owner))
	var row *JobJSON
	for i := range jl.Jobs {
		if jl.Jobs[i].Name == "test-job" {
			row = &jl.Jobs[i]
		}
	}
	if row == nil || !row.Registered || row.Status != "never_run" {
		t.Fatalf("a registered job that never ran must be listed as never_run: %+v", jl.Jobs)
	}
	if res, b := e.req("POST", "/api/v1/system/jobs/nope/run", owner, nil); res.StatusCode != 404 {
		t.Fatalf("unknown job = %d %s", res.StatusCode, b)
	}

	before := e.audits()
	res, b := e.req("POST", "/api/v1/system/jobs/test-job/run", owner, nil)
	if res.StatusCode != 202 || decode[JobRunResponse](t, b).Status != "accepted" {
		t.Fatalf("run-now: %d %s", res.StatusCode, b)
	}
	jobBackground.Wait()
	select {
	case <-ran:
	default:
		t.Fatal("the job did not run")
	}
	if got := e.audits() - before; got != 1 {
		t.Errorf("run-now wrote %d audit rows, want 1", got)
	}
	js, err := e.st.GetJobState(context.Background(), "test-job")
	if err != nil || js.LastStatus != "ok" || js.RunCount != 1 {
		t.Fatalf("job state: %+v %v", js, err)
	}
	jl = decode[JobListResponse](t, mustGet(e, "/api/v1/system/jobs", owner))
	for _, j := range jl.Jobs {
		if j.Name == "test-job" && (j.Status != "ok" || j.RunCount != 1 || j.NextDueAt == nil || j.Running) {
			t.Errorf("job row after a run: %+v", j)
		}
		if strings.Contains(string(mustGet(e, "/api/v1/system/jobs", owner)), "test-holder") {
			t.Error("the lease holder (it names the host) is shown")
		}
	}
	// Lease-guarded: while another holder has the lease, 409 and no second run.
	if ok, err := e.st.AcquireJobLease(context.Background(), "test-job", "another-process", time.Hour); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	before = e.audits()
	if res, b := e.req("POST", "/api/v1/system/jobs/test-job/run", owner, nil); res.StatusCode != 409 {
		t.Fatalf("run-now while the lease is held = %d %s, want 409", res.StatusCode, b)
	}
	jobBackground.Wait()
	if len(ran) != 0 || e.audits() != before {
		t.Error("a refused run-now still ran or wrote an audit row")
	}
	for _, j := range decode[JobListResponse](t, mustGet(e, "/api/v1/system/jobs", owner)).Jobs {
		if j.Name == "test-job" && !j.Running {
			t.Error("the job table does not show the held lease as running")
		}
	}
}

func TestM3SystemHealthNeverSaysOKWithoutEvidence(t *testing.T) { // F18 F19, C01 C02 C05
	e := newEnv(t)
	owner := e.login("admin", pw)
	comp := func(h SystemHealthResponse, id string) SystemComponentJSON {
		for _, c := range h.Components {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("no component %s", id)
		return SystemComponentJSON{}
	}
	h := decode[SystemHealthResponse](t, mustGet(e, "/api/v1/system/health", owner))
	for _, id := range []string{"gateway", "smtp", "certificates"} {
		if c := comp(h, id); c.Status != statusNotChecked || c.Detail == "" {
			t.Errorf("%s with no prober record = %+v, want not_checked with a reason", id, c)
		}
	}
	// Storage has another witness: the maintenance worker's own probe (the fixture worker reports it reachable).
	if c := comp(h, "storage"); c.Status != statusOK || !strings.Contains(c.Detail, "maintenance worker") {
		t.Errorf("storage with the worker reporting it reachable = %+v", c)
	}
	if h.Overall == statusOK || h.Versions.Console == "" || h.Versions.Schema == 0 || h.Versions.Maintain == nil || h.Versions.Maintain.ResticVersion != "unknown" {
		t.Errorf("overall/versions: %+v", h)
	}

	// A fresh prober record is the evidence; a banner turns a probe component into failed.
	now := e.clk.Now()
	next := now.Add(time.Minute)
	if err := e.st.RecordJobRun(context.Background(), proberJob, store.JobRun{Status: "ok", StartedAt: now.Add(-time.Second), FinishedAt: now, NextDueAt: &next}); err != nil {
		t.Fatal(err)
	}
	h = decode[SystemHealthResponse](t, mustGet(e, "/api/v1/system/health", owner))
	if c := comp(h, "gateway"); c.Status != statusOK || c.CheckedAt == nil {
		t.Errorf("gateway with a fresh prober record: %+v", c)
	}
	e.svc.Fleet.Extra = func(time.Time) []state.Banner {
		return []state.Banner{{ID: "C01", Severity: state.SeverityCritical, Text: "Gateway unreachable"}, {ID: "C05", Subject: state.SubjectGateway, Severity: state.SeverityAlarm, Text: "Certificate expires in 12 days"}}
	}
	h = decode[SystemHealthResponse](t, mustGet(e, "/api/v1/system/health", owner))
	if c := comp(h, "gateway"); c.Status != statusFailed || len(c.Banners) != 1 || c.Banners[0] != "C01" {
		t.Errorf("gateway with C01: %+v", c)
	}
	if c := comp(h, "certificates"); c.Status != statusWarn {
		t.Errorf("certificates with a warn C05: %+v", c)
	}
	if h.Overall != statusFailed {
		t.Errorf("overall = %s with a failed component", h.Overall)
	}
	// A stale prober record is no evidence any more.
	e.svc.Fleet.Extra = nil
	e.clk.Advance(2 * probeFreshness)
	h = decode[SystemHealthResponse](t, mustGet(e, "/api/v1/system/health", owner))
	if c := comp(h, "gateway"); c.Status != statusNotChecked || !strings.Contains(c.Detail, "last finished") {
		t.Errorf("gateway with an old prober record: %+v", c)
	}
}

func TestM3StorageProbe(t *testing.T) { // F18 F19
	e := newEnv(t)
	owner := e.login("admin", pw)
	t.Cleanup(jobBackground.Wait)
	target := e.firstStorageTarget()
	path := "/api/v1/storage-targets/" + target + "/probe"
	if res, _ := e.req("POST", "/api/v1/storage-targets/nope/probe", owner, nil); res.StatusCode != 404 {
		t.Fatalf("unknown target = %d", res.StatusCode)
	}

	// No health-prober job: the answer is "not checked", never "ok", and it still audits the attempt.
	before := e.audits()
	pr := decode[ProbeResponse](t, mustBody(e, "POST", path, owner, nil))
	if pr.Ran || pr.Reason == "" || len(pr.Checks) != 3 || pr.TargetID != target {
		t.Fatalf("probe without a prober: %+v", pr)
	}
	for _, c := range pr.Checks {
		if c.Status != statusNotChecked {
			t.Errorf("%s = %s without a prober", c.ID, c.Status)
		}
	}
	if e.audits()-before != 1 {
		t.Errorf("probe wrote %d audit rows", e.audits()-before)
	}

	// With the job: it runs now, and the checks are read from the banners the prober feeds.
	runner := jobs.NewRunner(e.st, "probe-test", nil)
	failing := false
	runner.Add(jobs.Job{Name: proberJob, Every: time.Minute, Run: func(context.Context) error {
		if failing {
			return &jobs.ClassError{Class: jobs.ClassIO}
		}
		return nil
	}})
	e.svc.Jobs = runner
	pr = decode[ProbeResponse](t, mustBody(e, "POST", path, owner, nil))
	if !pr.Ran || pr.Job == nil || pr.Job.Status != "ok" || len(pr.Checks) != 3 {
		t.Fatalf("probe with a prober: %+v", pr)
	}
	for _, c := range pr.Checks {
		if c.Status != statusOK {
			t.Errorf("%s = %s with nothing raised", c.ID, c.Status)
		}
	}
	e.svc.Fleet.Extra = func(time.Time) []state.Banner {
		return []state.Banner{{ID: "C02", Severity: state.SeverityCritical, Text: "Storage unreachable"}}
	}
	pr = decode[ProbeResponse](t, mustBody(e, "POST", path, owner, nil))
	for _, c := range pr.Checks {
		if (c.ID == "storage") != (c.Status == statusFailed) {
			t.Errorf("%s = %s with C02 raised", c.ID, c.Status)
		}
	}
	// A prober that itself fails is not a pass.
	failing = true
	pr = decode[ProbeResponse](t, mustBody(e, "POST", path, owner, nil))
	if pr.Ran || pr.Job == nil || pr.Job.Status != "error" || pr.Job.ErrorClass != jobs.ClassIO {
		t.Fatalf("a failing prober: %+v", pr)
	}
	for _, c := range pr.Checks {
		if c.Status != statusNotChecked {
			t.Errorf("%s = %s although the prober failed", c.ID, c.Status)
		}
	}
	// Lease held elsewhere: 409.
	if ok, err := e.st.AcquireJobLease(context.Background(), proberJob, "someone-else", time.Hour); err != nil || !ok {
		t.Fatal(err)
	}
	if res, _ := e.req("POST", path, owner, nil); res.StatusCode != 409 {
		t.Errorf("probe while the prober runs elsewhere = %d, want 409", res.StatusCode)
	}
}

// ---- status and metrics ----

func TestM3StatusOverallIsOKOnlyOnEvidence(t *testing.T) { // F14 F20
	e := newEnv(t)
	owner := e.login("admin", pw)
	st := decode[StatusResponse](t, mustGet(e, "/api/v1/status", owner))
	if st.Overall != OverallCritical || st.AllClear || !st.Complete || len(st.Customers) < 6 {
		t.Fatalf("status of the fixtures: overall %s all_clear %v complete %v customers %d", st.Overall, st.AllClear, st.Complete, len(st.Customers))
	}
	// Narrowed to a healthy customer it reads that customer's state, but all_clear stays the scope's.
	pine := e.customerNamed("Pinecrest Legal")
	one := decode[StatusResponse](t, mustGet(e, "/api/v1/status?customer="+pine.ID, owner))
	if len(one.Customers) != 1 || one.Customers[0].Status != "ok" || len(one.Devices) != 3 || one.AllClear {
		t.Errorf("one customer: %+v", one)
	}
	// overall() on views: empty fleet without a first run is unknown, a first run is not.
	empty := state.FleetView{Complete: true}
	if got := overall(empty, nil, nil); got != OverallUnknown {
		t.Errorf("an empty fleet that was enrolled before = %s, want unknown", got)
	}
	empty.FirstRun = true
	if got := overall(empty, nil, nil); got != OverallOK {
		t.Errorf("a first-run console with nothing wrong = %s", got)
	}
	incomplete := state.FleetView{Complete: false, FirstRun: true}
	if got := overall(incomplete, nil, nil); got != OverallUnknown {
		t.Errorf("incomplete data = %s, want unknown", got)
	}
	warnOnly := []state.Banner{{ID: "C10", Severity: state.SeverityAlarm}}
	if got := overall(state.FleetView{Complete: true, FirstRun: true, Banners: warnOnly}, nil, nil); got != OverallWarning {
		t.Errorf("a warn banner = %s", got)
	}
	crit := []state.Banner{{ID: "C01", Severity: state.SeverityCritical}}
	if got := overall(state.FleetView{Complete: false, FirstRun: true, Banners: crit}, nil, nil); got != OverallCritical {
		t.Errorf("a critical banner outranks incomplete data: %s", got)
	}
}

var metricLine = regexp.MustCompile(`^(# (HELP|TYPE) [a-z_]+ .+|[a-z_]+(\{(state|severity|status|job)="[^"\\]*"\})? -?[0-9.e+]+)$`)

func TestM3MetricsAreCountsAndDurationsOnly(t *testing.T) { // F20, HIPAA posture
	e := newEnv(t)
	owner := e.login("admin", pw)
	t.Cleanup(jobBackground.Wait)
	next := e.clk.Now().Add(-time.Hour)
	if err := e.st.RecordJobRun(context.Background(), "heartbeat-ring-trim", store.JobRun{Status: "ok", StartedAt: e.clk.Now().Add(-2 * time.Hour), FinishedAt: e.clk.Now().Add(-2*time.Hour + 1500*time.Millisecond), NextDueAt: &next}); err != nil {
		t.Fatal(err)
	}
	res, body := e.req("GET", "/metrics", owner, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	text := string(body)
	for i, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if !metricLine.MatchString(line) {
			t.Errorf("line %d is not a plain counter or gauge with a state/severity/status/job label: %q", i+1, line)
		}
	}
	// Nothing that names a customer, a device or a path, whatever the fixtures hold.
	var names []string
	for _, c := range store.FixtureCustomerExpectations() {
		_ = c
	}
	for n := range store.FixtureCustomerExpectations() {
		names = append(names, n)
	}
	for _, x := range store.FixtureExpectations() {
		names = append(names, x.Host)
	}
	for _, n := range names {
		if strings.Contains(strings.ToLower(text), strings.ToLower(n)) {
			t.Errorf("metrics mention %q", n)
		}
	}
	for _, want := range []string{`cryobackup_devices{state="STALE"} `, `cryobackup_devices{state="PAUSED"} 1`, `cryobackup_customers{status="missing"} 1`,
		`cryobackup_banners{severity="critical"} `, "cryobackup_fleet_complete 1", "cryobackup_fleet_all_clear 0",
		`cryobackup_job_up{job="heartbeat-ring-trim"} 1`, `cryobackup_job_overdue{job="heartbeat-ring-trim"} 1`, `cryobackup_job_last_duration_seconds{job="heartbeat-ring-trim"} 1.5`,
		"cryobackup_schema_version "} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	// A scoped account's counts are its own.
	nw := e.customerNamed("Northwind Dental")
	e.addAdmin("m3-metrics", "read_only", []string{nw.ID})
	_, sb := e.req("GET", "/metrics", e.login("m3-metrics", pw), nil)
	if !strings.Contains(string(sb), `cryobackup_customers{status="missing"} 0`) || !strings.Contains(string(sb), `cryobackup_devices{state="PAUSED"} 1`) || strings.Contains(string(sb), `cryobackup_devices{state="STALE"} 1`) {
		t.Errorf("scoped metrics: %.600s", sb)
	}
}

func TestM3StatusAndMetricsTakeAnAPIToken(t *testing.T) { // F20: external monitors
	e := newEnv(t)
	owner := e.login("admin", pw)
	res, b := e.req("POST", "/api/v1/me/api-tokens", owner, TokenCreateRequest{Name: "monitor", Scope: "read", Days: 30})
	if res.StatusCode != 201 {
		t.Fatalf("token: %d %s", res.StatusCode, b)
	}
	tok := decode[TokenCreateResponse](t, b).Token
	hdr := []string{"Authorization", "Bearer " + tok}
	for _, p := range []string{"/api/v1/status", "/metrics", "/api/v1/fleet/summary"} {
		if res, b := e.req("GET", p, nil, nil, hdr...); res.StatusCode != 200 {
			t.Errorf("GET %s with a read token = %d %s", p, res.StatusCode, b)
		}
	}
	for _, c := range []struct{ method, path string }{{"POST", "/api/v1/devices/" + e.dev.ID + "/pause"}, {"POST", "/api/v1/system/jobs/x/run"}, {"GET", "/api/v1/system/jobs"}} {
		if res, _ := e.req(c.method, c.path, nil, DevicePauseRequest{Reason: "x", Indefinite: true}, hdr...); res.StatusCode != 403 {
			t.Errorf("%s %s with a read token = %d, want 403", c.method, c.path, res.StatusCode)
		}
	}
}

// ---- the event stream ----

type sseEvent struct {
	Name string
	Data string
}

type sseStream struct {
	t      *testing.T
	events chan sseEvent
	cancel context.CancelFunc
}

func (e *env) openStream(path string, s *session, hdr ...string) (*sseStream, *http.Response) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
	if s != nil {
		for _, c := range s.cookies {
			req.AddCookie(c)
		}
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	st := &sseStream{t: e.t, events: make(chan sseEvent, 64), cancel: cancel}
	if res.StatusCode != 200 {
		return st, res
	}
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if ev.Name != "" {
					st.events <- ev
				}
				ev = sseEvent{}
			}
		}
		close(st.events)
	}()
	return st, res
}

// next returns the next event named name (others are skipped), or fails after d.
func (st *sseStream) next(name string, d time.Duration) sseEvent {
	st.t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-st.events:
			if !ok {
				st.t.Fatalf("the stream ended while waiting for %s", name)
			}
			if ev.Name == name {
				return ev
			}
		case <-timer.C:
			st.t.Fatalf("no %s event within %s", name, d)
		}
	}
}

// none asserts no event named name arrives within d.
func (st *sseStream) none(name string, d time.Duration) {
	st.t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-st.events:
			if !ok {
				return
			}
			if ev.Name == name {
				st.t.Fatalf("unexpected %s event: %s", name, ev.Data)
			}
		case <-timer.C:
			return
		}
	}
}

type fakeSource struct {
	mu      sync.Mutex
	chans   []chan Signal
	changed []string
}

func (f *fakeSource) Watch() (<-chan Signal, func()) {
	ch := make(chan Signal, 16)
	f.mu.Lock()
	f.chans = append(f.chans, ch)
	f.mu.Unlock()
	return ch, func() {}
}

func (f *fakeSource) DeviceChanged(id string) {
	f.mu.Lock()
	f.changed = append(f.changed, id)
	f.mu.Unlock()
	f.send(Signal{Kind: SignalChange, DeviceID: id})
}

func (f *fakeSource) send(sg Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.chans {
		c <- sg
	}
}

func TestM3EventStreamDeliversAndIsScoped(t *testing.T) { // F05 F18
	e := newEnv(t)
	src := &fakeSource{}
	e.svc.Events, e.svc.sseRefresh, e.svc.sseGap = src, time.Hour, 10*time.Millisecond
	owner := e.login("admin", pw)

	st, res := e.openStream("/api/v1/events", owner)
	if res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", res.Header)
	}
	first := st.next("fleet.state", 5*time.Second)
	var sum FleetSummaryResponse
	if err := json.Unmarshal([]byte(first.Data), &sum); err != nil || sum.Devices.Total == 0 || !sum.Complete {
		t.Fatalf("first fleet.state: %v %s", err, first.Data)
	}
	bn := st.next("banner.update", 5*time.Second)
	var bu BannerUpdateJSON
	if err := json.Unmarshal([]byte(bn.Data), &bu); err != nil || len(bu.Banners) == 0 {
		t.Fatalf("banner.update: %v %s", err, bn.Data)
	}

	// A device change re-evaluates; an unchanged banner set is not repeated.
	src.send(Signal{Kind: SignalChange, DeviceID: e.dev.ID})
	st.next("fleet.state", 5*time.Second)
	st.none("banner.update", 100*time.Millisecond)
	// Progress of a run reaches the stream.
	src.send(Signal{Kind: SignalProgress, DeviceID: e.dev.ID, RunID: "01J9Y4K3A0B1C2D3E4F5G6H7K9"})
	var rp RunProgressJSON
	if err := json.Unmarshal([]byte(st.next("run.progress", 5*time.Second).Data), &rp); err != nil || rp.DeviceID != e.dev.ID || rp.RunID == "" {
		t.Fatalf("run.progress: %+v %v", rp, err)
	}
	// A banner that appears is pushed.
	e.svc.Fleet.Extra = func(time.Time) []state.Banner {
		return []state.Banner{{ID: "C16", Severity: state.SeverityAlarm, Text: "Release manifest rejected."}}
	}
	src.send(Signal{Kind: SignalChange})
	if err := json.Unmarshal([]byte(st.next("banner.update", 5*time.Second).Data), &bu); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range bu.Banners {
		found = found || b.ID == "C16"
	}
	if !found {
		t.Errorf("the new banner is not in banner.update: %+v", bu.Banners)
	}
	// A pause through the API tells the hub, so every open stream re-renders.
	e.addAdmin("m3-sse-op", "operator", nil)
	op := e.login("m3-sse-op", pw)
	e.mustReq("POST", "/api/v1/devices/"+e.held.ID+"/pause", op, DevicePauseRequest{Reason: "x", Indefinite: true}, 200)
	st.next("fleet.state", 5*time.Second)
	src.mu.Lock()
	if len(src.changed) != 1 || src.changed[0] != e.held.ID {
		t.Errorf("the hub was told %v", src.changed)
	}
	src.mu.Unlock()

	// topics narrows; an unknown topic is a 400.
	only, _ := e.openStream("/api/v1/events?topics=runs", owner)
	src.send(Signal{Kind: SignalProgress, DeviceID: e.dev.ID, RunID: "01J9Y4K3A0B1C2D3E4F5G6H7KA"})
	only.next("run.progress", 5*time.Second)
	only.none("fleet.state", 100*time.Millisecond)
	if _, res := e.openStream("/api/v1/events?topics=everything", owner); res.StatusCode != 400 {
		t.Errorf("unknown topic = %d", res.StatusCode)
	}

	// An account scoped to one customer gets its fleet and its devices' progress only.
	nw := e.customerNamed("Northwind Dental")
	e.addAdmin("m3-sse-scoped", "read_only", []string{nw.ID})
	sc := e.login("m3-sse-scoped", pw)
	scoped, _ := e.openStream("/api/v1/events", sc)
	var ssum FleetSummaryResponse
	if err := json.Unmarshal([]byte(scoped.next("fleet.state", 5*time.Second).Data), &ssum); err != nil || ssum.Devices.Total != 3 {
		t.Fatalf("scoped fleet.state: %+v %v", ssum.Devices, err)
	}
	src.send(Signal{Kind: SignalProgress, DeviceID: e.dev.ID, RunID: "01J9Y4K3A0B1C2D3E4F5G6H7KB"})                      // Summit's device
	src.send(Signal{Kind: SignalProgress, DeviceID: e.deviceNamed("OPS-ND-01").ID, RunID: "01J9Y4K3A0B1C2D3E4F5G6H7KC"}) // Northwind's
	var sp RunProgressJSON
	if err := json.Unmarshal([]byte(scoped.next("run.progress", 5*time.Second).Data), &sp); err != nil || sp.RunID != "01J9Y4K3A0B1C2D3E4F5G6H7KC" {
		t.Errorf("the scoped stream's first progress is %+v (Summit's must not arrive)", sp)
	}
}

func TestM3EventStreamWithoutAHubStillRefreshesAndFailsLoudly(t *testing.T) {
	e := newEnv(t)
	e.svc.sseRefresh, e.svc.sseGap = 30*time.Millisecond, 10*time.Millisecond
	owner := e.login("admin", pw)
	st, _ := e.openStream("/api/v1/events?topics=fleet", owner)
	st.next("fleet.state", 5*time.Second)
	st.next("fleet.state", 5*time.Second) // the refresh tick alone keeps it moving
	st.cancel()
}

func TestM3EventStreamTakesAnAPIToken(t *testing.T) {
	e := newEnv(t)
	e.svc.sseRefresh = time.Hour
	owner := e.login("admin", pw)
	_, b := e.req("POST", "/api/v1/me/api-tokens", owner, TokenCreateRequest{Name: "stream", Scope: "read", Days: 7})
	tok := decode[TokenCreateResponse](t, b).Token
	st, res := e.openStream("/api/v1/events?topics=fleet", nil, "Authorization", "Bearer "+tok)
	if res.StatusCode != 200 {
		t.Fatalf("stream with a token: %d", res.StatusCode)
	}
	st.next("fleet.state", 5*time.Second)
	if _, res := e.openStream("/api/v1/events", nil); res.StatusCode != 401 {
		t.Errorf("stream without credentials = %d", res.StatusCode)
	}
	_ = io.Discard
}
