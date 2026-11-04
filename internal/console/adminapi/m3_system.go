package adminapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
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

// proberJob is the job_state name of the health-prober job, which feeds the
// gateway, storage, SMTP and certificate banners (architecture §5.7). A console
// without it cannot say any of those is fine: they read "not checked".
const proberJob = "health-prober"

// probeFreshness is how old the prober's last successful run may be and still
// count as evidence (it runs every minute).
const probeFreshness = 5 * time.Minute

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

// bannersWithID lists the ids of the banners among ids that are raised, and the
// worst status they imply ("" when none is raised).
func bannersWithID(bs []state.Banner, ids ...string) (out []string, worst string) {
	for _, b := range bs {
		if !slices.Contains(ids, b.ID) {
			continue
		}
		out = append(out, b.ID)
		if b.Severity >= state.SeverityCritical {
			worst = statusFailed
		} else if worst == "" {
			worst = statusWarn
		}
	}
	return out, worst
}

// proberEvidence is how the health-prober's own record reads: the time of its
// last good run, or a detail saying why there is no evidence.
func proberEvidence(f health.Fleet, now time.Time) (checked *time.Time, detail string) {
	j, ok := f.JobStatus(proberJob)
	switch {
	case !ok || j.LastFinishedAt == nil:
		return nil, "the health-prober job has not run"
	case j.LastStatus != "ok":
		return nil, "the health-prober job is failing (" + j.LastErrorClass + ")"
	case now.Sub(*j.LastFinishedAt) > probeFreshness:
		return nil, "the health-prober job last finished " + ageWords(now.Sub(*j.LastFinishedAt)) + " ago"
	}
	return ptrTime(*j.LastFinishedAt), ""
}

func (s *Service) systemHealth(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	f, err := s.Fleet.Fleet(ctx, p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := f.Computed
	bs := f.View.Banners
	probed, why := proberEvidence(f, now)

	// probeComponent builds a component fed by the prober: failed or warn when
	// its banners say so, ok only with fresh prober evidence.
	probeComponent := func(id, label string, ids ...string) SystemComponentJSON {
		c := SystemComponentJSON{ID: id, Label: label}
		var worst string
		c.Banners, worst = bannersWithID(bs, ids...)
		switch {
		case worst != "":
			c.Status = worst
		case probed != nil:
			c.Status, c.CheckedAt = statusOK, probed
		default:
			c.Status, c.Detail = statusNotChecked, why
		}
		if c.Banners == nil {
			c.Banners = []string{}
		}
		return c
	}
	out := SystemHealthResponse{StampJSON: StampJSON{ComputedAt: now, LoadedAt: f.View.LoadedAt, Complete: f.View.Complete}, Banners: bannersJSON(bs)}
	gateway := probeComponent("gateway", "Gateway", "C01")
	storage := probeComponent("storage", "Storage", "C02", "C15")
	smtp := probeComponent("smtp", "Alert e-mail", "C03", "C14")
	certs := probeComponent("certificates", "Certificates", "C05")

	var worker *Worker
	if s.Backend != nil {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		pr, perr := s.Backend.Ping(pctx)
		cancel()
		if perr == nil {
			worker = &Worker{Reachable: true, Version: pr.Version, MasterKeyLoaded: pr.MasterKeyLoaded, ResticVersion: toolUnknown, RcloneVersion: toolUnknown}
			// The worker's own storage probe is evidence too.
			if storage.Status == statusNotChecked && pr.StorageReachable != nil && *pr.StorageReachable {
				storage.Status, storage.Detail = statusOK, "reported by the maintenance worker"
			}
		} else {
			worker = &Worker{}
		}
	}

	agentAPI := SystemComponentJSON{ID: "agent_api", Label: "Agent API", Banners: []string{}}
	switch ids, worst := bannersWithID(bs, "C04", "C04b"); {
	case worst != "":
		agentAPI.Status, agentAPI.Banners = worst, ids
		if slices.Contains(ids, "C04b") && !slices.Contains(ids, "C04") {
			agentAPI.Status = statusNotChecked
		}
	case s.Fleet.Endpoint == nil:
		agentAPI.Status, agentAPI.Detail = statusNotChecked, "the agent endpoint self-probe is not wired"
	default:
		agentAPI.Status = statusOK
	}

	maint := SystemComponentJSON{ID: "maintenance_worker", Label: "Maintenance worker", Banners: []string{}}
	switch ids, worst := bannersWithID(bs, "C07"); {
	case worst != "":
		maint.Status, maint.Banners = worst, ids
	case worker != nil && worker.Reachable:
		maint.Status = statusOK
	case worker != nil:
		maint.Status, maint.Detail = statusFailed, "the worker did not answer a ping"
	case s.Fleet.Maintain != nil && s.Fleet.Maintain().Known:
		maint.Status = statusOK
	default:
		maint.Status, maint.Detail = statusNotChecked, "the worker has not been probed"
	}

	db := SystemComponentJSON{ID: "database", Label: "Console database", Banners: []string{}}
	schema, serr := s.Store.SchemaVersion(ctx)
	switch ids, worst := bannersWithID(bs, "C11"); {
	case serr != nil:
		db.Status, db.Detail = statusFailed, "the database did not answer"
	case worst != "":
		db.Status, db.Banners = worst, ids
	default:
		db.Status = statusOK
	}

	eval := SystemComponentJSON{ID: "evaluator", Label: "Alarm evaluator", Banners: []string{}}
	ej, hasEval := f.JobStatus(health.EvaluatorJob)
	switch ids, worst := bannersWithID(bs, "C06"); {
	case worst != "":
		eval.Status, eval.Banners = worst, ids
	case !hasEval || ej.LastFinishedAt == nil:
		eval.Status, eval.Detail = statusNotChecked, "the alarm-evaluator job has not run"
	default:
		eval.Status, eval.CheckedAt = statusOK, ptrTime(*ej.LastFinishedAt)
	}

	key := SystemComponentJSON{ID: "master_key", Label: "Master key", Banners: []string{}, Status: statusOK}
	if ids, worst := bannersWithID(bs, "C12"); worst != "" {
		key.Status, key.Banners = worst, ids
	}

	jobsC := SystemComponentJSON{ID: "jobs", Label: "Background jobs", Banners: []string{}}
	failing, overdue := 0, 0
	for _, jb := range f.Jobs {
		if jb.LastStatus == "error" {
			failing++
		}
		if jobOverdue(jb, now) {
			overdue++
		}
	}
	switch {
	case failing > 0 || overdue > 0:
		jobsC.Status, jobsC.Detail = statusWarn, jobSummary(failing, overdue)
	case len(f.Jobs) == 0:
		jobsC.Status, jobsC.Detail = statusNotChecked, "no job has run yet"
	default:
		jobsC.Status = statusOK
	}

	clock := SystemComponentJSON{ID: "clock", Label: "Server clock", Banners: []string{}, Status: statusOK}
	if ids, worst := bannersWithID(bs, "C13"); worst != "" {
		clock.Status, clock.Banners = worst, ids
	}
	releases := SystemComponentJSON{ID: "release_manifest", Label: "Release manifest", Banners: []string{}, Status: statusOK}
	if ids, worst := bannersWithID(bs, "C16"); worst != "" {
		releases.Status, releases.Banners = worst, ids
	}

	out.Components = []SystemComponentJSON{gateway, storage, smtp, certs, agentAPI, maint, db, eval, key, jobsC, clock, releases}
	out.Overall = statusOK
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

// ProbeCheckJSON is the result of one probe check. Status is ok, warn, failed or
// not_checked; ok only after the prober ran in this call and raised nothing.
type ProbeCheckJSON struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Detail  string   `json:"detail,omitempty"`
	Banners []string `json:"banners"`
}

// ProbeJobJSON is the health-prober job's record after the run.
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
}

