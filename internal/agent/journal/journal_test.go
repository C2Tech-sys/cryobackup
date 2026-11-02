package journal

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func open(t *testing.T, path string, c *clock) *Journal {
	t.Helper()
	j, err := Open(context.Background(), Options{Path: path, Clock: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
}

// ids are valid 26-character ULIDs (the wire validator is strict about them).
func id(n int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := []byte("01JA0000000000000000000000")
	b[25] = alphabet[n%32]
	b[24] = alphabet[(n/32)%32]
	return string(b)
}

func openRec(runID string, started time.Time) api.RunRecord {
	return api.RunRecord{
		ID: runID, Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: started,
		Attempt: 1, PolicyVersion: 3, AgentVersion: "test", Initial: true,
	}
}

func closed(rec api.RunRecord, at time.Time, outcome restic.Outcome) api.RunRecord {
	rec.FinishedAt = &at
	rec.Outcome = outcome
	zero := 0
	rec.ExitCode = &zero
	if outcome == restic.OutcomeSuccess {
		rec.SnapshotID = strings.Repeat("ab", 32)
		rec.Stats.TotalFilesProcessed = 5
	}
	return rec
}

func TestOpenCloseReopenKeepsRows(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	path := filepath.Join(t.TempDir(), "journal.db")
	j, err := Open(ctx, Options{Path: path, Clock: c.now})
	if err != nil {
		t.Fatal(err)
	}
	rec := openRec(id(1), c.now())
	if err := j.StartRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if err := j.FinishRun(ctx, closed(rec, c.now(), restic.OutcomeSuccess)); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	j2 := open(t, path, c)
	row, ok, err := j2.Run(ctx, id(1))
	if err != nil || !ok {
		t.Fatalf("run after reopen: %v %v", ok, err)
	}
	if row.Outcome != restic.OutcomeSuccess || row.State != StateDone || row.SnapshotID == "" || !row.Initial {
		t.Errorf("row = %+v", row)
	}
	if has, _ := j2.HasSnapshot(ctx); !has {
		t.Error("HasSnapshot = false after a successful run")
	}
}

func TestStaleOpenRunsAreClosed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		interrupted bool
		want        restic.Outcome
	}{
		{"no stop recorded: abandoned", false, restic.OutcomeAbandoned},
		{"stop recorded: interrupted", true, restic.OutcomeInterrupted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClock()
			path := filepath.Join(t.TempDir(), "journal.db")
			j := open(t, path, c)
			started := c.now()
			if err := j.StartRun(ctx, openRec(id(7), started)); err != nil {
				t.Fatal(err)
			}
			c.advance(5 * time.Minute)
			if err := j.MarkRunning(ctx, id(7)); err != nil {
				t.Fatal(err)
			}
			c.advance(10 * time.Minute)
			if err := j.Touch(ctx, id(7)); err != nil { // the last sign of life
				t.Fatal(err)
			}
			lastActivity := c.now()
			c.advance(2 * time.Hour) // the process died, and the machine was off

			closedRuns, err := j.OpenRunsOnStart(ctx, tc.interrupted, "test")
			if err != nil || len(closedRuns) != 1 {
				t.Fatalf("closed %d runs: %v", len(closedRuns), err)
			}
			row, _, _ := j.Run(ctx, id(7))
			if row.Outcome != tc.want || row.FinishedAt == nil {
				t.Fatalf("row = %+v", row)
			}
			if !row.FinishedAt.Equal(lastActivity) {
				t.Errorf("finished_at = %v, want the last journal activity %v", row.FinishedAt, lastActivity)
			}
			if n, _ := j.OpenRunCount(ctx); n != 0 {
				t.Errorf("%d runs still open", n)
			}
			// The closed record is queued for upload, as a closed body.
			items, _ := j.OutboxDue(ctx, c.now(), 10)
			if len(items) != 1 || !items[0].Closed || items[0].Body.Outcome != tc.want {
				t.Fatalf("outbox = %+v", items)
			}
			if err := items[0].Body.Validate(); err != nil {
				t.Errorf("the closed record does not validate: %v", err)
			}
		})
	}
}

