package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 delta round tests.

// canned replaces the pages' private admin API with fixed answers, to test what a
// page does with an answer the real API would only give under rare conditions
// (partial data, an unknown status). A path with no entry answers 404.
func (e *env) canned(routes map[string]any) {
	e.t.Helper()
	mux := http.NewServeMux()
	for path, v := range routes {
		b, err := json.Marshal(v)
		if err != nil {
			e.t.Fatal(err)
		}
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		})
	}
	e.svc.m3.mux = mux
}

func (e *env) countEvals() *atomic.Int32 {
	var n atomic.Int32
	e.svc.Fleet.Extra = func(time.Time) []state.Banner { n.Add(1); return nil } // Extra runs once per evaluation
	return &n
}

// ---- item 6: one evaluation per request ----

func TestM3T4EveryPageEvaluatesTheFleetOncePerRequest(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.m3Silence() // silence rows, expanded rows and the summary all want the fleet too
	e.login()
	paths := e.m3Pages()
	paths["unseen expanded"] = "/fleet/unseen?show=" + e.customerID("Summit Eye Care")
	paths["runs with a condition"] = "/runs?add=status:failed"
	paths["system after run"] = "/system?flash=x~1~y"
	n := e.countEvals()
	for name, path := range paths {
		before := n.Load()
		res, _ := e.do("GET", path, nil)
		if res.StatusCode >= 500 {
			t.Fatalf("%s: %d", name, res.StatusCode)
		}
		if got := n.Load() - before; got > 1 {
			t.Errorf("%s evaluated the fleet %d times for one request, want 1", name, got)
		}
		if name != "runs with a condition" && n.Load()-before == 0 && !strings.Contains(path, "/ui/") && res.StatusCode == 200 {
			t.Errorf("%s did not evaluate the fleet at all: its banners and stamp cannot be current", name)
		}
	}
	// A POST that renders the System page evaluates once too, and the next request starts fresh.
	before := n.Load()
	e.post("/ui/system/jobs/no-such/run", url.Values{}, false)
	if got := n.Load() - before; got != 1 {
		t.Errorf("run now evaluated %d times", got)
	}
	before = n.Load()
	e.page("/fleet/overdue", 200)
	e.page("/fleet/overdue", 200)
	if got := n.Load() - before; got != 2 {
		t.Errorf("two requests evaluated %d times, want 2 (one each)", got)
	}
}

// ---- item 6: paging by device inside a customer ----

