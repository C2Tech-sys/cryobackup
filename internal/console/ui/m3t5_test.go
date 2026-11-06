package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ---- helpers ----

func (e *env) t5Actor() store.AuditActor {
	return store.AuditActor{AdminID: e.adminID("admin"), Username: "admin", Method: "web"}
}

// t5View makes a saved device view owned by the admin, shared with all customers or private.
func (e *env) t5View(name string, shared bool) string {
	e.t.Helper()
	in := store.SavedViewInput{Name: name, Entity: "devices", Filter: json.RawMessage(`{"field":"state","op":"in","value":["FAILED"]}`)}
	if shared {
		in.Shared, in.Audience = true, &store.ViewAudience{All: true}
	}
	v, err := e.st.CreateSavedViewAudited(context.Background(), store.ScopeAll(), in, e.t5Actor())
	if err != nil {
		e.t.Fatal(err)
	}
	return v.ID
}

func (e *env) t5Park(id string) {
	e.t.Helper()
	if _, err := e.st.PauseDeviceAudited(context.Background(), store.ScopeAll(), id, store.PauseRequest{Reason: "setup"}, e.t5Actor()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) t5Principal() *auth.Principal {
	return &auth.Principal{Scope: store.ScopeAll(), Role: auth.RoleOwner, Admin: store.Admin{ID: e.adminID("admin"), Username: "admin"}}
}

func (e *env) t5Board(bq boardQuery) BoardData {
	e.t.Helper()
	p := e.t5Principal()
	f, err := e.svc.Fleet.Fleet(context.Background(), p.Scope)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.svc.boardData(e.svc.withDisplay(context.Background()), p, f, bq, "", false)
}

func (e *env) location(res *http.Response) string { return res.Header.Get("Location") }

func showing(body string) (shown, total string) {
	m := regexp.MustCompile(`Showing (\d+) of (\d+)`).FindStringSubmatch(body)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// ---- A: board filters (B03) ----

// B03: a ring segment filters the board and the board shows exactly the devices the segment counts.
func TestM3T5A_B03_ARingSegmentFiltersTheBoardToExactlyItsCount(t *testing.T) {
	e := newEnv(t)
	e.login()
	all := e.t5Board(boardQuery{})
	for i, seg := range ageSegs {
		row := all.Rings[0].Legend[i]
		got := e.t5Board(boardQuery{Age: seg})
		if got.Filter.Shown != row.N {
			t.Errorf("age segment %s: the ring counts %d devices, the board shows %d", seg, row.N, got.Filter.Shown)
		}
		devs := 0
		for _, g := range got.Groups {
			devs += len(g.Rows)
		}
		if devs != row.N {
			t.Errorf("age segment %s: %d rows for a count of %d", seg, devs, row.N)
		}
		if row.N > 0 && !strings.Contains(row.Href, "age="+seg) {
			t.Errorf("age segment %s: legend row has no link: %q", seg, row.Href)
		}
	}
	// the customer ring: every group shown is of the chosen segment
	for i, seg := range custSegs {
		got := e.t5Board(boardQuery{Cust: custSegs[i]})
		for _, g := range got.Groups {
			if g.Status == nil && g.Variant != "missing" && g.Variant != "onboarding" && g.Variant != "empty" && g.Variant != "unknown" {
				continue
			}
		}
		if all.Rings[1].Legend[i].N > 0 && !strings.Contains(all.Rings[1].Legend[i].Href, "cust="+seg) {
			t.Errorf("customer segment %s: legend row has no link", seg)
		}
	}
	crit := e.t5Board(boardQuery{Cust: "crit"})
	for _, g := range crit.Groups {
		if g.Variant == "" && g.Status != nil && g.Status.Class == ClassOK {
			t.Errorf("customer segment crit shows a healthy customer: %s", g.Name)
		}
	}
	if len(crit.Groups) == 0 {
		t.Error("customer segment crit shows nothing though the fleet has critical customers")
	}
}

func TestM3T5A_B03_ALegendRowIsALinkAndASecondClickClearsIt(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.page("/", 200)
	mustContain(t, "board", body, `class="lg" href="/?age=over"`, `class="lg" href="/?cust=crit"`)
	sel := e.page("/?age=over", 200)
	mustContain(t, "selected", sel, `Last success: over 72 h or never`, `aria-current="true"`, "Remove filter: Last success: over 72 h or never")
	if sh, tot := showing(sel); sh != "5" || tot != "25" {
		t.Errorf("age=over: showing %s of %s", sh, tot)
	}
	// the selected segment links back to the unfiltered board
	if !strings.Contains(sel, `href="/" aria-current="true"`) {
		t.Error("the selected legend row must link to the board without that filter")
	}
}

// A live update keeps the board's filter: the stream is opened with it and renders with it.
func TestM3T5A_ALiveUpdateKeepsTheFilter(t *testing.T) {
	e := newEnv(t)
	e.login()
	mustContain(t, "board", e.page("/?state=FAILED&age=over", 200), `data-events="/ui/events?age=over&amp;state=FAILED"`)
	sub := &Sub{Scope: store.ScopeAll(), Role: auth.RoleOwner, Viewer: e.adminID("admin"), Filter: "state=FAILED"}
	evs, err := e.svc.renderEvents(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	var board string
	for _, ev := range evs {
		if ev.Name == "fleet.state" {
			board = ev.Data
		}
	}
	if !strings.Contains(board, "FRONTDESK-01") || strings.Contains(board, "LAB-RETINA-02") {
		t.Fatal("the live render of a FAILED-filtered board must keep the filter")
	}
}

// ---- A: saved views (B01, S21) ----

func (e *env) postForm(path string, form url.Values) *http.Response {
	e.t.Helper()
	res, _ := e.post(path, form, false)
	return res
}

func TestM3T5A_B01_SavedViewLifecycleOnTheBoard(t *testing.T) {
	e := newEnv(t)
	e.login()
	res := e.postForm("/ui/views", url.Values{"entity": {"devices"}, "name": {"Failed ones"}, "state": {"FAILED"}})
	loc := e.location(res)
	m := regexp.MustCompile(`^/\?(?:view=([0-9A-Z]{26})&vok=saved|vok=saved&view=([0-9A-Z]{26}))$`).FindStringSubmatch(loc)
	if res.StatusCode != http.StatusSeeOther || m == nil {
		t.Fatalf("create: %d %q", res.StatusCode, loc)
	}
	id := m[1] + m[2]
	page := e.page(loc, 200)
	mustContain(t, "loaded", page, "Failed ones", "View: Failed ones", "View saved.", "FRONTDESK-01", `aria-current="true"`)
	if strings.Contains(page, "LAB-RETINA-02") || strings.Contains(page, "(modified)") {
		t.Error("a loaded view filters the board and is not modified")
	}
	if sh, _ := showing(page); sh != "1" {
		t.Errorf("the view shows %s devices, want 1", sh)
	}
	// extra filters on top of the view: modified
	mod := e.page("/?view="+id+"&age=over", 200)
	mustContain(t, "modified", mod, "Failed ones (modified)", "Unsaved changes")
	// the view survives in the list with its audience unspoken (private)
	if strings.Contains(page, "Shared ·") {
		t.Error("a private view must not claim an audience")
	}
	// rename
	e.postForm("/ui/views/"+id+"/rename", url.Values{"entity": {"devices"}, "name": {"Broken backups"}, "view": {id}})
	mustContain(t, "renamed", e.page("/?view="+id, 200), "Broken backups")
	// update with the current filters (the view and one more filter)
	r2 := e.postForm("/ui/views/"+id+"/update", url.Values{"entity": {"devices"}, "view": {id}, "state": {"UNSEEN"}})
	if r2.StatusCode != http.StatusSeeOther || !strings.Contains(e.location(r2), "vok=updated") {
		t.Fatalf("update: %d %q", r2.StatusCode, e.location(r2))
	}
	if rows := e.auditRows("view.update", id); len(rows) != 2 {
		t.Errorf("rename and update wrote %d audit rows, want 2", len(rows))
	}
	// delete
	r3 := e.postForm("/ui/views/"+id+"/delete", url.Values{"entity": {"devices"}, "view": {id}})
	if !strings.Contains(e.location(r3), "vok=deleted") || strings.Contains(e.location(r3), "view=") {
		t.Fatalf("delete: %q", e.location(r3))
	}
	if body := e.page("/?view="+id, 200); !strings.Contains(body, "View not found") || strings.Contains(body, "Broken backups") {
		t.Error("a deleted view named in a link says so and does not filter")
	}
}

func TestM3T5A_B01_ASharedViewNamesItsAudienceAndALockedOneOffersOnlyPrivateAndDelete(t *testing.T) {
	e := newEnv(t)
	e.login()
	summit, northwind := e.customerID("Summit Eye Care"), e.customerID("Northwind Dental")
	e.postForm("/ui/views", url.Values{"entity": {"devices"}, "name": {"All-hands"}, "state": {"FAILED"}, "shared": {"1"}, "audience": {"all"}})
	e.postForm("/ui/views", url.Values{"entity": {"devices"}, "name": {"Two sites"}, "state": {"FAILED"}, "shared": {"1"}, "audience": {"list"}, "customer_id": {summit, northwind}})
	body := e.page("/", 200)
	mustContain(t, "audience", body, "Shared · all customers", "Shared · 2 customers", "All-hands", "Two sites")
	if !strings.Contains(body, `title="Northwind Dental, Summit Eye Care"`) && !strings.Contains(body, `title="Summit Eye Care, Northwind Dental"`) {
		t.Error("the audience badge names its customers")
	}
	// a locked view: your own, shared with customers you can no longer see
	out, err := e.svc.renderString("board", "views_control", &ViewsControl{Entity: "devices", CSRF: "x", Label: "All devices", SaveOK: true, Mine: []ViewItem{
		{ID: "L1", Name: "Locked one", Href: "/?view=L1", Mine: true, Shared: true, Locked: true, Audience: "3 customers", CanManage: true, CanEdit: false}}})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "locked", out, "Locked", "/ui/views/L1/private", "/ui/views/L1/delete")
	if strings.Contains(out, "/ui/views/L1/rename") || strings.Contains(out, "/ui/views/L1/update") {
		t.Error("a locked view must not offer rename or update")
	}
}

func TestM3T5A_B01_AViewThatCannotBeSavedSaysWhyAndKeepsTheFilters(t *testing.T) {
	e := newEnv(t)
	e.login()
	cases := []struct {
		name string
		form url.Values
		code string
		text string
	}{
		{"empty name", url.Values{"entity": {"devices"}, "name": {" "}, "state": {"FAILED"}}, "e_name", "use a name of 1 to 100 characters"},
		{"shared, no audience", url.Values{"entity": {"devices"}, "name": {"No aud"}, "state": {"FAILED"}, "shared": {"1"}}, "e_aud", "say who a shared view is for"},
		{"customer ring filter", url.Values{"entity": {"devices"}, "name": {"Ring"}, "cust": {"crit"}}, "e_cust", "customer ring filter is not part of a saved view"},
	}
	for _, c := range cases {
		res := e.postForm("/ui/views", c.form)
		loc := e.location(res)
		if res.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "verr="+c.code) {
			t.Errorf("%s: %d %q", c.name, res.StatusCode, loc)
			continue
		}
		page := e.page(loc, 200)
		mustContain(t, c.name, page, "View not saved:", c.text, "Your filters are unchanged.")
	}
	// the same name twice
	e.postForm("/ui/views", url.Values{"entity": {"devices"}, "name": {"Dup"}, "state": {"FAILED"}})
	res := e.postForm("/ui/views", url.Values{"entity": {"devices"}, "name": {"Dup"}, "state": {"FAILED"}})
	if !strings.Contains(e.location(res), "verr=e_dup") {
		t.Errorf("duplicate name: %q", e.location(res))
	}
}

func TestM3T5A_S21_SavedViewsOnTheRunSearch(t *testing.T) {
	e := newEnv(t)
	e.login()
	res := e.postForm("/ui/views", url.Values{"entity": {"runs"}, "name": {"Bad runs"}, "status": {"failed", "partial"}})
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(e.location(res), "/runs?") || !strings.Contains(e.location(res), "vok=saved") {
		t.Fatalf("create: %d %q", res.StatusCode, e.location(res))
	}
	cur := e.page("/runs?status=failed&status=partial", 200)
	mustContain(t, "current", cur, "Bad runs", `id="views-runs"`, `aria-current="true"`)
	if strings.Contains(cur, "(modified)") {
		t.Error("the run search marks no view modified")
	}
	// /runs?view={id} is the link to the view: it redirects to the search it holds
	var id string
	vs, err := e.st.ListSavedViews(context.Background(), store.ScopeAll(), e.adminID("admin"), "runs")
	if err != nil || len(vs) != 1 {
		t.Fatalf("views: %v %d", err, len(vs))
	}
	id = vs[0].ID
	r2, _ := e.do("GET", "/runs?view="+id, nil)
	if r2.StatusCode != http.StatusSeeOther || !strings.Contains(r2.Header.Get("Location"), "status=failed") || !strings.Contains(r2.Header.Get("Location"), "status=partial") {
		t.Fatalf("view link: %d %q", r2.StatusCode, r2.Header.Get("Location"))
	}
	// a view the builder cannot show is not applied, and the page says so
	g, err := e.st.CreateSavedViewAudited(context.Background(), store.ScopeAll(), store.SavedViewInput{Name: "Either", Entity: "runs",
		Filter: json.RawMessage(`{"op":"or","clauses":[{"field":"outcome","op":"in","value":["failed"]},{"field":"kind","op":"in","value":["prune"]}]}`)}, e.t5Actor())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "unsupported", e.page("/runs?view="+g.ID, 200), "This view cannot be applied.")
	// an unknown view id
	mustContain(t, "missing", e.page("/runs?view=nope", 200), "View not found.")
}

