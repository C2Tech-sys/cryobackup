// Package migrations carries the console database's schema as embedded,
// numbered SQL files and the runner that applies them.
//
// # The DH20 rule
//
// Migration 0001_initial was replica/schema.sql, byte for byte (architecture
// §1.3 DH20). All 34 tables exist from the first migration; later milestones
// add behaviour, not tables. 0001 is frozen: it has been applied by a release
// and is never edited. Since 0002, replica/schema.sql is the CURRENT schema, the
// human-readable end state next to schema_test.py (and what the vocabulary tests
// in internal/restic and internal/api read their CHECK lists from): the schema
// that executing schema.sql produces must equal the schema that applying every
// embedded migration to an empty database produces. TestSchemaSQLMatchesMigrations
// (here and in internal/console/store) compares the two (sqlite_master,
// normalised), and TestFrozenMigrationsAreUnchanged pins 0001_initial.sql by
// checksum. A schema change is therefore two edits in one change: a new
// NNNN_name.sql here and the same change in replica/schema.sql.
//
// # Adding migration 0002
//
//  1. Create 0002_<name>.sql in this directory (lower-case letters, digits and
//     underscores; four-digit version, contiguous from 0001). Never edit a file
//     that a release has shipped; a change is a new number.
//  2. Write plain SQL. The runner executes the file inside one BEGIN IMMEDIATE
//     transaction, so DDL and data changes land together or not at all. Do not
//     put BEGIN/COMMIT, journal_mode or foreign_keys pragmas in the file (they
//     are no-ops or errors inside a transaction); the connection already runs
//     with WAL and foreign keys on. After the file runs and before COMMIT the
//     runner executes PRAGMA foreign_key_check (step 10 of SQLite's table-rebuild
//     procedure) and rolls the migration back if it reports any row. A rebuild
//     that DROPs a table other tables reference is not safe with foreign keys
//     enforced (the implicit DELETE cascades); 0002 rebuilds runs, which nothing
//     references, and says so.
//  3. Do not insert the schema_migrations row yourself: the runner records
//     (version, name, sha256 checksum, applied-at) after the file succeeds.
//     (0001 inserts a 'bootstrap' row so that a bare sqlite3 session that runs
//     schema.sql directly leaves a marker. When the runner itself executes 0001
//     it overwrites that row with the real checksum in the same transaction. A
//     database built by a bare session is different: it records version 1 with
//     the checksum 'bootstrap', so Apply stops at 0001 with ErrMigrationChanged,
//     changes nothing, and the store refuses to open it. That is deliberate: the
//     runner cannot tell which migrations such a database really has (schema.sql
//     is the current schema, not 0001), and guessing would be quietly wrong. A
//     bare-session database is for inspection and schema_test.py only; for a
//     console, delete that file and let cryo-console init create the database,
//     which applies every migration through the runner (the store does the same
//     whenever it opens a database). TestBareSchemaSQLDatabaseIsRefused pins
//     this.)
//  4. Make the same change in replica/schema.sql (TestSchemaSQLMatchesMigrations
//     fails until the two agree) and keep replica/schema_test.py passing against
//     the result.
//  5. The store's migration test pins the table and index counts; update them
//     deliberately in the same change.
//
// # What the runner guarantees
//
// Apply is idempotent and safe for two processes starting together: every
// migration runs inside BEGIN IMMEDIATE and re-reads schema_migrations after
// taking the write lock, so the loser of the race finds the work done. It
// returns ErrMigrationChanged, and the store then refuses to open, when an
// applied migration's recorded checksum differs from the embedded file (someone
// edited history), and ErrMigrationAhead when the database records a version
// this binary does not know (a downgrade). Checksums are SHA-256 over the file
// with CRLF normalised to LF, so a Windows checkout with autocrlf produces the
// same value as a Linux build.
package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed *.sql
var files embed.FS

// ErrMigrationChanged means an applied migration's recorded checksum no longer
// matches the embedded file. The database is not opened.
var ErrMigrationChanged = errors.New("migrations: an applied migration was changed")

// ErrMigrationAhead means the database records a migration version this binary
// does not contain (it was written by a newer release). The database is not
// opened.
var ErrMigrationAhead = errors.New("migrations: database is newer than this binary")

// Files returns the embedded migration files (NNNN_name.sql at the root).
func Files() fs.FS { return files }

// Migration is one embedded file.
type Migration struct {
	// Version is the four-digit number in the file name.
	Version int
	// Name is the file name without the .sql suffix, e.g. "0001_initial".
	Name string
	// SQL is the file content.
	SQL string
	// Checksum is the hex SHA-256 of SQL with CRLF normalised to LF.
	Checksum string
}

var nameRE = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Checksum is the value recorded in schema_migrations for a file's content.
func Checksum(content []byte) string {
	norm := strings.ReplaceAll(string(content), "\r\n", "\n")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// List returns the embedded migrations in version order. It fails if a file
// name is malformed or the versions are not contiguous from 1.
func List() ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := nameRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrations: file %q does not match NNNN_name.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		b, err := fs.ReadFile(files, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{
			Version:  v,
			Name:     strings.TrimSuffix(e.Name(), ".sql"),
			SQL:      string(b),
			Checksum: Checksum(b),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migrations: versions must be contiguous from 0001; found %04d at position %d", m.Version, i+1)
		}
	}
	return out, nil
}

