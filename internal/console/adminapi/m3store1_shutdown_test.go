package adminapi

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-STORE1 C1, C3: at shutdown a POST probe in flight ends and writes its audit
// row before the store closes; a new one is refused; a manual job run in flight
// is cancelled and waited for; what outlives the wait fails cleanly.

// A POST probe in flight at shutdown: stopping the prober ends its wait, and
// WaitBackground returns only after its audit row is written.
func TestStorageProbeInFlightAtShutdownWritesItsAuditRow(t *testing.T) { // M3-STORE1 C3
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := newProbeBackend()
	be.hold.Store(true)
	hp, _ := e.realProber(be, "console-a")
	answer := e.postProbe(owner, target)
	select {
	case <-be.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the storage probe never started")
	}
	e.svc.BeginShutdown()
	hp.Stop()
	if !e.svc.WaitBackground(10 * time.Second) {
		t.Fatal("WaitBackground gave up on the probe")
	}
	if n := e.auditCount("storage_target.probe", target); n != 1 {
		t.Fatalf("%d audit rows when WaitBackground returned, want the probe's one", n)
	}
	e.wantCancelledProbe(t, target, waitAnswer(t, answer))
}

// WaitBackground really waits for the handler: it does not return while the
// handler is still inside its probe.
func TestWaitBackgroundWaitsForTheProbeHandler(t *testing.T) { // M3-STORE1 C3
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	release := make(chan struct{})
	entered := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo) // before the test server closes, which waits for the handler
	e.svc.Prober = probeFunc(func(context.Context) (bool, error) {
		close(entered)
		<-release
		return false, context.Canceled
	})
	answer := e.postProbe(owner, target)
	<-entered
	e.svc.BeginShutdown()
	if e.svc.WaitBackground(50 * time.Millisecond) {
		t.Fatal("WaitBackground returned with the probe handler still running")
	}
	letGo()
	if !e.svc.WaitBackground(10 * time.Second) {
		t.Fatal("WaitBackground gave up after the handler could end")
	}
	if n := e.auditCount("storage_target.probe", target); n != 1 {
		t.Fatalf("%d audit rows, want 1", n)
	}
	waitAnswer(t, answer)
}

// probeFunc is a HealthProbe from a function.
type probeFunc func(context.Context) (bool, error)

func (f probeFunc) ProbeNow(ctx context.Context) (bool, error) { return f(ctx) }

// Past the wait the store is closed: the audit write fails and so does the
// request, with a 500, never an ok that no audit row backs.
func TestStorageProbeOutlivingTheStoreFailsTheRequest(t *testing.T) { // M3-STORE1 C3
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	e.svc.Prober = probeFunc(func(context.Context) (bool, error) {
		if err := e.st.Close(); err != nil {
			t.Error(err)
		}
		return true, nil
	})
	a := waitAnswer(t, e.postProbe(owner, target))
	if a.err != nil || a.status != 500 {
		t.Fatalf("probe over a closed store = %d %s (%v), want 500", a.status, a.body, a.err)
	}
}

func TestShutdownRefusesNewProbesAndJobRuns(t *testing.T) { // M3-STORE1 C1, C3
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	var probes, runs atomic.Int64
	e.svc.Prober = probeFunc(func(context.Context) (bool, error) { probes.Add(1); return true, nil })
	e.svc.Jobs = &scriptedRunner{run: func(context.Context) { runs.Add(1) }}
	e.svc.BeginShutdown()
	e.mustReq("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil, 503)
	e.mustReq("POST", "/api/v1/system/jobs/scripted/run", owner, nil, 503)
	if !e.svc.WaitBackground(10 * time.Second) {
		t.Fatal("nothing was running, yet WaitBackground gave up")
	}
	if probes.Load() != 0 || runs.Load() != 0 {
		t.Fatalf("probes %d, job runs %d after BeginShutdown; want none", probes.Load(), runs.Load())
	}
	if n := e.auditCount("storage_target.probe", target); n != 0 {
		t.Errorf("%d probe audit rows for a refused probe, want 0", n)
	}
}

// scriptedRunner is a job runner with one job, "scripted".
type scriptedRunner struct {
	run func(context.Context)
}

func (r *scriptedRunner) Names() []string { return []string{"scripted"} }

func (r *scriptedRunner) RunOnce(ctx context.Context, _ string) (bool, error) {
	r.run(ctx)
	return true, ctx.Err()
}

// A manual job run in flight at shutdown has its context cancelled, and
// WaitBackground returns after it has ended.
func TestShutdownCancelsAndWaitsForAManualJobRun(t *testing.T) { // M3-STORE1 C1
	e := newEnv(t)
	owner := e.login("admin", pw)
	entered := make(chan struct{})
	var ended atomic.Bool
	e.svc.Jobs = &scriptedRunner{run: func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // finishes its writes
		if err := e.st.Write(context.Background(), func(*store.Tx) error { return nil }); err != nil {
			t.Errorf("the job's last write: %v", err)
		}
		ended.Store(true)
	}}
	e.mustReq("POST", "/api/v1/system/jobs/scripted/run", owner, nil, 202)
	<-entered
	e.svc.BeginShutdown()
	if !e.svc.WaitBackground(10 * time.Second) {
		t.Fatal("WaitBackground gave up on a job that honours its context")
	}
	if !ended.Load() {
		t.Fatal("WaitBackground returned before the job ended")
	}
}

// A job that ignores its context cannot hold the shutdown.
func TestWaitBackgroundIsBounded(t *testing.T) { // M3-STORE1 C1
	e := newEnv(t)
	owner := e.login("admin", pw)
	release, entered := make(chan struct{}), make(chan struct{})
	t.Cleanup(jobBackground.Wait)
	t.Cleanup(func() { close(release) })
	e.svc.Jobs = &scriptedRunner{run: func(context.Context) { close(entered); <-release }}
	e.mustReq("POST", "/api/v1/system/jobs/scripted/run", owner, nil, 202)
	<-entered
	e.svc.BeginShutdown()
	start := time.Now()
	if e.svc.WaitBackground(50 * time.Millisecond) {
		t.Fatal("WaitBackground said a hung job ended")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("WaitBackground took %v", d)
	}
}
