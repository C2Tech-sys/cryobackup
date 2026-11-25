package maintain

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Fix batch 1 of ticket P22-C: the verdict's age in the facts (C1), the
// self-backup's place in the restic slots (C2), and configuration refusals
// that do not push the first backup out (C5).

// holdDownloads takes n stream tokens and n restic slots, as n technician
// downloads in flight do (Dump takes its stream token, then a slot for as long
// as the browser reads), and returns the release.
func holdDownloads(t *testing.T, w *Worker, n int) func() {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case w.streams <- struct{}{}:
		default:
			t.Fatalf("stream token %d of %d not free", i+1, n)
		}
		select {
		case w.sem <- struct{}{}:
		default:
			t.Fatalf("restic slot %d of %d not free", i+1, n)
		}
	}
	return func() {
		for i := 0; i < n; i++ {
			<-w.sem
			<-w.streams
		}
	}
}

// slowBackupRules are selfRules with a `restic backup` that takes 20 s, plus the
// device pass's stats.
func slowBackupRules(staging string) []fakeRule {
	r := selfRules(staging)
	for i := range r {
		if r[i].Cmd == "backup" {
			r[i].SleepMS = 20000
		}
	}
	return append(r, fakeRule{Cmd: "stats", Stdout: "stats_raw_data.json"})
}

// TestSelfBackupNoSlotBesideFullDownloads (C2): CRYO_MAINTAIN_MAX_STREAMS=3 of
// the 4 restic slots, three downloads running. The self-backup does not take
// the one slot left (it records no_slot and runs no restic), and the device
// pass that follows samples its device at once: not deferred.
func TestSelfBackupNoSlotBesideFullDownloads(t *testing.T) {
	s := newSelfEnv(t, slowBackupRules, func(c *Config) { c.MaxStreams = 3 })
	if s.w.cfg.MaxConcurrent != 4 || s.w.cfg.MaxStreams != 3 {
		t.Fatalf("slots %d, streams %d", s.w.cfg.MaxConcurrent, s.w.cfg.MaxStreams)
	}
	s.onlyDevice(s.esc)
	release := holdDownloads(t, s.w, 3)
	defer release()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	type out struct {
		rep SelfBackupReport
		err error
	}
	done := make(chan out, 1)
	go func() {
		rep, err := s.w.SelfBackupStep(ctx)
		done <- out{rep, err}
	}()
	var got out
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		cancel()
		got = <-done
		t.Fatalf("the self-backup step was still running beside three downloads: %+v", got.rep)
	}
	if got.err != nil || !got.rep.NoSlot || got.rep.BackupRan || got.rep.VerifyRan || got.rep.BackupClass != SelfClassNoSlot {
		t.Fatalf("self-backup step = %+v, %v; want no_slot and nothing run", got.rep, got.err)
	}
	if n := len(s.repoCalls()); n != 0 {
		t.Fatalf("the self-backup ran restic %d times without room for it", n)
	}
	if js := s.job(SelfBackupJob); js.LastStatus != "error" || js.LastErrorClass != SelfClassNoSlot {
		t.Fatalf("job row = %+v", js)
	}
	if !strings.Contains(s.logs.String(), "CRYO_MAINTAIN_MAX_STREAMS") {
		t.Errorf("the no_slot warning does not name the variable: %s", s.logs.String())
	}

	start := time.Now()
	rep, err := s.w.RunOnce(s.ctx)
	if err != nil || rep.Sampled != 1 || rep.UsageDeferred != 0 || rep.Failed != 0 {
		t.Fatalf("device pass beside three downloads = %+v, %v; want the device sampled, nothing deferred", rep, err)
	}
	if el := time.Since(start); el >= schedulerSlotWait {
		t.Fatalf("the device pass waited %v for its slot", el)
	}
	s.absent()
}

