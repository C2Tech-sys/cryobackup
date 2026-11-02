package ui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// ---- helpers (M2-W2C) ----

func (e *env) commandsOf(dev string) []store.Command {
	e.t.Helper()
	l, err := e.st.ListCommands(context.Background(), store.ScopeAll(), dev, store.Page{Limit: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func (e *env) commandsOfType(dev, typ string) []store.Command {
	var out []store.Command
	for _, c := range e.commandsOf(dev) {
		if c.Type == typ {
			out = append(out, c)
		}
	}
	return out
}

// postTo posts a form as the signed-in user and returns the redirect target.
func (e *env) postTo(path string, form url.Values, wantStatus int) string {
	e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	res, body := e.post(path, form, false)
	if res.StatusCode != wantStatus {
		e.t.Fatalf("POST %s: want %d, got %d\n%s", path, wantStatus, res.StatusCode, body[:min(len(body), 300)])
	}
	return res.Header.Get("Location")
}

func (e *env) wakes() (*[]string, *sync.Mutex) {
	var mu sync.Mutex
	var got []string
	e.svc.WakeAgent = func(id string) { mu.Lock(); got = append(got, id); mu.Unlock() }
	return &got, &mu
}

func TestCommandPagesRenderInFixturesMode(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, host := range []string{"FRONTDESK-01", "DR-OFFICE-PC", "LAB-RETINA-02", "FRONTDESK-02", "XRAY-OLD-01", "OLD-RECEPTION-PC", "EXAM-ROOM-3", "NEW-EXAM-6"} {
		id := e.dev[host]
		for _, path := range []string{"/devices/" + id, "/devices/" + id + "/commands", "/devices/" + id + "/commands?status=expired", "/devices/" + id + "/commands?status=queued", "/devices/" + id + "/audit", "/ui/devices/" + id + "/actions"} {
			body := e.page(path, 200)
			if strings.Contains(body, "ZgotmplZ") {
				t.Errorf("%s %s: html/template rejected a value", host, path)
			}
			if absoluteURL.MatchString(body) {
				t.Errorf("%s %s: an absolute URL reference", host, path)
			}
			if m := pathLike.FindString(body); m != "" {
				t.Errorf("%s %s: path-like string %q", host, path, m)
			}
		}
	}
}

func TestActionMenuReflectsTheDevice(t *testing.T) {
	e := newEnv(t)
	e.login()
	// An idle, online device: Back up now is a live button, with the online hint.
	body := e.page("/devices/"+e.dev["FRONTDESK-01"], 200)
	mustContain(t, "idle online", body, `id="dev-actions"`, `action="/ui/devices/`+e.dev["FRONTDESK-01"]+`/backup-now"`, ">Back up now</button>", "starts within seconds", ">Refresh policy</button>",
		">Check in now</button>", `href="/devices/`+e.dev["FRONTDESK-01"]+`/commands"`)
	// A device with an open run offers Cancel run (behind a confirmation), not Back up now.
	body = e.page("/devices/"+e.dev["DR-OFFICE-PC"], 200)
	mustContain(t, "running", body, "Cancel run…", `action="/ui/devices/`+e.dev["DR-OFFICE-PC"]+`/cancel-run"`, "Cancel this run? Files already uploaded are kept.")
	if strings.Contains(body, `/backup-now"`) {
		t.Error("a device with an open run must not offer Back up now")
	}
	// An unseen device: queued when offline, and the fixtures' queued request shows as in flight with Undo.
	body = e.page("/devices/"+e.dev["LAB-RETINA-02"], 200)
	mustContain(t, "unseen with a queued backup", body, "QUEUED", "Back up now queued", "Undo", `/commands/`)
	// A parked device cannot be backed up on demand, and says why.
	mustContain(t, "parked", e.page("/devices/"+e.dev["XRAY-OLD-01"], 200), "Resume first.")
	// A retired device offers nothing.
	body = e.page("/devices/"+e.dev["OLD-RECEPTION-PC"], 200)
	mustContain(t, "retired", body, "Retired.")
	if strings.Contains(body, `/backup-now"`) {
		t.Error("a retired device offers Back up now")
	}
	// The fragment route is the same menu without the page.
	frag := e.page("/ui/devices/"+e.dev["FRONTDESK-01"]+"/actions", 200)
	mustContain(t, "fragment", frag, `id="dev-actions"`, ">Back up now</button>")
	if strings.Contains(frag, "<html") {
		t.Error("the actions fragment is a whole page")
	}
}

func TestBackUpNowQueuesOnceAndShowsOnTheCommandsTab(t *testing.T) {
	e := newEnv(t)
	e.login()
	woke, mu := e.wakes()
	dev := e.dev["FRONTDESK-01"]
	before := e.auditTotal()
	loc := e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"form-key-1"}}, http.StatusSeeOther)
	if !strings.HasPrefix(loc, "/devices/"+dev+"/commands?") || !strings.Contains(loc, "cmd=queued") {
		t.Fatalf("redirect = %q", loc)
	}
	cmds := e.commandsOfType(dev, "backup_now")
	var mine []store.Command
	for _, c := range cmds {
		if c.IdempotencyKey == "form-key-1" {
			mine = append(mine, c)
		}
	}
	if len(mine) != 1 || mine[0].Status != "queued" {
		t.Fatalf("queued commands: %+v", cmds)
	}
	mu.Lock()
	if len(*woke) != 1 || (*woke)[0] != dev {
		t.Fatalf("the device's long poll was not woken: %v", *woke)
	}
	mu.Unlock()
	if rows := e.auditRows("command.queue", dev); len(rows) != 1 || e.auditTotal() != before+1 {
		t.Fatalf("audit rows for the request: %d (total %d -> %d)", len(rows), before, e.auditTotal())
	}
	body := e.page(loc, 200)
	mustContain(t, "commands tab after queueing", body, "Back up now queued.", `aria-current="page">Commands`, ">Back up now</td>", "QUEUED", "Waiting for the device to check in.",
		`data-fast-poll`, "Undo", "Back up now queued ")
	// The same request again (a double click with another key) adds nothing.
	loc2 := e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"form-key-2"}}, http.StatusSeeOther)
	if !strings.Contains(loc2, "cmd=coalesced") {
		t.Fatalf("second redirect = %q", loc2)
	}
	if n := len(e.auditRows("command.queue", dev)); n != 1 {
		t.Fatalf("audit rows after the coalesced request = %d, want 1", n)
	}
	mustContain(t, "coalesced notice", e.page(loc2, 200), "Already queued.")
	// The audit tab reads it as a sentence.
	mustContain(t, "audit tab", e.page("/devices/"+dev+"/audit", 200), "Command queued", "Back up now")
}

