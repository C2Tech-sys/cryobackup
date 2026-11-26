package ui

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P22-G: the restore wizard (snapshot -> browse and select -> destination -> review), the
// restore status page and their fixtures. Every flow here posts the form a browser would post
// from the page it was given, with no JavaScript (no HX-Request header) unless a test says so.

var (
	inputTagRE = regexp.MustCompile(`<input ([^>]*)>`)
	attrPairRE = regexp.MustCompile(`([a-z-]+)(?:="([^"]*)")?`)
	selOptRE   = regexp.MustCompile(`<option value="([^"]*)" selected>`)
	boxRE      = regexp.MustCompile(`(?s)<form id="restore-box".*?</form>`)
)

// browserForm is what a browser with JavaScript off posts from the restore box of body before the
// clicked button is added: every hidden field, every checked and enabled checkbox and radio, the
// selected snapshot. Text fields are left for the test to fill.
func browserForm(t *testing.T, body string) url.Values {
	t.Helper()
	box := boxRE.FindString(body)
	if box == "" {
		t.Fatal("the page has no restore box")
	}
	v := url.Values{}
	for _, m := range inputTagRE.FindAllStringSubmatch(box, -1) {
		a := map[string]string{}
		has := map[string]bool{}
		for _, p := range attrPairRE.FindAllStringSubmatch(m[1], -1) {
			a[p[1]], has[p[1]] = html.UnescapeString(p[2]), true
		}
		switch a["type"] {
		case "hidden":
			v.Add(a["name"], a["value"])
		case "checkbox", "radio":
			if has["checked"] && !has["disabled"] {
				v.Add(a["name"], a["value"])
			}
		}
	}
	if m := selOptRE.FindStringSubmatch(box); m != nil {
		v.Set("snapshot", m[1])
	}
	return v
}

// click posts the restore box of body with extra set over it (the clicked button and any typed field).
func (e *env) click(t *testing.T, id, body string, extra url.Values) (*http.Response, string) {
	t.Helper()
	v := browserForm(t, body)
	for k, vals := range extra {
		if k == "path" { // a tick adds to what the page already carries
			v[k] = append(v[k], vals...)
			continue
		}
		v[k] = vals
	}
	return e.post("/ui/devices/"+id+"/restore", v, false)
}

func vv(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}

func (e *env) seedP22(t *testing.T) {
	t.Helper()
	if err := fixtures.SeedP22(context.Background(), e.st, e.now, e.svc.Restores.Master); err != nil {
		t.Fatal(err)
	}
}