// TestSelfBackupLeavesTheMaintenanceSlot (C2): with the default two streams of
// four slots the self-backup runs, holding one slot for a long `restic backup`
// while two downloads hold two: the device pass still gets the fourth at once.
func TestSelfBackupLeavesTheMaintenanceSlot(t *testing.T) {
	s := newSelfEnv(t, slowBackupRules, nil)
	if s.w.cfg.MaxConcurrent != 4 || s.w.cfg.MaxStreams != 2 || !s.w.selfSlotRoom() {
		t.Fatalf("slots %d, streams %d", s.w.cfg.MaxConcurrent, s.w.cfg.MaxStreams)
	}
	s.onlyDevice(s.esc)
	release := holdDownloads(t, s.w, 2)
	defer release()

	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan SelfBackupReport, 1)
	go func() {
		rep, _ := s.w.SelfBackupStep(ctx)
		done <- rep
	}()
	// Wait until the self-backup's restic backup holds its slot: 3 of 4 taken.
	deadline := time.Now().Add(15 * time.Second)
	for len(s.w.sem) < 3 || len(s.callsOf("backup")) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the self-backup never reached restic backup (slots taken %d)", len(s.w.sem))
		}
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	rep, err := s.w.RunOnce(s.ctx)
	if err != nil || rep.Sampled != 1 || rep.UsageDeferred != 0 || rep.Failed != 0 {
		cancel()
		t.Fatalf("device pass beside a self-backup and two downloads = %+v, %v", rep, err)
	}
	if el := time.Since(start); el >= schedulerSlotWait {
		t.Fatalf("the device pass waited %v for its slot", el)
	}
	cancel()
	if sb := <-done; !sb.BackupRan || sb.BackupClass != SelfClassInterrupted {
		t.Fatalf("stopped self-backup = %+v", sb)
	}
	s.absent()
}

// TestSelfBackupNoSlotThenRoom (C2, C5): a no_slot row is due again at the
// next step once the configuration leaves room, and a due verify records an
// attempt with class no_slot (C11's text) while it cannot run.
func TestSelfBackupNoSlotThenRoom(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	if rep := s.step(); !rep.BackupOK || !rep.VerifyOK {
		t.Fatalf("first step = %+v", rep)
	}
	s.w.cfg.MaxStreams = 3 // a restart with CRYO_MAINTAIN_MAX_STREAMS=3
	s.clk.Advance(SelfBackupVerifyEvery)
	before := len(s.repoCalls())
	rep := s.step()
	if !rep.NoSlot || rep.BackupRan || rep.VerifyRan || len(s.repoCalls()) != before {
		t.Fatalf("no-slot step = %+v (restic calls %d)", rep, len(s.repoCalls())-before)
	}
	st := s.state()
	if st.Verify == nil || !st.Verify.OK || st.Verify.At != pms(t0) || st.Verify.AttemptClass != SelfClassNoSlot || st.Verify.AttemptAt != pms(s.clk.Now()) {
		t.Fatalf("verify setting = %+v", st.Verify)
	}
	if js := s.job(SelfBackupVerifyJob); js.LastErrorClass != SelfClassNoSlot {
		t.Fatalf("verify row = %+v", js)
	}
	f, err := s.st.SelfBackupFacts(s.ctx)
	if err != nil || f.VerifyFailed || int64(f.VerifyAt) != pms(t0) || f.VerifyAttemptClass != SelfClassNoSlot {
		t.Fatalf("facts = %+v, %v", f, err)
	}
	// Still no room: not due again before its time.
	if rep := s.step(); !rep.NothingDue {
		t.Fatalf("step without room, not yet due = %+v", rep)
	}
	// A restart with room: the very next step runs both.
	s.w.cfg.MaxStreams = 2
	if rep := s.step(); !rep.BackupOK || !rep.VerifyOK {
		t.Fatalf("step after room came back = %+v", rep)
	}
}

// TestSelfBackupFirstRunAfterPrefixSet (C5): a not_configured row does not
// push the first backup SelfBackupEvery out: once the prefix is set the next
// step backs up.
func TestSelfBackupFirstRunAfterPrefixSet(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	s.w.cfg.SelfPrefix = ""
	if rep := s.step(); !rep.NotConfigured {
		t.Fatalf("unconfigured step = %+v", rep)
	}
	if js := s.job(SelfBackupJob); js.LastErrorClass != SelfClassNotConfigured || !js.NextDueAt.Equal(t0.Add(SelfBackupEvery)) {
		t.Fatalf("job row = %+v", js)
	}
	// Still unset: the warning keeps its 6 h rhythm.
	if rep := s.step(); !rep.NothingDue {
		t.Fatalf("second unconfigured step = %+v", rep)
	}
	s.clk.Advance(time.Minute) // the next tick, after a restart with the prefix set
	s.w.cfg.SelfPrefix = testSelfPrefix
	rep := s.step()
	if !rep.BackupRan || !rep.BackupOK || !rep.VerifyOK {
		t.Fatalf("first step with the prefix set = %+v", rep)
	}
	if js := s.job(SelfBackupJob); js.LastStatus != "ok" {
		t.Fatalf("job row = %+v", js)
	}
}

