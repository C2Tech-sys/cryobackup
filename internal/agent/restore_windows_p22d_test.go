//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"golang.org/x/sys/windows"
)

// The restore executor against the fake console and the fake restic (which
// creates the include-file items under --target). The drive roots are temp
// folders standing in for C:\ and D:\; the staging and side roots live under
// them with the real root rule, plus this user as the lab trustee (a
// non-elevated test cannot be Administrators).

const testSnapID = "c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd"

type restoreRig struct {
	fc     *fakeConsole
	dir    string // state dir
	base   string
	out    string // fake restic argv log
	o      Options
	drives map[string]string
}

func newRestoreRig(t *testing.T, scenario string) *restoreRig {
	t.Helper()
	fc := newFakeConsole(t)
	// Paused: no scheduled backup competes for the restic slot unless a test
	// puts one there.
	fc.setPolicy(api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`, Paused: true})
	dir := enroll(t, fc)
	base := t.TempDir()
	r := &restoreRig{fc: fc, dir: dir, base: base, out: filepath.Join(base, "fake.out"), drives: map[string]string{}}
	for _, l := range []string{"C", "D"} {
		d := filepath.Join(base, "drive"+l)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		r.drives[l] = d
	}
	fakerestic.Use(t, scenario)
	t.Setenv(fakerestic.Out, r.out)
	o := fakeOptions(t, dir)
	o.restoreDriveRoot = func(l string) string { return r.drives[l] + `\` }
	o.restoreSideRoot = filepath.Join(base, "sys", "CryoBackup-Restore")
	if err := os.MkdirAll(filepath.Dir(o.restoreSideRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	o.restoreLabTrustee = currentSID(t)
	o.restoreSweepLetters = func() []string { return []string{"C", "D"} }
	o.restoreSlotPoll = 20 * time.Millisecond
	o.restoreFetchFor = 2 * time.Second
	r.o = o
	return r
}

func currentSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

func selection(id string, mode api.DeviceRestoreMode, ow api.DeviceRestoreOverwrite, paths ...string) api.RestoreSelection {
	return api.RestoreSelection{ID: id, SnapshotID: testSnapID, Mode: mode, Overwrite: ow, Paths: paths, ExpiresAt: time.Now().Add(time.Hour)}
}

var cmdSeq byte

func restoreCommand(t *testing.T, reqID string) api.Command {
	t.Helper()
	cmdSeq++
	params, _ := json.Marshal(api.RestoreParams{RestoreRequestID: reqID})
	return api.Command{ID: "01JB0000000000000000000C" + string("0123456789ABCDEFGHJKMNPQRSTVWXYZ"[cmdSeq%32]) + "1",
		Type: api.CommandRestore, Params: params, CreatedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}
}

func cancelCommand(runID string) api.Command {
	cmdSeq++
	params, _ := json.Marshal(map[string]string{"run_id": runID})
	return api.Command{ID: "01JB0000000000000000000K" + string("0123456789ABCDEFGHJKMNPQRSTVWXYZ"[cmdSeq%32]) + "1",
		Type: api.CommandCancel, Params: params, CreatedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}
}

func (r *restoreRig) waitClosed(t *testing.T, id string) api.RunRecord {
	t.Helper()
	waitFor(t, "the closed restore run", 60*time.Second, func() bool {
		rec, ok := r.fc.run(id)
		return ok && !rec.Open()
	})
	rec, _ := r.fc.run(id)
	if err := rec.Validate(); err != nil {
		t.Fatalf("the closed restore record does not validate: %v", err)
	}
	if rec.Kind != restic.KindRestore || rec.Trigger != api.TriggerManual || rec.ID != id {
		t.Fatalf("record = %+v", rec)
	}
	return rec
}

func (r *restoreRig) fakeArgs(t *testing.T) []string {
	t.Helper()
	b, _ := os.ReadFile(r.out)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "ARGS ") && !strings.HasPrefix(l, "ARGS version") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(l, "ARGS "), "\r"))
		}
	}
	return out
}

func (r *restoreRig) noIncludeFilesLeft(t *testing.T) {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(config.Layout{Root: r.dir}.TempDir(), "restore-*.include"))
	if len(m) != 0 {
		t.Errorf("include files left behind: %d", len(m))
	}
}

func (r *restoreRig) noStagingLeft(t *testing.T, id string) {
	t.Helper()
	for l, d := range r.drives {
		if _, err := os.Lstat(filepath.Join(d, ".cryobackup-restore", id)); err == nil {
			t.Errorf("the staging folder on %s was not removed", l)
		}
	}
}

func readOr(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// The command is accepted at once, before the selection is even fetched; the
// restore then runs on its own: dry run, restic into staging, placement, keep
// mode leaves an existing file alone.
func TestRestoreAcceptedAtOnceAndPlaced(t *testing.T) {
	r := newRestoreRig(t, "restore_ok")
	const id = "01JB0000000000000000000R01"
	hold := make(chan struct{})
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever,
		"/C/Users/x/Documents/a.txt", "/C/Users/x/Documents/kept.txt", "/C/Users/x/Pics"), hold: hold})
	kept := filepath.Join(r.drives["C"], "Users", "x", "Documents", "kept.txt")
	if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := start(t, r.o)
	cmd := restoreCommand(t, id)
	r.fc.enqueue(cmd)
	waitFor(t, "the accepted result while the fetch is held", 20*time.Second, func() bool { return len(r.fc.commandResults(cmd.ID)) > 0 })
	res := r.fc.commandResults(cmd.ID)[0]
	if res.Status != api.CommandSucceeded || !strings.Contains(string(res.Result), `"accepted":true`) {
		t.Fatalf("result = %+v %s", res, res.Result)
	}
	if rec, ok := r.fc.run(id); ok && !rec.Open() {
		t.Fatal("the restore closed before its selection was fetched")
	}
	close(hold)
	rec := r.waitClosed(t, id)
	// Restored: a.txt, the selected folder Pics (made by the agent) and the
	// file in it; skipped: kept.txt.
	if rec.Outcome != restic.OutcomeSuccess || rec.Stats.FilesRestored != 3 || rec.Stats.FilesSkipped != 1 || rec.Stats.RestoreResult != "" {
		t.Fatalf("record = %+v", rec.Stats)
	}
	if got := readOr(filepath.Join(r.drives["C"], "Users", "x", "Documents", "a.txt")); got != "restored:/Users/x/Documents/a.txt" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := readOr(filepath.Join(r.drives["C"], "Users", "x", "Pics", "inner.txt")); got != "restored:/Users/x/Pics" {
		t.Fatalf("Pics/inner.txt = %q", got)
	}
	if readOr(kept) != "existing" {
		t.Fatal("keep mode replaced an existing file")
	}
	if r.fc.restoreFetchCount(id) != 1 {
		t.Fatalf("fetched %d times", r.fc.restoreFetchCount(id))
	}
	run.stop()
	r.noIncludeFilesLeft(t)
	r.noStagingLeft(t, id)
}

// One restic run per volume, exactly ADR 0027's command line (plus the dry
// run for the size), never --delete; a volume whose run matched nothing makes
// the restore partial with a nothing_matched count.
func TestRestorePerVolumeInvocations(t *testing.T) {
	r := newRestoreRig(t, "restore_ok")
	t.Setenv(fakerestic.NothingFor, "D")
	const id = "01JB0000000000000000000R02"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteIfChanged,
		"/C/data/a [1].txt", "/D/data/b$x.txt")})
	run := start(t, r.o)
	r.fc.enqueue(restoreCommand(t, id))
	rec := r.waitClosed(t, id)
	run.stop()
	if rec.Outcome != restic.OutcomePartial || rec.Stats.RestoreResult != api.RestoreResultPartial || rec.Stats.FilesRestored != 1 {
		t.Fatalf("record = %+v", rec.Stats)
	}
	if len(rec.Stats.RestoreErrors) != 1 || rec.Stats.RestoreErrors[0] != (api.RestoreClassCount{Class: api.RestoreResultNothingMatched, Count: 1}) {
		t.Fatalf("rows = %+v", rec.Stats.RestoreErrors)
	}
	args := r.fakeArgs(t)
	var dry, real []string
	for _, a := range args {
		if strings.Contains(a, "--delete") {
			t.Fatalf("restic was given --delete: %s", a)
		}
		switch {
		case strings.HasPrefix(a, "restore") && strings.Contains(a, "--dry-run"):
			dry = append(dry, a)
		case strings.HasPrefix(a, "restore"):
			real = append(real, a)
		}
	}
	if len(dry) != 2 || len(real) != 2 {
		t.Fatalf("restic calls: %q", args)
	}
	for i, l := range []string{"C", "D"} {
		a := real[i]
		if !strings.HasPrefix(a, "restore --json --target ") || !strings.Contains(a, " --include-file ") ||
			!strings.HasSuffix(a, " -vv "+testSnapID+":/"+l) ||
			!strings.Contains(a, filepath.Join(r.drives[l], ".cryobackup-restore", id)) {
			t.Errorf("run %d = %q", i, a)
		}
	}
	if got := readOr(filepath.Join(r.drives["C"], "data", "a [1].txt")); got != "restored:/data/a [1].txt" {
		t.Fatalf("the bracket name round trip = %q", got)
	}
	r.noIncludeFilesLeft(t)
	r.noStagingLeft(t, id)
}

// A restore waits for the running backup and says so (device log and the
// journal key), then runs.
func TestRestoreWaitsForTheRunningBackup(t *testing.T) {
	r := newRestoreRig(t, "restore_ok")
	const id = "01JB0000000000000000000R03"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/w.txt")})
	run := start(t, r.o)
	a := run.a
	// A backup holds the restic slot.
	a.mu.Lock()
	a.running = &activeRun{id: "01JB0000000000000000000B01", kind: restic.KindBackup, cancel: func(error) {}, start: time.Now()}
	a.mu.Unlock()
	r.fc.enqueue(restoreCommand(t, id))
	waitFor(t, "the waiting key", 20*time.Second, func() bool {
		v, ok, _ := a.jr.KVGet(context.Background(), journal.KVRestoreWaiting)
		return ok && strings.HasPrefix(v, id+" ")
	})
	time.Sleep(200 * time.Millisecond)
	if rec, ok := r.fc.run(id); ok && !rec.Open() {
		t.Fatal("the restore did not wait for the backup")
	}
	for _, args := range r.fakeArgs(t) {
		if strings.HasPrefix(args, "restore") {
			t.Fatal("restic ran while the backup held the slot")
		}
	}
	a.clearRunning("01JB0000000000000000000B01")
	rec := r.waitClosed(t, id)
	run.stop()
	if rec.Outcome != restic.OutcomeSuccess {
		t.Fatalf("record = %+v", rec.Stats)
	}
	if _, ok, _ := a.jr.KVGet(context.Background(), journal.KVRestoreWaiting); ok {
		t.Error("the waiting key was not removed")
	}
	if log := readFile(t, config.Layout{Root: r.dir}.AgentLog()); !strings.Contains(log, "restore waiting for the running backup") {
		t.Error("the device log does not say the restore waited")
	}
}

// Every refusal code of the console is final: one fetch, no retry, the run
// closed failed and valid.
func TestRestoreRefusalsAreFinal(t *testing.T) {
	r := newRestoreRig(t, "restore_ok")
	run := start(t, r.o)
	for i, code := range []string{api.CodeAgentTooOld, api.CodeDeviceRetiredDownloadOnly, api.CodeDevicePurging,
		api.CodeDeviceRestoreActive, api.CodeRestoreExpired, api.CodeNotFound} {
		id := "01JB0000000000000000000R" + string("ABCDEFGH"[i]) + "1"
		r.fc.setRestore(id, restoreAnswer{code: code})
		r.fc.enqueue(restoreCommand(t, id))
		rec := r.waitClosed(t, id)
		if rec.Outcome != restic.OutcomeFailed || rec.Stats.RestoreResult != api.RestoreResultOther {
			t.Errorf("%s: record = %s / %+v", code, rec.Outcome, rec.Stats)
		}
		time.Sleep(100 * time.Millisecond)
		if n := r.fc.restoreFetchCount(id); n != 1 {
			t.Errorf("%s: fetched %d times", code, n)
		}
	}
	run.stop()
	for _, a := range r.fakeArgs(t) {
		if strings.HasPrefix(a, "restore") {
			t.Fatal("restic ran for a refused restore")
		}
	}
}

// Cancel by run id: while restic runs, the restore is stopped, restic's lock
// removed (unlock), staging and the include file removed, the run cancelled.
// The same restore command delivered again is refused: already run.
func TestRestoreCancelByRunID(t *testing.T) {
	r := newRestoreRig(t, "restore_slow")
	const id = "01JB0000000000000000000R04"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/c.txt")})
	run := start(t, r.o)
	cmd := restoreCommand(t, id)
	r.fc.enqueue(cmd)
	waitFor(t, "restic restoring", 30*time.Second, func() bool {
		for _, a := range r.fakeArgs(t) {
			if strings.HasPrefix(a, "restore") && !strings.Contains(a, "--dry-run") {
				return true
			}
		}
		return false
	})
	cancel := cancelCommand(id)
	r.fc.enqueue(cancel)
	rec := r.waitClosed(t, id)
	if rec.Outcome != restic.OutcomeCancelled || rec.Stats.RestoreResult != api.RestoreResultCancelled {
		t.Fatalf("record = %s / %+v", rec.Outcome, rec.Stats)
	}
	if res := r.fc.commandResults(cancel.ID); len(res) == 0 || !strings.Contains(string(res[0].Result), `"cancelled":true`) {
		t.Fatalf("cancel result = %+v", res)
	}
	waitFor(t, "restic unlock", 10*time.Second, func() bool {
		for _, a := range r.fakeArgs(t) {
			if strings.HasPrefix(a, "unlock") {
				return true
			}
		}
		return false
	})
	r.noIncludeFilesLeft(t)
	r.noStagingLeft(t, id)
	// Delivered again under a new command id: never run twice.
	again := restoreCommand(t, id)
	r.fc.enqueue(again)
	waitFor(t, "the second result", 20*time.Second, func() bool { return len(r.fc.commandResults(again.ID)) > 0 })
	if res := r.fc.commandResults(again.ID)[0]; res.Status != api.CommandFailed || !strings.Contains(string(res.Result), "already_run") {
		t.Fatalf("second delivery = %+v %s", res, res.Result)
	}
	run.stop()
}

// A cancel that arrives while the restore still waits for the backup stops it
// before restic ever runs.
func TestRestoreCancelWhileWaiting(t *testing.T) {
	r := newRestoreRig(t, "restore_ok")
	const id = "01JB0000000000000000000R05"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/c.txt")})
	run := start(t, r.o)
	run.a.mu.Lock()
	run.a.running = &activeRun{id: "01JB0000000000000000000B02", cancel: func(error) {}, start: time.Now()}
	run.a.mu.Unlock()
	r.fc.enqueue(restoreCommand(t, id))
	waitFor(t, "the waiting key", 20*time.Second, func() bool {
		_, ok, _ := run.a.jr.KVGet(context.Background(), journal.KVRestoreWaiting)
		return ok
	})
	r.fc.enqueue(cancelCommand(id))
	rec := r.waitClosed(t, id)
	run.a.clearRunning("01JB0000000000000000000B02")
	run.stop()
	if rec.Outcome != restic.OutcomeCancelled {
		t.Fatalf("record = %s", rec.Outcome)
	}
	for _, a := range r.fakeArgs(t) {
		if strings.HasPrefix(a, "restore") {
			t.Fatal("restic ran after the cancel")
		}
	}
}

// No resume after a restart: a restore left open by a process that died (its
// staging folder still there, an include file in tmp) is closed as
// interrupted when the agent starts, the staging folder is removed by the
// clean-up walk, the include file is gone, and the interrupted record goes to
// the console.
func TestRestoreNeverResumesAfterRestart(t *testing.T) {
	r := newRestoreRig(t, "restore_ok")
	const id = "01JB0000000000000000000R06"
	ctx := context.Background()
	layout := config.Layout{Root: r.dir}
	jr, err := journal.Open(ctx, journal.Options{Path: layout.JournalFile()})
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.StartRun(ctx, api.RunRecord{ID: id, Kind: restic.KindRestore, Trigger: api.TriggerManual,
		StartedAt: time.Now().UTC().Add(-time.Minute), Attempt: 1, AgentVersion: "0.4.0"}); err != nil {
		t.Fatal(err)
	}
	_ = jr.Close()
	// A start without a stop marker (the first restore was cut short by a
	// crash): New closes the open restore as interrupted, not abandoned; its
	// record goes out when the agent runs below.
	a0, err := New(ctx, r.o)
	if err != nil {
		t.Fatal(err)
	}
	a0.Close()
	// A second restore cut short, this time with its staging folder, an
	// include file and a junction in staging left behind.
	jr, _ = journal.Open(ctx, journal.Options{Path: layout.JournalFile()})
	const id2 = "01JB0000000000000000000R07"
	_ = jr.StartRun(ctx, api.RunRecord{ID: id2, Kind: restic.KindRestore, Trigger: api.TriggerManual,
		StartedAt: time.Now().UTC().Add(-time.Minute), Attempt: 1, AgentVersion: "0.4.0"})
	_ = jr.Close()
	stage := filepath.Join(r.drives["C"], ".cryobackup-restore")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	setRootSDDL(t, stage, r.o.restoreLabTrustee)
	left := filepath.Join(stage, id2, "Users", "x")
	if err := os.MkdirAll(left, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(left, "half.txt"), []byte("half"), 0o644)
	outside := filepath.Join(r.base, "outside")
	_ = os.MkdirAll(outside, 0o755)
	_ = os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("outside"), 0o644)
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", filepath.Join(left, "junc"), outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
	inc := filepath.Join(layout.TempDir(), "restore-"+id2+"-0.include")
	_ = os.WriteFile(inc, []byte("/x\n"), 0o600)

	run := start(t, r.o)
	for _, rid := range []string{id, id2} {
		rec := r.waitClosed(t, rid)
		if rec.Outcome != restic.OutcomeInterrupted || rec.Stats.RestoreResult != api.RestoreResultInterrupted {
			t.Errorf("%s: closed as %s / %s", rid, rec.Outcome, rec.Stats.RestoreResult)
		}
	}
	if _, err := os.Lstat(filepath.Join(stage, id2)); err == nil {
		t.Error("the staging folder of the interrupted restore was not swept")
	}
	if readOr(filepath.Join(outside, "victim.txt")) != "outside" {
		t.Error("the sweep followed the junction")
	}
	if _, err := os.Stat(inc); err == nil {
		t.Error("the include file a dead process left was not removed")
	}
	// A late delivery of the command for that restore is refused.
	cmd := restoreCommand(t, id2)
	r.fc.enqueue(cmd)
	waitFor(t, "the result", 20*time.Second, func() bool { return len(r.fc.commandResults(cmd.ID)) > 0 })
	if res := r.fc.commandResults(cmd.ID)[0]; !strings.Contains(string(res.Result), "already_run") {
		t.Fatalf("late delivery = %s", res.Result)
	}
	run.stop()
}

// setRootSDDL gives a pre-made root the root rule's protected descriptor for
// the lab trustee (what the agent would have created).
func setRootSDDL(t *testing.T, p, sid string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// The free-space check refuses before anything is written: FAILED disk, no
// staging root, no restic restore beyond the dry run; the numbers stay in the
// device log.
func TestRestoreRefusedForFreeSpace(t *testing.T) {
	for _, side := range []bool{false, true} {
		r := newRestoreRig(t, "restore_ok")
		r.o.restoreFreeBytes = func(string) (uint64, error) { return 3, nil }
		id := "01JB0000000000000000000R08"
		mode := api.DeviceRestoreOriginal
		if side {
			id, mode = "01JB0000000000000000000R09", api.DeviceRestoreSide
		}
		r.fc.setRestore(id, restoreAnswer{sel: selection(id, mode, api.DeviceOverwriteNever, "/C/big.txt")})
		run := start(t, r.o)
		r.fc.enqueue(restoreCommand(t, id))
		rec := r.waitClosed(t, id)
		run.stop()
		if rec.Outcome != restic.OutcomeFailed || rec.Stats.RestoreResult != api.RestoreResultDisk {
			t.Fatalf("side=%v: record = %s / %+v", side, rec.Outcome, rec.Stats)
		}
		for _, a := range r.fakeArgs(t) {
			if strings.HasPrefix(a, "restore") && !strings.Contains(a, "--dry-run") {
				t.Fatalf("side=%v: restic restored although the space was short", side)
			}
		}
		if _, err := os.Lstat(filepath.Join(r.drives["C"], ".cryobackup-restore")); err == nil {
			t.Errorf("side=%v: a staging root was written before the refusal", side)
		}
		if _, err := os.Lstat(r.o.restoreSideRoot); err == nil {
			t.Errorf("side=%v: a side root was written before the refusal", side)
		}
		log := readFile(t, config.Layout{Root: r.dir}.AgentLog())
		if !strings.Contains(log, `"needed_bytes":`) || !strings.Contains(log, `"free_bytes":3`) {
			t.Errorf("side=%v: the device log lacks the numbers", side)
		}
	}
}

// A staging root that fails the root rule refuses the restore as a whole
// (FAILED guard_refused) and is left exactly as it was.
func TestRestoreRefusedForABadRoot(t *testing.T) {
	r := newRestoreRig(t, "restore_ok")
	const id = "01JB0000000000000000000R10"
	stage := filepath.Join(r.drives["C"], ".cryobackup-restore")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	// Everyone may write: not the agent's root.
	sd, _ := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + r.o.restoreLabTrustee + ")(A;OICI;FA;;;WD)")
	dacl, _, _ := sd.DACL()
	_ = windows.SetNamedSecurityInfo(stage, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	before, _ := windows.GetNamedSecurityInfo(stage, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/r.txt")})
	run := start(t, r.o)
	r.fc.enqueue(restoreCommand(t, id))
	rec := r.waitClosed(t, id)
	run.stop()
	if rec.Outcome != restic.OutcomeFailed || rec.Stats.RestoreResult != api.RestoreResultGuardRefused {
		t.Fatalf("record = %s / %+v", rec.Outcome, rec.Stats)
	}
	after, _ := windows.GetNamedSecurityInfo(stage, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if before.String() != after.String() {
		t.Fatalf("the root was repaired: %s -> %s", before, after)
	}
	log := readFile(t, config.Layout{Root: r.dir}.AgentLog())
	if !strings.Contains(log, `"check":"dacl_grants_other_trustee"`) {
		t.Error("the device log does not name the failed check")
	}
}

// Side folder: the files land in <side>\<id>\<L>\..., a junction restic made
// is deleted unfollowed and counted as skipped, every entry inherits from the
// side folder only.
func TestRestoreSideFolder(t *testing.T) {
	r := newRestoreRig(t, "restore_link")
	outside := filepath.Join(r.base, "outside")
	_ = os.MkdirAll(outside, 0o755)
	_ = os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("outside"), 0o644)
	t.Setenv(fakerestic.LinkTarget, outside)
	const id = "01JB0000000000000000000R11"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreSide, api.DeviceOverwriteNever, "/C/Users/x/s.txt")})
	run := start(t, r.o)
	r.fc.enqueue(restoreCommand(t, id))
	rec := r.waitClosed(t, id)
	run.stop()
	if rec.Outcome != restic.OutcomeSuccess || rec.Stats.FilesRestored != 1 || rec.Stats.FilesSkipped != 1 {
		t.Fatalf("record = %s / %+v", rec.Outcome, rec.Stats)
	}
	f := filepath.Join(r.o.restoreSideRoot, id, "C", "Users", "x", "s.txt")
	if readOr(f) != "restored:/Users/x/s.txt" {
		t.Fatalf("side file = %q", readOr(f))
	}
	if _, err := os.Lstat(filepath.Join(r.o.restoreSideRoot, id, "C", "junction-from-snapshot")); err == nil {
		t.Fatal("the junction restic made is still in the side folder")
	}
	sd, err := windows.GetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if s := sd.String(); !strings.Contains(s, "ID;") || strings.Contains(s, "D:P") {
		t.Fatalf("side file descriptor = %s", s)
	}
}

// Nothing matched: FAILED nothing_matched, zero counts.
func TestRestoreNothingMatched(t *testing.T) {
	r := newRestoreRig(t, "restore_nothing")
	const id = "01JB0000000000000000000R12"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/none.txt", "/D/none.txt")})
	run := start(t, r.o)
	r.fc.enqueue(restoreCommand(t, id))
	rec := r.waitClosed(t, id)
	run.stop()
	if rec.Outcome != restic.OutcomeFailed || rec.Stats.RestoreResult != api.RestoreResultNothingMatched ||
		rec.Stats.FilesRestored != 0 || len(rec.Stats.RestoreErrors) != 0 {
		t.Fatalf("record = %s / %+v", rec.Outcome, rec.Stats)
	}
	r.noStagingLeft(t, id)
}

// The gateway refused the login: FAILED auth, read from restic's error output,
// before anything is written.
func TestRestoreAuthRefused(t *testing.T) {
	r := newRestoreRig(t, "restore_auth")
	const id = "01JB0000000000000000000R13"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/a.txt")})
	run := start(t, r.o)
	r.fc.enqueue(restoreCommand(t, id))
	rec := r.waitClosed(t, id)
	run.stop()
	if rec.Outcome != restic.OutcomeFailed || rec.Stats.RestoreResult != api.RestoreResultAuth {
		t.Fatalf("record = %s / %+v", rec.Outcome, rec.Stats)
	}
}

// A restore that stops moving is stopped by the watchdog: FAILED with the
// storage class, restic unlocked afterwards.
func TestRestoreWatchdog(t *testing.T) {
	r := newRestoreRig(t, "restore_stall")
	r.o.restoreIdle = 700 * time.Millisecond
	const id = "01JB0000000000000000000R14"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/a.txt")})
	run := start(t, r.o)
	r.fc.enqueue(restoreCommand(t, id))
	rec := r.waitClosed(t, id)
	run.stop()
	if rec.Outcome != restic.OutcomeFailed || rec.Stats.RestoreResult != api.RestoreResultRepoUnreachable {
		t.Fatalf("record = %s / %+v", rec.Outcome, rec.Stats)
	}
	found := false
	for _, a := range r.fakeArgs(t) {
		found = found || strings.HasPrefix(a, "unlock")
	}
	if !found {
		t.Fatal("no restic unlock after the watchdog stopped restic")
	}
	r.noIncludeFilesLeft(t)
	r.noStagingLeft(t, id)
}

// A per-file restic error (a file in use) is counted by class on the device:
// RESTORED WITH ERRORS, target_locked 1.
func TestRestoreFileErrorIsCounted(t *testing.T) {
	r := newRestoreRig(t, "restore_file_error")
	const id = "01JB0000000000000000000R15"
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/ok.txt")})
	run := start(t, r.o)
	r.fc.enqueue(restoreCommand(t, id))
	rec := r.waitClosed(t, id)
	run.stop()
	if rec.Outcome != restic.OutcomePartial || rec.Stats.FilesRestored != 1 ||
		len(rec.Stats.RestoreErrors) != 1 || rec.Stats.RestoreErrors[0] != (api.RestoreClassCount{Class: api.RestoreResultTargetLocked, Count: 1}) {
		t.Fatalf("record = %s / %+v", rec.Outcome, rec.Stats)
	}
}

// One restore at a time: a second restore command while one runs is refused.
func TestRestoreOneAtATime(t *testing.T) {
	r := newRestoreRig(t, "restore_ok")
	const id1, id2 = "01JB0000000000000000000R16", "01JB0000000000000000000R17"
	hold := make(chan struct{})
	r.fc.setRestore(id1, restoreAnswer{sel: selection(id1, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, "/C/a.txt"), hold: hold})
	run := start(t, r.o)
	c1 := restoreCommand(t, id1)
	r.fc.enqueue(c1)
	waitFor(t, "the first accepted", 20*time.Second, func() bool { return len(r.fc.commandResults(c1.ID)) > 0 })
	c2 := restoreCommand(t, id2)
	r.fc.enqueue(c2)
	waitFor(t, "the second answered", 20*time.Second, func() bool { return len(r.fc.commandResults(c2.ID)) > 0 })
	if res := r.fc.commandResults(c2.ID)[0]; res.Status != api.CommandFailed || !strings.Contains(string(res.Result), "restore_active") {
		t.Fatalf("second = %s", res.Result)
	}
	close(hold)
	r.waitClosed(t, id1)
	run.stop()
	if _, ok := r.fc.run(id2); ok {
		t.Fatal("a run was journaled for the refused restore")
	}
}