func TestUndoCancelsAQueuedCommandOnceAndOnlyBeforeDelivery(t *testing.T) {
	e := newEnv(t)
	e.login()
	dev := e.dev["FRONTDESK-01"]
	e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"k-undo"}}, http.StatusSeeOther)
	var id string
	for _, c := range e.commandsOfType(dev, "backup_now") {
		if c.IdempotencyKey == "k-undo" {
			id = c.ID
		}
	}
	loc := e.postTo("/ui/devices/"+dev+"/commands/"+id+"/cancel", nil, http.StatusSeeOther)
	if !strings.Contains(loc, "cmd=cancelled") {
		t.Fatalf("redirect = %q", loc)
	}
	if rows := e.auditRows("command.cancel", dev); len(rows) != 1 {
		t.Fatalf("cancel audit rows = %d", len(rows))
	}
	mustContain(t, "after undo", e.page(loc, 200), "Cancelled.", "CANCELLED", "Withdrawn before the device received it.")
	// Again: idempotent, still one audit row.
	e.postTo("/ui/devices/"+dev+"/commands/"+id+"/cancel", nil, http.StatusSeeOther)
	if rows := e.auditRows("command.cancel", dev); len(rows) != 1 {
		t.Fatalf("a repeated undo wrote %d audit rows", len(rows))
	}
	// Once the device has the command it can no longer be undone.
	e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"k-delivered"}}, http.StatusSeeOther)
	delivered, err := e.st.AgentNextCommands(context.Background(), dev, 10)
	if err != nil || len(delivered) == 0 {
		t.Fatalf("delivery: %v %v", delivered, err)
	}
	loc = e.postTo("/ui/devices/"+dev+"/commands/"+delivered[0].ID+"/cancel", nil, http.StatusSeeOther)
	if !strings.Contains(loc, "cmd=err_delivered") {
		t.Fatalf("undo of a delivered command redirected to %q", loc)
	}
	mustContain(t, "too late", e.page(loc, 200), "Too late to undo.")
	for _, c := range e.commandsOf(dev) {
		if c.ID == delivered[0].ID && c.Status != "delivered" {
			t.Fatalf("a refused undo changed the command to %s", c.Status)
		}
	}
	if rows := e.auditRows("command.cancel", dev); len(rows) != 1 {
		t.Fatalf("a refused undo wrote an audit row (%d)", len(rows))
	}
}

