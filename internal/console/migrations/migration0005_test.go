package migrations

import (
	"context"
	"path/filepath"
	"testing"
)

// Migration 0005 (M3-T3): saved_views gains audience. A view that was shared at
// version 4 gets 'all' (it disappears from customer-scoped accounts: the safe
// default); a private view keeps no audience; the column refuses anything that
// is not NULL, 'all' or a JSON array.
func TestMigrate4To5GivesSharedViewsTheAudienceAllCustomers(t *testing.T) {
	ctx := context.Background()
	_, conn := openConn(t, filepath.Join(t.TempDir(), "m.db"))
	if applied, err := applyUpTo(ctx, conn, fixedNow, 4); err != nil || len(applied) != 4 {
		t.Fatalf("apply to version 4 = %v, %v", applied, err)
	}
	ins := `INSERT INTO saved_views (id, owner_id, name, entity, shared, created_at, updated_at) VALUES (?, NULL, ?, 'devices', ?, 1, 1)`
	for _, v := range []struct {
		id, name string
		shared   int
	}{{"01JVIEW0000000000000000SH1", "Shared one", 1}, {"01JVIEW0000000000000000PR1", "Private one", 0}} {
		if _, err := conn.ExecContext(ctx, ins, v.id, v.name, v.shared); err != nil {
			t.Fatal(err)
		}
	}
	if applied, err := applyUpTo(ctx, conn, fixedNow, 5); err != nil || len(applied) != 1 || applied[0] != 5 {
		t.Fatalf("apply 5 = %v, %v", applied, err)
	}
	if n := count(t, conn, `SELECT count(*) FROM saved_views WHERE id = '01JVIEW0000000000000000SH1' AND audience = 'all'`); n != 1 {
		t.Error("the view shared at version 4 did not get the audience 'all'")
	}
	if n := count(t, conn, `SELECT count(*) FROM saved_views WHERE id = '01JVIEW0000000000000000PR1' AND audience IS NULL`); n != 1 {
		t.Error("the private view got an audience")
	}
	for _, bad := range []string{`everyone`, `{"a":1}`, `"all"`} {
		if _, err := conn.ExecContext(ctx, `UPDATE saved_views SET audience = ? WHERE id = '01JVIEW0000000000000000SH1'`, bad); err == nil {
			t.Errorf("audience %s was accepted", bad)
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE saved_views SET audience = '["01JCUST000000000000000000A"]' WHERE id = '01JVIEW0000000000000000SH1'`); err != nil {
		t.Errorf("a JSON array of ids was refused: %v", err)
	}
}
