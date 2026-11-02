package ui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Classes of the state language (components.md §0.1): four classes plus neutral.
const (
	ClassOK      = "ok"
	ClassInfo    = "info"
	ClassWarn    = "warn"
	ClassCrit    = "crit"
	ClassNeutral = "neutral"
)

// Chip is one rendered state chip. The label and class come from the
// catalogue, which is golden-tested against state.Precedence(); the template
// does no classification of its own (components §3.1 notes).
type Chip struct {
	ID     string
	State  state.State // "" for an overlay or an outcome chip
	Label  string
	Class  string
	Qual   string // qualifier ("9d", "x3", "battery"), already formatted
	Joiner string // " " or " . " between label and qualifier
	Title  string // first line of the reason popover
	Reason string // second line
	Pulse  bool
	Dashed bool
	Small  bool
}

// Text is the label with its qualifier, as shown.
func (c Chip) Text() string {
	if c.Qual == "" {
		return c.Label
	}
	return c.Label + c.Joiner + c.Qual
}

type chipSpec struct {
	Label string
	Class string
}

// catalogue is components.md §3.1: the 17 primary states in precedence order.
var catalogue = map[state.State]chipSpec{
	state.StateRetired:          {"RETIRED", ClassNeutral},
	state.StatePaused:           {"PARKED", ClassNeutral},
	state.StateUnknown:          {"UNKNOWN", ClassCrit},
	state.StateIdentityConflict: {"IDENTITY CONFLICT", ClassCrit},
	state.StatePendingFirst:     {"PENDING FIRST", ClassInfo},
	state.StateBlank:            {"NEVER BACKED UP", ClassCrit},
	state.StateStuck:            {"STUCK", ClassCrit},
	state.StateFailed:           {"FAILED", ClassCrit},
	state.StatePartial:          {"PARTIAL", ClassWarn},
	state.StateUnseen:           {"UNSEEN", ClassCrit},
	state.StateStale:            {"STALE", ClassCrit},
	state.StateEscrowUnverified: {"ESCROW UNVERIFIED", ClassWarn},
	state.StateMaintFailed:      {"RETENTION FAILED", ClassWarn},
	state.StateMissed:           {"MISSED RUN", ClassWarn},
	state.StateDeferred:         {"DEFERRED", ClassWarn},
	state.StateOutdated:         {"OUTDATED AGENT", ClassWarn},
	state.StateOK:               {"OK", ClassOK},
}

// CatalogueRow is one line of the state-chip catalogue.
type CatalogueRow struct {
	State state.State
	Label string
	Class string
}

// Catalogue returns the catalogue in precedence order (state.Precedence()). The
// golden test compares it with testdata/state_chips.golden; /design renders it.
func Catalogue() []CatalogueRow {
	prec := state.Precedence()
	out := make([]CatalogueRow, 0, len(prec))
	for _, s := range prec {
		spec, ok := catalogue[s]
		if !ok {
			// A state the engine learned that the UI has not: fail loud, never OK.
			spec = chipSpec{"UNKNOWN", ClassCrit}
		}
		out = append(out, CatalogueRow{State: s, Label: spec.Label, Class: spec.Class})
	}
	return out
}

// ClassOf maps a primary state to its chip class; an unrecognised state is crit.
func ClassOf(s state.State) string {
	if spec, ok := catalogue[s]; ok {
		return spec.Class
	}
	return ClassCrit
}

// fmtCtx carries what formatting needs.
type fmtCtx struct {
	Now time.Time
	Loc *time.Location
	// Zone is the IANA name of Loc, taken from the same display snapshot
	// (M2-W3E), so a view that names the zone never asks for it separately.
	Zone string
}

func (c fmtCtx) loc() *time.Location {
	if c.Loc == nil {
		return time.UTC
	}
	return c.Loc
}

// ageShort is "9d", "36h", "12m": a compact age for qualifiers.
func ageShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 48*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
}

// ageLong is "40 s ago", "12 min ago", "6 h ago", "3 d ago".
func ageLong(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + " s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h ago"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + " d ago"
	}
}