func (e *env) auditCount(t *testing.T, action string) int {
	t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action}, store.Page{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

func TestP22G_TwoThousandEntryFolderThroughPaging(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.be.Files = fixtures.P22Files()
	id := e.dev["DR-OFFICE-PC"]
	res, body := e.post("/ui/devices/"+id+"/restore", vv("snapshot", fixtures.FixtureSnapshotID, "dir", fixtures.P22BigFolder), false)
	if res.StatusCode != 200 {
		t.Fatalf("browse: %d", res.StatusCode)
	}
	mustContain(t, "page 1", body, "1–500 of 2,000 entries", "scan-0001.pdf", "scan-0500.pdf", `name="page" value="500"`, "Next 500")
	if strings.Contains(body, "scan-0501.pdf") {
		t.Fatal("page 1 shows an entry of page 2")
	}
	seen := map[string]bool{}
	for off := 0; ; off += 500 {
		names := regexp.MustCompile(`scan-(\d{4})\.pdf`).FindAllString(body, -1)
		for _, n := range names {
			seen[n] = true
		}
		if !strings.Contains(body, `name="page" value="`+fmt.Sprint(off+500)+`">Next 500`) {
			break // the Next button is disabled on the last page
		}
		res, body = e.click(t, id, body, vv("page", fmt.Sprint(off+500)))
		if res.StatusCode != 200 {
			t.Fatalf("page at %d: %d", off+500, res.StatusCode)
		}
	}
	if len(seen) != fixtures.P22BigCount {
		t.Fatalf("paging reached %d of %d entries", len(seen), fixtures.P22BigCount)
	}
	mustContain(t, "last page", body, "1,501–2,000 of 2,000 entries", "scan-2000.pdf", `value="2000" disabled>Next 500`)
}

func TestP22G_BasketSurvivesNavigationAndASnapshotChangeClearsIt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	ctx := context.Background()
	id := e.dev["DR-OFFICE-PC"]
	older := strings.Repeat("e2", 32)
	if _, _, err := e.st.AgentReplaceSnapshots(ctx, id, []store.Snapshot{
		{DeviceID: id, SnapshotID: fixtures.FixtureSnapshotID, TakenAt: e.now.Add(-time.Hour), Hostname: "DR-OFFICE-PC", Paths: []string{`C:\Users`}},
		{DeviceID: id, SnapshotID: older, TakenAt: e.now.Add(-30 * time.Hour), Hostname: "DR-OFFICE-PC", Paths: []string{`C:\Users`}}}); err != nil {
		t.Fatal(err)
	}
	docs := "/C/Users/Public/Documents"
	_, body := e.post("/ui/devices/"+id+"/restore", vv("snapshot", fixtures.FixtureSnapshotID, "dir", docs), false)
	// Tick both files, then open another folder: the ticks travel along as hidden fields.
	res, body := e.click(t, id, body, vv("path", docs+"/Quarterly-Report.txt", "path", docs+"/Budget-2026.csv", "dir", "/C/Users/Public/Desktop"))
	if res.StatusCode != 200 {
		t.Fatalf("navigate: %d", res.StatusCode)
	}
	mustContain(t, "after navigating", body, "Welcome.txt", `<input type="hidden" name="path" value="`+docs+`/Quarterly-Report.txt">`,
		`<input type="hidden" name="path" value="`+docs+`/Budget-2026.csv">`, `2 of 50`)
	// Back in the folder they came from, they are checked boxes again (not hidden fields).
	_, body = e.click(t, id, body, vv("dir", docs))
	mustContain(t, "back", body, `name="path" value="`+docs+`/Quarterly-Report.txt" checked`, "2 of 50")
	if strings.Contains(body, `<input type="hidden" name="path" value="`+docs+`/Quarterly-Report.txt">`) {
		t.Fatal("a selected item shown in the listing must ride as its checkbox, not twice")
	}
	// A remove button drops one.
	_, body = e.click(t, id, body, vv("drop", docs+"/Budget-2026.csv"))
	mustContain(t, "removed", body, "1 of 50")
	// Selecting the folder that holds the other folds it in.
	_, body = e.click(t, id, body, vv("dir", "/C/Users/Public", "path", docs))
	mustContain(t, "folded", body, "1 of 50", "1 item is inside a selected folder.")
	// Another snapshot clears the selection, with a notice, and returns to the root.
	b := browserForm(t, body)
	b.Set("snapshot", older)
	b.Set("action", "snapshot")
	res, body = e.post("/ui/devices/"+id+"/restore", b, false)
	if res.StatusCode != 200 {
		t.Fatalf("snapshot change: %d", res.StatusCode)
	}
	mustContain(t, "cleared", body, "The selection was cleared.", "0 of 50", "Nothing selected yet.", `<option value="`+older+`" selected>`)
	if strings.Contains(body, `name="path" value="/C/Users/Public/Documents" checked`) || strings.Contains(body, `type="hidden" name="path"`) {
		t.Fatal("the selection outlived the snapshot change")
	}
	// The same clearing happens with JavaScript off, when the select changed and a folder button was pressed.
	_, body = e.click(t, id, body, vv("path", "/C"))
	b = browserForm(t, body)
	b.Set("snapshot", fixtures.FixtureSnapshotID)
	b.Set("dir", "/C")
	_, body = e.post("/ui/devices/"+id+"/restore", b, false)
	mustContain(t, "cleared without js", body, "The selection was cleared.", "0 of 50")
}

