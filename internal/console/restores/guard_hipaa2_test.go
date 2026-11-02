package restores

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
)

// fakeGuardClock drives a stream guard from the test: the guard's time is t,
// a tick reaches it only when the test sends one, and the guard reports each
// finished check on checked (guardClock.checked). advance moves t only while
// the guard is idle in its select - after the previous check has reported -
// so a check can never read a time beyond the tick it is processing.
type fakeGuardClock struct {
	mu      sync.Mutex
	t       time.Time
	tick    chan time.Time
	checked chan struct{}
}

func newFakeGuardClock() *fakeGuardClock {
	return &fakeGuardClock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), tick: make(chan time.Time), checked: make(chan struct{})}
}

func (c *fakeGuardClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *fakeGuardClock) clock() *guardClock {
	return &guardClock{
		now:     c.now,
		ticks:   func(time.Duration) (<-chan time.Time, func()) { return c.tick, func() {} },
		checked: func() { c.checked <- struct{}{} },
	}
}

// advance moves the guard's time by d, delivers one tick, and returns only once
// the guard has finished the check that tick caused (a check reports even when
// it trips the guard). It reports whether the tick was delivered (false: the
// download ended first, the guard is gone).
func (c *fakeGuardClock) advance(t *testing.T, d time.Duration, done <-chan struct{}) bool {
	t.Helper()
	c.mu.Lock()
	c.t = c.t.Add(d)
	now := c.t
	c.mu.Unlock()
	select {
	case c.tick <- now:
	case <-done:
		return false
	case <-time.After(30 * time.Second):
		t.Fatal("the guard took no tick for 30 s")
		return false
	}
	select {
	case <-c.checked:
		return true
	case <-time.After(30 * time.Second):
		t.Fatal("the guard did not finish its check in 30 s")
		return false
	}
}

// stepBackend writes exactly what the test asks for, when it asks: each value
// sent on steps is written as one chunk, and wrote reports the write's error.
// Closing steps ends the dump successfully.
type stepBackend struct {
	*fixtures.Backend
	steps chan int
	wrote chan error
}

func newStepBackend(be *fixtures.Backend) *stepBackend {
	return &stepBackend{Backend: be, steps: make(chan int), wrote: make(chan error, 1)}
}

func (b *stepBackend) Dump(ctx context.Context, _ adminpath.DumpRequest, w io.Writer) (adminpath.DumpResult, error) {
	for {
		select {
		case <-ctx.Done():
			return adminpath.DumpResult{}, ctx.Err()
		case n, ok := <-b.steps:
			if !ok {
				return adminpath.DumpResult{}, nil
			}
			_, err := w.Write(bytes.Repeat([]byte{'z'}, n))
			b.wrote <- err
			if err != nil {
				return adminpath.DumpResult{}, adminpath.ErrStreamWrite
			}
		}
	}
}

// write asks the backend for one chunk and waits until it was written. It
// reports false when the download ended first.
func (b *stepBackend) write(t *testing.T, n int, done <-chan struct{}) bool {
	t.Helper()
	select {
	case b.steps <- n:
	case <-done:
		return false
	}
	select {
	case <-b.wrote:
		return true
	case <-done:
		return false
	}
}

// abortRecorder is a browser whose stream can be aborted (the admin API's
// streamWriter implements Abort).
type abortRecorder struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	aborted atomic.Bool
}

func (a *abortRecorder) Abort() { a.aborted.Store(true) }

func (a *abortRecorder) Write(p []byte) (int, error) {
	if a.aborted.Load() {
		return 0, errors.New("aborted")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.Write(p)
}

func (a *abortRecorder) Len() int { a.mu.Lock(); defer a.mu.Unlock(); return a.buf.Len() }

// guarded is one download under a test-driven guard.
type guarded struct {
	e    *env
	clk  *fakeGuardClock
	be   *stepBackend
	out  *abortRecorder
	done chan struct{}
	err  error
}

func startGuarded(t *testing.T, e *env, minBytes int64, window, firstByte time.Duration) (*guarded, string) {
	t.Helper()
	g := &guarded{e: e, clk: newFakeGuardClock(), be: newStepBackend(e.be), out: &abortRecorder{}, done: make(chan struct{})}
	e.svc.Backend, e.svc.guardClock = g.be, g.clk.clock()
	e.svc.MinRateBytes, e.svc.MinRateWindow, e.svc.FirstByteTimeout = minBytes, window, firstByte
	r := e.create(t, goodDir)
	go func() {
		_, g.err = e.svc.Download(context.Background(), Actor{}, r, g.out)
		close(g.done)
	}()
	// A tick without moving the clock: it goes through only once the guard
	// runs, so the guard has read its start time before the test moves time.
	if !g.clk.advance(t, 0, g.done) {
		t.Fatalf("the download ended before its guard ran: %v", g.err)
	}
	return g, r.ID
}

func (g *guarded) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-g.done:
		return g.err
	case <-time.After(30 * time.Second):
		t.Fatal("the download did not end")
		return nil
	}
}

