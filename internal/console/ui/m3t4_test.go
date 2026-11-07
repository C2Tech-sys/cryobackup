package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 tests. Test ids of the form B04, S21 and so on are the screen ids of
// replica/recon.md.

// m3Pages lists every page this ticket adds, one request each, for the tests
// that look at all of them.
func (e *env) m3Pages() map[string]string {
	summit, blue, lake, north := e.customerID("Summit Eye Care"), e.customerID("Blue Ridge Accounting"), e.customerID("Lakeshore Pediatrics"), e.customerID("Northwind Dental")
	var runs adminapi.RunListResponse
	e.apiGet("/api/v1/runs?limit=1", &runs)
	return map[string]string{
		"runs":                  "/runs",
		"runs filtered":         "/runs?status=failed&status=partial&kind=backup",
		"runs by day":           "/runs?device=" + e.dev["FRONTDESK-01"] + "&from=2026-10-18&to=2026-10-20",
		"runs fragment":         "/ui/runs/table?status=failed",
		"run detail":            "/runs/" + runs.Runs[0].ID,
		"customer runs":         "/customers/" + summit + "/runs",
		"customer runs missing": "/customers/" + blue + "/runs",
		"customer runs new":     "/customers/" + lake + "/runs",
		"customer runs healthy": "/customers/" + north + "/runs",
		"activity":              "/activity",
		"activity problems":     "/activity?problems=1",
		"activity one customer": "/activity?customer=" + summit,
		"overdue":               "/fleet/overdue",
		"overdue one customer":  "/fleet/overdue?customer=" + summit,
		"unseen":                "/fleet/unseen",
		"unseen one customer":   "/fleet/unseen?customer=" + north,
		"system":                "/system",
	}
}

// ---- rendering, banners, states ----

func TestM3T4PagesRenderInFixturesMode(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	for name, path := range e.m3Pages() {
		body := e.page(path, 200)
		if strings.Contains(body, "ZgotmplZ") {
			t.Errorf("%s: html/template rejected a value (ZgotmplZ)", name)
		}
		if absoluteURL.MatchString(body) {
			t.Errorf("%s: an absolute URL reference (no CDN, no third party)", name)
		}
		if pathLike.MatchString(body) {
			t.Errorf("%s: the page carries something path-like", name)
		}
		if !strings.Contains(path, "/ui/") && !strings.Contains(body, `id="freshness"`) {
			t.Errorf("%s: no freshness stamp", name)
		}
	}
}

// Every banner the console can raise (C01 to C16, C00, the "b" not-checked
// warnings, C11 critical) shows on every page that has the strip, each under its
// own label rather than "CONDITION C01b".
func TestM3T4EveryBannerShowsOnEveryPage(t *testing.T) {
	e, _ := newM3Env(t)
	e.svc.Fleet.Extra = func(now time.Time) []state.Banner { return allBanners(now) }
	e.login()
	all := allBanners(e.now)
	for name, path := range e.m3Pages() {
		if strings.Contains(path, "/ui/") {
			continue
		}
		body := e.page(path, 200)
		for _, b := range all {
			label := BannerLabel(b.ID)
			if strings.HasPrefix(label, "CONDITION") {
				t.Errorf("%s: banner %s has no label of its own", name, b.ID)
			}
			if !strings.Contains(body, label) {
				t.Errorf("%s: banner %s (%s) is not on the page", name, b.ID, label)
			}
		}
	}
	// C11 is critical in the strip (it collapses behind "more critical conditions" past the third).
	c11 := func() state.Banner {
		for _, b := range all {
			if b.ID == "C11" {
				return b
			}
		}
		return state.Banner{}
	}()
	if bannerView(c11).Class != ClassCrit {
		t.Errorf("C11 critical is not critical in the strip")
	}
}

// The MISSING customer is on each page that can show a customer: the activity
// grid says it is an alarm, its Runs tab says nobody is there to run.
func TestM3T4TheMissingCustomerIsAnAlarmWhereItShows(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	blue := e.customerID("Blue Ridge Accounting")
	act := e.page("/activity", 200)
	mustContain(t, "activity", act, "Blue Ridge Accounting", "MISSING: no devices", `panel-state alarm`, `class="actgroup missing"`)
	tab := e.page("/customers/"+blue+"/runs", 200)
	mustContain(t, "customer runs tab", tab, "No devices are enrolled for an expected customer", `panel-state alarm`)
	if strings.Contains(tab, "No runs yet") {
		t.Error("a MISSING customer's Runs tab reads as a calm empty list")
	}
	mustContain(t, "banner", act, "MISSING CUSTOMER")
	// An expected customer with no devices but inside onboarding is neutral, not an alarm.
	lake := e.page("/customers/"+e.customerID("Lakeshore Pediatrics")+"/runs", 200)
	mustContain(t, "onboarding", lake, "Onboarding: no devices yet")
}

// Every outcome chip of components 3.2 on the run list, and the open run as RUNNING.
func TestM3T4S21EveryOutcomeChip(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	body := e.page("/runs?size=200", 200)
	for _, label := range []string{"SUCCESS", "PARTIAL", "FAILED", "CANCELLED", "SKIPPED", "DEFERRED", "INTERRUPTED", "ABANDONED", "RUNNING"} {
		if !strings.Contains(body, `<i aria-hidden="true"></i>`+label+`</span>`) {
			t.Errorf("the run list has no %s chip", label)
		}
	}
	mustContain(t, "classes", body, `permission <b>x23</b>`, `vss <b>x3</b>`, "attempt 2", "deferred: battery")
}

// ---- scope and roles ----

