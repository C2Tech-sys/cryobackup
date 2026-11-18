package ui

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ADR 0019 in the pages (P19-T5): the purge dialog of a retired device, its fresh-MFA step, the purging
// state, the PURGE NOT FINISHED banner, and the customer delete. OLD-RECEPTION-PC is a retired fixture
// device that is not purging; SPARE-PC-09 is purging and its last attempt failed (storage unreachable);
// Cedar Valley Veterinary has no devices.

const adr19Host = "OLD-RECEPTION-PC"

// adr19Purging reports whether the device is purging; a device that is gone reads as not found.
func (e *env) adr19Device(id string) (store.Device, bool) {
	e.t.Helper()
	d, err := e.st.GetDevice(context.Background(), store.ScopeAll(), id)
	if err != nil {
		return store.Device{}, false
	}
	return d, true
}

// adr19RetireAllOf retires every active device of a fixture customer, straight in the store, and
// returns the customer's id.
func (e *env) adr19RetireAllOf(name string) string {
	e.t.Helper()
	ctx := context.Background()
	id := e.customerID(name)
	a, err := e.st.GetAdminByUsername(ctx, "admin")
	if err != nil {
		e.t.Fatal(err)
	}
	l, err := e.st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{CustomerID: id, IncludeRetired: true, Page: store.Page{Limit: 100}})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, d := range l.Items {
		if d.RetiredAt != nil {
			continue
		}
		if _, err := e.st.RetireDeviceAudited(ctx, store.ScopeAll(), d.ID, store.RetireRequest{Reason: "customer left"}, store.AuditActor{AdminID: a.ID, Username: a.Username, Method: "web"}); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

// purgeFormOf cuts the purge form out of a page, so markers are checked inside the dialog.
func purgeFormOf(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<form method="post" action="[^"]*/(?:purge|delete|purge-retired)" class="stack" data-purge.*?</form>`).FindString(page)
	if m == "" {
		t.Fatal("no purge or delete dialog on the page")
	}
	return m
}

func TestADR19_PurgeDialogIsForOwnersOfRetiredDevicesOnly(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev[adr19Host]
	page := e.page("/devices/"+id, 200)
	form := purgeFormOf(t, page)
	mustContain(t, "owner, retired device", page, `act-purge`, "Purge permanently…")
	mustContain(t, "dialog", form, `action="/ui/devices/`+id+`/purge"`, `name="reason"`, `name="confirm"`, `data-purge-name="`+adr19Host+`"`,
		"Purge is one-way", "Type the device name to continue",
		// the three statements of ADR 0019, in plain words
		"Deletes this device's backups from storage, and its escrowed repository key with them. They cannot be recovered or restored afterwards, by anyone.",
		"versioning, object lock or a minimum storage duration may keep, and bill, the deleted data",
		"An agent still installed on the machine stays REFUSED: uninstall it there.",
		// a fresh session needs no code, and says until when
		"no code is needed until")
	if strings.Contains(form, `name="code"`) {
		t.Error("a session that just passed MFA is asked for a code")
	}
	if strings.Index(form, "stays REFUSED") > strings.Index(form, `name="confirm"`) {
		t.Error("the statements come after the confirm field")
	}
	// An active device offers no purge.
	if p := e.page("/devices/"+e.dev["FRONTDESK-01"], 200); strings.Contains(p, "/purge\"") {
		t.Error("an active device offers the purge dialog")
	}
	// Below the owner role the control is not there at all.
	for _, role := range []string{"admin", "operator", "read_only"} {
		e.signInAs("p19-"+role, role, nil)
		p := e.page("/devices/"+id, 200)
		if strings.Contains(p, "/purge\"") || strings.Contains(p, "Purge permanently") {
			t.Errorf("%s sees the purge control", role)
		}
	}
}