func (c fmtCtx) day(t time.Time) string     { return t.In(c.loc()).Format("2 Jan") }
func (c fmtCtx) dayTime(t time.Time) string { return t.In(c.loc()).Format("Mon 3:04 PM") }
func (c fmtCtx) clock(t time.Time) string   { return t.In(c.loc()).Format("3:04:05 PM") }
func (c fmtCtx) fullTime(t time.Time) string {
	return t.In(c.loc()).Format("Mon 2 Jan 2006 3:04:05 PM MST")
}

// reasonWord names a BLANK reason.
func reasonWord(r state.Reason) string {
	switch r {
	case state.ReasonNeverReported:
		return "never reported"
	case state.ReasonNoSuccessfulRun:
		return "no successful run"
	case state.ReasonOnlyFailures:
		return "only failures"
	}
	return ""
}

func classList(cc []state.ClassCount) string {
	parts := make([]string, 0, len(cc))
	for _, c := range cc {
		parts = append(parts, c.Class)
	}
	return strings.Join(parts, ", ")
}

// qualifier builds the chip qualifier of a state (components §3.1 patterns).
func qualifier(ds state.DeviceState, c fmtCtx, runPct float64) (qual, joiner string) {
	d := ds.Detail
	switch ds.Primary {
	case state.StateRetired:
		if !ds.Since.IsZero() {
			return "since " + c.day(ds.Since), " · "
		}
	case state.StatePaused:
		if d.PauseUntil.IsZero() {
			return "indefinite", " · "
		}
		return "until " + c.dayTime(d.PauseUntil), " · "
	case state.StateUnknown:
		return "facts unavailable", " · "
	case state.StatePendingFirst:
		if runPct >= 0 {
			return strconv.Itoa(int(runPct*100+0.5)) + "%", " · "
		}
		if !d.GraceDeadline.IsZero() {
			if left := d.GraceDeadline.Sub(c.Now); left > 0 {
				return strconv.Itoa(int(left/time.Hour)) + "h left", " · "
			}
		}
	case state.StateBlank:
		q := reasonWord(ds.Reason)
		if d.ErrorClass != "" {
			if q != "" {
				q += " · "
			}
			q += d.ErrorClass
		}
		return q, " · "
	case state.StateStuck:
		if d.OpenRunAge > 0 {
			return fmt.Sprintf("running %d h (limit %d h)", int(d.OpenRunAge/time.Hour), int(d.OpenRunLimit/time.Hour)), " · "
		}
	case state.StateFailed:
		if d.ConsecutiveFailures > 1 {
			return "×" + strconv.Itoa(d.ConsecutiveFailures), " "
		}
	case state.StatePartial:
		if d.FailedFiles > 0 {
			return fmt.Sprintf("%d files", d.FailedFiles), " · "
		}
	case state.StateUnseen:
		if !d.LastSeen.IsZero() {
			return ageShort(c.Now.Sub(d.LastSeen)), " "
		}
	case state.StateStale:
		if !d.LastSuccess.IsZero() {
			return ageShort(c.Now.Sub(d.LastSuccess)), " "
		}
	case state.StateMaintFailed:
		if d.MaintOp != "" {
			return strings.ReplaceAll(d.MaintOp, "_", " "), " · "
		}
	case state.StateDeferred:
		if r := d.DeferredReason; r != "" {
			return r, " · "
		}
	case state.StateOutdated:
		if d.AgentVersion != "" && d.ExpectedVersion != "" {
			return d.AgentVersion + " to " + d.ExpectedVersion, " · "
		}
	}
	return "", ""
}

