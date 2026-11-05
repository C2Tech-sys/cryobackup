package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// JobRunner is the console's job runner as the system routes need it
// (jobs.Runner satisfies it). RunOnce runs a registered job now under its
// lease and returns ran false when another holder has the lease.
type JobRunner interface {
	Names() []string
	RunOnce(ctx context.Context, name string) (ran bool, err error)
}

// jobRunTimeout bounds one manual job run.
const jobRunTimeout = 15 * time.Minute

// jobBackground tracks manual job runs so a test (and a shutdown) can wait for them.
var jobBackground sync.WaitGroup

// ---- system/health ----

// Component statuses. ok is only ever said on evidence: a probe that has not
// run, or cannot, is not_checked, never ok.
const (
	statusOK         = "ok"
	statusWarn       = "warn"
	statusFailed     = "failed"
	statusNotChecked = "not_checked"
)

// SystemComponentJSON is one line of the system health page.
type SystemComponentJSON struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Status    string     `json:"status"`
	Detail    string     `json:"detail,omitempty"`
	Banners   []string   `json:"banners"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// VersionsJSON are the versions the console knows. The worker's restic and
// rclone versions are "unknown" until the worker's ping carries them.
type VersionsJSON struct {
	Console   string  `json:"console"`
	Commit    string  `json:"commit"`
	BuildDate string  `json:"build_date"`
	Schema    int     `json:"schema"`
	Maintain  *Worker `json:"maintain,omitempty"`
}

// SystemHealthResponse is GET /api/v1/system/health. Overall is the worst
// component: failed, then warn, then not_checked; ok only when every component
// is ok.
type SystemHealthResponse struct {
	StampJSON
	Overall    string                `json:"overall"`
	Components []SystemComponentJSON `json:"components"`
	Banners    []BannerJSON          `json:"banners"`
	Versions   VersionsJSON          `json:"versions"`
}

// healthRowBanners is the one mapping of every console banner the engine can
// raise to the system health row that banner speaks for. A row is never ok
// while a banner mapped to it is raised, and the "b" ids (the engine's own
// "not checked" warnings, architecture ADR 0013 pattern) are mapped beside
// their conditions. The test TestSystemHealthMapsEveryBannerTheEngineCanRaise
// reads the banner ids out of the state and health sources and fails when one
// is missing here, so a new banner cannot be added without a row to show it.
var healthRowBanners = map[string][]string{
	"gateway":            {"C01", "C01b"},
	"storage":            {"C02", "C02b", "C15", "C15b"},
	"smtp":               {"C03", "C03b", "C14", "C14b"},
	"certificates":       {"C05", "C05b"},
	"agent_api":          {"C04", "C04b"},
	"maintenance_worker": {"C07", "C07b"},
	"database":           {"C11"},
	"evaluator":          {"C06", "C06b"},
	"master_key":         {"C12", "C12b"},
	"fleet_data":         {"C08", "C09", "C10"},
	"clock":              {"C13"},
	"release_manifest":   {"C16", "C16b"},
}

// bannerNotWired is C00: the console's own self-checks are not wired, so no
// probe-driven row can be called ok.
const bannerNotWired = "C00"

// clockMinReporting is the fewest reporting devices C13 can judge from (§4.6).
const clockMinReporting = 5

// healthRowOrder is the order of the rows on the page.
var healthRowOrder = []struct{ id, label string }{
	{"gateway", "Gateway"}, {"storage", "Storage"}, {"smtp", "Alert e-mail"}, {"certificates", "Certificates"}, {"agent_api", "Agent API"},
	{"maintenance_worker", "Maintenance worker"}, {"database", "Console database"}, {"evaluator", "Alarm evaluator"}, {"master_key", "Master key"},
	{"jobs", "Background jobs"}, {"fleet_data", "Fleet data"}, {"clock", "Server clock"}, {"release_manifest", "Release manifest"},
}

// healthFacts is everything buildComponents needs: the engine's banners and the
// local evidence the handler gathered. Nothing here is computed state.
type healthFacts struct {
	Banners  []state.Banner
	Now      time.Time
	Complete bool
	// EndpointWired: the agent endpoint self-probe is wired.
	EndpointWired bool
	// WorkerPinged is whether the maintenance worker was pinged, WorkerReachable its answer, MaintainKnown
	// whether the periodic probe has an answer.
	WorkerPinged, WorkerReachable, MaintainKnown bool
	// SchemaErr is the error of the schema version read (the database answering).
	SchemaErr error
	// EvaluatorRanAt is when the alarm-evaluator job last finished.
	EvaluatorRanAt *time.Time
	// MasterKeyWired: the master key state is known to the evaluator.
	MasterKeyWired bool
	// ReportingDevices counts devices with a current heartbeat, for C13.
	ReportingDevices int
	Jobs             []store.JobState
}

// worstOf folds a row's raised banners into a status: failed (a critical
// banner), warn, then not_checked (a "b" banner). "" means none is raised.
func worstOf(bs []state.Banner, ids []string) (raised []string, worst string) {
	rank := map[string]int{"": 0, statusNotChecked: 1, statusWarn: 2, statusFailed: 3}
	for _, b := range bs {
		if !slices.Contains(ids, b.ID) {
			continue
		}
		raised = append(raised, b.ID)
		st := statusWarn
		switch {
		case strings.HasSuffix(b.ID, "b"):
			st = statusNotChecked
		case b.Severity >= state.SeverityCritical:
			st = statusFailed
		}
		if rank[st] > rank[worst] {
			worst = st
		}
	}
	return raised, worst
}

// buildComponents turns the engine's banners and the local evidence into the
// rows of the system health page. A row is ok only when none of its banners is
// raised AND there is evidence for it: C00 (self-checks not wired) takes every
// otherwise-ok row to not_checked, and each row names what its evidence is.
func buildComponents(f healthFacts) []SystemComponentJSON {
	_, c00 := worstOf(f.Banners, []string{bannerNotWired})
	notWired := c00 != ""
	out := make([]SystemComponentJSON, 0, len(healthRowOrder))
	for _, row := range healthRowOrder {
		c := SystemComponentJSON{ID: row.id, Label: row.label, Banners: []string{}}
		var worst string
		if ids, ok := healthRowBanners[row.id]; ok {
			c.Banners, worst = worstOf(f.Banners, ids)
			if c.Banners == nil {
				c.Banners = []string{}
			}
		}
		// Local evidence: what this console can see for itself, beyond the engine's banners.
		status, detail := statusOK, ""
		switch row.id {
		case "agent_api":
			if !f.EndpointWired {
				status, detail = statusNotChecked, "the agent endpoint self-probe is not wired"
			}
		case "maintenance_worker":
			switch {
			case f.WorkerPinged && !f.WorkerReachable:
				status, detail = statusFailed, "the worker did not answer a ping"
			case !f.WorkerReachable && !f.MaintainKnown:
				status, detail = statusNotChecked, "the worker has not been probed"
			}
		case "database":
			if f.SchemaErr != nil {
				status, detail = statusFailed, "the database did not answer"
			}
		case "evaluator":
			if f.EvaluatorRanAt == nil {
				status, detail = statusNotChecked, "the alarm-evaluator job has not run"
			} else {
				c.CheckedAt = ptrTime(*f.EvaluatorRanAt)
			}
		case "master_key":
			if !f.MasterKeyWired {
				status, detail = statusNotChecked, "the master key state is not reported"
			}
		case "clock":
			if f.ReportingDevices < clockMinReporting {
				status, detail = statusNotChecked, "fewer than 5 devices are reporting, so offsets cannot be compared"
			}
		case "fleet_data":
			if !f.Complete {
				status, detail = statusWarn, "the facts load was incomplete"
			}
		case "jobs":
			failing, overdue := 0, 0
			for _, jb := range f.Jobs {
				if jb.LastStatus == "error" {
					failing++
				}
				if jobOverdue(jb, f.Now) {
					overdue++
				}
			}
			switch {
			case failing > 0 || overdue > 0:
				status, detail = statusWarn, jobSummary(failing, overdue)
			case len(f.Jobs) == 0:
				status, detail = statusNotChecked, "no job has run yet"
			}
		}
		switch {
		case worst != "":
			status, detail = worst, ""
		case status == statusOK && notWired:
			status, detail = statusNotChecked, "console self-checks are not wired (C00): this is not being monitored"
		}
		c.Status, c.Detail = status, detail
		out = append(out, c)
	}
	return out
}

func (s *Service) systemHealth(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	f, err := s.Fleet.Fleet(ctx, p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	facts := healthFacts{Banners: f.View.Banners, Now: f.Computed, Complete: f.View.Complete, EndpointWired: s.Fleet.Endpoint != nil,
		MasterKeyWired: s.Fleet.MasterLoaded != nil, Jobs: f.Jobs}
	var worker *Worker
	if s.Backend != nil {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		pr, perr := s.Backend.Ping(pctx)
		cancel()
		facts.WorkerPinged = true
		if perr == nil {
			facts.WorkerReachable = true
			worker = &Worker{Reachable: true, Version: pr.Version, MasterKeyLoaded: pr.MasterKeyLoaded, ResticVersion: toolUnknown, RcloneVersion: toolUnknown}
		} else {
			worker = &Worker{}
		}
	}
	if s.Fleet.Maintain != nil {
		facts.MaintainKnown = s.Fleet.Maintain().Known
	}
	schema, serr := s.Store.SchemaVersion(ctx)
	facts.SchemaErr = serr
	if ej, ok := f.JobStatus(health.EvaluatorJob); ok && ej.LastFinishedAt != nil {
		facts.EvaluatorRanAt = ej.LastFinishedAt
	}
	for _, d := range f.View.Devices {
		if d.Primary != state.StateRetired && d.Primary != state.StatePaused && d.Primary != state.StateUnseen && !d.Detail.LastSeen.IsZero() {
			facts.ReportingDevices++
		}
	}
	out := SystemHealthResponse{StampJSON: StampJSON{ComputedAt: f.Computed, LoadedAt: f.View.LoadedAt, Complete: f.View.Complete}, Banners: bannersJSON(f.View.Banners),
		Components: buildComponents(facts), Overall: statusOK}
	rank := map[string]int{statusOK: 0, statusNotChecked: 1, statusWarn: 2, statusFailed: 3}
	for _, c := range out.Components {
		if rank[c.Status] > rank[out.Overall] {
			out.Overall = c.Status
		}
	}
	out.Versions = VersionsJSON{Console: buildinfo.Version, Commit: buildinfo.Commit, BuildDate: buildinfo.Date, Schema: schema, Maintain: worker}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func jobSummary(failing, overdue int) string {
	switch {
	case failing > 0 && overdue > 0:
		return "jobs are failing and overdue"
	case failing > 0:
		return "a job's last run failed"
	}
	return "a job is overdue"
}

// ---- system/jobs ----

// JobJSON is one row of the job table. The lease holder is not shown: it names
// the host.
type JobJSON struct {
	Name           string     `json:"name"`
	Registered     bool       `json:"registered"`
	Status         string     `json:"status"` // running | ok | error | skipped | never_run
	LastStartedAt  *time.Time `json:"last_started_at,omitempty"`
	LastFinishedAt *time.Time `json:"last_finished_at,omitempty"`
	LastDurationMS *int64     `json:"last_duration_ms,omitempty"`
	LastErrorClass string     `json:"last_error_class,omitempty"`
	RunCount       int        `json:"run_count"`
	NextDueAt      *time.Time `json:"next_due_at,omitempty"`
	Running        bool       `json:"running"`
	Overdue        bool       `json:"overdue"`
}

// JobListResponse is GET /api/v1/system/jobs.
type JobListResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	Jobs       []JobJSON `json:"jobs"`
}

func jobRunning(j store.JobState, now time.Time) bool {
	return j.LeaseHolder != "" && j.LeaseUntil != nil && j.LeaseUntil.After(now)
}

// jobOverdue: a job whose next run was due more than a minute ago and which is
// not running now.
func jobOverdue(j store.JobState, now time.Time) bool {
	return j.NextDueAt != nil && now.Sub(*j.NextDueAt) > time.Minute && !jobRunning(j, now)
}

func (s *Service) systemJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListJobStates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := s.now()
	registered := map[string]bool{}
	if s.Jobs != nil {
		for _, n := range s.Jobs.Names() {
			registered[n] = true
		}
	}
	seen := map[string]bool{}
	out := JobListResponse{ComputedAt: now, Jobs: []JobJSON{}}
	for _, j := range rows {
		if !store.IsJobRow(j.Name) {
			continue // the health-prober's lease and per-probe rows are not jobs
		}
		seen[j.Name] = true
		row := JobJSON{Name: j.Name, Registered: registered[j.Name], Status: j.LastStatus, LastStartedAt: j.LastStartedAt, LastFinishedAt: j.LastFinishedAt,
			LastDurationMS: j.LastDurationMS, LastErrorClass: j.LastErrorClass, RunCount: j.RunCount, NextDueAt: j.NextDueAt, Running: jobRunning(j, now), Overdue: jobOverdue(j, now)}
		if row.Status == "" {
			row.Status = "never_run"
		}
		out.Jobs = append(out.Jobs, row)
	}
	// A registered job with no row has never run: say so rather than omit it.
	for n := range registered {
		if !seen[n] {
			out.Jobs = append(out.Jobs, JobJSON{Name: n, Registered: true, Status: "never_run"})
		}
	}
	slices.SortFunc(out.Jobs, func(a, b JobJSON) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	httpx.WriteJSON(w, http.StatusOK, out)
}

// JobRunResponse is the 202 answer of POST /api/v1/system/jobs/{name}/run: the
// run was accepted, not finished. The outcome is the job's row in the job table.
type JobRunResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	Job        string    `json:"job"`
	Status     string    `json:"status"`
}

// auditAccepted writes the audit row of an accepted action and reports whether
// it did. A refusal to audit refuses the action ("NOT AUDITED").
func (s *Service) auditAccepted(w http.ResponseWriter, r *http.Request, p *auth.Principal, action, objectType, objectID string) bool {
	_, err := s.Store.AppendAudit(r.Context(), store.AuditEvent{ActorType: "admin", ActorID: p.Admin.ID, ActorLabel: p.Admin.Username, Method: "api",
		SourceIP: httpx.ClientIP(r), Action: action, ObjectType: objectType, ObjectID: objectID, Result: "ok"})
	if err != nil {
		s.log().Error("admin api: change refused, not audited", logx.Class("audit"), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return false
	}
	return true
}

// auditOutcome writes the one audit row of an action whose outcome is known, with
// that outcome: result is ok or error, and details (fixed words, nothing
// identifying) say what happened. It writes even when the client has gone away.
// A refusal to audit answers 500 ("NOT AUDITED").
func (s *Service) auditOutcome(w http.ResponseWriter, r *http.Request, p *auth.Principal, action, objectType, objectID, result string, details map[string]any) bool {
	ev := store.AuditEvent{ActorType: "admin", ActorID: p.Admin.ID, ActorLabel: p.Admin.Username, Method: "api",
		SourceIP: httpx.ClientIP(r), Action: action, ObjectType: objectType, ObjectID: objectID, Result: result}
	if len(details) > 0 {
		b, err := json.Marshal(details)
		if err != nil {
			httpx.Problem(w, r, api.CodeInternal, 0)
			return false
		}
		ev.Details = b
	}
	// The action has happened by now: a client that went away (a cancelled request
	// context) must not cost it its audit row.
	if _, err := s.Store.AppendAudit(context.WithoutCancel(r.Context()), ev); err != nil {
		s.log().Error("admin api: action not audited", logx.Class("audit"), logx.Err(err))
		httpx.Problem(w, r, api.CodeInternal, 0)
		return false
	}
	return true
}

// jobBusy reports whether another holder has the job's lease now.
func (s *Service) jobBusy(ctx context.Context, name string) (bool, error) {
	j, err := s.Store.GetJobState(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return jobRunning(j, s.now()), nil
}

func (s *Service) systemJobRun(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	name := r.PathValue("name")
	if s.Jobs == nil {
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second)
		return
	}
	if !slices.Contains(s.Jobs.Names(), name) {
		httpx.Problem(w, r, api.CodeNotFound, 0)
		return
	}
	busy, err := s.jobBusy(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if busy {
		httpx.Problem(w, r, api.CodeConflict, 0)
		return
	}
	if !s.auditAccepted(w, r, p, "system.job.run", "job", name) {
		return
	}
	jobBackground.Add(1)
	go func() {
		defer jobBackground.Done()
		ctx, cancel := context.WithTimeout(context.Background(), jobRunTimeout)
		defer cancel()
		if _, err := s.Jobs.RunOnce(ctx, name); err != nil {
			s.log().Warn("manual job run failed", logx.Str("job", name), logx.Err(err))
		}
	}()
	httpx.WriteJSON(w, http.StatusAccepted, JobRunResponse{ComputedAt: s.now(), Job: name, Status: "accepted"})
}

// ---- storage-targets/{id}/probe ----

// HealthProbe runs the console's health probes now (jobs.HealthProber satisfies
// it; cli wires it). ProbeNow takes the probe lease and returns ran false, nil
// when another console holds it, so nothing was probed here. Holding it, it
// runs the gateway probe (which also reads the gateway certificate's expiry)
// and the storage probe, or waits for the run already in flight, and returns
// once both have recorded their facts, bounded by ctx (ran true and ctx's
// error when the wait ran out). Probes that ended without a result (the lease
// was lost mid-wait, or the console is shutting down) are ran false and an
// error that is context.Canceled.
type HealthProbe interface {
	ProbeNow(ctx context.Context) (ran bool, err error)
}

// probeNowTimeout bounds the wait for the probes: the storage probe's own bound
// (45 s) with room for the gateway probe beside it.
const probeNowTimeout = 90 * time.Second

// ProbeCheckJSON is the result of one probe check. Status is ok, warn, failed or
// not_checked; ok only when the probe this request asked for produced the fact
// and raised nothing.
type ProbeCheckJSON struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Detail  string   `json:"detail,omitempty"`
	Banners []string `json:"banners"`
	// Scope is always "console-wide": the engine keeps gateway, storage and
	// certificate facts for the console as a whole, not per storage target.
	Scope string `json:"scope"`
}

// probeScope and probeNote say what a probe answer is about. The gateway, the
// storage probe and the quota banner are console-wide in the engine's facts;
// nothing in them names a target, so this route cannot honestly answer "is
// target X fine" and does not pretend to.
const (
	probeScope = "console-wide"
	probeNote  = "The engine records gateway, storage, certificate and quota facts for the console as a whole, not per storage target. These checks speak for the whole console; the target named here is the one you asked about, and its own state is not separately measured."
)

// ProbeJobJSON is the record of this request's probe run: ok when the probes
// ran and every one ended within the bound, error (with a class) otherwise.
type ProbeJobJSON struct {
	Status     string `json:"status"`
	ErrorClass string `json:"error_class,omitempty"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
}