func TestADR19_APurgeFromANonOwnerIsRefusedServerSide(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev[adr19Host]
	cedar := e.customerID("Cedar Valley Veterinary")
	for _, role := range []string{"admin", "operator", "read_only"} {
		e.signInAs("p19f-"+role, role, nil)
		// the forged form carries everything a valid one does
		if res, _ := e.post("/ui/devices/"+id+"/purge", vals("reason", "forged", "confirm", adr19Host), false); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s purge: %d", role, res.StatusCode)
		}
		if res, _ := e.post("/ui/customers/"+cedar+"/delete", vals("reason", "forged", "confirm", "Cedar Valley Veterinary"), false); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s customer delete: %d", role, res.StatusCode)
		}
		if res, _ := e.post("/ui/customers/"+cedar+"/purge-retired", vals("reason", "forged", "confirm", "Cedar Valley Veterinary"), false); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s purge retired: %d", role, res.StatusCode)
		}
	}
	if d, _ := e.adr19Device(id); d.Purging() {
		t.Fatal("a non-owner purged a device")
	}
	if n := len(e.auditRows("device.purge.request", id)); n != 0 {
		t.Fatalf("refused purges wrote %d audit rows", n)
	}
	if _, err := e.st.GetCustomer(context.Background(), store.ScopeAll(), cedar); err != nil {
		t.Fatalf("a non-owner deleted a customer: %v", err)
	}
	for p, min := range PurgeUIRoles() {
		if min != "OW" {
			t.Errorf("%s: %s, ADR 0019 says owner", p, min)
		}
	}
}

func TestADR19_ANameThatDoesNotMatchExactlyPurgesNothing(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev[adr19Host]
	for _, typed := range []string{"old-reception-pc", adr19Host + " ", "", "FRONTDESK-01"} {
		res, body := e.post("/ui/devices/"+id+"/purge", vals("reason", "lab machine decommissioned", "confirm", typed), false)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("typed %q: %d", typed, res.StatusCode)
		}
		form := purgeFormOf(t, body)
		mustContain(t, "mismatch "+typed, body, `act-purge" open`)
		mustContain(t, "mismatch "+typed, form, "Not purged.", "name exactly as shown, including its case.", `value="lab machine decommissioned"`, `aria-invalid="true"`)
		if d, _ := e.adr19Device(id); d.Purging() {
			t.Fatalf("typed %q purged the device", typed)
		}
	}
	// No reason: refused, the name kept.
	res, body := e.post("/ui/devices/"+id+"/purge", vals("reason", " ", "confirm", adr19Host), false)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("no reason: %d", res.StatusCode)
	}
	mustContain(t, "no reason", purgeFormOf(t, body), "A reason is required", `value="`+adr19Host+`"`)
	if n := len(e.auditRows("device.purge.request", id)); n != 0 {
		t.Fatalf("refused purges wrote %d audit rows", n)
	}
}

func TestADR19_StaleMFAAsksForACodeAndKeepsWhatWasTyped(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev[adr19Host]
	e.advance(auth.FreshMFAWindow + time.Minute)
	form := purgeFormOf(t, e.page("/devices/"+id, 200))
	mustContain(t, "stale dialog", form, `name="code"`, `autocomplete="one-time-code"`, "Authenticator code", "A recovery code is not accepted here")

	// A forged form without the code field: the server refuses it, purges nothing, and asks for the code.
	res, body := e.post("/ui/devices/"+id+"/purge", vals("reason", "lab machine decommissioned", "confirm", adr19Host), false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("stale MFA, no code: %d", res.StatusCode)
	}
	f := purgeFormOf(t, body)
	mustContain(t, "stale refusal", f, "A recent MFA check is required.", `name="code"`, `value="lab machine decommissioned"`, `value="`+adr19Host+`"`)
	if d, _ := e.adr19Device(id); d.Purging() {
		t.Fatal("a stale session purged a device")
	}
	// A wrong code: the existing sentence, nothing purged, the code not echoed.
	res, body = e.post("/ui/devices/"+id+"/purge", vals("reason", "lab machine decommissioned", "confirm", adr19Host, "code", "000000"), false)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", res.StatusCode)
	}
	f = purgeFormOf(t, body)
	mustContain(t, "wrong code", f, "Code incorrect or expired.", `value="lab machine decommissioned"`)
	if strings.Contains(f, `value="000000"`) {
		t.Error("the code is echoed back into the form")
	}
	if d, _ := e.adr19Device(id); d.Purging() {
		t.Fatal("a wrong code purged a device")
	}
	// The right code: one post steps up and purges.
	res, _ = e.post("/ui/devices/"+id+"/purge", vals("reason", "lab machine decommissioned", "confirm", adr19Host, "code", e.mfa.code("admin")), false)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(res), "cmd=purge_requested") {
		t.Fatalf("right code: %d %q", res.StatusCode, e.location(res))
	}
	if d, _ := e.adr19Device(id); !d.Purging() || d.PurgeReason != "lab machine decommissioned" {
		t.Fatalf("after the step-up the device is not purging: %+v", d)
	}
	if n := len(e.auditRows("device.purge.request", id)); n != 1 {
		t.Errorf("device.purge.request rows = %d, want 1", n)
	}
	ok := 0
	for _, ev := range e.auditRows(auth.ActionStepUp, "") {
		if ev.Result == "ok" {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("successful step-ups audited = %d, want 1", ok)
	}
}

