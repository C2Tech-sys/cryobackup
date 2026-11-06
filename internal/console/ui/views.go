package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// UserView is the signed-in admin in the sidebar.
type UserView struct {
	Username string
	Role     string
}

// SysItem is one line of the sidebar's system status.
type SysItem struct {
	Label string
	Value string
	Class string
}

// Stamp is the freshness stamp of the topbar (components §9.1).
type Stamp struct {
	State   string // fresh | partial | stale | unreachable | loading (words: Live, Partial, Polling, No data, Waiting)
	At      string // 1:42:10 PM, in the display zone
	ISO     string
	N, M    int
	Zone    string
	Tooltip string
	OOB     bool
}

// stampWords is the status word of each stamp state (components 9.1). fresh.js
// carries the same table; the two must agree so the page reads the same before
// and after the script runs.
var stampWords = map[string]string{
	"fresh":       "Live",
	"partial":     "Partial",
	"stale":       "Polling",
	"unreachable": "No data",
	"loading":     "Waiting",
}

// Word is the pill's status word. An unknown state falls back to the no-data
// word: a stamp must never read healthy by accident.
func (s Stamp) Word() string {
	if w, ok := stampWords[s.State]; ok {
		return w
	}
	return stampWords["unreachable"]
}

// Time is the pill's time text: "1:42:10 PM", "last 1:42:10 PM" when polling or
// "since 1:42:10 PM" when no data arrives (12-hour, in the display zone).
func (s Stamp) Time() string {
	switch s.State {
	case "stale":
		return "last " + s.At
	case "unreachable":
		return "since " + s.At
	}
	return s.At
}

// HasAgents reports whether the "n of m agents" segment is shown: only when the
// enrolled total is known.
func (s Stamp) HasAgents() bool { return s.M > 0 }

// Link is a banner action.
type Link struct {
	Href  string
	Label string
	// Admin marks a destination only an admin may open (forRole drops it for the others).
	Admin bool
}

// BannerView is a rendered condition banner.
type BannerView struct {
	ID     string
	Label  string // chip label
	Class  string // crit | warn
	Text   string
	Action *Link
}

// BannersView is the banner strip. Order is fixed (components 9.2, M3-T5 D): the
// first three critical banners (Items), then the rest of the critical ones
// behind a button whose count is always visible (Extra), then every warning
// (Warn). Critical banners are therefore never hidden behind warnings; up to
// warnInline warnings show as banners, more fold into one summary line that
// states their count and names. A warning is never removed.
type BannersView struct {
	Items []BannerView
	Extra []BannerView
	Warn  []BannerView
	OOB   bool
}

// warnInline is how many warning banners the strip shows in full; more fold.
const warnInline = 2

// WarnFold reports whether the warnings fold into one summary line.
func (v BannersView) WarnFold() bool { return len(v.Warn) > warnInline }

// WarnLabels names the folded warnings in one line, for the summary.
func (v BannersView) WarnLabels() string {
	l := make([]string, len(v.Warn))
	for i, b := range v.Warn {
		l[i] = b.Label
	}
	return strings.Join(l, " · ")
}

// Page is what the base template needs; page data structs embed it.
type Page struct {
	Title    string
	Nav      string
	User     *UserView
	CSRF     string
	Banners  BannersView
	Stamp    Stamp
	Refresh  int
	Events   string
	Fixtures bool
	Sys      []SysItem
	Zone     string
}

var bannerLabels = map[string]string{
	"C01": "GATEWAY UNREACHABLE", "C02": "STORAGE PROBE FAILED", "C03": "EMAIL NOT DELIVERING", "C04": "AGENT API UNREACHABLE",
	"C05": "CERTIFICATE EXPIRING", "C06": "ALERTING STALLED", "C07": "MAINTENANCE WORKER DOWN", "C08": "NO ACTIVE DEVICES",
	"C09": "PARTIAL DATA", "C10": "LIVE UPDATES DISCONNECTED", "C11": "DATABASE NEEDS ATTENTION", "C12": "MASTER KEY UNAVAILABLE",
	"C13": "CLOCK SUSPECT", "C14": "DIGEST NOT SENT", "C15": "STORAGE QUOTA", "C16": "RELEASE MANIFEST REJECTED",
	// ADR 0013 (M2-FIX-B2): C04 is now the self-probe of the agent endpoint; C04b is its not-checked warning.
	"C04b": "AGENT API NOT CHECKED",
}

