package maintain

import (
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// stampAll marks every candidate's escrowed key verified, so only the usage
// refresh runs in a pass.
func (e *tenv) stampAll() int {
	e.t.Helper()
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET verified_at = ? WHERE custody = 'escrowed' AND retired_at IS NULL`, e.clk.Now().UnixMilli())
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	cands, err := e.w.candidates(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(cands)
}

// M2 carry-over (M3-T2 item 7): step 8 starts no usage refresh after
// usageRefreshBudget, so a slow storage endpoint cannot hold one pass past the
// 30 minutes after which C07 reads the scheduler as stopped. The devices left
// over are deferred, not failed, and later passes sample them.
func TestC14UsageRefreshStopsAtTheTimeBudget(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	n := e.stampAll()
	if n < 3 {
		t.Fatalf("the fixtures give %d candidates; the test needs at least 3", n)
	}
	// Every read of the worker's clock is 2 min later than the one before (a slow storage endpoint).
	var mu sync.Mutex
	reads := 0
	e.w.clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return e.clk.Now().Add(time.Duration(reads) * 2 * time.Minute)
	}
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sampled < 1 || rep.Sampled >= n || rep.UsageDeferred == 0 || rep.Failed != 0 {
		t.Fatalf("with a 5 min budget and 2 min per clock read: %+v (candidates %d)", rep, n)
	}
	if rep.Sampled+rep.UsageDeferred+rep.Preflight != n {
		t.Fatalf("every due device is sampled, deferred or preflight-skipped: %+v (candidates %d)", rep, n)
	}
}

// The per-pass count bound: once usageRefreshPerPass stats runs have started, a
// due device is deferred (counted, no stats run, not a failure).
func TestC14UsageRefreshIsBoundedPerPass(t *testing.T) {
	if usageRefreshPerPass < 1 || usageRefreshBudget <= 0 || usageRefreshBudget+schedulerSlotWait >= 30*time.Minute {
		t.Fatalf("usage bound %d / %v must keep a pass well inside C07's 30 min", usageRefreshPerPass, usageRefreshBudget)
	}
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	n := e.stampAll()
	cands, _ := e.w.candidates(e.ctx)
	var rep CycleReport
	for i := range cands {
		if e.w.step(e.ctx, cands[i], &rep, nil, false) {
			t.Fatal("a step past the bound started a stats run")
		}
	}
	if rep.UsageDeferred != n || rep.Sampled != 0 || rep.Failed != 0 {
		t.Fatalf("past the bound: %+v (candidates %d)", rep, n)
	}
	if n, _ := usageRows(t, e, e.esc); n != 0 {
		t.Fatalf("usage written past the bound: %d rows", n)
	}
}

// The scheduler lease is renewed while a pass runs: a pass that outlives the
// lease's TTL still holds it at the end, and a pass whose lease another
// process took stops without writing job_state.
func TestC14SchedulerLeaseRenewedDuringThePass(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json", SleepMS: 300}}, nil)
	e.onlyDevice(e.esc)
	e.stampAll()
	e.w.renewEvery = 50 * time.Millisecond
	done := make(chan CycleReport, 1)
	go func() {
		rep, _ := e.w.RunOnce(e.ctx)
		done <- rep
	}()
	// Wait until the pass holds the lease, note its expiry, move the clock on a
	// minute while the stats run sleeps: a renewal pushes the expiry out.
	leaseUntil := func() (string, time.Time) {
		js, err := e.st.GetJobState(e.ctx, SchedulerJob)
		if err != nil || js.LeaseUntil == nil {
			return "", time.Time{}
		}
		return js.LeaseHolder, *js.LeaseUntil
	}
	deadline := time.Now().Add(2 * time.Second)
	holder, first := leaseUntil()
	for holder != e.w.holder && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		holder, first = leaseUntil()
	}
	if holder != e.w.holder {
		t.Fatal("the pass never took the lease")
	}
	e.clk.Advance(time.Minute)
	time.Sleep(150 * time.Millisecond)
	if h, later := leaseUntil(); h != e.w.holder || !later.After(first) {
		t.Fatalf("lease not renewed during the pass: %s until %v, first %v", h, later, first)
	}
	if got, err := e.st.AcquireJobLease(e.ctx, SchedulerJob, "other-maintain", time.Minute); err != nil || got {
		t.Fatalf("another process took a lease held by a running pass: %v %v", got, err)
	}
	rep := <-done
	if rep.LeaseLost || rep.Sampled != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestC14SchedulerLeaseLostStopsThePass(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}, {Cmd: "stats", Stdout: "stats_raw_data.json", SleepMS: 400}}, nil)
	e.onlyDevice(e.esc)
	e.stampAll()
	e.w.renewEvery = 50 * time.Millisecond
	done := make(chan CycleReport, 1)
	go func() {
		rep, _ := e.w.RunOnce(e.ctx)
		done <- rep
	}()
	time.Sleep(100 * time.Millisecond)
	// An operator (or a crashed-lease takeover) hands the lease to another holder.
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE job_state SET lease_holder = 'other-maintain' WHERE name = ?`, SchedulerJob)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rep := <-done
	if !rep.LeaseLost {
		t.Fatalf("report %+v: the pass kept running without its lease", rep)
	}
	if js, err := e.st.GetJobState(e.ctx, SchedulerJob); err == nil && js.LastFinishedAt != nil {
		t.Fatalf("a pass without its lease wrote job_state: %+v", js)
	}
}
