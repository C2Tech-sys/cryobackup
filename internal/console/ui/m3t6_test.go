package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T6: the enrollment page (token table first, the wizard on its own page, token
// delete), the retire dialog and "Retired (n)".

func (e *env) t6Device(id string) store.Device {
	e.t.Helper()
	d, err := e.st.GetDevice(context.Background(), store.ScopeAll(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// t6Token makes a token of a customer straight in the store and returns its id.
func (e *env) t6Token(cust, label string, revoked bool) string {
	e.t.Helper()
	ctx := context.Background()
	tok, err := e.st.CreateEnrollmentToken(ctx, store.ScopeAll(), store.EnrollmentToken{CustomerID: cust, Label: label,
		TokenHash: fmt.Sprintf("%064x", time.Now().UnixNano()), ExpiresAt: e.now.Add(48 * time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	if revoked {
		if err := e.st.RevokeEnrollmentToken(ctx, store.ScopeAll(), tok.ID); err != nil {
			e.t.Fatal(err)
		}
	}
	return tok.ID
}

// ---- A: the enrollment page ----

func TestM3T6A_EnrollIsTheTokenTableAndTheWizardIsItsOwnPage(t *testing.T) {
	e := newEnv(t)
	e.login()
	page := e.page("/enroll", 200)
	mustContain(t, "/enroll", page, `class="fleet tokens"`, `href="/enroll/new"`, "New enrollment")
	if strings.Contains(page, `action="/ui/enroll/token"`) || strings.Contains(page, `name="new_customer"`) {
		t.Error("/enroll still carries the wizard's form: it must be on its own page")
	}
	wiz := e.page("/enroll/new", 200)
	mustContain(t, "/enroll/new", wiz, `action="/ui/enroll/token"`, `name="customer_id"`, `name="max_uses"`, `name="expires"`, `href="/enroll"`, `aria-current="step"`)
	if strings.Contains(wiz, `class="fleet tokens"`) {
		t.Error("/enroll/new also shows the token table")
	}
	// A rejected create stays on the wizard, with what was typed.
	summit := e.customerID("Summit Eye Care")
	res, body := e.post("/ui/enroll/token", vals("customer_id", summit, "new_customer", "Both At Once", "label", "kept label", "expires", "7d", "max_uses", "10", "device_class", "unknown"), false)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("rejected create: %d", res.StatusCode)
	}
	mustContain(t, "rejected", body, "The token was not created", `value="kept label"`, `action="/ui/enroll/token"`)
	if strings.Contains(body, `class="fleet tokens"`) {
		t.Error("the rejected wizard also shows the token table")
	}
	// The filtered list's button carries its customer into the wizard.
	filtered := e.page("/enroll?customer="+summit, 200)
	mustContain(t, "filtered", filtered, `href="/enroll/new?customer=`+summit+`"`)
	preselected := e.page("/enroll/new?customer="+summit, 200)
	mustContain(t, "preselected", preselected, `value="`+summit+`" selected`)
}

func TestM3T6A_ActiveTokensComeFirstAndStatusesAreFilterable(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	gone := e.t6Token(summit, "newest but revoked", true)
	page := e.page("/enroll?customer="+summit, 200)
	if strings.Index(page, `id="tok-`+gone+`"`) < strings.Index(page, `summit-eye-care-2026q4`) {
		t.Error("a revoked token sits above an active one, though it is newer")
	}
	mustContain(t, "segments", page, `class="tokensegs"`, `status=revoked`, `status=expired`, `status=active`)
	// The revoked filter shows it alone, with Delete and no Revoke.
	f := e.page("/enroll?customer="+summit+"&status=revoked", 200)
	mustContain(t, "revoked filter", f, `id="tok-`+gone+`"`, `/ui/enroll/tokens/`+gone+`/delete`)
	if strings.Contains(f, `/ui/enroll/tokens/`+gone+`/revoke`) {
		t.Error("a revoked token still offers Revoke")
	}
	// An active token offers Revoke and no Delete.
	a := e.page("/enroll?customer="+summit+"&status=active", 200)
	mustContain(t, "active filter", a, `/revoke"`)
	if strings.Contains(a, `/delete"`) {
		t.Error("an active token offers Delete")
	}
}

func TestM3T6A_DeleteHidesARevokedTokenAndRefusesAnActiveOne(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	live := e.t6Token(summit, "still live", false)
	res, _ := e.post("/ui/enroll/tokens/"+live+"/delete", vals(), false)
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "saved=not_deletable") {
		t.Fatalf("deleting an active token: %d %q", res.StatusCode, loc)
	}
	mustContain(t, "refusal", e.page("/enroll?saved=not_deletable", 200), "Only a revoked or expired token can be deleted")
	if n := len(e.auditRows("enrollment_token.delete", live)); n != 0 {
		t.Fatalf("a refused delete wrote %d audit rows", n)
	}

	gone := e.t6Token(summit, "old and revoked", true)
	res, _ = e.post("/ui/enroll/tokens/"+gone+"/delete", vals(), false)
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "saved=deleted") {
		t.Fatalf("delete: %d %q", res.StatusCode, loc)
	}
	if n := len(e.auditRows("enrollment_token.delete", gone)); n != 1 {
		t.Fatalf("delete audit rows = %d, want 1", n)
	}
	if list := e.page("/enroll?customer="+summit+"&status=revoked&saved=deleted", 200); strings.Contains(list, `id="tok-`+gone+`"`) {
		t.Error("the deleted token is still listed")
	}
	// The detail page still answers, says so and offers nothing more.
	detail := e.page("/enroll/tokens/"+gone, 200)
	mustContain(t, "detail", detail, "Deleted", "hidden from the token list")
	if strings.Contains(detail, "/delete\"") || strings.Contains(detail, "/revoke\"") {
		t.Error("a deleted token's page still offers an action")
	}
}

func TestM3T6_EnrollRoutesNeedTheAdminRole(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	gone := e.t6Token(summit, "old", true)
	id := e.dev["FRONTDESK-01"]
	for _, role := range []string{"read_only", "operator"} {
		e.signInAs("t6-"+role, role, nil)
		if res, _ := e.do("GET", "/enroll/new", nil); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s GET /enroll/new: %d", role, res.StatusCode)
		}
		if res, _ := e.post("/ui/enroll/tokens/"+gone+"/delete", vals(), false); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s delete: %d", role, res.StatusCode)
		}
		if res, _ := e.post("/ui/devices/"+id+"/retire", vals("reason", "r", "confirm", "FRONTDESK-01"), false); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s retire: %d", role, res.StatusCode)
		}
		if e.t6Device(id).RetiredAt != nil {
			t.Fatalf("%s retired a device", role)
		}
	}
	for p, min := range M3T6Roles() {
		if min != "AD" {
			t.Errorf("%s: %s, §5.3 says AD", p, min)
		}
	}
}

// ---- B: the retire dialog ----

func TestM3T6B_TheRetireDialog(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	page := e.page("/devices/"+id, 200)
	mustContain(t, "dialog", page, `act-retire`, `action="/ui/devices/`+id+`/retire"`, `name="reason"`, `name="confirm"`, `name="final_backup"`, `name="replaced_by"`, "Retire device",
		"Deletes nothing", "not available in the console yet", "reads MISSING, or ONBOARDING while its onboarding window is open", "Uninstall the agent", "recurring RMM script",
		"at the next sync", "If that sync fails")
	if strings.Contains(page, "Revokes its gateway login and device token now") {
		t.Error("the dialog promises the login is gone at once: the sync can fail")
	}
	post := func(form url.Values) *http.Response { return e.postForm("/ui/devices/"+id+"/retire", form) }
	// The confirm step: without the name typed, nothing happens.
	if r := post(url.Values{"reason": {"dead"}, "confirm": {"WRONG"}}); !strings.Contains(e.location(r), "cmd=err_retire_confirm") || e.t6Device(id).RetiredAt != nil {
		t.Fatalf("wrong name: %q", e.location(r))
	}
	if r := post(url.Values{"reason": {" "}, "confirm": {"FRONTDESK-01"}}); !strings.Contains(e.location(r), "cmd=err_retire_reason") || e.t6Device(id).RetiredAt != nil {
		t.Fatalf("no reason: %q", e.location(r))
	}
	if r := post(url.Values{"reason": {`moved C:\Users\frank`}, "confirm": {"FRONTDESK-01"}}); !strings.Contains(e.location(r), "cmd=err_retire_reason") || e.t6Device(id).RetiredAt != nil {
		t.Fatalf("a path: %q", e.location(r))
	}
	if r := post(url.Values{"reason": {"x"}, "confirm": {"FRONTDESK-01"}, "replaced_by": {e.dev["LEGAL-PC-01"]}}); !strings.Contains(e.location(r), "cmd=err_retire_repl") || e.t6Device(id).RetiredAt != nil {
		t.Fatalf("another customer's replacement: %q", e.location(r))
	}
	// A refused retire comes back with the dialog open.
	mustContain(t, "reopened", e.page("/devices/"+id+"?retire=1&cmd=err_retire_reason", 200), `act-retire" open`, "A reason is required")

	// Final backup: queued, not retired, and the page says so.
	if r := post(url.Values{"reason": {"replaced"}, "confirm": {"FRONTDESK-01"}, "final_backup": {"1"}}); !strings.Contains(e.location(r), "cmd=final_backup_queued") || e.t6Device(id).RetiredAt != nil {
		t.Fatalf("final backup: %q retired=%v", e.location(r), e.t6Device(id).RetiredAt != nil)
	}
	mustContain(t, "final backup notice", e.page("/devices/"+id+"?cmd=final_backup_queued", 200), "The device is not retired yet")

	repl := e.dev["DR-OFFICE-PC"]
	// L7: "exactly" means exactly.
	if r := post(url.Values{"reason": {"dead lab machine"}, "confirm": {"frontdesk-01"}}); !strings.Contains(e.location(r), "cmd=err_retire_confirm") || e.t6Device(id).RetiredAt != nil {
		t.Fatalf("a name in the wrong case: %q", e.location(r))
	}
	r := post(url.Values{"reason": {"dead lab machine"}, "confirm": {"FRONTDESK-01"}, "replaced_by": {repl}})
	if !strings.Contains(e.location(r), "cmd=retired_ok") {
		t.Fatalf("retire: %q", e.location(r))
	}
	d := e.t6Device(id)
	if d.RetiredAt == nil || d.RetiredReason != "dead lab machine" || d.ReplacedByDeviceID != repl {
		t.Fatalf("after retire: %+v", d)
	}
	if n := len(e.auditRows("device.retire", id)); n != 1 {
		t.Fatalf("device.retire audit rows = %d, want 1", n)
	}
	after := e.page("/devices/"+id+"?cmd=retired_ok", 200)
	mustContain(t, "after", after, "Device retired.", "RETIRED", "Reason: dead lab machine.", "DR-OFFICE-PC", "Already retired.", "Its repository is kept", "Uninstall the agent", "recurring RMM script")
	if strings.Contains(after, "never seen") || strings.Contains(after, "history unavailable") {
		t.Error("the retired device's page reads as if data were missing: the engine loads no presence or strip for it, on purpose")
	}
	mustContain(t, "audit tab", e.page("/devices/"+id+"/audit", 200), "Device retired", "dead lab machine")
	if strings.Contains(after, `action="/ui/devices/`+id+`/retire"`) {
		t.Error("a retired device still offers the dialog")
	}
	// Retiring again answers the same state, with no second audit row and nothing left to confirm.
	if r := post(url.Values{}); !strings.Contains(e.location(r), "cmd=retired_ok") {
		t.Fatalf("second retire: %q", e.location(r))
	}
	if n := len(e.auditRows("device.retire", id)); n != 1 {
		t.Errorf("a repeated retire wrote %d audit rows", n)
	}
	if e.t6Device(id).RetiredReason != "dead lab machine" {
		t.Error("a repeated retire changed the reason")
	}
}

func TestM3T6B_RetireOffersNoDialogBelowTheAdminRole(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	e.signInAs("t6-op", "operator", nil)
	page := e.page("/devices/"+id, 200)
	mustContain(t, "operator", page, "Retire…", "Needs the admin role.")
	if strings.Contains(page, `action="/ui/devices/`+id+`/retire"`) {
		t.Error("an operator gets the retire form")
	}
}

// The Not seen page (B05) links each device to the same dialog and comes back.
func TestM3T6B_NotSeenLinksToTheRetireDialogAndComesBack(t *testing.T) {
	e := newEnv(t)
	e.login()
	page := e.page("/fleet/unseen", 200)
	id := e.dev["LAB-RETINA-02"]
	link := `/devices/` + id + `?retire=1&amp;from=unseen`
	mustContain(t, "not seen", page, link)
	// Below admin: the same row, and no link. (Checked here, while the device still has a row.)
	for _, role := range []string{"read_only", "operator"} {
		e.signInAs("t6-unseen-"+role, role, nil)
		p := e.page("/fleet/unseen", 200)
		mustContain(t, role, p, `id="dev-`+id+`"`)
		if strings.Contains(p, "retire=1") {
			t.Errorf("%s gets a Retire link", role)
		}
	}
	e.login()
	dialog := e.page("/devices/"+id+"?retire=1&from=unseen", 200)
	mustContain(t, "opened", dialog, `act-retire" open`, `name="return" value="unseen"`)
	r := e.postForm("/ui/devices/"+id+"/retire", url.Values{"reason": {"gone"}, "confirm": {"LAB-RETINA-02"}, "return": {"unseen"}})
	if !strings.HasPrefix(e.location(r), "/fleet/unseen") || !strings.Contains(e.location(r), "cmd=retired_ok") {
		t.Fatalf("return: %q", e.location(r))
	}
	back := e.page(e.location(r), 200)
	mustContain(t, "notice", back, "Device retired.", "Retired (")
	if strings.Contains(back, `id="dev-`+id+`"`) {
		t.Error("the retired device is still a row of Not seen: it must raise no alarm")
	}
	// A refused retire from the list keeps the way back.
	other := e.dev["FRONTDESK-01"]
	r = e.postForm("/ui/devices/"+other+"/retire", url.Values{"reason": {"gone"}, "confirm": {"WRONG"}, "return": {"unseen"}})
	if loc := e.location(r); !strings.Contains(loc, "retire=1") || !strings.Contains(loc, "from=unseen") {
		t.Errorf("refused from the list: %q", loc)
	}
}

// ---- Retired (n) on every list ----

func TestM3T6B_RetiredDevicesCollapseUnderRetiredNOnEveryList(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.retiredDevice()
	summit := e.customerID("Summit Eye Care")
	row := `id="dev-` + id + `"`
	// The token the retired device enrolled with.
	tok := e.t6Token(summit, "the retired device's token", false)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET enrollment_token_id = ? WHERE id = ?`, tok, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"board":         "/",
		"devices":       "/devices",
		"customer tab":  "/customers/" + summit + "/devices",
		"unseen":        "/fleet/unseen",
		"overdue":       "/fleet/overdue",
		"activity":      "/activity",
		"token devices": "/enroll/tokens/" + tok,
	} {
		page := e.page(path, 200)
		mustContain(t, name, page, `class="retired-fold"`, "Retired (1)", `id="retired-`+id+`"`, "RETIRED")
		if strings.Contains(page, row) {
			t.Errorf("%s: the retired device is an ordinary row, not under Retired (n)", name)
		}
	}
	// The fold is collapsed: a details element with no open attribute.
	if p := e.page("/devices", 200); strings.Contains(p, `class="retired-fold" open`) {
		t.Error("the fold starts open")
	}
	// Include retired puts them in the table, and the fold steps aside.
	inc := e.page("/devices?retired=1", 200)
	mustContain(t, "include retired", inc, row)
	if strings.Contains(inc, `class="retired-fold"`) {
		t.Error("the device is both a row and under Retired (n)")
	}
	// A search narrows the fold with the table.
	if p := e.page("/devices?q=DOESNOTMATCH", 200); strings.Contains(p, `class="retired-fold"`) {
		t.Error("a search that matches nothing still lists the retired device")
	}
	// Another customer's list says nothing about it.
	if p := e.page("/customers/"+e.customerID("Northwind Dental")+"/devices", 200); strings.Contains(p, `id="retired-`+id+`"`) {
		t.Error("another customer's table lists it")
	}
	if p := e.page("/fleet/unseen?customer="+e.customerID("Northwind Dental"), 200); strings.Contains(p, `id="retired-`+id+`"`) {
		t.Error("another customer's Not seen lists it")
	}
}

// M1, on the page: a sync that has not passed since the retire reads pending, in the
// notice and on the device page, and is never called revoked.
func TestM3T6B_ASyncThatDidNotPassReadsPending(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.syncDown = true
	e.svc.m3.api.GatewaySyncWait = 60 * time.Millisecond
	e.now = e.now.Add(time.Minute) // the fixtures' passing run is now from before the retire
	id := e.dev["FRONTDESK-01"]
	r := e.postForm("/ui/devices/"+id+"/retire", url.Values{"reason": {"compromised"}, "confirm": {"FRONTDESK-01"}})
	if !strings.Contains(e.location(r), "cmd=retired_pending") || !strings.Contains(e.location(r), "why=no_sync_since_retire") {
		t.Fatalf("went to %q", e.location(r))
	}
	page := e.page(e.location(r), 200)
	mustContain(t, "pending", page, "Device retired, but its gateway login is not removed.", "the gateway may still accept this device&#39;s login until the sync succeeds", "Reason: no_sync_since_retire.")
	if strings.Contains(page, "Its device token and gateway login are revoked") {
		t.Error("the page says the login is revoked")
	}
	// A failed run after the retire: the page names its class, critical.
	e.now = e.now.Add(time.Minute)
	if err := e.st.RecordJobRun(context.Background(), store.JobHtpasswdSync, store.JobRun{Status: "error", ErrorClass: "io", StartedAt: e.now, FinishedAt: e.now}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "failed run", e.page("/devices/"+id, 200), "gateway login not removed", "until the sync succeeds (reason: io)", `class="banner crit dense"`)
	// A passing run clears it, by itself.
	e.now = e.now.Add(time.Minute)
	if err := e.st.RecordJobRun(context.Background(), store.JobHtpasswdSync, store.JobRun{Status: "ok", StartedAt: e.now, FinishedAt: e.now}); err != nil {
		t.Fatal(err)
	}
	cleared := e.page("/devices/"+id, 200)
	mustContain(t, "cleared", cleared, "Its device token and gateway login are revoked")
	if strings.Contains(cleared, "gateway login not removed") {
		t.Error("still pending after a passing run")
	}
}

// L4: no runs on a retired device is not an alarm, and the page does not call it one.
func TestM3T6B_ARetiredDeviceWithNoRunsIsNotCalledAnAlarm(t *testing.T) {
	e := newEnv(t)
	e.login()
	ctx := context.Background()
	id := ""
	for host, dev := range e.dev {
		runs, err := e.st.ListRuns(ctx, store.ScopeAll(), dev, store.Page{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(runs.Items) == 0 && host != "OLD-RECEPTION-PC" {
			id = dev
			mustContain(t, "active with no runs", e.page("/devices/"+id, 200), "No runs recorded yet.") // the wording it replaces
			if r := e.postForm("/ui/devices/"+id+"/retire", url.Values{"reason": {"never ran"}, "confirm": {host}}); !strings.Contains(e.location(r), "cmd=retired_ok") {
				t.Fatalf("retire: %q", e.location(r))
			}
			break
		}
	}
	if id == "" {
		t.Fatal("the fixtures have no active device without runs")
	}
	page := e.page("/devices/"+id, 200)
	mustContain(t, "retired with no runs", page, "this raises no alarm")
	if strings.Contains(page, "is an alarm") {
		t.Error("the retired device's page calls it an alarm")
	}
}

// L6: only not-found is 404. A store that cannot answer fails loudly and changes nothing.
func TestM3T6B_AStoreErrorIsNotCalledNotFound(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	if res, _ := e.post("/ui/devices/01J9Y4K3A0B1C2D3E4F5G6H7ZZ/retire", vals("reason", "r", "confirm", "x"), false); res.StatusCode != http.StatusNotFound {
		t.Errorf("a device that does not exist: %d", res.StatusCode)
	}
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE devices RENAME COLUMN notes TO notes_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, body := e.post("/ui/devices/"+id+"/retire", vals("reason", "r", "confirm", "FRONTDESK-01"), false)
	if res.StatusCode == http.StatusNotFound || res.StatusCode < 500 {
		t.Fatalf("a read failure answered %d", res.StatusCode)
	}
	mustContain(t, "failure", body, "Not retired") // the page around it needs the same store, so it is the title alone
}

// layoutProbe measures the page as laid out: sideways overflow, and every visible
// action button against the viewport.
const layoutProbe = `
var out = {iw: innerWidth, overflow: document.documentElement.scrollWidth - innerWidth, outside: 0, buttons: 0};
document.querySelectorAll('.btn').forEach(function (b) {
  var r = b.getBoundingClientRect();
  if (r.width === 0 || r.height === 0 || b.closest('.tip') || !b.checkVisibility()) return; // a closed details keeps its boxes in Chrome
  out.buttons++;
  if (r.left < -0.5 || r.right > innerWidth + 0.5) out.outside++;
});
return out;`

// Rendered in a real browser, at both widths, dark and light (the ticket's list):
// nothing overflows the page, and every action button sits inside the viewport,
// including the open retire dialog's. The mutant proves the probe sees the fault.
func TestM3T6_LayoutAtBothWidthsAndThemes(t *testing.T) {
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	e.t6Token(summit, "a revoked token with quite a long label for the phone", true)
	css := e.appCSS(t)
	pages := map[string]string{
		"table":    "/enroll",
		"wizard":   "/enroll/new",
		"not_seen": "/fleet/unseen",
		"retire":   "/devices/" + e.dev["FRONTDESK-01"] + "?retire=1",
		"devices":  "/devices",
	}
	for name, path := range pages {
		for _, w := range []int{1360, 390} {
			for _, theme := range []string{"dark", "light"} {
				t.Run(fmt.Sprintf("%s_%d_%s", name, w, theme), func(t *testing.T) {
					got := e.layout(t, layoutOpts{Path: path, CSS: css, W: w, H: 900,
						Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `');` + layoutProbe})
					if got["buttons"] == 0 {
						t.Fatalf("no action button measured on %s", path)
					}
					if got["overflow"] > 0.5 {
						t.Errorf("the page overflows sideways by %.0f px", got["overflow"])
					}
					if got["outside"] > 0 {
						t.Errorf("%.0f action buttons sit outside the viewport", got["outside"])
					}
				})
			}
		}
	}
	i := strings.Index(css, "/* ===== M3-T6:")
	if i < 0 {
		t.Fatal("the M3-T6 block is not where the test expects it")
	}
	bad := e.layout(t, layoutOpts{Path: "/enroll", CSS: css[:i], W: 390, H: 900, Probe: layoutProbe})
	if bad["outside"] == 0 {
		t.Error("the layout test cannot tell a token table whose buttons run off a phone from one whose buttons fit")
	}
}