func TestM3T4UnitWindowPagesByDeviceInsideABigCustomer(t *testing.T) {
	// A 3000-device customer between two small ones: 5, 3000, 1 (an empty customer is one row).
	units := []int{5, 3000, 1}
	for _, tc := range []struct {
		lo, hi int
		want   []winPart
	}{
		{0, 50, []winPart{{0, 0, 5}, {1, 0, 45}}},
		{50, 100, []winPart{{1, 45, 95}}},
		{2950, 3000, []winPart{{1, 2945, 2995}}},
		{3000, 3050, []winPart{{1, 2995, 3000}, {2, 0, 1}}},
		{3006, 3056, nil},
	} {
		got := unitWindow(units, tc.lo, tc.hi)
		if len(got) != len(tc.want) {
			t.Errorf("window %d-%d: %+v, want %+v", tc.lo, tc.hi, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("window %d-%d part %d: %+v, want %+v", tc.lo, tc.hi, i, got[i], tc.want[i])
			}
		}
	}
	// On the page: every device appears on exactly one page, and a page past the end redirects to the last.
	e, _ := newM3Env(t)
	e.login()
	seen := map[string]int{}
	for pageNo := 1; pageNo <= 2; pageNo++ {
		body := e.page("/activity?size=25&page="+strconv.Itoa(pageNo), 200)
		for _, m := range rowRE.FindAllStringSubmatch(body, -1) {
			seen[m[1]]++
		}
		mustContain(t, "page text", body, "Showing "+map[int]string{1: "1-25", 2: "26-27"}[pageNo]+" of 27")
	}
	if len(seen) != 25 {
		t.Errorf("%d devices over two pages, want 25", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("device %s on %d pages", id, n)
		}
	}
	if res, _ := e.do("GET", "/activity?size=25&page=9", nil); res.StatusCode != 303 || !strings.Contains(res.Header.Get("Location"), "page=2") {
		t.Errorf("a page past the end: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

// ---- item 3: no-data states that are alarms ----

func TestM3T4ActivityAlarmsWhenEveryDeviceIsRetiredAndSaysWhenTheFilterFindsNothing(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	// A healthy customer under the failed-or-missed filter: an explicit answer with its evidence, not a blank page.
	pine := e.customerID("Pinecrest Legal")
	body := e.page("/activity?customer="+pine+"&problems=1", 200)
	mustContain(t, "filter finds nothing", body, "No device has a failed, mixed or missed day", "3 devices checked at 1:42:10 PM (America/Chicago)", `name="problems"`)
	if strings.Contains(body, "panel-state alarm") {
		t.Error("a checked healthy customer is an alarm")
	}
	// Every device retired, and not the first run: an alarm, never a row of neutral panels.
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE devices SET paused_at = NULL, paused_until = NULL, pause_reason = NULL, paused_by = NULL WHERE paused_at IS NOT NULL`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE devices SET retired_at = ?, retired_reason = 'replaced' WHERE retired_at IS NULL`, e.now.Add(-time.Hour).UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body = e.page("/activity", 200)
	mustContain(t, "all retired", body, "panel-state alarm", "No devices to show", "not a quiet fleet")
	if strings.Contains(body, "No customers yet") {
		t.Error("a console that has enrolled before says it is new")
	}
}

// ---- item 5: the silence row's link lists exactly the devices it counts ----

func TestM3T4SilenceRowLinkListsExactlyTheDevicesItCounts(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	e.m3Silence()
	summit := e.customerID("Summit Eye Care")
	var api adminapi.FleetUnseenResponse
	e.apiGet("/api/v1/fleet/unseen", &api)
	var silence adminapi.UnseenRowJSON
	for _, r := range api.Rows {
		if r.Kind == "customer_silence" {
			silence = r
		}
	}
	if len(silence.DeviceIDs) != 10 {
		t.Fatalf("the engine's silence set has %d devices", len(silence.DeviceIDs))
	}
	body := e.page("/fleet/unseen", 200)
	mustContain(t, "link", body, `href="/fleet/unseen?show=`+summit+`#silence-`+summit+`">Show the 10 devices</a>`)
	if strings.Contains(body, "state=UNSEEN") {
		t.Error("the link is still a state filter, which matches primary states only")
	}
	open := e.page("/fleet/unseen?show="+summit, 200)
	rows := regexp.MustCompile(`class="dev silence-dev" id="dev-([0-9A-Za-z]+)"`).FindAllStringSubmatch(open, -1)
	if len(rows) != 10 {
		t.Fatalf("%d devices listed under the row that says 10", len(rows))
	}
	want := map[string]bool{}
	for _, id := range silence.DeviceIDs {
		want[id] = true
	}
	for _, m := range rows {
		if !want[m[1]] {
			t.Errorf("device %s is listed but not in the collapse set", m[1])
		}
	}
	mustContain(t, "hide", open, `>Hide the 10 devices</a>`)
	// Devices of the set whose primary state is not UNSEEN are listed too: that is what the state filter missed.
	notUnseen := 0
	for _, id := range silence.DeviceIDs {
		var d adminapi.DeviceDetailJSON
		e.apiGet("/api/v1/devices/"+id, &d)
		if d.State != string(state.StateUnseen) {
			notUnseen++
		}
	}
	if notUnseen == 0 {
		t.Log("every device of the set is primary UNSEEN in this fleet; the exact-list rule is still what the page does")
	}
	// A show= for a customer with no silence row changes nothing.
	if strings.Contains(e.page("/fleet/unseen?show="+e.customerID("Northwind Dental"), 200), "silence-dev") {
		t.Error("expanded a customer that has no silence row")
	}
}

// ---- item 1 on the page, and items 7 (survivors) ----

func TestM3T4SystemBuildFactShowsTheRealStamp(t *testing.T) {
	facts := versionFacts(adminapi.VersionsJSON{Console: "1.4.2", Commit: "abc1234", BuildDate: "2026-11-05T10:20:30Z", Schema: 5}, fmtCtx{})
	var build string
	for _, f := range facts {
		if f.Key == "Build" {
			build = f.Value
		}
	}
	if build != "abc1234 · 2026-11-05T10:20:30Z" {
		t.Errorf("build = %q", build)
	}
	for _, f := range versionFacts(adminapi.VersionsJSON{Console: "Patient_Chart.docx", Commit: "secret-payroll.xlsx", BuildDate: "John Smith MRI.pdf"}, fmtCtx{}) {
		if strings.Contains(f.Value, "Patient") || strings.Contains(f.Value, "secret") || strings.Contains(f.Value, "John") {
			t.Errorf("a fact carries a file name: %+v", f)
		}
	}
}

func TestM3T4RunsFragmentIsPinnedAndScopedAndAFailedFleetIsNotBlank(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	summit, north := e.customerID("Summit Eye Care"), e.customerID("Northwind Dental")
	frag := e.page("/ui/runs/table?pin="+summit+"&size=200", 200)
	if !strings.Contains(frag, "FRONTDESK-01") || strings.Contains(frag, "HYGIENE-PC-2") || strings.Contains(frag, "Pinecrest Legal") || strings.Contains(frag, "<th scope=\"col\">Customer</th>") {
		t.Errorf("the fragment of a customer tab is not pinned to its customer")
	}
	e.signInAs("scoped-frag", "read_only", []string{north})
	if res, _ := e.do("GET", "/ui/runs/table?pin="+summit, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("a pin outside the scope: %d", res.StatusCode)
	}
	if body := e.page("/ui/runs/table?size=200", 200); strings.Contains(body, "FRONTDESK-01") {
		t.Error("the fragment of a scoped account shows another customer")
	}
	// The fleet cannot be evaluated: the fragment is an alarm panel, the page an error page, never blank.
	e.login()
	closed, err := store.OpenMemory(context.Background(), e.svc.Clock)
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	e.svc.Fleet = &health.Evaluator{Store: closed, Clock: e.svc.Clock}
	res, body := e.do("GET", "/ui/runs/table", nil)
	if res.StatusCode != 200 || !strings.Contains(body, "panel-state alarm") || !strings.Contains(body, "could not be loaded") || strings.TrimSpace(body) == "" {
		t.Errorf("fragment with a failed fleet: %d %.300s", res.StatusCode, body)
	}
	if res, body := e.do("GET", "/runs", nil); res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "alarm") {
		t.Errorf("page with a failed fleet: %d", res.StatusCode)
	}
}

func TestM3T4NoRunsForEnrolledDevicesIsAnAlarm(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec(`DELETE FROM runs`); return err }); err != nil {
		t.Fatal(err)
	}
	body := e.page("/runs", 200)
	if !strings.Contains(body, "panel-state alarm") || !strings.Contains(body, "No runs are recorded for") {
		t.Errorf("no runs at all for enrolled devices is not an alarm: %.400s", body)
	}
	tab := e.page("/customers/"+e.customerID("Summit Eye Care")+"/runs", 200)
	if !strings.Contains(tab, "panel-state alarm") {
		t.Error("a customer with devices and no runs is not an alarm")
	}
}

