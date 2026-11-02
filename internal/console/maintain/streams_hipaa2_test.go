package maintain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
)

// heldDest is a browser that does not read: its first write signals started
// and blocks until release is closed.
type heldDest struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHeldDest() *heldDest {
	return &heldDest{started: make(chan struct{}), release: make(chan struct{})}
}

func (d *heldDest) Write(p []byte) (int, error) {
	d.once.Do(func() { close(d.started) })
	select {
	case <-d.release:
		return len(p), nil
	case <-time.After(time.Minute): // bounds a regression; never reached when green
		return 0, errors.New("held too long")
	}
}

// TestDumpStreamsHaveTheirOwnBudget (M1-HIPAA2 item 1): with the default budget
// of 2 streams, two downloads that the browser does not read hold their slots;
// a third is refused at once with busy (not queued), logged path-free, and
// maintenance (here: snapshots) still runs while both streams are held. A slot
// freed by a finished stream can be used again.
func TestDumpStreamsHaveTheirOwnBudget(t *testing.T) {
	src, _ := realZip(t, map[string]string{"report.txt": strings.Repeat("quarterly report\n", 4096)})
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsDirJSON},
		{Cmd: "dump", StdoutFile: src},
		{Cmd: "snapshots", Stdout: "inline:[]"},
	}, nil)
	if e.w.cfg.MaxStreams != DefaultMaxStreams || DefaultMaxStreams != 2 {
		t.Fatalf("MaxStreams = %d, want the default 2", e.w.cfg.MaxStreams)
	}
	req := adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: dumpTree}

	held := []*heldDest{newHeldDest(), newHeldDest()}
	errs := make(chan error, len(held))
	for _, d := range held {
		go func(d *heldDest) {
			_, err := e.w.Dump(e.ctx, req, d)
			errs <- err
		}(d)
	}
	for i, d := range held {
		select {
		case <-d.started:
		case <-time.After(30 * time.Second):
			t.Fatalf("stream %d never started", i)
		}
	}

	t0 := time.Now()
	// The deadline only bounds a regression (a queued or running third stream).
	ctx3, cancel3 := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel3()
	third := newHeldDest()
	close(third.release) // a third stream that did run would complete
	_, err := e.w.Dump(ctx3, req, third)
	wantCode(t, err, adminpath.CodeBusy, "")
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("the third stream waited %v: it must be refused at once, not queued", d)
	}
	if !strings.Contains(e.logs.String(), "streams_busy") || strings.Contains(e.logs.String(), dumpTree) {
		t.Fatalf("the refusal is not logged path-free with class streams_busy:\n%s", e.logs.String())
	}

	// Maintenance keeps its slots while both streams are held.
	if _, err := e.w.Snapshots(e.ctx, dev(e.esc)); err != nil {
		t.Fatalf("snapshots while two streams are held: %v", err)
	}

	for _, d := range held {
		close(d.release)
	}
	for range held {
		if err := <-errs; err != nil {
			t.Fatalf("a held stream failed: %v", err)
		}
	}
	// The budget is free again.
	if _, err := e.w.Dump(e.ctx, req, &watchWriter{}); err != nil {
		t.Fatalf("a stream after the others finished: %v", err)
	}
}

// TestMaxStreamsMustStayBelowMaxConcurrent: a budget that could take every
// restic slot is a configuration error; a small MaxConcurrent lowers the
// default.
func TestMaxStreamsMustStayBelowMaxConcurrent(t *testing.T) {
	base := Config{Bucket: "b", ScratchDir: t.TempDir()}
	for _, tc := range []struct {
		maxConc, maxStreams, want int
		ok                        bool
	}{
		{0, 0, 2, true},
		{4, 3, 3, true},
		{2, 0, 1, true},
		{4, 4, 0, false},
		{3, 5, 0, false},
		{1, 0, 0, false},
	} {
		c := base
		c.MaxConcurrent, c.MaxStreams = tc.maxConc, tc.maxStreams
		got, err := c.withDefaults()
		switch {
		case tc.ok && err != nil:
			t.Errorf("MaxConcurrent %d MaxStreams %d: %v", tc.maxConc, tc.maxStreams, err)
		case tc.ok && got.MaxStreams != tc.want:
			t.Errorf("MaxConcurrent %d MaxStreams %d: got %d streams, want %d", tc.maxConc, tc.maxStreams, got.MaxStreams, tc.want)
		case !tc.ok && !errors.Is(err, errConfig):
			t.Errorf("MaxConcurrent %d MaxStreams %d: err = %v, want a config error", tc.maxConc, tc.maxStreams, err)
		}
	}
}