// reasonLine is the second line of the reason popover (components §3.1).
func reasonLine(ds state.DeviceState, c fmtCtx) string {
	d := ds.Detail
	switch ds.Primary {
	case state.StateRetired:
		return "Retired. Excluded from rings and rollups."
	case state.StatePaused:
		s := "Parked"
		if d.PauseReason != "" {
			s += ": \"" + d.PauseReason + "\""
		}
		return s + ". Retention still runs."
	case state.StateUnknown:
		cl := d.LoadErrorClass
		if cl == "" {
			cl = "unknown"
		}
		return "Could not load this device's facts (" + cl + "). Counted as unhealthy until it loads."
	case state.StateIdentityConflict:
		return "A second machine presented this device's identity. Resolve it to continue."
	case state.StatePendingFirst:
		s := "Waiting for the first backup."
		if !d.GraceDeadline.IsZero() {
			s += " Grace ends " + c.dayTime(d.GraceDeadline) + "."
		}
		if d.ErrorClass != "" {
			s += " Last error class " + d.ErrorClass + "."
		}
		return s
	case state.StateBlank:
		s := "No successful backup since enrollment"
		if !d.GraceDeadline.IsZero() && d.GraceDeadline.Before(c.Now) {
			s += " and the grace period ended " + c.dayTime(d.GraceDeadline)
		}
		return s + "."
	case state.StateStuck:
		return "The run has exceeded its limit. 100% done is not health."
	case state.StateFailed:
		s := fmt.Sprintf("%d consecutive failure", max(d.ConsecutiveFailures, 1))
		if d.ConsecutiveFailures > 1 {
			s += "s"
		}
		if !ds.Since.IsZero() {
			s += " since " + c.dayTime(ds.Since)
		}
		if d.ErrorClass != "" {
			s += "; class " + d.ErrorClass
		}
		return s + "."
	case state.StatePartial:
		s := fmt.Sprintf("%d files unreadable, over the tolerance", d.FailedFiles)
		if cl := classList(d.ErrorCounts); cl != "" {
			s += "; classes " + cl
		}
		return s + "."
	case state.StateUnseen:
		s := "No heartbeat"
		if !d.LastSeen.IsZero() {
			s += " for " + ageShort(c.Now.Sub(d.LastSeen))
		}
		return s + ". Likely cause: certificate, DNS or the machine is off."
	case state.StateStale:
		s := "No successful backup"
		if !d.LastSuccess.IsZero() {
			s += " for " + ageShort(c.Now.Sub(d.LastSuccess))
		}
		return s + "."
	case state.StateEscrowUnverified:
		cl := string(d.EscrowIssue)
		if cl == "" {
			cl = "not verified"
		}
		return "The recovery key copy was not verified (" + strings.ReplaceAll(cl, "_", " ") + ")."
	case state.StateMaintFailed:
		op := strings.ReplaceAll(d.MaintOp, "_", " ")
		if op == "" {
			op = "maintenance"
		}
		return op + " failed. Storage will keep growing until it succeeds."
	case state.StateMissed:
		return "A scheduled run did not start. This becomes STALE if it continues."
	case state.StateDeferred:
		s := "Waiting on " + d.DeferredReason
		if d.DeferredReason == "" {
			s = "Waiting"
		}
		return s + ". Digest lists devices deferred over 24 h."
	case state.StateOutdated:
		return "Agent " + d.AgentVersion + " is older than the expected " + d.ExpectedVersion + "."
	case state.StateOK:
		if !d.LastSuccess.IsZero() {
			return "Last success " + ageLong(c.Now.Sub(d.LastSuccess)) + ". All checks passed."
		}
		return "All checks passed."
	}
	return ""
}

// RunInfo describes the open run of a device, if any.
type RunInfo struct {
	Open bool
	Pct  float64 // 0..1, or -1 when unknown
}