// TestSlowStreamIsAbortedAsTooSlow (M1-HIPAA2 item 1): a slow-but-steady
// stream that never stalls a single write, but delivers fewer than
// MinRateBytes in MinRateWindow, is aborted once a full window has passed:
// the destination's Abort is called, the restore records class too_slow and
// stays downloadable, the failure is audited (before the class is visible on
// the row) and logged path-free.
func TestSlowStreamIsAbortedAsTooSlow(t *testing.T) {
	e := newEnv(t)
	g, id := startGuarded(t, e, 64<<10, 100*time.Millisecond, time.Hour)
	ticks := 0
	for g.be.write(t, 16, g.done) && g.clk.advance(t, 10*time.Millisecond, g.done) {
		ticks++
		if ticks > 100 {
			t.Fatal("a stream far below the minimum rate was never cut")
		}
	}
	var fe *FailedError
	if err := g.wait(t); !errors.As(err, &fe) || fe.Class != ClassTooSlow {
		t.Fatalf("a too slow stream: %v", err)
	}
	if ticks < 10 {
		t.Fatalf("cut after %d ticks of 10 ms: a 100 ms window must pass first", ticks)
	}
	if !g.out.aborted.Load() || g.out.Len() == 0 {
		t.Fatalf("aborted=%v after %d bytes: the destination must be cut once the stream started", g.out.aborted.Load(), g.out.Len())
	}
	row := e.get(t, id)
	if row.ErrorClass != ClassTooSlow || row.Status != "ready" || row.DownloadCount != 0 || row.StartedAt == nil || !e.svc.Downloadable(row) {
		t.Fatalf("row after a too slow stream: %+v", row)
	}
	if !auditHas(t, e, "restore.failed", id) {
		t.Fatal("no restore.failed audit row")
	}
	logs := e.logs.String()
	if !strings.Contains(logs, ClassTooSlow) || strings.Contains(logs, goodDir) {
		t.Fatalf("the failure is not logged path-free with its class:\n%s", logs)
	}
}

// TestSteadyStreamAboveTheMinimumPasses: a stream that keeps above the minimum
// rate for many windows is delivered.
func TestSteadyStreamAboveTheMinimumPasses(t *testing.T) {
	e := newEnv(t)
	g, id := startGuarded(t, e, 4096, 100*time.Millisecond, time.Hour)
	for i := 0; i < 100; i++ {
		if !g.be.write(t, 1024, g.done) || !g.clk.advance(t, 10*time.Millisecond, g.done) {
			t.Fatalf("the stream ended after %d steps: %v", i, g.wait(t))
		}
	}
	close(g.be.steps)
	if err := g.wait(t); err != nil {
		t.Fatalf("a stream above the minimum rate: %v", err)
	}
	if g.out.aborted.Load() || g.out.Len() != 1024*100 {
		t.Fatalf("aborted=%v len=%d", g.out.aborted.Load(), g.out.Len())
	}
	if row := e.get(t, id); row.Status != "delivered" || row.DownloadCount != 1 || row.StartedAt == nil {
		t.Fatalf("row: %+v", row)
	}
}

// TestConsoleStallDoesNotCutASteadyStream: the guard's check comes a long time
// after the previous one (the console process was not scheduled; the browser
// kept up). That gap must restart the window, not count as a window in which
// the browser received nothing.
func TestConsoleStallDoesNotCutASteadyStream(t *testing.T) {
	e := newEnv(t)
	g, _ := startGuarded(t, e, 4096, 100*time.Millisecond, time.Hour)
	for i := 0; i < 20; i++ {
		if !g.be.write(t, 1024, g.done) || !g.clk.advance(t, 10*time.Millisecond, g.done) {
			t.Fatalf("ended early: %v", g.wait(t))
		}
	}
	// Three windows pass on the clock with no check and no write in between.
	if !g.clk.advance(t, 300*time.Millisecond, g.done) {
		t.Fatalf("a console stall cut a steady stream: %v", g.wait(t))
	}
	for i := 0; i < 30; i++ {
		if !g.be.write(t, 1024, g.done) || !g.clk.advance(t, 10*time.Millisecond, g.done) {
			t.Fatalf("a console stall cut a steady stream: %v", g.wait(t))
		}
	}
	close(g.be.steps)
	if err := g.wait(t); err != nil {
		t.Fatalf("a steady stream after a console stall: %v", err)
	}
}

