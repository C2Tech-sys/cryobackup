package migrations

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// ADR 0020: 0008 adds alarm_episodes (one open episode per subject and alarm type) and seeds
// digest_enabled. It leaves the three tables of 0001 that M4 puts to use exactly as they were.
func TestMigration0008EpisodesTableAndItsRules(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := applyUpTo(ctx, conn, fixedNow, 7); err != nil {
		t.Fatal(err)
	}
	const (
		cust = "01JCUST000000000000000000A"
		dev  = "01JDEV0000000000000000ACTV"
	)
	seed := []string{
		`INSERT INTO storage_targets (id, name, provider, endpoint, bucket, gateway_url, credential_ref, created_at, updated_at) VALUES ('01JTARGET0000000000000000A', 'Example', 'wasabi', 's3.example.invalid', 'b', 'https://gateway.example.invalid', 'REF', 1700000000000, 1700000000000)`,
		`INSERT INTO customers (id, name, slug, storage_target_id, created_at, updated_at) VALUES ('` + cust + `', 'Example', 'example', '01JTARGET0000000000000000A', 1700000000000, 1700000000000)`,
		`INSERT INTO devices (id, customer_id, hostname, enrolled_at, enrolled_via, repo_path, created_at, updated_at) VALUES ('` + dev + `', '` + cust + `', 'HOST-A', 1700000000000, 'token', 'fleet/` + dev + `/', 1700000000000, 1700000000000)`,
		`INSERT INTO alarm_acks (id, subject_type, device_id, alarm_type, acknowledged_at, created_at, updated_at) VALUES ('01JACK00000000000000000001', 'device', '` + dev + `', 'failed', 1700000000000, 1700000000000, 1700000000000)`,
		`INSERT INTO notification_rules (id, scope, name, channel, destination, created_at, updated_at) VALUES ('01JRULE0000000000000000001', 'global', 'Ops', 'email', 'ops@example.test', 1700000000000, 1700000000000)`,
		`INSERT INTO notification_log (id, rule_id, dedupe_key, kind, subject_type, channel, result, sent_at, created_at, updated_at) VALUES ('01JLOG00000000000000000001', '01JRULE0000000000000000001', 'k1', 'digest', 'console', 'email', 'sent', 1700000000000, 1700000000000, 1700000000000)`,
		// A value an operator might already hold must survive the seed.
		`INSERT INTO settings (key, value, created_at, updated_at) VALUES ('digest_enabled', 'false', 1700000000000, 1700000000000)`,
	}
	for _, q := range seed {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed at version 7: %v", err)
		}
	}
	tableSQL := func(name string) string {
		var s string
		if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&s); err != nil {
			t.Fatalf("sql of %s: %v", name, err)
		}
		return s
	}
	before := map[string]string{}
	for _, n := range []string{"alarm_acks", "notification_rules", "notification_log"} {
		before[n] = tableSQL(n)
	}
	if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE name = 'alarm_episodes'`); n != 0 {
		t.Fatal("alarm_episodes exists before 0008")
	}

	if applied, err := applyUpTo(ctx, conn, fixedNow, 8); err != nil || len(applied) != 1 || applied[0] != 8 {
		t.Fatalf("applied %v, %v", applied, err)
	}
	for n, was := range before {
		if now := tableSQL(n); now != was {
			t.Errorf("0008 changed the definition of %s", n)
		}
	}
	for _, q := range []string{`alarm_acks`, `notification_rules`, `notification_log`} {
		if n := count(t, conn, `SELECT count(*) FROM `+q); n != 1 {
			t.Errorf("%s has %d rows after the migration, want the 1 seeded", q, n)
		}
	}
	for _, idx := range []string{"idx_alarm_episodes_device", "idx_alarm_episodes_customer", "uq_alarm_episodes_open_device", "uq_alarm_episodes_open_customer",
		"idx_alarm_episodes_open", "idx_alarm_episodes_unnotified", "idx_alarm_episodes_recovery_due"} {
		if n := count(t, conn, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx); n != 1 {
			t.Errorf("index %s is missing", idx)
		}
	}
	if v := valueOf(t, conn, `SELECT value FROM settings WHERE key = 'digest_enabled'`); v != "false" {
		t.Errorf("digest_enabled = %s after the migration: an operator's stored value must survive", v)
	}

	exec := func(q string, args ...any) error {
		_, err := conn.ExecContext(ctx, q, args...)
		return err
	}
	const open = `INSERT INTO alarm_episodes (id, subject_type, device_id, customer_id, alarm_type, opened_at, state_since, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1750000000000, 1749999000000, 1750000000000, 1750000000000)`
	if err := exec(open, "01JEP00000000000000000000A", "device", dev, nil, "FAILED"); err != nil {
		t.Fatalf("opening a device episode: %v", err)
	}
	// One OPEN episode per subject and alarm type; another type is fine.
	if err := exec(open, "01JEP00000000000000000000B", "device", dev, nil, "FAILED"); err == nil {
		t.Error("a second OPEN episode for the same device and alarm type was accepted")
	}
	if err := exec(open, "01JEP00000000000000000000C", "device", dev, nil, "STALE"); err != nil {
		t.Errorf("a different alarm type on the same device: %v", err)
	}
	// Exactly one subject.
	if err := exec(open, "01JEP00000000000000000000D", "device", dev, cust, "UNSEEN"); err == nil {
		t.Error("an episode naming both a device and a customer was accepted")
	}
	if err := exec(open, "01JEP00000000000000000000E", "customer", nil, nil, "MISSING"); err == nil {
		t.Error("a customer episode without a customer was accepted")
	}
	if err := exec(open, "01JEP00000000000000000000F", "customer", nil, cust, "MISSING"); err != nil {
		t.Errorf("opening a customer episode: %v", err)
	}
	if err := exec(open, "01JEP00000000000000000000G", "customer", nil, cust, "MISSING"); err == nil {
		t.Error("a second OPEN customer episode for the same alarm type was accepted")
	}
	// A recovery notice only follows a close; the times are epoch milliseconds.
	if err := exec(`UPDATE alarm_episodes SET recovery_notified_at = 1750000100000 WHERE id = '01JEP00000000000000000000A'`); err == nil {
		t.Error("a recovery notice on an OPEN episode was accepted")
	}
	if err := exec(`UPDATE alarm_episodes SET closed_at = 12 WHERE id = '01JEP00000000000000000000A'`); err == nil {
		t.Error("a closed_at in seconds was accepted")
	}
	if err := exec(`UPDATE alarm_episodes SET closed_at = 1750000100000, notified_at = 1750000050000, recovery_notified_at = 1750000200000 WHERE id = '01JEP00000000000000000000A'`); err != nil {
		t.Errorf("closing an episode with its recovery notice: %v", err)
	}
	// Closed, the same alarm may open again.
	if err := exec(open, "01JEP00000000000000000000H", "device", dev, nil, "FAILED"); err != nil {
		t.Errorf("a new episode after the old one closed: %v", err)
	}
	if err := exec(open, "01JEP0000000000000000000", "device", dev, nil, "BLANK"); err == nil {
		t.Error("an id that is not 26 characters was accepted")
	}
	if err := exec(open, "01JEP00000000000000000000J", "device", dev, nil, ""); err == nil {
		t.Error("an empty alarm type was accepted")
	}
	if err := exec(open, "01JEP00000000000000000000K", "device", "01JNOSUCHDEVICE0000000000", nil, "BLANK"); err == nil {
		t.Error("an episode for a device that does not exist was accepted")
	}
	// Deleting a device or a customer takes its episodes with it.
	if err := exec(`DELETE FROM devices WHERE id = ?`, dev); err != nil {
		t.Fatalf("deleting the device: %v", err)
	}
	if n := count(t, conn, `SELECT count(*) FROM alarm_episodes WHERE device_id IS NOT NULL`); n != 0 {
		t.Errorf("%d device episodes outlived their device", n)
	}
	if err := exec(`DELETE FROM customers WHERE id = ?`, cust); err != nil {
		t.Fatalf("deleting the customer: %v", err)
	}
	if n := count(t, conn, `SELECT count(*) FROM alarm_episodes`); n != 0 {
		t.Errorf("%d episodes outlived their customer", n)
	}
	// The migration file is plain LF, like the others.
	raw, err := fs.ReadFile(Files(), "0008_m4_alerts.sql")
	if err != nil || strings.Contains(string(raw), "") {
		t.Errorf("0008_m4_alerts.sql unreadable or has CRLF line endings: %v", err)
	}
}

// With no stored value the seed makes the digest default to on (ADR 0020 decision 8).
func TestMigration0008SeedsDigestEnabledOn(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := Apply(ctx, conn, fixedNow); err != nil {
		t.Fatal(err)
	}
	if v := valueOf(t, conn, `SELECT value FROM settings WHERE key = 'digest_enabled'`); v != "true" {
		t.Errorf("digest_enabled = %s on a fresh database, want true", v)
	}
	if v := valueOf(t, conn, `SELECT value FROM settings WHERE key = 'digest_local_time'`); v != `"07:00"` {
		t.Errorf("digest_local_time = %s, want the 0001 default", v)
	}
}

func valueOf(t *testing.T, conn *sql.Conn, q string) string {
	t.Helper()
	var v string
	if err := conn.QueryRowContext(context.Background(), q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}
