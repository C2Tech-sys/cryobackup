package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // the pure-Go driver, registered as "sqlite"

	"github.com/c2tech-sys/cryobackup/internal/console/migrations"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// Options configures Open.
type Options struct {
	// Path is the database file; it is created if missing. Required.
	Path string
	// Clock supplies every timestamp the store writes (created_at, updated_at,
	// ULID time). Nil means time.Now. Tests inject a frozen clock.
	Clock func() time.Time
	// BusyTimeout is how long SQLite waits for another process's write lock
	// before returning ErrBusy. Zero means 5 s.
	BusyTimeout time.Duration
	// ReadPool is the number of read connections. Zero means 4.
	ReadPool int
}

// Store is the console's database. Create one with Open (or OpenMemory) and
// share it for the life of the process; it is safe for concurrent use.
type Store struct {
	path   string
	clock  func() time.Time
	ids    *ulid.Generator
	wdb    *sql.DB // exactly one connection: the process's only writer
	rdb    *sql.DB // read pool, query_only
	wsem   chan struct{}
	writer atomic.Uint64 // goroutine id currently inside Write, 0 when none

	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error
	closeWait time.Duration // overrides CloseWait (tests)
	cleanup   func()        // removes the temp directory of OpenMemory

	// prev holds, per Scope, the device count of the last complete LoadWorld in
	// this process (state.World.PreviousDeviceCount; absent means -1).
	prevMu sync.Mutex
	prev   map[string]int
}

const (
	defaultBusyTimeout = 5 * time.Second
	defaultReadPool    = 4
	// CloseWait bounds Close: how long it waits for in-flight writes and reads.
	CloseWait = 10 * time.Second
)

// Open opens (creating if needed) the SQLite database at o.Path, configures every
// connection (WAL, synchronous NORMAL, foreign keys on, busy_timeout), runs
// PRAGMA quick_check, and applies the embedded migrations. It returns
// ErrCorrupt when quick_check fails, ErrMigrationChanged when an applied
// migration was edited and ErrMigrationAhead when the file is newer than the
// binary; in each case nothing is left open.
//
// The write connection is opened and migrated first, so by the time the read
// pool opens its connections the file is already in WAL mode.
func Open(ctx context.Context, o Options) (*Store, error) {
	if o.Path == "" {
		return nil, invalidf("Options.Path is required")
	}
	if strings.ContainsAny(o.Path, "?#") || strings.HasPrefix(o.Path, "file:") {
		return nil, invalidf("Options.Path must be a plain file path")
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = defaultBusyTimeout
	}
	if o.ReadPool <= 0 {
		o.ReadPool = defaultReadPool
	}

	s := &Store{
		path:  o.Path,
		clock: o.Clock,
		ids:   ulid.NewGenerator(o.Clock, nil),
		wsem:  make(chan struct{}, 1),
	}

	wdb, err := sql.Open("sqlite", dsn(o.Path, o.BusyTimeout, false))
	if err != nil {
		return nil, mapErr(err)
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)
	s.wdb = wdb

	if err := s.prepare(ctx); err != nil {
		_ = wdb.Close()
		return nil, err
	}

	rdb, err := sql.Open("sqlite", dsn(o.Path, o.BusyTimeout, true))
	if err != nil {
		_ = wdb.Close()
		return nil, mapErr(err)
	}
	rdb.SetMaxOpenConns(o.ReadPool)
	rdb.SetMaxIdleConns(o.ReadPool)
	rdb.SetConnMaxLifetime(0)
	if err := rdb.PingContext(ctx); err != nil {
		_ = rdb.Close()
		_ = wdb.Close()
		return nil, mapErr(err)
	}
	s.rdb = rdb
	return s, nil
}

// dsn builds the modernc.org/sqlite connection string. Every pragma here runs
// on every new connection the pool makes, so a connection that is recycled can
// never come back without foreign keys or the busy timeout.
func dsn(path string, busy time.Duration, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	return path + "?" + q.Encode()
}

// prepare runs quick_check and the migrations on the write connection.
func (s *Store) prepare(ctx context.Context) error {
	conn, err := s.wdb.Conn(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer conn.Close()

	rows, err := conn.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return mapErr(err)
	}
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return mapErr(err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mapErr(err)
	}
	if len(problems) > 0 {
		// quick_check lines name pages and tables, never row content.
		return fmt.Errorf("%w: quick_check: %s", ErrCorrupt, strings.Join(problems[:min(len(problems), 3)], "; "))
	}

	if _, err := migrations.Apply(ctx, conn, s.clock); err != nil {
		return mapErr(err)
	}
	return nil
}

