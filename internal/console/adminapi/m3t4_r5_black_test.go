package adminapi

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 fix round 5, through the real GET /api/v1/system/health and /system/jobs.

// F2: the disk probe's newest attempt errored, and only its job_state row says so.
func TestR5ADiskProbeThatErrorsReachesTheDatabaseRow(t *testing.T) { // r5 F2, C11
	e := newR3Env(t)
	e.jobRun("health-prober.database", "error", "disk_unmeasured", 5*time.Second)
	c := e.row("database")
	if c.Status != statusWarn || !strings.Contains(c.Detail, "disk") || !strings.Contains(c.Detail, "disk_unmeasured") {
		t.Errorf("database = %+v, want warn saying the disk measurement failed", c)
	}
}

// F2: an older failed row does not outlast a newer good result.
func TestR5AnOlderFailedAttemptRowDoesNotOutliveANewerPass(t *testing.T) { // r5 F2
	e := newR3Env(t)
	e.jobRun("health-prober.database", "error", "disk_unmeasured", 5*time.Minute)
	e.jobRun("health-prober.integrity", "error", "timeout", 2*time.Hour)
	if c := e.row("database"); c.Status != statusOK {
		t.Errorf("database = %+v: a newer measurement and check passed", c)
	}
}

// F5: a failed, stale probe plus a recent send. A send is evidence only when the newest attempt did not fail.
func TestR5ARecentSendDoesNotExcuseAFailedProbe(t *testing.T) { // r5 F5, C03
	e := newR3Env(t)
	now := e.clk.Now()
	p := *e.probes.Load()
	p.SMTP = &state.SMTPFacts{
		Probe:        state.ProbeRun{LastRunAt: state.Millis(now.Add(-time.Hour).UnixMilli()), ConsecutiveFailures: 1, LastErrorClass: "refused"},
		LastSendOKAt: state.Millis(now.Add(-time.Minute).UnixMilli()),
	}
	e.probes.Store(&p)
	if c := e.row("smtp"); c.Status != statusWarn {
		t.Errorf("smtp = %+v, want warn: the newest attempt failed", c)
	}
}

// A worker that answers its ping and reports no master key.
func TestR5AWorkerWithNoMasterKeyWarnsOnItsRow(t *testing.T) { // r5
	e := newR3Env(t)
	if c := e.row("maintenance_worker"); c.Status != statusOK {
		t.Fatalf("control: %+v", c)
	}
	e.be.PingStatus = &adminpath.PingResult{Version: "t", MasterKeyLoaded: false}
	c := e.row("maintenance_worker")
	if c.Status != statusWarn || !strings.Contains(c.Detail, "master key") {
		t.Errorf("maintenance_worker = %+v, want warn saying the worker has no master key", c)
	}
}

// F7: the maintenance scheduler is unregistered here and alive in another process: the job
// table still says when it is overdue, and the jobs row does not judge it.
func TestR5TheSchedulerCanBeOverdueInTheTable(t *testing.T) { // r5 F7
	e := newR3Env(t)
	e.svc.Jobs = staticJobs{"alarm-evaluator"}
	last := e.clk.Now().Add(-time.Hour)
	next := last.Add(maintain.DefaultTickEvery)
	e.recordSchedulerWithDue(last, next)
	jl := decode[JobListResponse](t, mustGet(e.env, "/api/v1/system/jobs", e.sess))
	seen := false
	for _, j := range jl.Jobs {
		if j.Name == maintain.SchedulerJob {
			seen = true
			if j.Registered || !j.Overdue {
				t.Errorf("the scheduler in the table: %+v, want unregistered and overdue", j)
			}
		}
	}
	if !seen {
		t.Error("the scheduler is not listed")
	}
	if c := e.row("jobs"); c.Status != statusOK {
		t.Errorf("the jobs row judges the scheduler: %+v", c)
	}
}
