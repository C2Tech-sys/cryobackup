package ui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

const testPassword = "ui-test-password-1"

type env struct {
	t    *testing.T
	st   *store.Store
	svc  *Service
	srv  *httptest.Server
	be   *fixtures.Backend
	hub  *Hub
	now  time.Time
	loc  *time.Location
	cli  *http.Client
	sess []*http.Cookie
	csrf string
	dev  map[string]string // hostname -> id
	mfa  *uiMFA            // account_test.go: the MFA step of e.login
	// syncDown stops the login sync's job recording runs: a retire then answers pending (M3-T6).
	syncDown bool
}

type muxReg struct{ m *http.ServeMux }

func (r muxReg) Handle(p string, h http.Handler) { r.m.Handle(p, h) }

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, now: fixtures.Now(), loc: loc, dev: map[string]string{}}
	clock := func() time.Time { return e.now }
	st, err := store.OpenMemory(ctx, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hash, _ := auth.HashPassword(testPassword)
	if err := store.LoadFixtures(ctx, st, e.now, store.WithAdminPasswordHash(hash)); err != nil {
		t.Fatal(err)
	}
	e.st = st
	mk, _ := escrow.NewMasterKey(bytes.Repeat([]byte{4}, 32))
	e.be = &fixtures.Backend{}
	rest := &restores.Service{Store: st, Backend: e.be, Master: mk, ArtifactsDir: t.TempDir(), Clock: clock}
	am := auth.NewManager(st, 12*time.Hour, 7*24*time.Hour, 1000, 5, 15*time.Minute, clock)
	e.mfa = newUIMFA(am, mk)
	maintain := func() health.MaintainStatus {
		return health.MaintainStatus{Known: true, Reachable: true, LastOK: e.now}
	}
	fleet := &health.Evaluator{Store: st, Clock: clock, MasterLoaded: mk.Loaded, Maintain: maintain}
	e.hub = NewHub(30*time.Second, nil)
	svc, err := New(&Service{Auth: am, Store: st, Fleet: fleet, Restores: rest, Backend: e.be, Hub: e.hub, Clock: clock, Loc: loc, Zone: "America/Chicago",
		Refresh: 30, Fixtures: true, MasterLoaded: mk.Loaded, Maintain: maintain,
		SyncGateway: func() { // a working htpasswd-sync job: M3-T6's retire waits for its run
			if !e.syncDown {
				_ = st.RecordJobRun(ctx, store.JobHtpasswdSync, store.JobRun{Status: "ok", StartedAt: e.now, FinishedAt: e.now})
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	mux := http.NewServeMux()
	svc.Register(muxReg{mux})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.cli = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	l, _ := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 100}})
	for _, d := range l.Items {
		e.dev[d.Hostname] = d.ID
	}
	for _, d := range []string{"DR-OFFICE-PC", "LEGAL-PC-01"} {
		if _, _, err := st.AgentReplaceSnapshots(ctx, e.dev[d], []store.Snapshot{{DeviceID: e.dev[d], SnapshotID: fixtures.FixtureSnapshotID, TakenAt: e.now.Add(-time.Hour), Hostname: d, Paths: []string{`C:\Users`}}}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) do(method, path string, form url.Values, hdr ...string) (*http.Response, string) {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range e.sess {
		r.AddCookie(c)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := e.cli.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func (e *env) login() {
	e.t.Helper()
	res, body := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {testPassword}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		e.t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	e.sess = res.Cookies()
	for _, c := range e.sess {
		if c.Name == auth.CSRFCookie {
			e.csrf = c.Value
		}
	}
	if e.csrf == "" {
		e.t.Fatal("no csrf cookie")
	}
	e.completeMFA("admin")
}

func (e *env) post(path string, form url.Values, hx bool) (*http.Response, string) {
	form.Set("csrf_token", e.csrf)
	hdr := []string{"Origin", e.srv.URL}
	if hx {
		hdr = append(hdr, "HX-Request", "true")
	}
	return e.do("POST", path, form, hdr...)
}

func mustContain(t *testing.T, name, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s: missing marker %q", name, w)
		}
	}
}

var pathLike = regexp.MustCompile(`[A-Za-z]:\\|\\\\[A-Za-z]|/(?:Users|home|var|etc|opt)/[A-Za-z]|%[A-Z]+%`)

func TestLoginPageAndFlow(t *testing.T) {
	e := newEnv(t)
	res, body := e.do("GET", "/login", nil)
	if res.StatusCode != 200 {
		t.Fatalf("%d", res.StatusCode)
	}
	mustContain(t, "login", body, `t-hero-wordmark`, `name="username"`, `name="password"`, `autocomplete="current-password"`, "Fixtures mode", `<h1 class="sr-only">Sign in</h1>`)
	if strings.Contains(body, "https://") || strings.Contains(body, "http://") {
		t.Fatal("the login page must make no absolute URL reference")
	}
	res, body = e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	if res.StatusCode != 401 || !strings.Contains(body, "not recognised") || !strings.Contains(body, `aria-describedby="signin-err"`) {
		t.Fatalf("bad password: %d", res.StatusCode)
	}
	res, _ = e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {testPassword}}, "Origin", "https://evil.example")
	if res.StatusCode != 403 {
		t.Fatalf("cross-origin login: %d", res.StatusCode)
	}
	res, _ = e.do("GET", "/", nil)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous board: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	e.login()
	if res, _ := e.do("GET", "/login", nil); res.StatusCode != 303 {
		t.Fatalf("login while signed in: %d", res.StatusCode)
	}
	res, _ = e.post("/logout", url.Values{}, false)
	if res.StatusCode != 303 {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	if res, _ := e.do("GET", "/", nil); res.StatusCode != 303 {
		t.Fatal("session must be gone after logout")
	}
}

func TestFixturesPagesRenderWithTheirMarkers(t *testing.T) {
	e := newEnv(t)
	e.login()
	res, board := e.do("GET", "/", nil)
	if res.StatusCode != 200 {
		t.Fatalf("board: %d", res.StatusCode)
	}
	mustContain(t, "board", board, `id="banners"`, `id="freshness"`, `<span class="word" aria-live="polite">Live</span>`, `<span class="t" data-at="1:42:10 PM">1:42:10 PM</span>`, `<b>25</b> of <b>25</b> agents`, `id="kpis"`, `Within SLA`,
		"Summit Eye Care", "Harbor Family Practice", "Blue Ridge Accounting", `class="st crit"`, "MISSING", "FAILED", "UNSEEN", "PENDING FIRST", "PARKED", "RUNNING",
		`LAB-RETINA-02`, `FRONTDESK-01`, `role="img" aria-label="Backup age ring`, `class="strip"`, `data-live`, `data-events="/ui/events"`,
		`t-page-title`, `Skip to content`, `aria-current="page"`, "exceptions first", "MISSING CUSTOMER", `/static/app.css?v=`)
	if strings.Contains(board, "ZgotmplZ") {
		t.Fatal("html/template rejected a value somewhere (ZgotmplZ)")
	}
	// Exception-first: Summit (alarm) and the MISSING customer come before healthy Northwind.
	// (in the table: the saved-views control above it lists the customers by name, for the share dialog)
	tbl := board[strings.Index(board, "<table"):]
	if strings.Index(tbl, "Blue Ridge Accounting") > strings.Index(tbl, "Northwind Dental") || strings.Index(tbl, "Summit Eye Care") > strings.Index(tbl, "Northwind Dental") {
		t.Fatal("customer groups are not exception-first")
	}
	devPage := func(host string) string {
		res, body := e.do("GET", "/devices/"+e.dev[host], nil)
		if res.StatusCode != 200 {
			t.Fatalf("device %s: %d", host, res.StatusCode)
		}
		return body
	}
	fd := devPage("FRONTDESK-01")
	mustContain(t, "device", fd, "FRONTDESK-01", "FAILED", "Last ten runs", "Last 10 days", "Facts", `class="hist"`, "Key custody", "Restore files", `data-events="/ui/events?device=`, "consecutive failures")
	run := devPage("DR-OFFICE-PC")
	mustContain(t, "running device", run, `role="progressbar"`, `aria-valuenow="62"`, "23,418 / 37,902 files")
	if res, body := e.do("GET", "/devices/"+e.dev["FRONTDESK-01"]+"/restore", nil); res.StatusCode != 200 {
		t.Fatalf("restore page: %d", res.StatusCode)
	} else if !strings.Contains(body, "Customer-held") && !strings.Contains(body, "Snapshot") {
		t.Fatalf("restore page markers: %s", body[:min(len(body), 400)])
	}
	res, body := e.do("GET", "/devices/"+e.dev["DR-OFFICE-PC"]+"/restore", nil)
	if res.StatusCode != 200 {
		t.Fatalf("restore: %d", res.StatusCode)
	}
	mustContain(t, "restore", body, "Which file do you need back from DR-OFFICE-PC?", "Snapshot root", `name="snapshot"`, `value="/C"`, "Download as a zip", `aria-label="Progress"`)
	// ADR 0008 / M1-HIPAA2 item 4: the page says the zip streams and nothing is
	// stored on the server; nothing is "prepared" or kept for 24 hours.
	mustContain(t, "restore copy", body, "The zip streams from the snapshot straight to your browser.", "Nothing is stored on the server.",
		"The download link works once.", `value="submit">Continue</button>`)
	for _, stale := range []string{"Prepare zip", "prepares a zip", "deleted after download", "after 24 hours"} {
		if strings.Contains(body, stale) {
			t.Errorf("the restore page still says %q", stale)
		}
	}
	held := devPage("LEGAL-PC-01")
	mustContain(t, "customer-held device", held, "Customer-held key: no technician restore.", `aria-disabled="true"`)
	res, body = e.do("GET", "/devices/"+e.dev["LEGAL-PC-01"]+"/restore", nil)
	mustContain(t, "customer-held restore", body, "Customer-held key: no technician restore")
	if strings.Contains(body, `name="path"`) {
		t.Fatal("no browsing for a customer-held key")
	}
	res, design := e.do("GET", "/design", nil)
	if res.StatusCode != 200 {
		t.Fatalf("design: %d", res.StatusCode)
	}
	mustContain(t, "design", design, "Built-in checks", "Token coverage", "Orbitron audit", "Contrast (WCAG AA)", "C01", "C16", "GATEWAY UNREACHABLE", "RELEASE MANIFEST REJECTED",
		`data-toggle="stale"`, `data-toggle="drop"`, `data-toggle="hit"`, "NEVER BACKED UP", "RETENTION FAILED", "UNKNOWN OUTCOME", "No devices are enrolled")
	for _, r := range Catalogue() {
		mustContain(t, "design catalogue "+string(r.State), design, ">"+string(r.State)+"<")
	}
	for _, sec := range designSections {
		if res, _ := e.do("GET", "/design/"+sec.Key, nil); res.StatusCode != 200 {
			t.Errorf("/design/%s: %d", sec.Key, res.StatusCode)
		}
	}
	if res, _ := e.do("GET", "/design/nope", nil); res.StatusCode != 404 {
		t.Fatalf("unknown component: %d", res.StatusCode)
	}
	// No page shows a path.
	for name, b := range map[string]string{"board": board, "device": fd, "design": design, "running": run} {
		if m := pathLike.FindString(b); m != "" {
			t.Errorf("%s shows a path-like string %q", name, m)
		}
	}
}

func TestDesignIsAbsentWithoutFixturesMode(t *testing.T) {
	e := newEnv(t)
	e.svc.Fixtures = false
	mux := http.NewServeMux()
	e.svc.Register(muxReg{mux})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, p := range []string{"/design", "/design/state"} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("%s outside fixtures mode: %d", p, res.StatusCode)
		}
	}
}