func TestP22G_OverTheLimitIsRefusedWithTheReason(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.be.Files = fixtures.P22Files()
	id := e.dev["DR-OFFICE-PC"]
	v := vv("snapshot", fixtures.FixtureSnapshotID, "snapcur", fixtures.FixtureSnapshotID, "step", "select", "cur", fixtures.P22BigFolder, "action", "submit")
	for i := 1; i <= 51; i++ {
		v.Add("path", fmt.Sprintf("%s/scan-%04d.pdf", fixtures.P22BigFolder, i))
	}
	res, body := e.post("/ui/devices/"+id+"/restore", v, false)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("over the limit: %d", res.StatusCode)
	}
	mustContain(t, "over", body, "51 items are selected; one restore takes at most 50.", `rw-count over`, "51 of 50", `name="step" value="select"`)
	if strings.Contains(body, `name="mode"`) {
		t.Fatal("an over-limit selection reached the destination step")
	}
	// 50 is fine.
	v.Del("path")
	for i := 1; i <= 50; i++ {
		v.Add("path", fmt.Sprintf("%s/scan-%04d.pdf", fixtures.P22BigFolder, i))
	}
	if res, body := e.post("/ui/devices/"+id+"/restore", v, false); res.StatusCode != 200 || !strings.Contains(body, `name="step" value="dest"`) {
		t.Fatalf("50 items: %d", res.StatusCode)
	}
}

// walkToReview walks the wizard with JavaScript off from the first page to the review step:
// tick pick in folder dir, Continue, choose mode, Continue.
func (e *env) walkToReview(t *testing.T, id, dir string, pick []string, mode string) string {
	t.Helper()
	res, body := e.do("GET", "/devices/"+id+"/restore", nil)
	if res.StatusCode != 200 {
		t.Fatalf("wizard: %d", res.StatusCode)
	}
	if dir != "/" {
		res, body = e.click(t, id, body, vv("dir", dir))
		if res.StatusCode != 200 {
			t.Fatalf("browse: %d", res.StatusCode)
		}
	}
	x := url.Values{"path": pick, "action": {"submit"}}
	res, body = e.click(t, id, body, x)
	if res.StatusCode != 200 || !strings.Contains(body, `name="step" value="dest"`) {
		t.Fatalf("to the destination: %d", res.StatusCode)
	}
	res, body = e.click(t, id, body, vv("mode", mode, "action", "submit"))
	if res.StatusCode != 200 || !strings.Contains(body, `name="step" value="review"`) {
		t.Fatalf("to the review: %d", res.StatusCode)
	}
	return body
}

func TestP22G_UsableWithJavaScriptOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	docs := "/C/Users/Public/Documents"
	// The destination step offers the device first: original location, keep existing (ruling C1).
	res, body := e.do("GET", "/devices/"+id+"/restore", nil)
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	_, body = e.click(t, id, body, vv("dir", docs))
	_, body = e.click(t, id, body, vv("path", docs+"/Quarterly-Report.txt", "action", "submit"))
	mustContain(t, "destination", body, `name="mode" value="original" checked`, `name="mode" value="side"`, `name="mode" value="replace"`, `name="mode" value="zip"`,
		"Original location, keep existing files", "Where should 1 item go?")
	// Back keeps the selection.
	_, back := e.click(t, id, body, vv("action", "back"))
	mustContain(t, "back to select", back, `name="path" value="`+docs+`/Quarterly-Report.txt" checked`)
	// Continue to the review: plain words.
	_, body = e.click(t, id, body, vv("action", "submit"))
	mustContain(t, "review", body, "Restore 1 item to their original places on DR-OFFICE-PC", "Files that already exist there are kept as they are", "Nothing on DR-OFFICE-PC is deleted.",
		`<input type="hidden" name="mode" value="original">`, `value="submit">Start the restore</button>`, "DR-OFFICE-PC is online.")
	before := len(e.apiWakes)
	res, _ = e.click(t, id, body, vv("action", "submit"))
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/restores/") {
		t.Fatalf("start: %d %s", res.StatusCode, loc)
	}
	e.apiWakeMu.Lock()
	woke := len(e.apiWakes) > before && e.apiWakes[len(e.apiWakes)-1] == id
	e.apiWakeMu.Unlock()
	if !woke {
		t.Error("a device restore must wake the device's command poll")
	}
	_, status := e.do("GET", loc, nil)
	mustContain(t, "queued status", status, "QUEUED", "Queued. DR-OFFICE-PC is online.", "Original location, keep existing files", `hx-get="/ui/partials/restore/`)
	rr, err := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), strings.TrimPrefix(loc, "/restores/"))
	if err != nil || rr.Mode != store.RestoreModeDeviceOriginal || rr.Overwrite != store.RestoreOverwriteNever || rr.Status != "queued" {
		t.Fatalf("the row: %+v %v", rr, err)
	}
	// The download goes the same way, with JavaScript off, and lands on the status page.
	review := e.walkToReview(t, e.dev["FRONTDESK-01"], "/C/Users/Public/Documents", []string{docs + "/Budget-2026.csv", docs + "/Quarterly-Report.txt"}, "zip")
	mustContain(t, "zip review", review, "Download 2 items from FRONTDESK-01 as a zip", `value="submit">Continue to the download</button>`)
	res, _ = e.click(t, e.dev["FRONTDESK-01"], review, vv("action", "submit"))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("zip: %d", res.StatusCode)
	}
	_, status = e.do("GET", res.Header.Get("Location"), nil)
	mustContain(t, "zip status", status, "READY", "Download zip", "2 items")
}

