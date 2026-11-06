package adminapi

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 fix round 3, through the real GET /api/v1/system/health: a wired console
// whose probes all pass, as the health-prober reports them.

type r3Env struct {
	*env
	sess   *session
	probes atomic.Pointer[state.ProbeFacts]
	ep     atomic.Pointer[probe.Status]
}

// newR3Env is a console with healthy probe facts and a passing endpoint self-probe.
func newR3Env(t *testing.T) *r3Env {
	t.Helper()
	e := &r3Env{env: newEnv(t)}
	now := e.clk.Now()
	p := r3Healthy(now)
	e.probes.Store(&p)
	st := probe.Status{Runs: true, Known: true, Interval: time.Minute, Started: now.Add(-time.Hour), LastOK: now.Add(-20 * time.Second),
		Last: probe.Result{At: now.Add(-20 * time.Second), OK: true, NotAfter: now.Add(90 * 24 * time.Hour)}}
	e.ep.Store(&st)
	e.svc.Fleet.Probes = func() state.ProbeFacts { return *e.probes.Load() }
	e.svc.Fleet.Endpoint = func() probe.Status { return *e.ep.Load() }
	e.sess = e.login("admin", pw)
	// The two heartbeats real jobs write: a good evaluation and a passing scheduler pass.
	e.jobRun(health.EvaluatorJob, "ok", "", 40*time.Second)
	e.jobRun(health.EvaluatorOKJob, "ok", "", 40*time.Second)
	e.jobRun(maintain.SchedulerJob, "ok", "", time.Minute)
	return e
}

func r3Healthy(now time.Time) state.ProbeFacts {
	at := func(d time.Duration) state.Millis { return state.Millis(now.Add(-d).UnixMilli()) }
	ok := func() state.ProbeRun {
		return state.ProbeRun{LastRunAt: at(20 * time.Second), LastOKAt: at(20 * time.Second)}
	}
	gw, st := ok(), ok()
	gw.CertNotAfter = state.Millis(now.Add(90 * 24 * time.Hour).UnixMilli())
	return state.ProbeFacts{Wired: true, StartedAt: at(time.Hour), ProberInterval: state.DefaultProberInterval, Gateway: &gw, Storage: &st,
		SMTP: &state.SMTPFacts{Probe: ok()}, Maintain: &state.MaintainFacts{RPC: ok()},
		Database:  &state.DatabaseFacts{IntegrityCheckedAt: at(time.Hour), IntegrityOK: true, MeasuredAt: at(20 * time.Second), FreeBytes: 80 << 30, TotalBytes: 100 << 30},
		MasterKey: &state.MasterKeyFacts{Loaded: true, KeyID: "k"}}
}