// BannerLabel returns the chip label of a C-id.
func BannerLabel(id string) string {
	if l, ok := bannerLabels[id]; ok {
		return l
	}
	return "CONDITION " + id
}

// bannerView maps an engine banner to its view. Only banners whose destination
// exists in this milestone carry an action.
func bannerView(b state.Banner) BannerView { return bannerViewIn(b, time.Time{}, nil) }

// bannerViewIn is bannerView with the banner's clock time worded in loc (retime); nil leaves the engine's text.
func bannerViewIn(b state.Banner, now time.Time, loc *time.Location) BannerView {
	v := BannerView{ID: b.ID, Label: BannerLabel(b.ID), Class: ClassWarn, Text: retime(b.Text, b.At, now, loc)}
	if b.Severity >= state.SeverityCritical {
		v.Class = ClassCrit
	}
	if b.ID == "C08" && !strings.Contains(b.Text, "Nothing is being backed up") {
		v.Text += " Nothing is being backed up."
	}
	v.Action = bannerAction(b.ID)
	return v
}

// bannerAction is the action of a console banner (components 9.2). Every banner
// the engine raises has a row on the System page (adminapi healthRowBanners),
// so "Open System health" is where each one is explained, including C00 and the
// "b" not-checked warnings. The spec names "Open email settings" for C03 and
// C14; there is no mail settings page yet, so they open the System row that
// carries their evidence. C10 (live updates) is client-side and has none.
func bannerAction(id string) *Link {
	switch id {
	case "C09":
		return &Link{Href: "/?state=UNKNOWN", Label: "Show UNKNOWN devices"}
	case "C08":
		return &Link{Href: "/enroll", Label: "Enroll devices", Admin: true}
	case "C10":
		return nil
	}
	if strings.HasPrefix(id, "C") && len(id) >= 3 {
		return &Link{Href: "/system", Label: "Open System health"}
	}
	return nil
}

// forRole drops the actions a role cannot open: a read-only or operator account
// is never offered a link that answers 403.
func (v BannersView) forRole(role auth.Role) BannersView {
	if role >= auth.RoleAdmin {
		return v
	}
	strip := func(in []BannerView) []BannerView {
		out := make([]BannerView, len(in))
		for i, b := range in {
			if b.Action != nil && b.Action.Admin {
				b.Action = nil
			}
			out[i] = b
		}
		return out
	}
	v.Items, v.Extra, v.Warn = strip(v.Items), strip(v.Extra), strip(v.Warn)
	return v
}

// buildBanners merges the console banners with the entity banners.
func buildBanners(f health.Fleet, oob bool) BannersView { return buildBannersIn(f, oob, nil) }

// buildBannersIn is buildBanners with every clock time in a banner worded in loc, the reader's display zone.
func buildBannersIn(f health.Fleet, oob bool, loc *time.Location) BannersView {
	var crit, warn []BannerView
	for _, b := range f.View.Banners {
		v := bannerViewIn(b, f.Computed, loc)
		if v.Class == ClassCrit {
			crit = append(crit, v)
		} else {
			warn = append(warn, v)
		}
	}
	ents := health.EntityBanners(f.View)
	var missing []health.EntityBanner
	for _, e := range ents {
		switch e.Kind {
		case "missing":
			missing = append(missing, e)
		case "silence":
			crit = append(crit, BannerView{ID: "silence-" + e.Customer.ID, Label: "CUSTOMER-WIDE SILENCE", Class: ClassCrit,
				Text: fmt.Sprintf("%d of %d devices at %s have gone silent at once.", e.Silent, e.Total, e.Customer.Name)})
		}
	}
	switch len(missing) {
	case 0:
	case 1:
		crit = append(crit, BannerView{ID: "missing-" + missing[0].Customer.ID, Label: "MISSING CUSTOMER", Class: ClassCrit,
			Text: missing[0].Customer.Name + " is marked expected but has zero active devices: every machine there is unmonitored."})
	default:
		names := make([]string, len(missing))
		for i, m := range missing {
			names[i] = m.Customer.Name
		}
		crit = append(crit, BannerView{ID: "missing-many", Label: "MISSING CUSTOMER", Class: ClassCrit,
			Text: fmt.Sprintf("%d expected customers have no devices: %s.", len(missing), strings.Join(names, ", "))})
	}
	bv := BannersView{OOB: oob, Warn: warn}
	if len(crit) > 3 {
		bv.Items, bv.Extra = crit[:3:3], crit[3:]
	} else {
		bv.Items = crit
	}
	return bv
}

