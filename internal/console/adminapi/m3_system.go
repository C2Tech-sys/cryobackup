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
	"gateway":            {"C01", "C01b", "C17", "C17b"},
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
	// ProbeRows are the health-prober's per-probe job_state rows, by name (jobs.ProbeRow*): the record
	// of each attempt, including the ones that left its facts alone.
	ProbeRows map[string]store.JobState
	// Since is set by POST .../probe: when the request began. A probe result from before it is not evidence.
	Since time.Time
	// WorkerKeyLoaded is what the worker's answer to the ping said about the master key.
	WorkerKeyLoaded bool
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

// ---- the one rule ----
//
// Every row of the health page and every check of POST .../probe is judged by attempt.judge
// and judgeRow, and by nothing else. The functions below them only state facts: they
// build attempts and never name a status of health. A test reads this file and fails if one does.

type result int

const (
	noAttempt result = iota // nothing on record, or the check cannot run: nothing can be said
	passed
	failed
	skipped // it tried and could not run (busy, worker unreachable): neither a pass nor a failure
)

// attempt is everything one check knows about its newest try.
type attempt struct {
	Result    result
	At        time.Time     // when the newest attempt ended
	LastPass  time.Time     // when it last passed, for a skipped attempt to fall back on
	MaxAge    time.Duration // how old a pass may be and still be evidence; 0 is this request's own read
	NotBefore time.Time     // POST .../probe: a result from before the request is not evidence of it
	Severe    bool          // a failure here is failed, not warn
	What      string        // names the check in the words of a row ("the gateway probe")
	Why       string        // why it failed, was skipped, or has no attempt
	Note      string        // what an ok row also says
	Outside   string        // not_in_build or not_applicable: nothing here to judge, and Why says why
}

// judge is the rule for one attempt:
//   - no attempt: not_checked
//   - newest attempt failed: warn or failed, never ok, however fresh an older pass is
//   - newest attempt skipped: the last pass stands until MaxAge, then not_checked
//   - stale, or dated in the future (the engine calls that garbled): not_checked
//   - ok only when the newest attempt passed and is fresh
func (a attempt) judge(now time.Time) (status, detail string) {
	pass := a.LastPass
	if a.Result == passed {
		pass = a.At
	}
	noResult := a.What + " produced no result during this request, so its older result is not evidence of now"
	switch {
	case a.Outside != "":
		return a.Outside, a.Why
	case a.Result == noAttempt:
		return statusNotChecked, a.Why
	case a.At.Before(a.NotBefore):
		return statusNotChecked, noResult
	case a.Result == failed && a.Severe:
		return statusFailed, a.Why
	case a.Result == failed:
		return statusWarn, a.Why
	case pass.IsZero():
		return statusNotChecked, a.Why
	case pass.Before(a.NotBefore):
		return statusNotChecked, noResult
	case pass.After(now.Add(state.FutureSkewAllowed)):
		return statusNotChecked, a.What + " is dated in the future, so it is not evidence of now"
	case a.MaxAge > 0 && now.Sub(pass) > a.MaxAge:
		return statusNotChecked, a.What + " has not passed for " + strconv.Itoa(int(now.Sub(pass)/time.Minute)) + " min, so its last result is not evidence of now"
	}
	return statusOK, a.Note
}

// judgeRow is the row: its worst attempt, then its banners. It is ok only when every attempt is
// and no banner is raised. A banner only ever makes a row worse, and C00 (self-checks not
// wired) takes every otherwise-ok row to not_checked.
func judgeRow(now time.Time, attempts []attempt, worstBanner string, notWired bool) (status, detail string, at *time.Time) {
	status, detail = statusNotChecked, "no evidence for this row"
	for i, a := range attempts {
		st, d := a.judge(now)
		switch {
		case i == 0 || statusRank[st] > statusRank[status]:
			status, detail = st, d
		case statusRank[st] == statusRank[status] && d != "" && d != detail:
			detail = strings.TrimPrefix(detail+"; "+d, "; ")
		}
		if a.Result != noAttempt && a.Outside == "" && !a.At.IsZero() && (at == nil || a.At.Before(*at)) {
			at = ptrTime(a.At)
		}
	}
	switch {
	case worstBanner != "" && statusRank[worstBanner] > statusRank[status]:
		status, detail = worstBanner, ""
	case status == statusOK && notWired:
		status, detail = statusNotChecked, "console self-checks are not wired (C00): this is not being monitored"
	}
	return status, detail, at
}