func TestM3T4PartialAnswersAreWarningsNotCleanLists(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	dev := adminapi.DeviceJSON{ID: "dev1", CustomerID: "c1", Customer: "Summit Eye Care", Hostname: "PARTIAL-TEST-01", State: "stale", Flags: []string{}, Attributes: []string{}}
	stamp := adminapi.StampJSON{ComputedAt: e.now, LoadedAt: e.now, Complete: false}
	e.canned(map[string]any{
		"/api/v1/fleet/overdue": adminapi.FleetOverdueResponse{StampJSON: stamp, Total: 1, Devices: []adminapi.OverdueRowJSON{{DeviceJSON: dev, Chips: []string{"stale"}}}},
		"/api/v1/fleet/unseen":  adminapi.FleetUnseenResponse{StampJSON: stamp, Devices: 1, Total: 1, Rows: []adminapi.UnseenRowJSON{{Kind: "device", CustomerID: "c1", Customer: "Summit Eye Care", Device: &dev}}},
		"/api/v1/runs":          adminapi.RunListResponse{ComputedAt: e.now, Total: 5, Of: 5, Runs: []adminapi.RunRowJSON{{RunJSON: adminapi.RunJSON{ID: ulid(5), Kind: "backup", Trigger: "schedule", Origin: "agent", StartedAt: e.now, Attempt: 1}, Hostname: "PARTIAL-TEST-01", Customer: "Summit Eye Care"}}},
		"/api/v1/fleet/ten-day": adminapi.FleetTenDayResponse{StampJSON: stamp, Customers: []adminapi.TenDayCustomerJSON{{ID: "c1", Name: "Summit Eye Care", Status: "alarm"}}},
	})
	for name, path := range map[string]string{"overdue": "/fleet/overdue", "unseen": "/fleet/unseen"} {
		body := e.page(path, 200)
		if !strings.Contains(body, "panel-state partial") || !strings.Contains(body, "Partial data") || !strings.Contains(body, "PARTIAL-TEST-01") {
			t.Errorf("%s: a list the console says is incomplete does not warn", name)
		}
	}
	if body := e.page("/runs", 200); !strings.Contains(body, "Partial answer.") {
		t.Error("a run list shorter than its count does not warn")
	}
	if body := e.page("/activity", 200); !strings.Contains(body, "panel-state partial") || !strings.Contains(body, "Partial data") {
		t.Error("an incomplete activity answer does not warn")
	}
}

