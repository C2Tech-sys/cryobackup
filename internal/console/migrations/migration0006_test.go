package migrations

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// M3-T6, ADR 0016: 0006 adds enrollment_tokens.deleted_at. The rule below the
// store: only a token that can no longer enroll may be hidden.
func TestMigration0006HidesOnlyTokensThatCannotEnroll(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if _, err := applyUpTo(ctx, conn, fixedNow, 5); err != nil {
		t.Fatal(err)
	}
	seed := []string{
		`INSERT INTO storage_targets (id, name, provider, endpoint, bucket, gateway_url, credential_ref, created_at, updated_at) VALUES ('01JTARGET0000000000000000A', 'Example', 'wasabi', 's3.example.invalid', 'b', 'https://gateway.example.invalid', 'REF', 1700000000000, 1700000000000)`,
		`INSERT INTO customers (id, name, slug, storage_target_id, created_at, updated_at) VALUES ('01JCUST000000000000000000A', 'Example', 'example', '01JTARGET0000000000000000A', 1700000000000, 1700000000000)`,
		`INSERT INTO enrollment_tokens (id, customer_id, token_hash, expires_at, created_at, updated_at) VALUES
		 ('01JTOKEN00000000000000LIVE', '01JCUST000000000000000000A', '` + strings.Repeat("a", 64) + `', 1800000000000, 1700000000000, 1700000000000),
		 ('01JTOKEN00000000000000OLD0', '01JCUST000000000000000000A', '` + strings.Repeat("b", 64) + `', 1700000000001, 1700000000000, 1700000000000)`,
		`INSERT INTO enrollment_tokens (id, customer_id, token_hash, expires_at, revoked_at, created_at, updated_at) VALUES
		 ('01JTOKEN00000000000000REV0', '01JCUST000000000000000000A', '` + strings.Repeat("c", 64) + `', 1800000000000, 1700000000000, 1700000000000, 1700000000000)`,
	}
	for _, q := range seed {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed at version 5: %v", err)
		}
	}
	if applied, err := applyUpTo(ctx, conn, fixedNow, 6); err != nil || len(applied) != 1 || applied[0] != 6 {
		t.Fatalf("applied %v, %v", applied, err)
	}
	if n := count(t, conn, `SELECT count(*) FROM enrollment_tokens WHERE deleted_at IS NOT NULL`); n != 0 {
		t.Errorf("%d existing tokens came out of the migration hidden", n)
	}
	const hide = `UPDATE enrollment_tokens SET deleted_at = 1750000000000 WHERE id = ?`
	if _, err := conn.ExecContext(ctx, hide, "01JTOKEN00000000000000LIVE"); err == nil {
		t.Error("a live token was hidden: the CHECK must refuse it")
	}
	for _, id := range []string{"01JTOKEN00000000000000OLD0", "01JTOKEN00000000000000REV0"} {
		if _, err := conn.ExecContext(ctx, hide, id); err != nil {
			t.Errorf("hiding %s: %v", id, err)
		}
	}
}