// rowAttempts is what each row knows. A row added to healthRowOrder needs an entry here,
// and a case in the property test.
var rowAttempts = map[string]func(healthFacts) []attempt{
	"gateway":            healthFacts.gatewayAttempts,
	"storage":            healthFacts.storageAttempts,
	"storage_quota":      healthFacts.quotaAttempts,
	"smtp":               healthFacts.smtpAttempts,
	"digest":             healthFacts.digestAttempts,
	"certificates":       healthFacts.certificateAttempts,
	"agent_api":          healthFacts.agentAPIAttempts,
	"maintenance_worker": healthFacts.workerAttempts,
	"database":           healthFacts.databaseAttempts,
	"self_backup":        healthFacts.selfBackupAttempts,
	"evaluator":          healthFacts.evaluatorAttempts,
	"master_key":         healthFacts.masterKeyAttempts,
	"jobs":               healthFacts.jobsAttempts,
	"fleet_data":         healthFacts.fleetDataAttempts,
	"clock":              healthFacts.clockAttempts,
	"release_manifest":   healthFacts.releaseAttempts,
}

// ---- what each row knows ----

func millisTime(m state.Millis) time.Time { return time.UnixMilli(int64(m)).UTC() }

// probeStale is how old a prober run may be and still be evidence.
func (f healthFacts) probeStale() time.Duration {
	iv := f.Probes.ProberInterval
	if iv <= 0 {
		iv = state.DefaultProberInterval
	}
	return time.Duration(state.ProbeStaleIntervals) * iv
}

// endpointStale is the same for the agent endpoint self-probe, on its own period
// (agent_probe.interval_s), which is not the health-prober's.
func (f healthFacts) endpointStale() time.Duration {
	iv := f.Endpoint.Interval
	if iv <= 0 {
		iv = state.DefaultProberInterval
	}
	return time.Duration(state.ProbeStaleIntervals)*iv + probe.TotalTimeout
}

// withRow applies the attempt's own job_state row when it is newer than the facts. The prober
// keeps its facts in memory and leaves them alone when a run is skipped or errors, so only the
// row knows that the newest attempt did not pass.
func (f healthFacts) withRow(a attempt, name string) attempt {
	j, ok := f.ProbeRows[name]
	if !ok || j.LastFinishedAt == nil || !j.LastFinishedAt.After(a.At) {
		return a
	}
	if a.Result == passed && a.LastPass.IsZero() {
		a.LastPass = a.At
	}
	a.At = *j.LastFinishedAt
	switch j.LastStatus {
	case "ok":
		a.Result, a.LastPass = passed, a.At
	case "skipped":
		if a.Result == failed { // a skip is no result: the failure before it stands
			a.Why += "; it could not run since" + jobClassOf(j)
			break
		}
		a.Result, a.Why = skipped, a.What+" could not run"+jobClassOf(j)
	case "error":
		a.Result, a.Severe, a.Why = failed, false, "the latest attempt of "+a.What+" failed"+jobClassOf(j)
	default: // "running", or a status added later: not an outcome
		a.Result, a.Why = noAttempt, a.What+" has no outcome for its latest attempt"
	}
	return a
}

// probeAttempt is one prober run and its row.
func (f healthFacts) probeAttempt(r *state.ProbeRun, row, what string) attempt {
	a := attempt{What: what, MaxAge: f.probeStale(), NotBefore: f.Since, Why: what + " has never completed a run"}
	switch {
	case !f.Probes.Wired:
		a.Why = "the console's probe facts are not wired"
		return a
	case r == nil:
		a.Why = what + " is not supplied"
		return a
	case r.Unconfigured != "":
		a.Why = "not checked: " + r.Unconfigured
		return a
	}
	if r.LastRunAt != 0 {
		a.At, a.Result = millisTime(r.LastRunAt), passed
		if r.LastOKAt != 0 {
			a.LastPass = millisTime(r.LastOKAt)
		}
		if r.ConsecutiveFailures > 0 || r.LastOKAt != r.LastRunAt {
			a.Result, a.Why = failed, "the latest run of "+what+" failed"
			if r.LastErrorClass != "" {
				a.Why += " (" + r.LastErrorClass + ")"
			}
		}
	}
	return f.withRow(a, row)
}

func (f healthFacts) gatewayAttempts() []attempt {
	return []attempt{f.probeAttempt(f.Probes.Gateway, jobs.ProbeRowGateway, "the gateway probe")}
}

func (f healthFacts) storageAttempts() []attempt {
	return []attempt{f.probeAttempt(f.Probes.Storage, jobs.ProbeRowStorage, "the storage probe")}
}

// smtpSendEvidence is how long a delivered alert e-mail stands in for a probe that has no fresh run.
const smtpSendEvidence = 24 * time.Hour

