package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/service"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M1-FIX3 item 10, architecture §6.6 (resume row): a run a resume makes due
// waits for the console (a heartbeat it answers) up to 2 minutes before it
// starts; a stop or a cancel ends the wait at once.

// startResumeWait is a power rig whose console is unreachable when the machine
// wakes past its slot, with a start lease to tell a run that waited (granted)
// from one that started while the console was away (bypassed: fail open). It
// returns once the wake run is active, that is inside the wait; it sleeps and
// asserts nothing about time.
func startResumeWait(t *testing.T, mutate ...func(*Options)) *powerRig {
	t.Helper()
	p := newPowerRig(t, true, append([]func(*Options){func(o *Options) { o.resumeProbe = 50 * time.Millisecond }}, mutate...)...)
	p.fc.leaseMode.Store(leaseGrant)
	p.fc.down.Store(true)
	p.sleepPastTheSlot()
	p.r.a.HandleEvent(resumeEvent(service.PBTAPMResumeAutomatic))
	waitFor(t, "the wake run to become active", 10*time.Second, func() bool {
		p.r.a.mu.Lock()
		defer p.r.a.mu.Unlock()
		return p.r.a.running != nil
	})
	return p
}

// resumeWaitRig is startResumeWait with real timers, given a moment for a run
// that does not wait to reach restic.
func resumeWaitRig(t *testing.T) *powerRig {
	t.Helper()
	p := startResumeWait(t)
	time.Sleep(200 * time.Millisecond) // long enough for a run that does not wait to reach restic
	if !strings.Contains(p.log.String(), "resume: waiting for the console") {
		t.Errorf("the wait was not logged")
	}
	return p
}

// fakeTimers is the wait's clock and timers, driven by the test: a timer fires
// only when the test advances (Advance) or fires (FireAll) it, so no assertion
// about the wait depends on how fast the machine is.
type fakeTimers struct {
	mu    sync.Mutex
	now   time.Time
	list  []*fakeTimer
	armed chan struct{} // one token per timer started
}

type fakeTimer struct {
	at      time.Time
	ch      chan time.Time
	stopped bool
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{now: time.Unix(1_800_000_000, 0), armed: make(chan struct{}, 1024)}
}

func (f *fakeTimers) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// New is Options.resumeTimer.
func (f *fakeTimers) New(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	tm := &fakeTimer{at: f.now.Add(d), ch: make(chan time.Time, 1)}
	f.list = append(f.list, tm)
	f.mu.Unlock()
	f.armed <- struct{}{}
	return tm.ch, func() { f.mu.Lock(); tm.stopped = true; f.mu.Unlock() }
}

// Advance moves the clock and fires every timer that is due.
func (f *fakeTimers) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for _, tm := range f.list {
		if !tm.stopped && !tm.at.After(f.now) {
			tm.stopped = true
			tm.ch <- f.now
		}
	}
}

// FireAll fires every pending timer whatever its time (the rig's clock is the
// real one plus an offset, so the test moves that and wakes the wait).
func (f *fakeTimers) FireAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tm := range f.list {
		if !tm.stopped {
			tm.stopped = true
			tm.ch <- f.now
		}
	}
}

// waitArmed blocks until the wait has started another timer (the wait is idle
// and listening). The timeout is only a hang guard.
func (f *fakeTimers) waitArmed(t *testing.T) {
	t.Helper()
	select {
	case <-f.armed:
	case <-time.After(60 * time.Second):
		t.Fatal("the wait never armed a timer")
	}
}

// starts counts the runs that reached restic (the boot run is one).
func (p *powerRig) starts() int { return strings.Count(p.log.String(), "msg=\"backup starting\"") }

func TestResumeWaitsForTheConsole(t *testing.T) {
	p := resumeWaitRig(t)
	time.Sleep(1500 * time.Millisecond) // the console stays away for 1.5 s
	if n := p.starts(); n != 1 {
		t.Fatalf("%d runs reached restic while the console was unreachable after the resume, want only the boot run", n)
	}
	p.fc.down.Store(false)
	waitFor(t, "the wake run once the console is back", 30*time.Second, func() bool { return len(p.fc.closedRuns()) == 2 })
	run := p.fc.closedRuns()[1]
	if run.Trigger != api.TriggerWake || run.Outcome != restic.OutcomeSuccess || run.Stats.Lease != api.LeaseGranted {
		t.Fatalf("wake run = %+v (lease %q), want a wake success that asked the console for its lease", run, run.Stats.Lease)
	}
	if !strings.Contains(p.log.String(), "resume: the console answered") {
		t.Errorf("the end of the wait was not logged")
	}
}

