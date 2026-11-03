package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// designSections are the sections of /design, one per group of components of
// components.md §12 (the primitives the M0 screens use).
var designSections = []DesignSection{
	{"tokens", "Foundations", "step 1 · tokens, type roles, contrast"},
	{"surfaces", "Surfaces", "step 2 · B01 B07 B17"},
	{"controls", "Controls", "step 3 · S01 B17"},
	{"state", "State language", "step 4 · B01 B07"},
	{"live", "Live and degraded", "step 5 · every screen"},
	{"data", "Data table and customer groups", "step 6 · B01"},
	{"visuals", "Visuals", "step 7 · B01 B07"},
	{"runs", "Run views", "step 8 · B07 B17"},
	{"shell", "App shell", "step 9 · every screen"},
}

// DesignSection names a /design section.
type DesignSection struct {
	Key   string
	Title string
	Used  string
}

// SampleDeviceState builds a deterministic DeviceState for a primary state: the
// catalogue examples of components §3.1 (FAILED x3, UNSEEN 9d, STALE 4d ...).
func SampleDeviceState(st state.State, now time.Time) state.DeviceState {
	ds := state.DeviceState{ID: "sample-" + strings.ToLower(string(st)), CustomerID: "sample", Hostname: "FRONTDESK-01", Primary: st, Flags: []state.State{st}}
	d := &ds.Detail
	switch st {
	case state.StateRetired:
		ds.Since = now.AddDate(0, 0, -18)
	case state.StatePaused:
		ds.Since, d.PauseUntil, d.PauseReason = now.Add(-2*time.Hour), now.Add(30*time.Hour), "awaiting replacement"
	case state.StateUnknown:
		d.LoadErrorClass = "timeout"
	case state.StatePendingFirst:
		ds.Since, d.GraceDeadline = now.Add(-31*time.Hour), now.Add(41*time.Hour)
	case state.StateBlank:
		ds.Since, ds.Reason, d.ErrorClass = now.Add(-24*time.Hour), state.ReasonOnlyFailures, "permission"
		d.GraceDeadline = now.Add(-24 * time.Hour)
	case state.StateStuck:
		ds.Since, d.OpenRunAge, d.OpenRunLimit = now.Add(-76*time.Hour), 76*time.Hour, 12*time.Hour
	case state.StateFailed:
		ds.Since, d.ConsecutiveFailures, d.ErrorClass = now.Add(-50*time.Hour), 3, "vss"
	case state.StatePartial:
		ds.Since, d.FailedFiles = now.Add(-6*time.Hour), 23
		d.ErrorCounts = []state.ClassCount{{Class: "permission", N: 23}}
	case state.StateUnseen:
		ds.Since, d.LastSeen = now.Add(-9*24*time.Hour), now.Add(-9*24*time.Hour)
	case state.StateStale:
		ds.Since, d.LastSuccess = now.Add(-24*time.Hour), now.Add(-4*24*time.Hour)
	case state.StateEscrowUnverified:
		d.EscrowIssue = state.ReasonEscrowNeverVerified
	case state.StateMaintFailed:
		d.MaintOp = "prune"
	case state.StateDeferred:
		ds.Since, d.DeferredReason = now.Add(-3*time.Hour), "battery"
	case state.StateOutdated:
		d.AgentVersion, d.ExpectedVersion = "1.4.2", "1.5.0"
	case state.StateOK:
		d.LastSuccess = now.Add(-5 * time.Hour)
	}
	return ds
}

// SampleChips returns the primary chip of every state in precedence order, built
// from SampleDeviceState. The golden file testdata/state_chips.golden pins it.
func SampleChips(now time.Time, loc *time.Location) []Chip {
	c := fmtCtx{Now: now, Loc: loc}
	var out []Chip
	for _, st := range state.Precedence() {
		out = append(out, ChipsFor(SampleDeviceState(st, now), c, RunInfo{})[0])
	}
	return out
}

// designCat is a row of the state catalogue.
type designCat struct {
	Row     CatalogueRow
	Chip    Chip
	Fixture []Chip
	Host    string
}

type designBadge struct {
	Name string
	Set  BadgeSet
}

type designStamp struct {
	Stamp Stamp
	Note  string
}

type designPanel struct {
	Name string
	View EmptyView
}

type designKPISet struct {
	Name string
	KPIs []KPI
}

type designRing struct {
	Name string
	Ring Ring
}

type designGauge struct {
	Name string
	G    GaugeView
}

type designGroup struct {
	Name  string
	Group Group
}

type designProgress struct {
	Name string
	P    ProgressView
}

