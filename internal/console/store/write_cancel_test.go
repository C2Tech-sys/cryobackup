package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// M3-STORE1 A: a Write whose context ends at any moment leaves the one write
// connection usable and the answer true. The driver can report a context error
// for a BEGIN IMMEDIATE (or COMMIT) that took effect, so these tests hammer the
// race: on the old code the very next plain write failed with "cannot start a
// transaction within a transaction". write_lying_driver_test.go stages the same
// answers deterministically, for a red that does not depend on the scheduler.

const (
	hammerBudget = 2 * time.Second // per subtest: the test's run time is bounded whatever the machine
	hammerMaxGap = 600 * time.Microsecond
	hammerRow    = "hammer"
)

// storeKinds runs a subtest on both ways to open a store.
func storeKinds(t *testing.T, f func(t *testing.T, s *Store)) {
	t.Run("OpenMemory", func(t *testing.T) {
		s, err := OpenMemory(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		f(t, s)
	})
	t.Run("file", func(t *testing.T) {
		s, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "console.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		f(t, s)
	})
}

// spin waits d without a timer, whose resolution on Windows is far coarser than the race being hit.
func spin(d time.Duration) {
	for start := time.Now(); time.Since(start) < d; {
	}
}

// hammerGap is 0-600 microseconds, mostly under 60: a statement takes tens of
// microseconds, so that is where a context ends inside BEGIN IMMEDIATE.
func hammerGap(i int) time.Duration {
	if i%8 == 0 {
		return time.Duration(i%int(hammerMaxGap/time.Microsecond+1)) * time.Microsecond
	}
	return time.Duration(i%60) * time.Microsecond
}

// byDeadline is a context with a deadline hammerGap away.
func byDeadline(i int) (context.Context, func()) {
	return context.WithTimeout(context.Background(), hammerGap(i))
}

// byCancel is a context another goroutine, already running, cancels hammerGap from now.
func byCancel(i int) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		started.Store(true)
		spin(hammerGap(i))
		cancel()
	}()
	for !started.Load() {
		runtime.Gosched()
	}
	return ctx, func() { <-done; cancel() }
}

// runCount is how many runs of the hammer row are committed.
func runCount(t *testing.T, s *Store) int {
	t.Helper()
	j, err := s.GetJobState(context.Background(), hammerRow)
	if errors.Is(err, ErrNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("read the hammer row: %v", err)
	}
	return j.RunCount
}

// hammerWrites issues a cancelled write and then an uncancelled one, until the
// budget ends. Every uncancelled write must succeed, and a cancelled write's
// answer must match the database: nil means committed, an error means not.
func hammerWrites(t *testing.T, s *Store, mkCtx func(int) (context.Context, func())) {
	t.Helper()
	bg := context.Background()
	run := JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}
	if err := s.RecordJobRun(bg, hammerRow, run); err != nil {
		t.Fatal(err)
	}
	want := runCount(t, s)
	refused, iter := 0, 0
	for end := time.Now().Add(hammerBudget); time.Now().Before(end); iter++ {
		ctx, done := mkCtx(iter)
		err := s.RecordJobRun(ctx, hammerRow, run)
		done()
		switch {
		case err == nil:
			want++
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			refused++
		default:
			t.Fatalf("iteration %d: the cancelled write answered %v, want nil or the context's error", iter, err)
		}
		if err := s.RecordJobRun(bg, hammerRow, run); err != nil {
			t.Fatalf("iteration %d: the next uncancelled write after %d cancelled ones failed: %v", iter, refused, err)
		}
		want++
		if got := runCount(t, s); got != want {
			t.Fatalf("iteration %d: %d runs committed, the answers say %d: a write's answer does not match the database", iter, got, want)
		}
	}
	if refused == 0 {
		t.Fatalf("no write was cancelled in %d iterations: the test did not exercise the race", iter)
	}
	t.Logf("%d iterations, %d writes refused", iter, refused)
}

// hammerBegin is the fast loop that hits the wedge itself: writes that change
// nothing cost tens of microseconds, not a WAL append, so tens of thousands of
// cancelled BEGINs fit the budget.
func hammerBegin(t *testing.T, s *Store, mkCtx func(int) (context.Context, func())) {
	t.Helper()
	bg := context.Background()
	noop := func(*Tx) error { return nil }
	refused, iter := 0, 0
	for end := time.Now().Add(hammerBudget); time.Now().Before(end); iter++ {
		ctx, done := mkCtx(iter)
		err := s.Write(ctx, noop)
		done()
		if err != nil {
			refused++
		}
		if err := s.Write(bg, noop); err != nil {
			t.Fatalf("iteration %d: the next uncancelled write after %d cancelled ones failed: %v", iter, refused, err)
		}
	}
	if refused == 0 {
		t.Fatalf("no write was cancelled in %d iterations: the test did not exercise the race", iter)
	}
	t.Logf("%d iterations, %d writes refused", iter, refused)
}