// ChipsFor returns the chips of one device: the primary state chip, with the
// RUNNING overlay of components §3.1 (it replaces OK; any other primary keeps
// its chip and gains a small RUNNING chip; PENDING_FIRST shows its own progress).
func ChipsFor(ds state.DeviceState, c fmtCtx, run RunInfo) []Chip {
	spec, ok := catalogue[ds.Primary]
	if !ok {
		spec = chipSpec{"UNKNOWN", ClassCrit}
	}
	pct := -1.0
	if run.Open {
		pct = run.Pct
	}
	q, j := qualifier(ds, c, pct)
	title := spec.Label
	if !ds.Since.IsZero() && ds.Primary != state.StateOK {
		title += " since " + c.dayTime(ds.Since)
	}
	primary := Chip{ID: "t-" + ds.ID, State: ds.Primary, Label: spec.Label, Class: spec.Class, Qual: q, Joiner: j, Title: title,
		Reason: reasonLine(ds, c), Dashed: ds.Primary == state.StateUnknown || !ok, Pulse: ds.Primary == state.StatePendingFirst && run.Open && pct >= 0}
	if !run.Open {
		return []Chip{primary}
	}
	running := runningChip(ds.ID, pct)
	switch ds.Primary {
	case state.StateOK:
		return []Chip{running}
	case state.StatePendingFirst:
		return []Chip{primary}
	}
	running.Small = true
	return []Chip{primary, running}
}

func runningChip(id string, pct float64) Chip {
	q := ""
	if pct >= 0 {
		q = strconv.Itoa(int(pct*100+0.5)) + "%"
	}
	return Chip{ID: "r-" + id, Label: "RUNNING", Class: ClassInfo, Qual: q, Joiner: " ", Pulse: true, Title: "RUNNING", Reason: "A backup is running now."}
}

// RunningChip is the RUNNING overlay on its own (the /design page and the device panel).
func RunningChip(id string, pct float64) Chip { return runningChip(id, pct) }

// OutcomeChip renders a run outcome (components §3.2). An empty outcome is an
// open run (RUNNING); a value the console does not know is UNKNOWN OUTCOME and a
// failure (architecture §4.4).
func OutcomeChip(id, outcome string, small bool) Chip {
	var label, class string
	switch outcome {
	case "":
		return Chip{ID: id, Label: "RUNNING", Class: ClassInfo, Pulse: true, Small: small}
	case "success":
		label, class = "SUCCESS", ClassOK
	case "partial":
		label, class = "PARTIAL", ClassWarn
	case "failed":
		label, class = "FAILED", ClassCrit
	case "cancelled":
		label, class = "CANCELLED", ClassNeutral
	case "skipped":
		label, class = "SKIPPED", ClassNeutral
	case "deferred":
		label, class = "DEFERRED", ClassWarn
	case "interrupted":
		label, class = "INTERRUPTED", ClassNeutral
	case "abandoned":
		label, class = "ABANDONED", ClassCrit
	default:
		label, class = "UNKNOWN OUTCOME", ClassCrit
	}
	return Chip{ID: id, Label: label, Class: class, Small: small}
}

// OutcomeCatalogue lists the eight outcomes plus RUNNING and UNKNOWN OUTCOME, in
// the order of components §3.2.
func OutcomeCatalogue() []Chip {
	var out []Chip
	for _, o := range []string{"success", "partial", "failed", "cancelled", "skipped", "deferred", "interrupted", "abandoned", "", "something-new"} {
		out = append(out, OutcomeChip("o-"+o, o, false))
	}
	return out
}

// Badge is an attribute badge (components §3.3).
type Badge struct {
	Text  string
	Class string
}

// BadgeSet is the visible badges and the overflow.
type BadgeSet struct {
	Shown []Badge
	More  []Badge
}

var classRank = map[string]int{ClassCrit: 0, ClassWarn: 1, ClassInfo: 2, ClassNeutral: 3, ClassOK: 4}