// DesignData is the /design page.
type DesignData struct {
	Page
	Only      string
	Sections  []DesignSection
	Checks    []Check
	Contrast  []ContrastRow
	Tokens    []string
	Catalogue []designCat
	Outcomes  []Chip
	Running   []Chip
	Badges    []designBadge
	Rolls     []RollChip
	Stamps    []designStamp
	CBanners  []BannerView
	Entity    BannersView
	Panels    []designPanel
	KPISets   []designKPISet
	Strips    []StripView
	Rings     []designRing
	Gauges    []designGauge
	Board     BoardData
	Variants  []designGroup
	Progress  []designProgress
	Runs      []RunRow
	Steps     []Step
	Facts     []Fact
}

func sampleStrip(panel bool, now time.Time) StripView {
	kinds := []health.DayKind{health.DayNone, health.DayNone, health.DaySuccess, health.DaySuccess, health.DayMixed, health.DayMissed, health.DayFailed, health.DaySuccess, health.DaySuccess, health.DayNone}
	var cells [health.StripDays]health.Day
	for i, k := range kinds {
		cells[i] = health.Day{Kind: k, Date: now.AddDate(0, 0, i-(health.StripDays-1)), Success: 2, Failed: 1}
	}
	return stripView("sample", cells, false, panel, fmtCtx{Now: now})
}