// TestNoFirstByteIsCancelledAsNoData (M1-HIPAA2 follow-up 2): restic produces
// nothing; at FirstByteTimeout the stream is cancelled (the worker's run with
// it), the restore records class no_data with the timeout code, the failure is
// audited and logged path-free, and nothing was written to the browser.
func TestNoFirstByteIsCancelledAsNoData(t *testing.T) {
	e := newEnv(t)
	g, id := startGuarded(t, e, 64<<10, 100*time.Millisecond, 50*time.Millisecond)
	ticks := 0
	for g.clk.advance(t, 10*time.Millisecond, g.done) {
		ticks++
		if ticks > 100 {
			t.Fatal("a stream with no first byte was never cancelled")
		}
	}
	var fe *FailedError
	if err := g.wait(t); !errors.As(err, &fe) || fe.Class != ClassNoData || fe.Code != adminpath.CodeTimeout {
		t.Fatalf("no first byte: %v", err)
	}
	if ticks < 4 {
		t.Fatalf("cancelled after %d ticks of 10 ms, before the 50 ms bound", ticks)
	}
	if g.out.aborted.Load() || g.out.Len() != 0 {
		t.Fatal("nothing was sent, so nothing is to be aborted: the caller still answers with a problem")
	}
	row := e.get(t, id)
	if row.ErrorClass != ClassNoData || row.Status != "ready" || row.DownloadCount != 0 {
		t.Fatalf("row: %+v", row)
	}
	if !auditHas(t, e, "restore.failed", id) {
		t.Fatal("no restore.failed audit row")
	}
	if logs := e.logs.String(); !strings.Contains(logs, ClassNoData) || strings.Contains(logs, goodDir) {
		t.Fatalf("the failure is not logged path-free with its class:\n%s", logs)
	}
}

// busyOnce answers busy until released; hook runs inside the refused call.
type busyOnce struct {
	*fixtures.Backend
	busy atomic.Bool
	hook func()
}

func (b *busyOnce) Dump(ctx context.Context, req adminpath.DumpRequest, w io.Writer) (adminpath.DumpResult, error) {
	if b.busy.Load() {
		if b.hook != nil {
			b.hook()
		}
		return adminpath.DumpResult{}, adminpath.NewError(adminpath.CodeBusy, "")
	}
	return b.Backend.Dump(ctx, req, w)
}

// TestBusyLeavesTheTokenUsable (M1-HIPAA2 follow-up 3): a download refused
// because every stream is in use does not spend its token; once a slot frees,
// the same token streams exactly once, and a second use is refused. A token
// minted while the refused download was in flight wins over the given-back one.
func TestBusyLeavesTheTokenUsable(t *testing.T) {
	e := newEnv(t)
	be := &busyOnce{Backend: e.be}
	be.busy.Store(true)
	e.svc.Backend = be
	ctx := context.Background()
	r := e.create(t, goodPath)
	tok, _, err := e.svc.MintToken(r, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), tok, "s1", io.Discard); !errors.Is(err, ErrStreamsBusy) {
		t.Fatalf("busy: %v", err)
	}
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), tok, "s2", io.Discard); !errors.Is(err, ErrToken) {
		t.Fatalf("a given-back token is still bound to its session: %v", err)
	}
	be.busy.Store(false)
	var out bytes.Buffer
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), tok, "s1", &out); err != nil || out.Len() == 0 {
		t.Fatalf("the same token after a slot freed: %v (%d bytes)", err, out.Len())
	}
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), tok, "s1", io.Discard); !errors.Is(err, ErrToken) {
		t.Fatalf("a used token must not work again: %v", err)
	}
	if row := e.get(t, r.ID); row.DownloadCount != 1 {
		t.Fatalf("download_count = %d, want 1", row.DownloadCount)
	}

	// A token minted while the refused download was in flight wins.
	old, _, _ := e.svc.MintToken(e.get(t, r.ID), "s1")
	var fresh string
	be.busy.Store(true)
	be.hook = func() { fresh, _, _ = e.svc.MintToken(e.get(t, r.ID), "s1") }
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), old, "s1", io.Discard); !errors.Is(err, ErrStreamsBusy) {
		t.Fatalf("busy: %v", err)
	}
	be.busy.Store(false)
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), old, "s1", io.Discard); !errors.Is(err, ErrToken) {
		t.Fatalf("the older token must stay dead once a newer one was minted: %v", err)
	}
	if _, err := e.svc.DownloadWithToken(ctx, Actor{}, e.get(t, r.ID), fresh, "s1", io.Discard); err != nil {
		t.Fatalf("the newer token: %v", err)
	}
}
