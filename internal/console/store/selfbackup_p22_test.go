package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestVacuumIntoIsAnInspectableCopy: VACUUM INTO writes a copy whose integrity,
// schema version and row counts equal the live database's, and leaves no -wal
// or -shm file beside it when inspected.
func TestVacuumIntoIsAnInspectableCopy(t *testing.T) {
	w := seed(t)
	ctx := w.ctx
	dir := t.TempDir()
	dst := filepath.Join(dir, "console.db")
	ok(t, w.s.VacuumInto(ctx, dst))
	if err := w.s.VacuumInto(ctx, dst); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second VacuumInto onto an existing file = %v, want ErrInvalid", err)
	}
	f, err := InspectDatabaseFile(ctx, dst)
	ok(t, err)
	if f.Integrity != "ok" {
		t.Fatalf("integrity = %q", f.Integrity)
	}
	want, err := w.s.SchemaVersion(ctx)
	ok(t, err)
	if f.SchemaVersion != want || want == 0 {
		t.Fatalf("schema version = %d, live %d", f.SchemaVersion, want)
	}
	var devices, migrations int64
	ok(t, w.s.Read(ctx, func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&devices); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrations)
	}))
	if f.Tables["devices"] != devices || devices == 0 || f.Tables["schema_migrations"] != migrations {
		t.Fatalf("tables = %v, live devices %d migrations %d", f.Tables, devices, migrations)
	}
	for n := range f.Tables {
		if len(n) >= 7 && n[:7] == "sqlite_" {
			t.Errorf("internal table %s counted", n)
		}
	}
	ents, err := os.ReadDir(dir)
	ok(t, err)
	if len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("directory after inspect = %v, want only console.db", names)
	}
}

// TestInspectDatabaseFileRefusesGarbage: a file that is not a database, or a
// copy whose pages were overwritten, never reads as sound.
func TestInspectDatabaseFileRefusesGarbage(t *testing.T) {
	w := seed(t)
	ctx := w.ctx
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.db")
	ok(t, os.WriteFile(junk, []byte("this is not an sqlite database, not even close............................................................................................"), 0o600))
	if _, err := InspectDatabaseFile(ctx, junk); !errors.Is(err, ErrNotADatabase) {
		t.Fatalf("junk = %v, want ErrNotADatabase", err)
	}
	if _, err := InspectDatabaseFile(ctx, filepath.Join(dir, "missing.db")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v, want ErrNotFound", err)
	}
	cp := filepath.Join(dir, "copy.db")
	ok(t, w.s.VacuumInto(ctx, cp))
	b, err := os.ReadFile(cp)
	ok(t, err)
	// Overwrite everything after the first page (the header stays readable).
	for i := 4096; i < len(b); i++ {
		b[i] = 0xA5
	}
	ok(t, os.WriteFile(cp, b, 0o600))
	f, err := InspectDatabaseFile(ctx, cp)
	if err == nil && f.Integrity == "ok" {
		t.Fatalf("a damaged copy read as sound: %+v", f)
	}
}

// TestSelfBackupSettingsRoundTrip: the three rows read back as written, and the
// facts the health loader will read follow them.
func TestSelfBackupSettingsRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	st, err := s.SelfBackupState(ctx)
	ok(t, err)
	if st.Repo != nil || st.Last != nil || st.Verify != nil {
		t.Fatalf("fresh state = %+v", st)
	}
	f, err := s.SelfBackupFacts(ctx)
	ok(t, err)
	if f.LastAt != 0 || f.VerifyFailed || f.NotInBuild {
		t.Fatalf("fresh facts = %+v", f)
	}
	ok(t, s.PutSelfBackupRepo(ctx, SelfBackupRepoMarker{CreatedAt: 1, Bucket: "b", Prefix: "p-console"}))
	if err := s.RecordSelfBackup(ctx, SelfBackupLast{At: 0, SnapshotID: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("record without time = %v", err)
	}
	ok(t, s.RecordSelfBackup(ctx, SelfBackupLast{At: 5000, SnapshotID: "abc123", DBBytes: 10, DBSHA256: "ff", SchemaVersion: 8}))
	// An attempt without a verdict is not a failure.
	ok(t, s.RecordSelfBackupVerify(ctx, SelfBackupVerify{AttemptAt: 6000, AttemptClass: "network"}))
	f, err = s.SelfBackupFacts(ctx)
	ok(t, err)
	if f.LastAt != 5000 || f.VerifyFailed {
		t.Fatalf("facts after backup and inconclusive verify = %+v", f)
	}
	ok(t, s.RecordSelfBackupVerify(ctx, SelfBackupVerify{At: 7000, OK: false, Class: "manifest", SnapshotID: "abc123", AttemptAt: 7000}))
	f, err = s.SelfBackupFacts(ctx)
	ok(t, err)
	if !f.VerifyFailed {
		t.Fatalf("failed verdict not a failure: %+v", f)
	}
	st, err = s.SelfBackupState(ctx)
	ok(t, err)
	if st.Repo == nil || st.Repo.Prefix != "p-console" || st.Last == nil || st.Last.SnapshotID != "abc123" ||
		st.Verify == nil || st.Verify.Class != "manifest" {
		t.Fatalf("state = %+v %+v %+v", st.Repo, st.Last, st.Verify)
	}
	ok(t, s.RecordSelfBackupVerify(ctx, SelfBackupVerify{At: 8000, OK: true, AttemptAt: 8000}))
	if f, _ := s.SelfBackupFacts(ctx); f.VerifyFailed {
		t.Fatalf("passing verdict still failed: %+v", f)
	}
	// A damaged guard row is an error, never "absent".
	ok(t, s.PutSetting(ctx, SettingSelfBackupRepo, []byte(`"not an object"`), ""))
	if _, err := s.SelfBackupState(ctx); !errors.Is(err, ErrInvalid) {
		t.Fatalf("damaged row = %v, want ErrInvalid", err)
	}
}