func TestM3T4ScopedAccountSeesOnlyItsCustomer(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	north, summit := e.customerID("Northwind Dental"), e.customerID("Summit Eye Care")
	var summitRun adminapi.RunListResponse
	e.apiGet("/api/v1/runs?customer="+summit+"&limit=1", &summitRun)
	e.signInAs("scoped-ro", "read_only", []string{north})
	foreign := []string{"Summit Eye Care", "Harbor Family Practice", "Pinecrest Legal", "FRONTDESK-01", "LEGAL-PC-01", "LT-BILLING-1", "Blue Ridge"}
	for name, path := range map[string]string{"runs": "/runs?size=200", "customer clause": "/runs?customer=" + summit, "activity": "/activity", "overdue": "/fleet/overdue",
		"unseen": "/fleet/unseen", "system": "/system", "own runs tab": "/customers/" + north + "/runs", "run list fragment": "/ui/runs/table"} {
		body := e.page(path, 200)
		for _, f := range foreign {
			if strings.Contains(body, f) {
				t.Errorf("%s: a scoped account sees %q", name, f)
			}
		}
	}
	own := e.page("/runs?size=200", 200)
	mustContain(t, "own runs", own, "HYGIENE-PC-2", "Northwind Dental")
	if body := e.page("/runs?customer="+summit, 200); !strings.Contains(body, "(not found)") || strings.Contains(body, `class="run"`) {
		t.Errorf("a customer outside the scope must match nothing and not say that it exists")
	}
	e.page("/customers/"+summit+"/runs", 404)
	e.page("/runs/"+summitRun.Runs[0].ID, 404)
	e.page("/fleet/overdue?customer="+summit, 404)
	e.page("/fleet/unseen?customer="+summit, 404)
	e.page("/activity?customer="+summit, 404)
	if body := e.page("/activity", 200); !strings.Contains(body, "Northwind Dental") || strings.Count(body, `<section class="actgroup`) != 1 {
		t.Errorf("the activity grid of a scoped account is exactly its own customer")
	}
}

func TestM3T4RoleGates(t *testing.T) {
	e, jobs := newM3Env(t)
	e.login()
	pages := e.m3Pages()
	e.sess, e.csrf = nil, ""
	// Anonymous: every page and the POST go to the sign-in page.
	for name, path := range pages {
		if res, _ := e.do("GET", path, nil); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
			t.Errorf("%s anonymous: %d %s", name, res.StatusCode, res.Header.Get("Location"))
		}
	}
	e.login()
	for _, role := range []string{"read_only", "operator", "admin", "owner"} {
		e.signInAs("m3-"+role, role, nil)
		for name, path := range pages {
			body := e.page(path, 200)
			if name == "system" {
				admin := role == "admin" || role == "owner"
				if admin != strings.Contains(body, `class="rowmenu act-confirm"`) {
					t.Errorf("%s sees Run now = %v", role, !admin)
				}
				if admin == strings.Contains(body, "for administrators") {
					t.Errorf("%s: the job table note is wrong", role)
				}
			}
		}
		res, _ := e.post("/ui/system/jobs/alarm-evaluator/run", url.Values{}, false)
		switch role {
		case "read_only", "operator":
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("%s may not run a job: %d", role, res.StatusCode)
			}
		default:
			if res.StatusCode != http.StatusSeeOther {
				t.Errorf("%s run now: %d", role, res.StatusCode)
			}
			jobs.waitRun(t)
		}
	}
	if got := len(jobs.runs()); got != 2 {
		t.Errorf("the runner was asked %d times (%v), want 2: admin and owner only", got, jobs.runs())
	}
}

// ---- S21 / S22 / S16 ----

