//go:build windows

package supervisor

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestMemoryLimitHitWaitsForActiveProcessZero drives the job messages through
// the reader's seam (deliver) in the order the race produces: the child has
// exited (Run calls memoryLimitHit) before the memory-limit message has been
// taken off the port. memoryLimitHit must wait for "no active process", which
// the port delivers after every earlier message, and then see the hit
// (M1-FIX1 C8).
func TestMemoryLimitHitWaitsForActiveProcessZero(t *testing.T) {
	c := &control{}
	c.initWatch()
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.deliver(jobMsgProcessMemoryLimit)
		c.deliver(jobMsgActiveProcessZero)
	}()
	start := time.Now()
	if !c.memoryLimitHit() {
		t.Fatal("memoryLimitHit = false: it did not wait for the messages still on the port")
	}
	if took := time.Since(start); took >= activeProcessZeroWait {
		t.Errorf("memoryLimitHit took %s: it waited out the bound although no active process had arrived", took)
	}
}

// TestMemoryLimitHitWaitIsBounded: without "no active process" (a lost
// message) the wait ends after activeProcessZeroWait with what was seen.
func TestMemoryLimitHitWaitIsBounded(t *testing.T) {
	c := &control{}
	c.initWatch()
	c.deliver(jobMsgProcessMemoryLimit)
	start := time.Now()
	if !c.memoryLimitHit() {
		t.Fatal("a delivered memory-limit message was lost")
	}
	if took := time.Since(start); took < activeProcessZeroWait-100*time.Millisecond || took > activeProcessZeroWait+time.Second {
		t.Errorf("memoryLimitHit took %s, want about %s", took, activeProcessZeroWait)
	}
	if (&control{}).memoryLimitHit() {
		t.Error("a control without a port reported a memory hit")
	}
}

// fakePort stands in for the completion port. The reader is always inside a
// wait on it (an unrelated message every few milliseconds), and the wait counts
// as in flight for its whole duration. A posted packet wakes the wait, but the
// wait returns wakeDelay later, as a kernel wake-up does: the handle is still in
// use in between. portClose records how many waits are in flight at the moment
// the handle is closed, and a wait started after the close is counted and fails
// as Windows fails a wait on a closed handle.
type fakePort struct {
	q             chan uintptr
	closed        atomic.Bool
	afterClose    atomic.Int32
	inFlight      atomic.Int32
	inFlightClose atomic.Int32 // waits in flight when CloseHandle ran
}

const (
	fakeSlice     = 5 * time.Millisecond
	fakeWakeDelay = 40 * time.Millisecond
)

func (f *fakePort) wait(_ windows.Handle, msg *uint32, key *uintptr, _ **windows.Overlapped, millis uint32) error {
	if f.closed.Load() {
		f.afterClose.Add(1)
		return windows.ERROR_INVALID_HANDLE
	}
	f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	select {
	case k := <-f.q:
		time.Sleep(fakeWakeDelay)
		*key = k
		return nil
	case <-time.After(fakeSlice):
		*key, *msg = 1, 0 // a message the reader ignores
		return nil
	}
}

// install replaces the post and close seams. With postFails the quit packet
// cannot be posted, so the reader can only learn of the stop from c.stopping.
func (f *fakePort) install(t *testing.T, postFails bool) {
	oldPost, oldClose := portPost, portClose
	t.Cleanup(func() { portPost, portClose = oldPost, oldClose })
	portPost = func(_ windows.Handle, _ uint32, key uintptr, _ *windows.Overlapped) error {
		if postFails {
			return errors.New("post refused")
		}
		f.q <- key
		return nil
	}
	portClose = func(windows.Handle) error {
		f.inFlightClose.Store(f.inFlight.Load())
		f.closed.Store(true)
		return nil
	}
}

