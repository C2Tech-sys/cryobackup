package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/migrations"
)

// ---------------------------------------------------------------------------
// migrations
// ---------------------------------------------------------------------------

// DH20 (since 0002): replica/schema.sql is the current schema. The database
// Open builds by applying the embedded migrations must have the same schema as
// the one executing schema.sql gives. (internal/console/migrations compares the
// same two without the store; this is the path production takes.)
func TestOpenedSchemaMatchesSchemaSQL(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "replica", "schema.sql"))
	if err != nil {
		t.Fatalf("read replica/schema.sql: %v", err)
	}
	signature := func(q interface {
		Query(string, ...any) (*sql.Rows, error)
	}) []string {
		rows, err := q.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
		ok(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var typ, name, tbl, def string
			ok(t, rows.Scan(&typ, &name, &tbl, &def))
			// ALTER TABLE ... RENAME stores the new name quoted; nothing else differs.
			def = strings.Replace(def, `CREATE TABLE "runs"`, `CREATE TABLE runs`, 1)
			out = append(out, typ+" | "+name+" | "+tbl+" | "+def)
		}
		ok(t, rows.Err())
		return out
	}

	ref, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.db"))
	ok(t, err)
	defer ref.Close()
	ref.SetMaxOpenConns(1)
	if _, err := ref.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("executing replica/schema.sql: %v", err)
	}
	want := signature(ref)

	s, _ := newStore(t)
	var got []string
	ok(t, s.Read(ctx, func(tx *Tx) error {
		got = signature(tx)
		return nil
	}))
	if len(want) < 100 || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Open's schema (%d objects) differs from replica/schema.sql (%d objects): a schema change is a new migration AND the same change in schema.sql", len(got), len(want))
	}
	// 0001 is frozen on disk as well as embedded.
	disk, err := os.ReadFile(filepath.Join("..", "migrations", "0001_initial.sql"))
	if err != nil || migrations.Checksum(disk) != must[[]migrations.Migration](t)(migrations.List())[0].Checksum {
		t.Fatalf("migrations/0001_initial.sql on disk differs from the embedded copy (err=%v)", err)
	}
}

func countObjects(t *testing.T, s *Store) (tables, indexes int) {
	t.Helper()
	ok(t, s.Read(context.Background(), func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL`).Scan(&indexes)
	}))
	return tables, indexes
}

func TestOpenAppliesMigrationsAndReopenIsNoop(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "console.db")
	clk := newFakeClock(t0)
	s, err := Open(ctx, Options{Path: path, Clock: clk.Now})
	ok(t, err)
	tables, indexes := countObjects(t, s)
	if tables != 35 || indexes != 105 {
		t.Fatalf("schema has %d tables and %d indexes, want 35 and 105 (same as replica/schema_test.py)", tables, indexes)
	}
	if v := must[int](t)(s.SchemaVersion(ctx)); v != 6 {
		t.Fatalf("SchemaVersion = %d, want 6", v)
	}
	var name, sum string
	var created int64
	ok(t, s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT name, checksum, created_at FROM schema_migrations WHERE version = 1`).Scan(&name, &sum, &created)
	}))
	ms := must[[]migrations.Migration](t)(migrations.List())
	if name != "0001_initial" || sum != ms[0].Checksum || sum == "bootstrap" {
		t.Fatalf("schema_migrations row = (%q, %q); want (0001_initial, %s)", name, sum, ms[0].Checksum)
	}
	if created != t0.UnixMilli() {
		t.Fatalf("created_at = %d, want the injected clock %d", created, t0.UnixMilli())
	}
	var name2, sum2 string
	ok(t, s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT name, checksum FROM schema_migrations WHERE version = 2`).Scan(&name2, &sum2)
	}))
	if name2 != "0002_unsupported_file_class" || sum2 != ms[1].Checksum {
		t.Fatalf("schema_migrations row 2 = (%q, %q); want (0002_unsupported_file_class, %s)", name2, sum2, ms[1].Checksum)
	}
	ok(t, s.Close())

	// Re-open later: nothing is re-applied, so the applied-at stamp does not move.
	clk.Advance(48 * time.Hour)
	s2, err := Open(ctx, Options{Path: path, Clock: clk.Now})
	ok(t, err)
	defer s2.Close()
	var created2 int64
	ok(t, s2.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT created_at FROM schema_migrations WHERE version = 1`).Scan(&created2)
	}))
	if created2 != created {
		t.Fatalf("re-open re-applied the migration (created_at %d -> %d)", created, created2)
	}
	if tb, ix := countObjects(t, s2); tb != 35 || ix != 105 {
		t.Fatalf("after re-open: %d tables, %d indexes", tb, ix)
	}
}

func TestTamperedChecksumRefusesToOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "console.db")
	s, err := Open(ctx, Options{Path: path})
	ok(t, err)
	ok(t, s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE schema_migrations SET checksum = ? WHERE version = 1`, strings.Repeat("0", 64))
		return err
	}))
	ok(t, s.Close())
	s2, err := Open(ctx, Options{Path: path})
	if !errors.Is(err, ErrMigrationChanged) || !errors.Is(err, migrations.ErrMigrationChanged) {
		t.Fatalf("Open with a tampered checksum = %v, want ErrMigrationChanged", err)
	}
	if s2 != nil {
		t.Fatal("a store was returned alongside the error")
	}
}

func TestNewerDatabaseRefusesToOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "console.db")
	s, err := Open(ctx, Options{Path: path})
	ok(t, err)
	ok(t, s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO schema_migrations (version, name, checksum, created_at) VALUES (99, '0099_future', 'x', 1)`)
		return err
	}))
	ok(t, s.Close())
	if _, err := Open(ctx, Options{Path: path}); !errors.Is(err, ErrMigrationAhead) {
		t.Fatalf("Open of a newer database = %v, want ErrMigrationAhead", err)
	}
}

func TestMigrationsListAndChecksum(t *testing.T) {
	ms := must[[]migrations.Migration](t)(migrations.List())
	if len(ms) != 6 || ms[0].Version != 1 || ms[0].Name != "0001_initial" || ms[1].Version != 2 || ms[1].Name != "0002_unsupported_file_class" ||
		ms[2].Version != 3 || ms[2].Name != "0003_fleet_start_lease" || ms[3].Version != 4 || ms[3].Name != "0004_m2w1a" ||
		ms[4].Version != 5 || ms[4].Name != "0005_m3t3_view_audience" || ms[5].Version != 6 || ms[5].Name != "0006_m3t6_token_delete" {
		t.Fatalf("List = %+v", ms)
	}
	lf, crlf := migrations.Checksum([]byte("a\nb\n")), migrations.Checksum([]byte("a\r\nb\r\n"))
	if lf != crlf {
		t.Fatal("checksum must not depend on line endings")
	}
	if migrations.Checksum([]byte("a\nc\n")) == lf {
		t.Fatal("checksum ignores content")
	}
}

// ---------------------------------------------------------------------------
// open discipline
// ---------------------------------------------------------------------------

func TestPragmasOnWriterAndReaders(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	check := func(tx *Tx, who string) {
		t.Helper()
		var mode string
		var fk, busy, sync int
		ok(t, tx.QueryRow(`PRAGMA journal_mode`).Scan(&mode))
		ok(t, tx.QueryRow(`PRAGMA foreign_keys`).Scan(&fk))
		ok(t, tx.QueryRow(`PRAGMA busy_timeout`).Scan(&busy))
		ok(t, tx.QueryRow(`PRAGMA synchronous`).Scan(&sync))
		if mode != "wal" || fk != 1 || busy != 5000 || sync != 1 {
			t.Errorf("%s: journal_mode=%s foreign_keys=%d busy_timeout=%d synchronous=%d; want wal, 1, 5000, 1 (NORMAL)", who, mode, fk, busy, sync)
		}
	}
	ok(t, s.Write(ctx, func(tx *Tx) error { check(tx, "writer"); return nil }))
	// Hold several readers at once so more than one pooled connection is checked.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok(t, s.Read(ctx, func(tx *Tx) error {
				<-start
				check(tx, fmt.Sprintf("reader %d", i))
				return nil
			}))
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(start)
	wg.Wait()
}

func TestBusyTimeoutIsConfigurable(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "c.db"), BusyTimeout: 1500 * time.Millisecond, ReadPool: 2})
	ok(t, err)
	defer s.Close()
	var busy int
	ok(t, s.Write(ctx, func(tx *Tx) error { return tx.QueryRow(`PRAGMA busy_timeout`).Scan(&busy) }))
	if busy != 1500 {
		t.Fatalf("busy_timeout = %d, want 1500", busy)
	}
}

