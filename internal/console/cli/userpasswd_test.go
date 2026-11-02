package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// `user passwd` is a password reset like the route's: in the same transaction
// as the new password it ends every session of the account and revokes its
// API tokens, records both counts in the audit row and says so (foreman
// verification notes, M2-W1A).
func TestUserPasswdEndsSessionsAndRevokesTokens(t *testing.T) {
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg := filepath.Join(dir, "config.json")
	if code, out, _ := run(t, "", "user", "add", "--config", cfg, "--username", "tech1", "--role", "operator"); code != 0 {
		t.Fatalf("user add: %d %s", code, out)
	}
	ctx := context.Background()
	c, err := loadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, store.Options{Path: c.DBPath})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetAdminByUsername(ctx, "tech1")
	now := time.Now()
	exp := now.Add(24 * time.Hour)
	if err := st.Write(ctx, func(tx *store.Tx) error {
		if _, err := tx.InsertSession(store.Session{AdminID: a.ID, TokenHash: strings.Repeat("a", 64), MFAVerifiedAt: &now,
			LastSeenAt: now, ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(2 * time.Hour)}); err != nil {
			return err
		}
		_, err := tx.CreateAPIToken(store.APIToken{AdminID: a.ID, Name: "script", Scopes: []string{"full"}, TokenPrefix: "cbk_abcd",
			TokenHash: strings.Repeat("b", 64), ExpiresAt: &exp})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	code, out, errb := run(t, "a-long-enough-password\n", "user", "passwd", "--config", cfg, "--username", "tech1", "--password-stdin")
	if code != 0 || !strings.Contains(out, "1 session(s) ended") || !strings.Contains(out, "1 API token(s) revoked") {
		t.Fatalf("passwd: %d\n%s\n%s", code, out, errb)
	}
	st, err = store.Open(ctx, store.Options{Path: c.DBPath})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var sessions, live int
	if err := st.Read(ctx, func(tx *store.Tx) error {
		ss, err := tx.ListAdminSessions(a.ID)
		sessions = len(ss)
		if err != nil {
			return err
		}
		ts, err := tx.ListAPITokens(a.ID)
		for _, x := range ts {
			if x.RevokedAt == nil {
				live++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || live != 0 {
		t.Fatalf("after passwd: %d sessions, %d live tokens", sessions, live)
	}
	evs, _ := st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "admin.password_reset"}, store.Page{Limit: 10})
	if len(evs.Items) != 1 || !strings.Contains(string(evs.Items[0].Details), `"sessions_ended":1`) || !strings.Contains(string(evs.Items[0].Details), `"tokens_revoked":1`) {
		t.Fatalf("audit: %+v", evs.Items)
	}
}