func TestCancelRunQueuesACancelForTheOpenRun(t *testing.T) {
	e := newEnv(t)
	e.login()
	dev := e.dev["DR-OFFICE-PC"]
	loc := e.postTo("/ui/devices/"+dev+"/cancel-run", url.Values{"key": {"k-cancel"}}, http.StatusSeeOther)
	if !strings.Contains(loc, "cmd=queued") || !strings.Contains(loc, "t=cancel") {
		t.Fatalf("redirect = %q", loc)
	}
	cs := e.commandsOfType(dev, "cancel")
	var queued []store.Command
	for _, c := range cs {
		if c.Status == "queued" {
			queued = append(queued, c)
		}
	}
	if len(queued) != 1 || !strings.Contains(string(queued[0].Params), `"run_id"`) {
		t.Fatalf("cancel commands: %+v", cs)
	}
	mustContain(t, "tab", e.page(loc, 200), "Cancel run queued.", ">Cancel run</td>")
	if rows := e.auditRows("command.queue", dev); len(rows) != 1 {
		t.Fatalf("audit rows = %d", len(rows))
	}
	// A device with no open run has nothing to cancel, and nothing is queued.
	idle := e.dev["FRONTDESK-01"]
	loc = e.postTo("/ui/devices/"+idle+"/cancel-run", nil, http.StatusSeeOther)
	if !strings.Contains(loc, "cmd=err_no_run") {
		t.Fatalf("redirect = %q", loc)
	}
	if got := e.commandsOfType(idle, "cancel"); len(got) != 0 {
		t.Fatalf("a cancel was queued for an idle device: %+v", got)
	}
	mustContain(t, "notice", e.page(loc, 200), "No run to cancel.")
}

func TestExpiredCommandsShowAsExpiredBeforeTheJobRuns(t *testing.T) {
	e := newEnv(t)
	e.login()
	// The fixtures hold one expired back-up-now that the job already recorded.
	body := e.page("/devices/"+e.dev["FRONTDESK-01"]+"/commands", 200)
	mustContain(t, "fixture expiry", body, "EXPIRED", "The device did not receive it in time. Nothing ran.")
	filtered := e.page("/devices/"+e.dev["FRONTDESK-01"]+"/commands?status=expired", 200)
	mustContain(t, "expired filter", filtered, "EXPIRED", "Status: expired")
	// A command that waits past its deadline reads expired at once, though the
	// stored status is still queued until the 60-second job ticks.
	dev := e.dev["NEW-EXAM-6"]
	e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"k-late"}}, http.StatusSeeOther)
	e.now = e.now.Add(store.DefaultOperatorTTL("backup_now") + time.Minute)
	stored := e.commandsOfType(dev, "backup_now")
	if len(stored) != 1 || stored[0].Status != "queued" {
		t.Fatalf("stored: %+v", stored)
	}
	body = e.page("/devices/"+dev+"/commands", 200)
	mustContain(t, "expired by the clock", body, "EXPIRED", "The device did not receive it in time. Nothing ran.")
	if strings.Contains(body, "Waiting for the device to check in.") || strings.Contains(body, `data-fast-poll`) {
		t.Error("an expired command is shown as in flight")
	}
	// And it is no longer an in-flight request: a new one can be queued.
	loc := e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"k-late-2"}}, http.StatusSeeOther)
	if !strings.Contains(loc, "cmd=queued") {
		t.Fatalf("redirect = %q", loc)
	}
	// Undo of an expired one is refused with its own message.
	loc = e.postTo("/ui/devices/"+dev+"/commands/"+stored[0].ID+"/cancel", nil, http.StatusSeeOther)
	if !strings.Contains(loc, "cmd=err_expired") {
		t.Fatalf("undo of an expired command redirected to %q", loc)
	}
}