func TestP22G_ReplaceNeedsTheExactHostName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	body := e.walkToReview(t, id, "/C/Users/Public/Desktop", []string{"/C/Users/Public/Desktop/Welcome.txt"}, "replace")
	mustContain(t, "replace review", body, `name="confirm_host"`, "Type <span class=\"mono\">DR-OFFICE-PC</span> to confirm", "replacing changed files")
	for _, typed := range []string{"", "dr-office-pc", "DR-OFFICE-PC ", "DR-OFFICE"} {
		res, b := e.click(t, id, body, vv("confirm_host", typed, "action", "submit"))
		if res.StatusCode != http.StatusBadRequest || !strings.Contains(b, "The host name does not match") || !strings.Contains(b, `aria-invalid="true"`) {
			t.Fatalf("%q: %d", typed, res.StatusCode)
		}
		if n := e.restoreCount(id); n != 0 {
			t.Fatalf("%q created %d restores", typed, n)
		}
	}
	res, _ := e.click(t, id, body, vv("confirm_host", "DR-OFFICE-PC", "action", "submit"))
	if res.StatusCode != http.StatusSeeOther || e.restoreCount(id) != 1 {
		t.Fatalf("exact host name: %d", res.StatusCode)
	}
	l, _ := e.st.ListRestoreRequests(context.Background(), store.ScopeAll(), id, store.Page{Limit: 5})
	if l.Items[0].Overwrite != store.RestoreOverwriteIfChanged {
		t.Fatalf("replace stored as %q", l.Items[0].Overwrite)
	}
}

func TestP22G_StaleSessionAsksForTheCodeOnTheReviewStep(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.advance(6 * time.Minute) // the login's check ages past the window
	id := e.dev["DR-OFFICE-PC"]
	docs := "/C/Users/Public/Documents"
	body := e.walkToReview(t, id, docs, []string{docs + "/Quarterly-Report.txt"}, "zip")
	mustContain(t, "stale review", body, `name="code"`, `autocomplete="one-time-code"`, "Download 1 item from DR-OFFICE-PC as a zip",
		`<input type="hidden" name="path" value="`+docs+`/Quarterly-Report.txt">`)
	calls := len(e.be.Calls())
	res, b := e.click(t, id, body, vv("action", "submit"))
	if res.StatusCode != http.StatusForbidden || !strings.Contains(b, "A recent MFA check is required") || !strings.Contains(b, `name="code"`) {
		t.Fatalf("stale submit: %d", res.StatusCode)
	}
	if strings.Contains(b, "This folder is empty.") {
		t.Fatal("the stale re-render shows an empty folder")
	}
	mustContain(t, "stale keeps the selection", b, `<input type="hidden" name="path" value="`+docs+`/Quarterly-Report.txt">`, "1 selected item")
	if e.restoreCount(id) != 0 || len(e.be.Calls()) != calls {
		t.Fatal("a stale submit wrote a restore or asked the worker")
	}
	res, _ = e.click(t, id, b, vv("action", "submit", "code", e.mfa.code("admin")))
	if res.StatusCode != http.StatusSeeOther || e.restoreCount(id) != 1 {
		t.Fatalf("with the code: %d, %d restores", res.StatusCode, e.restoreCount(id))
	}
}

