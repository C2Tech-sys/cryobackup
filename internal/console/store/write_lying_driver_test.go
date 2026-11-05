package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
)

// lyingConn makes deterministic what the driver does when a context ends just
// before a statement's first step: the statement runs to completion, and the
// call answers the context's error (modernc.org/sqlite, stmt.exec). The
// connection stays valid, so database/sql keeps it. Every other statement gets the
// caller's context, so one that has ended really stops a later ROLLBACK.
type lyingConn struct {
	driver.Conn
	d *lyingDriver
}

func (c *lyingConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	exec := c.Conn.(driver.ExecerContext)
	lie := c.d.lie.Load()
	if lie == nil || *lie != q || ctx.Done() == nil {
		return exec.ExecContext(ctx, q, args)
	}
	r, err := exec.ExecContext(context.Background(), q, args)
	if err != nil {
		return r, err
	}
	if end := c.d.endCtx.Load(); end != nil {
		(*end)() // the caller's context has ended by the time the answer comes back
	}
	return nil, context.Canceled
}

func (c *lyingConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func (c *lyingConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

func (c *lyingConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

// lyingDriver is what a test controls: the statement to lie about, what ends the
// caller's context when it does, and how many connections the pool has opened.
type lyingDriver struct {
	lie      atomic.Pointer[string]
	endCtx   atomic.Pointer[context.CancelFunc]
	connects atomic.Int64
}

type lyingConnector struct {
	drv  driver.Driver
	name string
	d    *lyingDriver
}

func (l lyingConnector) Connect(context.Context) (driver.Conn, error) {
	c, err := l.drv.Open(l.name)
	if err != nil {
		return nil, err
	}
	l.d.connects.Add(1)
	return &lyingConn{Conn: c, d: l.d}, nil
}

func (l lyingConnector) Driver() driver.Driver { return l.drv }

// withLyingDriver puts the store's write pool behind lyingConn.
func withLyingDriver(t *testing.T, s *Store) *lyingDriver {
	t.Helper()
	d := &lyingDriver{}
	old := s.wdb
	db := sql.OpenDB(lyingConnector{drv: old.Driver(), name: dsn(s.path, defaultBusyTimeout, false), d: d})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s.wdb = db
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	return d
}

// A BEGIN IMMEDIATE that took effect but answered the context's error: the
// connection does not go back to the pool inside the transaction.
func TestWriteBeginThatTookEffectButAnsweredAnError(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) {
		d := withLyingDriver(t, s)
		begin := "BEGIN IMMEDIATE"
		d.lie.Store(&begin)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ran := false
		err := s.Write(ctx, func(*Tx) error { ran = true; return nil })
		if !errors.Is(err, context.Canceled) || ran {
			t.Fatalf("Write = %v, fn ran %v; want context.Canceled and no fn", err, ran)
		}
		d.lie.Store(nil)
		if err := s.RecordJobRun(context.Background(), hammerRow, JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}); err != nil {
			t.Fatalf("the next uncancelled write: %v", err)
		}
		if got := runCount(t, s); got != 1 {
			t.Fatalf("%d runs committed, want 1", got)
		}
	})
}

// COMMIT runs on a context that cannot end, so a Write that committed never
// answers an error: a caller's retry cannot double the work.
func TestWriteCommitAnswerIsTheTruth(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) {
		d := withLyingDriver(t, s)
		commit := "COMMIT"
		d.lie.Store(&commit)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := s.RecordJobRun(ctx, hammerRow, JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0})
		d.lie.Store(nil)
		got := runCount(t, s)
		if (err == nil) != (got == 1) {
			t.Fatalf("Write answered %v and %d runs are committed", err, got)
		}
		if err != nil {
			t.Fatalf("Write = %v, want the commit to run and answer nil", err)
		}
	})
}

// Mutant m03: ROLLBACK runs on a context that cannot end. The context here has
// really ended when the rollback runs, so a rollback under it would fail, and the
// connection would be discarded where it should have been kept. The pool's count of
// connections tells the two apart.
func TestWriteRollbackRunsAfterTheContextEnded(t *testing.T) { // M3-STORE2 2
	bg := context.Background()
	run := JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}
	wantKept := func(t *testing.T, s *Store, d *lyingDriver) {
		t.Helper()
		if err := s.RecordJobRun(bg, hammerRow, run); err != nil {
			t.Fatalf("the next write: %v", err)
		}
		if n := d.connects.Load(); n != 1 {
			t.Fatalf("%d connections opened: the rollback did not run and the connection was discarded", n)
		}
	}
	t.Run("the context ends inside fn", func(t *testing.T) {
		storeKinds(t, func(t *testing.T, s *Store) {
			d := withLyingDriver(t, s)
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			if err := s.RecordJobRun(bg, hammerRow, run); err != nil {
				t.Fatal(err)
			}
			err := s.Write(ctx, func(tx *Tx) error {
				if _, err := tx.Exec(`INSERT INTO job_state (name, created_at, updated_at) VALUES ('rolled-back', 1, 1)`); err != nil {
					return err
				}
				cancel()
				return ctx.Err()
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Write = %v, want context.Canceled", err)
			}
			if _, err := s.GetJobState(bg, "rolled-back"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("the row of the rolled-back write: %v, want not found", err)
			}
			wantKept(t, s, d)
		})
	})
	t.Run("the context ends as BEGIN answers", func(t *testing.T) {
		storeKinds(t, func(t *testing.T, s *Store) {
			d := withLyingDriver(t, s)
			if err := s.RecordJobRun(bg, hammerRow, run); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			end := context.CancelFunc(cancel)
			d.endCtx.Store(&end)
			begin := "BEGIN IMMEDIATE"
			d.lie.Store(&begin)
			if err := s.Write(ctx, func(*Tx) error { return nil }); !errors.Is(err, context.Canceled) {
				t.Fatalf("Write = %v, want context.Canceled", err)
			}
			d.lie.Store(nil)
			wantKept(t, s, d)
		})
	})
}
