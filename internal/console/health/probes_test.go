package health

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// §4.8 C16 (M3-T1 review): the page path always sets the view facts, so a
// handler cannot lose C10 by forgetting them; the page-less path never does.
func TestC16PagePathSetsViewAlarmsDoesNot(t *testing.T) {
	e, c := newEval(t)
	e.Probes = FixtureProbes(c.now)
	e.Refresh = func() time.Duration { return 45 * time.Second }
	jobs, err := e.Store.ListJobStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := e.ProbeFacts(jobs, nil, true); p.View == nil || p.View.RefreshInterval != 45*time.Second {
		t.Fatalf("page path view %+v", p.View)
	}
	if p := e.ProbeFacts(jobs, nil, false); p.View != nil {
		t.Fatalf("page-less path view %+v", p.View)
	}
	e.Refresh = nil
	if p := e.ProbeFacts(jobs, nil, true); p.View == nil || p.View.RefreshInterval != state.DefaultRefreshInterval {
		t.Fatalf("default refresh %+v", p.View)
	}
}

// Fixtures mode is wired: no C00 and no probe banner, only what the fixture fleet earns.
func TestC21FixtureProbesAreWiredAndClear(t *testing.T) {
	e, c := newEval(t)
	e.Probes = FixtureProbes(c.now)
	for _, run := range []func(context.Context) (Fleet, error){
		func(ctx context.Context) (Fleet, error) { return e.Fleet(ctx, store.ScopeAll()) }, e.Alarms,
	} {
		f, err := run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(f.View.Banners); got != "" {
			t.Fatalf("wired fixture banners %q, want none", got)
		}
	}
	e.Probes = nil
	if f, _ := e.Fleet(context.Background(), store.ScopeAll()); ids(f.View.Banners) != "C00" {
		t.Fatalf("unwired: %s", ids(f.View.Banners))
	}
}

// The job heartbeats come from job_state: no alarm-evaluator row is a job that
// never finished (C06b within three periods of the start, C06 after), a stale
// row is C06; a job_state read error leaves the check nil, which is loud.
func TestC13EvaluatorFactsFromJobState(t *testing.T) {
	e, c := newEval(t)
	base := FixtureProbes(c.now)
	e.Probes = func() state.ProbeFacts {
		p := base()
		p.Evaluator, p.Maintain.Scheduler = nil, nil
		return p
	}
	ctx := context.Background()
	f, _ := e.Fleet(ctx, store.ScopeAll())
	if got := ids(f.View.Banners); !strings.Contains(got, "C06") || !strings.Contains(got, "C07") {
		t.Fatalf("no evaluator or scheduler row ever, started a day ago: %s", got)
	}
	for _, name := range []string{EvaluatorOKJob, SchedulerJob} {
		at := fixtures.Now().Add(-20 * time.Second)
		if err := e.Store.RecordJobRun(ctx, name, store.JobRun{Status: "ok", StartedAt: at, FinishedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if f, _ = e.Fleet(ctx, store.ScopeAll()); ids(f.View.Banners) != "" {
		t.Fatalf("fresh rows: %s", ids(f.View.Banners))
	}
	c.t = fixtures.Now().Add(10 * time.Minute)
	f, _ = e.Fleet(ctx, store.ScopeAll())
	if got := ids(f.View.Banners); !strings.HasPrefix(got, "C06") {
		t.Fatalf("evaluator finished 10 min ago: %s", got)
	}
	if p := e.ProbeFacts(nil, context.DeadlineExceeded, false); p.Evaluator != nil || p.Maintain.Scheduler != nil {
		t.Fatalf("a job_state read error must leave the heartbeats unsupplied: %+v %+v", p.Evaluator, p.Maintain.Scheduler)
	}
}

// §4.8 C13 (M3-T2 delta, LOW): an evaluator that runs but fails every
// evaluation is stalled alerting. C06 reads the newest successful evaluation
// (the alarm-evaluator.ok row), not the newest run, so fresh error rows do not
// clear it.
func TestC13EvaluatorErroringEveryRunRaisesC06(t *testing.T) {
	e, c := newEval(t)
	base := FixtureProbes(c.now)
	e.Probes = func() state.ProbeFacts { p := base(); p.Evaluator = nil; return p }
	ctx := context.Background()
	okAt := fixtures.Now().Add(-10 * time.Minute)
	if err := e.Store.RecordJobRun(ctx, EvaluatorOKJob, store.JobRun{Status: "ok", StartedAt: okAt, FinishedAt: okAt}); err != nil {
		t.Fatal(err)
	}
	failAt := fixtures.Now().Add(-20 * time.Second)
	if err := e.Store.RecordJobRun(ctx, EvaluatorJob, store.JobRun{Status: "error", ErrorClass: "store", StartedAt: failAt, FinishedAt: failAt}); err != nil {
		t.Fatal(err)
	}
	f, err := e.Fleet(ctx, store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(f.View.Banners); !strings.HasPrefix(got, "C06") {
		t.Fatalf("an evaluator failing for 10 min: %q, want C06", got)
	}
}

// The later-milestone checks carry their NotInBuild marker, never nil.
func TestNotInBuildMarkersApplied(t *testing.T) {
	e, c := newEval(t)
	e.Probes = FixtureProbes(c.now)
	p := e.ProbeFacts(nil, nil, false)
	if !p.Quota.NotInBuild || !p.Digest.NotInBuild || !p.Release.NotInBuild || !p.Database.SelfBackup.NotInBuild {
		t.Fatalf("markers: quota %+v digest %+v release %+v self-backup %+v", p.Quota, p.Digest, p.Release, p.Database.SelfBackup)
	}
	if len(NotInBuild) != 4 {
		t.Fatalf("NotInBuild table %v", NotInBuild)
	}
}