func TestReaderCannotWrite(t *testing.T) {
	s, _ := newStore(t)
	err := s.Read(context.Background(), func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO settings (key, value, created_at, updated_at) VALUES ('x', '1', 1, 1)`)
		return err
	})
	if err == nil {
		t.Fatal("a write through Read succeeded")
	}
}

func TestOpenRejectsBadOptions(t *testing.T) {
	ctx := context.Background()
	for _, p := range []string{"", "file:x.db", "x.db?mode=ro"} {
		if _, err := Open(ctx, Options{Path: p}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Open(%q) = %v, want ErrInvalid", p, err)
		}
	}
}

func TestOpenMemoryCleansUp(t *testing.T) {
	s, err := OpenMemory(context.Background(), nil)
	ok(t, err)
	path := s.Path()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("temp database missing: %v", err)
	}
	ok(t, s.Close())
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("temp directory survived Close: %v", err)
	}
	if err := s.Write(context.Background(), func(*Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestCorruptFileIsErrCorrupt(t *testing.T) {
	ctx := context.Background()
	t.Run("not a database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "c.db")
		ok(t, os.WriteFile(path, bytes.Repeat([]byte("this is not a sqlite database file. "), 600), 0o600))
		if _, err := Open(ctx, Options{Path: path}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open(garbage) = %v, want ErrCorrupt", err)
		}
	})
	t.Run("damaged pages", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "c.db")
		s, err := Open(ctx, Options{Path: path})
		ok(t, err)
		ok(t, s.Close())
		b, err := os.ReadFile(path)
		ok(t, err)
		if len(b) < 4096*8 {
			t.Skipf("database is only %d bytes", len(b))
		}
		for i := 4096 * 2; i < 4096*6; i++ { // trash interior pages, keep the header
			b[i] = 0xFF
		}
		ok(t, os.WriteFile(path, b, 0o600))
		_ = os.Remove(path + "-wal")
		_ = os.Remove(path + "-shm")
		if _, err := Open(ctx, Options{Path: path}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open(damaged) = %v, want ErrCorrupt", err)
		}
	})
}

// ---------------------------------------------------------------------------
// writer discipline
// ---------------------------------------------------------------------------

func TestNestedWriteIsAnError(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	err := s.Write(ctx, func(tx *Tx) error {
		// Even with the outer ctx captured (the easy mistake), this must not deadlock.
		return s.Write(ctx, func(*Tx) error { return nil })
	})
	if !errors.Is(err, ErrNestedWrite) {
		t.Fatalf("nested Write = %v, want ErrNestedWrite", err)
	}
	// The outer transaction rolled back and the writer is usable again.
	ok(t, s.Write(ctx, func(tx *Tx) error { return nil }))
}

func insertSetting(tx *Tx, key string) error {
	_, err := tx.Exec(`INSERT INTO settings (key, value, created_at, updated_at) VALUES (?, '1', 1, 1)`, key)
	return err
}

func countSettings(t *testing.T, s *Store, prefix string) int {
	t.Helper()
	var n int
	ok(t, s.Read(context.Background(), func(tx *Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE ?`, prefix+"%").Scan(&n)
	}))
	return n
}