// ProbeResponse is POST /api/v1/storage-targets/{id}/probe. Ran is false when the
// probe could not run (Reason says why) and every check is then not_checked.
type ProbeResponse struct {
	ComputedAt time.Time        `json:"computed_at"`
	TargetID   string           `json:"target_id"`
	Target     string           `json:"target"`
	Ran        bool             `json:"ran"`
	Reason     string           `json:"reason,omitempty"`
	Job        *ProbeJobJSON    `json:"job,omitempty"`
	Checks     []ProbeCheckJSON `json:"checks"`
	Worker     *Worker          `json:"worker,omitempty"`
	// Scope says what the checks are about: always "console-wide" (see probeNote).
	Scope string `json:"scope"`
	Note  string `json:"note"`
}

// probeChecks judges the gateway, storage and certificates checks of a probe
// that began at since. A check reads only a fact produced at or after since:
// an older one (the probe did not finish, the worker was busy, the gateway is
// not configured) is not_checked whatever it said, because it is not evidence
// of now. A fresh fact is then judged by the banners the engine raised from it
// (the not-checked "b" banners included), by C00, and by the run itself: a probe
// that failed just now is failed even before its streak raises a banner. The
// certificates check reads the gateway run: a failed one read no certificate.
func probeChecks(banners []state.Banner, facts state.ProbeFacts, since time.Time) []ProbeCheckJSON {
	from := state.Millis(since.UnixMilli())
	fresh := func(r *state.ProbeRun) bool { return r != nil && r.LastRunAt != 0 && r.LastRunAt >= from }
	failedNow := func(r *state.ProbeRun) bool { return r.LastOKAt != r.LastRunAt }
	staleWhy := func(r *state.ProbeRun, what string) string {
		if r != nil && r.Unconfigured != "" {
			return "not checked: " + r.Unconfigured
		}
		return "the " + what + " probe produced no result during this request, so its older result is not evidence of now"
	}
	_, c00 := worstOf(banners, []string{bannerNotWired})
	out := make([]ProbeCheckJSON, 0, 3)
	for _, c := range []struct {
		id, what string
		run      *state.ProbeRun
	}{{"gateway", "gateway", facts.Gateway}, {"storage", "storage", facts.Storage}, {"certificates", "gateway", facts.Gateway}} {
		ids, worst := worstOf(banners, healthRowBanners[c.id])
		if ids == nil {
			ids = []string{}
		}
		chk := ProbeCheckJSON{ID: c.id, Status: statusOK, Banners: ids, Scope: probeScope}
		switch {
		case !fresh(c.run):
			chk.Status, chk.Detail = statusNotChecked, staleWhy(c.run, c.what)
		case c.id == "certificates" && failedNow(c.run):
			chk.Status, chk.Detail = statusNotChecked, "the gateway probe failed just now, so no certificate was read"
		case c.id != "certificates" && failedNow(c.run):
			chk.Status, chk.Detail = statusFailed, "the "+c.what+" probe failed just now ("+c.run.LastErrorClass+")"
			if worst == statusFailed {
				chk.Detail = ""
			}
		case worst != "":
			chk.Status = worst
		case c00 != "":
			chk.Status, chk.Detail = statusNotChecked, "console self-checks are not wired (C00)"
		}
		out = append(out, chk)
	}
	return out
}