func TestADR19_APurgeShowsPurgingUntilTheRowIsGone(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev[adr19Host]
	res, _ := e.post("/ui/devices/"+id+"/purge", vals("reason", "lab machine decommissioned", "confirm", adr19Host), false)
	if loc := e.location(res); res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "cmd=purge_requested") {
		t.Fatalf("purge: %d %q", res.StatusCode, loc)
	}
	page := e.page("/devices/"+id+"?cmd=purge_requested", 200)
	mustContain(t, "purging page", page, "Purge started.", "PURGING", "purge-state", "Requested", "</time> by ", "lab machine decommissioned",
		"None finished yet.", "It retries by itself", "This device is being purged.", "No action is offered while the console removes this device")
	if strings.Contains(page, `action="/ui/devices/`+id+`/purge"`) || strings.Contains(page, `action="/ui/devices/`+id+`/retire"`) || strings.Contains(page, "/backup-now\"") {
		t.Error("a purging device still offers an action")
	}
	board := e.page("/", 200)
	row := regexp.MustCompile(`(?s)<li id="retired-` + id + `">.*?</li>`).FindString(board)
	if !strings.Contains(row, "PURGING") {
		t.Errorf("the board's retired row does not say purging: %q", row)
	}
	mustContain(t, "board fold", board, "· 1 purging")
	// A second request changes nothing.
	res, _ = e.post("/ui/devices/"+id+"/purge", vals("reason", "again", "confirm", adr19Host), false)
	if !strings.Contains(e.location(res), "cmd=purge_already") {
		t.Fatalf("second purge: %q", e.location(res))
	}
	if n := len(e.auditRows("device.purge.request", id)); n != 1 {
		t.Errorf("device.purge.request rows = %d, want 1", n)
	}
	// The job finishes: the row is gone and the page is the normal not-found page.
	if err := e.st.CompleteDevicePurge(context.Background(), store.ScopeAll(), id, 10, 1000); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "after", e.page("/devices/"+id, http.StatusNotFound), "Not found", "This device was retired, removed, or never existed")
}

func TestADR19_AFailingPurgeSaysWhyInWordsAndRetries(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["SPARE-PC-09"]
	page := e.page("/devices/"+id, 200)
	mustContain(t, "failing", page, "Purge not finished.", "The storage target could not be reached.", "storage_unreachable", "Last attempt", "Last error",
		"It retries by itself", "PURGING")
	if !strings.Contains(page, `class="banner crit purge-state" role="alert"`) {
		t.Error("a failing purge is not a critical statement")
	}
	board := e.page("/", 200)
	if row := regexp.MustCompile(`(?s)<li id="retired-` + id + `">.*?</li>`).FindString(board); !strings.Contains(row, "PURGE FAILING") {
		t.Errorf("the board's retired row does not say the purge is failing: %q", row)
	}
	mustContain(t, "board fold", board, "1 purge failing")
	// Words for every class the job records.
	for _, c := range []string{"worker_unreachable", "objects_remaining", "bad_result", "artifact_cleanup", "complete_failed", "not_wired", "not_recorded", "timeout", "storage_unreachable", "restic_failed"} {
		if w := purgeClassText(c); w == "" || w == "The attempt failed." {
			t.Errorf("class %s has no words", c)
		}
	}
}