func TestP22G_EachRefusalHasItsPage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedP22(t)
	docs := "/C/Users/Public/Documents"
	forged := func(host, mode string) (*http.Response, string) {
		return e.post("/ui/devices/"+e.dev[host]+"/restore", vv("snapshot", fixtures.FixtureSnapshotID, "step", "review", "mode", mode, "action", "submit",
			"path", docs+"/Quarterly-Report.txt"), false)
	}
	destOf := func(host string) string {
		t.Helper()
		e.do("GET", "/devices/"+e.dev[host]+"/restore", nil) // an empty snapshot cache is filled from the repository
		_, body := e.post("/ui/devices/"+e.dev[host]+"/restore", vv("snapshot", fixtures.FixtureSnapshotID, "step", "select", "action", "submit", "path", docs+"/Quarterly-Report.txt"), false)
		if !strings.Contains(body, `name="step" value="dest"`) {
			t.Fatalf("%s: no destination step", host)
		}
		return body
	}
	disabledDevice := func(name, body string, why ...string) {
		t.Helper()
		for _, m := range []string{"original", "side", "replace"} {
			mustContain(t, name, body, `name="mode" value="`+m+`" disabled`)
		}
		mustContain(t, name, body, append([]string{`name="mode" value="zip" checked`}, why...)...)
	}
	// Retired: the wizard opens, says download only, and offers only the zip.
	_, page := e.do("GET", "/devices/"+e.dev["OLD-RECEPTION-PC"]+"/restore", nil)
	mustContain(t, "retired wizard", page, "This device is retired: download only.", `name="path"`)
	disabledDevice("retired dest", destOf("OLD-RECEPTION-PC"), "it can be downloaded from, not restored to (ADR 0030)")
	if res, b := forged("OLD-RECEPTION-PC", "original"); res.StatusCode != 409 || !strings.Contains(b, "This device is retired: download only") {
		t.Fatalf("retired, device mode: %d", res.StatusCode)
	}
	if res, _ := forged("OLD-RECEPTION-PC", "zip"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("retired, zip: %d", res.StatusCode)
	}
	// An agent older than 0.4.0.
	disabledDevice("old agent dest", destOf(fixtures.P22OldAgentHost), "is "+fixtures.P22OldAgentVersion+". Restoring to the device needs agent 0.4.0 or newer.")
	if res, b := forged(fixtures.P22OldAgentHost, "side"); res.StatusCode != 409 || !strings.Contains(b, "The agent on this device is too old") {
		t.Fatalf("old agent: %d", res.StatusCode)
	}
	// A device restore already active (the seeded queued one on OPS-ND-01).
	active := destOf("OPS-ND-01")
	disabledDevice("active dest", active, "is already queued or running")
	mustContain(t, "active link", active, `href="/restores/`+fixtures.P22RestoreID(6)+`"`)
	if res, b := forged("OPS-ND-01", "original"); res.StatusCode != 409 || !strings.Contains(b, "A restore is already active for this device") {
		t.Fatalf("active: %d", res.StatusCode)
	}
	// Blocked outright: purging and customer-held, on the wizard and on a forged create.
	for host, want := range map[string]string{"SPARE-PC-09": "This device is being purged", "LEGAL-PC-01": "Customer-held key"} {
		_, page := e.do("GET", "/devices/"+e.dev[host]+"/restore", nil)
		mustContain(t, host+" wizard", page, want)
		if strings.Contains(page, `name="path"`) {
			t.Errorf("%s: a blocked device is browsable", host)
		}
		if res, b := forged(host, "zip"); res.StatusCode != 409 || !strings.Contains(b, want) {
			t.Errorf("%s forged: %d", host, res.StatusCode)
		}
		if e.restoreCount(e.dev[host]) != 0 {
			t.Errorf("%s got a restore", host)
		}
	}
	// The master key is missing: blocked with its own sentence.
	e.svc.MasterLoaded = func() bool { return false }
	_, page = e.do("GET", "/devices/"+e.dev["FRONTDESK-01"]+"/restore", nil)
	mustContain(t, "no master key", page, "Master key unavailable", "Restores are disabled until the master key is loaded")
}

