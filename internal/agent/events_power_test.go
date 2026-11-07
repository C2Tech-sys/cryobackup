package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/service"
	"github.com/c2tech-sys/cryobackup/internal/api"
)

// r24 and r207: power and session events reach the agent through the service
// host's OnEvent.

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// powerRig is a running agent with a clock the test can move and a debug log
// it can read. The scheduler ticks once an hour, so only an event (or the
// first plan) can start a run.
type powerRig struct {
	fc     *fakeConsole
	r      *running
	log    *syncBuf
	offset atomic.Int64 // added to the real clock, nanoseconds
}

func newPowerRig(t *testing.T, runOnWake bool, mutate ...func(*Options)) *powerRig {
	t.Helper()
	fc := newFakeConsole(t)
	pol := fc.policy
	pol.Policy.Schedule.RunOnWake = runOnWake
	fc.setPolicy(pol)
	p := &powerRig{fc: fc, log: &syncBuf{}}
	fakerestic.Use(t, "backup_ok")
	o := fakeOptions(t, enroll(t, fc))
	o.schedTick = time.Hour
	o.Clock = func() time.Time { return time.Now().Add(time.Duration(p.offset.Load())) }
	o.Logger = slog.New(slog.NewTextHandler(p.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, m := range mutate {
		m(&o)
	}
	p.r = start(t, o)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("agent log:\n%s", p.log.String())
		}
	})
	waitFor(t, "the first (boot) run to be uploaded", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	// Let the post-run re-plan settle: nothing is due, the loop sleeps.
	time.Sleep(300 * time.Millisecond)
	if got := fc.closedRuns()[0].Trigger; got != api.TriggerBoot {
		t.Fatalf("first run trigger = %q, want boot", got)
	}
	return p
}

// sleepPastTheSlot moves the device clock a day and an hour on, as a laptop
// asleep overnight would find it.
func (p *powerRig) sleepPastTheSlot() { p.offset.Add(int64(25 * time.Hour)) }

func resumeEvent(code uint32) service.Event { return service.Event{Power: true, Code: code} }

func TestResumeStartsADueRunJournaledWake(t *testing.T) {
	for name, code := range map[string]uint32{"PBT_APMRESUMEAUTOMATIC": service.PBTAPMResumeAutomatic, "PBT_APMRESUMESUSPEND": service.PBTAPMResumeSuspend} {
		t.Run(name, func(t *testing.T) {
			p := newPowerRig(t, true)
			p.sleepPastTheSlot()
			p.r.a.HandleEvent(resumeEvent(code))
			waitFor(t, "the resume to start a second run", 30*time.Second, func() bool { return len(p.fc.closedRuns()) == 2 })
			if got := p.fc.closedRuns()[1].Trigger; got != api.TriggerWake {
				t.Errorf("trigger = %q, want wake", got)
			}
			if !strings.Contains(p.log.String(), "resume: the scheduler re-plans") {
				t.Errorf("no debug line for the resume; log:\n%s", p.log.String())
			}
		})
	}
}

// Without the event nothing starts (the tick is an hour): the wake run above
// is the event's doing.
func TestNoRunWithoutTheEvent(t *testing.T) {
	p := newPowerRig(t, true)
	p.sleepPastTheSlot()
	time.Sleep(1500 * time.Millisecond)
	if n := len(p.fc.closedRuns()); n != 1 {
		t.Fatalf("%d runs without any event, want 1", n)
	}
}

func TestResumeIgnoredWhenRunOnWakeIsOff(t *testing.T) {
	p := newPowerRig(t, false)
	p.sleepPastTheSlot()
	p.r.a.HandleEvent(resumeEvent(service.PBTAPMResumeAutomatic))
	time.Sleep(1500 * time.Millisecond)
	if n := len(p.fc.closedRuns()); n != 1 {
		t.Fatalf("%d runs after a resume with run_on_wake off, want 1", n)
	}
	if !strings.Contains(p.log.String(), "run_on_wake is off") {
		t.Errorf("the ignored resume was not logged; log:\n%s", p.log.String())
	}
}

func TestSessionAndOtherPowerEventsAreRecordedAndIgnored(t *testing.T) {
	p := newPowerRig(t, true)
	p.sleepPastTheSlot()
	for _, c := range []uint32{service.WTSSessionLogon, service.WTSSessionLogoff, service.WTSSessionLock, service.WTSSessionUnlock} {
		p.r.a.HandleEvent(service.Event{Power: false, Code: c})
	}
	p.r.a.HandleEvent(resumeEvent(service.PBTAPMSuspend))
	time.Sleep(1500 * time.Millisecond)
	if n := len(p.fc.closedRuns()); n != 1 {
		t.Fatalf("%d runs after session and suspend events, want 1: they must not start a backup", n)
	}
	out := p.log.String()
	if !strings.Contains(out, "session change ignored") || !strings.Contains(out, "power event ignored") {
		t.Errorf("events not recorded; log:\n%s", out)
	}
}

// The control handler also receives Stop, Shutdown and PreShutdown: HandleEvent
// must return while the agent's own locks are held elsewhere.
func TestHandleEventCannotDelayStop(t *testing.T) {
	p := newPowerRig(t, true)
	a := p.r.a
	a.mu.Lock()
	a.secMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.HandleEvent(resumeEvent(service.PBTAPMResumeAutomatic))
		a.HandleEvent(resumeEvent(service.PBTAPMResumeSuspend))
		a.HandleEvent(resumeEvent(service.PBTAPMSuspend))
		a.HandleEvent(service.Event{Code: service.WTSSessionLogon})
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		a.secMu.Unlock()
		a.mu.Unlock()
		t.Fatal("HandleEvent blocked while the agent's locks were held")
	}
	a.secMu.Unlock()
	a.mu.Unlock()
}

