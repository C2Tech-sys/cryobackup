package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
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

// shutdownGate is what serve's shutdown needs of the routes whose work can outlive
// the HTTP server's grace: manual job runs, which run past their request, and POST
// probes, which wait up to probeNowTimeout and write their audit row after it.
type shutdownGate struct {
	mu       sync.Mutex
	stopping bool
	jobs     context.Context // the parent of every manual job run
	cancel   context.CancelFunc
	probes   sync.WaitGroup // POST probes in flight
}

// jobContext is the manual job runs' parent context: cancelled by BeginShutdown.
func (s *Service) jobContext() context.Context {
	g := &s.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.jobs == nil {
		g.jobs, g.cancel = context.WithCancel(context.Background())
		if g.stopping {
			g.cancel()
		}
	}
	return g.jobs
}

// enterJobRun counts one manual job run; false once shutdown has begun.
func (s *Service) enterJobRun() bool {
	g := &s.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return false
	}
	jobBackground.Add(1)
	return true
}

// enterProbe counts one POST probe; false once shutdown has begun.
func (s *Service) enterProbe() bool {
	g := &s.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return false
	}
	g.probes.Add(1)
	return true
}

// BeginShutdown refuses new manual job runs and POST probes (503) and cancels the
// manual job runs in flight. It is the first half of serve's shutdown; the prober
// is stopped next (which ends the POST probes' waits), then WaitBackground.
func (s *Service) BeginShutdown() {
	g := &s.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopping = true
	if g.cancel != nil {
		g.cancel()
	}
}

// WaitBackground waits at most d for the manual job runs and POST probes in
// flight, so that they end before the store closes, and reports whether they all
// did. A job or probe that outlives d and is not inside a store call finds the
// store closed: its writes answer store.ErrClosed, and a POST probe then fails its
// request with a 500 because the audit row could not be written. One stuck inside
// a Write holds the store's close until Store.Close gives up on its own bound, and
// the process then exits with that transaction unfinished.
func (s *Service) WaitBackground(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		jobBackground.Wait()
		s.gate.probes.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		s.log().Warn("admin api: background work abandoned at shutdown", logx.Class("shutdown"))
		return false
	}
}

// ---- system/health ----