func TestBoardFilterAndPartials(t *testing.T) {
	e := newEnv(t)
	e.login()
	res, body := e.do("GET", "/?state=FAILED", nil)
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	mustContain(t, "filtered", body, "FRONTDESK-01", "Showing 1 of", "Remove filter: State: FAILED")
	if strings.Contains(body, "LAB-RETINA-02") {
		t.Fatal("filter must hide other devices")
	}
	_, body = e.do("GET", "/?state=NOPE", nil)
	mustContain(t, "bad filter ignored", body, "LAB-RETINA-02")
	_, body = e.do("GET", "/?state=UNKNOWN", nil)
	mustContain(t, "filter with no matches", body, "No devices match these filters", "Clear filters")

	res, part := e.do("GET", "/ui/partials/board", nil)
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	mustContain(t, "board partial", part, `id="board"`, `hx-swap-oob="outerHTML"`, `id="banners"`, `id="freshness"`, `hx-trigger="cryo:poll"`)
	if strings.Contains(part, "<html") || strings.Contains(part, "sidebar") {
		t.Fatal("a partial must not carry the page shell")
	}
	if res, _ := e.do("GET", "/ui/partials/run-progress/01J9ZZ0000000000000000ZZZZ", nil); res.StatusCode != 404 {
		t.Fatalf("unknown run: %d", res.StatusCode)
	}
	// The running device's run id resolves to a progress fragment.
	l, _ := e.st.ListRuns(context.Background(), store.ScopeAll(), e.dev["DR-OFFICE-PC"], store.Page{Limit: 5})
	var open string
	for _, r := range l.Items {
		if r.Outcome == "" {
			open = r.ID
		}
	}
	if open == "" {
		t.Fatal("fixture running device has no open run")
	}
	res, body = e.do("GET", "/ui/partials/run-progress/"+open, nil)
	mustContain(t, "progress partial", body, `role="progressbar"`, `id="run-`+open+`"`)
	// Empty-fleet and partial-data variants are alarms, never a neutral blank.
	empty := health.Fleet{View: state.FleetView{Complete: true}}
	mustContain(t, "empty-fleet panel", renderPanel(t, e, EmptyView{Kind: "alarm", Title: "No devices are enrolled", Body: "Nothing is being backed up."}), `panel-state alarm`, `role="alert"`)
	_ = empty
}