func (f healthFacts) smtpAttempts() []attempt {
	sm := f.Probes.SMTP
	if sm == nil {
		return []attempt{{Why: "no alert e-mail probe is supplied"}}
	}
	a := f.probeAttempt(&sm.Probe, jobs.ProbeRowSMTP, "the alert e-mail probe")
	// A recent delivery is evidence where the probe has none, never over a probe that failed.
	if sent := millisTime(sm.LastSendOKAt); sm.LastSendOKAt != 0 && sm.Probe.Unconfigured == "" && a.Result != failed && (a.At.IsZero() || f.Now.Sub(a.At) > a.MaxAge) {
		a = attempt{Result: passed, At: sent, MaxAge: smtpSendEvidence, What: "the latest alert e-mail delivery"}
	}
	out := []attempt{a}
	bad := func(why string) { out = append(out, attempt{Result: failed, At: f.Now, Why: why}) }
	if sm.ConsecutiveSendFailures > 0 {
		bad("the latest alert e-mail send failed")
	}
	if sm.LastTestFailed {
		bad("the latest test e-mail failed")
	}
	if sm.PendingAlerts > 0 && (sm.LastSendOKAt == 0 || f.Now.Sub(millisTime(sm.LastSendOKAt)) > state.SMTPPendingWithoutSend) {
		bad("alert e-mail is waiting to go out and none has been delivered recently")
	}
	return out
}

// certRefusalClasses are the failure classes of a probe whose handshake got a certificate and
// refused it (probe.transportClass and verifyClass, and the self-probe's pin).
var certRefusalClasses = map[string]bool{"expired": true, "chain_expired": true, "untrusted": true, "hostname": true, "invalid": true, "pin_mismatch": true}

// certAttempt is what one probe learned about one listener's certificate. A refused certificate, or
// one inside C05's thresholds, is a failed attempt however the probe's other stages went: C01
// and C04 return before C05, so the row cannot wait for a banner to say so. With no
// handshake there is no certificate to judge.
func (f healthFacts) certAttempt(who string, at time.Time, maxAge time.Duration, notBefore time.Time, handshook bool, refusedClass string, notAfter time.Time) attempt {
	a := attempt{What: who + "'s certificate", At: at, MaxAge: maxAge, NotBefore: notBefore, Why: "no certificate was read from " + who}
	const day = 24 * time.Hour
	left := notAfter.Sub(f.Now)
	switch {
	case refusedClass == "expired" || (handshook && !notAfter.IsZero() && left <= 0):
		a.Result, a.Severe, a.Why = failed, true, who+" has an expired certificate"
	case refusedClass != "":
		a.Result, a.Severe, a.Why = failed, true, who+" has a certificate that failed verification ("+refusedClass+")"
	case !handshook || notAfter.IsZero():
	case left < state.CertWarnDays*day:
		a.Result, a.Severe, a.Why = failed, left < state.CertCritDays*day, who+"'s certificate expires in "+daysText(left)
	default:
		a.Result = passed
	}
	return a
}

// daysText words the time left as C05 does.
func daysText(left time.Duration) string {
	switch days := int(left / (24 * time.Hour)); days {
	case 0:
		return "less than a day"
	case 1:
		return "1 day"
	default:
		return strconv.Itoa(days) + " days"
	}
}

// certificateAttempts is the one place both listeners' certificates are judged: the health row and
// the POST check are this. Only the gateway is probed by the POST, so only its half needs a result from the request.
func (f healthFacts) certificateAttempts() []attempt {
	g := f.Probes.Gateway
	gateway := attempt{What: "the gateway's certificate", Why: "no certificate was read from the gateway"}
	if f.Probes.Wired && g != nil && g.Unconfigured == "" && g.LastRunAt != 0 {
		refused, class := "", g.LastErrorClass
		ok := g.LastOKAt == g.LastRunAt
		if !ok && certRefusalClasses[class] {
			refused = class
		}
		answered := ok || class == "unauthorized" || strings.HasPrefix(class, "status_") // a failure past the handshake still read the certificate
		var notAfter time.Time
		if g.CertNotAfter != 0 {
			notAfter = millisTime(g.CertNotAfter)
		}
		gateway = f.certAttempt("the gateway", millisTime(g.LastRunAt), f.probeStale(), f.Since, answered, refused, notAfter)
	}
	e := f.Endpoint
	agent := attempt{What: "the agent listener's certificate", Why: "no certificate was read from the agent listener"}
	if f.EndpointWired && e.Runs && e.Known {
		refused := ""
		switch {
		case !e.Last.OK && e.Last.Stage == probe.StageCertExpired:
			refused = "expired"
		case !e.Last.OK && e.Last.Stage == probe.StageTLS && !e.Last.NotAfter.IsZero():
			refused = e.Last.Class
		}
		answered := e.Last.OK || e.Last.Stage == probe.StageResponder
		agent = f.certAttempt("the agent listener", e.Last.At, f.endpointStale(), time.Time{}, answered, refused, e.Last.NotAfter)
	}
	return []attempt{gateway, agent}
}

