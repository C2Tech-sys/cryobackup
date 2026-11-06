package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T5 delta: the blind review's four MEDIUMs, the surviving mutants and the small findings.

func groupNames(b BoardData) []string {
	var out []string
	for _, g := range b.Groups {
		out = append(out, g.Name)
	}
	return out
}

// M1: a ring segment filter shows every customer the legend counts, customers with no devices included.
// It hid the MISSING group of /?cust=crit and answered "No devices match" for /?cust=other.
func TestM3T5M1_TheCustomerRingFilterShowsEveryCustomerItCounts(t *testing.T) {
	e := newEnv(t)
	e.login()
	all := e.t5Board(boardQuery{})
	for i, seg := range custSegs {
		n := all.Rings[1].Legend[i].N
		got := e.t5Board(boardQuery{Cust: seg})
		if len(got.Groups) != n {
			t.Errorf("customer segment %s: the legend counts %d customers, the board shows %d: %v", seg, n, len(got.Groups), groupNames(got))
		}
		if n > 0 && got.Empty != nil {
			t.Errorf("customer segment %s: the board says %q although it shows %d customers", seg, got.Empty.Title, len(got.Groups))
		}
	}
	has := func(seg, name string) bool {
		for _, n := range groupNames(e.t5Board(boardQuery{Cust: seg})) {
			if n == name {
				return true
			}
		}
		return false
	}
	// a MISSING customer is a critical customer; an onboarding one is "onboarding or empty", never healthy
	for _, c := range []struct {
		seg, name string
		want      bool
	}{{"crit", "Blue Ridge Accounting", true}, {"other", "Blue Ridge Accounting", false}, {"ok", "Blue Ridge Accounting", false},
		{"other", "Lakeshore Pediatrics", true}, {"crit", "Lakeshore Pediatrics", false}, {"ok", "Lakeshore Pediatrics", false},
		{"ok", "Summit Eye Care", false}, {"crit", "Summit Eye Care", true}} {
		if has(c.seg, c.name) != c.want {
			t.Errorf("cust=%s: %s shown = %v, want %v", c.seg, c.name, !c.want, c.want)
		}
	}
	// the filter is a filter: with no filter every customer shows, and the healthy ones are not in the critical segment
	if len(all.Groups) != 6 {
		t.Errorf("the unfiltered board shows %d customers, want 6", len(all.Groups))
	}
	page := e.page("/?cust=other", 200)
	if strings.Contains(page, "No devices match these filters") || !strings.Contains(page, "Lakeshore Pediatrics") {
		t.Error("/?cust=other must show Lakeshore Pediatrics, not an empty-filter panel")
	}
	crit := e.page("/?cust=crit", 200)
	mustContain(t, "cust=crit", crit, "Blue Ridge Accounting", "MISSING", "Harbor Family Practice", "Summit Eye Care")
	// a device filter on top: customers with no devices cannot match a device filter
	if got := e.t5Board(boardQuery{Cust: "crit", States: []string{"FAILED"}}); len(groupNames(got)) != 1 || groupNames(got)[0] != "Summit Eye Care" {
		t.Errorf("cust=crit&state=FAILED shows %v, want only Summit Eye Care", groupNames(got))
	}
}