func renderPanel(t *testing.T, e *env, v EmptyView) string {
	s, err := e.svc.renderString("board", "panel_state", v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRestoreWizardEndToEndThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	base := "/ui/devices/" + id + "/restore"
	// Browse one level down.
	res, body := e.post(base, url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "dir": {"/C/Users/Public/Documents"}}, true)
	if res.StatusCode != 200 {
		t.Fatalf("browse: %d %s", res.StatusCode, body)
	}
	mustContain(t, "browse", body, "Quarterly-Report.txt", `name="path" value="/C/Users/Public/Documents/Quarterly-Report.txt"`, `id="restore-box"`, `aria-current="step"`)
	if strings.Contains(body, "<html") {
		t.Fatal("an htmx browse answers with the box only")
	}
	if e.be.Count(adminpath.MethodLs) == 0 {
		t.Fatal("browsing must go through adminpath Ls")
	}
	// Submitting without a path is a 422 with the listing kept.
	res, body = e.post(base, url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "cur": {"/C/Users/Public/Documents"}, "action": {"submit"}}, false)
	if res.StatusCode != 422 || !strings.Contains(body, "Choose a file first") {
		t.Fatalf("no file: %d", res.StatusCode)
	}
	// Submit.
	form := url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "cur": {"/C/Users/Public/Documents"}, "action": {"submit"}, "path": {"/C/Users/Public/Documents/Quarterly-Report.txt"}}
	res, _ = e.post(base, form, false)
	loc := res.Header.Get("Location")
	if res.StatusCode != 303 || !strings.HasPrefix(loc, "/restores/") {
		t.Fatalf("submit: %d %s", res.StatusCode, loc)
	}
	if strings.Contains(loc, "Quarterly") || strings.Contains(loc, "Documents") {
		t.Fatal("the path must not appear in a URL")
	}
	res, body = e.do("GET", loc, nil)
	if res.StatusCode != 200 {
		t.Fatalf("status page: %d", res.StatusCode)
	}
	mustContain(t, "restore status", body, "READY", "Download zip", `<form class="row" method="post" action="/ui/restores/`, `name="csrf_token"`, "The link works once")
	if strings.Contains(body, "Quarterly") {
		t.Fatal("the status page must not echo the path")
	}
	if strings.Contains(body, "token=") || strings.Contains(body, "/api/v1/restores/") {
		t.Fatal("no download token or URL may sit in page HTML: the button is a POST that redirects")
	}
	if res.Header.Get("Referrer-Policy") != "same-origin" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status page headers: %v", res.Header)
	}
	// The POST mints the token for this session and redirects to the GET URL.
	rid := strings.TrimPrefix(loc, "/restores/")
	res, _ = e.post("/ui/restores/"+rid+"/download", url.Values{}, false)
	if res.StatusCode != 303 || !strings.HasPrefix(res.Header.Get("Location"), "/api/v1/restores/"+rid+"/download?token=") {
		t.Fatalf("mint redirect: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res.Header.Get("Referrer-Policy") != "no-referrer" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("mint headers: %v", res.Header)
	}
	if res, _ := e.do("POST", "/ui/restores/"+rid+"/download", url.Values{}, "Origin", e.srv.URL); res.StatusCode != 403 {
		t.Fatalf("minting needs the CSRF token: %d", res.StatusCode)
	}
	if res, _ := e.do("GET", "/ui/partials/restore/"+strings.TrimPrefix(loc, "/restores/"), nil); res.StatusCode != 200 {
		t.Fatalf("restore partial: %d", res.StatusCode)
	}
	// htmx submit redirects with HX-Redirect.
	res, _ = e.post(base, form, true)
	if res.StatusCode != 204 || !strings.HasPrefix(res.Header.Get("HX-Redirect"), "/restores/") {
		t.Fatalf("htmx submit: %d %v", res.StatusCode, res.Header)
	}
	// CSRF and role.
	form.Del("csrf_token")
	r, _ := http.NewRequest("POST", e.srv.URL+base, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", e.srv.URL)
	for _, c := range e.sess {
		r.AddCookie(c)
	}
	if res, err := e.cli.Do(r); err != nil || res.StatusCode != 403 {
		t.Fatalf("missing CSRF token must be 403: %v %v", err, res)
	}
	// A customer-held device is refused with 409 even if the form is forged.
	held := e.dev["LEGAL-PC-01"]
	res, body = e.post("/ui/devices/"+held+"/restore", form, false)
	if res.StatusCode != 409 && !strings.Contains(body, "Customer-held") {
		t.Fatalf("customer-held submit: %d", res.StatusCode)
	}
	// Browse errors show a fixed sentence only.
	e.be.Errors = map[string]error{adminpath.MethodLs: adminpath.ErrUnavailable}
	_, body = e.post(base, url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "dir": {"/"}}, true)
	mustContain(t, "browse error", body, "Browsing is unavailable", "banner C07")
	e.be.Errors = nil
	// Another device's restore is a 404 page, not a leak.
	if res, _ := e.do("GET", "/restores/01J9ZZ0000000000000000ZZZZ", nil); res.StatusCode != 404 {
		t.Fatalf("unknown restore: %d", res.StatusCode)
	}
}