func TestWriteNextWriteSucceedsAfterDeadlines(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) { hammerBegin(t, s, byDeadline) })
}

func TestWriteNextWriteSucceedsAfterCancelFromAnotherGoroutine(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) { hammerBegin(t, s, byCancel) })
}

func TestWriteAnswerMatchesTheDatabaseUnderDeadlines(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) { hammerWrites(t, s, byDeadline) })
}

func TestWriteAnswerMatchesTheDatabaseUnderCancelFromAnotherGoroutine(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) { hammerWrites(t, s, byCancel) })
}

// The read pool: a Read whose context ends at any moment leaves no pooled
// connection inside a transaction. A connection left in one would fail its next
// BEGIN, or serve a stale snapshot, so every pooled connection is exercised at
// once after each round and must see the newest commit.
func TestReadPoolSurvivesCancelledReads(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) {
		bg := context.Background()
		run := JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}
		cancelled, iter := 0, 0
		for end := time.Now().Add(hammerBudget); time.Now().Before(end); iter++ {
			for k := 0; k < 32; k++ {
				mk := byDeadline
				if k%2 == 0 {
					mk = byCancel
				}
				ctx, done := mk(iter*32 + k)
				_, err := s.SchemaVersion(ctx)
				done()
				if err != nil {
					cancelled++
				}
			}
			if err := s.RecordJobRun(bg, hammerRow, run); err != nil {
				t.Fatal(err)
			}
			want := runCount(t, s)
			var ready, failed atomic.Int64
			release := make(chan struct{})
			var wg sync.WaitGroup
			for range defaultReadPool {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := s.Read(bg, func(tx *Tx) error {
						var n int
						if err := tx.QueryRow(`SELECT run_count FROM job_state WHERE name = ?`, hammerRow).Scan(&n); err != nil {
							return err
						}
						if n != want {
							return errors.New("stale snapshot")
						}
						if ready.Add(1) == defaultReadPool {
							close(release)
						}
						select { // hold this connection until the whole pool is in use
						case <-release:
						case <-time.After(5 * time.Second):
							return errors.New("the pool was not fully used")
						}
						return nil
					})
					if err != nil {
						failed.Add(1)
						t.Errorf("iteration %d: a pooled read after cancelled reads: %v", iter, err)
					}
				}()
			}
			wg.Wait()
			if failed.Load() > 0 {
				t.FailNow()
			}
		}
		if cancelled == 0 {
			t.Fatalf("no read was cancelled in %d rounds: the test did not exercise the race", iter)
		}
		t.Logf("%d rounds, %d reads cancelled", iter, cancelled)
	})
}

// fn succeeds but the context ended before COMMIT: nothing is committed and
// the answer says so, whether or not fn noticed.
func TestWriteContextEndedBeforeCommitCommitsNothing(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) {
		bg := context.Background()
		ctx, cancel := context.WithCancel(bg)
		err := s.Write(ctx, func(tx *Tx) error {
			if _, err := tx.Exec(`INSERT INTO job_state (name, created_at, updated_at) VALUES ('uncommitted', 1, 1)`); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Write = %v, want context.Canceled", err)
		}
		if _, err := s.GetJobState(bg, "uncommitted"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the row of a refused write: %v, want not found", err)
		}
		if err := s.RecordJobRun(bg, hammerRow, JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}); err != nil {
			t.Fatalf("the next write: %v", err)
		}
	})
}