// M4: the API's banner text is the engine's own, untouched; the page words the time in the display zone itself.
func TestM3T5M4_TheAPIBannerTextIsUntouchedAndThePageShowsTheDisplayZone(t *testing.T) {
	e := newEnv(t) // display zone America/Chicago; the clock is 18:42:10 UTC = 1:42:10 PM there
	e.login()
	now := fixtures.Now()
	at := func(d time.Duration) state.Millis { return state.Millis(now.Add(-d).UnixMilli()) }
	old := e.svc.Fleet.Probes
	defer func() { e.svc.Fleet.Probes = old }()
	e.svc.Fleet.Probes = func() state.ProbeFacts {
		return state.ProbeFacts{Wired: true, ProberInterval: time.Minute, Gateway: &state.ProbeRun{LastRunAt: at(10 * time.Second), LastOKAt: at(9 * time.Minute), ConsecutiveFailures: 4}}
	}
	// the evaluator the API shares: what GET /api/v1/fleet/banners prints (BannerJSON text and at)
	f, err := e.svc.Fleet.Fleet(e.svc.withDisplay(context.Background()), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	var c01 *state.Banner
	for i := range f.View.Banners {
		if f.View.Banners[i].ID == "C01" {
			c01 = &f.View.Banners[i]
		}
	}
	if c01 == nil {
		t.Fatal("no C01")
	}
	const main = "Gateway unreachable: no agent can back up. Last OK 6:33 PM." // byte for byte what main prints
	if c01.Text != main || c01.At.Format(time.RFC3339) != "2026-10-20T18:33:10Z" {
		t.Fatalf("the API banner reads %q at %s; main reads %q at 2026-10-20T18:33:10Z", c01.Text, c01.At.Format(time.RFC3339), main)
	}
	// the page: the same instant in the display zone, 12-hour
	page := e.page("/", 200)
	if !strings.Contains(page, "Last OK 1:33 PM.") || strings.Contains(page, "Last OK 6:33 PM.") {
		t.Error("the board page does not show the last OK time in the display zone (1:33 PM)")
	}
	if s := e.page("/system", 200); !strings.Contains(s, "Last OK 1:33 PM.") || strings.Contains(s, "Last OK 6:33 PM.") {
		t.Error("the System page does not show the last OK time in the display zone (1:33 PM)")
	}
	// the live stream re-renders the strip the same way
	evs, err := e.svc.renderEvents(context.Background(), &Sub{Scope: store.ScopeAll(), Role: auth.RoleOwner, Viewer: e.adminID("admin")})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Name == "banner.update" && (!strings.Contains(ev.Data, "Last OK 1:33 PM.") || strings.Contains(ev.Data, "Last OK 6:33 PM.")) {
			t.Error("the live banner strip does not show the display-zone time")
		}
	}
}

// A time re-worded for the display zone moves with the zone, and a banner without an instant is left alone.
func TestM3T5M4_RetimeFollowsTheZone(t *testing.T) {
	now := fixtures.Now()
	chicago := fixtures.Zone()
	at := now.Add(-9 * time.Minute)
	if got := retime("Gateway unreachable. Last OK 6:33 PM.", at, now, chicago); got != "Gateway unreachable. Last OK 1:33 PM." {
		t.Errorf("same day: %q", got)
	}
	yesterday := now.Add(-26 * time.Hour) // Mon 19 Oct 6:42 PM UTC = 1:42 PM there... the date differs from today's in both zones
	if got := retime("Last send Mon 19 Oct 4:42 PM.", yesterday, now, chicago); got != "Last send Mon 19 Oct 11:42 AM." {
		t.Errorf("other day: %q", got)
	}
	if got := retime("Agent listener certificate expires in 12 days.", at, now, chicago); got != "Agent listener certificate expires in 12 days." {
		t.Errorf("text without a clock time changed: %q", got)
	}
	if got := retime("Last OK 6:33 PM.", time.Time{}, now, chicago); got != "Last OK 6:33 PM." {
		t.Errorf("no instant, no change: %q", got)
	}
	if got := retime("Last OK 6:33 PM.", at, now, nil); got != "Last OK 6:33 PM." {
		t.Errorf("no zone, no change: %q", got)
	}
}

// Surviving mutant: the fixtures cookie rule on a real console. A real (non-fixtures) App issues exactly
// auth's expiry: the pending session's ten minutes, then the absolute lifetime of the full session.
func TestM3T5_ARealConsoleKeepsAuthsCookieExpiry(t *testing.T) {
	e := newEnv(t)
	e.svc.Fixtures = false // a real console: the fixtures cookie rule must not apply
	res, body := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {testPassword}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	check := func(when string, cookies []*http.Cookie, want time.Time) {
		n := 0
		for _, c := range cookies {
			if c.Name != auth.SessionCookie && c.Name != auth.CSRFCookie {
				continue
			}
			n++
			if c.Expires.Unix() != want.Unix() {
				t.Errorf("%s: cookie %s expires %v, auth's rule gives %v (the console's own clock)", when, c.Name, c.Expires.UTC(), want.UTC())
			}
		}
		if n != 2 {
			t.Errorf("%s: %d session cookies", when, n)
		}
	}
	check("password step", res.Cookies(), e.now.Add(auth.PendingTTL))
	e.login()
	check("after MFA", e.sess, e.now.Add(7*24*time.Hour))
}