// TestRestoreFailureAndPendingAreStatedPlainly (ADR 0008): a restore is ready
// at once (nothing is prepared); a failed download says so with its class and
// stays downloadable; an expired or failed restore offers no download.
func TestRestoreFailureAndPendingAreStatedPlainly(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.be.Errors = map[string]error{adminpath.MethodDump: adminpath.NewError(adminpath.CodeStorageUnreachable, "network")}
	res, _ := e.post("/ui/devices/"+e.dev["DR-OFFICE-PC"]+"/restore", url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "action": {"submit"}, "path": {"/C/Users/Public/Desktop/Welcome.txt"}}, false)
	loc := res.Header.Get("Location")
	rid := strings.TrimPrefix(loc, "/restores/")
	_, body := e.do("GET", loc, nil)
	mustContain(t, "ready restore", body, "READY", "Download zip", "nothing is stored on the server")
	if strings.Contains(body, "Preparing") {
		t.Fatal("nothing is prepared any more")
	}
	if e.be.Count(adminpath.MethodDump) != 0 {
		t.Fatal("creating a restore ran a dump")
	}
	ctx := context.Background()
	class := "network"
	if _, err := e.st.UpdateRestoreRequest(ctx, store.ScopeAll(), rid, store.RestoreUpdate{ErrorClass: &class}); err != nil {
		t.Fatal(err)
	}
	_, body = e.do("GET", loc, nil)
	mustContain(t, "failed download", body, "READY", "The last download did not finish (class: network)", "Download zip")
	failed := "failed"
	if _, err := e.st.UpdateRestoreRequest(ctx, store.ScopeAll(), rid, store.RestoreUpdate{Status: &failed}); err != nil {
		t.Fatal(err)
	}
	_, body = e.do("GET", loc, nil)
	mustContain(t, "failed restore", body, "FAILED", "Nothing was restored", `role="alert"`)
	if strings.Contains(body, "Download zip") {
		t.Fatal("no download for a failed restore")
	}
	expired := "expired" // what the restore-purge job sets at expires_at
	if _, err := e.st.UpdateRestoreRequest(ctx, store.ScopeAll(), rid, store.RestoreUpdate{Status: &expired, ClearSelection: true}); err != nil {
		t.Fatal(err)
	}
	_, body = e.do("GET", loc, nil)
	mustContain(t, "expired restore", body, "EXPIRED", "no longer available")
	if strings.Contains(body, "Download zip") {
		t.Fatal("no download for an expired restore")
	}
}

func TestStaticAssets(t *testing.T) {
	e := newEnv(t)
	for path, ct := range map[string]string{"/static/app.css": "text/css", "/static/tokens.css": "text/css", "/static/fonts.css": "text/css", "/static/js/fresh.js": "text/javascript",
		"/static/js/htmx.min.js": "text/javascript", "/static/fonts/orbitron/Orbitron-var.woff2": "font/woff2", "/static/fonts/ibmplexsans/OFL.txt": "text/plain",
		"/static/fonts/martianmono/MartianMono-var.woff2": "font/woff2", "/static/fonts/martianmono/OFL.txt": "text/plain", "/static/fonts/martianmono/SOURCE.txt": "text/plain"} {
		res, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), ct) || len(b) == 0 || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d %s", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	// The data face is Martian Mono; IBM Plex Mono is gone from the stylesheets and from the served tree.
	for path, want := range map[string]string{"/static/fonts.css": "font-family: 'Martian Mono'", "/static/tokens.css": "--font-mono: 'Martian Mono'"} {
		res, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(b), want) || strings.Contains(string(b), "Plex Mono") {
			t.Errorf("%s: want %q and no 'Plex Mono'", path, want)
		}
	}
	if res, err := http.Get(e.srv.URL + "/static/fonts/ibmplexmono/IBMPlexMono-Regular.woff2"); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Error("the IBM Plex Mono files must be gone")
		}
	}
	// A versioned URL is immutable for a year; an unversioned one is short-lived.
	_, body := e.do("GET", "/login", nil)
	m := regexp.MustCompile(`/static/app\.css\?v=[0-9a-f]{8}`).FindString(body)
	if m == "" {
		t.Fatal("asset url without a content hash")
	}
	res, _ := http.Get(e.srv.URL + m)
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("versioned asset cache: %q", cc)
	}
	res, _ = http.Get(e.srv.URL + "/static/app.css")
	if cc := res.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Fatalf("unversioned asset cache: %q", cc)
	}
	for _, p := range []string{"/static/", "/static/fonts", "/static/fonts/", "/static/../go.mod", "/static/nope.css", "/static/fonts/orbitron/"} {
		res, err := http.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Errorf("%s must not be served (no directory listing, no traversal)", p)
		}
	}
}

