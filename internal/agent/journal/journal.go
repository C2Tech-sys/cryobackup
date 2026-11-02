package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the pure-Go driver, registered as "sqlite"
)

// SchemaVersion is the journal's PRAGMA user_version. A file with a higher
// number was written by a newer agent and is refused rather than guessed at.
const SchemaVersion = 1

// Errors returned by Open.
var (
	ErrNewer   = errors.New("journal: the file was written by a newer agent")
	ErrCorrupt = errors.New("journal: quick_check failed")
)

// DefaultBusyTimeout is how long SQLite waits for a lock held by another
// process (the service and a one-shot "check-in" share the file).
const DefaultBusyTimeout = 5 * time.Second

// Options configures Open.
type Options struct {
	// Path is the database file; created when missing. Required.
	Path string
	// Clock supplies every timestamp the journal writes. Nil means time.Now.
	Clock func() time.Time
	// BusyTimeout is the cross-process lock wait; zero means DefaultBusyTimeout.
	BusyTimeout time.Duration
	// ReadOnly opens the file query-only without migrating it (used by
	// "cryo-agent status", which must not change anything).
	ReadOnly bool
}

// Journal is the agent's durable state. It is safe for concurrent use: all
// statements go through one connection, so concurrent writers queue instead of
// failing.
type Journal struct {
	db    *sql.DB
	clock func() time.Time
}

// Open opens (creating and migrating when needed) the journal at o.Path.
func Open(ctx context.Context, o Options) (*Journal, error) {
	if o.Path == "" {
		return nil, errors.New("journal: Options.Path is required")
	}
	if strings.ContainsAny(o.Path, "?#") || strings.HasPrefix(o.Path, "file:") {
		return nil, errors.New("journal: Options.Path must be a plain file path")
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = DefaultBusyTimeout
	}
	db, err := sql.Open("sqlite", dsn(o.Path, o.BusyTimeout, o.ReadOnly))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	j := &Journal{db: db, clock: o.Clock}
	if err := j.prepare(ctx, o.ReadOnly); err != nil {
		_ = db.Close()
		return nil, err
	}
	return j, nil
}

func dsn(path string, busy time.Duration, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	return path + "?" + q.Encode()
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS runs (
	id              TEXT PRIMARY KEY,
	kind            TEXT NOT NULL,
	trigger         TEXT NOT NULL,
	state           TEXT NOT NULL,
	started_at      INTEGER NOT NULL,
	finished_at     INTEGER,
	outcome         TEXT NOT NULL DEFAULT '',
	exit_code       INTEGER,
	snapshot_id     TEXT NOT NULL DEFAULT '',
	stats           TEXT NOT NULL DEFAULT '{}',
	error_counts    TEXT NOT NULL DEFAULT '[]',
	error_class     TEXT NOT NULL DEFAULT '',
	attempt         INTEGER NOT NULL DEFAULT 1,
	deferred_reason TEXT NOT NULL DEFAULT '',
	policy_version  INTEGER NOT NULL DEFAULT 0,
	initial         INTEGER NOT NULL DEFAULT 0,
	agent_version   TEXT NOT NULL DEFAULT '',
	delivered_at    INTEGER,
	reject_reason   TEXT NOT NULL DEFAULT '',
	updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_by_kind_start ON runs(kind, started_at);

CREATE TABLE IF NOT EXISTS outbox (
	run_id     TEXT PRIMARY KEY,
	seq        INTEGER NOT NULL,
	body       TEXT NOT NULL,
	closed     INTEGER NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	next_at    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS kv (
	k          TEXT PRIMARY KEY,
	v          TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS commands (
	id          TEXT PRIMARY KEY,
	type        TEXT NOT NULL,
	params      TEXT NOT NULL DEFAULT '',
	received_at INTEGER NOT NULL,
	status      TEXT NOT NULL,
	error_class TEXT NOT NULL DEFAULT '',
	result      TEXT NOT NULL DEFAULT '',
	posted      INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER
);
`

func (j *Journal) prepare(ctx context.Context, readOnly bool) error {
	var version int
	if err := j.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return fmt.Errorf("%w (schema %d, this agent knows %d)", ErrNewer, version, SchemaVersion)
	}
	if readOnly {
		return nil
	}
	rows, err := j.db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return err
	}
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			_ = rows.Close()
			return err
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrCorrupt, strings.Join(problems[:min(len(problems), 3)], "; "))
	}
	if version == SchemaVersion {
		return nil
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the database.
func (j *Journal) Close() error { return j.db.Close() }

// now returns the journal clock as Unix milliseconds.
func (j *Journal) now() int64 { return j.clock().UnixMilli() }

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// tx runs fn in a transaction.
func (j *Journal) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