func (v BannersView) any() bool { return len(v.Items)+len(v.Extra)+len(v.Warn) > 0 }

// ---- board view models ----

// KPI is one tile.
type KPI struct {
	Label string
	Value string
	Unit  string
	Tone  string // "", good, warn, bad
	Glyph string // ok | warn | crit | ""
	SR    string
	Spark []int
	Na    bool
}

// RingSeg is one arc of a ring.
type RingSeg struct {
	Class  string
	Dash   string // stroke-dasharray
	Offset string // stroke-dashoffset
}

// LegendRow is one legend line of a ring.
type LegendRow struct {
	Class string
	Label string
	N     int
	// Href filters the board by this segment (B03); empty when there is nothing to show.
	Href    string
	Current bool
}

// Ring is a doughnut (components §4.4).
type Ring struct {
	Title   string
	Aria    string
	Center  string
	Caption string
	Segs    []RingSeg
	Legend  []LegendRow
	Alarm   bool
}

// StripCell is one day of the ten-day strip.
type StripCell struct {
	Kind  string
	Day   string // weekday initial (panel variant)
	Today bool
	Title string
}

// StripView is the ten-day strip of one device.
type StripView struct {
	Aria  string
	Href  string
	Cells []StripCell
	Panel bool
	Note  string
}

// DeviceRow is a table row.
type DeviceRow struct {
	ID       string
	Host     string
	Meta     string
	Chips    []Chip
	Badges   BadgeSet
	AgeText  string
	AgeSub   string
	AgeClass string
	AgeGlyph string
	Strip    StripView
	Agent    string
}

// GaugeView is the inline health gauge.
type GaugeView struct {
	Pct   int
	Class string
	Text  string
	Value string
}

// Group is a customer group of the board.
type Group struct {
	ID        string
	Name      string
	Variant   string // "", missing, onboarding, empty, unknown, silence
	CountText string
	Roll      []RollChip
	Status    *Chip
	Gauge     *GaugeView
	Aria      string
	Collapsed bool
	Rows      []DeviceRow
	Note      string
	// Retired is this customer's "Retired (n)" (M3-T6): never in Rows or any count.
	Retired *RetiredFold
}

// FilterView is the active board filter.
type FilterView struct {
	Active bool
	Chips  []FilterChip // each active filter, removable
	Shown  int
	Total  int
	// CustText is how many customers are on screen ("3 customers"): the customer ring counts customers, and a
	// customer with no devices has no rows to show for it.
	CustText string
}

// EmptyView is an empty-state panel (components §9.5).
type EmptyView struct {
	Kind  string // alarm | neutral | partial
	Title string
	Body  string
	Link  *Link
}

// BoardData is the fleet board.
type BoardData struct {
	Page
	KPIs    []KPI
	Rings   []Ring
	Groups  []Group
	Filter  FilterView
	Empty   *EmptyView
	QS      string
	AsOf    string
	Columns int
	// Views is the saved-views control (nil in the live fragment); ViewWarn says a view in the link could not be applied.
	Views    *ViewsControl
	ViewWarn *CauseView
	// viewLoaded is the saved view the link names, when it could be read.
	viewLoaded *store.SavedView
}

const ringCircumference = 2 * math.Pi * 46

func ringSegs(parts []RingSeg2) []RingSeg {
	total := 0
	for _, p := range parts {
		total += p.N
	}
	if total == 0 {
		return nil
	}
	var out []RingSeg
	cum := 0.0
	for _, p := range parts {
		if p.N == 0 {
			continue
		}
		length := float64(p.N) / float64(total) * ringCircumference
		gap := 2.0
		if len(parts) == 1 || length <= gap {
			gap = 0
		}
		out = append(out, RingSeg{Class: p.Class, Dash: fmt.Sprintf("%.2f %.2f", length-gap, ringCircumference-length+gap), Offset: fmt.Sprintf("%.2f", -cum)})
		cum += length
	}
	return out
}

