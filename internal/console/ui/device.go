package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// progressDoc is the progress document of device_presence (counts only).
type progressDoc struct {
	RunID      string  `json:"run_id"`
	Kind       string  `json:"kind"`
	Pct        float64 `json:"percent_done"`
	Files      uint64  `json:"files_done"`
	TotalFiles uint64  `json:"total_files"`
	Bytes      uint64  `json:"bytes_done"`
	Stalled    bool    `json:"stalled"`
	Total      uint64  `json:"total_bytes"`
}

func parseProgress(raw json.RawMessage) progressDoc {
	var d progressDoc
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &d)
	}
	return d
}

// ProgressView is the live run progress component (components §4.8).
type ProgressView struct {
	ID            string // run-<id>
	RunID         string
	Kind          string
	Pct           int
	Indeterminate bool
	Stalled       bool
	Meta          []string
	Label         string
	Done          bool
	Outcome       *Chip
	Summary       string
	PollHref      string
}

func progressView(runID string, d progressDoc, started time.Time, c fmtCtx) ProgressView {
	pv := ProgressView{ID: "run-" + runID, RunID: runID, Kind: orDefault(d.Kind, "backup"), PollHref: "/ui/partials/run-progress/" + runID}
	if d.Pct <= 0 && d.TotalFiles == 0 {
		pv.Indeterminate = true
	}
	pv.Pct = int(d.Pct*100 + 0.5)
	pv.Stalled = d.Stalled
	if d.TotalFiles > 0 {
		pv.Meta = append(pv.Meta, fmt.Sprintf("%s / %s files", groupInt(d.Files), groupInt(d.TotalFiles)))
	} else if d.Files > 0 {
		pv.Meta = append(pv.Meta, groupInt(d.Files)+" files")
	}
	if d.Bytes > 0 {
		pv.Meta = append(pv.Meta, byteSize(d.Bytes)+" processed")
	}
	pv.Label = "Live · " + pv.Kind
	if !started.IsZero() {
		pv.Label += " · started " + started.In(c.loc()).Format("3:04 PM")
	}
	return pv
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func groupInt(n uint64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	return b.String()
}

func byteSize(n uint64) string {
	const unit = 1024.0
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < unit || u == "TB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= unit
	}
	return fmt.Sprint(n)
}

// RunRow is a line of the run history (components §5.6).
type RunRow struct {
	ID       string
	Date     string
	DateISO  string
	Chip     Chip
	Summary  string
	Exit     string
	Duration string
	Href     string
}