func TestADR19_PurgeNotFinishedBannerHasALabelAndLinksToTheDevice(t *testing.T) {
	if got := BannerLabel(health.BannerPurgeNotFinished); got != "PURGE NOT FINISHED" {
		t.Fatalf("label %q", got)
	}
	v := bannerView(state.Banner{ID: health.BannerPurgeNotFinished, Subject: "01JDEV0000000000000000ABCD", Severity: state.SeverityCritical, Text: "Purge not finished: 1 device."})
	if v.Label != "PURGE NOT FINISHED" || v.Class != ClassCrit || v.Action == nil || v.Action.Href != "/devices/01JDEV0000000000000000ABCD" || v.Action.Admin {
		t.Fatalf("banner view: %+v %+v", v, v.Action)
	}
	if v := bannerView(state.Banner{ID: health.BannerPurgeNotFinished, Subject: "../system", Severity: state.SeverityCritical}); v.Action != nil && strings.Contains(v.Action.Href, "..") {
		t.Errorf("a subject that is not an id became a link: %+v", v.Action)
	}
	if v := bannerView(state.Banner{ID: health.BannerPurgeNotFinished, Severity: state.SeverityCritical}); v.Action != nil {
		t.Errorf("a banner that names no device links somewhere: %+v", v.Action)
	}
	e := newEnv(t)
	mustContain(t, "/design", e.page("/design", 200), "PURGE NOT FINISHED")
}

func TestADR19_ACustomerWithDevicesCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	page := e.page("/customers/"+summit, 200)
	mustContain(t, "summit", page, "Delete customer", "Not available while Summit Eye Care has devices", "active</b>", "retired</b>")
	if strings.Contains(page, `action="/ui/customers/`+summit+`/delete"`) {
		t.Error("a customer with devices offers the delete form")
	}
	// A forged delete reaches the API, which refuses it with the counts; nothing is deleted.
	res, _ := e.post("/ui/customers/"+summit+"/delete", vals("reason", "forged", "confirm", "Summit Eye Care"), false)
	if loc := e.location(res); res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "cdel=err_cust_devices") {
		t.Fatalf("delete with devices: %d %q", res.StatusCode, loc)
	}
	if _, err := e.st.GetCustomer(context.Background(), store.ScopeAll(), summit); err != nil {
		t.Fatalf("the customer is gone: %v", err)
	}
	mustContain(t, "refusal", e.page("/customers/"+summit+"?cdel=err_cust_devices", 200), "Customer not deleted.", "It still has devices")
	// Below the owner role the section is not there.
	e.signInAs("p19c-admin", "admin", nil)
	if p := e.page("/customers/"+summit, 200); strings.Contains(p, "Delete customer") {
		t.Error("an admin sees the delete section")
	}
}

func TestADR19_DeleteAnEmptyCustomer(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.customerID("Cedar Valley Veterinary")
	form := purgeFormOf(t, e.page("/customers/"+id, 200))
	mustContain(t, "delete dialog", form, `action="/ui/customers/`+id+`/delete"`, "Delete Cedar Valley Veterinary?", "its enrollment tokens and its own policies",
		"This cannot be undone", `data-purge-name="Cedar Valley Veterinary"`, "Type the customer name to continue")
	res, body := e.post("/ui/customers/"+id+"/delete", vals("reason", "never signed", "confirm", "cedar valley veterinary"), false)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatch: %d", res.StatusCode)
	}
	mustContain(t, "mismatch", purgeFormOf(t, body), "Customer not deleted.", "name exactly as shown", `value="never signed"`)
	res, _ = e.post("/ui/customers/"+id+"/delete", vals("reason", "never signed", "confirm", "Cedar Valley Veterinary"), false)
	if loc := e.location(res); res.StatusCode != http.StatusSeeOther || loc != "/customers?deleted=1" {
		t.Fatalf("delete: %d %q", res.StatusCode, loc)
	}
	if _, err := e.st.GetCustomer(context.Background(), store.ScopeAll(), id); err == nil {
		t.Fatal("the customer still exists")
	}
	if n := len(e.auditRows("customer.delete", id)); n != 1 {
		t.Errorf("customer.delete rows = %d, want 1", n)
	}
	mustContain(t, "list", e.page("/customers?deleted=1", 200), "Customer deleted.", "Its enrollment tokens and own policies were deleted with it")
	e.page("/customers/"+id, http.StatusNotFound)
}

