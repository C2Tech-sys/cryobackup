package cli

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// nilPointers lists every nil pointer field of v (a struct), recursing into the
// structs the non-nil pointers point at, by dotted path.
func nilPointers(v reflect.Value, prefix string) []string {
	var out []string
	for i := 0; i < v.NumField(); i++ {
		f, name := v.Field(i), prefix+v.Type().Field(i).Name
		switch {
		case f.Kind() == reflect.Pointer && f.IsNil():
			out = append(out, name)
		case f.Kind() == reflect.Pointer && f.Elem().Kind() == reflect.Struct:
			out = append(out, nilPointers(f.Elem(), name+".")...)
		case f.Kind() == reflect.Struct:
			out = append(out, nilPointers(f, name+".")...)
		}
	}
	return out
}

// serveProbeFacts is what serve's World carries: the page path and the
// page-less path of the assembly, from a production Build.
func serveProbeFacts(t *testing.T, app *App, page bool) state.ProbeFacts {
	t.Helper()
	jobsRows, jerr := app.Store.ListJobStates(context.Background())
	if jerr != nil {
		t.Fatal(jerr)
	}
	return app.Admin.Fleet.ProbeFacts(jobsRows, nil, page)
}

// M3-T2 task 1: serve supplies every ProbeFacts pointer. The only nil allowed is
// View on the page-less path (the alarm evaluator has no page); the page path
// always sets it. A new pointer added to state.ProbeFacts that serve forgets
// fails here by name.
func TestC21ServeSuppliesEveryProbeFactsPointer(t *testing.T) {
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	if app.Prober == nil {
		t.Fatal("serve built no health-prober")
	}
	// Before the first master-key read the generation comparison is not done: MasterKey is nil (C12b).
	if p := serveProbeFacts(t, app, true); p.MasterKey != nil {
		t.Fatalf("a comparison before any escrow read: %+v", p.MasterKey)
	}
	if err := app.Prober.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { app.Prober.WaitAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("the probes did not finish within their bounds")
	}
	page := serveProbeFacts(t, app, true)
	if !page.Wired {
		t.Fatal("serve's probe facts are not wired: C00 would show")
	}
	if n := nilPointers(reflect.ValueOf(page), ""); len(n) != 0 {
		t.Fatalf("page path leaves nil: %v", n)
	}
	bg := serveProbeFacts(t, app, false)
	if n := nilPointers(reflect.ValueOf(bg), ""); !slices.Equal(n, []string{"View"}) {
		t.Fatalf("page-less path nil pointers %v, want only View", n)
	}
	if page.View.RefreshInterval <= 0 {
		t.Fatalf("view refresh %v", page.View.RefreshInterval)
	}
}

// C00 is gone on a wired console and comes back when the wiring is removed.
func TestC21C00GoneWhenWiredBackWhenUnwired(t *testing.T) {
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	has := func(id string) bool {
		f, err := app.Admin.Fleet.Fleet(context.Background(), store.ScopeAll())
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range f.View.Banners {
			if b.ID == id {
				return true
			}
		}
		return false
	}
	if has("C00") {
		t.Fatal("C00 on a wired console")
	}
	app.Admin.Fleet.Probes = nil
	if !has("C00") {
		t.Fatal("no C00 after the wiring was removed")
	}
}

// markedChecks returns the check names serve marks NotInBuild.
func markedChecks(p state.ProbeFacts) []string {
	var out []string
	if p.Quota != nil && p.Quota.NotInBuild {
		out = append(out, health.CheckQuota)
	}
	if p.Database != nil && p.Database.SelfBackup != nil && p.Database.SelfBackup.NotInBuild {
		out = append(out, health.CheckSelfBackup)
	}
	if p.Digest != nil && p.Digest.NotInBuild {
		out = append(out, health.CheckDigest)
	}
	if p.Release != nil && p.Release.NotInBuild {
		out = append(out, health.CheckRelease)
	}
	return out
}

// A check may carry its NotInBuild marker only while no job of its producer's
// name exists: neither in serve's runner nor as the maintenance worker's job.
// Registering, say, release-check without removing the marker fails here.
func TestNotInBuildMarksOnlyUnbuiltJobs(t *testing.T) {
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	registered := append(app.Runner.Names(), health.SchedulerJob)
	p := serveProbeFacts(t, app, true)
	if marked := markedChecks(p); len(marked) != len(health.NotInBuild) {
		t.Fatalf("marked %v, table %v: every NotInBuild entry must be applied and every marker listed", marked, health.NotInBuild)
	}
	if v := notInBuildViolations(registered, p); len(v) != 0 {
		t.Fatal(strings.Join(v, "\n"))
	}
	// The guard catches a producer registered while its marker stays.
	if v := notInBuildViolations(append(registered, "release-check"), p); len(v) != 1 || !strings.Contains(v[0], "release-check") {
		t.Fatalf("guard missed a registered release-check: %v", v)
	}
}

// notInBuildViolations names each marked check whose producer job is registered.
func notInBuildViolations(registered []string, p state.ProbeFacts) []string {
	var out []string
	for _, check := range markedChecks(p) {
		job, ok := health.NotInBuild[check]
		switch {
		case !ok:
			out = append(out, "check "+check+" is marked but not in health.NotInBuild")
		case slices.Contains(registered, job):
			out = append(out, "check "+check+" is marked NotInBuild while job "+job+" is registered: remove the marker and wire the producer")
		}
	}
	return out
}

// Serve registers the M3 jobs; the healthcheck ping only with a URL.
func TestServeRegistersTheMonitoringJobs(t *testing.T) {
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	names := strings.Join(app.Runner.Names(), ",")
	for _, want := range []string{jobs.NameHealthProber, jobs.NameAlarmEvaluator, jobs.NameAbandonedRunSweeper} {
		if !strings.Contains(names, want) {
			t.Errorf("job %s not registered: %s", want, names)
		}
	}
	if strings.Contains(names, jobs.NameHealthcheckPing) {
		t.Errorf("healthcheck-ping registered without a URL: %s", names)
	}
	// With a URL configured it is registered.
	cfg := app.Cfg
	cfg.HealthcheckPing.URL = "https://monitor.example.invalid/ping/x"
	app.Runner = jobs.NewRunner(app.Store, "t", nil)
	app.addMonitorJobs(cfg, app.Admin.Fleet)
	if names := strings.Join(app.Runner.Names(), ","); !strings.Contains(names, jobs.NameHealthcheckPing) {
		t.Errorf("healthcheck-ping not registered with a URL: %s", names)
	}
}

// The alarm-evaluator records its job_state row, which clears C06b and is what
// healthcheck-ping reads.
func TestC13AlarmEvaluatorRecordsItsRun(t *testing.T) {
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	ctx := context.Background()
	if ran, err := app.Runner.RunOnce(ctx, jobs.NameAlarmEvaluator); !ran || err != nil {
		t.Fatalf("ran %v err %v", ran, err)
	}
	j, err := app.Store.GetJobState(ctx, jobs.NameAlarmEvaluator)
	if err != nil || j.LastStatus != "ok" || j.LastFinishedAt == nil {
		t.Fatalf("job_state %+v %v", j, err)
	}
	p := serveProbeFacts(t, app, true)
	if p.Evaluator == nil || p.Evaluator.LastFinishedAt == 0 {
		t.Fatalf("evaluator facts %+v", p.Evaluator)
	}
	ok, err := jobs.EvaluatorHealthy(ctx, app.Store, 0, app.Store.Now())
	if err != nil || !ok {
		t.Fatalf("evaluator healthy %v %v", ok, err)
	}
}