func (e *r3Env) jobRun(name, status, class string, ago time.Duration) {
	e.t.Helper()
	at := e.clk.Now().Add(-ago)
	if err := e.st.RecordJobRun(context.Background(), name, store.JobRun{Status: status, ErrorClass: class, StartedAt: at, FinishedAt: at}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *r3Env) health() SystemHealthResponse {
	e.t.Helper()
	return decode[SystemHealthResponse](e.t, mustGet(e.env, "/api/v1/system/health", e.sess))
}

func (e *r3Env) row(id string) SystemComponentJSON {
	e.t.Helper()
	for _, c := range e.health().Components {
		if c.ID == id {
			return c
		}
	}
	e.t.Fatalf("no component %s", id)
	return SystemComponentJSON{}
}

// H1: the row reads how the newest run ended, not that one ended.
func TestR3EvaluatorRowOKOnlyWhenTheNewestRunPassed(t *testing.T) { // M3-T4 r3 H1, C06
	e := newR3Env(t)
	if c := e.row("evaluator"); c.Status != "ok" || c.CheckedAt == nil {
		t.Fatalf("control: a good evaluation 40 s ago = %+v", c)
	}
	// The reviewer's reproduction: alarm-evaluator recorded error 20 s ago. C06 reads the last good run, which
	// is still fresh, so no banner speaks yet: the row itself must.
	e.jobRun(health.EvaluatorJob, "error", "store", 20*time.Second)
	c := e.row("evaluator")
	if c.Status != "warn" || !strings.Contains(c.Detail, "failed") || !strings.Contains(c.Detail, "store") {
		t.Errorf("evaluator after a failed run = %+v, want warn naming the failure", c)
	}
	if len(c.Banners) != 0 {
		t.Errorf("the control is meant to be before C06: %+v", c.Banners)
	}
	// A run that ran is not a run that passed: the job's row says ok, and the marker of a good evaluation does not exist.
	e2 := newR3Env(t)
	if err := e2.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM job_state WHERE name = ?`, health.EvaluatorOKJob)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c := e2.row("evaluator"); c.Status == "ok" {
		t.Errorf("no good evaluation was ever recorded, yet evaluator = %+v", c)
	}
}

// The H1 audit: a worker that answers its ping is not a scheduler that passed.
func TestR3MaintenanceRowOKOnlyWhenTheSchedulerPassed(t *testing.T) { // M3-T4 r3 H1 audit, C07
	e := newR3Env(t)
	if c := e.row("maintenance_worker"); c.Status != "ok" {
		t.Fatalf("control: worker answers, scheduler passed = %+v", c)
	}
	e.jobRun(maintain.SchedulerJob, "error", "internal", time.Minute)
	if c := e.row("maintenance_worker"); c.Status != "warn" || !strings.Contains(c.Detail, "scheduler") {
		t.Errorf("the worker answers but its scheduler's last pass failed: %+v", c)
	}
}

// M1: what this build does not run is said on its row, and is not "no evidence".
func TestR3NotInBuildChecksSayTheirNameOnTheirRow(t *testing.T) { // M3-T4 r3 M1, C14 C15 C16 C11
	e := newR3Env(t)
	rows := map[string]SystemComponentJSON{}
	for _, c := range e.health().Components {
		rows[c.ID] = c
	}
	for _, id := range []string{"release_manifest", "storage_quota", "digest", "self_backup"} {
		c, ok := rows[id]
		if !ok || c.Status != "not_in_build" || !strings.Contains(c.Detail, "build") {
			t.Errorf("%s = %+v (present %v), want not_in_build saying so", id, c, ok)
		}
	}
}

// M1: with fewer than five reporting devices C13 cannot run: said on the row, not blocking.
func TestR3ClockRowSaysItNeedsFiveDevices(t *testing.T) { // M3-T4 r3 M1, C13
	e := newR3Env(t)
	if c := e.row("clock"); c.Status != "ok" {
		t.Fatalf("control: the fixture fleet reports enough devices: %+v", c)
	}
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE device_presence SET received_at = ?`, e.clk.Now().Add(-30*24*time.Hour).UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c := e.row("clock")
	if c.Status != "not_applicable" || !strings.Contains(c.Detail, "needs at least 5 devices") {
		t.Errorf("clock = %+v, want not_applicable saying it needs at least 5 devices", c)
	}
}

// LOW: the self-probe's staleness is judged against its own interval on both rows that read it.
func TestR3CertificatesRowUsesTheEndpointsOwnInterval(t *testing.T) { // M3-T4 r3 low, C05
	e := newR3Env(t)
	now := e.clk.Now()
	st := probe.Status{Runs: true, Known: true, Interval: 600 * time.Second, Started: now.Add(-time.Hour), LastOK: now.Add(-10 * time.Minute),
		Last: probe.Result{At: now.Add(-10 * time.Minute), OK: true, NotAfter: now.Add(90 * 24 * time.Hour)}}
	e.ep.Store(&st)
	if a := e.row("agent_api"); a.Status != "ok" {
		t.Fatalf("agent_api at agent_probe.interval_s=600 = %+v", a)
	}
	if c := e.row("certificates"); c.Status != "ok" {
		t.Errorf("certificates = %+v while agent_api, reading the same probe, is ok", c)
	}
	// Past three of its own intervals both go together.
	st2 := st
	st2.Last.At = now.Add(-31*time.Minute - probe.TotalTimeout)
	e.ep.Store(&st2)
	if a, c := e.row("agent_api"), e.row("certificates"); a.Status != "not_checked" || c.Status != "not_checked" {
		t.Errorf("a stale probe: agent_api %s, certificates %s", a.Status, c.Status)
	}
}

// LOW: a registered job that never ran counts against the row, after the start-up grace.
func TestR3JobsRowCountsARegisteredJobThatNeverRan(t *testing.T) { // M3-T4 r3 low
	e := newR3Env(t)
	e.svc.Jobs = staticJobs{"alarm-evaluator", "maintenance-scheduler"}
	if c := e.row("jobs"); c.Status != "ok" {
		t.Fatalf("control: every registered job has run: %+v", c)
	}
	e.svc.Jobs = staticJobs{"alarm-evaluator", "never-ran"}
	if c := e.row("jobs"); c.Status != "warn" || !strings.Contains(c.Detail, "never run") {
		t.Errorf("an hour after start, a job that never ran: %+v", c)
	}
	// Inside the grace: not healthy yet, and not an alarm either.
	p := *e.probes.Load()
	p.StartedAt = state.Millis(e.clk.Now().Add(-time.Minute).UnixMilli())
	e.probes.Store(&p)
	if c := e.row("jobs"); c.Status != "not_checked" || !strings.Contains(c.Detail, "first run") {
		t.Errorf("a minute after start, a job that has not run yet: %+v", c)
	}
}

type staticJobs []string

func (j staticJobs) Names() []string                               { return j }
func (j staticJobs) RunOnce(context.Context, string) (bool, error) { return true, nil }

// M1: what overall may read beside each status. not_checked for anything in this build always blocks ok.
func TestR3OverallIsTheWorstRowAndOnlyTwoStatusesDoNotCount(t *testing.T) { // M3-T4 r3 M1
	for _, tc := range []struct {
		rows []string
		want string
	}{
		{[]string{statusOK}, statusOK},
		{[]string{statusOK, statusNotInBuild}, statusOK},
		{[]string{statusOK, statusNotApplicable}, statusOK},
		{[]string{statusOK, statusNotInBuild, statusNotApplicable}, statusOK},
		{[]string{statusOK, statusNotInBuild, statusNotChecked}, statusNotChecked},
		{[]string{statusNotApplicable, statusNotChecked}, statusNotChecked},
		{[]string{statusNotChecked, statusWarn, statusNotInBuild}, statusWarn},
		{[]string{statusWarn, statusFailed, statusNotChecked}, statusFailed},
	} {
		var cs []SystemComponentJSON
		for _, r := range tc.rows {
			cs = append(cs, SystemComponentJSON{Status: r})
		}
		if got := overallOf(cs); got != tc.want {
			t.Errorf("rows %v: overall %s, want %s", tc.rows, got, tc.want)
		}
	}
}

// M1: every row status the API can say is in the contract, and overall never says the two that do not count.
func TestR3OpenAPIListsEveryRowStatus(t *testing.T) { // M3-T4 r3 M1
	b, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "\n    SystemHealthResponse:")
	if i < 0 {
		t.Fatal("no SystemHealthResponse schema")
	}
	schema := doc[i:]
	schema = schema[:strings.Index(schema, "\n    JobListResponse:")]
	row := "status: {type: string, enum: [ok, not_checked, warn, failed, not_in_build, not_applicable]}"
	all := "overall: {type: string, enum: [ok, not_checked, warn, failed]}"
	if !strings.Contains(schema, row) || !strings.Contains(schema, all) {
		t.Errorf("the schema does not state the row and overall statuses:\n%.400s", schema)
	}
	for _, id := range []string{statusOK, statusWarn, statusFailed, statusNotChecked, statusNotInBuild, statusNotApplicable} {
		if !strings.Contains(row, id) {
			t.Errorf("status %s is not in the row enum", id)
		}
	}
}