func TestADR19_PurgeEveryRetiredDeviceOfACustomer(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.adr19RetireAllOf("Northwind Dental") // 3 retired now, plus SPARE-PC-09 already purging
	page := e.page("/customers/"+id, 200)
	form := purgeFormOf(t, page)
	mustContain(t, "only retired", page, "Purge all retired devices of this customer…", "1 is already being purged")
	mustContain(t, "purge-all dialog", form, "Purge 3 retired devices of Northwind Dental permanently?", "cannot be recovered or restored afterwards, by anyone",
		"versioning, object lock or a minimum storage duration", "stays REFUSED", `data-purge-name="Northwind Dental"`)
	e.advance(auth.FreshMFAWindow + time.Minute) // the same MFA step
	res, body := e.post("/ui/customers/"+id+"/purge-retired", vals("reason", "customer left", "confirm", "Northwind Dental"), false)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "A recent MFA check is required.") {
		t.Fatalf("stale: %d", res.StatusCode)
	}
	res, _ = e.post("/ui/customers/"+id+"/purge-retired", vals("reason", "customer left", "confirm", "Northwind Dental", "code", e.mfa.code("admin")), false)
	loc := e.location(res)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "cdel=cpurge_queued") || !strings.Contains(loc, "n=3") || !strings.Contains(loc, "already=1") {
		t.Fatalf("purge retired: %d %q", res.StatusCode, loc)
	}
	after := e.page(loc, 200)
	mustContain(t, "after", after, "Purge started.", "3 retired devices are being purged", "1 other was already purging", "are being purged. Delete the customer once")
	l, _ := e.st.ListDevicesPurging(context.Background(), store.ScopeAll())
	if len(l) != 4 {
		t.Errorf("purging devices = %d, want 4", len(l))
	}
}

// No host name, customer name, reason or code in any log line of these flows. The refused-audit
// pass makes sure the capture is wired: it must log.
func TestADR19_NoHostNameInAnyLogLine(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	lg := logx.New(&logs, logx.LevelDebug)
	e.svc.Log, e.svc.m3.api.Log = lg, lg
	e.login()
	id := e.dev[adr19Host]
	cedar := e.customerID("Cedar Valley Veterinary")
	e.post("/ui/devices/"+id+"/purge", vals("reason", "secret reason text", "confirm", "WRONG-NAME"), false)
	e.mutRefuseAudits(true) // a refused audit write is logged (by class and status): proves the capture
	e.post("/ui/devices/"+id+"/purge", vals("reason", "secret reason text", "confirm", adr19Host), false)
	e.post("/ui/customers/"+cedar+"/delete", vals("reason", "secret reason text", "confirm", "Cedar Valley Veterinary"), false)
	e.mutRefuseAudits(false)
	e.advance(auth.FreshMFAWindow + time.Minute)
	e.post("/ui/devices/"+id+"/purge", vals("reason", "secret reason text", "confirm", adr19Host, "code", "123456"), false)
	e.post("/ui/devices/"+id+"/purge", vals("reason", "secret reason text", "confirm", adr19Host, "code", e.mfa.code("admin")), false)
	e.page("/devices/"+id, 200)
	e.page("/", 200)
	out := logs.String()
	if out == "" {
		t.Fatal("nothing was logged: the capture is not wired, so this test proves nothing")
	}
	for _, bad := range []string{adr19Host, "WRONG-NAME", "Cedar Valley", "secret reason", "123456"} {
		if strings.Contains(out, bad) {
			t.Errorf("a log line carries %q:\n%s", bad, out)
		}
	}
}