func TestM3T4TheMissingChipIsCriticalAndAnUnknownJobStatusIsNotOK(t *testing.T) {
	if c := customerStatusChip("x", "missing"); c.Class != ClassCrit || c.Label != "MISSING" {
		t.Errorf("MISSING chip: %+v", c)
	}
	if c := customerStatusChip("x", "never-heard-of"); c.Class != ClassCrit || !c.Dashed {
		t.Errorf("unknown customer status: %+v", c)
	}
	if c := customerStatusChip("x", "ok"); c.Class != ClassOK {
		t.Errorf("ok customer: %+v", c)
	}
	e, _ := newM3Env(t)
	e.login()
	last := e.now.Add(-time.Minute)
	e.canned(map[string]any{
		"/api/v1/system/health": adminapi.SystemHealthResponse{StampJSON: adminapi.StampJSON{ComputedAt: e.now, LoadedAt: e.now, Complete: true}, Overall: "ok",
			Components: []adminapi.SystemComponentJSON{{ID: "gateway", Label: "Gateway", Status: "ok", Banners: []string{}}}},
		"/api/v1/system/jobs": adminapi.JobListResponse{ComputedAt: e.now, Jobs: []adminapi.JobJSON{
			{Name: "weird-job", Registered: true, Status: "melting", LastFinishedAt: &last},
			{Name: "good-job", Registered: true, Status: "ok", LastFinishedAt: &last},
			{Name: "Not A Job Name", Registered: true, Status: "ok"},
		}},
	})
	body := e.page("/system", 200)
	row := func(id string) string {
		i := strings.Index(body, `id="`+id+`"`)
		if i < 0 {
			t.Fatalf("no row %s", id)
		}
		return body[i : i+strings.Index(body[i:], "</tr>")]
	}
	if w := row("job-weird-job"); !strings.Contains(w, "UNKNOWN") || strings.Contains(w, ">OK<") || !strings.Contains(w, "st crit dashed") {
		t.Errorf("an unknown job status is drawn calm: %s", w)
	}
	if g := row("job-good-job"); !strings.Contains(g, "st ok") {
		t.Errorf("an ok job is not ok: %s", g)
	}
	if n := row("job-" + withheld); strings.Contains(n, "Run now") {
		t.Errorf("a job with a name the console does not use can be run from the page: %s", n)
	}
}