// ---- golden: the state chip catalogue ----

func TestStateChipCatalogueGolden(t *testing.T) {
	rows := Catalogue()
	prec := state.Precedence()
	if len(rows) != len(prec) || len(prec) != 17 {
		t.Fatalf("catalogue has %d rows for %d states", len(rows), len(prec))
	}
	for i, r := range rows {
		if r.State != prec[i] {
			t.Fatalf("row %d is %s, precedence says %s", i, r.State, prec[i])
		}
	}
	var b strings.Builder
	chips := SampleChips(fixtures.Now(), time.UTC)
	glyph := map[string]string{ClassOK: "dot", ClassInfo: "square", ClassWarn: "triangle", ClassCrit: "diamond", ClassNeutral: "ring"}
	for i, r := range rows {
		b.WriteString(string(r.State) + "|" + r.Label + "|" + r.Class + "|" + glyph[r.Class] + "|" + chips[i].Text() + "\n")
	}
	run := RunningChip("g", 0.62)
	b.WriteString("RUNNING|" + run.Label + "|" + run.Class + "|" + glyph[run.Class] + "|" + run.Text() + "|pulse=" + map[bool]string{true: "yes", false: "no"}[run.Pulse] + "\n")
	for _, c := range OutcomeCatalogue() {
		b.WriteString("OUTCOME|" + c.Label + "|" + c.Class + "\n")
	}
	got := b.String()
	if os.Getenv("CRYO_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile("testdata/state_chips.golden", []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/state_chips.golden")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Fatalf("state chip catalogue differs from testdata/state_chips.golden:\n%s", got)
	}
}