func (f healthFacts) agentAPIAttempts() []attempt {
	st := f.Endpoint
	a := attempt{What: "the agent endpoint self-probe", MaxAge: f.endpointStale(), At: st.Last.At, LastPass: st.LastOK}
	switch {
	case !f.EndpointWired:
		a.At, a.Why = time.Time{}, "the agent endpoint self-probe is not wired"
	case !st.Runs:
		a.At, a.Why = time.Time{}, "the agent endpoint self-probe cannot run"
		if st.Limit != "" {
			a.Why += " (" + st.Limit + ")"
		}
	case !st.Known:
		a.At, a.Why = time.Time{}, "the agent endpoint self-probe has not completed a run yet"
	case st.Last.OK:
		a.Result = passed
	default:
		a.Result, a.Why = failed, "the latest agent endpoint self-probe failed"
		if st.Last.Class != "" {
			a.Why += " (" + st.Last.Class + ")"
		}
	}
	return []attempt{a}
}

// jobClassOf is " (class)" of a failed run, or nothing.
func jobClassOf(j store.JobState) string {
	if j.LastErrorClass == "" {
		return ""
	}
	return " (" + j.LastErrorClass + ")"
}

// heartbeatAttempt is a job whose good runs the engine reads as a heartbeat: the time of the newest
// good run (the facts), and how the newest run ended (its job_state row), which C06 and C07 do not read.
func heartbeatAttempt(what string, good *state.JobFacts, newest store.JobState, maxAge time.Duration) attempt {
	a := attempt{What: what, MaxAge: maxAge, Why: what + " has not finished a good run"}
	if good != nil && good.LastFinishedAt != 0 {
		a.Result, a.At, a.LastPass = passed, millisTime(good.LastFinishedAt), millisTime(good.LastFinishedAt)
	}
	if newest.LastFinishedAt != nil && (a.At.IsZero() || newest.LastFinishedAt.After(a.At)) && newest.LastStatus != "ok" {
		a.At = *newest.LastFinishedAt
	}
	switch newest.LastStatus {
	case "error":
		a.Result, a.Why = failed, "the latest run of "+what+" failed"+jobClassOf(newest)
	case "skipped":
		a.Result, a.Why = skipped, what+" skipped its latest run and has no recent good run"
	}
	return a
}

func (f healthFacts) evaluatorAttempts() []attempt {
	return []attempt{heartbeatAttempt("the alarm evaluator", f.Probes.Evaluator, f.EvaluatorLast, jobs.EvaluatorHealthyPeriods*health.EvaluatorPeriod)}
}

// workerAttempts: a worker that answers a ping can be failing every pass, or have no master key.
// The scheduler's row is what `cryo-console maintain` writes (maintain.Worker.recordJob).
func (f healthFacts) workerAttempts() []attempt {
	ping := attempt{What: "the worker", At: f.Now, Why: "the worker has not been probed"}
	switch {
	case f.WorkerPinged && !f.WorkerReachable:
		ping.Result, ping.Severe, ping.Why = failed, true, "the worker did not answer a ping"
	case f.WorkerReachable, f.MaintainKnown && f.MaintainReachable:
		ping.Result = passed
	case f.MaintainKnown:
		ping.Result, ping.Why = failed, "the latest worker probe did not reach it"
	}
	var good *state.JobFacts
	if m := f.Probes.Maintain; m != nil {
		good = m.Scheduler
	}
	out := []attempt{ping, heartbeatAttempt("the maintenance scheduler", good, f.SchedulerLast, state.MaintenanceSchedulerStale)}
	if f.WorkerPinged && f.WorkerReachable && !f.WorkerKeyLoaded {
		out = append(out, attempt{Result: failed, At: f.Now, Why: "the worker answers, but it has not loaded the master key"})
	}
	return out
}