// TestSelfBackupFactsVerdictAgeAndAttempt (fix C1): the facts carry the
// verdict's time, the newest attempt's time and class, and the first backup's
// time, which RecordSelfBackup keeps across later records (and takes from At
// for a row written before first_at existed).
func TestSelfBackupFactsVerdictAgeAndAttempt(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ok(t, s.RecordSelfBackup(ctx, SelfBackupLast{At: 5000, SnapshotID: "a"}))
	ok(t, s.RecordSelfBackup(ctx, SelfBackupLast{At: 9000, SnapshotID: "b"}))
	st, err := s.SelfBackupState(ctx)
	ok(t, err)
	if st.Last == nil || st.Last.FirstAt != 5000 || st.Last.At != 9000 {
		t.Fatalf("last = %+v, want first_at kept at 5000", st.Last)
	}
	f, err := s.SelfBackupFacts(ctx)
	ok(t, err)
	if f.FirstBackupAt != 5000 || f.LastAt != 9000 || f.VerifyAt != 0 || f.VerifyAttemptAt != 0 || f.VerifyAttemptClass != "" {
		t.Fatalf("facts with backups and no verify = %+v", f)
	}

	// A verdict, then an attempt without one: the verdict's time stays, the
	// attempt is beside it.
	ok(t, s.RecordSelfBackupVerify(ctx, SelfBackupVerify{At: 10000, OK: true, AttemptAt: 10000}))
	ok(t, s.RecordSelfBackupVerify(ctx, SelfBackupVerify{At: 10000, OK: true, AttemptAt: 20000, AttemptClass: "storage"}))
	f, err = s.SelfBackupFacts(ctx)
	ok(t, err)
	if f.VerifyAt != 10000 || f.VerifyAttemptAt != 20000 || f.VerifyAttemptClass != "storage" || f.VerifyFailed {
		t.Fatalf("facts after an inconclusive attempt = %+v", f)
	}

	// A row from before first_at: the facts and the next record use its At.
	ok(t, s.PutSetting(ctx, SettingSelfBackupLast, []byte(`{"at":3000,"snapshot_id":"old"}`), ""))
	if f, _ := s.SelfBackupFacts(ctx); f.FirstBackupAt != 3000 {
		t.Fatalf("legacy row: first backup at %d, want 3000", f.FirstBackupAt)
	}
	ok(t, s.RecordSelfBackup(ctx, SelfBackupLast{At: 4000, SnapshotID: "new"}))
	if f, _ := s.SelfBackupFacts(ctx); f.FirstBackupAt != 3000 || f.LastAt != 4000 {
		t.Fatalf("after a legacy row: %+v", f)
	}
}

// TestAnyEscrowedDevice: the newest active escrowed row, ErrNotFound without one.
func TestAnyEscrowedDevice(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.AnyEscrowedDevice(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store = %v, want ErrNotFound", err)
	}
	w := seed(t)
	id, err := w.s.AnyEscrowedDevice(w.ctx)
	ok(t, err)
	if id != w.devA.ID && id != w.devB.ID {
		t.Fatalf("device %q is neither seeded device", id)
	}
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET retired_at = 1`)
		return err
	}))
	if _, err := w.s.AnyEscrowedDevice(w.ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("all retired = %v, want ErrNotFound", err)
	}
}
