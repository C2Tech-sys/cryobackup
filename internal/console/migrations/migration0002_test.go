package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// frozen0001 is the checksum of 0001_initial.sql as released. The file is never
// edited; a schema change is a new migration (and the same change in
// replica/schema.sql).
const frozen0001 = "b8ad4875d02feeddf384746bd9afaa38609cc812829b9c1c3237255136763d8b"

func TestFrozenMigrationsAreUnchanged(t *testing.T) {
	ms, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].Checksum != frozen0001 {
		t.Fatalf("0001_initial.sql checksum = %s, want %s: 0001 is frozen, add a migration instead", ms[0].Checksum, frozen0001)
	}
}

// schemaSignature is the schema as sqlite_master states it, one normalised line
// per object, sorted. Row data (schema_migrations) is not part of it. SQLite
// stores a table renamed by ALTER TABLE ... RENAME with a quoted name, which is
// the only text difference between a rebuilt table and a created one.
func schemaSignature(t *testing.T, conn *sql.Conn) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(),
		`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, tbl, def string
		if err := rows.Scan(&typ, &name, &tbl, &def); err != nil {
			t.Fatal(err)
		}
		def = strings.Replace(def, `CREATE TABLE "runs"`, `CREATE TABLE runs`, 1)
		out = append(out, strings.Join([]string{typ, name, tbl, def}, " | "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// DH20 (since 0002): replica/schema.sql is the current schema. Executing it on
// an empty database must give the same schema as applying every migration.
func TestSchemaSQLMatchesMigrations(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "replica", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	_, fromSQL := openConn(t, filepath.Join(t.TempDir(), "schema.db"))
	if _, err := fromSQL.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("executing replica/schema.sql: %v", err)
	}
	_, migrated := openConn(t, filepath.Join(t.TempDir(), "migrated.db"))
	if _, err := Apply(ctx, migrated, fixedNow); err != nil {
		t.Fatal(err)
	}
	a, b := schemaSignature(t, fromSQL), schemaSignature(t, migrated)
	if len(a) < 100 {
		t.Fatalf("schema.sql produced only %d objects", len(a))
	}
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		inA, inB := map[string]bool{}, map[string]bool{}
		for _, l := range a {
			inA[l] = true
		}
		for _, l := range b {
			inB[l] = true
		}
		for _, l := range a {
			if !inB[l] {
				t.Errorf("only in replica/schema.sql: %.200s", l)
			}
		}
		for _, l := range b {
			if !inA[l] {
				t.Errorf("only in the migrations: %.200s", l)
			}
		}
		t.Fatalf("replica/schema.sql and the migrations disagree (%d vs %d objects): a schema change is a new migration AND the same change in schema.sql", len(a), len(b))
	}
}

// A database built by running replica/schema.sql in a bare sqlite3 session is
// refused, not adopted: schema.sql records version 1 with the checksum
// 'bootstrap', which is not 0001's checksum, so Apply returns
// ErrMigrationChanged before touching anything and the database is left as it
// was (embed.go, "Adding migration 0002", step 3, says what to do instead).
func TestBareSchemaSQLDatabaseIsRefused(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "replica", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	_, conn := openConn(t, filepath.Join(t.TempDir(), "bare.db"))
	if _, err := conn.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("executing replica/schema.sql: %v", err)
	}
	before := rowsOf(t, conn, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if strings.Join(before, "\n") == "" || !strings.Contains(before[0], "bootstrap") || len(before) != 1 {
		t.Fatalf("schema.sql recorded %v, want only version 1 with the bootstrap marker", before)
	}
	applied, err := Apply(ctx, conn, fixedNow)
	if !errors.Is(err, ErrMigrationChanged) || len(applied) != 0 {
		t.Fatalf("Apply on a bare schema.sql database = %v, %v; want ErrMigrationChanged and nothing applied", applied, err)
	}
	if after := rowsOf(t, conn, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("a refused Apply changed schema_migrations: %v -> %v", before, after)
	}
}

func mustExec(t *testing.T, conn *sql.Conn, q string, args ...any) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", strings.Fields(q)[0], err)
	}
}

const (
	tsStart = 1790000000000 // 2026-09 in ms
	devID   = "01JDEVICE0000000000000000A"
)

// seedV1 inserts a storage target, customer, device and a mixed set of runs into
// a version-1 database, the way a deployed console would hold them.
func seedV1(t *testing.T, conn *sql.Conn) {
	t.Helper()
	mustExec(t, conn, `INSERT INTO storage_targets (id, name, provider, endpoint, bucket, gateway_url, credential_ref, created_at, updated_at)
		VALUES ('01JSTORAGE000000000000000A', 'target-a', 's3_compatible', 'https://s3.example.invalid', 'bkt', 'https://gw.example.invalid', 'CRED_REF', ?, ?)`, tsStart, tsStart)
	mustExec(t, conn, `INSERT INTO customers (id, name, slug, storage_target_id, created_at, updated_at)
		VALUES ('01JCUSTOMER00000000000000A', 'Customer A', 'customer-a', '01JSTORAGE000000000000000A', ?, ?)`, tsStart, tsStart)
	mustExec(t, conn, `INSERT INTO devices (id, customer_id, hostname, enrolled_at, enrolled_via, repo_path, created_at, updated_at)
		VALUES (?, '01JCUSTOMER00000000000000A', 'host-a', ?, 'token', 'prefix/dev-a', ?, ?)`, devID, tsStart, tsStart, tsStart)

	ins := `INSERT INTO runs (id, device_id, origin, kind, trigger_kind, started_at, finished_at, received_at, clock_offset_ms,
		outcome, restic_exit_code, snapshot_id, stats, data_added_bytes, error_counts, error_class, attempt, deferred_reason,
		policy_version, agent_version, source_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	snap := strings.Repeat("ab", 32)
	mustExec(t, conn, ins, "01JRUN000000000000000000A1", devID, "agent", "backup", "schedule", tsStart, tsStart+60000, tsStart+61000, -250,
		"success", 0, snap, `{"files_new":3}`, 4096, `{}`, nil, 1, nil, 7, "0.1.0", nil, tsStart+61000, tsStart+61000)
	mustExec(t, conn, ins, "01JRUN000000000000000000A2", devID, "agent", "backup", "boot", tsStart+100000, tsStart+160000, tsStart+161000, 0,
		"partial", 3, nil, `{"files_failed":4}`, nil, `{"permission":3,"locked":1}`, "permission", 2, nil, 7, "0.1.0", nil, tsStart+161000, tsStart+162000)
	mustExec(t, conn, ins, "01JRUN000000000000000000A3", devID, "agent", "forget", "manual", tsStart+200000, nil, tsStart+200500, 0,
		nil, nil, nil, `{}`, nil, `{}`, nil, 1, nil, nil, nil, nil, tsStart+200500, tsStart+200500)
	mustExec(t, conn, ins, "01JRUN000000000000000000A4", devID, "console", "check", "retry", tsStart+300000, tsStart+300100, tsStart+300200, 0,
		"deferred", nil, nil, `{}`, nil, `{}`, "lock", 3, "lock", nil, nil, "src-1", tsStart+300200, tsStart+300200)
}

