package migrations

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// ADR 0019: 0007 adds the purge columns to devices. The rules below the store: a
// purge can be requested only on a retired device, the detail columns only mean
// something while a purge is requested, and the admin who asked is forgotten (not
// the device) when that account goes.
func TestMigration0007PurgeColumnsAndTheirRules(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := applyUpTo(ctx, conn, fixedNow, 6); err != nil {
		t.Fatal(err)
	}
	const (
		admin   = "01JADMIN00000000000000000A"
		cust    = "01JCUST000000000000000000A"
		active  = "01JDEV0000000000000000ACTV"
		retired = "01JDEV00000000000000000RET"
	)
	seed := []string{
		`INSERT INTO storage_targets (id, name, provider, endpoint, bucket, gateway_url, credential_ref, created_at, updated_at) VALUES ('01JTARGET0000000000000000A', 'Example', 'wasabi', 's3.example.invalid', 'b', 'https://gateway.example.invalid', 'REF', 1700000000000, 1700000000000)`,
		`INSERT INTO customers (id, name, slug, storage_target_id, created_at, updated_at) VALUES ('` + cust + `', 'Example', 'example', '01JTARGET0000000000000000A', 1700000000000, 1700000000000)`,
		`INSERT INTO admin_users (id, username, email, role, password_hash, created_at, updated_at) VALUES ('` + admin + `', 'root', 'root@example.invalid', 'owner', 'x', 1700000000000, 1700000000000)`,
		`INSERT INTO devices (id, customer_id, hostname, enrolled_at, enrolled_via, repo_path, created_at, updated_at) VALUES ('` + active + `', '` + cust + `', 'HOST-A', 1700000000000, 'token', 'fleet/` + active + `/', 1700000000000, 1700000000000)`,
		`INSERT INTO devices (id, customer_id, hostname, enrolled_at, enrolled_via, retired_at, retired_reason, repo_path, created_at, updated_at) VALUES ('` + retired + `', '` + cust + `', 'HOST-R', 1700000000000, 'token', 1700000000001, 'gone', 'fleet/` + retired + `/', 1700000000000, 1700000000000)`,
	}
	for _, q := range seed {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed at version 6: %v", err)
		}
	}
	if applied, err := applyUpTo(ctx, conn, fixedNow, 7); err != nil || len(applied) != 1 || applied[0] != 7 {
		t.Fatalf("applied %v, %v", applied, err)
	}
	if n := count(t, conn, `SELECT count(*) FROM devices WHERE purge_requested_at IS NOT NULL OR purge_requested_by IS NOT NULL OR purge_reason IS NOT NULL OR purge_last_error_class IS NOT NULL OR purge_last_attempt_at IS NOT NULL`); n != 0 {
		t.Errorf("%d existing devices came out of the migration marked for purge", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM devices`); n != 2 {
		t.Errorf("%d devices after the migration, want the 2 seeded", n)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_devices_purge_requested_by'`); n != 1 {
		t.Error("the index on devices.purge_requested_by is missing (every foreign-key column leads an index)")
	}
	// The fresh-MFA column of ADR 0019 is the one 0001 made; 0007 leaves sessions alone.
	if n := count(t, conn, `SELECT count(*) FROM pragma_table_info('sessions') WHERE name = 'mfa_verified_at' AND type = 'INTEGER'`); n != 1 {
		t.Error("sessions.mfa_verified_at (INTEGER) is missing")
	}

	exec := func(q string, args ...any) error {
		_, err := conn.ExecContext(ctx, q, args...)
		return err
	}
	const request = `UPDATE devices SET purge_requested_at = ?, purge_requested_by = ?, purge_reason = ? WHERE id = ?`
	// Only a retired device can be marked.
	if err := exec(request, 1750000000000, admin, "lab clean-up", active); err == nil {
		t.Error("a purge was requested on an ACTIVE device: the CHECK must refuse it")
	}
	if err := exec(request, 999, admin, "lab clean-up", retired); err == nil {
		t.Error("a purge time before the schema's floor was accepted")
	}
	if err := exec(request, 1750000000000, admin, "lab clean-up", retired); err != nil {
		t.Fatalf("a purge on a retired device: %v", err)
	}
	// The detail columns follow the request.
	if err := exec(`UPDATE devices SET purge_last_error_class = 'storage_unreachable', purge_last_attempt_at = 1750000000500 WHERE id = ?`, retired); err != nil {
		t.Errorf("recording a failed attempt on a purging device: %v", err)
	}
	for _, q := range []string{
		`UPDATE devices SET purge_reason = 'x' WHERE id = '` + active + `'`,
		`UPDATE devices SET purge_last_error_class = 'x' WHERE id = '` + active + `'`,
		`UPDATE devices SET purge_last_attempt_at = 1750000000500 WHERE id = '` + active + `'`,
		`UPDATE devices SET purge_requested_by = '` + admin + `' WHERE id = '` + active + `'`,
	} {
		if err := exec(q); err == nil {
			t.Errorf("a purge detail without a purge request was accepted: %s", strings.SplitN(q, " WHERE", 2)[0])
		}
	}
	// A purging device cannot be un-retired.
	if err := exec(`UPDATE devices SET retired_at = NULL, retired_reason = NULL WHERE id = ?`, retired); err == nil {
		t.Error("a purging device was un-retired: the CHECK must refuse it")
	}
	// An unknown admin id is a broken reference; deleting the real one forgets who asked, not the purge.
	if err := exec(`UPDATE devices SET purge_requested_by = '01JNOSUCHADMIN00000000000' WHERE id = ?`, retired); err == nil {
		t.Error("purge_requested_by accepted an account that does not exist")
	}
	if err := exec(`DELETE FROM admin_users WHERE id = ?`, admin); err != nil {
		t.Fatalf("deleting the requesting admin: %v", err)
	}
	if n := count(t, conn, `SELECT count(*) FROM devices WHERE id = ? AND purge_requested_at IS NOT NULL AND purge_requested_by IS NULL AND purge_reason = 'lab clean-up'`, retired); n != 1 {
		t.Error("deleting the requesting admin must null purge_requested_by and keep the purge")
	}
}
