package health

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// EvaluatorJob is the job_state row whose staleness raises C06.
const EvaluatorJob = "alarm-evaluator"

// EvaluatorPeriod is the alarm evaluator's period; C06 fires after three.
const EvaluatorPeriod = time.Minute

// MaintainStatus is what the console last learned about the maintenance worker
// over the adminpath socket (banner C07). Known is false until the first probe.
type MaintainStatus struct {
	Known     bool
	Reachable bool
	LastOK    time.Time
}

// Evaluator builds the one fleet view every page, API response and stream event
// is rendered from: state.Evaluate over a fresh facts load, plus the
// console-level banners of architecture §4.6 that M0 can see. Nothing it
// produces is persisted.
type Evaluator struct {
	Store *store.Store
	Clock func() time.Time
	// MasterLoaded reports whether the master key is loaded (C12). Nil means no key.
	MasterLoaded func() bool
	// Maintain reports the maintenance worker probe (C07). Nil means "not probed",
	// which raises nothing: the prober is M3.
	Maintain func() MaintainStatus
	// Endpoint reports the self-probe of the public agent endpoint (C04, C05,
	// C04b; ADR 0013). Nil means "not wired" (unit tests), which raises nothing;
	// serve always wires it, and a probe that cannot run says so (C04b).
	Endpoint func() probe.Status
	// Extra supplies banners that cannot be derived here (fixtures mode shows the
	// full C01-C16 set; the M3/M4 probers will feed the real ones).
	Extra func(now time.Time) []state.Banner
	// SystemDrive is passed to the facts loader (default "C").
	SystemDrive string
}

// Fleet is one evaluated view of the fleet inside a Scope.
type Fleet struct {
	View state.FleetView
	// Computed is the evaluation time (the freshness stamp).
	Computed time.Time
	// InView and Enrolled are the freshness stamp's "n of m agents": devices
	// classified without an UNKNOWN state over non-retired devices.
	InView, Enrolled int
	// LastHeartbeat is the newest heartbeat among active devices (zero if none).
	LastHeartbeat time.Time
	// Jobs are the job_state rows (the last status of each job).
	Jobs []store.JobState
}

// Fleet loads the facts for scope and evaluates them.
func (e *Evaluator) Fleet(ctx context.Context, scope store.Scope) (Fleet, error) {
	now := e.Clock().UTC()
	w, err := e.Store.LoadWorld(ctx, scope, store.LoadOptions{SystemDrive: e.SystemDrive})
	if err != nil {
		return Fleet{}, err
	}
	view := state.Evaluate(w, now)
	jobs, jerr := e.Store.ListJobStates(ctx)

	f := Fleet{View: view, Computed: now, Jobs: jobs}
	var active int
	for i := range view.Devices {
		d := &view.Devices[i]
		if d.Primary == state.StateRetired {
			continue
		}
		f.Enrolled++
		if d.Primary != state.StateUnknown {
			f.InView++
		}
		if d.Primary != state.StatePaused {
			active++
			if d.Detail.LastSeen.After(f.LastHeartbeat) {
				f.LastHeartbeat = d.Detail.LastSeen
			}
		}
	}

	var extra []state.Banner
	// C04, C05, C04b (ADR 0013): from the self-probe of the agent endpoint only.
	// Devices being offline raise no console banner, however many and however
	// long: each is its own UNSEEN/STALE row.
	if e.Endpoint != nil {
		extra = append(extra, EndpointBanners(e.Endpoint(), now)...)
	}
	// C06: only when an evaluator job exists (it arrives at M3/M4).
	if jerr == nil {
		for _, j := range jobs {
			if j.Name == EvaluatorJob && j.LastFinishedAt != nil && now.Sub(*j.LastFinishedAt) > 3*EvaluatorPeriod {
				extra = append(extra, state.Banner{ID: "C06", Severity: state.SeverityCritical,
					Text: fmt.Sprintf("Alerting is stalled: this view and notifications may be stale. Last evaluation %s.", j.LastFinishedAt.UTC().Format("15:04"))})
			}
		}
	}
	// C07: the maintenance worker did not answer its last probe.
	if e.Maintain != nil {
		if m := e.Maintain(); m.Known && !m.Reachable {
			extra = append(extra, state.Banner{ID: "C07", Severity: state.SeverityCritical,
				Text: "Maintenance worker is not running: retention, restore browsing and usage sampling are off."})
		}
	}
	// C12: no master key.
	if e.MasterLoaded == nil || !e.MasterLoaded() {
		extra = append(extra, state.Banner{ID: "C12", Severity: state.SeverityCritical,
			Text: "Master key unavailable: enrollment, escrow reveal and server-side retention are disabled."})
	}
	if e.Extra != nil {
		extra = append(extra, e.Extra(now)...)
	}
	f.View.Banners = SortBanners(dedupe(append(append([]state.Banner(nil), view.Banners...), extra...)))
	return f, nil
}

