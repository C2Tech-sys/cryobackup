package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// insertDeferred inserts a minimal deferred run with the given reason.
func insertDeferred(conn *sql.Conn, id string, reason any) error {
	_, err := conn.ExecContext(context.Background(),
		`INSERT INTO runs (id, device_id, kind, trigger_kind, started_at, finished_at, received_at, outcome, deferred_reason, created_at, updated_at)
		 VALUES (?, ?, 'backup', 'schedule', ?, ?, ?, 'deferred', ?, ?, ?)`,
		id, devID, tsStart+500000, tsStart+500000, tsStart+500100, reason, tsStart+500100, tsStart+500100)
	return err
}

// insertLease inserts a start lease for the seeded device and target.
func insertLease(conn *sql.Conn, runID, target string, granted, until int64) error {
	_, err := conn.ExecContext(context.Background(),
		`INSERT INTO start_leases (run_id, device_id, storage_target_id, granted_at, lease_until, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`, runID, devID, target, granted, until, granted, granted)
	return err
}

const seededTarget = "01JSTORAGE000000000000000A"

// The version-2 database a release left behind, with runs and a storage target
// in it, migrates to version 3: every run row, index and constraint intact,
// fleet_cap accepted, the target keeps its columns and gains
// max_concurrent_backups = 0 (unlimited, so the upgrade changes nothing), and
// start_leases exists with its foreign keys and indexes.
func TestMigrate2To3KeepsDataAndAddsTheLease(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))

	applied, err := applyUpTo(ctx, conn, fixedNow, 2)
	if err != nil || len(applied) != 2 {
		t.Fatalf("apply to version 2 = %v, %v", applied, err)
	}
	seedV1(t, conn)
	if err := insertRun(conn, "01JRUN000000000000000000A5", "unsupported_file"); err != nil {
		t.Fatalf("a version-2 row: %v", err)
	}

	// Preconditions the rebuild relies on, stated on the version-2 schema.
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE sql LIKE '%REFERENCES runs%' OR sql LIKE '%REFERENCES "runs"%'`); n != 0 {
		t.Fatalf("%d objects reference runs: DROP TABLE runs would cascade into them with foreign keys on", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type IN ('trigger','view') AND sql LIKE '%runs%'`); n != 0 {
		t.Fatalf("%d triggers or views mention runs; the rebuild does not recreate them", n)
	}
	// The red state the migration cures: version 2 has no fleet_cap, no cap column, no lease table.
	if err := insertDeferred(conn, "01JRUN000000000000000000Z0", "fleet_cap"); err == nil {
		t.Fatal("version 2 accepted deferred_reason fleet_cap")
	}
	if n := count(t, conn, `SELECT count(*) FROM pragma_table_info('storage_targets') WHERE name = 'max_concurrent_backups'`); n != 0 {
		t.Fatal("version 2 already has max_concurrent_backups")
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE name = 'start_leases'`); n != 0 {
		t.Fatal("version 2 already has start_leases")
	}

	const q = `SELECT * FROM runs ORDER BY id`
	rowsBefore := rowsOf(t, conn, q)
	if len(rowsBefore) != 5 {
		t.Fatalf("seeded %d runs", len(rowsBefore))
	}
	idxBefore := rowsOf(t, conn, `SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'runs' AND sql IS NOT NULL ORDER BY name`)
	if len(idxBefore) != 8 {
		t.Fatalf("version 2 has %d explicit runs indexes, want 8", len(idxBefore))
	}
	targetsBefore := rowsOf(t, conn, `SELECT * FROM storage_targets ORDER BY id`)
	devicesBefore := rowsOf(t, conn, `SELECT * FROM devices`)
	targetIdxBefore := rowsOf(t, conn, `SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'storage_targets' ORDER BY name`)

	applied, err = applyUpTo(ctx, conn, fixedNow, 3)
	if err != nil || len(applied) != 1 || applied[0] != 3 {
		t.Fatalf("apply 2 -> 3 = %v, %v", applied, err)
	}

	// Data: every column of every run row, the other tables, the indexes.
	if got := rowsOf(t, conn, q); strings.Join(got, "\n") != strings.Join(rowsBefore, "\n") {
		t.Fatalf("runs rows changed by the migration:\nbefore %v\nafter  %v", rowsBefore, got)
	}
	if got := rowsOf(t, conn, `SELECT * FROM devices`); strings.Join(got, "\n") != strings.Join(devicesBefore, "\n") {
		t.Fatal("devices changed by the migration")
	}
	if got := rowsOf(t, conn, `SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'runs' AND sql IS NOT NULL ORDER BY name`); strings.Join(got, "\n") != strings.Join(idxBefore, "\n") {
		t.Fatalf("runs indexes changed:\nbefore %v\nafter  %v", idxBefore, got)
	}
	if got := rowsOf(t, conn, `SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'storage_targets' ORDER BY name`); strings.Join(got, "\n") != strings.Join(targetIdxBefore, "\n") {
		t.Fatal("storage_targets indexes changed")
	}
	// The target: the old columns unchanged, the new one 0 (unlimited).
	targetsAfter := rowsOf(t, conn, `SELECT * FROM storage_targets ORDER BY id`)
	if len(targetsAfter) != 1 || !strings.HasPrefix(strings.TrimSuffix(targetsAfter[0], "}"), strings.TrimSuffix(targetsBefore[0], "}")) {
		t.Fatalf("storage_targets changed:\nbefore %v\nafter  %v", targetsBefore, targetsAfter)
	}
	if n := count(t, conn, `SELECT max_concurrent_backups FROM storage_targets WHERE id = ?`, seededTarget); n != 0 {
		t.Fatalf("max_concurrent_backups of an existing target = %d, want 0 (unlimited)", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE name = 'runs_new'`); n != 0 {
		t.Fatal("runs_new left behind")
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`); n != 35 {
		t.Fatalf("%d tables after the migration, want 35", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL`); n != 105 {
		t.Fatalf("%d explicit indexes after the migration, want 105", n)
	}

	// runs constraints: fleet_cap accepted, every old reason still, a bogus one and the couplings still refused.
	if err := insertDeferred(conn, "01JRUN000000000000000000B1", "fleet_cap"); err != nil {
		t.Fatalf("version 3 rejected fleet_cap: %v", err)
	}
	for i, reason := range []string{"lock", "gateway", "battery", "metered", "window", "conditions"} {
		if err := insertDeferred(conn, "01JRUN00000000000000000OL"+string(rune('A'+i)), reason); err != nil {
			t.Errorf("version 3 rejected the old reason %s: %v", reason, err)
		}
	}
	for _, bogus := range []string{"fleet", "fleetcap", "Fleet_Cap", "fleet_cap "} {
		if err := insertDeferred(conn, "01JRUN000000000000000000C1", bogus); err == nil {
			t.Errorf("version 3 accepted the bogus reason %q", bogus)
		}
	}
	if err := insertDeferred(conn, "01JRUN000000000000000000C2", nil); err == nil {
		t.Error("a deferred run without a reason was accepted: the coupling CHECK is gone")
	}
	if err := insertRun(conn, "01JRUN000000000000000000B2", "unsupported_file"); err != nil {
		t.Errorf("version 3 rejected the 0002 class: %v", err)
	}
	if n := count(t, conn, `SELECT count(*) FROM pragma_foreign_key_list('runs')`); n != 1 {
		t.Errorf("runs has %d foreign keys, want 1", n)
	}

	// storage_targets: the cap CHECK holds.
	if _, err := conn.ExecContext(ctx, `UPDATE storage_targets SET max_concurrent_backups = -1 WHERE id = ?`, seededTarget); err == nil {
		t.Error("max_concurrent_backups accepted -1")
	}
	mustExec(t, conn, `UPDATE storage_targets SET max_concurrent_backups = 3 WHERE id = ?`, seededTarget)

	// start_leases: constraints, foreign keys with cascade.
	if err := insertLease(conn, "01JRUN000000000000000000L1", seededTarget, tsStart, tsStart+900000); err != nil {
		t.Fatalf("a valid lease was refused: %v", err)
	}
	if err := insertLease(conn, "01JRUN000000000000000000L1", seededTarget, tsStart, tsStart+900000); err == nil {
		t.Error("a second lease for the same run id was accepted")
	}
	if err := insertLease(conn, "short", seededTarget, tsStart, tsStart+900000); err == nil {
		t.Error("the run_id length CHECK is missing")
	}
	if err := insertLease(conn, "01JRUN000000000000000000L2", seededTarget, tsStart, tsStart); err == nil {
		t.Error("a lease that ends when it starts was accepted")
	}
	if err := insertLease(conn, "01JRUN000000000000000000L3", seededTarget, 1790000, 1790000+900); err == nil {
		t.Error("a seconds timestamp was accepted for granted_at")
	}
	if err := insertLease(conn, "01JRUN000000000000000000L4", "01JNOSUCHTARGET00000000000", tsStart, tsStart+1); err == nil {
		t.Error("a lease for an unknown storage target was accepted")
	}
	if n := count(t, conn, `SELECT count(*) FROM pragma_foreign_key_list('start_leases') WHERE "on_delete" = 'CASCADE'`); n != 2 {
		t.Errorf("start_leases has %d cascading foreign keys, want 2", n)
	}
	for _, idx := range []string{"idx_start_leases_target_until", "idx_start_leases_device"} {
		if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ? AND tbl_name = 'start_leases'`, idx); n != 1 {
			t.Errorf("index %s missing", idx)
		}
	}
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("foreign_key_check reports a violation")
	}
	_ = rows.Close()

	// Deleting the device removes its runs and its lease.
	mustExec(t, conn, `DELETE FROM devices WHERE id = ?`, devID)
	if n := count(t, conn, `SELECT count(*) FROM runs`) + count(t, conn, `SELECT count(*) FROM start_leases`); n != 0 {
		t.Errorf("%d runs or leases survived deleting their device; the cascade is gone", n)
	}

	// Bookkeeping: all three versions recorded with the embedded checksums.
	ms, _ := List()
	for _, m := range ms[:3] {
		var sum, name string
		if err := conn.QueryRowContext(ctx, `SELECT name, checksum FROM schema_migrations WHERE version = ?`, m.Version).Scan(&name, &sum); err != nil {
			t.Fatal(err)
		}
		if name != m.Name || sum != m.Checksum {
			t.Errorf("schema_migrations %d = (%s, %s), want (%s, %s)", m.Version, name, sum, m.Name, m.Checksum)
		}
	}
}

// A version-1 database (the first release) migrates straight to version 3.
func TestMigrate1To3InOneOpen(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := applyUpTo(ctx, conn, fixedNow, 1); err != nil {
		t.Fatal(err)
	}
	seedV1(t, conn)
	before := rowsOf(t, conn, `SELECT * FROM runs ORDER BY id`)
	applied, err := applyUpTo(ctx, conn, fixedNow, 3)
	if err != nil || len(applied) != 2 || applied[0] != 2 || applied[1] != 3 {
		t.Fatalf("apply 1 -> 3 = %v, %v", applied, err)
	}
	if got := rowsOf(t, conn, `SELECT * FROM runs ORDER BY id`); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatal("runs changed across 1 -> 3")
	}
	if err := insertDeferred(conn, "01JRUN000000000000000000B1", "fleet_cap"); err != nil {
		t.Fatalf("fleet_cap after 1 -> 3: %v", err)
	}
}

// start_leases is not referenced by anything, and nothing new references runs
// (the next rebuild of runs relies on it).
func TestNothingReferencesRunsOrLeases(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := Apply(ctx, conn, fixedNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE sql LIKE '%REFERENCES runs%' OR sql LIKE '%REFERENCES "runs"%' OR sql LIKE '%REFERENCES start_leases%'`); n != 0 {
		t.Fatalf("%d objects reference runs or start_leases", n)
	}
}
