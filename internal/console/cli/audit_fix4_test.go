package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M1-FIX4 item 8: a CLI change whose audit row cannot be written fails loudly
// (exit 1, NOT AUDITED on stderr) and shows no secret it created.

// refuseAudit makes the audit table refuse every new row of these actions.
func refuseAudit(t *testing.T, dir string, actions ...string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "data", "console.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Write(ctx, func(tx *store.Tx) error {
		for _, a := range actions {
			if _, err := tx.Exec(`CREATE TRIGGER fix4_audit_refused_` + strings.ReplaceAll(a, ".", "_") + ` BEFORE INSERT ON audit_events WHEN NEW.action = '` + a +
				`' BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, dir, query string) int {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "data", "console.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.Read(ctx, func(tx *store.Tx) error { return tx.QueryRow(query).Scan(&n) }); err != nil {
		t.Fatal(err)
	}
	return n
}

func countText(t *testing.T, dir, query string) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "data", "console.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var s string
	if err := st.Read(ctx, func(tx *store.Tx) error { return tx.QueryRow(query).Scan(&s) }); err != nil {
		t.Fatal(err)
	}
	return s
}

func wantNotAudited(t *testing.T, what string, code int, out, errb string, stderrHas ...string) {
	t.Helper()
	if code != ExitFailed {
		t.Errorf("%s: exit %d, want %d for an unaudited change\nstdout %s\nstderr %s", what, code, ExitFailed, out, errb)
	}
	for _, s := range append([]string{"NOT AUDITED", "audit refused by the test"}, stderrHas...) {
		if !strings.Contains(errb, s) {
			t.Errorf("%s: stderr lacks %q: %s", what, s, errb)
		}
	}
}

func TestInitFailsLoudlyWhenTheAuditFails(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatal(e)
	}
	refuseAudit(t, dir, "console.init")
	code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--force", "--owner", "second-owner")...)
	wantNotAudited(t, "init", code, out, errb, "the console was set up")
	if strings.Contains(out, "Password (shown once") || !strings.Contains(out, "second-owner (its password is not shown") {
		t.Errorf("an unaudited init showed the owner's password, or did not say why it does not:\n%s", out)
	}
}

func TestTokenFailsLoudlyWhenTheAuditFails(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatal(e)
	}
	cfg := filepath.Join(dir, "config.json")

	// The customer is created but its audit row is refused: no token is minted.
	refuseAudit(t, dir, "customer.create")
	code, out, errb := run(t, "", "token", "--config", cfg, "--create-customer", "Acme Dental")
	wantNotAudited(t, "token --create-customer", code, out, errb, `customer "Acme Dental" was created`, "no token was minted")
	if strings.Contains(out, "never stored") || countRows(t, dir, `SELECT COUNT(*) FROM enrollment_tokens`) != 0 {
		t.Errorf("a token was minted after an unaudited customer:\n%s", out)
	}

	// The token is minted but its audit row is refused: it is not shown.
	refuseAudit(t, dir, "enrollment_token.create")
	code, out, errb = run(t, "", "token", "--config", cfg, "--customer", "acme-dental")
	wantNotAudited(t, "token --customer", code, out, errb, "it is not shown and expires unused")
	if strings.Contains(out, "never stored") || strings.Contains(out, "Expires:") {
		t.Errorf("an unaudited enrollment token was shown:\n%s", out)
	}
}

func TestUserCommandsFailLoudlyWhenTheAuditFails(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatal(e)
	}
	cfg := filepath.Join(dir, "config.json")
	refuseAudit(t, dir, "admin.create", "admin.password_reset", "admin.disable")

	code, out, errb := run(t, "", "user", "add", "--config", cfg, "--username", "tech1", "--role", "operator")
	wantNotAudited(t, "user add", code, out, errb, "account tech1 was created", "is not shown")
	if strings.Contains(out, "Password (shown once)") {
		t.Errorf("an unaudited account's generated password was shown:\n%s", out)
	}
	// passwd: the change and its audit row are one transaction, so a failed
	// audit leaves the old password in place (M1-FIX4 fix round, item 5).
	hash := func() string {
		return countText(t, dir, `SELECT password_hash FROM admin_users WHERE username = 'tech1'`)
	}
	before := hash()
	code, out, errb = run(t, "a-long-enough-password\n", "user", "passwd", "--config", cfg, "--username", "tech1", "--password-stdin")
	wantNotAudited(t, "user passwd", code, out, errb, "so it was not made", "the password of tech1 is unchanged")
	code, out, errb = run(t, "", "user", "passwd", "--config", cfg, "--username", "tech1")
	wantNotAudited(t, "user passwd (generated)", code, out, errb, "the password of tech1 is unchanged")
	if strings.Contains(out, "New password (shown once)") {
		t.Errorf("an unaudited generated password was shown:\n%s", out)
	}
	if after := hash(); after != before || before == "" {
		t.Fatalf("the password hash changed although the audit failed")
	}
	code, out, errb = run(t, "", "user", "disable", "--config", cfg, "--username", "tech1")
	wantNotAudited(t, "user disable", code, out, errb, "account tech1 was disabled")
}