func TestM3T4S21ClauseBuilderPagerAndAgreementWithTheAPI(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	var all adminapi.RunListResponse
	e.apiGet("/api/v1/runs?limit=1", &all)
	// Add condition: answered with the canonical deep link.
	res, _ := e.do("GET", "/runs?add=status:failed", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/runs?status=failed" {
		t.Fatalf("add condition: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	res, _ = e.do("GET", "/runs?status=failed&add=from:", nil)
	if loc := res.Header.Get("Location"); res.StatusCode != 303 || loc != "/runs?from=&status=failed" {
		t.Fatalf("add a date: %d %q", res.StatusCode, loc)
	}
	// The count in the sentence is the API's count for the same conditions.
	for _, q := range []string{"status=failed", "status=failed&status=partial", "status=failed&kind=backup&device=" + e.dev["FRONTDESK-01"], "kind=prune", "from=2026-10-20&to=2026-10-20"} {
		var api adminapi.RunListResponse
		since, until := "", ""
		v, _ := url.ParseQuery(q)
		api2 := url.Values{}
		for _, k := range []string{"status", "kind", "device"} {
			for _, x := range v[k] {
				api2.Add(k, x)
			}
		}
		if f := v.Get("from"); f != "" {
			since = "2026-10-20T00:00:00-05:00"
			until = "2026-10-21T00:00:00-05:00"
			api2.Set("since", since)
			api2.Set("until", until)
		}
		api2.Set("limit", "1")
		e.apiGet("/api/v1/runs?"+api2.Encode(), &api)
		body := e.page("/runs?"+q, 200)
		want := "Matches " + strconv.Itoa(api.Total) + " of " + strconv.Itoa(api.Of) + " runs."
		if !strings.Contains(body, want) {
			t.Errorf("/runs?%s: want %q on the page (API total %d)", q, want, api.Total)
		}
		if got := strings.Count(body, `class="run"`); got != min(api.Total, 50) {
			t.Errorf("/runs?%s: %d rows, API says %d", q, got, api.Total)
		}
	}
	body := e.page("/runs?status=failed&status=partial&kind=backup", 200)
	mustContain(t, "sentence", body, "outcome is failed or partial", "kind is backup", `name="status"`, `aria-label="Remove condition 1: Outcome is"`)
	// Bad values are a 400 page, not an empty list.
	for _, q := range []string{"status=melted", "kind=teleport", "customer=a/b", "from=yesterday", "tz=Mars/Base"} {
		e.page("/runs?"+q, 400)
	}
	// A range that ends before it starts says so and makes no query.
	body = e.page("/runs?from=2026-10-20&to=2026-10-01", 200)
	mustContain(t, "range", body, "The end of the range is before its start.", "No query was made")
	if strings.Contains(body, `class="run"`) {
		t.Error("rows listed for an impossible range")
	}
	// Paging: stated totals, clamped past the end, partial for htmx.
	b1 := e.page("/runs?size=25", 200)
	mustContain(t, "page 1", b1, "Showing 1-25 of "+strconv.Itoa(all.Of))
	b3 := e.page("/runs?size=25&page=3", 200)
	mustContain(t, "page 3", b3, "Showing 51-75 of "+strconv.Itoa(all.Of), `aria-current="page"`)
	if firstRun(b1) == firstRun(b3) {
		t.Error("page 3 shows the rows of page 1")
	}
	last := e.page("/runs?size=25&page=999", 200)
	mustContain(t, "clamped", last, "That page is past the end, so the last page is shown.")
	res, frag := e.do("GET", "/ui/runs/table?size=25&page=2&status=success", nil, "HX-Request", "true")
	if res.StatusCode != 200 || strings.Contains(frag, "<html") || res.Header.Get("HX-Push-Url") != "/runs?page=2&size=25&status=success" && res.Header.Get("HX-Push-Url") != "/runs?size=25&page=2&status=success" {
		t.Errorf("fragment: %d push=%q html=%v", res.StatusCode, res.Header.Get("HX-Push-Url"), strings.Contains(frag, "<html"))
	}
	// Customer tab: the customer is pinned, there is no customer column or condition.
	summit := e.customerID("Summit Eye Care")
	tab := e.page("/customers/"+summit+"/runs?status=failed", 200)
	mustContain(t, "tab", tab, `aria-current="page">Runs</a>`, "Runs of Summit Eye Care", "FRONTDESK-01")
	if strings.Contains(tab, "<th scope=\"col\">Customer</th>") || strings.Contains(tab, `<optgroup label="Customer">`) || strings.Contains(tab, "HYGIENE-PC-2") {
		t.Error("the customer tab shows another customer's rows or a customer condition")
	}
	var sum adminapi.RunListResponse
	e.apiGet("/api/v1/runs?customer="+summit+"&status=failed&limit=1", &sum)
	mustContain(t, "tab count", tab, "Matches "+strconv.Itoa(sum.Total)+" of ")
}

var firstRunRE = regexp.MustCompile(`id="run-([0-9A-Z]{26})"`)

func firstRun(body string) string {
	if m := firstRunRE.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// S21 and S22 never print a file name or a path, whatever a run carries.
func TestM3T4RunPagesAreFreeOfPaths(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	north := e.customerID("Northwind Dental")
	pages := map[string]string{"S21": "/runs?size=200", "S21 filtered": "/runs?status=failed", "S22": "/runs/" + hostileRunID, "S16": "/customers/" + north + "/runs",
		"S21 fragment": "/ui/runs/table?size=200", "S23": "/activity"}
	for name, path := range pages {
		body := e.page(path, 200)
		for _, bad := range hostileTexts {
			if strings.Contains(body, bad) {
				t.Errorf("%s: the page carries %q", name, bad)
			}
		}
		if strings.Contains(body, "secret") || strings.Contains(body, `\\`) || pathLike.MatchString(body) {
			t.Errorf("%s: something path-like on the page", name)
		}
	}
	d := e.page("/runs/"+hostileRunID, 200)
	mustContain(t, "hostile run", d, "permission", "(withheld)", "Failed files", "Total files processed", "Error classes")
	if strings.Contains(d, ">note<") || strings.Contains(d, ">path<") || strings.Contains(d, "nested") {
		t.Error("a non-numeric or nested counter reached the page")
	}
	// The view itself, fed a response a hostile agent might have got through.
	dur := int64(1000)
	hostile := adminapi.RunDetailResponse{RunRowJSON: adminapi.RunRowJSON{RunJSON: adminapi.RunJSON{ID: "x", Kind: `C:\Users\x`, Trigger: `/etc/passwd`, Origin: "agent", Outcome: "failed",
		ErrorClass: `\\srv\share`, DeferredReason: `D:\y`, StartedAt: e.now}, DurationMS: &dur, Hostname: "H", Customer: "C"},
		SnapshotID: `C:\snap`, AgentVersion: `C:\agent.exe`, Stats: map[string]float64{"failed_files": 3},
		ErrorClasses: []adminapi.ErrorClassJSON{{Class: `C:\Users\x\secret.docx`, Count: 4}, {Class: "vss", Count: 1}}}
	v := runDetailView(hostile, e.svc.fc(context.Background()))
	raw, _ := json.Marshal(v)
	for _, bad := range []string{`C:\\Users`, "passwd", "srv", "secret", `D:\\y`, "snap", "agent.exe"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("the run detail view carries %q: %s", bad, raw)
		}
	}
}

func TestM3T4S22RunDetailNumbersAndClasses(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	d := e.page("/runs/"+ulid(2), 200)
	mustContain(t, "partial run", d, "PARTIAL", "27 files could not be read", "permission", "23", "locked", "Total files processed", "2,000", "Failed files", `href="/devices/`, "Summit Eye Care", "never file names or paths")
	ok := e.page("/runs/"+ulid(1), 200)
	mustContain(t, "success run", ok, "SUCCESS", "Snapshot", "a1b2c3d4e5f60718", "Data added", "412.0 MB", "No error was recorded for this run.")
	run := e.page("/runs/"+ulid(9), 200)
	mustContain(t, "open run", run, "RUNNING", "still running")
	e.page("/runs/"+ulid(777), 404)
	e.page("/runs/not-a-run", 404)
}

// ---- S36 ----

type compRow struct{ id, label string }

var compRowRE = regexp.MustCompile(`(?s)<tr id="comp-([a-z_]+)" class="comp">\s*<td class="dev-cell"><span class="hostname">([^<]+)</span></td>\s*<td class="state-cell"><span class="st [a-z]+(?: dashed)?"><i aria-hidden="true"></i>([A-Z ]+)</span>`)

var sysChipWord = map[string]string{"ok": "OK", "warn": "WARNING", "failed": "FAILED", "not_checked": "NOT CHECKED", "not_in_build": "NOT IN THIS BUILD", "not_applicable": "NOT APPLICABLE"}

func sysRows(body string) (ids, words []string) {
	for _, m := range compRowRE.FindAllStringSubmatch(body, -1) {
		ids, words = append(ids, m[1]), append(words, m[3])
	}
	return ids, words
}

// S36 shows the API's verdict, row for row, and reads NOT CHECKED, never OK,
// where the console has no evidence.
func TestM3T4S36AgreesWithTheAPIAndNeverSaysOKWithoutEvidence(t *testing.T) {
	for _, withBanners := range []bool{false, true} {
		e, _ := newM3Env(t)
		if withBanners {
			e.svc.Fleet.Extra = func(now time.Time) []state.Banner { return allBanners(now) }
		}
		e.login()
		var api adminapi.SystemHealthResponse
		e.apiGet("/api/v1/system/health", &api)
		body := e.page("/system", 200)
		ids, words := sysRows(body)
		if len(ids) != len(api.Components) || len(ids) == 0 {
			t.Fatalf("banners=%v: %d rows on the page, %d components in the API", withBanners, len(ids), len(api.Components))
		}
		notChecked := 0
		for i, c := range api.Components {
			if ids[i] != c.ID || words[i] != sysChipWord[c.Status] {
				t.Errorf("banners=%v row %d: page %s %q, API %s %s", withBanners, i, ids[i], words[i], c.ID, c.Status)
			}
			if c.Status == "not_checked" {
				notChecked++
			}
		}
		if !withBanners {
			// This console has no probe facts and no self-check wiring: C00. Nothing may read OK.
			if notChecked == 0 {
				t.Fatal("no row is not_checked on a console whose self-checks are not wired")
			}
			for i, w := range words {
				if w == "OK" {
					t.Errorf("row %s reads OK without evidence", ids[i])
				}
			}
			mustContain(t, "C00", body, "SELF-CHECKS NOT WIRED", "NOT CHECKED")
		}
		mustContain(t, "overall", body, `id="sys-h"`, sysChipWord[api.Overall])
		overall := regexp.MustCompile(`id="sys-h" class="sys-title">Overall</h2><span class="st [a-z]+(?: dashed)?"><i aria-hidden="true"></i>([A-Z ]+)<`).FindStringSubmatch(body)
		if overall == nil || overall[1] != sysChipWord[api.Overall] {
			t.Errorf("banners=%v: overall %v, API %s", withBanners, overall, api.Overall)
		}
		for _, b := range api.Banners {
			if !strings.Contains(body, b.Text) {
				t.Errorf("banners=%v: banner %s text is not listed", withBanners, b.ID)
			}
		}
		mustContain(t, "versions", body, "Database schema", "Maintenance worker", "restic unknown")
	}
}

// When the console cannot read its own health, no component is shown, so none
// can be shown as OK; the page says it is an alarm.
func TestM3T4S36UnreadableHealthIsAnAlarm(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	// A private API that refuses the health read.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/system/health", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	mux.HandleFunc("/api/v1/system/jobs", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	e.svc.m3.mux = mux
	body := e.page("/system", 200)
	mustContain(t, "alarm", body, `panel-state alarm`, "Could not load system health", "No component is shown as OK", "Could not load the job table")
	if ids, _ := sysRows(body); len(ids) != 0 {
		t.Errorf("rows %v shown although health could not be read", ids)
	}
}

// An unreadable run list is an alarm panel with no rows, never an empty list.
func TestM3T4S21UnreadableRunListIsAnAlarm(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	e.svc.m3.mux = mux
	for name, path := range map[string]string{"runs": "/runs", "tab": "/customers/" + e.customerID("Summit Eye Care") + "/runs", "overdue": "/fleet/overdue", "unseen": "/fleet/unseen", "activity": "/activity"} {
		body := e.page(path, 200)
		if !strings.Contains(body, "panel-state alarm") || !strings.Contains(body, "This is an alarm, not an empty list") {
			t.Errorf("%s: an unreadable answer is not an alarm", name)
		}
		if strings.Contains(body, `class="run"`) || strings.Contains(body, "Nothing overdue") || strings.Contains(body, "Nothing unseen") {
			t.Errorf("%s: rows or a calm empty state shown for an unreadable answer", name)
		}
	}
	e.page("/runs/"+ulid(1), 503)
}

// ---- Run now ----

func TestM3T4RunNowAuditsOnceAndARefusedAuditStartsNothing(t *testing.T) {
	e, jobs := newM3Env(t)
	e.login()
	body := e.page("/system", 200)
	mustContain(t, "job table", body, `id="job-alarm-evaluator"`, `Run <b class="mono">alarm-evaluator</b> now`, `action="/ui/system/jobs/alarm-evaluator/run"`, "NEVER RUN", "outside its schedule")

	// Accepted: one audit row, the runner asked once, a redirect that says so.
	before := e.auditTotal()
	res, _ := e.post("/ui/system/jobs/alarm-evaluator/run", url.Values{}, false)
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/system?flash=alarm-evaluator~") || !strings.HasSuffix(loc, "#jobs") {
		t.Fatalf("run now: %d %q", res.StatusCode, loc)
	}
	if n := jobs.waitRun(t); n != "alarm-evaluator" {
		t.Fatalf("the runner ran %q", n)
	}
	rows := e.auditRows("system.job.run", "alarm-evaluator")
	if len(rows) != 1 || e.auditTotal()-before != 1 || rows[0].Result != "ok" || rows[0].ObjectType != "job" {
		t.Fatalf("audit rows: %+v (total moved %d)", rows, e.auditTotal()-before)
	}
	mustContain(t, "flash", e.page(strings.TrimSuffix(loc, "#jobs"), 200), "Run accepted.", "One audit entry was written")

	// A refused audit row: the page says so with an error status and nothing starts.
	e.mutRefuseAudits(true)
	before = e.auditTotal()
	res, page := e.post("/ui/system/jobs/session-expire/run", url.Values{}, false)
	e.mutRefuseAudits(false)
	if res.StatusCode < 500 || !strings.Contains(page, "Refused (NOT AUDITED)") || !strings.Contains(page, "nothing was started") {
		t.Fatalf("a refused audit: %d %.300s", res.StatusCode, page)
	}
	select {
	case n := <-jobs.done:
		t.Fatalf("the runner started %q although its audit row was refused", n)
	case <-time.After(300 * time.Millisecond):
	}
	if e.auditTotal() != before || len(jobs.runs()) != 1 {
		t.Errorf("audit rows %d -> %d, runs %v", before, e.auditTotal(), jobs.runs())
	}

	// An unknown job, a bad name, a missing CSRF token: nothing audited, nothing started.
	res, page = e.post("/ui/system/jobs/no-such-job/run", url.Values{}, false)
	if res.StatusCode != 404 || !strings.Contains(page, "is not a job this console runs") {
		t.Errorf("unknown job: %d", res.StatusCode)
	}
	if res, _ := e.post("/ui/system/jobs/Bad%20Name/run", url.Values{}, false); res.StatusCode != 404 {
		t.Errorf("bad name: %d", res.StatusCode)
	}
	if res, _ := e.do("POST", "/ui/system/jobs/alarm-evaluator/run", url.Values{}, "Origin", e.srv.URL); res.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", res.StatusCode)
	}
	if e.auditTotal() != before || len(jobs.runs()) != 1 {
		t.Errorf("a refused request left a trace: audit %d, runs %v", e.auditTotal()-before, jobs.runs())
	}
}