func (f healthFacts) databaseAttempts() []attempt {
	read := attempt{Result: passed, At: f.Now, What: "the database"}
	if f.SchemaErr != nil {
		read.Result, read.Severe, read.Why = failed, true, "the database did not answer"
	}
	integrity := attempt{What: "the integrity check", MaxAge: state.IntegrityMaxAge, Why: "no integrity check has finished yet"}
	disk := attempt{What: "the disk measurement", MaxAge: state.DBMeasureMaxAge, Why: "free disk and WAL size have not been measured yet"}
	out := []attempt{read}
	if d := f.Probes.Database; d != nil {
		if d.IntegrityCheckedAt != 0 {
			integrity.At, integrity.Result = millisTime(d.IntegrityCheckedAt), passed
			if !d.IntegrityOK {
				integrity.Result, integrity.Severe, integrity.Why = failed, true, "the latest integrity check failed"
			}
		}
		if d.MeasuredAt != 0 {
			disk.At, disk.Result = millisTime(d.MeasuredAt), passed
		}
		if d.WorkerStoreFailures != 0 {
			out = append(out, attempt{Result: failed, At: f.Now, Why: "the maintenance worker could not read or write this database on its latest storage probes"})
		}
	}
	integrity = f.withRow(integrity, jobs.ProbeRowIntegrity)
	if integrity.Result == failed && f.ProbeRows[jobs.ProbeRowIntegrity].LastErrorClass == "integrity_failed" {
		integrity.Severe = true // damage, as opposed to a check that could not finish
	}
	return append(out, integrity, f.withRow(disk, jobs.ProbeRowDatabase))
}

func (f healthFacts) selfBackupAttempts() []attempt {
	a := attempt{What: "the database self-backup", MaxAge: state.DBSelfBackupMaxAge, Why: "the console does not supply the database self-backup's state"}
	var sb *state.SelfBackupFacts
	if d := f.Probes.Database; d != nil {
		sb = d.SelfBackup
	}
	switch {
	case sb == nil:
	case sb.NotInBuild:
		a.Outside, a.Why = statusNotInBuild, "this build does not run db-self-backup, so the console database's own backup is not judged"
	case sb.VerifyFailed:
		a.Result, a.Severe, a.At, a.Why = failed, true, millisTime(sb.LastAt), "the newest tested restore of the self-backup failed"
	case sb.LastAt == 0:
		a.Why = "no self-backup has been written"
	default:
		a.Result, a.At = passed, millisTime(sb.LastAt)
	}
	return []attempt{a}
}

func (f healthFacts) masterKeyAttempts() []attempt {
	a := attempt{What: "the master key", At: f.Now, Why: "the master key's generation has not been compared with the escrows recently"}
	read := f.ProbeRows[jobs.ProbeRowMasterKey]
	switch m := f.Probes.MasterKey; {
	case !f.MasterKeyWired:
		a.Why = "the master key state is not reported"
	case !f.MasterKeyLoaded || (m != nil && !m.Loaded):
		a.Result, a.Severe, a.Why = failed, true, "the master key is not loaded"
	case m == nil && read.LastStatus == "error": // the prober forgets the comparison when the escrow read fails: its row says so
		a.Result, a.Why = failed, "the latest read of the escrowed keys failed"+jobClassOf(read)
	case m == nil:
	case slices.ContainsFunc(m.EscrowKeyIDs, func(id string) bool { return id != m.KeyID }):
		a.Result, a.Severe, a.Why = failed, true, "an escrowed repository key is wrapped under another master key"
	default:
		a.Result = passed
	}
	return []attempt{a}
}