// Surviving mutant: events() dropped the board's filter. The stream's first paint must be the filtered board.
func TestM3T5_TheEventStreamOpensWithTheBoardFilter(t *testing.T) {
	e := newEnv(t)
	e.login()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/ui/events?state=FAILED", nil)
	for _, c := range e.sess {
		req.AddCookie(c)
	}
	res, err := e.cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var ev string
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && ev == "fleet.state":
			data.WriteString(strings.TrimPrefix(line, "data: "))
		case line == "" && ev == "fleet.state" && data.Len() > 0:
			cancel()
			board := data.String()
			if !strings.Contains(board, "FRONTDESK-01") || strings.Contains(board, "LAB-RETINA-02") {
				t.Fatal("the stream's first board is not the FAILED-filtered one: the filter was dropped")
			}
			return
		}
	}
	t.Fatal("no fleet.state event arrived")
}

// Surviving mutant: safeVersion bypassed at the OUTDATED qualifier and in the explain text.
func TestM3T5F_NoRawVersionAtTheOutdatedQualifierReasonOrExplain(t *testing.T) {
	hostile := hostileTexts[4]
	now := fixtures.Now()
	c := fmtCtx{Now: now, Loc: fixtures.Zone(), Zone: "America/Chicago"}
	ds := SampleDeviceState(state.StateOutdated, now)
	ds.Detail.AgentVersion, ds.Detail.ExpectedVersion = hostile, "1.5.0"
	if q, _ := qualifier(ds, c, -1); strings.Contains(q, hostile) || !strings.Contains(q, withheld) {
		t.Errorf("the OUTDATED chip qualifier is %q", q)
	}
	if r := reasonLine(ds, c); strings.Contains(r, hostile) || !strings.Contains(r, withheld) {
		t.Errorf("the OUTDATED reason is %q", r)
	}
	e := newEnv(t)
	st := adminapi.DeviceStateResponse{StampJSON: adminapi.StampJSON{ComputedAt: now}, DeviceID: "d1",
		Explain: []adminapi.ExplainJSON{{State: "OUTDATED", Text: "The agent runs " + hostile + "; 1.5.0 is expected."}},
		Inputs:  adminapi.StateInputsJSON{AgentVersion: hostile, ExpectedVersion: "1.5.0"}}
	v := e.svc.explainOf(context.Background(), st)
	all := v.Note + v.Err
	for _, l := range v.Lines {
		all += l.Text
	}
	for _, f := range v.Facts {
		all += f.Value
	}
	if strings.Contains(all, hostile) || !strings.Contains(all, withheld) {
		t.Errorf("the explanation prints the raw version: %q", all)
	}
}

// Outside scope: a foreign device (one that exists, in a customer this account cannot see) is a 404.
func TestM3T5_TheExplainFragmentRefusesAForeignDevice(t *testing.T) {
	e := newEnv(t)
	north := e.customerID("Northwind Dental")
	e.signInAs("scoped-explain", "read_only", []string{north})
	foreign := e.dev["FRONTDESK-01"] // Summit Eye Care's
	if res, _ := e.do("GET", "/ui/devices/"+foreign+"/explain", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("a device of another customer: %d, want 404", res.StatusCode)
	}
	if res, body := e.do("GET", "/ui/devices/"+e.dev["OPS-ND-01"]+"/explain", nil); res.StatusCode != 200 || !strings.Contains(body, "Why this state") {
		t.Errorf("the account's own device: %d", res.StatusCode)
	}
	if res, _ := e.do("GET", "/ui/devices/01NOSUCHDEVICE00000000000/explain", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown id: %d", res.StatusCode)
	}
}