// Without a job runner "Run now" is not offered and says why; a forged POST is refused.
func TestM3T4RunNowWithoutARunnerIsRefusedAndSaysSo(t *testing.T) {
	e := newEnv(t) // no Jobs on the UI service, and not fixtures mode (fixtures mode brings its own fictional jobs)
	e.svc.Fixtures = false
	e.login()
	body := e.page("/system", 200)
	if strings.Contains(body, "Run now") {
		t.Error("Run now is offered with no job runner")
	}
	before := e.auditTotal()
	res, page := e.post("/ui/system/jobs/alarm-evaluator/run", url.Values{}, false)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(page, "cannot run jobs now") {
		t.Errorf("no runner: %d", res.StatusCode)
	}
	if e.auditTotal() != before {
		t.Error("an audit row was written for a run that could not start")
	}
}

// ---- B04 / B05 ----

var devRowRE = regexp.MustCompile(`<tr id="dev-([0-9A-Za-z]+)" class="dev">`)

func TestM3T4OverdueAgreesWithTheAPI(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	var api adminapi.FleetOverdueResponse
	e.apiGet("/api/v1/fleet/overdue", &api)
	if api.Total == 0 {
		t.Fatal("the fixtures have nothing overdue")
	}
	body := e.page("/fleet/overdue", 200)
	got := map[string]bool{}
	for _, m := range devRowRE.FindAllStringSubmatch(body, -1) {
		got[m[1]] = true
	}
	if len(got) != api.Total {
		t.Errorf("%d rows on the page, API total %d", len(got), api.Total)
	}
	for _, d := range api.Devices {
		if !got[d.ID] {
			t.Errorf("%s (%s) is overdue in the API and not on the page", d.Hostname, d.State)
		}
		for _, chip := range d.Chips {
			if !strings.Contains(body, stateLabel(state.State(chip))) {
				t.Errorf("reason chip %s is not on the page", chip)
			}
		}
	}
	mustContain(t, "page", body, "Showing 1-"+strconv.Itoa(api.Total)+" of "+strconv.Itoa(api.Total), `class="ghead"`, "Why it is overdue")
	// A customer filter narrows to the API's answer for that customer.
	summit := e.customerID("Summit Eye Care")
	var one adminapi.FleetOverdueResponse
	e.apiGet("/api/v1/fleet/overdue?customer="+summit, &one)
	if n := len(devRowRE.FindAllString(e.page("/fleet/overdue?customer="+summit, 200), -1)); n != one.Total {
		t.Errorf("customer filter: %d rows, API %d", n, one.Total)
	}
	// Nothing overdue is a calm answer only with its evidence.
	clear := e.page("/fleet/overdue?customer="+e.customerID("Pinecrest Legal"), 200)
	mustContain(t, "clear", clear, "Nothing overdue", "3 devices checked at 1:42:10 PM (America/Chicago)")
	if strings.Contains(clear, "panel-state alarm") || strings.Contains(clear, "panel-state partial") {
		t.Error("a checked, clear customer is not neutral")
	}
}