// jobsAttempts is one attempt per job registered in this process. A job past its schedule is a
// failed attempt: it ran, then stopped, so it is known late rather than unknown. A job with no
// finished run has no attempt while its first run is in progress or inside jobsFirstRunGrace of this
// console's start; past that (or at once when the start is unknown), never having run is a failure.
// The ping's skip while no evaluation has finished is by design, and its own row: it counts as
// nothing until the job is overdue, and the ping fails on its next run if the evaluator never runs.
func (f healthFacts) jobsAttempts() []attempt {
	var out []attempt
	for _, j := range f.Jobs {
		if j.LastStatus == "" {
			continue // NeverRun and FirstRunning below
		}
		a := attempt{What: "job " + j.Name, Why: "the status of job " + j.Name + " is not one this console knows"}
		if j.LastFinishedAt != nil {
			a.At = *j.LastFinishedAt
		}
		switch {
		case j.LastStatus == "error":
			a.Result, a.Why = failed, "the latest run of job "+j.Name+" failed"+jobClassOf(j)
		case jobOverdue(j, f.Now):
			a.Result, a.Why = failed, "job "+j.Name+" is overdue: it last finished "+strconv.Itoa(int(f.Now.Sub(*j.LastFinishedAt)/time.Minute))+" min ago"
		case j.LastStatus == "ok", j.LastStatus == "skipped" && j.LastErrorClass == jobs.ClassAwaitingFirstEvaluation:
			a.Result = passed
		case j.LastStatus == "skipped":
			a.Result, a.Why = skipped, "job "+j.Name+" skipped its latest run and has no pass on record"
		}
		out = append(out, a)
	}
	young := f.Probes.StartedAt != 0 && f.Now.Sub(millisTime(f.Probes.StartedAt)) < jobsFirstRunGrace
	for _, n := range f.NeverRun {
		a := attempt{What: "job " + n, Why: "job " + n + " has not finished a first run yet"}
		if !young {
			a.Result, a.At, a.Why = failed, f.Now, "job "+n+" has never run"
		}
		out = append(out, a)
	}
	for _, n := range f.FirstRunning {
		out = append(out, attempt{What: "job " + n, Why: "job " + n + " has not finished a first run yet"})
	}
	if len(out) == 0 {
		out = append(out, attempt{Why: "no job has run yet"})
	}
	return out
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

func (f healthFacts) fleetDataAttempts() []attempt {
	a := attempt{What: "the facts load", At: f.Now}
	switch {
	case !f.Complete:
		a.Result, a.Why = failed, "the facts load was incomplete"
	case f.FirstRun:
		a.Outside, a.Why = statusNotApplicable, noDevicesYet
	default:
		a.Result = passed
	}
	return []attempt{a}
}

// clockAttempts says what C13 judged from, in the scope of this request: the same World and the
// same state.ReportingDevices. For a scoped account that is its own customers' devices, so the
// row says so, and no number from outside them reaches it.
func (f healthFacts) clockAttempts() []attempt {
	n, least := strconv.Itoa(f.ReportingDevices), strconv.Itoa(state.ClockSuspectMinDevices)
	a := attempt{What: "the clock comparison", At: f.Now, Result: passed, Note: "compared with " + n + " reporting devices"}
	enough := f.ReportingDevices >= state.ClockSuspectMinDevices
	switch {
	case !enough && f.Scoped:
		a.Outside, a.Why = statusNotApplicable, "needs at least "+least+" devices reporting among those this account can see; "+n+" report here"
	case !enough:
		a.Outside, a.Why = statusNotApplicable, "needs at least "+least+" devices reporting to compare clocks; "+n+" report now"
	case f.Scoped:
		a.Note = "compared with the " + n + " reporting devices this account can see"
	}
	return []attempt{a}
}

func (f healthFacts) releaseAttempts() []attempt {
	a := attempt{What: "release-check", MaxAge: state.ReleaseCheckMaxAge, Why: "release-check has never run"}
	switch r := f.Probes.Release; {
	case r == nil:
		a.Why = "the console does not supply the release check"
	case r.NotInBuild:
		a.Outside, a.Why = statusNotInBuild, "this build does not run release-check, so no release manifest is checked"
	case r.CheckedAt == 0:
	case r.Rejected:
		a.Result, a.At, a.Why = failed, millisTime(r.CheckedAt), "a release manifest was rejected"
	default:
		a.Result, a.At = passed, millisTime(r.CheckedAt)
	}
	return []attempt{a}
}

func (f healthFacts) quotaAttempts() []attempt {
	a := attempt{What: "the usage of the storage targets with a quota", MaxAge: state.QuotaMeasureMaxAge}
	q := f.Probes.Quota
	switch {
	case q == nil:
		a.Why = "the console does not supply storage usage"
		return []attempt{a}
	case q.NotInBuild:
		a.Outside, a.Why = statusNotInBuild, "this build does not run the provider-side usage listing, so usage against a storage quota is not measured"
		return []attempt{a}
	case q.WithQuota == 0:
		a.Outside, a.Why = statusNotApplicable, "no storage target has a soft quota set"
		return []attempt{a}
	}
	seen := 0
	for _, t := range q.Targets {
		if t.SoftQuotaBytes <= 0 {
			continue
		}
		seen++
		if t.MeasuredAt == 0 {
			a.At, a.Why = time.Time{}, "the usage of a storage target with a quota has never been measured"
			return []attempt{a}
		}
		if m := millisTime(t.MeasuredAt); a.At.IsZero() || m.Before(a.At) {
			a.At = m
		}
	}
	if seen < q.WithQuota {
		a.At, a.Why = time.Time{}, "the usage of a storage target with a quota is missing"
		return []attempt{a}
	}
	a.Result = passed
	return []attempt{a}
}

// digestStale is how old the newest digest may be: two daily sends and an hour. C14 names a
// missed send sooner, from the configured time; this row holds without it.
const digestStale = 49 * time.Hour

func (f healthFacts) digestAttempts() []attempt {
	a := attempt{What: "the daily digest", MaxAge: digestStale, Why: "no daily digest has been sent yet"}
	switch d := f.Probes.Digest; {
	case d == nil:
		a.Why = "the console does not supply the daily digest's state"
	case d.NotInBuild:
		a.Outside, a.Why = statusNotInBuild, "this build does not send the daily digest"
	case d.Disabled:
		a.Outside, a.Why = statusNotApplicable, "the daily digest is turned off"
	case d.LastSentAt != 0:
		a.Result, a.At = passed, millisTime(d.LastSentAt)
	}
	return []attempt{a}
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

// buildComponents is the rows of the system health page: what each row knows, judged by the one rule.
func buildComponents(f healthFacts) []SystemComponentJSON {
	_, c00 := worstOf(f.Banners, []string{bannerNotWired})
	out := make([]SystemComponentJSON, 0, len(healthRowOrder))
	for _, row := range healthRowOrder {
		c := SystemComponentJSON{ID: row.id, Label: row.label, Banners: []string{}}
		raised, worst := worstOf(f.Banners, healthRowBanners[row.id])
		if raised != nil {
			c.Banners = raised
		}
		c.Status, c.Detail, c.CheckedAt = judgeRow(f.Now, rowAttempts[row.id](f), worst, c00 != "")
		out = append(out, c)
	}
	return out
}

// healthFactsOf gathers what the rows read from one evaluation. The health page and the POST probe both use it.
func (s *Service) healthFactsOf(p *auth.Principal, f health.Fleet, probes state.ProbeFacts) healthFacts {
	facts := healthFacts{Banners: f.View.Banners, Now: f.Computed, Complete: f.View.Complete, EndpointWired: s.Fleet.Endpoint != nil,
		MasterKeyWired: s.Fleet.MasterLoaded != nil, Probes: probes, ReportingDevices: f.Reporting, Scoped: !p.Scope.All, FirstRun: f.View.FirstRun,
		ProbeRows: map[string]store.JobState{}}
	facts.MasterKeyLoaded = facts.MasterKeyWired && s.Fleet.MasterLoaded()
	if s.Fleet.Endpoint != nil {
		facts.Endpoint = s.Fleet.Endpoint()
	}
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
		if !store.IsJobRow(j.Name) {
			facts.ProbeRows[j.Name] = j
		}
	}
	facts.NeverRun, facts.FirstRunning = unfinishedJobs(names, facts.Jobs, f.Computed)
	facts.EvaluatorLast, _ = f.JobStatus(health.EvaluatorJob)
	facts.SchedulerLast, _ = f.JobStatus(health.SchedulerJob)
	return facts
}

func (s *Service) systemHealth(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	f, err := s.Fleet.Fleet(ctx, p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	facts := s.healthFactsOf(p, f, s.Fleet.ProbeFacts(f.Jobs, nil, true))
	var worker *Worker
	if s.Backend != nil {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		pr, perr := s.Backend.Ping(pctx)
		cancel()
		facts.WorkerPinged = true
		if perr == nil {
			facts.WorkerReachable, facts.WorkerKeyLoaded = true, pr.MasterKeyLoaded
			worker = &Worker{Reachable: true, Version: pr.Version, MasterKeyLoaded: pr.MasterKeyLoaded, ResticVersion: toolUnknown, RcloneVersion: toolUnknown}
		} else {
			worker = &Worker{}
		}
	}
	if s.Fleet.Maintain != nil {
		m := s.Fleet.Maintain()
		facts.MaintainKnown, facts.MaintainReachable = m.Known, m.Reachable
	}
	schema, serr := s.Store.SchemaVersion(ctx)
	facts.SchemaErr = serr
	out := SystemHealthResponse{StampJSON: StampJSON{ComputedAt: f.Computed, LoadedAt: f.View.LoadedAt, Complete: f.View.Complete}, Banners: bannersJSON(f.View.Banners),
		Components: buildComponents(facts), FirstRun: f.View.FirstRun}
	out.Overall = overallOf(out.Components)
	out.Versions = VersionsJSON{Console: buildinfo.Version, Commit: buildinfo.Commit, BuildDate: buildinfo.Date, Schema: schema, Maintain: worker}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// labFacts are a small healthy console's: every check in this build passing, three devices reporting.
func labFacts(now time.Time) healthFacts {
	at := func(d time.Duration) state.Millis { return state.Millis(now.Add(-d).UnixMilli()) }
	ok := func() state.ProbeRun {
		return state.ProbeRun{LastRunAt: at(20 * time.Second), LastOKAt: at(20 * time.Second)}
	}
	gw, st := ok(), ok()
	gw.CertNotAfter = state.Millis(now.Add(365 * 24 * time.Hour).UnixMilli())
	good := &state.JobFacts{LastFinishedAt: at(time.Minute)}
	ran := now.Add(-time.Minute)
	return healthFacts{Now: now, Complete: true, EndpointWired: true, WorkerPinged: true, WorkerReachable: true, WorkerKeyLoaded: true, MaintainKnown: true, MaintainReachable: true,
		EvaluatorLast: store.JobState{LastStatus: "ok"}, SchedulerLast: store.JobState{LastStatus: "ok"}, MasterKeyWired: true, MasterKeyLoaded: true, ReportingDevices: 3,
		Jobs: []store.JobState{{Name: "lab", LastStatus: "ok", LastFinishedAt: &ran}},
		Probes: state.ProbeFacts{Wired: true, Gateway: &gw, Storage: &st, SMTP: &state.SMTPFacts{Probe: ok()}, Evaluator: good,
			Maintain: &state.MaintainFacts{RPC: ok(), Scheduler: good}, MasterKey: &state.MasterKeyFacts{Loaded: true, KeyID: "lab"},
			Database: &state.DatabaseFacts{IntegrityCheckedAt: at(time.Hour), IntegrityOK: true, MeasuredAt: at(20 * time.Second), SelfBackup: &state.SelfBackupFacts{NotInBuild: true}},
			Digest:   &state.DigestFacts{NotInBuild: true}, Quota: &state.QuotaFacts{NotInBuild: true}, Release: &state.ReleaseFacts{NotInBuild: true}},
		Endpoint: probe.Status{Runs: true, Known: true, Interval: time.Minute, Last: probe.Result{At: now.Add(-20 * time.Second), OK: true, NotAfter: now.Add(365 * 24 * time.Hour)}},
	}
}

// LabHealthy is overall and the rows of a small healthy console, from the same rules as a real
// answer: every check in this build passing, three devices reporting. Only fixtures mode's
// lab view shows it (/fixtures?health=ok): the fictional fleet raises real conditions and
// has more than five devices, so these states can be seen no other way.
func LabHealthy(now time.Time) (overall string, rows []SystemComponentJSON) {
	rows = buildComponents(labFacts(now))
	return overallOf(rows), rows
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

// jobMaxAge is how long after its last finish a job may go without another run: its period plus
// what the runner can add. The row holds the real schedule, because RunOnce records the next due time
// as the finish plus the period; the runner then fires up to jobs.MaxJitter of that period after it.
// 0 is a job that cannot be late: one running now, or one with no schedule on its row.
func jobMaxAge(j store.JobState, now time.Time) time.Duration {
	if j.NextDueAt == nil || j.LastFinishedAt == nil || jobRunning(j, now) {
		return 0
	}
	every := j.NextDueAt.Sub(*j.LastFinishedAt)
	return every + jobs.MaxJitter(every) + overdueMargin
}

// jobOverdue: a job later than the runner can make it, for the job table. The jobs row reads the same limit.
func jobOverdue(j store.JobState, now time.Time) bool {
	m := jobMaxAge(j, now)
	return m > 0 && now.Sub(*j.LastFinishedAt) > m
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

// probeChecks is the gateway, storage and certificates checks of a probe that began at f.Since:
// the same attempts and the same rule as the health page's rows, so the two cannot disagree. f.Since
// is the one difference: a result the probe did not produce during this request is not_checked, because it is
// not evidence of now. The storage check also speaks for the quota, which has its own row on the health page.
func probeChecks(f healthFacts) []ProbeCheckJSON {
	_, c00 := worstOf(f.Banners, []string{bannerNotWired})
	out := make([]ProbeCheckJSON, 0, 3)
	for _, id := range []string{"gateway", "storage", "certificates"} {
		rowIDs := healthRowBanners[id]
		if id == "storage" {
			rowIDs = slices.Concat(rowIDs, healthRowBanners["storage_quota"])
		}
		raised, worst := worstOf(f.Banners, rowIDs)
		if raised == nil {
			raised = []string{}
		}
		status, detail, _ := judgeRow(f.Now, rowAttempts[id](f), worst, c00 != "")
		out = append(out, ProbeCheckJSON{ID: id, Status: status, Detail: detail, Banners: raised, Scope: probeScope})
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
			probes := s.Fleet.ProbeFacts(nil, nil, true)
			f, ferr := s.Fleet.Fleet(ctx, p.Scope)
			if ferr != nil {
				s.fail(w, r, ferr)
				return
			}
			facts := s.healthFactsOf(p, f, probes)
			facts.Since = started.Truncate(time.Millisecond) // the facts hold milliseconds
			out.Checks = probeChecks(facts)
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