func (s *Service) storageTargetProbe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	t, err := s.Store.GetStorageTarget(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Exactly one audit row per probe. Without a prober nothing runs: the row is
	// written first and a refused audit refuses the probe. With one, the probe
	// lease is taken inside ProbeNow, so whether the probe ran is known only after
	// it: the row is written then, with the outcome (ok; error "conflict" when
	// another console holds the lease, answered 409; error "timeout" or "failed"
	// for probes that did not finish; error "cancelled" for probes that ended
	// without a result). It is written even when the client has gone
	// (auditOutcome). A refused audit then answers 500; the probe itself only
	// refreshes health facts.
	const auditAction = "storage_target.probe"
	out := ProbeResponse{TargetID: t.ID, Target: t.Name, Checks: []ProbeCheckJSON{}, Scope: probeScope, Note: probeNote}
	notChecked := func(reason string) {
		out.Reason = reason
		for _, id := range []string{"gateway", "storage", "certificates"} {
			out.Checks = append(out.Checks, ProbeCheckJSON{ID: id, Status: statusNotChecked, Detail: reason, Banners: []string{}, Scope: probeScope})
		}
	}
	if s.Prober == nil {
		if !s.auditAccepted(w, r, p, auditAction, "storage_target", t.ID) {
			return
		}
		notChecked("the health prober does not run in this console")
	} else {
		started := s.now()
		pctx, cancel := context.WithTimeout(ctx, probeNowTimeout)
		ran, rerr := s.Prober.ProbeNow(pctx)
		cancel()
		if ran {
			s.committed(r) // the probe facts changed: signal now, before the audit and the evaluation, either of which can fail
		}
		took := s.now().Sub(started).Milliseconds()
		out.Ran = ran && rerr == nil
		result, outcome := "ok", ""
		switch {
		case !ran && rerr == nil:
			result, outcome = "error", "conflict"
		case errors.Is(rerr, context.DeadlineExceeded):
			result, outcome = "error", "timeout"
		case !ran && errors.Is(rerr, context.Canceled):
			result, outcome = "error", "cancelled"
		case rerr != nil:
			result, outcome = "error", "failed"
		}
		out.Job = &ProbeJobJSON{Status: result, ErrorClass: outcome, DurationMS: &took}
		var details map[string]any
		if outcome != "" {
			details = map[string]any{"outcome": outcome}
		}
		if !s.auditOutcome(w, r, p, auditAction, "storage_target", t.ID, result, details) {
			return
		}
		switch {
		case !ran && rerr == nil:
			httpx.Problem(w, r, api.CodeConflict, 0) // another console holds the probe lease: nothing ran here
			return
		case outcome == "timeout":
			notChecked("the probes did not finish within " + probeNowTimeout.String())
		case outcome == "cancelled":
			notChecked("the probes were cancelled before they finished: the probe lease was lost, or the console is shutting down")
		case rerr != nil:
			notChecked("the health prober failed to run")
		default:
			// The facts are read before the evaluation, so the banners are judged from
			// facts at least as new as the ones the freshness test reads.
			var facts state.ProbeFacts
			if s.Fleet.Probes != nil {
				facts = s.Fleet.Probes()
			}
			f, ferr := s.Fleet.Fleet(ctx, p.Scope)
			if ferr != nil {
				s.fail(w, r, ferr)
				return
			}
			out.Checks = probeChecks(f.View.Banners, facts, started)
		}
	}
	if s.Backend != nil {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if pr, perr := s.Backend.Ping(pctx); perr == nil {
			out.Worker = &Worker{Reachable: true, Version: pr.Version, MasterKeyLoaded: pr.MasterKeyLoaded, ResticVersion: toolUnknown, RcloneVersion: toolUnknown}
		} else {
			out.Worker = &Worker{}
		}
		cancel()
	}
	out.ComputedAt = s.now()
	httpx.WriteJSON(w, http.StatusOK, out)
}