// run_on_wake changed at runtime: a policy pushed with run_on_wake off is
// mirrored into the lock-free flag HandleEvent reads (applyPolicy), so a resume
// after it is ignored (M1-FIX1 D13).
//
// What this judges is the resume, not the run count. applyPolicy publishes the
// policy and then asks the scheduler to re-plan, so that re-plan can read the clock
// after this test moved it a day on, and the daily slot it then finds due is really
// due: a schedule-triggered run is correct there (it failed 1 run in 40 on exactly
// that, "backup starting trigger=schedule" 39 ms after the policy applied;
// TestAPolicyChangeOnlyRePlans below pins both halves). So a run is allowed; a wake
// run, or the resume reaching the scheduler at all, is not.
func TestRunOnWakeTurnedOffAtRuntimeIgnoresTheNextResume(t *testing.T) {
	p := newPowerRig(t, true)
	a := p.r.a
	v2 := api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 2, ETag: `"policy-v2"`, ServerTime: time.Now()}
	v2.Policy.Schedule.RunOnWake = false
	p.fc.setPolicy(v2)
	waitFor(t, "policy v2 to be applied", 10*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.pol != nil && a.pol.Version == 2
	})
	p.sleepPastTheSlot()
	a.HandleEvent(resumeEvent(service.PBTAPMResumeAutomatic))
	time.Sleep(1500 * time.Millisecond)
	for _, r := range p.fc.closedRuns() {
		if r.Trigger == api.TriggerWake {
			t.Fatalf("a wake run under a policy with run_on_wake off: %+v", r)
		}
	}
	log := p.log.String()
	if !strings.Contains(log, "run_on_wake is off") {
		t.Errorf("the ignored resume was not logged; log:\n%s", log)
	}
	if strings.Contains(log, "resume: the scheduler re-plans") {
		t.Errorf("the resume reached the scheduler under a policy with run_on_wake off; log:\n%s", log)
	}
}

// M3-HARDEN A4: a policy change is a re-plan and nothing more. With nothing due it
// starts nothing; with a slot due the run it starts is the schedule's, which is what
// the hourly tick would have started anyway.
func TestAPolicyChangeOnlyRePlans(t *testing.T) {
	v2 := func() api.PolicyResponse {
		return api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 2, ETag: `"policy-v2"`, ServerTime: time.Now()}
	}
	waitV2 := func(t *testing.T, p *powerRig) {
		t.Helper()
		a := p.r.a
		waitFor(t, "policy v2 to be applied", 10*time.Second, func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.pol != nil && a.pol.Version == 2
		})
	}
	t.Run("nothing due starts nothing", func(t *testing.T) {
		p := newPowerRig(t, true)
		p.fc.setPolicy(v2())
		waitV2(t, p)
		time.Sleep(1500 * time.Millisecond)
		if n := len(p.fc.closedRuns()); n != 1 {
			t.Fatalf("%d runs after a policy change with nothing due, want 1", n)
		}
	})
	t.Run("a due slot runs as the schedule", func(t *testing.T) {
		p := newPowerRig(t, true)
		p.sleepPastTheSlot()
		p.fc.setPolicy(v2())
		waitV2(t, p)
		waitFor(t, "the due slot to run", 30*time.Second, func() bool { return len(p.fc.closedRuns()) == 2 })
		if got := p.fc.closedRuns()[1].Trigger; got != api.TriggerSchedule {
			t.Fatalf("trigger = %q, want schedule", got)
		}
	})
}

// schedUp publishes a.sched to the service's control goroutine, so it must not
// close before a.sched exists. facts() is called by the heartbeat as well as by
// the scheduler; a call before Run has set a.sched (here: directly, as a
// heartbeat would) must leave schedUp open, and a resume then is ignored, not
// a nil dereference (M1-FIX1 D16).
func TestSchedulerUpNeverClosesBeforeTheSchedulerExists(t *testing.T) {
	fc := newFakeConsole(t)
	fakerestic.Use(t, "backup_ok")
	a, err := New(context.Background(), fakeOptions(t, enroll(t, fc)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_ = a.facts(context.Background())
	select {
	case <-a.schedUp:
		t.Fatal("schedUp closed before a.sched was set")
	default:
	}
	a.HandleEvent(resumeEvent(service.PBTAPMResumeAutomatic)) // ignored: no scheduler yet
}

func TestEventRelay(t *testing.T) {
	var r EventRelay
	r.Handle(resumeEvent(service.PBTAPMResumeAutomatic)) // no agent yet: dropped, no panic
	p := newPowerRig(t, true)
	p.sleepPastTheSlot()
	r.Set(p.r.a)
	r.Handle(resumeEvent(service.PBTAPMResumeAutomatic))
	waitFor(t, "the relayed resume to start a run", 30*time.Second, func() bool { return len(p.fc.closedRuns()) == 2 })
	r.Set(nil)
	r.Handle(resumeEvent(service.PBTAPMResumeAutomatic)) // gone again: dropped
}