func runRow(r store.Run, c fmtCtx) RunRow {
	when := r.StartedAt
	if r.FinishedAt != nil {
		when = *r.FinishedAt
	}
	row := RunRow{ID: r.ID, Date: when.In(c.loc()).Format("Mon 3:04 PM"), DateISO: when.UTC().Format(time.RFC3339), Chip: OutcomeChip("o-"+r.ID, r.Outcome, false)}
	var stats struct {
		FilesNew    int64  `json:"files_new"`
		DataAdded   uint64 `json:"data_added"`
		FailedFiles int64  `json:"failed_files"`
		TotalFiles  int64  `json:"total_files_processed"`
	}
	_ = json.Unmarshal(r.Stats, &stats)
	var parts []string
	if r.Kind != "backup" {
		parts = append(parts, r.Kind)
	}
	switch r.Outcome {
	case "success":
		if r.Kind == "backup" {
			parts = append(parts, fmt.Sprintf("%d new", stats.FilesNew), byteSize(stats.DataAdded)+" added")
		}
	case "partial":
		parts = append(parts, fmt.Sprintf("%d files unreadable", stats.FailedFiles))
	case "failed", "abandoned":
		if r.ErrorClass != "" {
			parts = append(parts, r.ErrorClass)
		}
	case "deferred":
		parts = append(parts, r.DeferredReason)
	case "":
		parts = append(parts, "running")
	}
	if r.Attempt > 1 {
		parts = append(parts, fmt.Sprintf("attempt %d", r.Attempt))
	}
	row.Summary = strings.Join(parts, " · ")
	if r.ResticExitCode != nil {
		row.Exit = fmt.Sprintf("exit %d", *r.ResticExitCode)
	}
	if r.FinishedAt != nil {
		d := r.FinishedAt.Sub(r.StartedAt).Round(time.Second)
		row.Duration = fmt.Sprintf("%dm %02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return row
}

// Fact is a line of the facts list.
type Fact struct {
	Key   string
	Value string
	Mono  bool
	Chip  *Chip
}

// CauseView is the inline banner explaining the state (components §9.3).
type CauseView struct {
	Class string
	Title string
	Text  string
	// Link, when set, is where the message sends the reader (a view that belongs to another page).
	Link *Link
}

// DeviceView is the device panel summary (B07).
type DeviceView struct {
	// Retired: no ten-day strip (the engine computes none) and no presence line (M3-T6).
	Retired    bool
	ID         string
	Host       string
	Customer   string
	CustomerID string
	Chips      []Chip
	Badges     BadgeSet
	Sub        []string
	Cause      *CauseView
	Progress   *ProgressView
	Strip      StripView
	Runs       []RunRow
	// RunsErr and FactsErr (M2-W3A): the run list or a fact could not be read, so
	// the section says so instead of showing an empty list or a missing row.
	RunsErr     bool
	FactsErr    bool
	Facts       []Fact
	CanRestore  bool
	RestoreNote string
	AsOf        string
}

func scheduleText(s policy.Schedule) string {
	if s.Mode == "fixed" && len(s.FixedTimes) > 0 {
		return "at " + strings.Join(s.FixedTimes, ", ") + " · window " + s.Window
	}
	iv := s.IntervalMinutes
	var every string
	switch {
	case iv >= 60 && iv%60 == 0:
		every = fmt.Sprintf("every %d h", iv/60)
	default:
		every = fmt.Sprintf("every %d min", iv)
	}
	t := every + " in " + s.Window
	if s.Catchup {
		t += " · catch-up on"
	}
	return t
}

func offsetText(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	if d < time.Minute {
		return fmt.Sprintf("%s%d s", sign, int(d/time.Second))
	}
	return fmt.Sprintf("%s%d min", sign, int(d/time.Minute))
}

// deviceView builds the summary of one device; ok is false when the device is
// not in the principal's view.
func (s *Service) deviceView(ctx context.Context, p *auth.Principal, f health.Fleet, id string) (DeviceView, bool) {
	c := s.fcAt(ctx, f.Computed)
	var ds *state.DeviceState
	for i := range f.View.Devices {
		if f.View.Devices[i].ID == id {
			ds = &f.View.Devices[i]
		}
	}
	if ds == nil {
		return DeviceView{}, false
	}
	dev, err := s.Store.GetDevice(ctx, p.Scope, id)
	if err != nil {
		return DeviceView{}, false
	}
	custName := ""
	for _, cs := range f.View.Customers {
		if cs.ID == dev.CustomerID {
			custName = cs.Name
		}
	}
	ri := s.runningInfo(ctx, p, *ds)
	v := DeviceView{ID: id, Host: ds.Hostname, Customer: custName, CustomerID: dev.CustomerID, Chips: ChipsFor(*ds, c, ri), Badges: BadgesFor(*ds, c), AsOf: c.clock(f.Computed)}
	v.Sub = append(v.Sub, custName)
	if dev.OSVersion != "" {
		v.Sub = append(v.Sub, osLabel(dev.OSVersion))
	}
	if dev.DeviceClass != "" && dev.DeviceClass != "unknown" {
		v.Sub = append(v.Sub, dev.DeviceClass)
	}
	if ds.Detail.AgentVersion != "" {
		v.Sub = append(v.Sub, "agent "+safeVersion(ds.Detail.AgentVersion))
	}
	seen := "never seen"
	if !ds.Detail.LastSeen.IsZero() && dev.EnrollConfirmedAt != nil {
		online := "online"
		if c.Now.Sub(ds.Detail.LastSeen) > 2*time.Minute {
			online = "offline"
		}
		seen = online + " · seen " + ageLong(c.Now.Sub(ds.Detail.LastSeen))
	}
	if ds.Primary != state.StateRetired { // the engine loads no presence for a retired device: "never seen" would be false
		v.Sub = append(v.Sub, seen)
	}

	if ds.Primary != state.StateOK && ds.Primary != state.StateRetired {
		v.Cause = &CauseView{Class: ClassOf(ds.Primary), Title: v.Chips[0].Title, Text: reasonLine(*ds, c)}
	}
	if ds.Primary == state.StateRetired {
		v.Retired = true
		v.Cause = s.retiredCause(ctx, p, dev, c) // M3-T6: when, why, what it means
	}
	pr, perr := s.Store.GetPresence(ctx, p.Scope, id)
	if ri.Open && perr == nil {
		var started time.Time
		if runs, err := s.Store.ListRuns(ctx, p.Scope, id, store.Page{Limit: 5}); err == nil {
			for _, r := range runs.Items {
				if r.Outcome == "" && r.Kind == "backup" {
					started = r.StartedAt
					break
				}
			}
		}
		doc := parseProgress(pr.Progress)
		runID := doc.RunID
		if runID == "" {
			runID = "device-" + id
		}
		pv := progressView(runID, doc, started, c)
		v.Progress = &pv
	}
	if cells, err := health.TenDay(ctx, s.Store, p.Scope, state.FleetView{Devices: []state.DeviceState{*ds}}, f.Computed, s.loc(ctx)); err == nil {
		if row, ok := cells[id]; ok {
			v.Strip = stripView(id, row, ds.Primary == state.StatePaused, true, c)
		}
	}
	if runs, err := s.Store.ListRuns(ctx, p.Scope, id, store.Page{Limit: 10}); err == nil {
		for _, r := range runs.Items {
			v.Runs = append(v.Runs, runRow(r, c))
		}
	} else {
		v.RunsErr = true
		s.log().Error("ui: device runs failed", logx.Err(err))
	}

	// Facts: counts, versions and classes only.
	if pol, ver, _, err := s.Store.EffectivePolicyFor(ctx, p.Scope, id); err == nil {
		pv := fmt.Sprintf("v%d", ver)
		if dev.PolicyVersionApplied != nil && *dev.PolicyVersionApplied != ver {
			pv += fmt.Sprintf(" (device applied v%d)", *dev.PolicyVersionApplied)
		} else if dev.PolicyVersionApplied == nil {
			pv += " (not yet applied)"
		}
		v.Facts = append(v.Facts, Fact{Key: "Policy", Value: pv}, Fact{Key: "Schedule", Value: scheduleText(pol.Schedule), Mono: true})
	} else {
		// The effective policy does not compose (or cannot be read): the device is not being served a valid policy.
		v.Facts = append(v.Facts, Fact{Key: "Policy", Chip: &Chip{ID: "p-" + id, Label: "NOT AVAILABLE", Qual: "does not compose", Joiner: " · ", Class: ClassCrit, Small: true}})
		s.log().Error("ui: effective policy failed", logx.Class("policy"))
	}
	if snaps, err := s.Store.ListSnapshots(ctx, p.Scope, id); err == nil {
		val := plural(len(snaps), "snapshot", "snapshots")
		if len(snaps) > 0 {
			newest := snaps[0].TakenAt
			for _, sn := range snaps {
				if sn.TakenAt.After(newest) {
					newest = sn.TakenAt
				}
			}
			val += " · latest " + ageLong(c.Now.Sub(newest))
		}
		v.Facts = append(v.Facts, Fact{Key: "Repository", Value: val, Mono: true})
	} else {
		v.FactsErr = true
	}
	if esc, err := s.Store.ActiveEscrow(ctx, p.Scope, id); err == nil {
		switch {
		case esc.Custody != "escrowed":
			v.Facts = append(v.Facts, Fact{Key: "Key custody", Chip: &Chip{ID: "k-" + id, Label: "CUSTOMER-HELD", Class: ClassNeutral, Small: true}})
		case esc.VerifiedAt == nil:
			v.Facts = append(v.Facts, Fact{Key: "Key custody", Chip: &Chip{ID: "k-" + id, Label: "ESCROWED", Qual: "not verified", Joiner: " · ", Class: ClassWarn, Small: true}})
		default:
			v.Facts = append(v.Facts, Fact{Key: "Key custody", Chip: &Chip{ID: "k-" + id, Label: "ESCROWED", Qual: "verified " + ageLong(c.Now.Sub(*esc.VerifiedAt)), Joiner: " · ", Class: ClassOK, Small: true}})
		}
	}
	v.Facts = append(v.Facts, Fact{Key: "Enrolled", Value: c.day(dev.EnrolledAt) + " · via " + orDefault(dev.EnrolledVia, "token"), Mono: true})
	if perr == nil {
		v.Facts = append(v.Facts, Fact{Key: "Last heartbeat", Value: ageLong(c.Now.Sub(pr.ReceivedAt)) + " · " + pr.ServiceState, Mono: true})
		if pr.ClockOffsetMS != nil {
			v.Facts = append(v.Facts, Fact{Key: "Clock offset", Value: offsetText(*pr.ClockOffsetMS), Mono: true})
		}
	}

	// Restore action: enabled only when it can work, otherwise a stated reason.
	v.CanRestore = true
	switch esc, err := s.Store.ActiveEscrow(ctx, p.Scope, id); {
	case p.Role < auth.RoleOperator:
		v.CanRestore, v.RestoreNote = false, "Restore needs the operator role."
	case ds.Primary == state.StateRetired:
		v.CanRestore, v.RestoreNote = false, "This device is retired."
	case err == nil && esc.Custody != "escrowed":
		v.CanRestore, v.RestoreNote = false, "Customer-held key: no technician restore."
	case s.MasterLoaded == nil || !s.MasterLoaded():
		v.CanRestore, v.RestoreNote = false, "The master key is unavailable (banner C12)."
	}
	return v, true
}

// deviceLive renders the SSE fragment of a device page.
func (s *Service) deviceLive(ctx context.Context, p *auth.Principal, f health.Fleet, id string) (string, error) {
	v, ok := s.deviceView(ctx, p, f, id)
	if !ok {
		return "", fmt.Errorf("ui: device not in view")
	}
	return s.renderString("device", "device_live", struct {
		Dev  DeviceView
		CSRF string
	}{v, p.CSRF})
}

// runProgressPartial is the 5 s polling fallback of one run's progress.
func (s *Service) runProgressPartial(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	run, err := s.Store.GetRun(r.Context(), p.Scope, r.PathValue("run_id"))
	c := s.fc(r.Context())
	if err != nil {
		// The synthetic id of a run whose agent has not reported its id yet.
		if id, ok := strings.CutPrefix(r.PathValue("run_id"), "device-"); ok {
			if pr, perr := s.Store.GetPresence(r.Context(), p.Scope, id); perr == nil {
				pv := progressView(r.PathValue("run_id"), parseProgress(pr.Progress), time.Time{}, c)
				s.render(w, r, http.StatusOK, "device", "run_progress", pv)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	var pv ProgressView
	if run.Outcome == "" {
		started := run.StartedAt
		pr, perr := s.Store.GetPresence(r.Context(), p.Scope, run.DeviceID)
		doc := progressDoc{}
		if perr == nil {
			doc = parseProgress(pr.Progress)
		}
		pv = progressView(run.ID, doc, started, c)
	} else {
		ch := OutcomeChip("o-"+run.ID, run.Outcome, false)
		pv = ProgressView{ID: "run-" + run.ID, RunID: run.ID, Kind: run.Kind, Done: true, Outcome: &ch, Summary: runRow(run, c).Summary}
	}
	s.render(w, r, http.StatusOK, "device", "run_progress", pv)
}
