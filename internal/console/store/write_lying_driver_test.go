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
// connection stays valid, so database/sql keeps it. lie names the statement.
type lyingConn struct {
	driver.Conn
	lie *atomic.Pointer[string]
}

func (c *lyingConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.Conn.(driver.ExecerContext).ExecContext(context.Background(), q, args)
	if lie := c.lie.Load(); lie != nil && *lie == q && err == nil && ctx.Done() != nil {
		return nil, context.Canceled
	}
	return r, err
}

func (c *lyingConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func (c *lyingConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

func (c *lyingConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

type lyingConnector struct {
	drv  driver.Driver
	name string
	lie  *atomic.Pointer[string]
}

func (l lyingConnector) Connect(context.Context) (driver.Conn, error) {
	c, err := l.drv.Open(l.name)
	if err != nil {
		return nil, err
	}
	return &lyingConn{Conn: c, lie: l.lie}, nil
}

func (l lyingConnector) Driver() driver.Driver { return l.drv }

// withLyingDriver puts the store's write pool behind lyingConn.
func withLyingDriver(t *testing.T, s *Store) *atomic.Pointer[string] {
	t.Helper()
	lie := &atomic.Pointer[string]{}
	old := s.wdb
	db := sql.OpenDB(lyingConnector{drv: old.Driver(), name: dsn(s.path, defaultBusyTimeout, false), lie: lie})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s.wdb = db
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	return lie
}

// A BEGIN IMMEDIATE that took effect but answered the context's error: the
// connection does not go back to the pool inside the transaction.
func TestWriteBeginThatTookEffectButAnsweredAnError(t *testing.T) { // M3-STORE1 A
	storeKinds(t, func(t *testing.T, s *Store) {
		lie := withLyingDriver(t, s)
		begin := "BEGIN IMMEDIATE"
		lie.Store(&begin)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ran := false
		err := s.Write(ctx, func(*Tx) error { ran = true; return nil })
		if !errors.Is(err, context.Canceled) || ran {
			t.Fatalf("Write = %v, fn ran %v; want context.Canceled and no fn", err, ran)
		}
		lie.Store(nil)
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
		lie := withLyingDriver(t, s)
		commit := "COMMIT"
		lie.Store(&commit)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := s.RecordJobRun(ctx, hammerRow, JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0})
		lie.Store(nil)
		got := runCount(t, s)
		if (err == nil) != (got == 1) {
			t.Fatalf("Write answered %v and %d runs are committed", err, got)
		}
		if err != nil {
			t.Fatalf("Write = %v, want the commit to run and answer nil", err)
		}
	})
}