// OpenMemory opens a private database for tests and --fixtures mode. It is a
// real SQLite file in a fresh temporary directory (removed by Close), not
// ":memory:": the production code path uses one write connection plus a read
// pool, and ":memory:" gives every connection its own empty database while
// shared-cache mode swaps busy_timeout semantics for table-level SQLITE_LOCKED
// errors. A temp file keeps fixtures and tests on exactly the code path that
// runs in production (architecture §3.5 item 6: "there is no second code
// path"). clock may be nil.
func OpenMemory(ctx context.Context, clock func() time.Time) (*Store, error) {
	dir, err := os.MkdirTemp("", "cryo-store-")
	if err != nil {
		return nil, err
	}
	s, err := Open(ctx, Options{Path: dir + string(os.PathSeparator) + "console.db", Clock: clock})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.cleanup = func() { _ = os.RemoveAll(dir) }
	return s, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes both pools and, for OpenMemory, removes the temporary database.
// It waits at most CloseWait: a Write stuck in its callback must not hold the
// process's exit. Past that it answers ErrCloseAbandoned and the caller exits with
// the transaction unfinished. Nothing committed is lost, because SQLite's WAL
// recovers on the next open and the unfinished transaction never happened. The
// close still completes in the background if the stuck work ever ends.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		wait := s.closeWait
		if wait <= 0 {
			wait = CloseWait
		}
		done := make(chan error, 1)
		go func() { done <- s.closeNow() }()
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case s.closeErr = <-done:
		case <-t.C:
			s.closeErr = ErrCloseAbandoned
		}
	})
	return s.closeErr
}

// closeNow is the close itself. It blocks behind an in-flight Write.
func (s *Store) closeNow() error {
	// Wait for an in-flight writer: take the semaphore.
	s.wsem <- struct{}{}
	// Release afterwards so a Write queued behind Close wakes up and returns ErrClosed.
	defer func() { <-s.wsem }()
	// Fold the WAL back into the main file so the file alone is a complete copy.
	if conn, err := s.wdb.Conn(context.Background()); err == nil {
		_, _ = conn.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
		_ = conn.Close()
	}
	err1 := s.rdb.Close()
	err2 := s.wdb.Close()
	if s.cleanup != nil {
		s.cleanup()
	}
	return errors.Join(err1, err2)
}

// Now returns the store clock's current time in UTC.
func (s *Store) Now() time.Time { return s.clock().UTC() }

// querier is what both *sql.Conn (the writer) and *sql.Tx (a reader) provide.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx is the handle a Write or Read callback receives. It is valid only inside
// the callback. Write's Tx runs inside BEGIN IMMEDIATE on the one write
// connection; Read's runs in a deferred transaction on a pooled read
// connection (a consistent snapshot: it never sees a concurrent commit
// halfway).
type Tx struct {
	s     *Store
	ctx   context.Context
	q     querier
	write bool
}

// Context is the context of the enclosing Write or Read.
func (tx *Tx) Context() context.Context { return tx.ctx }

// Exec runs a statement. It returns the raw driver error; Write and Read map it.
func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.q.ExecContext(tx.ctx, query, args...)
}

// Query runs a query.
func (tx *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.q.QueryContext(tx.ctx, query, args...)
}

// QueryRow runs a query expected to return one row.
func (tx *Tx) QueryRow(query string, args ...any) *sql.Row {
	return tx.q.QueryRowContext(tx.ctx, query, args...)
}

// Now returns the store clock in UTC.
func (tx *Tx) Now() time.Time { return tx.s.Now() }

// nowMS is the store clock as epoch milliseconds, the schema's time type.
func (tx *Tx) nowMS() int64 { return tx.s.clock().UnixMilli() }

// id returns a new ULID. Entropy cannot fail on a supported platform (Go 1.24+
// terminates the process if the OS random source breaks), so a failure here is
// a programming error and panics; Write rolls back and re-panics.
func (tx *Tx) id() string { return tx.s.ids.MustNew() }