// TestClosePortNeverWaitsOnAClosedHandle: closePort stops the reader before it
// closes the port, so the handle is never closed under a wait that is still in
// flight, and nothing waits on the handle value after CloseHandle (M1-FIX1 C11,
// M1-FIX2): whether or not the quit packet can be posted. The fake wait is in
// flight whenever the reader is not between two waits, and a posted packet
// keeps it in flight for fakeWakeDelay, so closing before the reader has
// returned (the mutation this test exists for) is seen as a wait in flight at
// CloseHandle.
func TestClosePortNeverWaitsOnAClosedHandle(t *testing.T) {
	for _, postFails := range []bool{false, true} {
		name := "quit packet"
		if postFails {
			name = "post fails, stop flag"
		}
		t.Run(name, func(t *testing.T) {
			fp := &fakePort{q: make(chan uintptr, 1)}
			fp.install(t, postFails)

			c := &control{port: windows.Handle(0x1234)}
			c.initWatch()
			go c.readPort(c.port, fp.wait)
			time.Sleep(20 * time.Millisecond) // the reader is looping on the busy port
			if fp.inFlight.Load() == 0 {
				// A wait slice is 5 ms and the gap between two is nanoseconds.
				time.Sleep(fakeSlice)
			}

			done := make(chan struct{})
			go func() { c.closePort(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * portPollMillis * time.Millisecond):
				t.Fatal("closePort did not return: the reader was not stopped")
			}
			select {
			case <-c.portDone:
			default:
				t.Fatal("closePort returned while the reader was still running")
			}
			if !fp.closed.Load() || c.port != 0 {
				t.Fatal("closePort did not close the port")
			}
			if n := fp.inFlightClose.Load(); n != 0 {
				t.Fatalf("CloseHandle ran with %d wait(s) still in flight: the port was closed before the reader returned", n)
			}
			if n := fp.afterClose.Load(); n != 0 {
				t.Fatalf("the reader waited on the port %d time(s) after CloseHandle", n)
			}
		})
	}
}

// TestReadPortStopsOnTheQuitPacket: the reader returns when it takes the quit
// packet off the port, without c.stopping (the flag is only the fallback for a
// packet that could not be posted), and keeps running on every other key.
func TestReadPortStopsOnTheQuitPacket(t *testing.T) {
	fp := &fakePort{q: make(chan uintptr, 2)}
	c := &control{}
	c.initWatch()
	t.Cleanup(func() { c.stopping.Store(true); <-c.portDone }) // never leak the reader
	go c.readPort(windows.Handle(0x1234), fp.wait)

	fp.q <- 1 // the job's own key: not a stop
	select {
	case <-c.portDone:
		t.Fatal("the reader stopped on a packet with the job's key")
	case <-time.After(3 * fakeWakeDelay):
	}
	fp.q <- portQuitKey
	select {
	case <-c.portDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the reader did not stop on the quit packet (c.stopping was not set)")
	}
}

// TestMemoryLimitHitReturnsAtOnceWhenThePortIsGone: when the job could not be
// set up after the port was created, newControl calls closePort and the reader
// is gone; no message can arrive any more, so memoryLimitHit must not wait out
// activeProcessZeroWait for a "no active process" that will never come
// (M1-FIX2). What was delivered before the reader stopped still counts.
func TestMemoryLimitHitReturnsAtOnceWhenThePortIsGone(t *testing.T) {
	for _, hit := range []bool{false, true} {
		name := "no memory message"
		if hit {
			name = "memory message delivered first"
		}
		t.Run(name, func(t *testing.T) {
			fp := &fakePort{q: make(chan uintptr, 1)}
			fp.install(t, false)
			c := &control{port: windows.Handle(0x1234)}
			c.initWatch()
			go c.readPort(c.port, fp.wait)
			if hit {
				c.deliver(jobMsgProcessMemoryLimit)
			}
			c.closePort() // what newControl does when OpenProcess or Assign fails

			start := time.Now()
			got := c.memoryLimitHit()
			if took := time.Since(start); took > activeProcessZeroWait/4 {
				t.Fatalf("memoryLimitHit took %s with the reader gone: it waited for a message that cannot arrive", took)
			}
			if got != hit {
				t.Fatalf("memoryLimitHit = %v, want %v", got, hit)
			}
		})
	}
}

// TestMemoryLimitHitReturnsAtOnceWhenThePortFailed: the reader also returns
// when the port itself fails; the answer is final then, too.
func TestMemoryLimitHitReturnsAtOnceWhenThePortFailed(t *testing.T) {
	c := &control{}
	c.initWatch()
	go c.readPort(windows.Handle(0x1234), func(windows.Handle, *uint32, *uintptr, **windows.Overlapped, uint32) error {
		return windows.ERROR_INVALID_HANDLE
	})
	start := time.Now()
	if c.memoryLimitHit() {
		t.Fatal("memoryLimitHit = true with no memory message")
	}
	if took := time.Since(start); took > activeProcessZeroWait/4 {
		t.Fatalf("memoryLimitHit took %s after the port failed", took)
	}
}