func TestCatalogueAgainstTheDesignSpec(t *testing.T) {
	// components.md 3.1: label and class of every state.
	spec := map[state.State][2]string{
		state.StateRetired: {"RETIRED", "neutral"}, state.StatePaused: {"PARKED", "neutral"}, state.StateUnknown: {"UNKNOWN", "crit"},
		state.StateIdentityConflict: {"IDENTITY CONFLICT", "crit"}, state.StatePendingFirst: {"PENDING FIRST", "info"}, state.StateBlank: {"NEVER BACKED UP", "crit"},
		state.StateStuck: {"STUCK", "crit"}, state.StateFailed: {"FAILED", "crit"}, state.StatePartial: {"PARTIAL", "warn"}, state.StateUnseen: {"UNSEEN", "crit"},
		state.StateStale: {"STALE", "crit"}, state.StateEscrowUnverified: {"ESCROW UNVERIFIED", "warn"}, state.StateMaintFailed: {"RETENTION FAILED", "warn"},
		state.StateMissed: {"MISSED RUN", "warn"}, state.StateDeferred: {"DEFERRED", "warn"}, state.StateOutdated: {"OUTDATED AGENT", "warn"}, state.StateOK: {"OK", "ok"},
	}
	for _, r := range Catalogue() {
		if w := spec[r.State]; w[0] != r.Label || w[1] != r.Class {
			t.Errorf("%s: %s/%s, design says %s/%s", r.State, r.Label, r.Class, w[0], w[1])
		}
	}
	if ClassOf(state.State("SOMETHING_NEW")) != ClassCrit {
		t.Fatal("an unrecognised state is never OK")
	}
	// The real classifier output for every fixture device renders its catalogue label.
	e := newEnv(t)
	f, err := e.svc.Fleet.Fleet(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	c := fmtCtx{Now: f.Computed, Loc: e.loc}
	for _, ds := range f.View.Devices {
		chips := ChipsFor(ds, c, RunInfo{})
		if chips[0].Label != catalogue[ds.Primary].Label {
			t.Errorf("%s: chip %q for state %s", ds.Hostname, chips[0].Label, ds.Primary)
		}
	}
}

func TestChipQualifiers(t *testing.T) {
	now := fixtures.Now()
	want := map[state.State]string{
		state.StateFailed: "FAILED \u00d73", state.StateUnseen: "UNSEEN 9d", state.StateStale: "STALE 4d", state.StatePartial: "PARTIAL \u00b7 23 files",
		state.StatePendingFirst: "PENDING FIRST \u00b7 41h left", state.StateDeferred: "DEFERRED \u00b7 battery", state.StateOutdated: "OUTDATED AGENT \u00b7 1.4.2 to 1.5.0",
		state.StateStuck: "STUCK \u00b7 running 76 h (limit 12 h)", state.StateMaintFailed: "RETENTION FAILED \u00b7 prune", state.StateBlank: "NEVER BACKED UP \u00b7 only failures \u00b7 permission",
		state.StateOK: "OK", state.StateMissed: "MISSED RUN", state.StateUnknown: "UNKNOWN \u00b7 facts unavailable",
	}
	for st, w := range want {
		got := ChipsFor(SampleDeviceState(st, now), fmtCtx{Now: now, Loc: time.UTC}, RunInfo{})[0].Text()
		if got != w {
			t.Errorf("%s: %q, want %q", st, got, w)
		}
	}
	// RUNNING overlay: replaces OK, accompanies others, PENDING_FIRST keeps its own.
	c := fmtCtx{Now: now, Loc: time.UTC}
	if ch := ChipsFor(SampleDeviceState(state.StateOK, now), c, RunInfo{Open: true, Pct: 0.62}); len(ch) != 1 || ch[0].Text() != "RUNNING 62%" || !ch[0].Pulse {
		t.Fatalf("OK + run: %+v", ch)
	}
	if ch := ChipsFor(SampleDeviceState(state.StateFailed, now), c, RunInfo{Open: true, Pct: -1}); len(ch) != 2 || ch[0].Label != "FAILED" || ch[1].Label != "RUNNING" || !ch[1].Small {
		t.Fatalf("FAILED + run: %+v", ch)
	}
	if ch := ChipsFor(SampleDeviceState(state.StatePendingFirst, now), c, RunInfo{Open: true, Pct: 0.5}); len(ch) != 1 || ch[0].Text() != "PENDING FIRST \u00b7 50%" {
		t.Fatalf("pending + run: %+v", ch)
	}
	if OutcomeChip("x", "brand-new", false).Class != ClassCrit || OutcomeChip("x", "brand-new", false).Label != "UNKNOWN OUTCOME" {
		t.Fatal("an outcome from a newer agent is a failure")
	}
}

func TestBannerFixturesC01ToC16Render(t *testing.T) {
	e := newEnv(t)
	now := fixtures.Now()
	all := fixtures.Banners(now)
	if len(all) != 16 {
		t.Fatal(len(all))
	}
	crit := 0
	for i, b := range all {
		v := bannerView(b)
		if v.ID != b.ID || v.Label != BannerLabel(b.ID) || strings.HasPrefix(v.Label, "CONDITION") || v.Text == "" {
			t.Errorf("banner %d: %+v", i, v)
		}
		if (v.Class == ClassCrit) != (b.Severity >= state.SeverityCritical) {
			t.Errorf("%s class %s for severity %v", b.ID, v.Class, b.Severity)
		}
		if v.Class == ClassCrit {
			crit++
		}
		out, err := e.svc.renderString("board", "banners", BannersView{Items: []BannerView{v}})
		if err != nil || !strings.Contains(out, v.Label) || !strings.Contains(out, "<li class=\"banner "+v.Class) {
			t.Errorf("%s did not render: %v", b.ID, err)
		}
	}
	// More than three critical banners collapse behind a button that states the count.
	f := health.Fleet{View: state.FleetView{Banners: all}}
	bv := buildBanners(f, false)
	if len(bv.Extra) != crit-3 {
		t.Fatalf("collapse: %d extra for %d critical", len(bv.Extra), crit)
	}
	out, _ := e.svc.renderString("board", "banners", bv)
	if !strings.Contains(out, "and 7 more critical conditions") && !strings.Contains(out, "more critical conditions") {
		t.Fatalf("collapse button: %s", out)
	}
	if !strings.Contains(out, `Warning: `) {
		t.Fatal("warn banners are listed after the critical ones")
	}
	// Order inside the strip: critical before warn.
	seenWarn := false
	for _, it := range bv.Items {
		if it.Class == ClassWarn {
			seenWarn = true
		} else if seenWarn {
			t.Fatal("critical banner after a warning")
		}
	}
	// C09 carries its action, C08 its consequence.
	for _, b := range all {
		if b.ID == "C09" && bannerView(b).Action == nil {
			t.Error("C09 action")
		}
	}
	if v := bannerView(state.Banner{ID: "C08", Severity: state.SeverityCritical, Text: "No active devices enrolled."}); !strings.Contains(v.Text, "Nothing is being backed up") {
		t.Errorf("C08 text: %q", v.Text)
	}
}

func TestEmptyFleetAndPartialDataAreAlarms(t *testing.T) {
	e := newEnv(t)
	p := &auth.Principal{Scope: store.ScopeOf("no-such-customer"), Role: auth.RoleReadOnly}
	f, err := e.svc.Fleet.Fleet(context.Background(), p.Scope)
	if err != nil {
		t.Fatal(err)
	}
	// A scope with no customers, on an installation that has enrolled before: C08, an alarm panel and an alarm ring.
	b := e.svc.boardData(context.Background(), p, f, boardQuery{}, "", false)
	if b.Empty == nil || b.Empty.Kind != "alarm" {
		t.Fatalf("empty fleet must be an alarm panel: %+v", b.Empty)
	}
	if !b.Rings[0].Alarm || b.Rings[0].Center != "0" || b.KPIs[0].Tone != "bad" {
		t.Fatalf("zero devices: ring %+v kpi %+v", b.Rings[0], b.KPIs[0])
	}
	// First run is the only neutral case.
	fr := health.Fleet{View: state.FleetView{FirstRun: true, Complete: true}}
	if b := e.svc.boardData(context.Background(), p, fr, boardQuery{}, "", false); b.Empty == nil || b.Empty.Kind != "neutral" {
		t.Fatalf("first run: %+v", b.Empty)
	}
	// Partial data: C09 with its action.
	partial := health.Fleet{View: state.FleetView{Banners: []state.Banner{{ID: "C09", Severity: state.SeverityCritical, Text: "Partial data: 2 devices could not be classified", Count: 2}}}}
	if bv := buildBanners(partial, false); len(bv.Items) != 1 || bv.Items[0].Action == nil || bv.Items[0].Action.Href != "/?state=UNKNOWN" {
		t.Fatalf("C09: %+v", bv)
	}
}

func TestKPIsAndRings(t *testing.T) {
	e := newEnv(t)
	f, _ := e.svc.Fleet.Fleet(context.Background(), store.ScopeAll())
	c := fmtCtx{Now: f.Computed, Loc: e.loc}
	k := kpis(f, c, 1)
	if len(k) != 5 || k[0].Label != "Devices" || k[1].Label != "Within SLA" || k[2].Label != "Critical" || k[3].Label != "Warnings" || k[4].Label != "Backed up today" {
		t.Fatalf("tiles: %+v", k)
	}
	if k[0].Value != "25" || k[2].Tone != "bad" || k[4].Unit != "+ 1 running" {
		t.Fatalf("fixture tiles: %+v", k)
	}
	r := ageRing(f, c)
	total := 0
	for _, l := range r.Legend {
		total += l.N
	}
	if total != 25 || len(r.Segs) == 0 || r.Center != "25" {
		t.Fatalf("ring: %+v", r)
	}
	for _, s := range r.Segs {
		if !strings.Contains(s.Dash, " ") {
			t.Fatalf("dash: %q", s.Dash)
		}
	}
}

// ---- SSE hub ----

// fakeTicks hands the hub unbuffered tick channels: send blocks until the hub's
// Run loop has received the tick, so two consecutive sends prove the first
// tick's handler has finished (Run is single-threaded). The map is guarded by
// mu because the hub's goroutine fills it while the test goroutine reads it.
type fakeTicks struct {
	mu    sync.Mutex
	ch    map[time.Duration]chan time.Time
	ready chan struct{} // closed once all three tickers exist
}

func (f *fakeTicks) tick(d time.Duration) (<-chan time.Time, func()) {
	c := make(chan time.Time)
	f.mu.Lock()
	f.ch[d] = c
	if len(f.ch) == 3 {
		close(f.ready)
	}
	f.mu.Unlock()
	return c, func() {}
}

func (f *fakeTicks) send(t *testing.T, d time.Duration) {
	t.Helper()
	f.mu.Lock()
	c := f.ch[d]
	f.mu.Unlock()
	select {
	case c <- time.Now():
	case <-time.After(3 * time.Second):
		t.Fatalf("hub did not take the %v tick", d)
	}
}

func newFakeHub(t *testing.T) (*Hub, *fakeTicks, *atomic.Int32, func()) {
	t.Helper()
	ft := &fakeTicks{ch: map[time.Duration]chan time.Time{}, ready: make(chan struct{})}
	h := NewHub(30*time.Second, nil)
	h.Tick = ft.tick
	renders := new(atomic.Int32)
	h.render = func(ctx context.Context, sub *Sub) ([]Event, error) {
		renders.Add(1)
		return []Event{{Name: "fleet.state", Data: "<div id=\"board\">x</div>"}, {Name: "freshness", Data: "{}"}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx)
	}()
	select {
	case <-ft.ready:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("hub did not start its tickers")
	}
	return h, ft, renders, func() {
		cancel()
		<-done
	}
}

func recv(t *testing.T, ch <-chan Event, name string) Event {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Name != name {
			t.Fatalf("got event %q, want %q", ev.Name, name)
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatalf("no %q event", name)
	}
	return Event{}
}

func TestHubPingChangeAndRefreshCadence(t *testing.T) {
	h, ft, renders, stop := newFakeHub(t)
	defer stop()
	if h.Ping != 15*time.Second || h.Coalesce != 2*time.Second {
		t.Fatalf("cadences: ping %v coalesce %v", h.Ping, h.Coalesce)
	}
	a, cancelA := h.Subscribe(&Sub{Scope: store.ScopeAll(), Role: auth.RoleReadOnly})
	defer cancelA()
	b, cancelB := h.Subscribe(&Sub{Scope: store.ScopeAll(), Role: auth.RoleReadOnly})
	defer cancelB()
	other, cancelO := h.Subscribe(&Sub{Scope: store.ScopeOf("x"), Role: auth.RoleReadOnly})
	defer cancelO()

	ft.send(t, 15*time.Second)
	for _, c := range []<-chan Event{a, b, other} {
		recv(t, c, "ping")
	}
	// A change renders only after the coalescing tick, once per distinct view.
	h.DeviceChanged("d1")
	h.DeviceChanged("d2")
	h.RunProgress("d1", "r1")
	select {
	case ev := <-a:
		t.Fatalf("no event before the coalescing tick: %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	ft.send(t, 2*time.Second)
	recv(t, a, "fleet.state")
	recv(t, a, "freshness")
	recv(t, b, "fleet.state")
	recv(t, other, "fleet.state")
	if n := renders.Load(); n != 2 {
		t.Fatalf("one render per distinct view (two views), got %d", n)
	}
	// Nothing dirty: the coalescing tick is quiet. The ticks are unbuffered and
	// Run is single-threaded, so once the ping tick is taken the quiet tick's
	// handler has finished; subscriber a then sees the ping, not a fleet.state.
	ft.send(t, 2*time.Second)
	ft.send(t, 15*time.Second)
	recv(t, a, "ping")
	if n := renders.Load(); n != 2 {
		t.Fatalf("a clean fleet must not re-render on the coalescing tick, got %d renders", n)
	}
	// The refresh tick re-renders regardless (states change with the clock).
	ft.send(t, 30*time.Second)
	recv(t, a, "fleet.state")
	recv(t, a, "freshness")
	if n := renders.Load(); n != 4 {
		t.Fatalf("refresh renders: %d", n)
	}
	cancelB()
	if h.Subscribers() != 2 {
		t.Fatalf("subscribers: %d", h.Subscribers())
	}
}

func TestHubDropsASlowSubscriber(t *testing.T) {
	h, ft, _, stop := newFakeHub(t)
	defer stop()
	slow, cancel := h.Subscribe(&Sub{Scope: store.ScopeAll()})
	defer cancel()
	// The outbox holds 32; nobody reads it while the hub pings 40 times.
	for i := 0; i < 40; i++ {
		ft.send(t, 15*time.Second)
	}
	// The drop closes the channel (after removing the subscriber, under one
	// lock), so draining it ends exactly when the hub has dropped us.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range slow {
		}
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("a subscriber that cannot keep up must be dropped so the browser reconnects and refetches")
	}
	if n := h.Subscribers(); n != 0 {
		t.Fatalf("dropped subscriber still registered: %d", n)
	}
}

func TestSSEEndpointStreamsTheContract(t *testing.T) {
	e := newEnv(t)
	e.login()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/ui/events", nil)
	for _, c := range e.sess {
		r.AddCookie(c)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %d %v", res.StatusCode, res.Header)
	}
	buf := make([]byte, 0, 1<<16)
	tmp := make([]byte, 4096)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n, err := res.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		s := string(buf)
		if strings.Contains(s, "event: freshness") && strings.Contains(s, "event: banner.update") && strings.Contains(s, "event: fleet.state") && strings.Contains(s, "event: ping") {
			mustContain(t, "sse", s, "retry: 3000", `data: {"at":"1:42:10 PM"`, `<div id="board"`, `<section id="banners"`)
			if strings.Contains(s, "\r") {
				t.Fatal("event data must not contain carriage returns")
			}
			return
		}
		if err != nil {
			break
		}
	}
	t.Fatalf("stream did not deliver the events: %q", buf)
}