// rowsOf returns every row of a query as strings, in the query's order.
func rowsOf(t *testing.T, conn *sql.Conn, q string) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%#v", vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func count(t *testing.T, conn *sql.Conn, q string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// insertRun inserts a minimal valid run with the given id and error class.
func insertRun(conn *sql.Conn, id string, class any) error {
	_, err := conn.ExecContext(context.Background(),
		`INSERT INTO runs (id, device_id, kind, trigger_kind, started_at, finished_at, received_at, outcome, error_counts, error_class, created_at, updated_at)
		 VALUES (?, ?, 'backup', 'schedule', ?, ?, ?, 'partial', '{"x":1}', ?, ?, ?)`,
		id, devID, tsStart+400000, tsStart+460000, tsStart+461000, class, tsStart+461000, tsStart+461000)
	return err
}

// The version-1 database a release left behind, with runs in it, migrates to
// version 2 with every row, index and constraint intact, and accepts the new
// error class afterwards.
func TestMigrate1To2KeepsDataAndWidensTheCheck(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))

	applied, err := applyUpTo(ctx, conn, fixedNow, 1)
	if err != nil || len(applied) != 1 || applied[0] != 1 {
		t.Fatalf("apply to version 1 = %v, %v", applied, err)
	}
	seedV1(t, conn)

	// Preconditions the rebuild relies on, stated on the version-1 schema.
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE sql LIKE '%REFERENCES runs%' OR sql LIKE '%REFERENCES "runs"%'`); n != 0 {
		t.Fatalf("%d objects reference runs: DROP TABLE runs would cascade into them with foreign keys on", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type IN ('trigger','view') AND sql LIKE '%runs%'`); n != 0 {
		t.Fatalf("%d triggers or views mention runs; the rebuild does not recreate them", n)
	}
	// Version 1 rejects the new class (this is the red state the migration cures).
	if err := insertRun(conn, "01JRUN000000000000000000Z0", "unsupported_file"); err == nil {
		t.Fatal("version 1 accepted unsupported_file")
	}

	const q = `SELECT * FROM runs ORDER BY id`
	rowsBefore := rowsOf(t, conn, q)
	if len(rowsBefore) != 4 {
		t.Fatalf("seeded %d runs", len(rowsBefore))
	}
	idxBefore := rowsOf(t, conn, `SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'runs' AND sql IS NOT NULL ORDER BY name`)
	if len(idxBefore) != 8 {
		t.Fatalf("version 1 has %d explicit runs indexes, want 8", len(idxBefore))
	}
	otherBefore := rowsOf(t, conn, `SELECT * FROM devices`)

	applied, err = applyUpTo(ctx, conn, fixedNow, 2)
	if err != nil || len(applied) != 1 || applied[0] != 2 {
		t.Fatalf("apply 1 -> 2 = %v, %v", applied, err)
	}

	// Data: every column of every row, the other tables, the indexes.
	if got := rowsOf(t, conn, q); strings.Join(got, "\n") != strings.Join(rowsBefore, "\n") {
		t.Fatalf("runs rows changed by the migration:\nbefore %v\nafter  %v", rowsBefore, got)
	}
	if got := rowsOf(t, conn, `SELECT * FROM devices`); strings.Join(got, "\n") != strings.Join(otherBefore, "\n") {
		t.Fatal("devices changed by the migration")
	}
	if got := rowsOf(t, conn, `SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'runs' AND sql IS NOT NULL ORDER BY name`); strings.Join(got, "\n") != strings.Join(idxBefore, "\n") {
		t.Fatalf("runs indexes changed:\nbefore %v\nafter  %v", idxBefore, got)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE name = 'runs_new'`); n != 0 {
		t.Fatal("runs_new left behind")
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`); n != 34 {
		t.Fatalf("%d tables after the migration, want 34", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL`); n != 103 {
		t.Fatalf("%d explicit indexes after the migration, want 103", n)
	}

	// Constraints: the new class is accepted, a bogus one and every old rule still hold.
	if err := insertRun(conn, "01JRUN000000000000000000B1", "unsupported_file"); err != nil {
		t.Fatalf("version 2 rejected unsupported_file: %v", err)
	}
	for _, class := range []string{"missing_source", "locked", "permission"} {
		if err := insertRun(conn, "01JRUN0000000000000000"+strings.ToUpper(class[:3])+"1", class); err != nil {
			t.Errorf("version 2 rejected the old class %s: %v", class, err)
		}
	}
	for _, bogus := range []string{"bogus", "unsupported", "Unsupported_File", "unsupported_file "} {
		if err := insertRun(conn, "01JRUN000000000000000000C1", bogus); err == nil {
			t.Errorf("version 2 accepted the bogus class %q", bogus)
		}
	}
	if err := insertRun(conn, "short", "unsupported_file"); err == nil {
		t.Error("the id length CHECK is gone")
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO runs (id, device_id, kind, trigger_kind, started_at, received_at, created_at, updated_at)
		VALUES ('01JRUN000000000000000000D1', 'no-such-device', 'backup', 'schedule', ?, ?, 1, 1)`, tsStart, tsStart); err == nil {
		t.Error("the foreign key to devices is gone (or not enforced)")
	}
	if n := count(t, conn, `SELECT count(*) FROM pragma_foreign_key_list('runs')`); n != 1 {
		t.Errorf("runs has %d foreign keys, want 1", n)
	}
	if n := count(t, conn, `PRAGMA foreign_keys`); n != 1 {
		t.Errorf("foreign_keys = %d after the migration, want 1", n)
	}
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("foreign_key_check reports a violation")
	}
	_ = rows.Close()

	// ON DELETE CASCADE from devices survived the rebuild.
	mustExec(t, conn, `DELETE FROM devices WHERE id = ?`, devID)
	if n := count(t, conn, `SELECT count(*) FROM runs`); n != 0 {
		t.Errorf("%d runs survived deleting their device; the cascade is gone", n)
	}

	// Bookkeeping: both versions recorded with the embedded checksums.
	ms, _ := List()
	for _, m := range ms[:2] {
		var sum, name string
		if err := conn.QueryRowContext(ctx, `SELECT name, checksum FROM schema_migrations WHERE version = ?`, m.Version).Scan(&name, &sum); err != nil {
			t.Fatal(err)
		}
		if name != m.Name || sum != m.Checksum {
			t.Errorf("schema_migrations %d = (%s, %s), want (%s, %s)", m.Version, name, sum, m.Name, m.Checksum)
		}
	}
}

