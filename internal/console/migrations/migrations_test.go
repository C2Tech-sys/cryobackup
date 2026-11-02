package migrations

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var fixedNow = func() time.Time { return time.Date(2026, 10, 22, 13, 42, 10, 0, time.UTC) }

func openConn(t *testing.T, path string) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return db, conn
}

func TestListIsContiguousAndOrdered(t *testing.T) {
	ms, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range ms {
		if m.Version != i+1 || m.Checksum == "" || m.SQL == "" {
			t.Fatalf("migration %d = %+v", i, m)
		}
	}
	if ms[0].Name != "0001_initial" {
		t.Fatalf("first migration = %s", ms[0].Name)
	}
}

func TestApplyIsIdempotentAndRecordsChecksum(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	applied, err := Apply(ctx, conn, fixedNow)
	if err != nil || len(applied) != 4 || applied[0] != 1 || applied[1] != 2 || applied[2] != 3 || applied[3] != 4 {
		t.Fatalf("first Apply = %v, %v", applied, err)
	}
	again, err := Apply(ctx, conn, fixedNow)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Apply = %v, %v; want a no-op", again, err)
	}
	var sum string
	if err := conn.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = 1`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	ms, _ := List()
	if sum != ms[0].Checksum {
		t.Fatalf("recorded checksum %s, want %s (the bootstrap marker must be overwritten)", sum, ms[0].Checksum)
	}
	if err := conn.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = 2`).Scan(&sum); err != nil || sum != ms[1].Checksum {
		t.Fatalf("version 2 checksum = %q, %v; want %s", sum, err, ms[1].Checksum)
	}
	if err := conn.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = 3`).Scan(&sum); err != nil || sum != ms[2].Checksum {
		t.Fatalf("version 3 checksum = %q, %v; want %s", sum, err, ms[2].Checksum)
	}
}

func TestChangedMigrationAndNewerDatabase(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := Apply(ctx, conn, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE schema_migrations SET checksum = 'edited' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, conn, fixedNow); !errors.Is(err, ErrMigrationChanged) {
		t.Fatalf("Apply after an edit = %v, want ErrMigrationChanged", err)
	}
	// The failed Apply left no transaction open on the connection.
	if _, err := conn.ExecContext(ctx, `UPDATE schema_migrations SET checksum = ? WHERE version = 1`, func() string { ms, _ := List(); return ms[0].Checksum }()); err != nil {
		t.Fatalf("connection unusable after ErrMigrationChanged: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, created_at) VALUES (7, '0007_future', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, conn, fixedNow); !errors.Is(err, ErrMigrationAhead) {
		t.Fatalf("Apply on a newer database = %v, want ErrMigrationAhead", err)
	}
}

func TestChecksumIgnoresLineEndings(t *testing.T) {
	if Checksum([]byte("a\r\nb\r\n")) != Checksum([]byte("a\nb\n")) {
		t.Fatal("CRLF changes the checksum")
	}
	if Checksum([]byte("a")) == Checksum([]byte("b")) {
		t.Fatal("different content, same checksum")
	}
}

// Two processes starting together: the loser of the BEGIN IMMEDIATE race finds the work done.
func TestConcurrentApplyFromTwoConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	_, c1 := openConn(t, path)
	_, c2 := openConn(t, path)
	var wg sync.WaitGroup
	results := make([][]int, 2)
	errs := make([]error, 2)
	for i, c := range []*sql.Conn{c1, c2} {
		wg.Add(1)
		go func(i int, c *sql.Conn) {
			defer wg.Done()
			results[i], errs[i] = Apply(ctx, c, fixedNow)
		}(i, c)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Apply #%d: %v", i, err)
		}
	}
	all := append(append([]int(nil), results[0]...), results[1]...)
	sort.Ints(all)
	if len(all) != 4 || all[0] != 1 || all[1] != 2 || all[2] != 3 || all[3] != 4 {
		t.Fatalf("migrations applied %v (%v, %v), want each of 1 to 4 exactly once", all, results[0], results[1])
	}
	var n int
	if err := c1.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("schema_migrations rows = %d, %v", n, err)
	}
}