func TestCommandResultsAreShownAsFixedPhrases(t *testing.T) {
	e := newEnv(t)
	e.login()
	dev := e.dev["FRONTDESK-01"]
	e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"k-res"}}, http.StatusSeeOther)
	cmds, err := e.st.AgentNextCommands(context.Background(), dev, 10)
	if err != nil || len(cmds) == 0 {
		t.Fatal(err)
	}
	ack := func(status, result string) {
		if _, err := e.st.AgentAckCommand(context.Background(), dev, cmds[0].ID, store.CommandAck{Status: status, Result: []byte(result)}); err != nil {
			t.Fatal(err)
		}
	}
	ack("acked", "")
	mustContain(t, "acked", e.page("/devices/"+dev+"/commands", 200), "STARTED", "The device accepted it.")
	ack("succeeded", `{"started":true}`)
	body := e.page("/devices/"+dev+"/commands", 200)
	mustContain(t, "succeeded", body, "DONE", "The backup started.", "answered")
	if strings.Contains(body, `data-fast-poll`) {
		t.Error("a finished command keeps the list polling")
	}
	// A failed result shows a fixed phrase, never the agent's raw words.
	e.postTo("/ui/devices/"+dev+"/check-in", url.Values{"key": {"k-ci"}}, http.StatusSeeOther)
	cmds, _ = e.st.AgentNextCommands(context.Background(), dev, 10)
	if _, err := e.st.AgentAckCommand(context.Background(), dev, cmds[len(cmds)-1].ID, store.CommandAck{Status: "failed", Result: []byte(`{"reason":"not_implemented_m0","error_class":"gateway"}`)}); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "failed", e.page("/devices/"+dev+"/commands", 200), "FAILED", "This agent does not support the command yet.", "Error class: gateway.")
}

func TestCommandRoutesAreRoleGatedAndCSRFChecked(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.addAccount("viewer", "read_only")
	e.addAccount("oper", "operator")
	dev := e.dev["FRONTDESK-01"]
	idDev := e.dev["FRONTDESK-02"]
	posts := map[string][]string{ // path -> lowest role that may post it
		"/ui/devices/" + dev + "/backup-now":         {"operator"},
		"/ui/devices/" + dev + "/refresh-policy":     {"operator"},
		"/ui/devices/" + dev + "/check-in":           {"operator"},
		"/ui/devices/" + dev + "/cancel-run":         {"operator"},
		"/ui/devices/" + idDev + "/identity/resolve": {"admin"},
	}
	// Read-only: every post is refused, nothing changes, and the page says why.
	e.loginAs("viewer", testPassword)
	before := e.auditTotal()
	for path := range posts {
		res, _ := e.post(path, url.Values{"action": {"keep"}, "reason": {"x"}}, false)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("read-only POST %s = %d, want 403", path, res.StatusCode)
		}
	}
	if got := e.auditTotal(); got != before {
		t.Errorf("refused posts wrote %d audit rows", got-before)
	}
	mustContain(t, "read-only panel", e.page("/devices/"+dev, 200), "Needs the operator role.")
	mustContain(t, "read-only identity banner", e.page("/devices/"+idDev, 200), "Resolving an identity conflict needs the admin role.")
	if strings.Contains(e.page("/devices/"+idDev, 200), `/identity/resolve"`) {
		t.Error("a read-only user is offered the resolve form")
	}
	// The read-only user can read the Commands tab.
	e.page("/devices/"+dev+"/commands", 200)
	// Operator: may queue, may not resolve an identity conflict.
	e.loginAs("oper", testPassword)
	res, _ := e.post("/ui/devices/"+idDev+"/identity/resolve", url.Values{"action": {"keep"}, "reason": {"x"}}, false)
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("operator resolving an identity conflict = %d, want 403", res.StatusCode)
	}
	e.postTo("/ui/devices/"+dev+"/refresh-policy", nil, http.StatusSeeOther)
	// CSRF: a cookie-authenticated post without the token is refused and queues nothing.
	n := len(e.commandsOf(dev))
	res, _ = e.do("POST", "/ui/devices/"+dev+"/check-in", url.Values{}, "Origin", e.srv.URL)
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("post without a CSRF token = %d, want 403", res.StatusCode)
	}
	if got := len(e.commandsOf(dev)); got != n {
		t.Errorf("a post without a CSRF token queued a command")
	}
}

