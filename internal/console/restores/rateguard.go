package restores

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// errStreamGuard is the cancellation cause of a stream the guard aborted.
var errStreamGuard = errors.New("restores: the download stream was cut by its guard")

// aborter is implemented by a destination that can cut a write in progress
// (the admin API's HTTP stream writer sets its write deadline to now), so a
// stream the guard aborts stops at once even when the browser reads nothing
// and a write is blocked.
type aborter interface{ Abort() }

// Why a guard tripped.
const (
	tripNone int32 = iota
	// tripTooSlow: fewer than min bytes were accepted in a window.
	tripTooSlow
	// tripNoData: no byte arrived within the first-byte bound.
	tripNoData
)

// guardClock is the guard's time source: now, and a ticker of period d (its
// stop function is called when the guard ends). The zero value is wall time;
// tests drive both.
type guardClock struct {
	now   func() time.Time
	ticks func(d time.Duration) (<-chan time.Time, func())
	// checked, when set, is called after every check a tick caused, once
	// the check is complete (also after one that tripped the guard). Tests
	// wait on it so they never move the clock under a check in progress.
	checked func()
}

func (c *guardClock) checkedFn() func() {
	if c != nil && c.checked != nil {
		return c.checked
	}
	return func() {}
}

func (c *guardClock) nowFn() func() time.Time {
	if c != nil && c.now != nil {
		return c.now
	}
	return time.Now
}

func (c *guardClock) tickFn() func(time.Duration) (<-chan time.Time, func()) {
	if c != nil && c.ticks != nil {
		return c.ticks
	}
	return func(d time.Duration) (<-chan time.Time, func()) {
		t := time.NewTicker(d)
		return t.C, t.Stop
	}
}

// rateGuard bounds one download stream (M1-HIPAA2):
//
//   - first byte: when no write arrives within firstByte of the guard's start
//     (restic hangs before it produces anything), it trips with tripNoData;
//   - minimum rate: from the first write on, it checks every step whether
//     fewer than min bytes were accepted by the destination in the last window
//     and trips with tripTooSlow if so. A stream younger than the window is not
//     judged, and a check that comes half a window or more after the previous
//     one (this process was stalled, not the browser) restarts the window
//     instead of judging it, so a steady stream is not cut for the console's
//     own scheduling delay. Time is read when a check runs, never taken from
//     the tick, whose value is stale when the goroutine ran late.
//
// It calls trip once, from its own goroutine, and refuses every later write.
// Bytes count when the destination accepted them (what reached the browser, as
// far as this process can know).
type rateGuard struct {
	w         io.Writer
	min       int64
	window    time.Duration
	firstByte time.Duration
	step      time.Duration
	now       func() time.Time
	ticks     func(time.Duration) (<-chan time.Time, func())
	checked   func()
	trip      func(reason int32)

	n       atomic.Int64
	reason  atomic.Int32
	first   sync.Once
	startAt time.Time   // written once, before started is set
	started atomic.Bool // set on the first write
	stop    chan struct{}
	done    chan struct{}
	stopped sync.Once
}

func newRateGuard(w io.Writer, minBytes int64, window, firstByte time.Duration, clk *guardClock, trip func(reason int32)) *rateGuard {
	step := window
	if firstByte > 0 && firstByte < step {
		step = firstByte
	}
	step /= 8
	if step < time.Millisecond {
		step = time.Millisecond
	}
	g := &rateGuard{w: w, min: minBytes, window: window, firstByte: firstByte, step: step,
		now: clk.nowFn(), ticks: clk.tickFn(), checked: clk.checkedFn(), trip: trip,
		stop: make(chan struct{}), done: make(chan struct{})}
	begin := g.now()
	go g.run(begin)
	return g
}

func (g *rateGuard) Write(p []byte) (int, error) {
	g.first.Do(func() {
		g.startAt = g.now()
		g.started.Store(true)
	})
	if g.reason.Load() != tripNone {
		return 0, errStreamGuard
	}
	n, err := g.w.Write(p)
	g.n.Add(int64(n))
	return n, err
}

// tripped returns why the guard cut the stream (tripNone when it did not).
// Call it after close for a final answer.
func (g *rateGuard) tripped() int32 { return g.reason.Load() }

// close stops the watchdog and waits for it. Safe to call twice.
func (g *rateGuard) close() {
	g.stopped.Do(func() { close(g.stop) })
	<-g.done
}

type rateSample struct {
	at time.Time
	n  int64
}

func (g *rateGuard) fire(reason int32) {
	g.reason.Store(reason)
	g.trip(reason)
}

func (g *rateGuard) run(begin time.Time) {
	defer close(g.done)
	tick, stopTicks := g.ticks(g.step)
	defer stopTicks()
	st := checkState{begin: begin, last: begin}
	for {
		select {
		case <-g.stop:
			return
		case <-tick:
		}
		ended := g.check(&st)
		g.checked()
		if ended {
			return
		}
	}
}

// checkState is what run carries from one check to the next.
type checkState struct {
	begin, last time.Time
	samples     []rateSample
}

// check is one tick's judgement. It reports true when it tripped the guard.
func (g *rateGuard) check(st *checkState) bool {
	now := g.now() // the tick's own value may be stale when this goroutine ran late
	gap := now.Sub(st.last)
	st.last = now
	if !g.started.Load() {
		if g.firstByte > 0 && now.Sub(st.begin) >= g.firstByte {
			g.fire(tripNoData)
			return true
		}
		return false
	}
	n := g.n.Load()
	if st.samples == nil {
		st.samples = []rateSample{{at: g.startAt}}
	}
	if gap > g.window/2 {
		// This goroutine (or the whole process) was not scheduled for half a
		// window or more: what the browser received meanwhile says nothing
		// about the browser. Start the window again from here.
		st.samples = []rateSample{{at: now, n: n}}
		return false
	}
	st.samples = append(st.samples, rateSample{at: now, n: n})
	// The baseline is the newest sample at or before now-window, so the window
	// judged is between window and window+step long.
	cut := now.Add(-g.window)
	base := -1
	for i, s := range st.samples {
		if s.at.After(cut) {
			break
		}
		base = i
	}
	if base < 0 {
		return false // the stream (or the restarted window) is younger than the window
	}
	st.samples = st.samples[base:]
	if n-st.samples[0].n < g.min {
		g.fire(tripTooSlow)
		return true
	}
	return false
}
