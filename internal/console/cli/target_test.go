package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func targetCap(t *testing.T, dir string) int {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "data", "console.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tg, err := st.DefaultStorageTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return tg.MaxConcurrentBackups
}

// The storage-target edit path of M1: init takes the cap of the first target,
// target list shows it, target set changes it and audits the change (ADR 0005).
func TestTargetCapViaInitListAndSet(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--max-concurrent-backups", "3")...); code != 0 {
		t.Fatal(e)
	}
	if n := targetCap(t, dir); n != 3 {
		t.Fatalf("cap after init = %d, want 3", n)
	}
	cfg := filepath.Join(dir, "config.json")
	code, out, errb := run(t, "", "target", "list", "--config", cfg)
	if code != 0 || !strings.Contains(out, "Default storage (default)") || !strings.Contains(out, "max_concurrent_backups 3") || !strings.Contains(out, "live leases 0") {
		t.Fatalf("list: %d\n%s\n%s", code, out, errb)
	}
	code, out, errb = run(t, "", "target", "set", "--config", cfg, "--max-concurrent-backups", "0")
	if code != 0 || !strings.Contains(out, "max_concurrent_backups 0 (unlimited) (was 3)") {
		t.Fatalf("set default: %d\n%s\n%s", code, out, errb)
	}
	if n := targetCap(t, dir); n != 0 {
		t.Fatalf("cap = %d, want 0", n)
	}
	code, out, errb = run(t, "", "target", "set", "--config", cfg, "--name", "default STORAGE", "--max-concurrent-backups", "12")
	if code != 0 || !strings.Contains(out, "max_concurrent_backups 12") {
		t.Fatalf("set by name: %d\n%s\n%s", code, out, errb)
	}
	if n := targetCap(t, dir); n != 12 {
		t.Fatalf("cap = %d, want 12", n)
	}
	// Audited, with the old and the new value.
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "data", "console.db")})
	if err != nil {
		t.Fatal(err)
	}
	var details string
	if err := st.Read(ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT details FROM audit_events WHERE action = 'storage_target.update' ORDER BY occurred_at DESC, id DESC LIMIT 1`).Scan(&details)
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if details != `{"max_concurrent_backups":{"from":0,"to":12}}` {
		t.Fatalf("audit details = %s", details)
	}

	for name, args := range map[string][]string{
		"no subcommand":     {"target"},
		"unknown":           {"target", "drop", "--config", cfg},
		"set without a cap": {"target", "set", "--config", cfg},
		"negative cap":      {"target", "set", "--config", cfg, "--max-concurrent-backups", "-2"},
	} {
		if code, _, _ := run(t, "", args...); code != ExitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, ExitUsage)
		}
	}
	if code, _, errb := run(t, "", "target", "set", "--config", cfg, "--name", "nope", "--max-concurrent-backups", "1"); code != ExitFailed || !strings.Contains(errb, "no storage target with that name") {
		t.Errorf("unknown name: %d %s", code, errb)
	}
	if code, _, _ := run(t, "", initArgs(t.TempDir(), "--master-key-copy-confirmed", "--max-concurrent-backups", "-1")...); code != ExitUsage {
		t.Errorf("init with a negative cap: exit %d, want %d", code, ExitUsage)
	}
}

// M1-FIX3 item 8, M1-FIX4 item 7: a cap change that cannot be audited fails
// loudly (exit 1 and stderr) and is never made: the cap and its audit row are
// one transaction, so there is no change-then-undo that could itself fail.
func TestTargetSetFailsWhenTheAuditFails(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--max-concurrent-backups", "3")...); code != 0 {
		t.Fatal(e)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "data", "console.db")})
	if err != nil {
		t.Fatal(err)
	}
	// The audit table refuses every new row (a full disk, a broken schema), and
	// any write that would set the cap back to 3 is refused too: a
	// change-then-undo would be left at 9, unaudited.
	if err := st.Write(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`CREATE TRIGGER fix3_audit_refused BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE TRIGGER fix4_undo_refused BEFORE UPDATE ON storage_targets WHEN NEW.max_concurrent_backups = 3 BEGIN SELECT RAISE(ABORT, 'undo refused by the test'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	code, out, errb := run(t, "", "target", "set", "--config", filepath.Join(dir, "config.json"), "--max-concurrent-backups", "9")
	if code != ExitFailed {
		t.Fatalf("exit %d, want %d (an unaudited cap change)\nstdout %s\nstderr %s", code, ExitFailed, out, errb)
	}
	if !strings.Contains(errb, "could not be audited, so it was not made") || !strings.Contains(errb, "unchanged (3)") {
		t.Fatalf("stderr = %q, want the audit failure and the unchanged cap", errb)
	}
	if strings.Contains(out, "max_concurrent_backups 9") {
		t.Fatalf("stdout reports the change as made: %q", out)
	}
	if n := targetCap(t, dir); n != 3 {
		t.Fatalf("cap = %d after an unaudited change, want it unchanged (3)", n)
	}
	// No storage_target.update row, and the target row was not touched.
	st, err = store.Open(ctx, store.Options{Path: filepath.Join(dir, "data", "console.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.Read(ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'storage_target.update'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("storage_target.update audit rows = %d, %v", n, err)
	}
}