func TestP22G_EveryStatusHasAFixtureAndItsSentence(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedP22(t)
	want := map[string][]string{
		"zip-ready":                 {"READY", "Download zip", "The link works once"},
		"zip-delivered":             {"DELIVERED", "The zip was downloaded.", "Download zip"},
		"zip-delivered-skipped":     {"DELIVERED", "Left out", "3 links or special entries", "CRYOBACKUP-SKIPPED.txt"},
		"zip-cut":                   {"READY", "The last download did not finish (class: disconnected).", "No zip was saved"},
		"zip-expired":               {"EXPIRED", "no longer available"},
		"device-queued":             {"QUEUED", "Queued. OPS-ND-01 is online.", "Download as zip instead"},
		"device-queued-offline":     {"QUEUED", "Queued. LAB-RETINA-02 is offline.", "Last seen 9 d ago (", "Starts when online, until", "For files now, use Download as zip."},
		"device-running":            {"RUNNING", "Running on HYGIENE-PC-2."},
		"device-waiting":            {"WAITING", "Waiting for the running backup."},
		"device-restored":           {"RESTORED", "RESTORED: 1284 restored, 37 skipped because they already exist.", "1,284", "already there"},
		"device-partial":            {"RESTORED WITH ERRORS", "Errors by class: access_denied 3, target_locked 1, other 1.", "Windows refused access", "file held open by another program", "Not restored"},
		"device-failed-auth":        {"FAILED", "the gateway refused the device&#39;s login", `role="alert"`},
		"device-failed-disk":        {"FAILED", "not enough free space on the device (see the device&#39;s log for the numbers)"},
		"device-failed-guard":       {"FAILED", "failed its safety check"},
		"device-failed-nothing":     {"FAILED", "nothing in the backup matched the selection"},
		"device-failed-interrupted": {"FAILED", "the restore was cut short", "may be incomplete"},
		"device-failed-repo":        {"FAILED", "could not reach its repository"},
		"device-cancelled":          {"CANCELLED", "cancelled before the device started it"},
		"device-expired":            {"EXPIRED", "EXPIRED: the device did not pick the restore up in time.", "Download as zip instead"},
	}
	rows := fixtures.P22Restores()
	if len(rows) != len(want) {
		t.Fatalf("%d fixture rows, %d expectations", len(rows), len(want))
	}
	for _, r := range rows {
		res, body := e.do("GET", "/restores/"+r.ID, nil)
		if res.StatusCode != 200 {
			t.Errorf("%s: %d", r.Key, res.StatusCode)
			continue
		}
		mustContain(t, r.Key, body, want[r.Key]...)
		active := r.Status == "queued" || r.Status == "running"
		if polls := strings.Contains(body, `hx-get="/ui/partials/restore/`+r.ID+`"`); polls != active {
			t.Errorf("%s: polling %v, want %v (it stops at a final status)", r.Key, polls, active)
		}
		if _, part := e.do("GET", "/ui/partials/restore/"+r.ID, nil); !strings.Contains(part, want[r.Key][0]) {
			t.Errorf("%s: the live part lacks %q", r.Key, want[r.Key][0])
		}
	}
}