func TestCommandRoutesAreScopedNotFoundNotForbidden(t *testing.T) {
	e := newEnv(t)
	e.login()
	harbor := e.customerID("Harbor Family Practice")
	e.signInAs("harbor-op", "operator", []string{harbor})
	foreign := e.dev["FRONTDESK-01"] // a Summit device
	own := e.dev["FRONTDESK-H1"]
	// Reading: 404 for the other customer's device, the same as for one that does not exist.
	for _, path := range []string{"/devices/" + foreign + "/commands", "/devices/" + foreign, "/ui/devices/" + foreign + "/actions"} {
		res, _ := e.do("GET", path, nil)
		if res.StatusCode != 404 {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}
	// Writing: 404, and nothing is queued or audited.
	before, audits := len(e.commandsOf(foreign)), e.auditTotal()
	for _, path := range []string{"/ui/devices/" + foreign + "/backup-now", "/ui/devices/" + foreign + "/cancel-run", "/ui/devices/" + foreign + "/refresh-policy", "/ui/devices/" + foreign + "/check-in"} {
		res, _ := e.post(path, url.Values{}, false)
		if res.StatusCode != 404 {
			t.Errorf("POST %s = %d, want 404", path, res.StatusCode)
		}
	}
	// A command id of the other customer, addressed through the other customer's device or through one's own.
	var theirs string
	for _, c := range e.commandsOfType(foreign, "backup_now") {
		theirs = c.ID
	}
	for _, dev := range []string{foreign, own} {
		res, _ := e.post("/ui/devices/"+dev+"/commands/"+theirs+"/cancel", url.Values{}, false)
		if res.StatusCode != 404 {
			t.Errorf("undo of a foreign command through %s = %d, want 404", dev, res.StatusCode)
		}
	}
	if got := len(e.commandsOf(foreign)); got != before || e.auditTotal() != audits {
		t.Errorf("denied requests changed data: commands %d -> %d, audit %d -> %d", before, got, audits, e.auditTotal())
	}
	for _, c := range e.commandsOf(foreign) {
		if c.ID == theirs && c.Status == "cancelled" {
			t.Error("a foreign command was cancelled")
		}
	}
	// The scoped operator's own device works.
	e.postTo("/ui/devices/"+own+"/backup-now", nil, http.StatusSeeOther)
}

func TestIdentityConflictShowsAndResolves(t *testing.T) {
	e := newEnv(t)
	e.login()
	dev := e.dev["FRONTDESK-02"]
	body := e.page("/devices/"+dev, 200)
	mustContain(t, "banner", body, "Resolve this identity conflict.", `role="alert"`, "IDENTITY CONFLICT", "Resolve conflict…", `action="/ui/devices/`+dev+`/identity/resolve"`)
	mustContain(t, "other tabs", e.page("/devices/"+dev+"/history", 200), "Resolve this identity conflict.")
	// A device without a conflict shows no panel.
	if strings.Contains(e.page("/devices/"+e.dev["FRONTDESK-01"], 200), "Resolve this identity conflict.") {
		t.Error("an identity panel on a device without a conflict")
	}
	post := func(f url.Values) string {
		return e.postTo("/ui/devices/"+dev+"/identity/resolve", f, http.StatusSeeOther)
	}
	conflicted := func() bool {
		d, err := e.st.GetDevice(context.Background(), store.ScopeAll(), dev)
		if err != nil {
			t.Fatal(err)
		}
		return d.IdentityConflictAt != nil
	}
	// A missing reason, a path in the reason and an unconfirmed retire are refused; nothing changes.
	for name, c := range map[string]struct {
		form url.Values
		code string
	}{
		"no reason":       {url.Values{"action": {"keep"}, "reason": {""}}, "err_identity_reason"},
		"a path":          {url.Values{"action": {"keep"}, "reason": {`copied C:\Users\x`}}, "err_identity_reason"},
		"bad action":      {url.Values{"action": {"ignore"}, "reason": {"x"}}, "err_identity_action"},
		"retire, no name": {url.Values{"action": {"retire"}, "reason": {"clone"}}, "err_identity_confirm"},
		"retire, wrong":   {url.Values{"action": {"retire"}, "reason": {"clone"}, "confirm": {"FRONTDESK-01"}}, "err_identity_confirm"},
	} {
		if loc := post(c.form); !strings.Contains(loc, "cmd="+c.code) {
			t.Errorf("%s: redirect %q, want %s", name, loc, c.code)
		}
		if !conflicted() {
			t.Fatalf("%s: the conflict was cleared by a refused request", name)
		}
	}
	if n := len(e.auditRows("device.identity_resolve", dev)); n != 0 {
		t.Fatalf("refusals wrote %d audit rows", n)
	}
	mustContain(t, "refusal notice", e.page("/devices/"+dev+"?cmd=err_identity_confirm", 200), "Type the device name exactly")
	// Keep.
	loc := post(url.Values{"action": {"keep"}, "reason": {"hardware swap confirmed with the front desk"}})
	if !strings.Contains(loc, "cmd=kept") || conflicted() {
		t.Fatalf("keep: redirect %q, conflicted=%v", loc, conflicted())
	}
	rows := e.auditRows("device.identity_resolve", dev)
	if len(rows) != 1 || rows[0].Severity != "warning" {
		t.Fatalf("audit rows: %+v", rows)
	}
	body = e.page(loc, 200)
	mustContain(t, "after keep", body, "Identity conflict resolved.")
	if strings.Contains(body, "Resolve this identity conflict.") || strings.Contains(body, "IDENTITY CONFLICT") {
		t.Error("the conflict still shows after it was resolved")
	}
	// Resolving again says there is nothing to resolve and writes nothing.
	if loc := post(url.Values{"action": {"keep"}, "reason": {"again"}}); !strings.Contains(loc, "cmd=err_identity_none") {
		t.Fatalf("second resolve: %q", loc)
	}
	if n := len(e.auditRows("device.identity_resolve", dev)); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	mustContain(t, "audit tab", e.page("/devices/"+dev+"/audit", 200), "Identity conflict resolved", "Resolution: keep")
}

func TestIdentityConflictRetire(t *testing.T) {
	e := newEnv(t)
	e.login()
	dev := e.dev["FRONTDESK-02"]
	loc := e.postTo("/ui/devices/"+dev+"/identity/resolve", url.Values{"action": {"retire"}, "reason": {"clone of a restored image"}, "confirm": {"frontdesk-02"}}, http.StatusSeeOther)
	if !strings.Contains(loc, "cmd=retired") {
		t.Fatalf("redirect %q", loc)
	}
	d, err := e.st.GetDevice(context.Background(), store.ScopeAll(), dev)
	if err != nil || d.RetiredAt == nil || d.IdentityConflictAt != nil {
		t.Fatalf("device after retire: %+v %v", d, err)
	}
	mustContain(t, "after retire", e.page(loc, 200), "Device retired.")
}