func TestOutboxAtLeastOnce(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	rec := openRec(id(2), c.now())
	if err := j.StartRun(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// The open record goes out at start.
	items, err := j.OutboxDue(ctx, c.now(), 10)
	if err != nil || len(items) != 1 || items[0].Closed {
		t.Fatalf("open outbox = %+v %v", items, err)
	}
	openSeq := items[0].Seq
	if depth, _ := j.OutboxDepth(ctx); depth != 0 {
		t.Errorf("depth counts only closed results, got %d", depth)
	}

	// The run closes while that delivery is in flight: the closed body replaces
	// the open one (new sequence), so the late "delivered" for the old body must
	// not delete it.
	c.advance(time.Minute)
	if err := j.FinishRun(ctx, closed(rec, c.now(), restic.OutcomeSuccess)); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkDelivered(ctx, id(2), openSeq); err != nil {
		t.Fatal(err)
	}
	items, _ = j.OutboxDue(ctx, c.now(), 10)
	if len(items) != 1 || !items[0].Closed || items[0].Seq == openSeq {
		t.Fatalf("the closed body must survive a late delivery of the open one: %+v", items)
	}
	if row, _, _ := j.Run(ctx, id(2)); row.DeliveredAt != nil {
		t.Error("delivered_at set by the delivery of the open body")
	}

	// A network failure: the entry waits, attempts count, nothing is lost.
	retryAt := c.now().Add(2 * time.Minute)
	if err := j.OutboxFailed(ctx, id(2), items[0].Seq, retryAt); err != nil {
		t.Fatal(err)
	}
	if due, _ := j.OutboxDue(ctx, c.now(), 10); len(due) != 0 {
		t.Error("an entry in backoff was offered again")
	}
	if next, ok, _ := j.OutboxNext(ctx); !ok || !next.Equal(retryAt) {
		t.Errorf("OutboxNext = %v %v, want %v", next, ok, retryAt)
	}
	c.advance(3 * time.Minute)
	due, _ := j.OutboxDue(ctx, c.now(), 10)
	if len(due) != 1 || due[0].Attempts != 1 {
		t.Fatalf("after the backoff: %+v", due)
	}

	// The console answers "duplicate" (it had the record already): delivered.
	if err := j.MarkDelivered(ctx, id(2), due[0].Seq); err != nil {
		t.Fatal(err)
	}
	if due, _ := j.OutboxDue(ctx, c.now(), 10); len(due) != 0 {
		t.Error("a delivered entry was offered again")
	}
	row, _, _ := j.Run(ctx, id(2))
	if row.DeliveredAt == nil {
		t.Error("delivered_at not set after the closed record was delivered")
	}
	// Delivering twice is harmless (at-least-once on the wire, idempotent here).
	if err := j.MarkDelivered(ctx, id(2), due0Seq(t, items)); err != nil {
		t.Fatal(err)
	}
}

func due0Seq(t *testing.T, items []OutboxItem) int64 {
	t.Helper()
	return items[0].Seq
}

func TestRejectedIsJournaledAndNotRetried(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	rec := openRec(id(3), c.now())
	if err := j.StartRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if err := j.FinishRun(ctx, closed(rec, c.now(), restic.OutcomeFailed)); err != nil {
		t.Fatal(err)
	}
	items, _ := j.OutboxDue(ctx, c.now(), 10)
	if err := j.MarkRejected(ctx, id(3), items[0].Seq, api.ReasonInvalid); err != nil {
		t.Fatal(err)
	}
	if due, _ := j.OutboxDue(ctx, c.now().Add(24*time.Hour), 10); len(due) != 0 {
		t.Error("a rejected record is retried")
	}
	row, _, _ := j.Run(ctx, id(3))
	if row.RejectReason != api.ReasonInvalid || row.DeliveredAt != nil {
		t.Errorf("row = %+v", row)
	}
}

func TestInsertFinishedQueuesAClosedRecord(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	now := c.now()
	rec := openRec(id(4), now)
	rec.Trigger = api.TriggerManual
	rec.Initial = false
	rec.FinishedAt, rec.Outcome = &now, restic.OutcomeSkipped
	if err := j.InsertFinished(ctx, rec); err != nil {
		t.Fatal(err)
	}
	items, _ := j.OutboxDue(ctx, now, 10)
	if len(items) != 1 || !items[0].Closed || items[0].Body.Outcome != restic.OutcomeSkipped {
		t.Fatalf("outbox = %+v", items)
	}
	if err := items[0].Body.Validate(); err != nil {
		t.Errorf("skipped record does not validate: %v", err)
	}
}

func TestOutboxRefusesAPathBearingRecord(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	rec := openRec(id(5), c.now())
	rec.AgentVersion = `C:\Users\PHI-sentinel\secret.docx`
	err := j.StartRun(ctx, rec)
	if err == nil || strings.Contains(err.Error(), "PHI-sentinel") {
		t.Fatalf("err = %v: want a refusal that does not echo the value", err)
	}
	if n, _ := j.OpenRunCount(ctx); n != 0 {
		t.Error("a refused record left a run row")
	}
}

func TestKVRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	if _, ok, err := j.KVGet(ctx, "k"); ok || err != nil {
		t.Fatalf("missing key: %v %v", ok, err)
	}
	for _, v := range []string{"one", "two", ""} {
		if err := j.KVSet(ctx, "k", v); err != nil {
			t.Fatal(err)
		}
		got, ok, err := j.KVGet(ctx, "k")
		if err != nil || !ok || got != v {
			t.Fatalf("kv = %q %v %v, want %q", got, ok, err, v)
		}
	}
	if err := j.KVDelete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := j.KVGet(ctx, "k"); ok {
		t.Error("deleted key still there")
	}
}