// L6: the restic version and the OS go through the same safe-display functions as the M3-T4 pages.
func TestM3T5L6_AHostileResticVersionAndOSNeverReachAPage(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET os_version = ?, restic_version = ? WHERE id = ?`, hostileTexts[4], hostileTexts[3], id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"board": "/", "devices": "/devices?size=100", "device summary": "/devices/" + id, "device settings": "/devices/" + id + "/settings",
		"customer": "/customers/" + e.customerID("Summit Eye Care")} {
		body := e.page(path, 200)
		for _, bad := range []string{hostileTexts[4], hostileTexts[3]} {
			if strings.Contains(body, bad) {
				t.Errorf("%s prints %q", name, bad)
			}
		}
	}
	if set := e.page("/devices/"+id+"/settings", 200); strings.Count(set, withheld) < 2 {
		t.Errorf("the settings facts should read %q for the OS and the restic version", withheld)
	}
}

// L7: a runs view applied on the board says it belongs to the runs page and links there.
func TestM3T5L7_ARunsViewOnTheBoardPointsToTheRunsPage(t *testing.T) {
	e := newEnv(t)
	e.login()
	v, err := e.st.CreateSavedViewAudited(context.Background(), store.ScopeAll(), store.SavedViewInput{Name: "Bad runs", Entity: "runs",
		Filter: json.RawMessage(`{"field":"outcome","op":"in","value":["failed"]}`)}, e.t5Actor())
	if err != nil {
		t.Fatal(err)
	}
	page := e.page("/?view="+v.ID, 200)
	mustContain(t, "runs view on the board", page, "belongs to the runs page", `href="/runs?view=`+v.ID+`"`)
	if sh, _ := showing(page); sh != "" {
		t.Errorf("a runs view must not filter the board (it says %q devices shown)", sh)
	}
	if !strings.Contains(page, "LAB-RETINA-02") || !strings.Contains(page, "FRONTDESK-01") {
		t.Error("with a runs view in the link every device still shows")
	}
	// and a device view on the runs page the other way round
	d := e.t5View("A device view", false)
	if body := e.page("/runs?view="+d, 200); !strings.Contains(body, "belongs to the board") {
		t.Error("a device view on the runs page should say it belongs to the board")
	}
}

// L7: the customer ring filter survives a failed Manage action, and is on every Manage form.
func TestM3T5L7_CustSurvivesAFailedManageAction(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.t5View("Mine", false)
	page := e.page("/?cust=crit&view="+id, 200)
	if n := strings.Count(page, `name="cust" value="crit"`); n < 3 {
		t.Errorf("only %d Manage forms carry the customer ring filter", n)
	}
	res := e.postForm("/ui/views/"+id+"/rename", url.Values{"entity": {"devices"}, "name": {" "}, "view": {id}, "cust": {"crit"}})
	if loc := e.location(res); !strings.Contains(loc, "cust=crit") || !strings.Contains(loc, "verr=") {
		t.Errorf("a failed rename returned to %q: the filter was lost", loc)
	}
}

// L7: the explain error does not cite a reason it has just replaced.
func TestM3T5L7_TheExplainErrorTextStandsOnItsOwn(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	saved := e.svc.m3.mux
	e.svc.m3.mux = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", http.StatusInternalServerError) })
	defer func() { e.svc.m3.mux = saved }()
	frag := e.page("/ui/devices/"+id+"/explain", 200)
	if !strings.Contains(frag, "could not be read") {
		t.Fatalf("the failure is not said: %q", frag)
	}
	if strings.Contains(frag, "shown before") || strings.Contains(frag, "page loaded") {
		t.Errorf("the error cites the reason it replaced: %q", frag)
	}
}

// L7: hovering a chip waits about 200 ms before it asks for the explanation.
func TestM3T5L7_TheExplainFetchWaitsForAHoverDelay(t *testing.T) {
	e := newEnv(t)
	e.login()
	board := e.page("/", 200)
	mustContain(t, "chip", board, `hx-trigger="mouseenter once delay:200ms, focus once delay:200ms"`)
}