// Apply brings the database behind conn up to date and returns the versions it
// applied (nil when there was nothing to do). conn must be the process's write
// connection; now supplies the applied-at time (milliseconds are recorded).
func Apply(ctx context.Context, conn *sql.Conn, now func() time.Time) ([]int, error) {
	return applyUpTo(ctx, conn, now, 0)
}

// applyUpTo is Apply limited to migrations numbered at most maxVersion (0 = no
// limit). Tests use it to build a database as an older release left it.
func applyUpTo(ctx context.Context, conn *sql.Conn, now func() time.Time, maxVersion int) ([]int, error) {
	ms, err := List()
	if err != nil {
		return nil, err
	}
	known := make(map[int]Migration, len(ms))
	for _, m := range ms {
		known[m.Version] = m
	}
	var applied []int
	for _, m := range ms {
		if maxVersion > 0 && m.Version > maxVersion {
			break
		}
		did, err := applyOne(ctx, conn, m, known, now)
		if err != nil {
			return applied, err
		}
		if did {
			applied = append(applied, m.Version)
		}
	}
	// A database that already holds versions beyond ours is refused even when
	// every embedded migration matched (the loop above only looks at ours).
	if err := checkAhead(ctx, conn, known); err != nil {
		return applied, err
	}
	return applied, nil
}

// applyOne runs one migration inside BEGIN IMMEDIATE, or verifies its checksum
// when it is already recorded.
func applyOne(ctx context.Context, conn *sql.Conn, m Migration, known map[int]Migration, now func() time.Time) (did bool, err error) {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, fmt.Errorf("migrations: begin %s: %w", m.Name, err)
	}
	committed := false
	defer func() {
		if !committed {
			// Roll back on a context that cannot be cancelled: a half-applied migration must not linger.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	recorded, found, err := recordedChecksum(ctx, conn, m.Version)
	if err != nil {
		return false, err
	}
	if found {
		if recorded != m.Checksum {
			return false, fmt.Errorf("%w: version %d (%s): recorded checksum %.12s..., embedded file %.12s...",
				ErrMigrationChanged, m.Version, m.Name, recorded, m.Checksum)
		}
		return false, commit(ctx, conn, &committed)
	}

	if _, err := conn.ExecContext(ctx, m.SQL); err != nil {
		return false, fmt.Errorf("migrations: apply %s: %w", m.Name, err)
	}
	if err := foreignKeyCheck(ctx, conn, m.Name); err != nil {
		return false, err
	}
	// 0001 inserts a 'bootstrap' marker row; later files insert nothing. Upsert covers both.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(version) DO UPDATE SET name = excluded.name, checksum = excluded.checksum, created_at = excluded.created_at`,
		m.Version, m.Name, m.Checksum, now().UnixMilli()); err != nil {
		return false, fmt.Errorf("migrations: record %s: %w", m.Name, err)
	}
	return true, commit(ctx, conn, &committed)
}

// foreignKeyCheck fails when the migration left a foreign-key violation (step 10
// of SQLite's table-rebuild procedure). It reports the count per table, never
// row contents.
func foreignKeyCheck(ctx context.Context, conn *sql.Conn, name string) error {
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("migrations: foreign_key_check after %s: %w", name, err)
	}
	defer rows.Close()
	tables := map[string]int{}
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("migrations: foreign_key_check after %s: %w", name, err)
		}
		tables[table]++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrations: foreign_key_check after %s: %w", name, err)
	}
	if len(tables) > 0 {
		return fmt.Errorf("migrations: %s left foreign key violations (rows per table: %v)", name, tables)
	}
	return nil
}

func commit(ctx context.Context, conn *sql.Conn, committed *bool) error {
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("migrations: commit: %w", err)
	}
	*committed = true
	return nil
}

// recordedChecksum reads schema_migrations; a database without the table has no
// recorded migrations.
func recordedChecksum(ctx context.Context, conn *sql.Conn, version int) (checksum string, found bool, err error) {
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&n); err != nil {
		return "", false, fmt.Errorf("migrations: inspect database: %w", err)
	}
	if n == 0 {
		return "", false, nil
	}
	err = conn.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = ?`, version).Scan(&checksum)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("migrations: read schema_migrations: %w", err)
	}
	return checksum, true, nil
}

func checkAhead(ctx context.Context, conn *sql.Conn, known map[int]Migration) error {
	rows, err := conn.QueryContext(ctx, `SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("migrations: read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var name string
		if err := rows.Scan(&v, &name); err != nil {
			return err
		}
		if _, ok := known[v]; !ok {
			return fmt.Errorf("%w: database records version %d (%s)", ErrMigrationAhead, v, name)
		}
	}
	return rows.Err()
}