// Write runs fn inside one transaction on the process's single write
// connection: BEGIN IMMEDIATE, then COMMIT if fn returns nil, otherwise
// ROLLBACK. It is the only way the store writes. Transactions must be short:
// other writers in this process queue behind fn (waiting respects ctx), and
// another process (cryo-console maintain) waits at most busy_timeout.
//
// The answer is the truth: nil means committed, an error means nothing was. A
// ctx that ends after fn has returned and the COMMIT has started does not stop
// the COMMIT, so a Write can answer nil just after its context ended.
//
// Calling Write from inside fn, on the same goroutine, returns ErrNestedWrite
// instead of deadlocking on the one connection. A panic in fn rolls back and
// is re-raised.
func (s *Store) Write(ctx context.Context, fn func(tx *Tx) error) (err error) {
	if s.closed.Load() {
		return ErrClosed
	}
	gid := goroutineID()
	if s.writer.Load() == gid {
		return ErrNestedWrite
	}
	select {
	case s.wsem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.wsem }()
	if s.closed.Load() {
		return ErrClosed
	}
	s.writer.Store(gid)
	defer s.writer.Store(0)

	conn, err := s.wdb.Conn(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer conn.Close()

	// Every way out but a COMMIT that succeeded ends here, a failed BEGIN included:
	// the driver can answer a context error for a BEGIN that took effect (abandonWrite).
	committed := false
	defer func() {
		if !committed {
			abandonWrite(ctx, conn)
		}
	}()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return mapErr(err)
	}
	tx := &Tx{s: s, ctx: ctx, q: conn, write: true}
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	// A context that ended before this line refuses the write. After it the COMMIT
	// cannot be cancelled, because the driver can also answer a context error for a
	// COMMIT that took effect, and the caller would be told a committed write failed.
	if err := ctx.Err(); err != nil {
		return mapErr(err)
	}
	if _, err := conn.ExecContext(context.WithoutCancel(ctx), "COMMIT"); err != nil {
		return mapErr(err)
	}
	committed = true
	return nil
}

// writeConn is what abandonWrite needs of the write connection (*sql.Conn).
type writeConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Raw(f func(driverConn any) error) error
}

// abandonWrite leaves the write connection outside any transaction before it
// returns to the pool. A connection returned inside one would fail every later
// Write with "cannot start a transaction within a transaction", because
// SetMaxOpenConns(1) makes it the only one.
//
// modernc.org/sqlite answers ctx.Err() whenever its watcher saw the context end
// during the call, even if the statement ran: stmt.exec overrides the result.
// SQLite clears an interrupt delivered before a statement's first step, so a
// context that ends in that window leaves BEGIN in effect, the interrupt flag
// clear and the connection valid to database/sql. So a failed BEGIN says nothing
// about the transaction, and ROLLBACK is what finds out: "no transaction is
// active" is the answer for a BEGIN that did not take effect, and for an error
// SQLite already rolled back.
//
// ROLLBACK runs on a context that cannot end. If it fails for any other reason
// the state is unknown, so the connection is discarded. That loses nothing, in
// OpenMemory too: the database is a file there as well, and the next Write opens
// a fresh connection with the same pragmas.
func abandonWrite(ctx context.Context, conn writeConn) {
	_, err := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	if err == nil || strings.Contains(err.Error(), "no transaction is active") {
		return
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// Read runs fn in a deferred transaction on the read pool: a consistent
// snapshot of the last committed state, any number in parallel with the writer
// (WAL). The transaction is query_only; a write attempt is an error.
func (s *Store) Read(ctx context.Context, fn func(tx *Tx) error) error {
	if s.closed.Load() {
		return ErrClosed
	}
	sqlTx, err := s.rdb.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = sqlTx.Rollback() }()
	if err := fn(&Tx{s: s, ctx: ctx, q: sqlTx}); err != nil {
		return mapErr(err)
	}
	return nil
}

// goroutineID parses the current goroutine's id out of runtime.Stack. It exists
// only to detect a nested Write cheaply and reliably; nothing else uses it.
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine 123 [running]:..."
	b := buf[:n]
	const prefix = "goroutine "
	if len(b) < len(prefix) {
		return 0
	}
	b = b[len(prefix):]
	var id uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}

// IntegrityCheck runs PRAGMA integrity_check and returns its first row: "ok"
// when the database is sound, otherwise SQLite's first complaint (a table or
// index name, never row content). It is the doctor command's full check; Open
// already ran the cheaper quick_check.
func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var out string
	err := s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`PRAGMA integrity_check`).Scan(&out)
	})
	return out, err
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	})
	return v, err
}