func (s *Service) storageTargetProbe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	t, err := s.Store.GetStorageTarget(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	runnable := s.Jobs != nil && slices.Contains(s.Jobs.Names(), proberJob)
	if runnable {
		busy, err := s.jobBusy(ctx, proberJob)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if busy {
			httpx.Problem(w, r, api.CodeConflict, 0)
			return
		}
	}
	if !s.auditAccepted(w, r, p, "storage_target.probe", "storage_target", t.ID) {
		return
	}
	out := ProbeResponse{TargetID: t.ID, Target: t.Name, Checks: []ProbeCheckJSON{}}
	notChecked := func(reason string) {
		out.Reason = reason
		for _, id := range []string{"gateway", "storage", "certificates"} {
			out.Checks = append(out.Checks, ProbeCheckJSON{ID: id, Status: statusNotChecked, Detail: reason, Banners: []string{}})
		}
	}
	if !runnable {
		notChecked("the health-prober job is not registered in this console")
	} else {
		pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		ran, rerr := s.Jobs.RunOnce(pctx, proberJob)
		cancel()
		out.Ran = ran
		if row, gerr := s.Store.GetJobState(ctx, proberJob); gerr == nil {
			out.Job = &ProbeJobJSON{Status: row.LastStatus, ErrorClass: row.LastErrorClass, DurationMS: row.LastDurationMS}
		}
		switch {
		case !ran && rerr == nil:
			httpx.Problem(w, r, api.CodeConflict, 0) // lost the lease to another run between the check and now
			return
		case rerr != nil:
			out.Ran = false
			notChecked("the health-prober job failed to run")
		default:
			f, ferr := s.Fleet.Fleet(ctx, p.Scope)
			if ferr != nil {
				s.fail(w, r, ferr)
				return
			}
			for _, c := range []struct {
				id  string
				ids []string
			}{{"gateway", []string{"C01"}}, {"storage", []string{"C02"}}, {"certificates", []string{"C05"}}} {
				ids, worst := bannersWithID(f.View.Banners, c.ids...)
				chk := ProbeCheckJSON{ID: c.id, Status: statusOK, Banners: ids}
				if worst != "" {
					chk.Status = worst
				}
				if chk.Banners == nil {
					chk.Banners = []string{}
				}
				out.Checks = append(out.Checks, chk)
			}
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
