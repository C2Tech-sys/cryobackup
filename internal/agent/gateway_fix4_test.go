package agent

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M1-FIX4 A: the gateway watchdog (gateway_watchdog.go). A backup that makes no
// progress while the gateway cannot be reached is stopped after
// throttle.gateway_down_grace_minutes and journaled deferred / gateway /
// network; a reachable gateway or a moving backup is left alone.

// ---- the rules, on their own ------------------------------------------------

func TestGatewayWatchRules(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	sample := func(bytes, files uint64) supervisor.Progress {
		return supervisor.Progress{BytesDone: bytes, FilesDone: files}
	}
	const grace = 15 * time.Minute

	t.Run("unreachable and not moving: stopped once both clocks pass the grace", func(t *testing.T) {
		w := newGatewayWatch(grace, 30*time.Second, t0)
		w.progress(sample(100, 1), at(time.Second))
		if st, stop := w.probed(false, at(time.Minute)); !st || stop {
			t.Fatalf("first failed probe: stalled %v stop %v, want stalled only", st, stop)
		}
		if _, stop := w.probed(false, at(time.Minute+grace-time.Second)); stop {
			t.Fatal("stopped before the grace")
		}
		if _, stop := w.probed(false, at(time.Minute+grace)); !stop {
			t.Fatal("not stopped at the grace")
		}
	})
	t.Run("the grace counts from the first failed probe, not from the last progress", func(t *testing.T) {
		w := newGatewayWatch(grace, 30*time.Second, t0)
		w.progress(sample(100, 1), at(time.Second)) // last progress at 1 s
		w.probed(true, at(20*time.Minute))          // reachable: nothing moves (the stall path)
		w.probed(false, at(25*time.Minute))         // the outage begins
		if _, stop := w.probed(false, at(39*time.Minute)); stop {
			t.Fatal("stopped 14 minutes into the outage")
		}
		if _, stop := w.probed(false, at(40*time.Minute)); !stop {
			t.Fatal("not stopped 15 minutes into the outage")
		}
	})
	t.Run("reachable and not moving: never stopped", func(t *testing.T) {
		w := newGatewayWatch(grace, 30*time.Second, t0)
		for d := time.Minute; d < 10*time.Hour; d += time.Minute {
			if st, stop := w.probed(true, at(d)); st || stop {
				t.Fatalf("at %v: stalled %v stop %v with the gateway reachable", d, st, stop)
			}
		}
	})
	t.Run("progress resets the clock while the gateway stays away", func(t *testing.T) {
		w := newGatewayWatch(grace, 30*time.Second, t0)
		for d := time.Minute; d < 10*time.Hour; d += time.Minute {
			w.progress(sample(uint64(d/time.Minute)*1000, 1), at(d)) // bytes move every minute
			if _, stop := w.probed(false, at(d+30*time.Second)); stop {
				t.Fatalf("a moving backup was stopped at %v", d)
			}
		}
		w.progress(sample(5, 2), at(11*time.Hour)) // files_done alone is progress too
		if _, stop := w.probed(false, at(11*time.Hour+grace-time.Second)); stop {
			t.Fatal("files_done did not count as progress")
		}
	})
	t.Run("a repeated sample is not progress", func(t *testing.T) {
		w := newGatewayWatch(grace, 30*time.Second, t0)
		w.progress(sample(100, 1), at(time.Second))
		w.probed(false, at(2*time.Second))
		for d := time.Minute; d < grace; d += time.Minute {
			w.progress(sample(100, 1), at(d))
		}
		if _, stop := w.probed(false, at(2*time.Second+grace)); !stop {
			t.Fatal("the same sample republished was taken for progress")
		}
	})
	t.Run("a gateway that answers again ends the outage", func(t *testing.T) {
		w := newGatewayWatch(grace, 30*time.Second, t0)
		w.probed(false, at(time.Minute))
		if st, _ := w.probed(true, at(10*time.Minute)); st {
			t.Fatal("still stalled with the gateway reachable")
		}
		w.probed(false, at(12*time.Minute)) // a new outage: its own clock
		if _, stop := w.probed(false, at(26*time.Minute)); stop {
			t.Fatal("the earlier outage was counted into the new one")
		}
		if _, stop := w.probed(false, at(27*time.Minute)); !stop {
			t.Fatal("the new outage was not stopped at its grace")
		}
	})
	t.Run("stalled is shown only after a period without progress", func(t *testing.T) {
		w := newGatewayWatch(grace, 30*time.Second, t0)
		w.progress(sample(100, 1), at(time.Minute))
		if st, _ := w.probed(false, at(time.Minute+10*time.Second)); st {
			t.Fatal("stalled 10 s after progress")
		}
		if st, _ := w.probed(false, at(time.Minute+30*time.Second)); !st {
			t.Fatal("not stalled 30 s after progress with the gateway away")
		}
	})
}