func TestM3T4EmptyListsAreAlarmsWhenThereIsNothingToCheckOrOnlyPartData(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	e.signInAs("empty-scope", "read_only", []string{"no-such-customer"})
	for _, p := range []string{"/fleet/overdue", "/fleet/unseen"} {
		body := e.page(p, 200)
		if !strings.Contains(body, "panel-state alarm") || !strings.Contains(body, "No devices to check") {
			t.Errorf("%s: an account that sees no device must get an alarm: %.400s", p, body)
		}
		if strings.Contains(body, "Nothing overdue") || strings.Contains(body, "Nothing unseen") {
			t.Errorf("%s: calm text for an account with nothing to check", p)
		}
	}
	// The classifier, on every kind of evidence.
	c := fmtCtx{Now: e.now, Loc: e.loc, Zone: "America/Chicago"}
	sum := func(total, in, enrolled int, complete, first bool) listEvidence {
		var s adminapi.FleetSummaryResponse
		s.Devices.Total, s.Freshness.InView, s.Freshness.Enrolled, s.Complete, s.FirstRun, s.ComputedAt = total, in, enrolled, complete, first, e.now
		return listEvidence{Sum: s}
	}
	for _, tc := range []struct {
		name     string
		ev       listEvidence
		complete bool
		cust     string
		kind     string
		title    string
	}{
		{"checked and clear", sum(12, 12, 12, true, false), true, "", "neutral", "Nothing overdue"},
		{"partial load", sum(12, 9, 12, true, false), true, "", "partial", "Not every device could be checked"},
		{"incomplete facts", sum(12, 12, 12, false, false), true, "", "partial", "Not every device could be checked"},
		{"the list itself was incomplete", sum(12, 12, 12, true, false), false, "", "partial", "Not every device could be checked"},
		{"no devices after enrollment", sum(0, 0, 0, true, false), true, "", "alarm", "No devices to check"},
		{"a customer with no devices", sum(0, 0, 0, true, true), true, "some-customer", "alarm", "No devices to check"},
		{"first run", sum(0, 0, 0, true, true), true, "", "neutral", "No devices yet"},
		{"summary unreadable", listEvidence{Err: &apiError{Status: 500}}, true, "", "alarm", "Could not confirm that nothing is overdue"},
	} {
		got := e.svc.clearOrAlarm("overdue", "Nothing overdue", tc.ev, tc.complete, tc.cust, c)
		if got.Kind != tc.kind || got.Title != tc.title {
			t.Errorf("%s: %s %q, want %s %q", tc.name, got.Kind, got.Title, tc.kind, tc.title)
		}
	}
}

