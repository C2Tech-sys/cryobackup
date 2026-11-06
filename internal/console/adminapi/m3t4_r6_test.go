package adminapi

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 final round, through the real GET /api/v1/system/health.

// r6Job records a registered job's newest run.
func (e *r3Env) r6Job(name, status, class string, ago, nextIn time.Duration) {
	e.t.Helper()
	at := e.clk.Now().Add(-ago)
	next := at.Add(nextIn)
	if err := e.st.RecordJobRun(context.Background(), name, store.JobRun{Status: status, ErrorClass: class, StartedAt: at, FinishedAt: at, NextDueAt: &next}); err != nil {
		e.t.Fatal(err)
	}
}

// M1: a failed attempt followed by a skip. The prober's facts still say it failed.
func TestR6ASkipNeverClearsAFailure(t *testing.T) { // r6 M1, C02
	e := newR3Env(t)
	now := e.clk.Now()
	p := *e.probes.Load()
	p.Storage = &state.ProbeRun{LastRunAt: state.Millis(now.Add(-time.Minute).UnixMilli()), LastOKAt: state.Millis(now.Add(-2 * time.Minute).UnixMilli()), ConsecutiveFailures: 1, LastErrorClass: "repo_missing"}
	e.probes.Store(&p)
	e.jobRun("health-prober.storage", "skipped", "busy", 5*time.Second)
	c := e.row("storage")
	if c.Status != statusWarn || !strings.Contains(c.Detail, "repo_missing") {
		t.Errorf("failed, then skipped: storage = %+v, want warn naming the failure", c)
	}
}

// M1: a row status that is no outcome is no result, however new.
func TestR6AnUnknownRowStatusIsNotChecked(t *testing.T) { // r6 M1
	e := newR3Env(t)
	e.jobRun("health-prober.storage", "running", "", 5*time.Second)
	if c := e.row("storage"); c.Status != statusNotChecked || c.Detail == "" {
		t.Errorf("an unknown status on the attempt's row: storage = %+v", c)
	}
}

// M2: a job that ran and stopped is known late, and the row names every job the table flags.
func TestR6AnOverdueJobWarnsAndTheRowNamesEveryFlaggedJob(t *testing.T) { // r6 M2
	e := newR3Env(t)
	e.svc.Jobs = staticJobs{"alarm-evaluator", "stopped-job", "broken-job", "never-job"}
	e.r6Job("stopped-job", "ok", "", time.Hour, 5*time.Minute)
	if c := e.row("jobs"); c.Status != statusWarn || !strings.Contains(c.Detail, "stopped-job") || !strings.Contains(c.Detail, "overdue") {
		t.Errorf("stopped an hour ago: jobs = %+v, want warn naming it", c)
	}
	e.r6Job("broken-job", "error", "store", time.Minute, 5*time.Minute)
	c := e.row("jobs")
	for _, name := range []string{"stopped-job", "broken-job", "never-job"} {
		if !strings.Contains(c.Detail, name) {
			t.Errorf("the jobs row does not name %s: %+v", name, c)
		}
	}
	if c.Status != statusWarn {
		t.Errorf("jobs = %+v", c)
	}
}

// M2: the engine calls a time ahead of now garbled: it proves nothing, here too.
func TestR6AJobFinishedInTheFutureDoesNotReadOK(t *testing.T) { // r6 M2
	e := newR3Env(t)
	e.svc.Jobs = staticJobs{"alarm-evaluator", "clock-confused"}
	e.r6Job("clock-confused", "ok", "", -time.Hour, 5*time.Minute)
	if c := e.row("jobs"); c.Status == statusOK {
		t.Errorf("a job that finishes an hour from now: jobs = %+v", c)
	}
	// Inside the skew the engine allows it is still evidence.
	e.r6Job("clock-confused", "ok", "", -time.Minute, 5*time.Minute)
	if c := e.row("jobs"); c.Status != statusOK {
		t.Errorf("a minute of clock skew: jobs = %+v", c)
	}
}

// L1: the ping's first skip is by design and holds nothing; any other skip, and an overdue one, still counts.
func TestR6ThePingsFirstSkipDoesNotCountAgainstTheRow(t *testing.T) { // r6 L1
	e := newR3Env(t)
	e.svc.Jobs = staticJobs{"alarm-evaluator", "healthcheck-ping"}
	e.r6Job("healthcheck-ping", "skipped", "awaiting_first_evaluation", 5*time.Second, time.Hour)
	if c := e.row("jobs"); c.Status != statusOK {
		t.Errorf("the first skip of a ping on a 1 h interval: jobs = %+v", c)
	}
	e.r6Job("healthcheck-ping", "skipped", "awaiting_first_evaluation", 2*time.Hour, time.Hour)
	if c := e.row("jobs"); c.Status != statusWarn {
		t.Errorf("that skip, and the job has not run again since: jobs = %+v", c)
	}
	e.r6Job("healthcheck-ping", "skipped", "", 5*time.Second, time.Hour)
	if c := e.row("jobs"); c.Status != statusNotChecked {
		t.Errorf("a skip with no stated reason: jobs = %+v", c)
	}
}

// L4: a row with no attempt has nothing that was checked at any time, and one outside the build neither.
func TestR6ARowWithNoAttemptHasNoCheckedAt(t *testing.T) { // r6 L4
	e := newR3Env(t)
	for _, id := range []string{"release_manifest", "storage_quota", "digest", "self_backup"} {
		if c := e.row(id); c.CheckedAt != nil {
			t.Errorf("%s (not in this build) has a checked_at: %+v", id, c)
		}
	}
	p := *e.probes.Load()
	p.MasterKey = nil
	e.probes.Store(&p)
	e.be.PingStatus = nil
	f := r5Passing()
	f.MasterKeyWired = false
	f.WorkerPinged, f.WorkerReachable, f.MaintainKnown, f.MaintainReachable = false, false, false, false
	f.Probes.Maintain = &state.MaintainFacts{}
	rows := rowsOf(f)
	if c := rows["master_key"]; c.Status != statusNotChecked || c.CheckedAt != nil {
		t.Errorf("master_key with no attempt: %+v", c)
	}
	if c := rows["maintenance_worker"]; c.Status != statusNotChecked || c.CheckedAt != nil {
		t.Errorf("maintenance_worker with no attempt: %+v", c)
	}
	// A row with nothing to judge has nothing checked either, though its facts were read just now.
	f = r5Passing()
	f.ReportingDevices, f.FirstRun = 3, true
	rows = rowsOf(f)
	for _, id := range []string{"clock", "fleet_data"} {
		if c := rows[id]; c.Status != statusNotApplicable || c.CheckedAt != nil {
			t.Errorf("%s, not applicable: %+v", id, c)
		}
	}
}

// L7: the contract says how POST differs from S36, and does not call it one difference.
func TestR6OpenAPIListsHowPostDiffersFromS36(t *testing.T) { // r6 L7
	b, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	if strings.Contains(doc, "the one difference") {
		t.Error("openapi still says there is one difference between POST and S36")
	}
	i := strings.Index(doc, "operationId: probeStorageTarget")
	if i < 0 {
		i = strings.Index(doc, "Runs the health-prober's gateway probe")
	}
	text := doc[i:]
	text = text[:strings.Index(text, "x-min-role")]
	for _, want := range []string{"C15", "agent listener", "not re-probed"} {
		if !strings.Contains(text, want) {
			t.Errorf("the POST description does not mention %q", want)
		}
	}
}