// A COMMIT that fails leaves the transaction open: deferred foreign keys fail
// at COMMIT, not at the statement. It is rolled back and the connection is usable.
func TestWriteFailedCommitRollsBack(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) {
		bg := context.Background()
		fnPassed := false
		err := s.Write(bg, func(tx *Tx) error {
			if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO job_state (name, created_at, updated_at) VALUES ('before-fk', 1, 1)`); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO customers (id, name, slug, storage_target_id, created_at, updated_at)
				VALUES ('01HZZZZZZZZZZZZZZZZZZZZZZZ', 'c', 'c', 'no-such-target', 1, 1)`); err != nil {
				return err
			}
			fnPassed = true
			return nil
		})
		if !fnPassed {
			t.Fatalf("fn failed before COMMIT: %v; the test must reach a failing COMMIT", err)
		}
		if err == nil {
			t.Fatal("a COMMIT with a broken foreign key succeeded")
		}
		if _, gerr := s.GetJobState(bg, "before-fk"); !errors.Is(gerr, ErrNotFound) {
			t.Fatalf("after the failed COMMIT (%v) the row: %v, want not found", err, gerr)
		}
		if err := s.RecordJobRun(bg, hammerRow, JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}); err != nil {
			t.Fatalf("the next write after a failed COMMIT: %v", err)
		}
	})
}

// A panic in fn rolls back, is re-raised, and leaves the connection usable.
func TestWritePanicRollsBackAndTheNextWriteWorks(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) {
		bg := context.Background()
		func() {
			defer func() {
				if p := recover(); p != "boom" {
					t.Fatalf("recovered %v, want the panic re-raised", p)
				}
			}()
			_ = s.Write(bg, func(tx *Tx) error {
				if _, err := tx.Exec(`INSERT INTO job_state (name, created_at, updated_at) VALUES ('panicked', 1, 1)`); err != nil {
					return err
				}
				panic("boom")
			})
		}()
		if _, err := s.GetJobState(bg, "panicked"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the row of a panicked write: %v, want not found", err)
		}
		if err := s.RecordJobRun(bg, hammerRow, JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}); err != nil {
			t.Fatalf("the next write after a panic: %v", err)
		}
	})
}

// failingRollback is the write connection with a ROLLBACK that fails as err says.
type failingRollback struct {
	*sql.Conn
	err  error
	raws int
}

func (f *failingRollback) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if strings.EqualFold(q, "ROLLBACK") && f.err != nil {
		return nil, f.err
	}
	return f.Conn.ExecContext(ctx, q, args...)
}

func (f *failingRollback) Raw(fn func(any) error) error {
	f.raws++
	return f.Conn.Raw(fn)
}

// abandonWrite: nothing to roll back keeps the connection; a real rollback
// failure discards it, and the next write opens a fresh one. No store loses
// data by that, OpenMemory included: it is a temporary file, not ":memory:".
func TestAbandonWriteDiscardsOnlyOnARealRollbackFailure(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) {
		bg := context.Background()
		run := JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}
		if err := s.RecordJobRun(bg, hammerRow, run); err != nil {
			t.Fatal(err)
		}
		borrow := func() *sql.Conn {
			t.Helper()
			c, err := s.wdb.Conn(bg)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}

		// No transaction open: the driver's real "no transaction is active" is success.
		c := borrow()
		f := &failingRollback{Conn: c}
		abandonWrite(bg, f)
		if f.raws != 0 {
			t.Fatal("a connection with no transaction open was discarded")
		}
		_ = c.Close()

		// A transaction open and a clean ROLLBACK: it ends the transaction and keeps the connection.
		c = borrow()
		if _, err := c.ExecContext(bg, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		f = &failingRollback{Conn: c}
		abandonWrite(bg, f)
		if f.raws != 0 {
			t.Fatal("a clean ROLLBACK discarded the connection")
		}
		if _, err := c.ExecContext(bg, "BEGIN IMMEDIATE"); err != nil {
			t.Fatalf("the transaction was not ended: %v", err)
		}
		if _, err := c.ExecContext(bg, "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
		_ = c.Close()

		// ROLLBACK fails for any other reason: the transaction may be open, so the connection goes.
		c = borrow()
		if _, err := c.ExecContext(bg, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		f = &failingRollback{Conn: c, err: errors.New("disk I/O error")}
		abandonWrite(bg, f)
		if f.raws != 1 {
			t.Fatalf("a failed ROLLBACK called Raw %d times, want the one discard", f.raws)
		}
		if _, err := c.ExecContext(bg, "SELECT 1"); !errors.Is(err, sql.ErrConnDone) {
			t.Fatalf("the discarded connection still answers: %v", err)
		}
		_ = c.Close()

		// The database is intact and the store writes on a fresh connection.
		before := runCount(t, s)
		if err := s.RecordJobRun(bg, hammerRow, run); err != nil {
			t.Fatalf("the write after a discard: %v", err)
		}
		if got := runCount(t, s); got != before+1 || before < 1 {
			t.Fatalf("runs %d -> %d after a discard, want the data kept and one more", before, got)
		}
	})
}