func TestM3T4UnseenCollapsesACustomersSilenceToOneRow(t *testing.T) {
	e, _ := newM3Env(t)
	e.login()
	// Before: one unseen device, listed on its own.
	one := e.page("/fleet/unseen", 200)
	mustContain(t, "device row", one, "LAB-RETINA-02", "UNSEEN", unseenDeviceHint)
	if strings.Contains(one, "CUSTOMER-WIDE SILENCE") {
		t.Error("silence reported for one unseen device")
	}
	e.m3Silence()
	var api adminapi.FleetUnseenResponse
	e.apiGet("/api/v1/fleet/unseen", &api)
	silent := 0
	for _, r := range api.Rows {
		if r.Kind == "customer_silence" {
			silent++
		}
	}
	if silent != 1 || api.Devices < 8 {
		t.Fatalf("the API: %d silence rows for %d unseen devices", silent, api.Devices)
	}
	body := e.page("/fleet/unseen", 200)
	if got := strings.Count(body, `class="silence-row"`); got != 1 {
		t.Fatalf("%d silence rows, want one", got)
	}
	mustContain(t, "silence", body, "CUSTOMER-WIDE SILENCE", "Summit Eye Care", "devices silent", unseenSilenceHint, `Show the 10 devices`)
	if got := len(devRowRE.FindAllString(body, -1)); got != api.Total-1 {
		t.Errorf("%d device rows beside the silence row, API has %d other rows", got, api.Total-1)
	}
	// A collapsed device is not listed again on its own.
	if strings.Contains(body, "FRONTDESK-01") || strings.Contains(body, "TECH-BENCH-07") {
		t.Error("a device covered by the customer's silence row is listed individually")
	}
	mustContain(t, "summary", body, plural(api.Devices, "device", "devices")+" in "+plural(api.Total, "row", "rows"))
	// Clear and checked.
	clear := e.page("/fleet/unseen?customer="+e.customerID("Northwind Dental"), 200)
	mustContain(t, "clear", clear, "Nothing unseen", "3 devices checked at 1:42:10 PM")
}

// ---- S23 ----

var (
	dayCellRE = regexp.MustCompile(`class="dayblk k-([a-z]+)`)
	rowRE     = regexp.MustCompile(`(?s)<tr id="act-dev-([0-9A-Za-z]+)">(.*?)</tr>`)
)

func TestM3T4ActivityGridMatchesTheAPI(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	var api adminapi.FleetTenDayResponse
	e.apiGet("/api/v1/fleet/ten-day?devices=true", &api)
	body := e.page("/activity", 200)
	rows := rowRE.FindAllStringSubmatch(body, -1)
	apiDevices := map[string]adminapi.TenDayDeviceJSON{}
	for _, c := range api.Customers {
		for _, d := range c.Devices {
			if d.State != "retired" {
				apiDevices[d.ID] = d
			}
		}
	}
	if len(rows) != len(apiDevices) || len(rows) == 0 {
		t.Fatalf("%d device rows on the page, %d in the API", len(rows), len(apiDevices))
	}
	for _, r := range rows {
		d := apiDevices[r[1]]
		kinds := dayCellRE.FindAllStringSubmatch(r[2], -1)
		if len(kinds) != 10 || len(d.Days) != 10 {
			t.Fatalf("%s: %d blocks, %d days", d.Hostname, len(kinds), len(d.Days))
		}
		for i, k := range kinds {
			want := d.Days[i].Kind
			if want != "success" && want != "failed" && want != "mixed" && want != "missed" {
				want = "none"
			}
			if k[1] != want {
				t.Errorf("%s day %d: block %s, API %s", d.Hostname, i, k[1], want)
			}
		}
		// the last block is today
		if !strings.Contains(r[2][strings.LastIndex(r[2], "<td>"):], "today") {
			t.Errorf("%s: the last block is not today's", d.Hostname)
		}
	}
	// A block's number is the jobs of the day, and its link opens exactly those runs.
	fd := apiDevices[e.dev["FRONTDESK-01"]]
	today := fd.Days[9]
	if today.Success+today.Failed == 0 {
		t.Fatal("FRONTDESK-01 has no runs today in the fixtures")
	}
	link := "/runs?device=" + e.dev["FRONTDESK-01"] + "&amp;from=" + today.Date + "&amp;to=" + today.Date
	if !strings.Contains(body, `href="`+link+`"`) {
		t.Fatalf("no link %s in the grid", link)
	}
	list := e.page(strings.ReplaceAll(link, "&amp;", "&"), 200)
	mustContain(t, "day link", list, "Matches "+strconv.Itoa(today.Success+today.Failed)+" of ")
	// Retired devices are not part of the grid (the ten-day API does not carry them).
	if strings.Contains(body, `id="act-dev-`+e.dev["OLD-RECEPTION-PC"]+`"`) || strings.Contains(body, `name="retired"`) {
		t.Error("a retired device or a control for them is in the grid")
	}
	// The problems filter keeps the devices with a failed, mixed or missed day.
	prob := e.page("/activity?problems=1", 200)
	mustContain(t, "problems", prob, "FRONTDESK-01")
	if strings.Contains(prob, "OPS-ND-01") {
		t.Error("a device with nothing wrong is in the problems view")
	}
	// Customer page of the grid: stated totals, complete groups.
	mustContain(t, "pager", body, "Showing 1-27 of 27 · 6 customers on this page")
}