// Reapplying on a database that already has runs in it changes nothing.
func TestMigration0002IsIdempotentOnReopen(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := Apply(ctx, conn, fixedNow); err != nil {
		t.Fatal(err)
	}
	seedV1(t, conn)
	before := rowsOf(t, conn, `SELECT * FROM runs ORDER BY id`)
	again, err := Apply(ctx, conn, fixedNow)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Apply = %v, %v", again, err)
	}
	if got := rowsOf(t, conn, `SELECT * FROM runs ORDER BY id`); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatal("reapplying changed the runs")
	}
}

// A migration that leaves a foreign-key violation is rolled back, even on a
// connection that does not enforce foreign keys (where COMMIT would not notice).
func TestForeignKeyCheckAbortsAMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fk.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := applyUpTo(ctx, conn, fixedNow, 1); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, `PRAGMA foreign_keys`); n != 0 {
		t.Fatalf("test connection enforces foreign keys (%d)", n)
	}
	bad := Migration{
		Version: 2, Name: "0002_orphan", Checksum: "x",
		SQL: `INSERT INTO runs (id, device_id, kind, trigger_kind, started_at, received_at, created_at, updated_at)
		      VALUES ('01JRUN000000000000000000E1', 'no-such-device', 'backup', 'schedule', 1790000000000, 1790000000000, 1, 1);`,
	}
	_, err = applyOne(ctx, conn, bad, map[int]Migration{2: bad}, fixedNow)
	if err == nil || !strings.Contains(err.Error(), "foreign key violations") || !strings.Contains(err.Error(), "runs:1") {
		t.Fatalf("applyOne = %v, want a foreign key violation naming runs", err)
	}
	if strings.Contains(err.Error(), "no-such-device") || strings.Contains(err.Error(), "01JRUN") {
		t.Fatalf("the error carries row contents: %v", err)
	}
	if n := count(t, conn, `SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("the failed migration left %d rows", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM schema_migrations WHERE version = 2`); n != 0 {
		t.Fatal("the failed migration was recorded")
	}
	// The connection is usable (no transaction left open).
	mustExec(t, conn, `SELECT 1`)
}

func TestEmbeddedFilesIncludeEveryMigration(t *testing.T) {
	ms, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 4 || ms[1].Name != "0002_unsupported_file_class" || ms[2].Name != "0003_fleet_start_lease" || ms[3].Name != "0004_m2w1a" {
		t.Fatalf("migrations = %+v", ms)
	}
	for _, name := range []string{"0002_unsupported_file_class.sql", "0003_fleet_start_lease.sql", "0004_m2w1a.sql"} {
		b, err := fs.ReadFile(Files(), name)
		if err != nil || strings.Contains(string(b), "\r") {
			t.Fatalf("%s unreadable or has CRLF: %v", name, err)
		}
	}
}