// ---- Run now: the form carries its token, shutdown refuses, the flash cannot be faked ----

func TestM3T4RunNowFormCarriesItsTokenAndWorksFromTheRenderedForm(t *testing.T) {
	e, jobs := newM3Env(t)
	e.login()
	body := e.page("/system", 200)
	m := regexp.MustCompile(`(?s)<form method="post" action="(/ui/system/jobs/alarm-evaluator/run)" class="stack"><input type="hidden" name="csrf_token" value="([^"]+)">`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the Run now form has no csrf_token field")
	}
	if m[2] != e.csrf {
		t.Errorf("the form's token is not the session's")
	}
	// Post exactly what the form holds.
	res, _ := e.do("POST", m[1], url.Values{"csrf_token": {m[2]}}, "Origin", e.srv.URL)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("posting the rendered form: %d", res.StatusCode)
	}
	jobs.waitRun(t)
	if res, _ := e.do("POST", m[1], url.Values{}, "Origin", e.srv.URL); res.StatusCode != http.StatusForbidden {
		t.Errorf("without the token: %d", res.StatusCode)
	}
}

func TestM3T4BeginShutdownRefusesRunNowFromTheFirstMoment(t *testing.T) {
	e, jobs := newM3Env(t)
	e.login()
	e.svc.BeginShutdown() // before any page has been rendered
	before := e.auditTotal()
	res, page := e.post("/ui/system/jobs/alarm-evaluator/run", url.Values{}, false)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, "cannot run jobs now") {
		t.Fatalf("run now after BeginShutdown: %d", res.StatusCode)
	}
	select {
	case n := <-jobs.done:
		t.Fatalf("%q ran after shutdown began", n)
	case <-time.After(300 * time.Millisecond):
	}
	if e.auditTotal() != before || len(jobs.runs()) != 0 {
		t.Error("shutdown left a trace")
	}
	if !e.svc.WaitBackground(time.Second) {
		t.Error("WaitBackground did not return with nothing running")
	}
}

func TestM3T4TheAcceptedFlashCannotBeFakedByAURL(t *testing.T) {
	e, jobs := newM3Env(t)
	e.login()
	for name, u := range map[string]string{
		"the old query":     "/system?ran=alarm-evaluator&result=accepted",
		"a made-up token":   "/system?flash=alarm-evaluator~9999999999~" + strings.Repeat("0", 64),
		"a bare job name":   "/system?flash=alarm-evaluator",
		"an expired format": "/system?flash=alarm-evaluator~1~" + strings.Repeat("a", 64),
	} {
		if body := e.page(u, 200); strings.Contains(body, "Run accepted.") {
			t.Errorf("%s: the page claims a run that never happened", name)
		}
	}
	res, _ := e.post("/ui/system/jobs/alarm-evaluator/run", url.Values{}, false)
	loc := strings.TrimSuffix(res.Header.Get("Location"), "#jobs")
	jobs.waitRun(t)
	if !strings.Contains(e.page(loc, 200), "Run accepted.") {
		t.Fatal("the real token is not honoured")
	}
	// The same token for another job name, or in another session, shows nothing. Its expiry is
	// TestR3TheFlashTokenExpiresInTheSameSession: only the same session can tell the expiry from the binding.
	if strings.Contains(e.page(strings.Replace(loc, "alarm-evaluator~", "session-expire~", 1), 200), "Run accepted.") {
		t.Error("a token for one job accepted for another")
	}
	e.signInAs("other-admin", "admin", nil)
	if strings.Contains(e.page(loc, 200), "Run accepted.") {
		t.Error("a token made in one session shows in another")
	}
}