// Two goroutines write 500 rows each through Write: no SQLITE_BUSY, no lost row.
// (CI runs this under -race; the one write connection is what makes it safe.)
func TestConcurrentWritersNeverBusy(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				key := fmt.Sprintf("w%d-%03d", g, i)
				if err := s.Write(ctx, func(tx *Tx) error { return insertSetting(tx, key) }); err != nil {
					errs <- fmt.Errorf("goroutine %d row %d: %w", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := countSettings(t, s, "w"); n != 1000 {
		t.Fatalf("%d rows written, want 1000", n)
	}
}

func TestWriteRollsBackOnErrorAndPanic(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := s.Write(ctx, func(tx *Tx) error {
		if err := insertSetting(tx, "rb-1"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write returned %v, want boom", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		_ = s.Write(ctx, func(tx *Tx) error {
			_ = insertSetting(tx, "rb-2")
			panic("kaboom")
		})
	}()
	if n := countSettings(t, s, "rb-"); n != 0 {
		t.Fatalf("%d rows survived a rolled-back Write", n)
	}
	// And the connection is healthy afterwards.
	ok(t, s.Write(ctx, func(tx *Tx) error { return insertSetting(tx, "rb-3") }))
	if n := countSettings(t, s, "rb-"); n != 1 {
		t.Fatalf("got %d rows, want 1", n)
	}
}

// A reader that began before a commit keeps a consistent snapshot of the old
// state for the whole transaction; a new reader sees the commit.
func TestReaderSeesConsistentSnapshotDuringWrite(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("snap-%d", i)
		ok(t, s.Write(ctx, func(tx *Tx) error { return insertSetting(tx, k) }))
	}
	readerStarted, writerDone := make(chan struct{}), make(chan struct{})
	var first, second int
	done := make(chan error, 1)
	go func() {
		done <- s.Read(ctx, func(tx *Tx) error {
			if err := tx.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'snap-%'`).Scan(&first); err != nil {
				return err
			}
			close(readerStarted)
			<-writerDone
			return tx.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'snap-%'`).Scan(&second)
		})
	}()
	<-readerStarted
	// While the reader is mid-transaction, a write transaction runs and commits (WAL: no blocking).
	ok(t, s.Write(ctx, func(tx *Tx) error {
		for i := 10; i < 20; i++ {
			if err := insertSetting(tx, fmt.Sprintf("snap-%d", i)); err != nil {
				return err
			}
		}
		return nil
	}))
	close(writerDone)
	ok(t, <-done)
	if first != 10 || second != 10 {
		t.Fatalf("reader saw %d then %d rows; a consistent snapshot sees 10 both times", first, second)
	}
	if n := countSettings(t, s, "snap-"); n != 20 {
		t.Fatalf("a new reader sees %d rows, want 20", n)
	}
}

// A reader never sees a half-committed multi-row write.
func TestReaderNeverSeesPartialWrite(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.Write(ctx, func(tx *Tx) error {
				for j := 0; j < 5; j++ {
					if err := insertSetting(tx, fmt.Sprintf("pw-%04d-%d", i, j)); err != nil {
						return err
					}
				}
				return nil
			})
		}
	}()
	for i := 0; i < 200; i++ {
		var n int
		ok(t, s.Read(ctx, func(tx *Tx) error {
			return tx.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'pw-%'`).Scan(&n)
		}))
		if n%5 != 0 {
			t.Fatalf("reader saw %d rows: a write of 5 was visible partially", n)
		}
	}
	close(stop)
	wg.Wait()
}

func TestWriteHonoursContextWhileQueued(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	hold, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = s.Write(ctx, func(tx *Tx) error {
			close(hold)
			<-release
			return nil
		})
	}()
	<-hold
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := s.Write(short, func(*Tx) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Write = %v, want DeadlineExceeded", err)
	}
	close(release)
}

// Direct driver access to the same file from a second connection (another
// process in production): a write while the console holds the write lock waits
// for busy_timeout and reports ErrBusy-class errors instead of corrupting.
func TestSecondWriterIsSerialisedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "c.db")
	s, err := Open(ctx, Options{Path: path, BusyTimeout: 200 * time.Millisecond})
	ok(t, err)
	defer s.Close()
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(200)")
	ok(t, err)
	defer other.Close()

	hold, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Write(ctx, func(tx *Tx) error {
			if err := insertSetting(tx, "held"); err != nil {
				return err
			}
			close(hold)
			<-release
			return nil
		})
	}()
	<-hold
	_, err = other.ExecContext(ctx, `INSERT INTO settings (key, value, created_at, updated_at) VALUES ('other', '1', 1, 1)`)
	if err == nil {
		t.Fatal("a second process wrote while the first held BEGIN IMMEDIATE")
	}
	if got := mapErr(err); !errors.Is(got, ErrBusy) {
		t.Fatalf("second writer error maps to %v, want ErrBusy", got)
	}
	close(release)
	ok(t, <-done)
	_, err = other.ExecContext(ctx, `INSERT INTO settings (key, value, created_at, updated_at) VALUES ('other', '1', 1, 1)`)
	ok(t, err)
}