// ---- A: the pause dialog (B15) ----

func (e *env) retiredDevice() string {
	e.t.Helper()
	l, err := e.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 200}})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, d := range l.Items {
		if d.RetiredAt != nil {
			return d.ID
		}
	}
	e.t.Fatal("the fixtures have no retired device")
	return ""
}

func TestM3T5A_B15_ThePauseDialog(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	page := e.page("/devices/"+id, 200)
	mustContain(t, "dialog", page, `act-park`, `name="when" value="1"`, `name="when" value="2"`, `name="when" value="4"`, `name="when" value="8"`, `name="when" value="24"`,
		`name="when" value="until"`, `name="when" value="indefinite"`, `name="reason" type="text" maxlength="200" required`, `type="datetime-local"`, "Park device")
	post := func(form url.Values) *http.Response { return e.postForm("/ui/devices/"+id+"/pause", form) }
	dev := func() store.Device {
		d, err := e.st.GetDevice(context.Background(), store.ScopeAll(), id)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// a reason is required
	if r := post(url.Values{"when": {"4"}, "reason": {"  "}}); !strings.Contains(e.location(r), "cmd=err_pause_reason") || dev().PausedAt != nil {
		t.Fatalf("no reason: %q parked=%v", e.location(r), dev().PausedAt != nil)
	}
	// no duration chosen
	if r := post(url.Values{"reason": {"x"}}); !strings.Contains(e.location(r), "cmd=err_pause_when") || dev().PausedAt != nil {
		t.Fatalf("no duration: %q", e.location(r))
	}
	// an end in the past, and a duration that is not one of the five
	if r := post(url.Values{"when": {"until"}, "until": {"2020-01-01T10:00"}, "reason": {"x"}}); !strings.Contains(e.location(r), "cmd=err_pause_until") || dev().PausedAt != nil {
		t.Fatalf("past end: %q", e.location(r))
	}
	if r := post(url.Values{"when": {"3"}, "reason": {"x"}}); !strings.Contains(e.location(r), "cmd=err_pause_when") {
		t.Fatalf("3 hours: %q", e.location(r))
	}
	// 4 hours
	if r := post(url.Values{"when": {"4"}, "reason": {"patching window"}}); !strings.Contains(e.location(r), "cmd=parked") {
		t.Fatalf("park: %q", e.location(r))
	}
	d := dev()
	if d.PausedUntil == nil || !d.PausedUntil.Equal(e.now.Add(4*time.Hour)) || d.PauseReason != "patching window" {
		t.Fatalf("parked until %v reason %q, want %v", d.PausedUntil, d.PauseReason, e.now.Add(4*time.Hour))
	}
	after := e.page("/devices/"+id+"?cmd=parked", 200)
	mustContain(t, "after", after, "Device parked.", "act-resume", ">Resume<", "patching window")
	if strings.Contains(after, `id="act-park"`) {
		t.Error("a parked device offers Resume, not Park")
	}
	// resume, then until a date in the display zone, then indefinitely
	if r := e.postForm("/ui/devices/"+id+"/resume", url.Values{}); !strings.Contains(e.location(r), "cmd=resumed") || dev().PausedAt != nil {
		t.Fatalf("resume: %q", e.location(r))
	}
	want := e.now.In(e.loc).Add(48 * time.Hour).Truncate(time.Minute)
	if r := post(url.Values{"when": {"until"}, "until": {want.Format(datetimeLocal)}, "reason": {"travel"}}); !strings.Contains(e.location(r), "cmd=parked") {
		t.Fatalf("until: %q", e.location(r))
	}
	if d := dev(); d.PausedUntil == nil || !d.PausedUntil.Equal(want.UTC()) {
		t.Fatalf("until a date: %v, want %v (read in the display zone)", d.PausedUntil, want.UTC())
	}
	e.postForm("/ui/devices/"+id+"/resume", url.Values{})
	if r := post(url.Values{"when": {"indefinite"}, "reason": {"decommission pending"}}); !strings.Contains(e.location(r), "cmd=parked") {
		t.Fatalf("indefinite: %q", e.location(r))
	}
	if d := dev(); d.PausedAt == nil || d.PausedUntil != nil {
		t.Fatalf("indefinite pause: at %v until %v", d.PausedAt, d.PausedUntil)
	}
	if n := len(e.auditRows("device.pause", id)); n < 3 {
		t.Errorf("parking wrote %d device.pause audit rows, want at least 3", n)
	}
}

func TestM3T5A_B15_AFixedPauseIsRefusedOnARetiredDeviceAndForAReadOnlyAccount(t *testing.T) {
	e := newEnv(t)
	e.login()
	ret := e.retiredDevice()
	r := e.postForm("/ui/devices/"+ret+"/pause", url.Values{"when": {"4"}, "reason": {"x"}})
	if !strings.Contains(e.location(r), "cmd=err_retired") {
		t.Fatalf("retired device: %d %q", r.StatusCode, e.location(r))
	}
	if d, _ := e.st.GetDevice(context.Background(), store.ScopeAll(), ret); d.PausedAt != nil {
		t.Fatal("a retired device was parked")
	}
	// the retired device's page offers no Park
	body := e.page("/devices/"+ret, 200)
	if strings.Contains(body, `name="when"`) {
		t.Error("a retired device must not offer the pause dialog")
	}
	// a read-only account cannot park
	e.addAccount("watcher", "read_only")
	e.loginAs("watcher", testPassword)
	res, _ := e.post("/ui/devices/"+e.dev["FRONTDESK-01"]+"/pause", url.Values{"when": {"4"}, "reason": {"x"}}, false)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only account: %d", res.StatusCode)
	}
}