func TestP22G_NoPathInAURLALogOrAnAuditRow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	const sentinel = "SENTINEL-7Q4ZK"
	dir := "/D/" + sentinel + "-dir"
	e.be.Files = append(fixtures.DefaultFiles(), fixtures.File{Path: dir + "/" + sentinel + "-file.txt", Content: []byte("x"), Mtime: e.now})
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	e.svc.Restores.Log = logx.New(&logs, logx.LevelDebug)
	var pages []string
	var locations []string
	keep := func(res *http.Response, body string) string {
		pages = append(pages, body)
		locations = append(locations, res.Header.Get("Location"), res.Header.Get("HX-Redirect"))
		return body
	}
	for _, host := range []string{"DR-OFFICE-PC", "FRONTDESK-01"} {
		id := e.dev[host]
		mode := map[string]string{"DR-OFFICE-PC": "zip", "FRONTDESK-01": "side"}[host]
		res, body := e.do("GET", "/devices/"+id+"/restore", nil)
		keep(res, body)
		res, body = e.click(t, id, body, vv("dir", dir))
		keep(res, body)
		res, body = e.click(t, id, body, vv("path", dir+"/"+sentinel+"-file.txt", "path", dir, "action", "submit"))
		keep(res, body)
		res, body = e.click(t, id, body, vv("mode", mode, "action", "submit"))
		keep(res, body)
		res, body = e.click(t, id, body, vv("action", "submit"))
		keep(res, body)
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: %d", host, res.StatusCode)
		}
		loc := res.Header.Get("Location")
		res, body = e.do("GET", loc, nil)
		keep(res, body)
		res, body = e.do("GET", "/ui/partials/restore/"+strings.TrimPrefix(loc, "/restores/"), nil)
		keep(res, body)
		if mode == "zip" {
			res, body = e.post("/ui/restores/"+strings.TrimPrefix(loc, "/restores/")+"/download", url.Values{}, false)
			keep(res, body)
		}
	}
	urlAttr := regexp.MustCompile(`(?:href|action|hx-get|hx-post|src|formaction)="([^"]*)"`)
	for i, p := range pages {
		for _, m := range urlAttr.FindAllStringSubmatch(p, -1) {
			if strings.Contains(m[1], sentinel) {
				t.Errorf("page %d puts the path in a URL: %s", i, m[0])
			}
		}
		if !strings.Contains(p, sentinel) && i == 1 {
			t.Error("the sentinel never reached the listing: the test proves nothing")
		}
	}
	for _, l := range locations {
		if strings.Contains(l, sentinel) {
			t.Errorf("a redirect carries the path: %s", l)
		}
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Errorf("the log holds the path:\n%s", logs.String())
	}
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{}, store.Page{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	browse := 0
	for _, a := range l.Items {
		if strings.Contains(string(a.Details), sentinel) || strings.Contains(a.ObjectID, sentinel) || strings.Contains(a.ActorLabel, sentinel) {
			t.Errorf("audit %s holds the path: %s", a.Action, a.Details)
		}
		if a.Action == "restore.browse" {
			browse++
		}
	}
	if browse != 2 {
		t.Errorf("%d restore.browse rows for two devices browsed in one hour, want 2", browse)
	}
}

func TestP22G_BrowseAuditIsThrottledAndRefusesWhenItCannotWrite(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	browse := func() string {
		_, body := e.post("/ui/devices/"+id+"/restore", vv("snapshot", fixtures.FixtureSnapshotID, "dir", "/C/Users"), false)
		return body
	}
	browse()
	browse()
	if n := e.auditCount(t, "restore.browse"); n != 1 {
		t.Fatalf("%d browse rows in one hour, want 1", n)
	}
	e.advance(61 * time.Minute)
	e.login() // a fresh session at the new time
	browse()
	if n := e.auditCount(t, "restore.browse"); n != 2 {
		t.Fatalf("%d browse rows after the hour, want 2", n)
	}
	// An hour later, with the audit table gone: the browse is refused, and the worker is not asked.
	e.advance(61 * time.Minute)
	e.login()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE audit_events RENAME TO audit_events_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ls := e.be.Count(adminpath.MethodLs)
	_, body := e.post("/ui/devices/"+id+"/restore", vv("snapshot", fixtures.FixtureSnapshotID, "dir", "/C/Users/Public"), false)
	mustContain(t, "not audited", body, "Browsing refused: NOT AUDITED")
	if e.be.Count(adminpath.MethodLs) != ls {
		t.Fatal("an unaudited browse reached the worker")
	}
	if strings.Contains(body, "Documents") {
		t.Fatal("an unaudited browse shows the listing")
	}
}