// Component statuses. ok is only ever said on evidence: a probe that has not
// run, or cannot, is not_checked, never ok.
//
// Two statuses say a row has nothing to judge, for a reason that is not a problem:
// not_in_build is a check this build does not run at all (state's NotInBuild
// markers, health.NotInBuild), and not_applicable is one that cannot apply to this
// console as it stands (too few devices for the clock comparison, no quota set, the
// digest turned off). Neither is ok and neither is evidence of health, so each row
// says which it is and why, but neither stops overall reaching ok: a healthy console
// must be able to read all clear. not_checked, for something that IS in this build,
// always blocks it.
const (
	statusOK            = "ok"
	statusWarn          = "warn"
	statusFailed        = "failed"
	statusNotChecked    = "not_checked"
	statusNotInBuild    = "not_in_build"
	statusNotApplicable = "not_applicable"
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
// is ok, not_in_build or not_applicable (overallOf).
type SystemHealthResponse struct {
	StampJSON
	// FirstRun is the engine's "this fleet has never enrolled a device". Overall can be ok then,
	// which says nothing about backups: the fleet_data row and the page say so plainly.
	FirstRun   bool                  `json:"first_run,omitempty"`
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
	"storage":            {"C02", "C02b"},
	"storage_quota":      {"C15", "C15b"},
	"smtp":               {"C03", "C03b"},
	"digest":             {"C14", "C14b"},
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

// noDevicesYet is what the fleet_data row and S36 say on a console that has never
// enrolled a device: the engine calls that all clear, and nothing is protected.
const noDevicesYet = "No devices enrolled yet: nothing is being backed up"

// healthRowOrder is the order of the rows on the page.
var healthRowOrder = []struct{ id, label string }{
	{"gateway", "Gateway"}, {"storage", "Storage"}, {"storage_quota", "Storage quota"}, {"smtp", "Alert e-mail"}, {"digest", "Daily digest"},
	{"certificates", "Certificates"}, {"agent_api", "Agent API"}, {"maintenance_worker", "Maintenance worker"},
	{"database", "Console database"}, {"self_backup", "Database self-backup"}, {"evaluator", "Alarm evaluator"}, {"master_key", "Master key"},
	{"jobs", "Background jobs"}, {"fleet_data", "Fleet data"}, {"clock", "Server clock"}, {"release_manifest", "Release manifest"},
}

// jobsFirstRunGrace is how long after this console started a registered job may
// still have no finished run before it counts as never having run. The runner
// starts every job within 5 s of its own start, whatever the job's period
// (jobs.Runner.loop), and a run cannot outlast its 5 min default lease
// (jobs.defaultLease): past that the job was not merely slow. Inside the grace the
// row is not_checked, not a warning, so a fresh console does not alarm; it is still
// not ok, because nothing has shown that job works yet.
const jobsFirstRunGrace = 5 * time.Minute

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
	// EvaluatorLast and SchedulerLast are the job_state rows of the alarm-evaluator and the maintenance
	// scheduler: how the newest run ended. The time of the newest good run is in Probes (Evaluator,
	// Maintain.Scheduler), as the engine reads it. A zero value has no status.
	EvaluatorLast, SchedulerLast store.JobState
	// MasterKeyWired: the master key state is known to the evaluator. MasterKeyLoaded is that state.
	MasterKeyWired, MasterKeyLoaded bool
	// ReportingDevices is how many devices C13 judged from (Fleet.Reporting), in this request's scope.
	// Scoped says that scope is not the whole fleet, so the clock row says it speaks for this account's view.
	ReportingDevices int
	Scoped           bool
	// FirstRun: this fleet has never enrolled a device (FleetView.FirstRun).
	FirstRun bool
	// Jobs are the rows of the jobs registered in this process: a row left by a job this process
	// does not run says nothing about this console's health, and the operator cannot clear it.
	Jobs []store.JobState
	// NeverRun names the registered jobs with no finished run and no run in progress. FirstRunning
	// names those whose first run is in progress now.
	NeverRun, FirstRunning []string
	// Probes are the probe facts the engine judged (Fleet.ProbeFacts: the health-prober's, the two job
	// heartbeats and the NotInBuild markers), Endpoint the agent endpoint self-probe (Fleet.Endpoint) and
	// MaintainReachable the periodic worker probe's answer. They are the evidence a probe-driven row
	// needs: a row reaches ok only on its own evidence.
	Probes            state.ProbeFacts
	Endpoint          probe.Status
	MaintainReachable bool
}

// ---- row evidence ----
//
// Every row starts at not_checked and reaches ok only on evidence that is
// specific to that row (M3-T4 delta, item 1). A probe-driven row that has no
// recent passing probe is not ok: no probe yet, an unconfigured probe or a stale
// one is not_checked, and a probe whose latest run is failing is warn (before the
// streak and the grace raise C01, C02, C03 or C04, the row already says so).

// probeStale is how old a probe's newest run may be and still be evidence.
func (f healthFacts) probeStale() time.Duration {
	iv := f.Probes.ProberInterval
	if iv <= 0 {
		iv = state.DefaultProberInterval
	}
	return time.Duration(state.ProbeStaleIntervals) * iv
}

func millisTime(m state.Millis) time.Time { return time.UnixMilli(int64(m)).UTC() }

// probeEvidence judges one prober run: ok only for a fresh, passing run. It
// returns the time of that run as the row's checked-at.
func (f healthFacts) probeEvidence(r *state.ProbeRun, what string) (status, detail string, at *time.Time) {
	switch {
	case !f.Probes.Wired:
		return statusNotChecked, "the console's probe facts are not wired", nil
	case r == nil:
		return statusNotChecked, "no " + what + " probe is supplied", nil
	case r.Unconfigured != "":
		return statusNotChecked, "not checked: " + r.Unconfigured, nil
	case r.LastRunAt == 0:
		return statusNotChecked, "the " + what + " probe has never completed a run", nil
	}
	last := millisTime(r.LastRunAt)
	if age := f.Now.Sub(last); age > f.probeStale() {
		return statusNotChecked, "the " + what + " probe has not completed for " + strconv.Itoa(int(age/time.Minute)) + " min, so its last result is not evidence of now", nil
	}
	if r.ConsecutiveFailures > 0 || r.LastOKAt != r.LastRunAt {
		d := "the latest " + what + " probe failed"
		if r.LastErrorClass != "" {
			d += " (" + r.LastErrorClass + ")"
		}
		return statusWarn, d, ptrTime(last)
	}
	return statusOK, "", ptrTime(last)
}

// endpointStale is how old the agent endpoint self-probe's newest run may be and
// still be evidence. It has its own period (agent_probe.interval_s), which is not
// the health-prober's: both rows that read this probe judge it by this one.
func (f healthFacts) endpointStale() time.Duration {
	iv := f.Endpoint.Interval
	if iv <= 0 {
		iv = state.DefaultProberInterval
	}
	return time.Duration(state.ProbeStaleIntervals)*iv + probe.TotalTimeout
}

// agentEndpointEvidence judges the agent endpoint self-probe the same way.
func (f healthFacts) agentEndpointEvidence() (status, detail string, at *time.Time) {
	st := f.Endpoint
	switch {
	case !f.EndpointWired:
		return statusNotChecked, "the agent endpoint self-probe is not wired", nil
	case !st.Runs:
		d := "the agent endpoint self-probe cannot run"
		if st.Limit != "" {
			d += " (" + st.Limit + ")"
		}
		return statusNotChecked, d, nil
	case !st.Known:
		return statusNotChecked, "the agent endpoint self-probe has not completed a run yet", nil
	}
	if age := f.Now.Sub(st.Last.At); age > f.endpointStale() {
		return statusNotChecked, "the agent endpoint self-probe has not completed for " + strconv.Itoa(int(age/time.Minute)) + " min, so its last result is not evidence of now", nil
	}
	if !st.Last.OK {
		d := "the latest agent endpoint self-probe failed"
		if st.Last.Class != "" {
			d += " (" + st.Last.Class + ")"
		}
		return statusWarn, d, ptrTime(st.Last.At)
	}
	return statusOK, "", ptrTime(st.Last.At)
}

// certRefusalClasses are the failure classes of a probe whose handshake got a
// certificate and refused it (probe.transportClass and verifyClass, and the self-probe's pin).
var certRefusalClasses = map[string]bool{"expired": true, "chain_expired": true, "untrusted": true, "hostname": true, "invalid": true, "pin_mismatch": true}

// certRefusal words a refused certificate of a listener.
func certRefusal(who, class string) string {
	if class == "expired" {
		return who + " has an expired certificate"
	}
	return who + " has a certificate that failed verification (" + class + ")"
}

// certificateEvidence is ok only when a recent probe of each listener completed a
// handshake that accepted the certificate, and that certificate has not run out since:
// the gateway (by the gateway probe) and the agent endpoint (by its self-probe). A
// certificate the probe refused is failed, because an agent would refuse it too, however
// fresh the expiry on file is. C04 reports the same self-probe on the agent_api row and
// returns before C05, so no banner corrects this row: it has to judge the run itself.
func (f healthFacts) certificateEvidence() (status, detail string, at *time.Time) {
	var refused, missing []string
	var oldest time.Time
	note := func(t time.Time) {
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	refuse := func(reason string, t time.Time) {
		refused = append(refused, reason)
		note(t)
	}
	g := f.Probes.Gateway
	switch {
	case !f.Probes.Wired || g == nil || g.Unconfigured != "" || g.LastRunAt == 0:
		missing = append(missing, "the gateway")
	case f.Now.Sub(millisTime(g.LastRunAt)) > f.probeStale():
		missing = append(missing, "the gateway (its latest probe is stale, so it is not evidence of now)")
	case g.LastOKAt != g.LastRunAt && certRefusalClasses[g.LastErrorClass]:
		refuse(certRefusal("the gateway", g.LastErrorClass), millisTime(g.LastRunAt))
	case g.LastOKAt != g.LastRunAt:
		missing = append(missing, "the gateway (its latest probe failed, so it read no certificate)")
	case g.CertNotAfter == 0:
		missing = append(missing, "the gateway")
	case !f.Now.Before(millisTime(g.CertNotAfter)):
		refuse(certRefusal("the gateway", "expired"), millisTime(g.LastRunAt))
	default:
		note(millisTime(g.LastRunAt))
	}
	e := f.Endpoint
	switch {
	case !f.EndpointWired || !e.Runs || !e.Known:
		missing = append(missing, "the agent listener")
	case f.Now.Sub(e.Last.At) > f.endpointStale():
		missing = append(missing, "the agent listener (its self-probe is stale)")
	case !e.Last.OK && e.Last.Stage == probe.StageCertExpired:
		refuse(certRefusal("the agent listener", "expired"), e.Last.At)
	case !e.Last.OK && e.Last.Stage == probe.StageTLS && !e.Last.NotAfter.IsZero():
		refuse(certRefusal("the agent listener", e.Last.Class), e.Last.At)
	case e.Last.NotAfter.IsZero():
		missing = append(missing, "the agent listener")
	case !f.Now.Before(e.Last.NotAfter):
		refuse(certRefusal("the agent listener", "expired"), e.Last.At)
	default:
		note(e.Last.At)
	}
	switch {
	case len(refused) > 0:
		return statusFailed, strings.Join(refused, "; "), ptrTime(oldest)
	case len(missing) > 0:
		return statusNotChecked, "no certificate expiry was read recently from " + strings.Join(missing, " or "), nil
	}
	return statusOK, "", ptrTime(oldest)
}

// jobClassOf is " (class)" of a failed run, or nothing.
func jobClassOf(j store.JobState) string {
	if j.LastErrorClass == "" {
		return ""
	}
	return " (" + j.LastErrorClass + ")"
}

// evaluatorEvidence is ok only when the newest evaluation passed and is recent. The
// job's own row says how the newest run ended, and the engine's fact (the
// health.EvaluatorOKJob row, written only after a good evaluation) says when one last
// passed. C06 reads only the second, so it stays quiet for up to three periods after a
// run starts failing: this row says so at once.
func (f healthFacts) evaluatorEvidence() (status, detail string, at *time.Time) {
	var good *time.Time
	e := f.Probes.Evaluator
	if e != nil && e.LastFinishedAt != 0 {
		good = ptrTime(millisTime(e.LastFinishedAt))
	}
	switch {
	case f.EvaluatorLast.LastStatus == "error":
		return statusWarn, "the alarm evaluator's latest run failed" + jobClassOf(f.EvaluatorLast), good
	case good == nil:
		return statusNotChecked, "the alarm evaluator has not completed a good evaluation", nil
	}
	if age := f.Now.Sub(*good); age > jobs.EvaluatorHealthyPeriods*health.EvaluatorPeriod {
		return statusNotChecked, "the alarm evaluator has not completed a good evaluation for " + strconv.Itoa(int(age/time.Minute)) + " min", nil
	}
	return statusOK, "", good
}

// workerEvidence is ok only when the worker answers AND its scheduler's newest pass
// passed and is recent: a worker that answers a ping can be failing every pass. The
// scheduler's row is what `cryo-console maintain` writes (maintain.Worker.recordJob).
func (f healthFacts) workerEvidence() (status, detail string, at *time.Time) {
	switch {
	case f.WorkerPinged && !f.WorkerReachable:
		return statusFailed, "the worker did not answer a ping", nil
	case f.WorkerReachable, f.MaintainKnown && f.MaintainReachable:
	case f.MaintainKnown:
		return statusWarn, "the latest worker probe did not reach it", nil
	default:
		return statusNotChecked, "the worker has not been probed", nil
	}
	var last *time.Time
	if m := f.Probes.Maintain; m != nil && m.Scheduler != nil && m.Scheduler.LastFinishedAt != 0 {
		last = ptrTime(millisTime(m.Scheduler.LastFinishedAt))
	}
	switch {
	case f.SchedulerLast.LastStatus == "error":
		return statusWarn, "the worker answers, but the maintenance scheduler's latest pass failed" + jobClassOf(f.SchedulerLast), last
	case last == nil:
		return statusNotChecked, "the worker answers, but the maintenance scheduler has not finished a pass", nil
	}
	if age := f.Now.Sub(*last); age > state.MaintenanceSchedulerStale {
		return statusNotChecked, "the worker answers, but the maintenance scheduler has not finished a pass for " + strconv.Itoa(int(age/time.Minute)) + " min", nil
	}
	return statusOK, "", last
}

// releaseEvidence is ok only for a recent check whose verdict was not a rejection.
func (f healthFacts) releaseEvidence() (status, detail string, at *time.Time) {
	r := f.Probes.Release
	switch {
	case r == nil:
		return statusNotChecked, "the console does not supply the release check", nil
	case r.NotInBuild:
		return statusNotInBuild, "this build does not run release-check, so no release manifest is checked", nil
	case r.CheckedAt == 0:
		return statusNotChecked, "release-check has never run", nil
	}
	checked := millisTime(r.CheckedAt)
	switch age := f.Now.Sub(checked); {
	case r.Rejected:
		return statusWarn, "a release manifest was rejected", ptrTime(checked)
	case age > state.ReleaseCheckMaxAge:
		return statusNotChecked, "release-check has not run for " + strconv.Itoa(int(age/time.Minute)) + " min", nil
	}
	return statusOK, "", ptrTime(checked)
}

// quotaEvidence is ok only when every target with a soft quota was measured recently.
func (f healthFacts) quotaEvidence() (status, detail string, at *time.Time) {
	q := f.Probes.Quota
	switch {
	case q == nil:
		return statusNotChecked, "the console does not supply storage usage", nil
	case q.NotInBuild:
		return statusNotInBuild, "this build does not run the provider-side usage listing, so usage against a storage quota is not measured", nil
	case q.WithQuota == 0:
		return statusNotApplicable, "no storage target has a soft quota set", nil
	}
	var oldest time.Time
	seen := 0
	for _, t := range q.Targets {
		if t.SoftQuotaBytes <= 0 {
			continue
		}
		seen++
		if t.MeasuredAt == 0 {
			return statusNotChecked, "the usage of a storage target with a quota has never been measured", nil
		}
		if m := millisTime(t.MeasuredAt); oldest.IsZero() || m.Before(oldest) {
			oldest = m
		}
	}
	if seen < q.WithQuota {
		return statusNotChecked, "the usage of a storage target with a quota is missing", nil
	}
	if age := f.Now.Sub(oldest); age > state.QuotaMeasureMaxAge {
		return statusNotChecked, "the usage of a storage target with a quota has not been measured for " + strconv.Itoa(int(age/time.Hour)) + " h", nil
	}
	return statusOK, "", ptrTime(oldest)
}

// digestEvidence is ok only on a digest actually sent; C14 raises the one that was missed.
func (f healthFacts) digestEvidence() (status, detail string, at *time.Time) {
	d := f.Probes.Digest
	switch {
	case d == nil:
		return statusNotChecked, "the console does not supply the daily digest's state", nil
	case d.NotInBuild:
		return statusNotInBuild, "this build does not send the daily digest", nil
	case d.Disabled:
		return statusNotApplicable, "the daily digest is turned off", nil
	case d.LastSentAt == 0:
		return statusNotChecked, "no daily digest has been sent yet", nil
	}
	last := millisTime(d.LastSentAt)
	if age := f.Now.Sub(last); age > digestStale {
		return statusWarn, "no daily digest has been sent for " + strconv.Itoa(int(age/time.Hour)) + " h", ptrTime(last)
	}
	return statusOK, "", ptrTime(last)
}

// digestStale is how old the newest digest may be: two daily sends and an hour. C14 names
// a missed send sooner, from the configured time; this row holds without it.
const digestStale = 49 * time.Hour

// selfBackupEvidence judges the database's own backup. Its failures raise C11, on the database row.
func (f healthFacts) selfBackupEvidence() (status, detail string, at *time.Time) {
	var sb *state.SelfBackupFacts
	if d := f.Probes.Database; d != nil {
		sb = d.SelfBackup
	}
	switch {
	case sb == nil:
		return statusNotChecked, "the console does not supply the database self-backup's state", nil
	case sb.NotInBuild:
		return statusNotInBuild, "this build does not run db-self-backup, so the console database's own backup is not judged", nil
	case sb.VerifyFailed:
		return statusFailed, "the newest tested restore of the self-backup failed", nil
	case sb.LastAt == 0:
		return statusNotChecked, "no self-backup has been written", nil
	}
	last := millisTime(sb.LastAt)
	if f.Now.Sub(last) > state.DBSelfBackupMaxAge {
		return statusWarn, "the newest self-backup is too old", ptrTime(last)
	}
	return statusOK, "", ptrTime(last)
}

// jobsEvidence is ok only when every job registered in this process has a finished run
// and none is failing or overdue. A job with no finished run counts against the row: not_checked
// while its first run is in progress or inside jobsFirstRunGrace of this console's start, a
// warning after it (or at once when the start is unknown).
func (f healthFacts) jobsEvidence() (status, detail string) {
	failing, overdue := 0, 0
	for _, jb := range f.Jobs {
		if jb.LastStatus == "error" {
			failing++
		}
		if jobOverdue(jb, f.Now) {
			overdue++
		}
	}
	never := len(f.NeverRun)
	young := f.Probes.StartedAt != 0 && f.Now.Sub(millisTime(f.Probes.StartedAt)) < jobsFirstRunGrace
	switch {
	case failing > 0 || overdue > 0:
		return statusWarn, jobSummary(failing, overdue)
	case never > 0 && !young:
		return statusWarn, strconv.Itoa(never) + " registered job(s) have never run: " + strings.Join(f.NeverRun, ", ")
	case never > 0 || len(f.FirstRunning) > 0:
		return statusNotChecked, strconv.Itoa(never+len(f.FirstRunning)) + " registered job(s) have not finished a first run yet"
	case len(f.Jobs) == 0:
		return statusNotChecked, "no job has run yet"
	}
	return statusOK, ""
}

// unfinishedJobs names the registered jobs with no finished run: never, for a row with no
// status (the table's never_run) that is not running now and a job with no row; running,
// for a row with no status whose first run holds the lease now.
func unfinishedJobs(names []string, rows []store.JobState, now time.Time) (never, running []string) {
	seen := map[string]bool{}
	for _, j := range rows {
		seen[j.Name] = true
		if j.LastStatus == "" {
			if jobRunning(j, now) {
				running = append(running, j.Name)
			} else {
				never = append(never, j.Name)
			}
		}
	}
	for _, n := range names {
		if !seen[n] {
			never = append(never, n)
		}
	}
	return never, running
}

// clockEvidence says what C13 judged from, in the scope of this request: the same World and
// the same state.ReportingDevices. For a scoped account that is its own customers' devices, so
// the row says so, and no number from outside them reaches it.
func (f healthFacts) clockEvidence() (status, detail string) {
	n, least := strconv.Itoa(f.ReportingDevices), strconv.Itoa(state.ClockSuspectMinDevices)
	switch enough := f.ReportingDevices >= state.ClockSuspectMinDevices; {
	case !enough && f.Scoped:
		return statusNotApplicable, "needs at least " + least + " devices reporting among those this account can see; " + n + " report here"
	case !enough:
		return statusNotApplicable, "needs at least " + least + " devices reporting to compare clocks; " + n + " report now"
	case f.Scoped:
		return statusOK, "compared with the " + n + " reporting devices this account can see"
	}
	return statusOK, "compared with " + n + " reporting devices"
}

// statusRank orders the statuses from no concern to the worst. A check this build does not
// run, or that cannot apply here, ranks with ok: neither is a concern.
var statusRank = map[string]int{statusOK: 0, statusNotInBuild: 0, statusNotApplicable: 0, statusNotChecked: 1, statusWarn: 2, statusFailed: 3}

// overallOf is the worst component: failed, then warn, then not_checked. A check
// this build does not run (not_in_build), or that cannot apply here
// (not_applicable), does not count against it; not_checked for anything else does,
// because a row with no evidence is not being monitored.
func overallOf(cs []SystemComponentJSON) string {
	overall := statusOK
	for _, c := range cs {
		if statusRank[c.Status] > statusRank[overall] {
			overall = c.Status
		}
	}
	return overall
}

// isJob reports whether a job_state row is a job: not the health-prober's lease
// and per-probe rows (store.IsJobRow) and not the alarm evaluator's marker row,
// which carries its last good run for the health rows.
func isJob(name string) bool {
	return store.IsJobRow(name) && name != health.EvaluatorOKJob
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
		// A row starts at not_checked and reaches ok only on its own evidence.
		status, detail := statusNotChecked, "no evidence for this row"
		switch row.id {
		case "gateway":
			status, detail, c.CheckedAt = f.probeEvidence(f.Probes.Gateway, "gateway")
		case "storage":
			status, detail, c.CheckedAt = f.probeEvidence(f.Probes.Storage, "storage")
		case "smtp":
			if sm := f.Probes.SMTP; sm != nil {
				status, detail, c.CheckedAt = f.probeEvidence(&sm.Probe, "alert e-mail")
				recentSend := sm.LastSendOKAt != 0 && f.Now.Sub(millisTime(sm.LastSendOKAt)) <= 24*time.Hour
				switch {
				case sm.ConsecutiveSendFailures > 0:
					status, detail = statusWarn, "the latest alert e-mail send failed"
				case sm.LastTestFailed:
					status, detail = statusWarn, "the latest test e-mail failed"
				case sm.PendingAlerts > 0 && (sm.LastSendOKAt == 0 || f.Now.Sub(millisTime(sm.LastSendOKAt)) > state.SMTPPendingWithoutSend):
					status, detail = statusWarn, "alert e-mail is waiting to go out and none has been delivered recently"
				case status == statusNotChecked && recentSend && sm.Probe.Unconfigured == "":
					status, detail, c.CheckedAt = statusOK, "", ptrTime(millisTime(sm.LastSendOKAt)) // a recent delivery is evidence
				}
			} else {
				status, detail = statusNotChecked, "no alert e-mail probe is supplied"
			}
		case "certificates":
			status, detail, c.CheckedAt = f.certificateEvidence()
		case "agent_api":
			status, detail, c.CheckedAt = f.agentEndpointEvidence()
		case "release_manifest":
			status, detail, c.CheckedAt = f.releaseEvidence()
		case "storage_quota":
			status, detail, c.CheckedAt = f.quotaEvidence()
		case "digest":
			status, detail, c.CheckedAt = f.digestEvidence()
		case "self_backup":
			status, detail, c.CheckedAt = f.selfBackupEvidence()
		case "maintenance_worker":
			status, detail, c.CheckedAt = f.workerEvidence()
		case "database":
			switch d := f.Probes.Database; {
			case f.SchemaErr != nil:
				status, detail = statusFailed, "the database did not answer"
			case d != nil && d.IntegrityCheckedAt != 0 && !d.IntegrityOK:
				status, detail = statusFailed, "the latest integrity check failed"
			case d != nil && d.WorkerStoreFailures != 0:
				status, detail = statusWarn, "the maintenance worker could not read or write this database on its latest storage probes"
			default:
				status, detail = statusOK, ""
			}
		case "evaluator":
			status, detail, c.CheckedAt = f.evaluatorEvidence()
		case "master_key":
			switch m := f.Probes.MasterKey; {
			case !f.MasterKeyWired:
				status, detail = statusNotChecked, "the master key state is not reported"
			case !f.MasterKeyLoaded || (m != nil && !m.Loaded):
				status, detail = statusFailed, "the master key is not loaded"
			case m != nil && slices.ContainsFunc(m.EscrowKeyIDs, func(id string) bool { return id != m.KeyID }):
				status, detail = statusFailed, "an escrowed repository key is wrapped under another master key"
			default:
				status, detail = statusOK, ""
			}
		case "clock":
			status, detail = f.clockEvidence()
		case "fleet_data":
			switch {
			case !f.Complete:
				status, detail = statusWarn, "the facts load was incomplete"
			case f.FirstRun:
				status, detail = statusNotApplicable, noDevicesYet
			default:
				status, detail = statusOK, ""
			}
		case "jobs":
			status, detail = f.jobsEvidence()
		}
		switch {
		case worst != "" && statusRank[worst] >= statusRank[status]:
			status, detail = worst, ""
		case worst != "":
			// A weaker banner beside a worse fact: "not checked" does not soften a refused certificate.
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
		MasterKeyWired: s.Fleet.MasterLoaded != nil, Probes: s.Fleet.ProbeFacts(f.Jobs, nil, true),
		ReportingDevices: f.Reporting, Scoped: !p.Scope.All, FirstRun: f.View.FirstRun}
	facts.MasterKeyLoaded = facts.MasterKeyWired && s.Fleet.MasterLoaded()
	var names []string
	if s.Jobs != nil {
		names = s.Jobs.Names()
	}
	for _, j := range f.Jobs {
		// Only what this process runs. The prober's rows and the evaluator's marker are not jobs; a row
		// left by a job no longer registered, and the maintenance scheduler (the worker's row judges it), say nothing here.
		if isJob(j.Name) && slices.Contains(names, j.Name) {
			facts.Jobs = append(facts.Jobs, j)
		}
	}
	facts.NeverRun, facts.FirstRunning = unfinishedJobs(names, facts.Jobs, f.Computed)
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
		m := s.Fleet.Maintain()
		facts.MaintainKnown, facts.MaintainReachable = m.Known, m.Reachable
	}
	if s.Fleet.Endpoint != nil {
		facts.Endpoint = s.Fleet.Endpoint()
	}
	schema, serr := s.Store.SchemaVersion(ctx)
	facts.SchemaErr = serr
	facts.EvaluatorLast, _ = f.JobStatus(health.EvaluatorJob)
	facts.SchedulerLast, _ = f.JobStatus(health.SchedulerJob)
	out := SystemHealthResponse{StampJSON: StampJSON{ComputedAt: f.Computed, LoadedAt: f.View.LoadedAt, Complete: f.View.Complete}, Banners: bannersJSON(f.View.Banners),
		Components: buildComponents(facts), FirstRun: f.View.FirstRun}
	out.Overall = overallOf(out.Components)
	out.Versions = VersionsJSON{Console: buildinfo.Version, Commit: buildinfo.Commit, BuildDate: buildinfo.Date, Schema: schema, Maintain: worker}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// LabHealthy is overall and the rows of a small healthy console, from the same rules as a real
// answer: every check in this build passing, three devices reporting. Only fixtures mode's
// lab view shows it (/fixtures?health=ok): the fictional fleet raises real conditions and
// has more than five devices, so these states can be seen no other way.
func LabHealthy(now time.Time) (overall string, rows []SystemComponentJSON) {
	at := func(d time.Duration) state.Millis { return state.Millis(now.Add(-d).UnixMilli()) }
	ok := func() state.ProbeRun {
		return state.ProbeRun{LastRunAt: at(20 * time.Second), LastOKAt: at(20 * time.Second)}
	}
	gw, st := ok(), ok()
	gw.CertNotAfter = state.Millis(now.Add(365 * 24 * time.Hour).UnixMilli())
	good := &state.JobFacts{LastFinishedAt: at(time.Minute)}
	rows = buildComponents(healthFacts{Now: now, Complete: true, EndpointWired: true, WorkerPinged: true, WorkerReachable: true, MaintainKnown: true, MaintainReachable: true,
		EvaluatorLast: store.JobState{LastStatus: "ok"}, SchedulerLast: store.JobState{LastStatus: "ok"}, MasterKeyWired: true, MasterKeyLoaded: true, ReportingDevices: 3,
		Jobs: []store.JobState{{Name: "lab", LastStatus: "ok"}},
		Probes: state.ProbeFacts{Wired: true, Gateway: &gw, Storage: &st, SMTP: &state.SMTPFacts{Probe: ok()}, Evaluator: good,
			Maintain: &state.MaintainFacts{RPC: ok(), Scheduler: good}, Database: &state.DatabaseFacts{SelfBackup: &state.SelfBackupFacts{NotInBuild: true}},
			Digest: &state.DigestFacts{NotInBuild: true}, Quota: &state.QuotaFacts{NotInBuild: true}, Release: &state.ReleaseFacts{NotInBuild: true}},
		Endpoint: probe.Status{Runs: true, Known: true, Interval: time.Minute, Last: probe.Result{At: now.Add(-20 * time.Second), OK: true, NotAfter: now.Add(365 * 24 * time.Hour)}},
	})
	return overallOf(rows), rows
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

// overdueMargin is what a job may run late beyond the runner's jitter: the timer firing
// under load and the wait for the store.
const overdueMargin = time.Minute

// jobOverdue: a job whose next run is later than the runner can make it and which is not
// running now. The row holds the job's real schedule: RunOnce records the next due time as the
// finish plus the period, and the runner then fires up to jobs.MaxJitter of that period after it.
func jobOverdue(j store.JobState, now time.Time) bool {
	if j.NextDueAt == nil || jobRunning(j, now) {
		return false
	}
	var every time.Duration
	if j.LastFinishedAt != nil {
		every = j.NextDueAt.Sub(*j.LastFinishedAt)
	}
	return now.Sub(*j.NextDueAt) > jobs.MaxJitter(every)+overdueMargin
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
		if !isJob(j.Name) {
			continue // the health-prober's lease and per-probe rows and the evaluator's marker row are not jobs
		}
		seen[j.Name] = true
		row := JobJSON{Name: j.Name, Registered: registered[j.Name], Status: j.LastStatus, LastStartedAt: j.LastStartedAt, LastFinishedAt: j.LastFinishedAt,
			LastDurationMS: j.LastDurationMS, LastErrorClass: j.LastErrorClass, RunCount: j.RunCount, NextDueAt: j.NextDueAt, Running: jobRunning(j, now),
			Overdue: (registered[j.Name] || j.Name == health.SchedulerJob) && jobOverdue(j, now)} // nothing will run a job no longer scheduled, so it is not late
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
	if !s.enterJobRun() {
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second) // shutting down
		return
	}
	if !s.auditAccepted(w, r, p, "system.job.run", "job", name) {
		jobBackground.Done()
		return
	}
	go func() {
		defer jobBackground.Done()
		ctx, cancel := context.WithTimeout(s.jobContext(), jobRunTimeout)
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
// error that is context.Canceled. Both probes skipped (neither could run) are
// ran false and jobs.ErrNothingProbed. A probe that panicked is ran false and
// jobs.ErrProbePanicked.
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
		rowIDs := healthRowBanners[c.id]
		if c.id == "storage" { // this answer's storage check also speaks for the quota, which has its own row on the health page
			rowIDs = slices.Concat(rowIDs, healthRowBanners["storage_quota"])
		}
		ids, worst := worstOf(banners, rowIDs)
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
	if !s.enterProbe() {
		httpx.Problem(w, r, api.CodeUnavailable, 30*time.Second) // shutting down: no probe, so no audit row
		return
	}
	defer s.gate.probes.Done()
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
	// without a result; error "nothing_probed" when neither probe could run; error
	// "internal_panic" when a probe panicked). It is written even when the client has gone
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
		if !s.auditOutcome(w, r, p, auditAction, "storage_target", t.ID, "error", map[string]any{"outcome": "not_run"}) {
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
		case errors.Is(rerr, jobs.ErrNothingProbed):
			result, outcome = "error", "nothing_probed"
		case errors.Is(rerr, jobs.ErrProbePanicked):
			result, outcome = "error", jobs.ClassPanic
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
		case outcome == "nothing_probed":
			notChecked("nothing was probed: neither the gateway probe nor the storage probe could run")
		case outcome == jobs.ClassPanic:
			notChecked("a health probe failed with an internal error, so nothing can be said from it; the console log names the probe")
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
