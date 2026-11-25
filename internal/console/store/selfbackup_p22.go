package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The console's self-backup (ADR 0029, ticket P22-C): the maintenance worker
// copies this database with VACUUM INTO, backs the copy up to a restic
// repository of its own and, weekly, restores and checks the newest copy. This
// file holds what the worker needs from the store for that: the copy itself,
// a read-only look at a copied or restored database file, one escrowed device
// to prove the master key against, and the three settings rows the worker
// writes. Nothing here holds a secret: the repository password is derived from
// the master key and never stored (as for the probe repository, ADR 0017).

// Settings keys of the self-backup. Only the maintenance worker's lease holder
// writes them (maintain: SelfBackupJob).
const (
	// SettingSelfBackupRepo records that the self-backup repository was created
	// (or found) at a location: a SelfBackupRepoMarker. It guards the one-time
	// `restic init`: once it names the current location, a missing repository
	// is a failure (repo_missing), never re-created.
	SettingSelfBackupRepo = "self_backup_repo"
	// SettingSelfBackupLast is the newest self-backup that was written: a
	// SelfBackupLast. Absent means never.
	SettingSelfBackupLast = "self_backup_last"
	// SettingSelfBackupVerify is the newest tested restore: a SelfBackupVerify.
	// Absent means never tested.
	SettingSelfBackupVerify = "self_backup_verify"
)

// SelfBackupRepoMarker is the value of SettingSelfBackupRepo.
type SelfBackupRepoMarker struct {
	CreatedAt int64  `json:"created_at"` // Unix ms, UTC
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"` // the self-backup prefix (CRYO_MAINTAIN_SELF_PREFIX)
}

// SelfBackupLast is the value of SettingSelfBackupLast: the newest self-backup
// whose `restic backup` finished with a snapshot.
type SelfBackupLast struct {
	At int64 `json:"at"` // Unix ms, UTC: when the snapshot was written
	// FirstAt is when the first self-backup was written (Unix ms, UTC):
	// RecordSelfBackup carries it over from the row it replaces. The age of
	// "never verified" is counted from it (SelfBackupFacts).
	FirstAt       int64  `json:"first_at"`
	SnapshotID    string `json:"snapshot_id"`
	DBBytes       int64  `json:"db_bytes"`
	DBSHA256      string `json:"db_sha256"`
	SchemaVersion int    `json:"schema_version"`
}

// SelfBackupVerify is the value of SettingSelfBackupVerify. At, OK, Class and
// SnapshotID are the newest VERDICT: a tested restore that reached a result
// about the backup itself. AttemptAt and AttemptClass are the newest attempt,
// which may have ended without a verdict (the storage could not be reached, the
// disk was too small): such an attempt leaves the verdict as it was.
type SelfBackupVerify struct {
	At            int64  `json:"at,omitempty"` // Unix ms, UTC; 0: no verdict yet
	OK            bool   `json:"ok"`
	Class         string `json:"class,omitempty"` // path-free reason when OK is false
	SnapshotID    string `json:"snapshot_id,omitempty"`
	EscrowChecked bool   `json:"escrow_checked"` // an escrowed key was unwrapped from the restored copy
	AttemptAt     int64  `json:"attempt_at"`
	AttemptClass  string `json:"attempt_class,omitempty"` // "" when the attempt reached a verdict
}

// SelfBackupState is the three settings rows, nil where absent.
type SelfBackupState struct {
	Repo   *SelfBackupRepoMarker
	Last   *SelfBackupLast
	Verify *SelfBackupVerify
}

