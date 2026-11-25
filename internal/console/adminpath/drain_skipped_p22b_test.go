package adminpath

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of the P22 fix batch additions: DumpResult.Skipped (counts by kind,
// never names) and the Drainer hook Serve calls at shutdown.

func TestDumpSkippedAddAndTotal(t *testing.T) {
	var s DumpSkipped
	for _, k := range []string{SkipSymlink, SkipSymlink, SkipHardLink, SkipDevice, SkipPipe, SkipSocket, SkipOther, "unheard-of"} {
		s.Add(k)
	}
	want := DumpSkipped{Symlinks: 2, HardLinks: 1, Devices: 1, Pipes: 1, Sockets: 1, Other: 2}
	if s != want || s.Total() != 8 {
		t.Fatalf("counts = %+v total %d, want %+v total 8", s, s.Total(), want)
	}
	data, err := json.Marshal(DumpResult{Skipped: s})
	if err != nil {
		t.Fatal(err)
	}
	var back DumpResult
	if err := json.Unmarshal(data, &back); err != nil || back.Skipped != want {
		t.Fatalf("round trip = %+v, %v (%s)", back.Skipped, err, data)
	}
	// A trailer from a worker that predates the count decodes as zero.
	var old DumpResult
	if err := json.Unmarshal([]byte(`{"bytes":1,"sha256":"x"}`), &old); err != nil || old.Skipped.Total() != 0 {
		t.Fatalf("old trailer = %+v, %v", old, err)
	}
}

// drainBackend records Drain and holds it for a while, like background work.
type drainBackend struct {
	*fakeBackend
	calls   atomic.Int32
	hold    time.Duration
	hadDead atomic.Bool
}

func (d *drainBackend) Drain(ctx context.Context) {
	d.calls.Add(1)
	_, has := ctx.Deadline()
	d.hadDead.Store(has)
	select {
	case <-time.After(d.hold):
	case <-ctx.Done():
	}
}

// TestServeDrainsTheBackend: Serve calls a Drainer backend's Drain once after
// the server shut down, with a bounded context, and returns only after Drain
// did; a backend that is not a Drainer is unaffected.
func TestServeDrainsTheBackend(t *testing.T) {
	b := &drainBackend{fakeBackend: newFake(), hold: 300 * time.Millisecond}
	path := filepath.Join(shortDir(t), "ap.sock")
	stop := serveAt(t, path, Handler(b))
	if b.calls.Load() != 0 {
		t.Fatal("Drain ran while serving")
	}
	t0 := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if n := b.calls.Load(); n != 1 {
		t.Fatalf("Drain called %d times, want 1", n)
	}
	if !b.hadDead.Load() {
		t.Fatal("Drain got a context without a deadline")
	}
	if d := time.Since(t0); d < b.hold {
		t.Fatalf("Serve returned after %v, before Drain finished (%v)", d, b.hold)
	}

	plain := newFake()
	path2 := filepath.Join(shortDir(t), "ap.sock")
	if err := serveAt(t, path2, Handler(plain))(); err != nil {
		t.Fatal(err)
	}
}