// RingSeg2 is the input of ringSegs.
type RingSeg2 struct {
	Class string
	N     int
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// ageRing is the last-success age ring (components §4.4): under 24 h, 24 h to
// the stale limit, beyond or never, and parked. Retired devices are excluded.
func ageRing(f health.Fleet, c fmtCtx) Ring {
	stale := state.DefaultThresholds().StaleAfter
	var ok, warn, crit, parked int
	for _, d := range f.View.Devices {
		switch {
		case d.Primary == state.StateRetired:
		case d.Primary == state.StatePaused:
			parked++
		case d.Detail.LastSuccess.IsZero() || c.Now.Sub(d.Detail.LastSuccess) > stale:
			crit++
		case c.Now.Sub(d.Detail.LastSuccess) > 24*time.Hour:
			warn++
		default:
			ok++
		}
	}
	total := ok + warn + crit + parked
	r := Ring{Title: "Last successful backup"}
	h := int(stale / time.Hour)
	r.Legend = []LegendRow{
		{Class: ClassOK, Label: "under 24 h", N: ok},
		{Class: ClassWarn, Label: fmt.Sprintf("24–%d h", h), N: warn},
		{Class: ClassCrit, Label: fmt.Sprintf("over %d h or never", h), N: crit},
		{Class: ClassNeutral, Label: "parked", N: parked},
	}
	switch {
	case total == 0:
		r.Alarm, r.Center, r.Caption = true, "0", "none yet"
		r.Aria = "Backup age ring: no devices enrolled"
	case ok+warn+crit == 0:
		r.Center, r.Caption = "n/a", "all parked"
		r.Segs = ringSegs([]RingSeg2{{ClassNeutral, parked}})
		r.Aria = "Backup age ring: every device is parked"
	default:
		r.Center, r.Caption = strconv.Itoa(total), "devices"
		r.Segs = ringSegs([]RingSeg2{{ClassOK, ok}, {ClassWarn, warn}, {ClassCrit, crit}, {ClassNeutral, parked}})
		r.Aria = fmt.Sprintf("Backup age ring: %d under 24 hours, %d between 24 and %d hours, %d over or never, %d parked", ok, warn, h, crit, parked)
	}
	return r
}

// customerRing is the customer health ring.
func customerRing(f health.Fleet) Ring {
	var ok, warn, crit, other int
	for _, c := range f.View.Customers {
		switch c.Status {
		case state.CustomerOK:
			ok++
		case state.CustomerWarn:
			warn++
		case state.CustomerAlarm, state.CustomerMissing, state.CustomerUnknown:
			crit++
		default:
			other++
		}
	}
	total := ok + warn + crit + other
	r := Ring{Title: "Customer health", Legend: []LegendRow{
		{Class: ClassOK, Label: "fully within SLA", N: ok}, {Class: ClassWarn, Label: "has warnings", N: warn}, {Class: ClassCrit, Label: "has critical or missing", N: crit}, {Class: ClassNeutral, Label: "onboarding or empty", N: other}}}
	if total == 0 {
		r.Alarm, r.Center, r.Caption, r.Aria = true, "0", "none yet", "Customer health ring: no customers"
		return r
	}
	r.Center, r.Caption = strconv.Itoa(total), "customers"
	r.Segs = ringSegs([]RingSeg2{{ClassOK, ok}, {ClassWarn, warn}, {ClassCrit, crit}, {ClassNeutral, other}})
	r.Aria = fmt.Sprintf("Customer health ring: %d within SLA, %d with warnings, %d critical or missing, %d onboarding or empty", ok, warn, crit, other)
	return r
}

// kpis computes the five tiles of the board from the evaluated fleet.
func kpis(f health.Fleet, c fmtCtx, running int) []KPI {
	var devices, active, healthy, critical, warnings, today int
	for _, d := range f.View.Devices {
		if d.Primary == state.StateRetired {
			continue
		}
		devices++
		if d.Primary != state.StatePaused {
			active++
			if d.Alarm == state.SeverityNone && !d.Detail.LastSuccess.IsZero() {
				healthy++
			}
		}
		switch {
		case d.Alarm == state.SeverityCritical:
			critical++
		case d.Alarm == state.SeverityAlarm, d.Alarm == state.SeverityNone && ClassOf(d.Primary) == ClassWarn:
			warnings++
		}
		if !d.Detail.LastSuccess.IsZero() && c.Now.Sub(d.Detail.LastSuccess) <= 24*time.Hour {
			today++
		}
	}
	out := []KPI{}
	dev := KPI{Label: "Devices", Value: strconv.Itoa(devices), Unit: plural(len(f.View.Customers), "customer", "customers"), SR: plural(devices, "device", "devices")}
	if devices == 0 {
		dev.Tone, dev.Glyph = "bad", ClassCrit
	}
	out = append(out, dev)
	sla := KPI{Label: "Within SLA"}
	if active == 0 {
		sla.Value, sla.Na, sla.SR = "n/a", true, "not available: no active devices"
	} else {
		pct := 100 * float64(healthy) / float64(active)
		sla.Value, sla.Unit, sla.SR = fmt.Sprintf("%.1f", pct), "%", fmt.Sprintf("%.1f percent of %s within SLA", pct, plural(active, "active device", "active devices"))
		switch {
		case pct >= 99.95:
			sla.Tone, sla.Glyph = "good", ClassOK
		case healthy == active:
			sla.Tone, sla.Glyph = "good", ClassOK
		case pct >= 90:
			sla.Tone, sla.Glyph = "warn", ClassWarn
		default:
			sla.Tone, sla.Glyph = "bad", ClassCrit
		}
	}
	out = append(out, sla)
	crit := KPI{Label: "Critical", Value: strconv.Itoa(critical), SR: plural(critical, "device", "devices") + " in a critical state"}
	if critical > 0 {
		crit.Tone, crit.Glyph = "bad", ClassCrit
	} else {
		crit.Tone, crit.Glyph = "good", ClassOK
	}
	out = append(out, crit)
	warn := KPI{Label: "Warnings", Value: strconv.Itoa(warnings), SR: plural(warnings, "device", "devices") + " with a warning"}
	if warnings > 0 {
		warn.Tone, warn.Glyph = "warn", ClassWarn
	}
	out = append(out, warn)
	bt := KPI{Label: "Backed up today", Value: strconv.Itoa(today), SR: plural(today, "device", "devices") + " backed up in the last 24 hours"}
	if running > 0 {
		bt.Unit = fmt.Sprintf("+ %d running", running)
	}
	out = append(out, bt)
	return out
}

// weekdayInitial for the panel strip.
func weekdayInitial(t time.Time) string { return t.Weekday().String()[:1] }

func stripView(id string, cells [health.StripDays]health.Day, paused bool, panel bool, c fmtCtx) StripView {
	v := StripView{Href: "/devices/" + id, Panel: panel}
	var ok, mixed, failed, missed int
	for i, cell := range cells {
		sc := StripCell{Kind: string(cell.Kind), Day: weekdayInitial(cell.Date), Today: i == health.StripDays-1}
		switch cell.Kind {
		case health.DaySuccess:
			ok++
		case health.DayMixed:
			mixed++
		case health.DayFailed:
			failed++
		case health.DayMissed:
			missed++
		}
		sc.Title = cell.Date.Format("Mon 2 Jan") + ": " + cellWord(cell)
		v.Cells = append(v.Cells, sc)
	}
	last := cells[health.StripDays-1]
	v.Aria = fmt.Sprintf("Last 10 days, oldest first: %d success, %d mixed, %d failed, %d missed; today %s", ok, mixed, failed, missed, cellWord(last))
	if paused {
		v.Note = "Parked"
	}
	return v
}

func cellWord(c health.Day) string {
	switch c.Kind {
	case health.DaySuccess:
		return plural(c.Success, "success", "successes")
	case health.DayMixed:
		return fmt.Sprintf("%d succeeded, %d failed", c.Success, c.Failed)
	case health.DayFailed:
		return plural(c.Failed, "failure", "failures")
	case health.DayMissed:
		return "no run"
	}
	return "no data"
}

// deviceRow builds a table row.
func deviceRow(ds state.DeviceState, meta deviceMeta, strips map[string][health.StripDays]health.Day, run RunInfo, c fmtCtx) DeviceRow {
	row := DeviceRow{ID: ds.ID, Host: ds.Hostname, Chips: ChipsFor(ds, c, run), Badges: BadgesFor(ds, c), Agent: agentLabel(ds.Detail.AgentVersion)}
	var bits []string
	if meta.OS != "" {
		bits = append(bits, osLabel(meta.OS))
	}
	if meta.Class != "" && meta.Class != "unknown" {
		bits = append(bits, meta.Class)
	}
	row.Meta = strings.Join(bits, " · ")
	cls := ClassOf(ds.Primary)
	switch {
	case !ds.Detail.LastSuccess.IsZero():
		row.AgeText = ageLong(c.Now.Sub(ds.Detail.LastSuccess))
	case ds.Primary == state.StatePendingFirst || ds.Primary == state.StateBlank:
		row.AgeText = "never"
		if !meta.Enrolled.IsZero() {
			row.AgeSub = "enrolled " + strings.TrimSuffix(ageLong(c.Now.Sub(meta.Enrolled)), " ago") + " ago"
		}
	default:
		row.AgeText = "never"
	}
	switch cls {
	case ClassCrit:
		row.AgeClass, row.AgeGlyph = "bad", ClassCrit
	case ClassWarn:
		row.AgeClass, row.AgeGlyph = "warn", ClassWarn
	}
	if cells, ok := strips[ds.ID]; ok {
		row.Strip = stripView(ds.ID, cells, ds.Primary == state.StatePaused, false, c)
	}
	return row
}

// deviceMeta is the part of a store.Device the table shows.
type deviceMeta struct {
	OS       string
	Class    string
	Enrolled time.Time
}

// groupView builds a customer group.
func groupView(cs state.CustomerState, rows []DeviceRow, c fmtCtx) Group {
	g := Group{ID: "cust-" + cs.ID, Name: cs.Name, Rows: rows, Roll: RollupChips(cs), Status: StatusChip(cs)}
	nonRetired := cs.Active + cs.Paused
	switch cs.Status {
	case state.CustomerMissing:
		g.Variant, g.CountText = "missing", "0 devices · expected"
		g.Aria = "Missing: " + cs.Name + " is expected but has no devices"
		g.Roll = nil
	case state.CustomerOnboarding:
		g.Variant, g.CountText, g.Roll = "onboarding", "0 devices · onboarding", nil
		g.Aria = cs.Name + ": onboarding"
	case state.CustomerEmpty:
		g.Variant, g.CountText, g.Roll = "empty", "0 devices", nil
		g.Aria = cs.Name + ": no devices"
	default:
		g.CountText = plural(nonRetired, "device", "devices")
		g.Aria = fmt.Sprintf("%s, %s", cs.Name, plural(nonRetired, "device", "devices"))
		if cs.Status == state.CustomerUnknown {
			g.Variant, g.Status = "unknown", nil
		}
		if s, n, is := health.Silence(cs); is {
			g.Variant = "silence"
			g.Note = fmt.Sprintf("CUSTOMER-WIDE SILENCE · %d of %d unseen", s, n)
			g.Aria = fmt.Sprintf("Customer-wide silence: %d of %d devices unseen at %s", s, n, cs.Name)
		}
		if cs.HealthKnown {
			pct := int(cs.HealthPct + 0.5)
			cl := ClassOK
			switch cs.Status {
			case state.CustomerWarn:
				cl = ClassWarn
			case state.CustomerAlarm, state.CustomerUnknown, state.CustomerMissing:
				cl = ClassCrit
			}
			g.Gauge = &GaugeView{Pct: pct, Class: cl, Value: strconv.Itoa(pct) + "%", Text: fmt.Sprintf("%d percent of devices within SLA", pct)}
		} else {
			g.Gauge = &GaugeView{Class: ClassNeutral, Value: "n/a", Text: "not available"}
		}
		g.Collapsed = cs.Status == state.CustomerOK
	}
	return g
}
