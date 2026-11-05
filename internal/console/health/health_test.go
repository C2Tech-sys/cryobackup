package health

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

type clk struct{ t time.Time }

func (c *clk) now() time.Time { return c.t }

func newEval(t *testing.T) (*Evaluator, *clk) {
	t.Helper()
	c := &clk{t: fixtures.Now()}
	st, err := store.OpenMemory(context.Background(), c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(context.Background(), st, c.t); err != nil {
		t.Fatal(err)
	}
	return &Evaluator{Store: st, Clock: c.now, MasterLoaded: func() bool { return true }}, c
}

func ids(b []state.Banner) string {
	var s []string
	for _, x := range b {
		s = append(s, x.ID)
	}
	return strings.Join(s, ",")
}

func TestFixturesFleetHasNoConsoleBannersWhenHealthy(t *testing.T) {
	e, _ := newEval(t)
	f, err := e.Fleet(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	// C00 (self-checks not wired) stays until store.LoadWorld supplies probe facts (M3-T2).
	if ids(f.View.Banners) != "C00" || !f.View.Complete {
		t.Fatalf("banners %s complete=%v", ids(f.View.Banners), f.View.Complete)
	}
	if f.View.AllClear() {
		t.Fatal("the fixture fleet has problems: AllClear must be false")
	}
	if f.Enrolled == 0 || f.InView != f.Enrolled || !f.Computed.Equal(fixtures.Now()) {
		t.Fatalf("stamp numbers: %d of %d at %v", f.InView, f.Enrolled, f.Computed)
	}
	if f.LastHeartbeat.IsZero() {
		t.Fatal("last heartbeat")
	}
}

func TestConsoleBanners(t *testing.T) {
	ctx := context.Background()
	e, c := newEval(t)

	e.MasterLoaded = nil
	f, _ := e.Fleet(ctx, store.ScopeAll())
	// C00 (self-checks not wired) stays until store.LoadWorld supplies probe facts (M3-T2).
	if ids(f.View.Banners) != "C12,C00" {
		t.Fatalf("no master key: %s", ids(f.View.Banners))
	}
	e.MasterLoaded = func() bool { return true }

	e.Maintain = func() MaintainStatus { return MaintainStatus{Known: true, Reachable: false} }
	f, _ = e.Fleet(ctx, store.ScopeAll())
	if ids(f.View.Banners) != "C07,C00" {
		t.Fatalf("maintain down: %s", ids(f.View.Banners))
	}
	e.Maintain = func() MaintainStatus { return MaintainStatus{} } // never probed: nothing raised
	if f, _ = e.Fleet(ctx, store.ScopeAll()); ids(f.View.Banners) != "C00" {
		t.Fatalf("unprobed worker must raise nothing: %s", ids(f.View.Banners))
	}

	// ADR 0013 (F5): silence from every agent, for 20 minutes or over a weekend,
	// raises no critical console banner. Devices change only their own state;
	// C04 comes from the self-probe of the agent endpoint.
	for _, quiet := range []time.Duration{20 * time.Minute, 4 * time.Hour, 60 * time.Hour} {
		c.t = fixtures.Now().Add(quiet)
		f, _ = e.Fleet(ctx, store.ScopeAll())
		for _, b := range f.View.Banners {
			if b.ID == "C04" || b.Severity >= state.SeverityCritical {
				t.Fatalf("fleet silent for %v: %s %q", quiet, b.ID, b.Text)
			}
		}
	}
	c.t = fixtures.Now()

	// C06 only when the evaluator job exists and stopped.
	if _, err := e.Store.AcquireJobLease(ctx, EvaluatorJob, "t", time.Second); err != nil {
		t.Fatal(err)
	}
	st := fixtures.Now().Add(-10 * time.Minute)
	if err := e.Store.RecordJobRun(ctx, EvaluatorJob, store.JobRun{Status: "ok", StartedAt: st, FinishedAt: st}); err != nil {
		t.Fatal(err)
	}
	f, _ = e.Fleet(ctx, store.ScopeAll())
	if ids(f.View.Banners) != "C06,C00" {
		t.Fatalf("stalled evaluator: %s", ids(f.View.Banners))
	}
}

func TestExtraAndOrdering(t *testing.T) {
	e, _ := newEval(t)
	e.Extra = func(now time.Time) []state.Banner { return fixtures.Banners(now) }
	e.MasterLoaded = nil
	f, err := e.Fleet(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	var sevDrop bool
	prev := state.SeverityCritical
	for _, b := range f.View.Banners {
		if b.Severity > prev {
			t.Fatalf("critical banners must come first: %s", ids(f.View.Banners))
		}
		if b.Severity < prev {
			sevDrop = true
		}
		prev = b.Severity
	}
	if !sevDrop {
		t.Fatal("expected both severities")
	}
	// Priority ids first within the critical group.
	got := strings.Split(ids(f.View.Banners), ",")
	want := []string{"C01", "C04", "C08", "C09", "C12", "C14"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("priority order: %v", got)
		}
	}
	if len(fixtures.Banners(fixtures.Now())) != 16 {
		t.Fatal("C01..C16")
	}
}

func TestEntityBanners(t *testing.T) {
	e, _ := newEval(t)
	f, _ := e.Fleet(context.Background(), store.ScopeAll())
	eb := EntityBanners(f.View)
	if len(eb) == 0 || eb[0].Kind != "missing" || eb[0].Customer.Name != "Blue Ridge Accounting" {
		t.Fatalf("entity banners: %+v", eb)
	}
	cs := state.CustomerState{Devices: []state.DeviceState{{Primary: state.StateUnseen}, {Primary: state.StateUnseen}, {Primary: state.StateUnseen}, {Primary: state.StateOK}, {Primary: state.StateOK}, {Primary: state.StateRetired}}}
	if s, n, is := Silence(cs); !is || s != 3 || n != 5 {
		t.Fatalf("silence: %d of %d %v", s, n, is)
	}
	cs.Devices = cs.Devices[:4]
	if _, _, is := Silence(cs); is {
		t.Fatal("fewer than 5 devices is never customer-wide silence")
	}
}

func TestTenDayStrips(t *testing.T) {
	e, _ := newEval(t)
	ctx := context.Background()
	f, _ := e.Fleet(ctx, store.ScopeAll())
	loc, _ := time.LoadLocation("America/Chicago")
	strips, err := TenDay(ctx, e.Store, store.ScopeAll(), f.View, fixtures.Now(), loc)
	if err != nil {
		t.Fatal(err)
	}
	byHost := map[string][StripDays]Day{}
	for _, d := range f.View.Devices {
		if s, ok := strips[d.ID]; ok {
			byHost[d.Hostname] = s
		}
	}
	if _, ok := byHost["OLD-RECEPTION-PC"]; ok {
		t.Fatal("retired devices have no strip")
	}
	// A parked device renders none from its pause day on (§4.7, M3-T2); the days before keep their runs.
	parked := byHost["XRAY-OLD-01"]
	if parked[StripDays-1].Kind != DayNone || parked[StripDays-2].Kind != DayNone {
		t.Fatalf("parked strip after the pause: %+v", parked)
	}
	// A healthy device has a run of green days ending yesterday or today.
	ok := byHost["OPS-ND-01"]
	var green int
	for _, c := range ok {
		if c.Kind == DaySuccess {
			green++
		}
	}
	if green < 7 || ok[StripDays-1].Date.Hour() != 0 {
		t.Fatalf("healthy strip: %+v", ok)
	}
	// FRONTDESK-01 failed three days in a row: failed cells at the right end.
	fd := byHost["FRONTDESK-01"]
	var failed int
	for _, c := range fd {
		if c.Kind == DayFailed || c.Kind == DayMixed {
			failed++
		}
	}
	if failed < 2 {
		t.Fatalf("failing device strip: %+v", fd)
	}
	// A device enrolled five days ago has leading none cells.
	scan := byHost["SCAN-STATION-1"]
	if scan[0].Kind != DayNone || scan[1].Kind != DayNone {
		t.Fatalf("new device strip: %+v", scan)
	}
	// Dates run oldest to newest.
	for i := 1; i < StripDays; i++ {
		if !ok[i].Date.After(ok[i-1].Date) {
			t.Fatal("cells must run oldest to newest")
		}
	}
}
