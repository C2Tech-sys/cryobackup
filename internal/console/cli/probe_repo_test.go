package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// ADR 0017 (M3-T2 delta 2, L3a; mutants m20, m21): the production keyer gives
// the probe repository a password of its own: 64 hex characters, stable for a
// master key, different for another one and never the gateway probe login's.
func TestC08ProbeRepoPasswordIsItsOwn(t *testing.T) {
	newKey := func() *escrow.MasterKey {
		var k [32]byte
		_, _ = rand.Read(k[:])
		mk, err := escrow.NewMasterKey(k[:])
		if err != nil {
			t.Fatal(err)
		}
		return mk
	}
	mk := newKey()
	keys := maintainKeys{&escrow.Keys{Master: mk}}
	var _ maintain.ProbeRepoKeyer = keys
	pw, err := keys.ProbeRepoPassword()
	if err != nil {
		t.Fatalf("loaded key: %v", err)
	}
	if b, err := hex.DecodeString(pw); err != nil || len(b) != 32 {
		t.Fatalf("password %q is not 64 hex characters", pw)
	}
	if again, _ := keys.ProbeRepoPassword(); again != pw {
		t.Fatal("the password changes between calls")
	}
	if login, _ := gateway.ProbePassword(mk.Bytes()); login == pw {
		t.Fatal("the probe repository password is the gateway probe login's password")
	}
	if other, _ := (maintainKeys{&escrow.Keys{Master: newKey()}}).ProbeRepoPassword(); other == pw {
		t.Fatal("two master keys give the same probe repository password")
	}
	if _, err := (maintainKeys{&escrow.Keys{}}).ProbeRepoPassword(); err == nil {
		t.Fatal("a probe repository password without a master key")
	}
}

// probeRepoDB is an initialised console (config + database) for the command.
func probeRepoDB(t *testing.T) (cfgPath string, st func() *store.Store) {
	t.Helper()
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--prefix", "tenant-a")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfgPath = filepath.Join(dir, "config.json")
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, func() *store.Store {
		s, err := store.Open(context.Background(), store.Options{Path: cfg.DBPath})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
}

func auditCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'storage_probe.reset'`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// ADR 0017 recovery (M3-T2 delta 2, L3b): `probe-repo reset` is a dry run by
// default, deletes the marker and writes one audit row only with --confirm,
// never touches storage, and names the probe repository from the configured
// bucket and prefix.
func TestProbeRepoResetDryRunThenConfirm(t *testing.T) {
	t.Setenv("CRYO_MAINTAIN_BUCKET", "")
	t.Setenv("CRYO_MAINTAIN_PREFIX", "")
	cfgPath, open := probeRepoDB(t)
	st := open()
	if err := st.PutSetting(context.Background(), maintain.SettingProbeRepo, []byte(`{"created_at":1}`), ""); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	code, out, errb := run(t, "", "probe-repo", "reset", "--config", cfgPath)
	if code != ExitOK || !strings.Contains(out, "dry run") || !strings.Contains(out, "b/tenant-a/probe") || !strings.Contains(out, "never deletes anything on storage") {
		t.Fatalf("dry run: %d\n%s\n%s", code, out, errb)
	}
	st = open()
	if _, err := st.GetSetting(context.Background(), maintain.SettingProbeRepo); err != nil {
		t.Fatalf("the dry run changed the marker: %v", err)
	}
	if n := auditCount(t, st); n != 0 {
		t.Fatalf("the dry run wrote %d audit rows", n)
	}
	_ = st.Close()

	code, out, errb = run(t, "", "probe-repo", "reset", "--config", cfgPath, "--confirm")
	if code != ExitOK || !strings.Contains(out, "marker deleted") || !strings.Contains(out, "b/tenant-a/probe") || !strings.Contains(out, "nothing was deleted on storage") {
		t.Fatalf("confirm: %d\n%s\n%s", code, out, errb)
	}
	st = open()
	if _, err := st.GetSetting(context.Background(), maintain.SettingProbeRepo); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("marker after --confirm: %v", err)
	}
	if n := auditCount(t, st); n != 1 {
		t.Fatalf("%d audit rows, want 1", n)
	}
	_ = st.Close()

	// Idempotent: nothing left to reset, no second audit row.
	if code, out, _ = run(t, "", "probe-repo", "reset", "--config", cfgPath, "--confirm"); code != ExitOK || !strings.Contains(out, "nothing to reset") {
		t.Fatalf("second run: %d %s", code, out)
	}
	if n := auditCount(t, open()); n != 1 {
		t.Fatalf("%d audit rows after the second run, want 1", n)
	}
	if code, _, _ := run(t, "", "probe-repo"); code != ExitUsage {
		t.Fatalf("no subcommand: %d", code)
	}
}