// N22: the flash token's expiry, in the same session, so the session binding cannot be what refuses it.
func TestR3TheFlashTokenExpiresInTheSameSession(t *testing.T) { // M3 N22
	e, jobs := newM3Env(t)
	e.login()
	res, _ := e.post("/ui/system/jobs/alarm-evaluator/run", url.Values{}, false)
	loc := strings.TrimSuffix(res.Header.Get("Location"), "#jobs")
	jobs.waitRun(t)
	start := e.now
	e.now = start.Add(flashTTL - time.Second)
	if !strings.Contains(e.page(loc, 200), "Run accepted.") {
		t.Fatal("control: the token is refused inside its lifetime")
	}
	e.now = start.Add(flashTTL + 2*time.Second)
	if strings.Contains(e.page(loc, 200), "Run accepted.") {
		t.Error("an expired token is honoured")
	}
}

// ---- fixtures mode ----

func TestM3T4FixturesModeBringsItsOwnJobsAndABannerSwitch(t *testing.T) {
	t.Cleanup(func() { fixtures.SetAllBanners(false) })
	e := newEnv(t) // Fixtures true, no job runner set: the pages use the fictional jobs
	e.login()
	body := e.page("/system", 200)
	for _, name := range fixtures.JobNames {
		mustContain(t, "fixtures jobs", body, `id="job-`+name+`"`)
	}
	mustContain(t, "run now offered", body, `class="rowmenu act-confirm"`)
	before := e.auditTotal()
	res, _ := e.post("/ui/system/jobs/alarm-evaluator/run", url.Values{}, false)
	if res.StatusCode != http.StatusSeeOther || len(e.auditRows("system.job.run", "alarm-evaluator")) != 1 || e.auditTotal()-before != 1 {
		t.Fatalf("Run now in fixtures mode: %d, audit rows %d", res.StatusCode, e.auditTotal()-before)
	}
	// The runner is the real one: the run is recorded, so the table shows it.
	var seen bool
	for i := 0; i < 50 && !seen; i++ {
		time.Sleep(100 * time.Millisecond)
		js, _ := e.st.GetJobState(context.Background(), "alarm-evaluator")
		seen = js.LastStatus == "ok"
	}
	if !seen {
		t.Error("the fictional job's run was not recorded")
	}
	// The banner switch: off by default, every banner on every page when on, off again.
	if strings.Contains(e.page("/fixtures", 200), "ALL BANNERS ON") || strings.Contains(e.page("/system", 200), "GATEWAY NOT CHECKED") {
		t.Fatal("the switch is on by default")
	}
	on := e.page("/fixtures?banners=all", 200)
	mustContain(t, "switch", on, "ALL BANNERS ON")
	sys := e.page("/system", 200)
	for _, b := range fixtures.AllBanners(e.now) {
		if !strings.Contains(sys, BannerLabel(b.ID)) {
			t.Errorf("banner %s is not on the System page with the switch on", b.ID)
		}
	}
	mustContain(t, "rows and conditions", sys, "Conditions raised", "C11")
	mustContain(t, "board too", e.page("/", 200), "GATEWAY NOT CHECKED")
	if strings.Contains(e.page("/fixtures?banners=off", 200), "ALL BANNERS ON") || strings.Contains(e.page("/", 200), "GATEWAY NOT CHECKED") {
		t.Error("the switch does not turn off")
	}
}