func (s *Service) designData(ctx context.Context, only string) DesignData {
	now := s.now()
	c := s.fc(ctx)
	d := DesignData{Only: only, Sections: designSections}
	d.Title = "Design check"
	d.Nav, d.Refresh, d.Fixtures, d.Zone = "design", 3600, true, s.zone(ctx)
	d.Sys = s.sysItems()
	d.Stamp = Stamp{State: "fresh", At: c.clock(now), ISO: now.Format(time.RFC3339), N: 26, M: 26, Zone: s.zone(ctx), Tooltip: "Fixtures clock, frozen"}
	d.Checks = []Check{TokenCoverage(), OrbitronAudit()}
	rows, cc := ContrastReport()
	d.Contrast = rows
	d.Checks = append(d.Checks, cc)
	d.Tokens = []string{"bg", "surface", "surface-raised", "border", "border-input", "text", "text-muted", "accent", "on-accent", "danger", "warning", "success", "on-status"}

	// The state catalogue: sample chips, and the real classifier output for the fixture fleet.
	fixtureChips := map[state.State][]Chip{}
	fixtureHosts := map[state.State]string{}
	var fleet health.Fleet
	if f, err := s.Fleet.Fleet(ctx, store.ScopeAll()); err == nil {
		fleet = f
		for _, ds := range f.View.Devices {
			if _, seen := fixtureChips[ds.Primary]; !seen {
				fixtureChips[ds.Primary] = ChipsFor(ds, c, RunInfo{})
				fixtureHosts[ds.Primary] = ds.Hostname
			}
		}
	} else {
		s.log().Warn("design: fixtures fleet unavailable", logx.Err(err))
	}
	samples := SampleChips(now, s.loc(ctx))
	for i, row := range Catalogue() {
		d.Catalogue = append(d.Catalogue, designCat{Row: row, Chip: samples[i], Fixture: fixtureChips[row.State], Host: fixtureHosts[row.State]})
	}
	d.Outcomes = OutcomeCatalogue()
	d.Running = []Chip{RunningChip("d1", 0.62), func() Chip { x := RunningChip("d2", -1); x.Small = true; return x }()}

	failed := SampleDeviceState(state.StateFailed, now)
	failed.Attrs = []state.Attr{state.AttrOffline, state.AttrClockSkew, state.AttrVSSFallback, state.AttrAcknowledged, state.AttrNoSchedule}
	failed.Flags = []state.State{state.StateFailed, state.StateStale, state.StateEscrowUnverified}
	failed.Detail.ClockOffset, failed.Detail.VSSVolumes = 4*time.Minute, []string{"C"}
	d.Badges = []designBadge{
		{"Several badges (three shown, the rest behind +N)", BadgesFor(failed, c)},
		{"Customer-held key", BadgesFor(state.DeviceState{Attrs: []state.Attr{state.AttrCustomerHeldKey}}, c)},
		{"Partial under tolerance", BadgesFor(state.DeviceState{Attrs: []state.Attr{state.AttrPartialUnderTolerance}}, c)},
	}
	d.Rolls = RollupChips(state.CustomerState{Counts: map[state.State]int{state.StateFailed: 2, state.StateUnseen: 1, state.StateMissed: 1, state.StateOK: 27}})

	at := c.clock(now)
	d.Stamps = []designStamp{
		{Stamp{State: "fresh", At: at, N: 26, M: 26}, "Live: events arriving, every enrolled agent in view"},
		{Stamp{State: "partial", At: at, N: 207, M: 212}, "207 of 212 agents in view: links to the C09 banner"},
		{Stamp{State: "stale", At: at, N: 26, M: 26}, "Polling: no event or poll within 2 x the refresh interval; time is the last data"},
		{Stamp{State: "unreachable", At: at}, "No data: polls failing, rendered client-side; time is since when"},
		{Stamp{State: "loading", At: "--:--:--"}, "first paint: neutral ring, never a green dot"},
	}
	// The agent endpoint conditions (ADR 0013) render every variant: C04 at
	// each failing stage, C05 warn and critical, and the C04b not-checked
	// warning, in place of the single canned C04 and C05.
	cb := health.EndpointExamples(now)
	for _, b := range fixtures.Banners(now) {
		if b.ID != health.BannerEndpointFailing && b.ID != health.BannerCertExpiring {
			cb = append(cb, b)
		}
	}
	for _, b := range health.SortBanners(cb) {
		d.CBanners = append(d.CBanners, bannerView(b))
	}
	d.Entity = BannersView{Items: []BannerView{
		{ID: "e1", Label: "MISSING CUSTOMER", Class: ClassCrit, Text: "Blue Ridge Accounting is marked expected but has zero active devices: every machine there is unmonitored."},
		{ID: "e2", Label: "CUSTOMER-WIDE SILENCE", Class: ClassCrit, Text: "38 of 44 devices at Summit Eye Care have gone silent at once."},
	}}
	d.Panels = []designPanel{
		{"First run (the only neutral no-data case)", EmptyView{Kind: "neutral", Title: "No customers yet", Body: "Create the first customer and an enrollment token with: cryo-console token --create-customer \"Customer name\"."}},
		{"No data where data must exist (alarm)", EmptyView{Kind: "alarm", Title: "No devices are enrolled", Body: "Nothing is being backed up. Enroll a customer or check the agent listener."}},
		{"Partial data", EmptyView{Kind: "partial", Title: "Partial data", Body: "5 devices could not be classified. They show as UNKNOWN and count as unhealthy."}},
		{"Filter-caused empty (neutral, with the denominator)", EmptyView{Kind: "neutral", Title: "No devices match these filters", Body: "0 of 26 shown.", Link: &Link{Href: "/design/data", Label: "Clear filters"}}},
		{"Checked and clear (neutral, with its evidence)", EmptyView{Kind: "neutral", Title: "Nothing overdue", Body: "26 devices checked at " + c.clock(now) + "."}},
	}

	live := fleet
	d.KPISets = []designKPISet{{"Fixture fleet", kpis(live, c, 1)}, {"Empty fleet (alarm tone)", kpis(health.Fleet{}, c, 0)}}
	d.KPISets[0].KPIs[0].Spark = []int{60, 70, 65, 80, 78, 90, 100}
	d.KPISets[0].KPIs[2].Spark = []int{30, 20, 60, 50, 25, 40, 70}
	d.Strips = []StripView{sampleStrip(false, now), sampleStrip(true, now)}
	paused := health.Fleet{View: state.FleetView{Devices: []state.DeviceState{{Primary: state.StatePaused}}}}
	d.Rings = []designRing{{"Fixture fleet", ageRing(live, c)}, {"Zero devices (alarm ring, never neutral)", ageRing(health.Fleet{}, c)}, {"Only parked devices (n/a)", ageRing(paused, c)}, {"Customers", customerRing(live)}}
	d.Gauges = []designGauge{
		{"within SLA", GaugeView{Pct: 94, Class: ClassOK, Value: "94%", Text: "94 percent of devices within SLA"}},
		{"warn rollup", GaugeView{Pct: 80, Class: ClassWarn, Value: "80%", Text: "80 percent of devices within SLA"}},
		{"critical rollup", GaugeView{Pct: 41, Class: ClassCrit, Value: "41%", Text: "41 percent of devices within SLA"}},
		{"zero denominator", GaugeView{Class: ClassNeutral, Value: "n/a", Text: "not available"}},
	}

	// The board and the customer groups, from the fixtures fleet and from synthetic variants.
	p := &auth.Principal{Scope: store.ScopeAll(), Role: auth.RoleReadOnly}
	d.Board = s.boardData(ctx, p, live, "", "", false)
	silent := []state.DeviceState{}
	for i := 0; i < 6; i++ {
		st := state.StateUnseen
		if i >= 4 {
			st = state.StateOK
		}
		silent = append(silent, SampleDeviceState(st, now))
	}
	row := func(st state.State) []DeviceRow {
		return []DeviceRow{deviceRow(SampleDeviceState(st, now), deviceMeta{OS: "Windows 11 Pro", Class: "desktop"}, nil, RunInfo{}, c)}
	}
	cs := func(name string, status state.CustomerStatus, counts map[state.State]int, active int, devs []state.DeviceState) state.CustomerState {
		x := state.CustomerState{ID: "d-" + strings.ToLower(strings.Fields(name)[0]), Name: name, Status: status, Counts: counts, Active: active, Devices: devs}
		if active > 0 {
			x.HealthKnown, x.HealthPct = true, 100*float64(counts[state.StateOK])/float64(active)
		}
		return x
	}
	d.Variants = []designGroup{
		{"Expanded, with exceptions", groupView(cs("Summit Eye Care", state.CustomerAlarm, map[state.State]int{state.StateFailed: 1, state.StateOK: 3}, 4, nil), append(row(state.StateFailed), row(state.StateOK)...), c)},
		{"Collapsed healthy row", groupView(cs("Northwind Dental", state.CustomerOK, map[state.State]int{state.StateOK: 12}, 12, nil), row(state.StateOK), c)},
		{"MISSING (expected, no devices)", groupView(cs("Blue Ridge Accounting", state.CustomerMissing, nil, 0, nil), nil, c)},
		{"ONBOARDING", groupView(cs("Lakeshore Pediatrics", state.CustomerOnboarding, nil, 0, nil), nil, c)},
		{"EMPTY (never green)", groupView(cs("Example Empty", state.CustomerEmpty, nil, 0, nil), nil, c)},
		{"Contains UNKNOWN devices", groupView(cs("Harbor Family", state.CustomerUnknown, map[state.State]int{state.StateUnknown: 2, state.StateOK: 3}, 5, nil), row(state.StateUnknown), c)},
		{"Customer-wide silence", groupView(cs("Pinecrest Legal", state.CustomerAlarm, map[state.State]int{state.StateUnseen: 4, state.StateOK: 2}, 6, silent), row(state.StateUnseen), c)},
	}

	prog := progressView("sample-run", progressDoc{Kind: "backup", Pct: 0.62, Files: 23418, TotalFiles: 37902, Bytes: 4400000000}, now.Add(-90*time.Minute), c)
	ind := progressView("sample-ind", progressDoc{Kind: "backup"}, now.Add(-5*time.Minute), c)
	stall := progressView("sample-stall", progressDoc{Kind: "backup", Pct: 0.31, Files: 100, TotalFiles: 300, Stalled: true}, now.Add(-8*time.Hour), c)
	doneChip := OutcomeChip("sample-done", "success", false)
	done := ProgressView{ID: "run-sample-done", RunID: "sample-done", Done: true, Outcome: &doneChip, Summary: "312 new · 1.1 GB added"}
	d.Progress = []designProgress{{"Running", prog}, {"Scanning (no total yet)", ind}, {"Stalled", stall}, {"Done (collapses to the outcome chip)", done}}
	d.Runs = []RunRow{
		{ID: "r1", Date: "Sat 2:10 AM", DateISO: now.Format(time.RFC3339), Chip: OutcomeChip("h1", "failed", false), Summary: "vss · 1 volume", Exit: "exit 3", Duration: "2m 04s"},
		{ID: "r2", Date: "Fri 2:10 AM", DateISO: now.Format(time.RFC3339), Chip: OutcomeChip("h2", "partial", false), Summary: "4 files unreadable", Exit: "exit 3", Duration: "15m 18s"},
		{ID: "r3", Date: "Thu 2:10 AM", DateISO: now.Format(time.RFC3339), Chip: OutcomeChip("h3", "success", false), Summary: "312 new · 1.1 GB added", Exit: "exit 0", Duration: "14m 40s"},
	}
	d.Steps = []Step{{1, "Snapshot", "done"}, {2, "Browse and select", "on"}, {3, "Mode", "todo"}, {4, "Progress", "todo"}}
	d.Facts = []Fact{{Key: "Policy", Value: "v12"}, {Key: "Schedule", Value: "every 4 h in Mon-Fri/7-19 · catch-up on", Mono: true}, {Key: "Repository", Value: "61 snapshots · latest 6 h ago", Mono: true}}
	return d
}

// design serves /design and /design/{component}; it is registered in fixtures mode only.
func (s *Service) design(w http.ResponseWriter, r *http.Request) {
	only := r.PathValue("component")
	if only != "" {
		ok := false
		for _, sec := range designSections {
			ok = ok || sec.Key == only
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
	}
	s.render(w, r, http.StatusOK, "design", "base", s.designData(r.Context(), only))
}