func TestSSEOfAnUnknownDeviceIsRefused(t *testing.T) {
	e := newEnv(t)
	e.login()
	if res, _ := e.do("GET", "/ui/events?device=01J9ZZ0000000000000000ZZZZ", nil); res.StatusCode != 404 {
		t.Fatalf("%d", res.StatusCode)
	}
}

// ---- design-system checks ----

func TestOrbitronAuditAndTokenCoverage(t *testing.T) {
	for _, c := range []Check{TokenCoverage(), OrbitronAudit()} {
		if !c.Pass {
			t.Errorf("%s failed:\n%s", c.Name, strings.Join(c.Details, "\n"))
		}
	}
}

func TestChecksCatchViolations(t *testing.T) {
	// The audit must be able to fail: feed the greps a bad stylesheet.
	if !hexColorRE.MatchString("a { color: #ff00aa; }") || !funcColorRE.MatchString("a { color: rgba(1,2,3,.5); }") {
		t.Fatal("colour grep is blind")
	}
	css := stripComments(`.btn { font-family: var(--font-display); } .t-page-title { font: 600 12px var(--font-display); } /* #fff */`)
	var bad []string
	for _, m := range ruleRE.FindAllStringSubmatch(css, -1) {
		if strings.Contains(m[2], "--font-display") && !orbitronRoles[strings.TrimSpace(m[1])] {
			bad = append(bad, strings.TrimSpace(m[1]))
		}
	}
	if len(bad) != 1 || bad[0] != ".btn" {
		t.Fatalf("orbitron audit logic: %v", bad)
	}
	if hexColorRE.MatchString(css) {
		t.Fatal("comments must be stripped before the grep")
	}
}