func TestP22G_RefreshFromTheRepository(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM snapshots WHERE device_id = ?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snaps := e.be.Count(adminpath.MethodSnapshots)
	_, body := e.do("GET", "/devices/"+id+"/restore", nil)
	if e.be.Count(adminpath.MethodSnapshots) != snaps+1 {
		t.Fatal("an empty cache must ask the repository once")
	}
	mustContain(t, "auto refresh", body, `<option value="`+fixtures.FixtureSnapshotID+`" selected>`, `value="/C"`)
	_, body = e.click(t, id, body, vv("action", "refresh"))
	mustContain(t, "refresh", body, "Snapshot list refreshed from the repository.", "1 snapshot found.")
	e.be.Errors = map[string]error{adminpath.MethodSnapshots: adminpath.ErrUnavailable}
	_, body = e.click(t, id, body, vv("action", "refresh"))
	mustContain(t, "refresh failed", body, "The snapshot list was not refreshed", "banner C07")
}

func TestP22G_DevicePageRetiredIsDownloadOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	retired := e.page("/devices/"+e.dev["OLD-RECEPTION-PC"], 200)
	mustContain(t, "retired device page", retired, `<a class="btn primary sm" href="/devices/`+e.dev["OLD-RECEPTION-PC"]+`/restore" aria-describedby="restore-why">`,
		"This device is retired: download as a zip only (ADR 0030).")
	purging := e.page("/devices/"+e.dev["SPARE-PC-09"], 200)
	mustContain(t, "purging device page", purging, `aria-disabled="true" aria-describedby="restore-why"`, "This device is being purged")
	held := e.page("/devices/"+e.dev["LEGAL-PC-01"], 200)
	mustContain(t, "customer-held device page", held, "Customer-held key: no technician restore.", `aria-disabled="true"`)
}

func TestP22G_DesignShowsTheRestoreComponents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	body := e.page("/design/restore", 200)
	mustContain(t, "design restore", body, `id="d-restore"`, "Wizard bar", "A page of a 2 000-entry folder", "501–1,000 of 2,000 entries", "Over the 50-item limit",
		"An agent older than 0.4.0", `name="confirm_host"`, `name="code"`, "RESTORED WITH ERRORS", "Waiting for the running backup.", `aria-current="page">Restore</a>`)
	for _, r := range fixtures.P22Restores() {
		mustContain(t, "design outcome "+r.Key, body, `id="d-rs-`+r.Key+`"`)
	}
	if m := pathLike.FindString(body); m != "" {
		t.Errorf("/design/restore shows a path-like string %q", m)
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Fatal("html/template rejected a value (ZgotmplZ)")
	}
}

// layoutProbeOverflow measures sideways overflow of the page as laid out.
const layoutProbeOverflow = `return {iw: innerWidth, overflow: document.documentElement.scrollWidth - innerWidth};`

func TestP22G_NoSidewaysScrollAt390(t *testing.T) {
	t.Parallel()
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	e.be.Files = fixtures.P22Files()
	e.seedP22(t)
	css := e.appCSS(t)
	id := e.dev["FRONTDESK-01"]
	forms := fixtures.P22FormsFolder
	pages := map[string]string{}
	_, body := e.do("GET", "/devices/"+id+"/restore", nil)
	_, body = e.click(t, id, body, vv("dir", forms))
	_, body = e.click(t, id, body, vv("path", forms+"/"+fixtures.P22LongName, "path", forms+"/Intake", "dir", forms))
	pages["select"] = body
	_, big := e.click(t, id, body, vv("dir", fixtures.P22BigFolder))
	pages["big"] = big
	_, dest := e.click(t, id, body, vv("action", "submit"))
	pages["dest"] = dest
	_, review := e.click(t, id, dest, vv("mode", "replace", "action", "submit"))
	pages["review"] = review
	for _, k := range []string{"device-partial", "device-queued-offline", "zip-delivered-skipped", "zip-cut"} {
		r, _ := fixtures.P22RestoreByKey(k)
		pages["status-"+k] = e.page("/restores/"+r.ID, 200)
	}
	for name, html := range pages {
		for _, w := range []int{390, 1360} {
			for _, theme := range []string{"dark", "light"} {
				t.Run(fmt.Sprintf("%s_%d_%s", name, w, theme), func(t *testing.T) {
					got := e.layout(t, layoutOpts{HTML: html, CSS: css, W: w, H: 900,
						Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `');` + layoutProbeOverflow})
					if got["overflow"] > 0.5 {
						t.Errorf("the page overflows sideways by %.0f px", got["overflow"])
					}
				})
			}
		}
	}
}