// ---- low items: live regions, labels, icons ----

func TestM3T4PagesRefreshLikeTheBoardAndLabelTheirPhoneValues(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	for name, path := range map[string]string{"overdue": "/fleet/overdue?customer=" + summit, "unseen": "/fleet/unseen", "activity": "/activity", "system": "/system"} {
		body := e.page(path, 200)
		want := `<div id="m3live" class="stack" data-live data-poll hx-get="` + strings.ReplaceAll(strings.ReplaceAll(path, "&", "&amp;"), "?", "?") + `" hx-trigger="cryo:poll, every 30s" hx-select="#m3live" hx-select-oob="#banners,#freshness" hx-swap="outerHTML">`
		if !strings.Contains(body, want) {
			t.Errorf("%s: no live region (want %s)", name, want)
		}
		if !strings.Contains(body, `<span class="asof"></span></div>`) {
			t.Errorf("%s: the live region has no as-of marker for a lost feed", name)
		}
	}
	runs := e.page("/runs?status=failed", 200)
	mustContain(t, "runs live", runs, `id="runresults"`, `data-live data-poll hx-get="/ui/runs/table?status=failed"`, `hx-trigger="cryo:poll, every 30s"`)
	// The jobs table is outside the polled region, so an open confirm is never swapped away.
	sys := e.page("/system", 200)
	if i, j := strings.Index(sys, `id="m3live"`), strings.Index(sys, `id="job-`); i < 0 || j < i || strings.Index(sys[i:], `<span class="asof"></span></div>`) > j-i {
		t.Error("the job table is inside the live region")
	}
	// Phone: values carry their labels; empty ones are hidden rather than shown as a stray dash.
	for _, l := range []string{`data-label="Last run"`, `data-label="Took"`, `data-label="Next"`, `data-label="Runs so far"`, `data-label="Why"`, `data-label="Raised"`, `data-label="Checked"`} {
		if !strings.Contains(sys, l) {
			t.Errorf("the System page lacks %s", l)
		}
	}
	if strings.Contains(sys, `<td class="due">-</td>`) || strings.Contains(sys, `<span class="muted">-</span>`) {
		t.Error("a bare dash is shown for an empty value")
	}
	act := e.page("/activity", 200)
	mustContain(t, "scroll", act, `data-scroll-end`, `js/m3t4.js`)
	if res, js := e.do("GET", "/static/js/m3t4.js", nil); res.StatusCode != 200 || !strings.Contains(js, "scrollLeft") {
		t.Errorf("m3t4.js: %d", res.StatusCode)
	}
	// The footer says what a block's number is, for devices and for the roll-up.
	mustContain(t, "footer", act, "In a device row a block's number is the jobs that ran that day", `the "All devices" row it is how many devices`)
	if strings.Contains(act, "Show retired") {
		t.Error("the footer names a control that does not exist")
	}
}

func TestM3T4EveryNewNavigationItemHasItsOwnIcon(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	body := e.page("/", 200)
	navRE := regexp.MustCompile(`<a class="navlink" href="([^"]+)"[^>]*><svg aria-hidden="true"><use href="#([a-z-]+)"/>`)
	icons := map[string]string{}
	count := map[string]int{}
	for _, m := range navRE.FindAllStringSubmatch(body, -1) {
		icons[m[1]] = m[2]
		count[m[2]]++
	}
	for _, href := range []string{"/activity", "/runs", "/fleet/overdue", "/fleet/unseen", "/system"} {
		ic := icons[href]
		if ic == "" {
			t.Errorf("no icon for %s", href)
		} else if count[ic] != 1 {
			t.Errorf("%s shares its icon %s with another item", href, ic)
		}
		if !strings.Contains(body, `<symbol id="`+ic+`"`) {
			t.Errorf("%s: icon %s is not defined", href, ic)
		}
	}
}
