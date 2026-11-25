package maintain

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// TestRunOnceExitsFailedOnNoSlot (P22-F backlog, main.go selfFailed): with the
// self-backup configured and CRYO_MAINTAIN_MAX_STREAMS=3 (of the 4 slots,
// selfSlotRoom leaves none for it), `maintain --once` prints no_slot and exits
// failed; the device pass itself has nothing to do. The control run, the same
// worker with the prefix unset, exits ok (not_configured is a warning on the
// job row, not a failed pass), so the exit code is the no_slot rule's alone.
func TestRunOnceExitsFailedOnNoSlot(t *testing.T) {
	dir := t.TempDir()
	exe, sum := fakeResticPath(t)
	harness := filepath.Join(dir, "harness")
	if err := os.MkdirAll(harness, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtures, err := filepath.Abs(filepath.Join("..", "..", "restic", "testdata", "v0.19.1"))
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rules := append([]fakeRule{{Cmd: "version", Stdout: "version.json"}}, selfRules(filepath.Join(dataDir, SelfBackupStagingDir))...)
	sc, _ := json.Marshal(fakeScenario{Fixtures: fixtures, Rules: rules})
	if err := os.WriteFile(filepath.Join(harness, "fake.json"), sc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(harness, "rclone.conf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "c.db")
	cfg := filepath.Join(dir, "config.json")
	body := `{"db_path":` + jsonStr(dbPath) + `,"master_key_path":` + jsonStr(filepath.Join(dir, "master.key")) + `}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_MAINTAIN_RESTIC", exe)
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256", sum)
	t.Setenv("RCLONE_CONFIG", filepath.Join(harness, "rclone.conf"))
	t.Setenv("CRYO_MAINTAIN_SCRATCH_DIR", filepath.Join(dir, "scratch"))
	t.Setenv("CRYO_MAINTAIN_BUCKET", "bucket")
	t.Setenv("CRYO_MAINTAIN_PREFIX", "cb")
	t.Setenv("CRYO_MAINTAIN_MAX_STREAMS", "3")
	keys := func(string, *store.Store) (KeyUnwrapper, error) {
		return selfAndUnwrap{&fakeKeys{pw: "x"}, &fakeSelfKeys{pw: testSelfPW, keyID: testSelfKeyID}}, nil
	}

	// Control: prefix unset, so the slot rule is never reached.
	t.Setenv("CRYO_MAINTAIN_SELF_PREFIX", "")
	var out, errb bytes.Buffer
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitOK {
		t.Fatalf("control --once exit %d (stdout %q, stderr %q)", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "self-backup not_configured") || !strings.Contains(out.String(), "failed=0") {
		t.Fatalf("control --once stdout = %q", out.String())
	}

	t.Setenv("CRYO_MAINTAIN_SELF_PREFIX", testSelfPrefix)
	out.Reset()
	errb.Reset()
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitFailed {
		t.Fatalf("no_slot --once exit %d, want %d (stdout %q, stderr %q)", code, ExitFailed, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "self-backup no_slot") || !strings.Contains(out.String(), "failed=0") {
		t.Fatalf("no_slot --once stdout = %q", out.String())
	}
	st, err := store.Open(context.Background(), store.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	js, err := st.GetJobState(context.Background(), SelfBackupJob)
	if err != nil || js.LastStatus != "error" || js.LastErrorClass != SelfClassNoSlot {
		t.Fatalf("job row after the no_slot --once = %+v, %v", js, err)
	}
}