func TestContrastHasZeroAAFailures(t *testing.T) {
	rows, c := ContrastReport()
	if !c.Pass || len(rows) < 40 {
		t.Fatalf("%s (%d pairs): %v", c.Summary, len(rows), c.Details)
	}
	if r := Ratio("#000000", "#FFFFFF"); r < 20.9 || r > 21.1 {
		t.Fatalf("WCAG ratio of black on white: %v", r)
	}
}

// The login page must not tell an unknown user, a wrong password and a locked
// account apart: same status, same rendered error message.
func TestLoginPageFailuresAreIndistinguishable(t *testing.T) {
	e := newEnv(t)
	errMsg := regexp.MustCompile(`(?s)id="signin-err".*?<div class="b-text">(.*?)</div>`)
	h, _ := auth.HashPassword(testPassword)
	if _, err := e.st.CreateAdmin(context.Background(), store.Admin{Username: "lockme", Email: "lockme@example.invalid", Role: "read_only", PasswordHash: h}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // lock "lockme" (LockAfter is 5)
		if res, _ := e.do("POST", "/login", url.Values{"username": {"lockme"}, "password": {"nope"}}); res.StatusCode != 401 {
			t.Fatalf("priming failure %d: %d", i, res.StatusCode)
		}
	}
	if a, err := e.st.GetAdminByUsername(context.Background(), "lockme"); err != nil || a.LockedUntil == nil {
		t.Fatalf("the priming did not lock the account: %+v %v", a, err)
	}
	rows := []struct{ name, user, pw string }{
		{"unknown user", "ghost", "nope"},
		{"wrong password", "admin", "nope"},
		{"locked, wrong password", "lockme", "nope"},
		{"locked, right password", "lockme", testPassword},
	}
	var wantMsg string
	for i, row := range rows {
		res, body := e.do("POST", "/login", url.Values{"username": {row.user}, "password": {row.pw}})
		if res.StatusCode != 401 {
			t.Errorf("%s: status %d, want 401", row.name, res.StatusCode)
		}
		m := errMsg.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no error message rendered", row.name)
		}
		if i == 0 {
			wantMsg = strings.TrimSpace(m[1])
			if !strings.Contains(wantMsg, "not recognised") {
				t.Fatalf("unexpected message %q", wantMsg)
			}
		} else if got := strings.TrimSpace(m[1]); got != wantMsg {
			t.Errorf("%s: message %q differs from %q", row.name, got, wantMsg)
		}
		if strings.Contains(strings.ToLower(body), "locked") {
			t.Errorf("%s: the page mentions a lock", row.name)
		}
	}
}