// BadgesFor lists the non-primary flags and attributes as badges: critical
// first, at most three visible, the rest behind a "+N" that lists them all.
func BadgesFor(ds state.DeviceState, c fmtCtx) BadgeSet {
	var all []Badge
	for _, f := range ds.Flags {
		if f == ds.Primary || f == state.StateOK {
			continue
		}
		if spec, ok := catalogue[f]; ok {
			all = append(all, Badge{Text: spec.Label, Class: spec.Class})
		}
	}
	for _, a := range ds.Attrs {
		switch a {
		case state.AttrOffline:
			all = append(all, Badge{"OFFLINE", ClassNeutral})
		case state.AttrCustomerHeldKey:
			all = append(all, Badge{"CUSTOMER-HELD KEY", ClassNeutral})
		case state.AttrPolicyDrift:
			all = append(all, Badge{"POLICY DRIFT", ClassWarn})
		case state.AttrClockSkew:
			min := int(ds.Detail.ClockOffset / time.Minute)
			sign := "+"
			if min < 0 {
				sign = "-"
				min = -min
			}
			all = append(all, Badge{fmt.Sprintf("CLOCK SKEW %s%d min", sign, min), ClassWarn})
		case state.AttrPartialUnderTolerance:
			all = append(all, Badge{"PARTIAL · UNDER TOLERANCE", ClassInfo})
		case state.AttrVSSFallback:
			v := ""
			if len(ds.Detail.VSSVolumes) > 0 {
				v = " · " + strings.Join(ds.Detail.VSSVolumes, ", ") + ":"
			}
			all = append(all, Badge{"VSS FALLBACK" + v, ClassInfo})
		case state.AttrNoSchedule:
			all = append(all, Badge{"NO SCHEDULE", ClassCrit})
		case state.AttrAcknowledged:
			all = append(all, Badge{"ACK", ClassNeutral})
		case state.AttrSnoozed:
			t := "SNOOZED"
			if ds.Ack != nil && !ds.Ack.SnoozedUntil.IsZero() {
				t += " until " + c.dayTime(ds.Ack.SnoozedUntil)
			}
			all = append(all, Badge{t, ClassNeutral})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return classRank[all[i].Class] < classRank[all[j].Class] })
	if len(all) <= 3 {
		return BadgeSet{Shown: all}
	}
	return BadgeSet{Shown: all[:3], More: all[3:]}
}

// RollChip is a count chip of a customer's group header (components §3.6).
type RollChip struct {
	Class string
	N     int
	Label string // "3 critical"
	Tip   string // "2 FAILED, 1 UNSEEN"
}

// RollupChips returns the count chips of a customer, worst first. A zero count
// is hidden except OK.
func RollupChips(cs state.CustomerState) []RollChip {
	counts := map[string]int{}
	names := map[string][]string{}
	prec := state.Precedence()
	for _, s := range prec {
		n := cs.Counts[s]
		if n == 0 {
			continue
		}
		cl := ClassOf(s)
		counts[cl] += n
		names[cl] = append(names[cl], fmt.Sprintf("%d %s", n, catalogue[s].Label))
	}
	word := map[string]string{ClassCrit: "critical", ClassWarn: "warning", ClassInfo: "in progress", ClassNeutral: "parked", ClassOK: "ok"}
	var out []RollChip
	for _, cl := range []string{ClassCrit, ClassWarn, ClassInfo, ClassNeutral, ClassOK} {
		if counts[cl] == 0 && cl != ClassOK {
			continue
		}
		if cl == ClassOK && counts[cl] == 0 && len(out) == 0 {
			continue
		}
		out = append(out, RollChip{Class: cl, N: counts[cl], Label: fmt.Sprintf("%d %s", counts[cl], word[cl]), Tip: strings.Join(names[cl], ", ")})
	}
	return out
}

// StatusChip is the customer state label chip for the variants that replace the
// count chips (MISSING, ONBOARDING, EMPTY, UNKNOWN).
func StatusChip(cs state.CustomerState) *Chip {
	switch cs.Status {
	case state.CustomerMissing:
		return &Chip{ID: "s-" + cs.ID, Label: "MISSING", Class: ClassCrit, Title: "MISSING", Reason: "Expected customer with no devices."}
	case state.CustomerOnboarding:
		return &Chip{ID: "s-" + cs.ID, Label: "ONBOARDING", Class: ClassInfo}
	case state.CustomerEmpty:
		return &Chip{ID: "s-" + cs.ID, Label: "NO DEVICES", Class: ClassNeutral}
	case state.CustomerUnknown:
		return &Chip{ID: "s-" + cs.ID, Label: "UNKNOWN", Class: ClassCrit, Dashed: true}
	}
	return nil
}