// SelfBackupState reads the self-backup settings rows. A row that is not valid
// for its type is an error (ErrInvalid): the worker must not mistake a damaged
// guard for "never created".
func (s *Store) SelfBackupState(ctx context.Context) (SelfBackupState, error) {
	var out SelfBackupState
	err := s.Read(ctx, func(tx *Tx) error {
		rows, err := tx.Query(`SELECT key, value FROM settings WHERE key IN (?, ?, ?)`,
			SettingSelfBackupRepo, SettingSelfBackupLast, SettingSelfBackupVerify)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			var derr error
			switch k {
			case SettingSelfBackupRepo:
				out.Repo = new(SelfBackupRepoMarker)
				derr = json.Unmarshal([]byte(v), out.Repo)
			case SettingSelfBackupLast:
				out.Last = new(SelfBackupLast)
				derr = json.Unmarshal([]byte(v), out.Last)
			case SettingSelfBackupVerify:
				out.Verify = new(SelfBackupVerify)
				derr = json.Unmarshal([]byte(v), out.Verify)
			}
			if derr != nil {
				return invalidf("self-backup setting %s is not valid", k)
			}
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) putSelfBackup(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.PutSetting(ctx, key, b, "")
}

// PutSelfBackupRepo writes SettingSelfBackupRepo.
func (s *Store) PutSelfBackupRepo(ctx context.Context, m SelfBackupRepoMarker) error {
	return s.putSelfBackup(ctx, SettingSelfBackupRepo, m)
}

// RecordSelfBackup writes SettingSelfBackupLast. FirstAt is taken from the
// row it replaces (its FirstAt, else its At, for a row written before
// FirstAt existed) or, for the first record, is At; a FirstAt passed in is
// kept. Only the maintenance worker's lease holder writes the row, so the
// read and the write need no common transaction.
func (s *Store) RecordSelfBackup(ctx context.Context, l SelfBackupLast) error {
	if l.At <= 0 || l.SnapshotID == "" {
		return invalidf("a self-backup record needs its time and snapshot id")
	}
	if l.FirstAt <= 0 {
		prev, err := s.SelfBackupState(ctx)
		if err != nil {
			return err
		}
		switch {
		case prev.Last == nil:
			l.FirstAt = l.At
		case prev.Last.FirstAt > 0:
			l.FirstAt = prev.Last.FirstAt
		case prev.Last.At > 0:
			l.FirstAt = prev.Last.At
		default:
			l.FirstAt = l.At
		}
	}
	return s.putSelfBackup(ctx, SettingSelfBackupLast, l)
}

// RecordSelfBackupVerify writes SettingSelfBackupVerify.
func (s *Store) RecordSelfBackupVerify(ctx context.Context, v SelfBackupVerify) error {
	if v.AttemptAt <= 0 {
		return invalidf("a self-backup verification needs its attempt time")
	}
	return s.putSelfBackup(ctx, SettingSelfBackupVerify, v)
}

// SelfBackupVerifyStale is how long the self-backup may go without a verify
// verdict before C11 must say so: the weekly verify plus one day (ticket
// P22-C fix C1). The health loader applies it (SelfBackupFacts).
const SelfBackupVerifyStale = 8 * 24 * time.Hour

// SelfBackupFacts is what the settings rows say about the self-backup, for
// the health loader (internal/console/health) once it stops marking the
// self-backup NotInBuild. The embedded state.SelfBackupFacts is what C11
// judges today: LastAt is SettingSelfBackupLast.at (zero: never written),
// VerifyFailed is a verdict in SettingSelfBackupVerify with ok false (no
// verdict yet, or only attempts without one, is not a failure), NotInBuild is
// always false (this build writes the rows). The other fields are the
// verdict's age and the newest attempt, so a verify that never reaches a
// verdict cannot stay green.
//
// The alarm rule for the loader (critical, C11), with now in UTC:
//
//	LastAt != 0 and now - ref > SelfBackupVerifyStale, where ref is VerifyAt,
//	or FirstBackupAt when there is no verdict yet (VerifyAt == 0);
//
// its text names the age in days and, when VerifyAttemptAt is later than
// VerifyAt, VerifyAttemptClass (the last attempt ended without a verdict:
// network, storage, disk, busy, no_slot, timeout, ...). VerifyFailed stays
// critical at once, as before.
type SelfBackupFacts struct {
	state.SelfBackupFacts
	// FirstBackupAt is SettingSelfBackupLast.first_at: when the first
	// self-backup was written; zero means never.
	FirstBackupAt state.Millis
	// VerifyAt is SettingSelfBackupVerify.at: when the newest verdict was
	// reached; zero means no verdict yet.
	VerifyAt state.Millis
	// VerifyAttemptAt and VerifyAttemptClass are SettingSelfBackupVerify's
	// attempt_at and attempt_class: when the newest attempt ended, and the
	// path-free class it ended with when it reached no verdict ("" when it
	// did). Zero: never attempted.
	VerifyAttemptAt    state.Millis
	VerifyAttemptClass string
}

// SelfBackupFacts reads the self-backup's facts (SelfBackupFacts, the type).
func (s *Store) SelfBackupFacts(ctx context.Context) (SelfBackupFacts, error) {
	st, err := s.SelfBackupState(ctx)
	if err != nil {
		return SelfBackupFacts{}, err
	}
	var f SelfBackupFacts
	if st.Last != nil {
		f.LastAt = state.Millis(st.Last.At)
		f.FirstBackupAt = state.Millis(st.Last.FirstAt)
		if f.FirstBackupAt == 0 {
			f.FirstBackupAt = f.LastAt // a row written before first_at existed
		}
	}
	if v := st.Verify; v != nil {
		f.VerifyFailed = v.At > 0 && !v.OK
		f.VerifyAt = state.Millis(v.At)
		f.VerifyAttemptAt = state.Millis(v.AttemptAt)
		f.VerifyAttemptClass = v.AttemptClass
	}
	return f, nil
}

// VacuumInto writes a consistent copy of the database to dst with SQLite's
// VACUUM INTO (a read transaction: the console keeps writing meanwhile). dst
// must not exist. The copy is an ordinary database file in rollback-journal
// mode. Errors are store sentinels and never name the path.
func (s *Store) VacuumInto(ctx context.Context, dst string) error {
	if dst == "" || strings.ContainsRune(dst, 0) {
		return invalidf("vacuum target is required")
	}
	if _, err := os.Lstat(dst); err == nil {
		return invalidf("vacuum target already exists")
	}
	if !s.enterRead() {
		return ErrClosed
	}
	defer s.leaveRead()
	// The read pool is query_only, which SQLite applies to VACUUM INTO as well,
	// and the one write connection would hold every writer of this process for
	// the length of the copy. So the copy runs on a connection of its own,
	// opened read-only (SQLITE_OPEN_READONLY: it cannot change the database or
	// checkpoint its WAL), and closed afterwards.
	q := url.Values{}
	q.Add("mode", "ro")
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(defaultBusyTimeout.Milliseconds(), 10)+")")
	db, err := sql.Open("sqlite", checkURIPath(s.path, runtime.GOOS == "windows")+"?"+q.Encode())
	if err != nil {
		return mapErr(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return mapErr(err)
	}
	return nil
}

// DatabaseFileFacts is what InspectDatabaseFile reads from a database file.
type DatabaseFileFacts struct {
	// Integrity is the first row of PRAGMA integrity_check: "ok" when sound,
	// otherwise SQLite's first complaint (a table or index name, never row
	// content).
	Integrity string
	// SchemaVersion is the highest applied migration (schema_migrations).
	SchemaVersion int
	// Tables is the row count of every table (sqlite_% tables excluded).
	Tables map[string]int64
}

// ErrNotADatabase is InspectDatabaseFile's answer for a file SQLite cannot
// read as a database at all (truncated, overwritten, not SQLite).
var ErrNotADatabase = errors.New("store: the file is not a readable database")

// InspectDatabaseFile opens the database file at path read-only and immutable
// (no lock, no -wal or -shm file is created next to it; the file must not be
// written meanwhile: a staged or restored copy, never the live database) and
// returns its integrity, schema version and row counts. It applies no
// migration. A file SQLite cannot read is ErrNotADatabase. It needs no Store
// (the self-backup restore on a bare host has none).
func InspectDatabaseFile(ctx context.Context, path string) (DatabaseFileFacts, error) {
	if err := checkPath(path); err != nil {
		return DatabaseFileFacts{}, err
	}
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return DatabaseFileFacts{}, ErrNotFound
	}
	db, err := sql.Open("sqlite", checkURIPath(path, runtime.GOOS == "windows")+"?mode=ro&immutable=1&_pragma=query_only(1)")
	if err != nil {
		return DatabaseFileFacts{}, ErrNotADatabase
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	out := DatabaseFileFacts{Tables: map[string]int64{}}
	// integrity_check first: on a damaged file it is the one answer that says so.
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&out.Integrity); err != nil {
		if ctx.Err() != nil {
			return DatabaseFileFacts{}, ctx.Err()
		}
		return DatabaseFileFacts{}, ErrNotADatabase
	}
	if out.Integrity != "ok" {
		return out, nil // counts from a damaged file would mean nothing
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&out.SchemaVersion); err != nil {
		if ctx.Err() != nil {
			return DatabaseFileFacts{}, ctx.Err()
		}
		return DatabaseFileFacts{}, ErrNotADatabase
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return DatabaseFileFacts{}, ErrNotADatabase
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return DatabaseFileFacts{}, ErrNotADatabase
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DatabaseFileFacts{}, ErrNotADatabase
	}
	rows.Close()
	for _, n := range names {
		var c int64
		q := `SELECT COUNT(*) FROM "` + strings.ReplaceAll(n, `"`, `""`) + `"`
		if err := db.QueryRowContext(ctx, q).Scan(&c); err != nil {
			if ctx.Err() != nil {
				return DatabaseFileFacts{}, ctx.Err()
			}
			return DatabaseFileFacts{}, ErrNotADatabase
		}
		out.Tables[n] = c
	}
	return out, nil
}

// AnyEscrowedDevice returns the device id of the newest active escrowed key
// (repo_key_escrow, custody escrowed, not retired, with a wrapped key), or
// ErrNotFound when there is none. The self-backup verification unwraps that
// row from a restored copy to prove the loaded master key opens it. It takes
// no Scope on purpose (scope_test.go, addSelfBackupP22Matrix): its only
// caller is the maintenance worker, on a restored copy of the whole database,
// and the id never reaches a user.
func (s *Store) AnyEscrowedDevice(ctx context.Context) (string, error) {
	var id string
	err := s.Read(ctx, func(tx *Tx) error {
		err := tx.QueryRow(`SELECT device_id FROM repo_key_escrow
			WHERE retired_at IS NULL AND custody = 'escrowed' AND wrapped_key IS NOT NULL
			ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return id, err
}