func TestGatewayProbeAddr(t *testing.T) {
	for repo, want := range map[string]string{
		"rest:https://01JB0000000000000000000001:secret@gateway.example:9443/01JB0000000000000000000001/": "gateway.example:9443",
		"rest:https://gateway.example/dev/":   "gateway.example:443",
		"rest:http://127.0.0.1:8000/":         "127.0.0.1:8000",
		"rest:http://lab-gw/":                 "lab-gw:80",
		"rest:https://[2001:db8::7]:9443/d/":  "[2001:db8::7]:9443",
		"rest:HTTPS://Gateway.Example:1/dev/": "Gateway.Example:1",
	} {
		if got, ok := gatewayProbeAddr(repo); !ok || got != want {
			t.Errorf("gatewayProbeAddr(%q) = %q, %v, want %q", repo, got, ok, want)
		}
	}
	for _, repo := range []string{`C:\lab\repo`, "/srv/repo", "s3:https://s3.example/bucket", "rest:", "rest:ftp://h/x", "rest:https:///nohost"} {
		if got, ok := gatewayProbeAddr(repo); ok {
			t.Errorf("gatewayProbeAddr(%q) = %q: nothing to probe expected", repo, got)
		}
	}
	// The probe itself: an open listener answers, a closed port does not.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go acceptAndClose(ln)
	if !probeGateway(context.Background(), ln.Addr().String(), 5*time.Second) {
		t.Error("an open listener was reported unreachable")
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if probeGateway(context.Background(), addr, 5*time.Second) {
		t.Error("a closed port was reported reachable")
	}
}

// acceptAndClose answers every connection by closing it (a gateway that is up).
func acceptAndClose(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// ---- the real agent with the fake restic -----------------------------------

// gatewayRig is a running agent whose restic is the fake of scenario and whose
// gateway is a local listener: up (accepting) or down (a closed port). The
// grace is the policy minimum, 5 minutes, and the device clock can be moved.
type gatewayRig struct {
	fc     *fakeConsole
	r      *running
	dir    string
	log    *syncBuf
	offset atomic.Int64 // added to the real clock, nanoseconds
}

func newGatewayRig(t *testing.T, scenario string, gatewayUp bool) *gatewayRig {
	t.Helper()
	fc := newFakeConsole(t)
	pol := fc.policy
	pol.Policy.Throttle.GatewayDownGraceMinutes = 5
	fc.setPolicy(pol)
	fakerestic.Use(t, scenario)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if gatewayUp {
		go acceptAndClose(ln)
		t.Cleanup(func() { _ = ln.Close() })
	} else {
		_ = ln.Close()
	}
	g := &gatewayRig{fc: fc, log: &syncBuf{}}
	g.dir = enroll(t, fc)
	o := fakeOptions(t, g.dir)
	o.gatewayAddr, o.gatewayProbeEvery = addr, 50*time.Millisecond
	o.Clock = func() time.Time { return time.Now().Add(time.Duration(g.offset.Load())) }
	o.Logger = slog.New(slog.NewTextHandler(g.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g.r = start(t, o)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("agent log:\n%s", g.log.String())
		}
	})
	waitFor(t, "the backup to be running", 30*time.Second, func() bool {
		return g.active() && strings.Contains(g.log.String(), `msg="backup starting"`)
	})
	return g
}

func (g *gatewayRig) active() bool {
	g.r.a.mu.Lock()
	defer g.r.a.mu.Unlock()
	return g.r.a.running != nil
}

func (g *gatewayRig) stalled() bool {
	g.r.a.mu.Lock()
	defer g.r.a.mu.Unlock()
	return g.r.a.gwStalled
}

// stalledHeartbeat reports whether the console got a heartbeat that says the
// run is running, stalled, with class network.
func (g *gatewayRig) stalledHeartbeat() bool {
	for _, hb := range g.fc.heartbeatList() {
		if hb.ServiceState == api.ServiceRunning && hb.ActiveRunID != "" && hb.Progress != nil && hb.Progress.Stalled &&
			hb.Progress.RunID == hb.ActiveRunID && hb.LastErrorClass == restic.ClassNetwork {
			return true
		}
	}
	return false
}

func TestGatewayDownMidRunIsDeferredByTheWatchdog(t *testing.T) {
	ctx := context.Background()
	g := newGatewayRig(t, "gateway_stall", false)

	// The condition is seen and reported while it lasts.
	waitFor(t, "the watchdog to see the gateway away", 20*time.Second, g.stalled)
	waitFor(t, "a heartbeat that says running, stalled, network", 10*time.Second, g.stalledHeartbeat)
	time.Sleep(500 * time.Millisecond)
	if n := len(g.fc.closedRuns()); n != 0 || !g.active() {
		t.Fatalf("the run ended inside the grace (%d closed runs)", n)
	}

	// The grace (5 minutes on the device clock) runs out.
	g.offset.Add(int64(5*time.Minute + time.Second))
	waitFor(t, "the deferred run at the console", 30*time.Second, func() bool { return len(g.fc.closedRuns()) == 1 })
	run := g.fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeDeferred || run.DeferredReason != restic.DeferredGateway || run.ErrorClass != restic.ClassNetwork {
		t.Fatalf("run = outcome %s reason %s class %s, want deferred / gateway / network (never cancelled, never failed)",
			run.Outcome, run.DeferredReason, run.ErrorClass)
	}
	if run.SnapshotID != "" || run.Attempt != 1 || run.Trigger != api.TriggerBoot {
		t.Errorf("run = %+v", run)
	}
	if err := run.Validate(); err != nil {
		t.Errorf("the deferred record does not validate: %v", err)
	}
	if !strings.Contains(g.log.String(), "restic is stopped and the run deferred") {
		t.Error("the watchdog's decision was not logged")
	}

	// The journal says the same; the retry waits out the normal backoff.
	jr := openJournalRO(t, g.dir)
	rows, _ := jr.RecentRuns(ctx, restic.KindBackup, 5)
	if len(rows) != 1 || rows[0].Outcome != restic.OutcomeDeferred || rows[0].DeferredReason != restic.DeferredGateway || rows[0].ErrorClass != restic.ClassNetwork {
		t.Fatalf("journal rows = %+v", rows)
	}
	f := g.r.a.facts(ctx)
	lo, hi := retryBounds(1)
	if !f.Retry.Pending || f.Attempts != 1 || f.Last == nil || f.Last.FinishedAt == nil {
		t.Fatalf("facts after the deferral = %+v", f)
	}
	if wait := f.Retry.NotBefore.Sub(*f.Last.FinishedAt); wait < lo || wait > hi {
		t.Errorf("next attempt in %v, want %v..%v", wait, lo, hi)
	}
	waitFor(t, "a heartbeat that says deferred / gateway", 10*time.Second, func() bool {
		for _, hb := range g.fc.heartbeatList() {
			if hb.ServiceState == api.ServiceDeferred && hb.DeferredReason == restic.DeferredGateway && hb.LastErrorClass == restic.ClassNetwork {
				return true
			}
		}
		return false
	})
	if g.stalled() {
		t.Error("the stalled flag outlived the run")
	}
}

// Before restic printed a status line there is no progress object to carry the
// flag: the heartbeat gets one of its own (and it validates).
func TestGatewayWatchdogReportsAStallBeforeTheFirstStatus(t *testing.T) {
	g := newGatewayRig(t, "gateway_silent", false)
	waitFor(t, "a heartbeat that says running, stalled, network", 20*time.Second, g.stalledHeartbeat)
	for _, hb := range g.fc.heartbeatList() {
		if hb.Progress != nil && hb.Progress.Stalled {
			if err := hb.Validate(); err != nil {
				t.Fatalf("the stalled heartbeat does not validate: %v", err)
			}
		}
	}
}

// A gateway that answers while nothing moves is the stall path: the watchdog
// does not stop the run, however long it waits.
func TestGatewayWatchdogLeavesAReachableGatewayAlone(t *testing.T) {
	g := newGatewayRig(t, "gateway_stall", true)
	time.Sleep(300 * time.Millisecond)
	g.offset.Add(int64(6 * time.Minute))
	time.Sleep(1500 * time.Millisecond) // dozens of probes on a clock past the grace
	if n := len(g.fc.closedRuns()); n != 0 || !g.active() || g.stalled() {
		t.Fatalf("the run with a reachable gateway was touched: %d closed runs, active %v, stalled %v", n, g.active(), g.stalled())
	}
	if strings.Contains(g.log.String(), "restic is stopped") || g.stalledHeartbeat() {
		t.Fatal("the watchdog acted with the gateway reachable")
	}
	// The operator's cancel still ends it, as cancelled.
	if ok, _ := g.r.a.cmdCancel(context.Background(), ""); !ok {
		t.Fatal("no active run to cancel")
	}
	waitFor(t, "the cancelled run", 30*time.Second, func() bool { return len(g.fc.closedRuns()) == 1 })
	if run := g.fc.closedRuns()[0]; run.Outcome != restic.OutcomeCancelled {
		t.Fatalf("run = %+v, want cancelled", run)
	}
}

// Progress alone resets the clock: a backup that keeps moving while every probe
// fails is never stopped. The device clock moves 100 times faster than real
// time, past three graces, while restic keeps printing new byte counts.
func TestGatewayWatchdogNeverStopsAMovingBackup(t *testing.T) {
	g := newGatewayRig(t, "progress_forever", false)
	for i := 0; i < 900; i++ { // 15 minutes on the device clock, 1 s per 10 ms
		g.offset.Add(int64(time.Second))
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(g.fc.closedRuns()); n != 0 || !g.active() {
		t.Fatalf("a moving backup was stopped (%d closed runs)", n)
	}
	if strings.Contains(g.log.String(), "restic is stopped") {
		t.Fatal("the watchdog stopped a moving backup")
	}
	if ok, _ := g.r.a.cmdCancel(context.Background(), ""); !ok {
		t.Fatal("no active run to cancel")
	}
	waitFor(t, "the cancelled run", 30*time.Second, func() bool { return len(g.fc.closedRuns()) == 1 })
	if run := g.fc.closedRuns()[0]; run.Outcome != restic.OutcomeCancelled {
		t.Fatalf("run = %+v, want cancelled", run)
	}
}

// A service stop while the watchdog waits out the grace is the stop it always
// was: prompt, and journaled interrupted, not deferred.
func TestStopDuringTheGatewayWatchdogWait(t *testing.T) {
	g := newGatewayRig(t, "gateway_stall", false)
	waitFor(t, "the watchdog to see the gateway away", 20*time.Second, g.stalled)
	began := time.Now()
	g.r.stop()
	if d := time.Since(began); d > 10*time.Second {
		t.Fatalf("the stop took %v during the watchdog's wait", d)
	}
	rows, err := openJournalRO(t, g.dir).RecentRuns(context.Background(), restic.KindBackup, 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("journal rows = %+v, %v", rows, err)
	}
	if rows[0].Outcome != restic.OutcomeInterrupted || rows[0].DeferredReason != "" {
		t.Fatalf("stopped run = outcome %s reason %q, want interrupted", rows[0].Outcome, rows[0].DeferredReason)
	}
}