// ---- layout, on the rendered page ----

var (
	tableRE   = regexp.MustCompile(`<table\b`)
	wrapRE    = regexp.MustCompile(`<div class="table-wrap[^"]*" role="region" aria-label="[^"]+" tabindex="0"[^>]*>`)
	ctlRE     = regexp.MustCompile(`<(select|input)\b([^>]*)>`)
	headRowRE = regexp.MustCompile(`(?s)<thead>\s*<tr><th scope="col" class="devcol">Device</th>(.*?)</tr></thead>`)
)

// Layout assertions on the rendered markup: every table scrolls inside its own
// focusable region (so the page never scrolls sideways), every control has a
// name, the day grid has exactly ten day columns of ten blocks, and nothing sets
// a pixel width inline.
func TestM3T4RenderedLayout(t *testing.T) {
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	for name, path := range e.m3Pages() {
		if strings.Contains(path, "/ui/") {
			continue
		}
		body := e.page(path, 200)
		if n := strings.Count(body, "<main "); n != 1 {
			t.Errorf("%s: %d <main> elements", name, n)
		}
		if tables, wraps := len(tableRE.FindAllString(body, -1)), len(wrapRE.FindAllString(body, -1)); tables != wraps {
			t.Errorf("%s: %d tables in %d scroll regions", name, tables, wraps)
		}
		if strings.Contains(body, "<table") && !strings.Contains(body, "<caption>") {
			t.Errorf("%s: a table without a caption", name)
		}
		for _, m := range ctlRE.FindAllStringSubmatch(body, -1) {
			attrs := m[2]
			if strings.Contains(attrs, `type="hidden"`) || strings.Contains(attrs, `type="checkbox"`) || strings.Contains(attrs, `name="csrf_token"`) {
				continue
			}
			if id := regexp.MustCompile(`\bid="([^"]+)"`).FindStringSubmatch(attrs); !strings.Contains(attrs, "aria-label=") && (id == nil || !strings.Contains(body, `for="`+id[1]+`"`)) {
				t.Errorf("%s: a control with no label: <%s%s>", name, m[1], attrs)
			}
		}
		main := body[strings.Index(body, "<main "):strings.Index(body, "</main>")]
		for _, m := range regexp.MustCompile(`style="([^"]*)"`).FindAllStringSubmatch(main, -1) {
			if !regexp.MustCompile(`^width: \d+%$`).MatchString(m[1]) && !strings.HasPrefix(m[1], "height: ") {
				t.Errorf("%s: inline style %q", name, m[1])
			}
		}
		if n := strings.Count(body, "<h1"); n != 1 {
			t.Errorf("%s: %d h1", name, n)
		}
	}
	act := e.page("/activity", 200)
	for _, h := range headRowRE.FindAllStringSubmatch(act, -1) {
		if n := strings.Count(h[1], `class="daycol`); n != 10 {
			t.Errorf("a day grid has %d day columns", n)
		}
	}
	if n := len(headRowRE.FindAllString(act, -1)); n < 4 {
		t.Errorf("%d grids", n)
	}
	for _, r := range regexp.MustCompile(`(?s)<tr id="act-dev-[^"]+">(.*?)</tr>`).FindAllStringSubmatch(act, -1) {
		if n := strings.Count(r[1], "<td>"); n != 10 {
			t.Errorf("a device row has %d day cells", n)
		}
	}
	// Every kind of block is in the key, with its own shape class.
	for _, k := range []string{"success", "failed", "mixed", "missed", "none"} {
		if !strings.Contains(act, `class="dayblk k-`+k+`"`) {
			t.Errorf("no %s block in the key", k)
		}
	}
	// The sidebar offers all seven pages.
	for _, href := range []string{"/activity", "/runs", "/fleet/overdue", "/fleet/unseen", "/system"} {
		if !strings.Contains(act, `class="navlink" href="`+href+`"`) {
			t.Errorf("no navigation entry for %s", href)
		}
	}
	// A fixture customer's page has the Runs tab beside Overview and Devices.
	summit := e.customerID("Summit Eye Care")
	if p := e.page("/customers/"+summit, 200); !strings.Contains(p, `href="/customers/`+summit+`/runs">Runs</a>`) {
		t.Error("the customer page has no Runs tab")
	}
}

// ---- units ----

func TestM3T4ParseRunQuery(t *testing.T) {
	q, bad := parseRunQuery(url.Values{"status": {"failed", "failed", "partial"}, "kind": {"backup"}, "from": {"2026-10-01"}, "to": {""}, "size": {"25"}, "page": {"2"}, "add": {"customer:abc"}})
	if len(bad) != 0 {
		t.Fatalf("bad: %v", bad)
	}
	if got := q.values().Encode(); got != "customer=abc&from=2026-10-01&kind=backup&page=2&size=25&status=failed&status=partial&to=" {
		t.Errorf("canonical: %s", got)
	}
	if !hasClause(q, "to") || q.date("to") != "" {
		t.Error("a date clause with no value is kept so the row stays on screen")
	}
	// 50 values per field, no more
	v := url.Values{}
	for i := 0; i < 60; i++ {
		v.Add("device", "d"+strconv.Itoa(i))
	}
	if _, bad := parseRunQuery(v); len(bad) == 0 {
		t.Error("60 device values were accepted")
	}
	// The range is [from 00:00, to + 1 day) in the chosen zone.
	loc, _ := time.LoadLocation("America/Chicago")
	q, _ = parseRunQuery(url.Values{"from": {"2026-03-07"}, "to": {"2026-03-08"}, "tz": {"Asia/Tokyo"}})
	av := q.apiValues(loc)
	if av.Get("since") != "2026-03-06T15:00:00Z" || av.Get("until") != "2026-03-08T15:00:00Z" {
		t.Errorf("range in Tokyo: %v", av)
	}
	av = (runQuery{Clauses: []runClause{{"from", "2026-03-08"}, {"to", "2026-03-08"}}}).apiValues(loc)
	if av.Get("since") != "2026-03-08T06:00:00Z" || av.Get("until") != "2026-03-09T05:00:00Z" { // the DST day has 23 hours
		t.Errorf("DST day: %v", av)
	}
}

