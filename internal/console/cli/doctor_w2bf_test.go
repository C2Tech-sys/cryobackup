package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/migrations"
)

// olderDatabase writes, at path, a database as the release with the first upTo migrations left it:
// each embedded file run and recorded as the migration runner does, in WAL mode, then closed
// (the last close checkpoints the WAL into the main file and removes it).
func olderDatabase(t *testing.T, path string, upTo int) {
	t.Helper()
	ms, err := migrations.List()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms[:upTo] {
		if _, err := db.Exec(m.SQL); err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, created_at) VALUES (?, ?, ?, 1)
			ON CONFLICT(version) DO UPDATE SET name = excluded.name, checksum = excluded.checksum`, m.Version, m.Name, m.Checksum); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// P20-W2BF item 1: doctor run against a database of the previous release changes nothing (the
// main file is byte for byte the same and no WAL with content is left), says which migrations the
// first start will apply, and reports the checks that need the new schema as NOT CHECKED.
func TestDoctorDoesNotMigrateAnOlderDatabase(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0")...); code != 0 {
		t.Fatal(e)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	ms, err := migrations.List()
	if err != nil {
		t.Fatal(err)
	}
	prev := len(ms) - 2 // v0.3.0: migrations 0001 to 0006
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(cfg.DBPath + suffix)
	}
	olderDatabase(t, cfg.DBPath, prev)
	before := fileHash(t, cfg.DBPath)

	_, out, _ := run(t, "", "doctor", "--config", cfgPath)

	if after := fileHash(t, cfg.DBPath); after != before {
		t.Fatalf("doctor changed the database file:\n%s", out)
	}
	if fi, err := os.Stat(cfg.DBPath + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("doctor left a WAL of %d bytes", fi.Size())
	}
	db, err := sql.Open("sqlite", cfg.DBPath+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := db.QueryRowContext(context.Background(), `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil || v != prev {
		t.Fatalf("schema version after doctor = %d (%v), want %d", v, err, prev)
	}
	_ = db.Close()
	for _, want := range []string{"schema version 6", "migrates it to schema version 8", ms[prev].Name, ms[prev+1].Name,
		"NOT CHECKED default policy", "NOT CHECKED storage target"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "OK    database") {
		t.Errorf("an unmigrated database reads OK:\n%s", out)
	}
}

// Item 1: a database newer than the binary still fails, and a missing one is not created.
func TestDoctorNewerOrMissingDatabaseFails(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0")...); code != 0 {
		t.Fatal(e)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, checksum, created_at) VALUES (99, '0099_future', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if code, out, _ := run(t, "", "doctor", "--config", cfgPath); code != ExitFailed || !strings.Contains(out, "FAIL  database") {
		t.Fatalf("a newer database: %d\n%s", code, out)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(cfg.DBPath + suffix)
	}
	if code, out, _ := run(t, "", "doctor", "--config", cfgPath); code != ExitFailed || !strings.Contains(out, "FAIL  database") {
		t.Fatalf("a missing database: %d\n%s", code, out)
	}
	if _, err := os.Stat(cfg.DBPath); err == nil {
		t.Fatal("doctor created the database")
	}
}
