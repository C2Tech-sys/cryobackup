package cli

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-INT item C: probe-repo reset reads the probe repository's location from
// what the worker recorded (C4), says so truthfully when nothing was deleted
// (C8), and the probe repository's password is none of the console's other
// secrets (C7).

// setMarker writes the probe repository marker the worker would have written.
func setMarker(t *testing.T, open func() *store.Store, value string) {
	t.Helper()
	st := open()
	if err := st.PutSetting(context.Background(), maintain.SettingProbeRepo, []byte(value), ""); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
}

// assertNoLeak fails when the command's output names a configured path, the
// CLI's own CRYO_MAINTAIN_* environment, the default target's location, or any
// probe repository location other than want ("" means none at all).
func assertNoLeak(t *testing.T, cfgPath, out, want string) {
	t.Helper()
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{cfg.MasterKeyPath, cfg.DBPath, cfg.ArtifactsDir, "env-bucket", "env-prefix", "b/tenant-a"} {
		if leak != "" && strings.Contains(out, leak) {
			t.Errorf("the output names %q:\n%s", leak, out)
		}
	}
	// Every concrete ".../probe" in the output is the recorded location: no hard-coded or guessed prefix.
	rest := out
	if want != "" {
		rest = strings.ReplaceAll(out, want, "")
	}
	for _, line := range strings.Split(rest, "\n") {
		if i := strings.Index(line, "/probe"); i >= 0 && !strings.Contains(line, "<bucket>/<prefix>/probe") {
			t.Errorf("the output names a probe repository location that is not the recorded one: %q", line)
		}
	}
}

// M3-T2 r3 LOW-4 (mutants m10a, m10b): the location comes from the marker the
// worker wrote, never from the CLI process's own environment, and the output
// names no configured path.
func TestProbeRepoResetUsesTheLocationTheWorkerRecorded(t *testing.T) { // M3-INT C4
	t.Setenv("CRYO_MAINTAIN_BUCKET", "env-bucket") // the CLI's environment, not the worker's
	t.Setenv("CRYO_MAINTAIN_PREFIX", "env-prefix")
	cfgPath, open := probeRepoDB(t)
	setMarker(t, open, `{"created_at":1,"bucket":"rec-bucket","prefix":"rec/prefix"}`)
	const want = "rec-bucket/rec/prefix/probe"

	code, out, errb := run(t, "", "probe-repo", "reset", "--config", cfgPath)
	if code != ExitOK || !strings.Contains(out, want) {
		t.Fatalf("dry run: %d\n%s\n%s", code, out, errb)
	}
	assertNoLeak(t, cfgPath, out+errb, want)
	code, out, errb = run(t, "", "probe-repo", "reset", "--config", cfgPath, "--confirm")
	if code != ExitOK || !strings.Contains(out, "marker deleted (audited)") || !strings.Contains(out, want) {
		t.Fatalf("confirm: %d\n%s\n%s", code, out, errb)
	}
	assertNoLeak(t, cfgPath, out+errb, want)
	code, out, errb = run(t, "", "probe-repo", "reset", "--config", cfgPath, "--confirm")
	if code != ExitOK || !strings.Contains(out, "nothing to reset") {
		t.Fatalf("nothing to reset: %d\n%s\n%s", code, out, errb)
	}
	assertNoLeak(t, cfgPath, out+errb, "")
}

// M3-T2 r3 LOW-4: a marker that does not record the location (written before
// it did) cannot be matched to the worker's environment, so the command prints
// no path at all, says why, and names where the location comes from.
func TestProbeRepoResetRefusesToGuessTheLocation(t *testing.T) { // M3-INT C4
	t.Setenv("CRYO_MAINTAIN_BUCKET", "env-bucket")
	t.Setenv("CRYO_MAINTAIN_PREFIX", "env-prefix")
	cfgPath, open := probeRepoDB(t)
	setMarker(t, open, `{"created_at":1}`)
	for _, args := range [][]string{{"probe-repo", "reset", "--config", cfgPath}, {"probe-repo", "reset", "--config", cfgPath, "--confirm"}} {
		code, out, errb := run(t, "", args...)
		if code != ExitOK || !strings.Contains(out, "not recorded") || !strings.Contains(out, "CRYO_MAINTAIN_BUCKET") {
			t.Fatalf("%v: %d\n%s\n%s", args, code, out, errb)
		}
		assertNoLeak(t, cfgPath, out+errb, "")
	}
}

// M3-T2 r3 LOW-8: when the marker is gone by the time --confirm deletes it
// (another reset won), the command says nothing was deleted and writes no
// audit row; it never claims "deleted (audited)".
func TestProbeRepoResetThatLosesTheRaceSaysNothingWasDeleted(t *testing.T) { // M3-INT C8
	cfgPath, open := probeRepoDB(t)
	setMarker(t, open, `{"created_at":1,"bucket":"rec-bucket","prefix":"p"}`)
	probeRepoBeforeReset = func(st *store.Store) {
		if err := st.Write(context.Background(), func(tx *store.Tx) error {
			_, err := tx.DeleteSetting(maintain.SettingProbeRepo)
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { probeRepoBeforeReset = nil })
	code, out, errb := run(t, "", "probe-repo", "reset", "--config", cfgPath, "--confirm")
	if code != ExitOK || strings.Contains(out, "deleted (audited)") || !strings.Contains(out, "nothing was deleted") {
		t.Fatalf("a reset that lost the race: %d\n%s\n%s", code, out, errb)
	}
	if n := auditCount(t, open()); n != 0 {
		t.Fatalf("%d audit rows for a reset that deleted nothing, want 0", n)
	}
}

// M3-T2 r3 LOW-7 (mutant m22): the probe repository's password is neither the
// master key (raw or hex) nor any other secret derived from it.
func TestC08ProbeRepoPasswordIsNoOtherSecret(t *testing.T) { // M3-INT C7
	var k [32]byte
	_, _ = rand.Read(k[:])
	mk, err := escrow.NewMasterKey(k[:])
	if err != nil {
		t.Fatal(err)
	}
	pw, err := (maintainKeys{&escrow.Keys{Master: mk}}).ProbeRepoPassword()
	if err != nil {
		t.Fatal(err)
	}
	others := map[string][]byte{"the master key": mk.Bytes()}
	for _, info := range []string{escrow.ContextRepoKey, escrow.ContextGatewaySecret, escrow.ContextRestoreSelection, auth.ContextTOTP, "cryobackup/download/v1", "cryobackup/probe/v1"} {
		d, err := hkdf.Key(sha256.New, mk.Bytes(), nil, info, 32)
		if err != nil {
			t.Fatal(err)
		}
		others["the key derived for "+info] = d
	}
	if d, err := mk.DownloadKey(); err == nil {
		others["the download key"] = d
	} else {
		t.Fatal(err)
	}
	if login, err := gateway.ProbePassword(mk.Bytes()); err == nil {
		others["the gateway probe login's password"] = []byte(login)
	} else {
		t.Fatal(err)
	}
	for what, secret := range others {
		if pw == hex.EncodeToString(secret) || pw == string(secret) || strings.EqualFold(pw, string(secret)) {
			t.Errorf("the probe repository password is %s", what)
		}
	}
	if raw, err := hex.DecodeString(pw); err != nil || len(raw) != 32 {
		t.Fatalf("password is not 64 hex characters: %v", err)
	}
	if _, err := (maintainKeys{&escrow.Keys{}}).ProbeRepoPassword(); err == nil {
		t.Fatal("a probe repository password without a master key")
	}
}