// What an agent supplies is allow-listed, not merely scrubbed: a bare file name is
// "(withheld)" wherever a known set or a version shape applies.
func TestM3T4AgentTextIsAllowListed(t *testing.T) {
	names := []string{"secret-payroll.xlsx", "Patient_Chart.docx", "John Smith MRI.pdf", "payroll", `C:\x`, "/etc/x", `\\srv\x`, "", "file.docx\x00", "x/y"}
	for _, bad := range names {
		for fn, f := range map[string]func(string) string{"class": safeClass, "deferred": safeDeferred, "kind": safeKind, "trigger": safeTrigger, "origin": safeOrigin, "os": safeOS, "commit": safeCommit, "date": safeBuildDate} {
			if got := f(bad); got != withheld {
				t.Errorf("%s(%q) = %q", fn, bad, got)
			}
		}
	}
	for _, bad := range []string{"secret-payroll.xlsx", "Patient_Chart.docx", "John Smith MRI.pdf", `C:\x`, "/etc/x", "a b", ""} {
		if got := safeVersion(bad); got != withheld {
			t.Errorf("safeVersion(%q) = %q", bad, got)
		}
	}
	for _, ok := range []string{"permission", "vss", "locked", "repo_missing", "guard_refused"} {
		if safeClass(ok) != ok {
			t.Errorf("class %q withheld", ok)
		}
	}
	for _, bad := range []string{"io", "io_error", "payroll"} { // a name that is not in the restic vocabulary
		if safeClass(bad) != withheld {
			t.Errorf("class %q passed", bad)
		}
	}
	for _, ok := range []string{"battery", "gateway", "lock", "metered", "window", "conditions", "fleet_cap"} {
		if safeDeferred(ok) != ok {
			t.Errorf("deferred %q withheld", ok)
		}
	}
	for _, ok := range []string{"1.4.2", "dev", "v1.2.0-3-gabc1234-dirty", "0.19.1", "unknown"} {
		if safeVersion(ok) != ok {
			t.Errorf("version %q withheld", ok)
		}
	}
	for _, ok := range []string{"Windows 11 Pro", "Windows Server 2022", "Windows Server 2012 R2 Standard", "10.0.22631", "windows", "linux"} {
		if safeOS(ok) != ok {
			t.Errorf("os %q withheld", ok)
		}
	}
	// M3-HARDEN: real OS names pass; a file name dressed as one does not.
	for _, ok := range []string{"Microsoft Windows 10 Pro 22H2", "Windows 11 Pro 24H2", "Ubuntu 24.04", "Ubuntu 24.04 LTS", "Debian GNU/Linux 12", "macOS 14.5", "Windows 8.1", "Windows 10 Enterprise LTSC", "Red Hat Enterprise Linux 9.4"} {
		if safeOS(ok) != ok {
			t.Errorf("os %q withheld", ok)
		}
	}
	for _, bad := range []string{"Windows 11 Pro.docx", "Ubuntu 24.04.xlsx", "Ubuntu payroll", "Windows payroll", "Windows 10 Patient Chart", "Debian/etc/passwd", "macOS 14.5 John Smith", "Windows 24H2.pdf", "/etc/Ubuntu 24.04"} {
		if got := safeOS(bad); got != withheld {
			t.Errorf("safeOS(%q) = %q", bad, got)
		}
	}
	if safeCommit("abc1234") != "abc1234" || safeBuildDate("2026-11-05T10:20:30Z") != "2026-11-05T10:20:30Z" || safeCommit("unknown") != "unknown" || safeBuildDate("unknown") != "unknown" {
		t.Error("the build stamp is withheld")
	}
	if got := classCounts(map[string]int{"permission": 2, `C:\u\f.txt`: 5, "secret-payroll.xlsx": 3, "vss": 5}); len(got) != 3 || got[0].Class != "vss" || got[2].Class != withheld || got[2].N != 8 {
		t.Errorf("class counts: %+v", got)
	}
}

func TestM3T4PageInfoAndSystemStatusChips(t *testing.T) {
	if i := m3PageInfo(120, 3, 50); i.From != 101 || i.To != 120 || i.Pages != 3 || i.Clamped {
		t.Errorf("%+v", i)
	}
	if i := m3PageInfo(120, 9, 50); !i.Clamped || i.Page != 3 {
		t.Errorf("%+v", i)
	}
	if i := m3PageInfo(0, 1, 50); i.From != 0 || i.To != 0 || i.Pages != 1 {
		t.Errorf("%+v", i)
	}
	// not_checked is amber and dashed, never the OK green; an unknown status is a failure.
	if c := sysStatusChip("x", "not_checked"); c.Class != ClassWarn || !c.Dashed || c.Label != "NOT CHECKED" {
		t.Errorf("not_checked chip: %+v", c)
	}
	if c := sysStatusChip("x", "something-new"); c.Class != ClassCrit {
		t.Errorf("unknown status chip: %+v", c)
	}
	if c := sysStatusChip("x", "ok"); c.Class != ClassOK {
		t.Errorf("ok chip: %+v", c)
	}
}