// The limit is read from the agent's clock: moving it past 2 minutes ends the
// wait, and the run starts anyway (the start lease fails open: bypassed). The
// wait's probe timer is the test's (fakeTimers), so nothing here races a real
// timer or a sleep: the run cannot leave the wait until the test says so.
func TestResumeWaitEndsAfterTheLimit(t *testing.T) {
	ft := newFakeTimers()
	p := startResumeWait(t, func(o *Options) { o.resumeTimer = ft.New })
	ft.waitArmed(t) // the wait is in its loop and the console is away
	if n := p.starts(); n != 1 {
		t.Fatalf("%d runs reached restic inside the wait, want only the boot run", n)
	}
	p.offset.Add(int64(resumeNetWait + time.Second))
	ft.FireAll()
	waitFor(t, "the wake run once the wait is over", 60*time.Second, func() bool { return p.starts() == 2 })
	if !strings.Contains(p.log.String(), "the console did not answer in time; starting the backup anyway") {
		t.Errorf("the timeout was not logged at warn")
	}
	p.fc.down.Store(false)
	waitFor(t, "the wake run to be uploaded", 60*time.Second, func() bool { return len(p.fc.closedRuns()) == 2 })
	if run := p.fc.closedRuns()[1]; run.Trigger != api.TriggerWake || run.Stats.Lease != api.LeaseBypassed {
		t.Fatalf("wake run = %+v (lease %q), want wake, lease bypassed (started while the console was away)", run, run.Stats.Lease)
	}
}

// The wait on a fake clock, with no agent around it: it probes every 10 s,
// gives up when the limit passes on the agent's clock and not before, and
// returns true the moment the console answers.
func TestWaitForConsoleOnAFakeClock(t *testing.T) {
	newWait := func(ft *fakeTimers) (*Agent, chan bool) {
		a := &Agent{clock: ft.Now, o: Options{resumeWait: 2 * time.Minute, resumeProbe: 10 * time.Second, resumeTimer: ft.New},
			consoleReached: make(chan struct{}, 1), kickHeartbeat: make(chan struct{}, 1)}
		done := make(chan bool, 1)
		go func() { done <- a.waitForConsole(context.Background()) }()
		return a, done
	}
	noResult := func(t *testing.T, done chan bool, what string) {
		t.Helper()
		select {
		case got := <-done:
			t.Fatalf("the wait returned %v %s", got, what)
		default:
		}
	}

	t.Run("limit", func(t *testing.T) {
		ft := newFakeTimers()
		_, done := newWait(ft)
		for i := 0; i < 12; i++ { // 12 probes of 10 s reach the 2-minute limit exactly
			ft.waitArmed(t)
			noResult(t, done, "before the limit")
			ft.Advance(10 * time.Second)
		}
		select {
		case got := <-done:
			if got {
				t.Fatal("the wait reported an answer the console never gave")
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the wait did not end at its limit")
		}
	})

	t.Run("answer", func(t *testing.T) {
		ft := newFakeTimers()
		a, done := newWait(ft)
		for i := 0; i < 3; i++ {
			ft.waitArmed(t)
			ft.Advance(10 * time.Second)
		}
		ft.waitArmed(t) // idle again, 30 s in
		noResult(t, done, "with the console still away")
		a.noteConsoleReached()
		select {
		case got := <-done:
			if !got {
				t.Fatal("the console answered but the wait said it did not")
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the wait did not end when the console answered")
		}
	})
}

func TestStopDuringTheResumeWait(t *testing.T) {
	p := resumeWaitRig(t)
	began := time.Now()
	p.r.stop()
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("the stop took %v during the resume wait", d)
	}
	log := p.log.String()
	if !strings.Contains(log, "backup stopped before restic started") || !strings.Contains(log, "outcome=interrupted") {
		t.Fatalf("the stopped wait was not journaled interrupted; log:\n%s", log)
	}
	if n := p.starts(); n != 1 {
		t.Fatalf("%d runs reached restic, want only the boot run", n)
	}
}

func TestCancelDuringTheResumeWait(t *testing.T) {
	p := resumeWaitRig(t)
	began := time.Now()
	if ok, _ := p.r.a.cmdCancel(context.Background(), ""); !ok {
		t.Fatal("cancel found no active run: the waiting run must be active")
	}
	waitFor(t, "the cancelled wait", 2*time.Second, func() bool {
		log := p.log.String()
		return strings.Contains(log, "backup stopped before restic started") && strings.Contains(log, "outcome=cancelled")
	})
	t.Logf("cancel to journaled: %v", time.Since(began))
	if n := p.starts(); n != 1 {
		t.Fatalf("%d runs reached restic, want only the boot run", n)
	}
}

// A heartbeat answered before the wait began does not end it.
func TestResumeWaitIgnoresAnEarlierAnswer(t *testing.T) {
	var off time.Duration
	a := &Agent{clock: func() time.Time { return time.Unix(1_800_000_000, 0).Add(off) },
		o:              Options{resumeWait: time.Minute, resumeProbe: 10 * time.Millisecond},
		consoleReached: make(chan struct{}, 1), kickHeartbeat: make(chan struct{}, 1)}
	a.noteConsoleReached() // before the wait
	off = time.Second
	done := make(chan bool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() { done <- a.waitForConsole(ctx) }()
	if <-done {
		t.Fatal("an answer from before the wait ended it")
	}
	if ctx.Err() == nil {
		t.Fatal("the wait ended before its context: it did not wait")
	}
}