func TestADR19_DesignPageShowsEveryState(t *testing.T) {
	e := newEnv(t)
	page := e.page("/design/purge", 200)
	mustContain(t, "/design/purge", page, `id="purge"`,
		"Idle: MFA fresh", "MFA needed", "Submitting", `aria-busy="true"`, "Name does not match", "Reason missing", "Wrong or expired code", "Too many attempts",
		"MFA went stale", "Purge every retired device of a customer", "Delete a customer with no devices",
		"purge_requested", "Purge started.", "Purging (no attempt finished yet)", "Purge failing", "Purge not finished (nothing finished for hours)",
		"PURGE NOT FINISHED", "Open the device", "PURGE FAILING", "purge state not read",
		"Not available while Summit Eye Care has devices", "Purge all retired devices of this customer…", "are being purged", "Example Empty has no devices")
	ids := regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(page, -1)
	seen := map[string]bool{}
	for _, m := range ids {
		if seen[m[1]] {
			t.Errorf("element id %q is used twice on /design/purge", m[1])
		}
		seen[m[1]] = true
	}
	mustContain(t, "/design nav", e.page("/design", 200), `href="/design/purge"`)
}

// Rendered in a real browser at both widths and themes: the open dialogs, the purging page, the
// customer section and /design/purge do not overflow, and every visible button is inside the viewport.
func TestADR19_LayoutAtBothWidthsAndThemes(t *testing.T) {
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	css := e.appCSS(t)
	northwind := e.adr19RetireAllOf("Northwind Dental")
	pages := map[string]string{
		"purge_dialog":  "/devices/" + e.dev[adr19Host] + "?purge=1",
		"purging":       "/devices/" + e.dev["SPARE-PC-09"],
		"delete_dialog": "/customers/" + e.customerID("Cedar Valley Veterinary") + "?delete=1",
		"purge_all":     "/customers/" + northwind + "?purge=1",
		"has_devices":   "/customers/" + e.customerID("Summit Eye Care"),
		"design":        "/design/purge",
	}
	for name, path := range pages {
		for _, w := range []int{1360, 390} {
			for _, theme := range []string{"dark", "light"} {
				t.Run(fmt.Sprintf("%s_%d_%s", name, w, theme), func(t *testing.T) {
					got := e.layout(t, layoutOpts{Path: path, CSS: css, W: w, H: 900, Scripts: []string{"purge.js"},
						Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `');` + layoutProbe})
					if got["buttons"] == 0 {
						t.Fatalf("no button measured on %s", path)
					}
					if got["overflow"] > 0.5 {
						t.Errorf("the page overflows sideways by %.0f px", got["overflow"])
					}
					if got["outside"] > 0 {
						t.Errorf("%.0f buttons sit outside the viewport", got["outside"])
					}
				})
			}
		}
	}
	// The mutant: without this ticket's CSS block the open dialog runs off a phone, and the probe sees it.
	i := strings.Index(css, "/* ===== ADR 0019 (P19-T5):")
	if i < 0 {
		t.Fatal("the ADR 0019 block is not where the test expects it")
	}
	bad := e.layout(t, layoutOpts{Path: pages["purge_dialog"], CSS: css[:i], W: 390, H: 900, Probe: layoutProbe})
	if bad["outside"] == 0 && bad["overflow"] <= 0.5 {
		t.Error("the layout test cannot tell a purge dialog that runs off a phone from one that fits")
	}
}

// F2: when the audit row of the step-up cannot be written, nothing changed, and the page says so
// ("not saved, nothing was changed"), not "Code incorrect or expired."
func TestADR19_StepUpAuditFailureSaysNotSavedNotWrongCode(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev[adr19Host]
	e.advance(auth.FreshMFAWindow + time.Minute)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, body := e.post("/ui/devices/"+id+"/purge", vals("reason", "lab machine decommissioned", "confirm", adr19Host, "code", e.mfa.code("admin")), false)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("audit failure: %d, want 500", res.StatusCode)
	}
	mustContain(t, "not saved", body, "Not saved", "nothing was changed")
	if strings.Contains(body, "Code incorrect or expired.") {
		t.Error("an audit failure reads as a wrong code")
	}
	if d, _ := e.adr19Device(id); d.Purging() {
		t.Fatal("a failed step-up purged a device")
	}
}