// selfAndUnwrap is a KeyUnwrapper that also derives the self-backup key, so
// New picks it as Worker.selfKeys (Run's tests need no master key file).
type selfAndUnwrap struct {
	*fakeKeys
	*fakeSelfKeys
}

// TestRunOnceBacksUpAfterPrefixSet (C5): `maintain --once` without the prefix
// writes not_configured; the next `--once` with the prefix set runs the
// backup at once.
func TestRunOnceBacksUpAfterPrefixSet(t *testing.T) {
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
	t.Setenv("CRYO_MAINTAIN_SELF_PREFIX", "")
	keys := func(string, *store.Store) (KeyUnwrapper, error) {
		return selfAndUnwrap{&fakeKeys{pw: "x"}, &fakeSelfKeys{pw: testSelfPW, keyID: testSelfKeyID}}, nil
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitOK {
		t.Fatalf("unconfigured --once exit %d (stdout %q, stderr %q)", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "self-backup not_configured") {
		t.Fatalf("unconfigured --once stdout = %q", out.String())
	}

	t.Setenv("CRYO_MAINTAIN_SELF_PREFIX", "console-self")
	out.Reset()
	errb.Reset()
	// The fake restores nothing, so the verify fails on the manifest; the
	// backup is what this test is about.
	_ = Run([]string{"--config", cfg, "--once"}, &out, &errb, keys)
	if !strings.Contains(out.String(), "self-backup backup=ok") {
		t.Fatalf("configured --once stdout = %q (stderr %q): the backup did not run at once", out.String(), errb.String())
	}
	st, err := store.Open(context.Background(), store.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	js, err := st.GetJobState(context.Background(), SelfBackupJob)
	if err != nil || js.LastStatus != "ok" || js.RunCount != 2 {
		t.Fatalf("job row after the second --once = %+v, %v", js, err)
	}
}

// TestSelfBackupFactsCarryVerdictAge (C1): an inconclusive verify leaves the
// verdict's time where it was and puts the attempt's time and class in the
// facts, so the loader can tell a verdict eight days old from a fresh one;
// FirstBackupAt stays at the first backup.
func TestSelfBackupFactsCarryVerdictAge(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	if rep := s.step(); !rep.VerifyOK {
		t.Fatalf("first step = %+v", rep)
	}
	f, err := s.st.SelfBackupFacts(s.ctx)
	if err != nil || int64(f.VerifyAt) != pms(t0) || int64(f.VerifyAttemptAt) != pms(t0) || f.VerifyAttemptClass != "" ||
		int64(f.FirstBackupAt) != pms(t0) || int64(f.LastAt) != pms(t0) {
		t.Fatalf("facts after a good verify = %+v, %v", f, err)
	}

	rules := selfRules(filepath.Join(s.stateDir, SelfBackupStagingDir))
	for i := range rules {
		if rules[i].Cmd == "snapshots" {
			rules[i] = fakeRule{Cmd: "snapshots", Exit: 1, Stderr: `inline:{"message_type":"exit_error","code":1,"message":"Fatal: read tcp: connection reset by peer"}` + "\n"}
		}
	}
	s.scenario(rules)
	for day := 0; day < 9; day++ {
		s.clk.Advance(24 * time.Hour)
		s.step()
	}
	f, err = s.st.SelfBackupFacts(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := s.clk.Now()
	if int64(f.VerifyAt) != pms(t0) || f.VerifyFailed || f.VerifyAttemptClass != string(restic.ClassNetwork) ||
		int64(f.VerifyAttemptAt) <= int64(f.VerifyAt) || int64(f.FirstBackupAt) != pms(t0) || int64(f.LastAt) != pms(now) {
		t.Fatalf("facts after nine days of inconclusive verifies = %+v", f)
	}
	// The loader's rule (store.SelfBackupFacts): no verdict for more than
	// SelfBackupVerifyStale with a backup present is critical.
	if age := now.Sub(time.UnixMilli(int64(f.VerifyAt))); age <= store.SelfBackupVerifyStale {
		t.Fatalf("verdict age %v, want more than %v", age, store.SelfBackupVerifyStale)
	}
}
