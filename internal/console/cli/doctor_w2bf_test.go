package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/migrations"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
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
	if strings.Contains(out, doctorLine("OK", "database")) {
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
	if code, out, _ := run(t, "", "doctor", "--config", cfgPath); code != ExitFailed || !strings.Contains(out, doctorLine("FAIL", "database")) {
		t.Fatalf("a newer database: %d\n%s", code, out)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(cfg.DBPath + suffix)
	}
	if code, out, _ := run(t, "", "doctor", "--config", cfgPath); code != ExitFailed || !strings.Contains(out, doctorLine("FAIL", "database")) {
		t.Fatalf("a missing database: %d\n%s", code, out)
	}
	if _, err := os.Stat(cfg.DBPath); err == nil {
		t.Fatal("doctor created the database")
	}
}

// doctorRig is an initialised console with its config and database paths.
func doctorRig(t *testing.T) (cfgPath, db string) {
	t.Helper()
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0")...); code != 0 {
		t.Fatal(e)
	}
	cfgPath = filepath.Join(dir, "config.json")
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, cfg.DBPath
}

// writer opens the database as a second process would, in WAL mode with automatic checkpoints
// off, and writes one row.
func writer(t *testing.T, db string, n int) *sql.DB {
	t.Helper()
	w, err := sql.Open("sqlite", db+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	writeRow(t, w, n)
	return w
}

func writeRow(t *testing.T, w *sql.DB, n int) {
	t.Helper()
	if _, err := w.Exec(`INSERT INTO settings (key, value, created_at, updated_at) VALUES (?, '"x"', 1, 1)`, fmt.Sprintf("doctor_test_%d", n)); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// P20-W2BF B2 (a): a database left with a non-empty WAL by an unclean stop: doctor leaves the main
// file and the WAL byte for byte as they were (it may create or touch the -shm index).
func TestDoctorLeavesAnUncleanWALAsItIs(t *testing.T) {
	cfgPath, db := doctorRig(t)
	w := writer(t, db, 1)
	tmp := t.TempDir()
	for _, s := range []string{"", "-wal", "-shm"} {
		copyFile(t, db+s, filepath.Join(tmp, "c"+s)) // the files as a crash would leave them
	}
	_ = w.Close()
	for _, s := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(db + s)
		copyFile(t, filepath.Join(tmp, "c"+s), db+s)
	}
	if fi, err := os.Stat(db + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("the test needs a non-empty WAL: %v", err)
	}
	mainBefore, walBefore := fileHash(t, db), fileHash(t, db+"-wal")
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if fileHash(t, db) != mainBefore {
		t.Fatalf("doctor changed the main file:\n%s", out)
	}
	if _, err := os.Stat(db + "-wal"); err != nil {
		t.Fatalf("doctor removed the WAL:\n%s", out)
	}
	if fileHash(t, db+"-wal") != walBefore {
		t.Fatalf("doctor changed the WAL:\n%s", out)
	}
	if !strings.Contains(out, doctorLine("OK", "database")) {
		t.Fatalf("doctor did not read the database:\n%s", out)
	}
}

// P20-W2BF B2 (b): doctor while another connection holds the database open and writes: the writer
// is undisturbed, the main file is unchanged and the WAL is not truncated.
func TestDoctorBesideALiveWriter(t *testing.T) {
	cfgPath, db := doctorRig(t)
	w := writer(t, db, 1)
	defer w.Close()
	writeRow(t, w, 2)
	mainBefore := fileHash(t, db)
	fi, err := os.Stat(db + "-wal")
	if err != nil || fi.Size() == 0 {
		t.Fatalf("the test needs a live WAL: %v", err)
	}
	walBefore := fi.Size()
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if fileHash(t, db) != mainBefore {
		t.Fatalf("doctor changed the main file under a live writer:\n%s", out)
	}
	if fi, err := os.Stat(db + "-wal"); err != nil || fi.Size() < walBefore {
		t.Fatalf("doctor truncated the live WAL (%v):\n%s", err, out)
	}
	writeRow(t, w, 3) // the writer goes on
	var n int
	if err := w.QueryRow(`SELECT count(*) FROM settings WHERE key LIKE 'doctor_test_%'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("writer rows %d (%v)", n, err)
	}
}

// P20-W2BF B5: the status column is wide enough for NOT CHECKED: every check name starts in the
// same column.
func TestDoctorStatusColumnFitsNotChecked(t *testing.T) {
	cfgPath, db := doctorRig(t)
	ms, err := migrations.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(db + suffix)
	}
	olderDatabase(t, db, len(ms)-2)
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	col := -1
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		for _, name := range []string{"database", "default policy", "master key", "config"} {
			if i := strings.Index(line, " "+name+" "); i >= 0 && i < 14 {
				if col < 0 {
					col = i
				}
				if i != col {
					t.Fatalf("the check name %q starts in column %d, not %d:\n%s", name, i+1, col+1, out)
				}
			}
		}
	}
	if col < 0 || !strings.Contains(out, doctorLine("NOT CHECKED", "default policy")) {
		t.Fatalf("output:\n%s", out)
	}
}

// doctorLine is the start of one doctor line: the status, padded to its column, and the check name.
func doctorLine(level, name string) string { return fmt.Sprintf("%-11s %s", level, name) }

// P20-W2BF C2: doctor reads exactly the file serve opens, also when the path holds "%41" and a
// space ("%41" is not "A").
func TestDoctorOpensTheConfiguredFileWhateverItsName(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0",
		"--data-dir", filepath.Join(dir, "pct%41 data"))...); code != 0 {
		t.Fatal(e)
	}
	_, out, _ := run(t, "", "doctor", "--config", filepath.Join(dir, "config.json"))
	ms, err := migrations.List()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, doctorLine("OK", "database")) || !strings.Contains(out, fmt.Sprintf("schema version %d,", len(ms))) {
		t.Fatalf("doctor on a path with %%41 and a space:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "pctA data")); err == nil {
		t.Fatal("a decoded directory exists")
	}
}

// P20-W2BF C3: on a cleanly stopped WAL database doctor leaves the main file as it was; what it
// may create beside it is the -shm index and an empty -wal file, nothing with content.
func TestDoctorOnACleanWALDatabaseCreatesOnlyEmptyFiles(t *testing.T) {
	cfgPath, db := doctorRig(t)
	if _, err := os.Stat(db + "-wal"); err == nil {
		t.Fatal("the test needs a cleanly stopped database (no -wal)")
	}
	before := fileHash(t, db)
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if fileHash(t, db) != before {
		t.Fatalf("doctor changed the main file:\n%s", out)
	}
	if fi, err := os.Stat(db + "-wal"); err == nil && fi.Size() != 0 {
		t.Fatalf("doctor left a -wal of %d bytes", fi.Size())
	}
	if !strings.Contains(out, "may create the -shm and an empty -wal file") {
		t.Fatalf("doctor's database line does not say what it may create:\n%s", out)
	}
}

// P20-W2BF C3: when SQLite cannot set up the -shm index it needs to read a WAL database (here a
// directory stands where the -shm file must go; in the field, a directory this account may not
// write in), doctor says so and what to do, not a bare "unable to open database file (14)".
//
// P20-W3D 5: the "injected" case holds on every OS: the directory at the -shm name is real and
// SQLite's open error is injected (openNoMigrate). The "real open" case lets SQLite itself fail
// and runs on Windows only: on the Linux CI runner a directory at the -shm name does not make the
// read-only open fail (doctor printed the database OK line), which by reading is the unix VFS
// falling back to a read-only, then an in-memory shm index when the -shm cannot be opened
// read-write and no -wal exists. That fallback was not verified on a Unix machine here.
func TestDoctorExplainsAnUnwritableShm(t *testing.T) {
	for _, tc := range []struct {
		name     string
		injected bool
	}{{"injected", true}, {"real open", false}} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.injected && runtime.GOOS != "windows" {
				t.Skip("a directory at the -shm name does not fail SQLite's read-only open outside Windows (unix VFS shm fallback); the injected case covers the branch")
			}
			cfgPath, db := doctorRig(t)
			_ = os.Remove(db + "-shm")
			if err := os.Mkdir(db+"-shm", 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.injected {
				injectOpenError(t, "store: open: unable to open database file: out of memory (14)")
			}
			code, out, _ := run(t, "", "doctor", "--config", cfgPath)
			if code != ExitFailed || !strings.Contains(out, doctorLine("FAIL", "database")) || strings.Contains(out, "unable to open database file") ||
				!strings.Contains(out, "-shm") || !strings.Contains(out, "service account") || strings.Contains(out, "not readable by this user") {
				t.Fatalf("doctor with no usable -shm: %d\n%s", code, out)
			}
		})
	}
}

// injectOpenError makes doctor's read-only open fail with msg (openNoMigrate), on every OS.
func injectOpenError(t *testing.T, msg string) {
	t.Helper()
	prev := openNoMigrate
	t.Cleanup(func() { openNoMigrate = prev })
	openNoMigrate = func(context.Context, store.Options) (*store.Store, []string, error) { return nil, nil, errors.New(msg) }
}

// P20-W2BF D5: when the main file itself cannot be opened for reading, doctor says so and does not
// blame the directory or the -shm index. Both the open error (openNoMigrate) and the unreadable
// main file (openForReading) are injected, so the fault holds on every OS (P20-W3D 5: it relied
// on a directory at the -shm name failing the open, which it does not on Linux).
func TestDoctorNamesAnUnreadableMainFile(t *testing.T) {
	cfgPath, db := doctorRig(t)
	injectOpenError(t, "store: open: unable to open database file: out of memory (14)")
	prev := openForReading
	t.Cleanup(func() { openForReading = prev })
	openForReading = func(path string) error {
		if path != db {
			t.Errorf("readability checked on %q, not the database file", path)
		}
		return os.ErrPermission
	}
	code, out, _ := run(t, "", "doctor", "--config", cfgPath)
	want := fmt.Sprintf("%-11s %-22s %s\n", "FAIL", "database", "database file is not readable by this user") // the whole line: no other detail
	if code != ExitFailed || !strings.Contains(out, want) || strings.Contains(out, "-shm") {
		t.Fatalf("doctor on an unreadable main file: %d\n%s", code, out)
	}
}

// D5 with a real permission: a main file mode 0000. Skipped on Windows, where making a file
// unreadable needs an ACL edit (icacls) that a test cannot make and undo hermetically, and for
// root, who reads it anyway; the branch is covered there by TestDoctorNamesAnUnreadableMainFile.
func TestDoctorNamesAnUnreadableMainFileByMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an unreadable file on Windows needs an ACL edit that cannot be made hermetic; the injected test covers the branch")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 0000 file")
	}
	cfgPath, db := doctorRig(t)
	if err := os.Chmod(db, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(db, 0o600) })
	code, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if code != ExitFailed || !strings.Contains(out, "database file is not readable by this user") || strings.Contains(out, "-shm") {
		t.Fatalf("doctor on a mode 0000 main file: %d\n%s", code, out)
	}
}