// dedupe keeps the first banner of each id.
func dedupe(b []state.Banner) []state.Banner {
	seen := map[string]bool{}
	out := b[:0:0]
	for _, x := range b {
		if !seen[x.ID] {
			seen[x.ID] = true
			out = append(out, x)
		}
	}
	return out
}

// priorityIDs are the banners that sort first within a severity (components.md
// §9.2: silence and loss of control).
var priorityIDs = []string{"C01", "C04", "C08", "C09", "C12", "C14"}

func priority(id string) int {
	for i, p := range priorityIDs {
		if p == id {
			return i
		}
	}
	return len(priorityIDs)
}

// SortBanners orders banners: critical before warn, the priority ids first
// within a severity, then by id.
func SortBanners(b []state.Banner) []state.Banner {
	sort.SliceStable(b, func(i, j int) bool {
		if b[i].Severity != b[j].Severity {
			return b[i].Severity > b[j].Severity
		}
		if pi, pj := priority(b[i].ID), priority(b[j].ID); pi != pj {
			return pi < pj
		}
		return b[i].ID < b[j].ID
	})
	return b
}

// JobStatus returns the named job's row, or ok=false when it has never run.
func (f Fleet) JobStatus(name string) (store.JobState, bool) {
	for _, j := range f.Jobs {
		if j.Name == name {
			return j, true
		}
	}
	return store.JobState{}, false
}

// EntityBanner is a banner about one customer rather than the console
// (components.md §9.2 "entity banners"): an expected customer with no devices,
// or a customer whose devices went silent together.
type EntityBanner struct {
	Kind     string // "missing" | "silence"
	Customer state.CustomerState
	Silent   int // silence: devices UNSEEN
	Total    int // silence: active devices
}

// SilenceMinDevices and SilenceShare are the architecture §4.3 customer-wide
// collapse thresholds: at least 5 devices and at least half of the active ones.
// The rule itself lives in internal/state (CustomerCollapse).
const (
	SilenceMinDevices = state.CollapseMinDevices
	SilenceShare      = state.CollapseShare
)

// Silence reports whether a customer's devices are in customer-wide silence and
// how many of how many. It calls state.CustomerCollapse, which owns the §4.3 rule.
func Silence(c state.CustomerState) (silent, total int, is bool) {
	col, is := state.CustomerCollapse(c)
	return col.Affected, col.Total, is
}

// EntityBanners lists the entity banners of a view, MISSING customers first.
func EntityBanners(v state.FleetView) []EntityBanner {
	var out []EntityBanner
	for _, c := range v.Customers {
		if c.Status == state.CustomerMissing {
			out = append(out, EntityBanner{Kind: "missing", Customer: c})
		}
	}
	for _, c := range v.Customers {
		if s, n, is := Silence(c); is {
			out = append(out, EntityBanner{Kind: "silence", Customer: c, Silent: s, Total: n})
		}
	}
	return out
}
