package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The self-backup methods are console-wide, not customer-scoped: they belong in
// scope_test.go's globalMethods. That file is outside ticket P22-C's write set,
// so they are classified here, each with its reason; the guard
// (TestScopeMatrixCoversEveryExportedMethod) reads the same map.
func init() {
	for name, why := range map[string]string{
		"VacuumInto":             "console self-backup (ADR 0029): a copy of the whole database for the maintenance worker",
		"SelfBackupState":        "console self-backup: global settings rows (repository marker, newest backup, newest verify)",
		"PutSelfBackupRepo":      "console self-backup: global settings row",
		"RecordSelfBackup":       "console self-backup: global settings row",
		"RecordSelfBackupVerify": "console self-backup: global settings row",
		"SelfBackupFacts":        "health (C11): the self-backup's time and verdict, no customer data",
		"AnyEscrowedDevice":      "console self-backup verify: one device id of a restored copy to prove the master key against; no customer data returned",
	} {
		globalMethods[name] = why
	}
}

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