func TestCommandLog(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	row, created, err := j.RecordCommand(ctx, id(9), "backup_now", nil)
	if err != nil || !created || row.Status != CommandReceived {
		t.Fatalf("record: %+v %v %v", row, created, err)
	}
	if _, created, _ := j.RecordCommand(ctx, id(9), "backup_now", nil); created {
		t.Error("a repeated command id was recorded as new")
	}
	if un, _ := j.UnpostedCommands(ctx); len(un) != 0 {
		t.Error("a command still running is not an unposted result")
	}
	if err := j.FinishCommand(ctx, id(9), "succeeded", "", []byte(`{"started":true}`)); err != nil {
		t.Fatal(err)
	}
	un, _ := j.UnpostedCommands(ctx)
	if len(un) != 1 || un[0].Result != `{"started":true}` {
		t.Fatalf("unposted = %+v", un)
	}
	if err := j.MarkCommandPosted(ctx, id(9)); err != nil {
		t.Fatal(err)
	}
	if un, _ := j.UnpostedCommands(ctx); len(un) != 0 {
		t.Error("a posted result is offered again")
	}
}

func TestConcurrentWritersDoNotError(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				n := w*20 + i + 100
				rec := openRec(id(n), c.now())
				if err := j.StartRun(ctx, rec); err != nil {
					errs <- err
					return
				}
				if err := j.KVSet(ctx, "k", "v"); err != nil {
					errs <- err
					return
				}
				if err := j.FinishRun(ctx, closed(rec, c.now(), restic.OutcomeSuccess)); err != nil {
					errs <- err
					return
				}
				if _, err := j.OutboxDue(ctx, c.now(), 5); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write failed: %v", err)
	}
	items, _ := j.OutboxDue(ctx, c.now(), 1000)
	if len(items) != 160 {
		t.Errorf("outbox holds %d entries, want 160", len(items))
	}
}

func TestTwoHandlesShareTheFile(t *testing.T) {
	// The service and a one-shot "check-in" are two processes on one file;
	// two handles in one process exercise the same SQLite lock.
	ctx := context.Background()
	c := newClock()
	path := filepath.Join(t.TempDir(), "journal.db")
	a, b := open(t, path, c), open(t, path, c)
	if err := a.KVSet(ctx, "x", "from-a"); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := b.KVGet(ctx, "x"); !ok || v != "from-a" {
		t.Fatalf("second handle read %q %v", v, ok)
	}
	var wg sync.WaitGroup
	for i, j := range []*Journal{a, b} {
		wg.Add(1)
		go func(i int, j *Journal) {
			defer wg.Done()
			for n := 0; n < 30; n++ {
				if err := j.KVSet(ctx, "y", "v"); err != nil {
					t.Errorf("handle %d: %v", i, err)
					return
				}
			}
		}(i, j)
	}
	wg.Wait()
}

func TestReadOnlyOpenDoesNotMigrate(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	path := filepath.Join(t.TempDir(), "journal.db")
	j := open(t, path, c)
	if err := j.KVSet(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(ctx, Options{Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if v, ok, _ := ro.KVGet(ctx, "k"); !ok || v != "v" {
		t.Fatalf("read-only read = %q %v", v, ok)
	}
	if err := ro.KVSet(ctx, "k2", "v"); err == nil {
		t.Error("a read-only handle accepted a write")
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	path := filepath.Join(t.TempDir(), "journal.db")
	j := open(t, path, c)
	if _, err := j.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	if _, err := Open(ctx, Options{Path: path, Clock: c.now}); !errors.Is(err, ErrNewer) {
		t.Fatalf("err = %v, want ErrNewer", err)
	}
}

func TestRecentRunsOrderAndKind(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	for i := 0; i < 3; i++ {
		rec := openRec(id(20+i), c.now())
		if err := j.StartRun(ctx, rec); err != nil {
			t.Fatal(err)
		}
		c.advance(time.Minute)
		if err := j.FinishRun(ctx, closed(rec, c.now(), restic.OutcomeFailed)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := j.RecentRuns(ctx, restic.KindBackup, 2)
	if err != nil || len(rows) != 2 || rows[0].ID != id(22) || rows[1].ID != id(21) {
		t.Fatalf("rows = %+v %v", rows, err)
	}
	if rows, _ := j.RecentRuns(ctx, restic.KindRestore, 5); len(rows) != 0 {
		t.Error("kind filter ignored")
	}
}