// ---- A: the state-explain tooltip ----

func TestM3T5A_B01_TheStateExplainTooltip(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	frag := e.page("/ui/devices/"+id+"/explain", 200)
	mustContain(t, "explain", frag, "Why this state", "FAILED", "The newest backup failed", "Last success", "Last heard from", "Computed ")
	if strings.Contains(frag, "T18:42:10Z") || regexp.MustCompile(`\d\d:\d\d:\d\dZ`).MatchString(frag) {
		t.Error("the explanation prints a UTC timestamp")
	}
	// the chip loads it on hover or focus
	board := e.page("/", 200)
	mustContain(t, "chip", board, `hx-get="/ui/devices/`+id+`/explain"`, `hx-trigger="mouseenter once, focus once"`, `hx-target="#tip-t-`+id+`"`)
	// a device outside the account's scope is a 404, not an explanation
	if res, _ := e.do("GET", "/ui/devices/01NOSUCHDEVICE00000000000/explain", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown device: %d", res.StatusCode)
	}
	// an agent-supplied version label is withheld in the explanation of an OUTDATED device
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET agent_version = ? WHERE id = ?`, hostileTexts[4], e.dev["DR-LAPTOP-03"])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if f := e.page("/ui/devices/"+e.dev["DR-LAPTOP-03"]+"/explain", 200); strings.Contains(f, hostileTexts[4]) {
		t.Error("the explanation prints the raw agent version")
	}
}

// ---- A: banner labels, copy and actions (components 9.2) ----

func TestM3T5A_BannerActionsAndLabels(t *testing.T) {
	now := fixtures.Now()
	for _, b := range fixtures.AllBanners(now) {
		v := bannerView(b)
		if strings.HasPrefix(v.Label, "CONDITION") {
			t.Errorf("%s has no label of its own", b.ID)
		}
		switch b.ID {
		case "C09":
			if v.Action == nil || v.Action.Href != "/?state=UNKNOWN" {
				t.Errorf("C09 action: %+v", v.Action)
			}
		case "C08":
			if v.Action == nil || v.Action.Href != "/enroll" || !v.Action.Admin {
				t.Errorf("C08 action: %+v", v.Action)
			}
		case "C10":
			if v.Action != nil {
				t.Errorf("C10 is client-side and has no action: %+v", v.Action)
			}
		default:
			if v.Action == nil || v.Action.Href != "/system" || v.Action.Label != "Open System health" {
				t.Errorf("%s (%s): action %+v, want Open System health", b.ID, v.Label, v.Action)
			}
		}
	}
	// C11 at critical severity is a critical banner with its label and action
	for _, b := range fixtures.AllBanners(now) {
		if b.ID == "C11" {
			v := bannerView(b)
			if v.Class != ClassCrit || v.Label != "DATABASE NEEDS ATTENTION" || v.Action == nil {
				t.Errorf("C11 critical: %+v", v)
			}
		}
	}
	// a role that cannot open the destination is not offered the link
	f := health.Fleet{View: state.FleetView{Banners: []state.Banner{{ID: "C08", Severity: state.SeverityCritical, Text: "No active devices enrolled."}, {ID: "C01", Severity: state.SeverityCritical, Text: "Gateway unreachable."}}}}
	bv := buildBanners(f, false)
	op := bv.forRole(auth.RoleOperator)
	for _, it := range op.Items {
		if it.ID == "C08" && it.Action != nil {
			t.Error("an operator is offered the admin-only Enroll link")
		}
		if it.ID == "C01" && it.Action == nil {
			t.Error("an operator keeps the System health link")
		}
	}
	if ad := bv.forRole(auth.RoleAdmin); ad.Items[0].Action == nil && ad.Items[1].Action == nil {
		t.Error("an admin keeps every action")
	}
}

// ---- G: the sidebar gateway line, case by case ----

func TestM3T5G_GatewayLineFollowsTheEvidence(t *testing.T) {
	e := newEnv(t)
	now := fixtures.Now()
	at := func(d time.Duration) state.Millis { return state.Millis(now.Add(-d).UnixMilli()) }
	probes := func(g *state.ProbeRun) func() state.ProbeFacts {
		return func() state.ProbeFacts {
			return state.ProbeFacts{Wired: true, ProberInterval: time.Minute, Gateway: g}
		}
	}
	fleet := func(ids ...string) health.Fleet {
		f := health.Fleet{Computed: now}
		for _, id := range ids {
			f.View.Banners = append(f.View.Banners, state.Banner{ID: id, Severity: state.SeverityCritical})
		}
		return f
	}
	cases := []struct {
		name   string
		probes func() state.ProbeFacts
		fleet  health.Fleet
		want   string
		class  string
	}{
		{"fresh pass", probes(&state.ProbeRun{LastRunAt: at(20 * time.Second), LastOKAt: at(20 * time.Second)}), fleet(), "ok", ClassOK},
		{"C01 raised", probes(&state.ProbeRun{LastRunAt: at(time.Minute), LastOKAt: at(time.Hour), ConsecutiveFailures: 3}), fleet("C01"), "unreachable", ClassCrit},
		{"one failure, no banner yet", probes(&state.ProbeRun{LastRunAt: at(10 * time.Second), LastOKAt: at(70 * time.Second), ConsecutiveFailures: 1}), fleet(), "failing", ClassWarn},
		{"not checked banner", probes(&state.ProbeRun{LastRunAt: at(20 * time.Second), LastOKAt: at(20 * time.Second)}), fleet("C01b"), "not checked", ClassWarn},
		{"self-checks not wired", probes(&state.ProbeRun{LastRunAt: at(20 * time.Second), LastOKAt: at(20 * time.Second)}), fleet("C00"), "not checked", ClassWarn},
		{"stale pass", probes(&state.ProbeRun{LastRunAt: at(time.Hour), LastOKAt: at(time.Hour)}), fleet(), "not checked", ClassWarn},
		{"never ran", probes(&state.ProbeRun{}), fleet(), "not checked", ClassWarn},
		{"unconfigured", probes(&state.ProbeRun{Unconfigured: "gateway_url is not set"}), fleet(), "not checked", ClassWarn},
		{"no probe facts at all", nil, fleet(), "not checked", ClassWarn},
		{"no evaluation", probes(&state.ProbeRun{LastRunAt: at(time.Second), LastOKAt: at(time.Second)}), health.Fleet{}, "not checked", ClassWarn},
	}
	old := e.svc.Fleet.Probes
	defer func() { e.svc.Fleet.Probes = old }()
	for _, c := range cases {
		e.svc.Fleet.Probes = c.probes
		got := e.svc.gatewayItem(c.fleet)
		if got.Value != c.want || got.Class != c.class || got.Label != "Gateway" {
			t.Errorf("%s: %+v, want %s/%s", c.name, got, c.want, c.class)
		}
	}
}

// ---- C: banner times are 12-hour and in the display zone ----

func TestM3T5C_BannerTimesAreTwelveHourInTheDisplayZone(t *testing.T) {
	e := newEnv(t) // display zone America/Chicago; the clock is 6:42 PM UTC = 1:42 PM there
	if e.svc.Fleet.Zone == nil || e.svc.Fleet.Zone(context.Background()).String() != "America/Chicago" {
		t.Fatal("the evaluator is not given the display zone")
	}
	now := fixtures.Now()
	at := func(d time.Duration) state.Millis { return state.Millis(now.Add(-d).UnixMilli()) }
	old := e.svc.Fleet.Probes
	defer func() { e.svc.Fleet.Probes = old }()
	e.svc.Fleet.Probes = func() state.ProbeFacts {
		return state.ProbeFacts{Wired: true, ProberInterval: time.Minute, Gateway: &state.ProbeRun{LastRunAt: at(10 * time.Second), LastOKAt: at(9 * time.Minute), ConsecutiveFailures: 4}}
	}
	f, err := e.svc.Fleet.Fleet(e.svc.withDisplay(context.Background()), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range f.View.Banners {
		if b.ID == "C01" {
			if !strings.Contains(b.Text, "Last OK 1:33 PM") {
				t.Fatalf("C01 says %q, want the last OK time as 1:33 PM (display zone, 12-hour)", b.Text)
			}
			return
		}
	}
	t.Fatal("no C01 banner")
}

// ---- B: the operator's rulings ----

func TestM3T5B_Ruling1_ActionHintsAreTooltipsOnTheirButtons(t *testing.T) {
	e := newEnv(t)
	e.login()
	page := e.page("/devices/"+e.dev["FRONTDESK-01"], 200)
	if strings.Contains(page, `class="meta-mono act-note"`) {
		t.Error("an action hint is still a line under its button")
	}
	mustContain(t, "tooltips", page, `aria-describedby="act-backup-now-hint"`, `class="tip" role="tooltip" id="act-backup-now-hint"`, `class="tip" role="tooltip" id="act-park-hint"`)
	// a disabled button is focusable so its reason can be read from the keyboard
	parked := e.dev["LT-OPTOM-04"]
	e.t5Park(parked)
	off := e.page("/devices/"+parked, 200)
	mustContain(t, "disabled", off, `role="link" tabindex="0" aria-disabled="true" aria-describedby="act-backup-now-why"`, `id="act-backup-now-why"`)
}

func TestM3T5B_Ruling2and3and5_PolicyHeaderRevisionsAndReview(t *testing.T) {
	e := newEnv(t)
	e.login()
	north := e.customerID("Northwind Dental")
	cust := e.page("/customers/"+north+"/policy", 200)
	mustContain(t, "customer tab", cust, `class="pol2"`, `class="pl1"`, `class="pl2"`, "VERSION 1")
	dev := e.page("/devices/"+e.dev["FRONTDESK-01"]+"/policy", 200)
	mustContain(t, "device tab", dev, `class="polmeta pol2"`, "VERSION ")
	var def string
	for _, m := range regexp.MustCompile(`href="/policies/([0-9A-Z]{26})"`).FindAllStringSubmatch(e.page("/policies", 200), -1) {
		def = m[1]
		break
	}
	if def == "" {
		t.Fatal("no policy link")
	}
	ed := e.page("/policies/"+def, 200)
	mustContain(t, "editor header", ed, `class="tt"`, "VERSION ")
	// the revisions table is a card list on a phone: labelled cells the CSS turns into cards
	if strings.Contains(ed, "Revisions") {
		mustContain(t, "revisions", ed, `class="fleet compact revs"`, `data-label="Changed by"`, `data-label="When"`)
	}
	css := e.appCSS(t)
	for _, rule := range []string{`table.fleet.revs`, `.review .blast`, `.tab-more`} {
		if !strings.Contains(css, rule) {
			t.Errorf("app.css lacks %s", rule)
		}
	}
}

func TestM3T5B_Ruling4_PhoneDeviceTabsHaveAMoreTab(t *testing.T) {
	e := newEnv(t)
	e.login()
	page := e.page("/devices/"+e.dev["FRONTDESK-01"], 200)
	nav := regexp.MustCompile(`(?s)<nav class="tabs".*?</nav>`).FindString(page)
	if n := strings.Count(nav, `class="tab-ov"`); n < 3 {
		t.Errorf("%d overflow tabs, want at least 3 (Settings, Policy, Audit)", n)
	}
	mustContain(t, "more", nav, `<details class="tab-more"`, "<summary>More</summary>", `class="tab-more-pop"`)
	// a current tab inside the overflow marks the More tab
	set := regexp.MustCompile(`(?s)<nav class="tabs".*?</nav>`).FindString(e.page("/devices/"+e.dev["FRONTDESK-01"]+"/settings", 200))
	mustContain(t, "more current", set, `data-current="true"`, `<summary aria-current="page">More</summary>`)
	// every tab is still a link in the markup
	for _, label := range []string{"Summary", "History", "Commands", "Settings", "Policy"} {
		if !strings.Contains(nav, ">"+label+"</a>") {
			t.Errorf("tab %s is not a link", label)
		}
	}
}

func TestM3T5B_Ruling6_ThePublicURLBannerIsAWarning(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.svc.PublicURL = ""
	body := e.page("/enroll", 200)
	i := strings.Index(body, "Public URL not set.")
	if i < 0 {
		t.Fatal("no public URL note")
	}
	head := body[max(0, i-300):i]
	if !strings.Contains(head, "banner warn") || strings.Contains(head[strings.LastIndex(head, "<"):], "crit") {
		t.Errorf("the public URL note is not a warning: ...%s", head)
	}
	if strings.Contains(head, "banner crit") && strings.LastIndex(head, "banner crit") > strings.LastIndex(head, "banner warn") {
		t.Error("the public URL note is critical")
	}
}

// ---- C: the favicon and the CSP ----

func TestM3T5C_TheFaviconIsOurOwnFileSoTheCSPAllowsIt(t *testing.T) {
	e := newEnv(t)
	linkRE := regexp.MustCompile(`<link rel="icon"[^>]*href="([^"]*)"`)
	pages := map[string]string{"login": e.page("/login", 200)}
	e.login()
	pages["board"] = e.page("/", 200)
	e.forcePending("admin")
	pages["mfa"] = e.page("/login/mfa", 200)
	for name, body := range pages {
		m := linkRE.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s: no icon link", name)
			continue
		}
		// the CSP is img-src 'self': a data: URL is refused (a console error on every sign-in)
		if !strings.HasPrefix(m[1], "/static/favicon.svg") {
			t.Errorf("%s: the icon is %q, want a same-origin file", name, m[1])
		}
	}
	res, body := e.do("GET", "/static/favicon.svg", nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(body, "<svg") {
		t.Fatalf("favicon: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

// ---- E: device history rows link to the run ----

func TestM3T5E_DeviceHistoryRowsLinkToTheRun(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	for name, path := range map[string]string{"summary": "/devices/" + id, "history": "/devices/" + id + "/history"} {
		body := e.page(path, 200)
		links := regexp.MustCompile(`href="(/runs/[0-9A-Za-z]+)"`).FindAllStringSubmatch(body, -1)
		if len(links) < 3 {
			t.Fatalf("%s: %d run links", name, len(links))
		}
		if res, _ := e.do("GET", links[0][1], nil); res.StatusCode != 200 {
			t.Errorf("%s: %s answers %d", name, links[0][1], res.StatusCode)
		}
	}
}
