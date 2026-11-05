package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-INT item C: the storage probe's later steps wait for a slot only so long
// (C2), and a settings-store failure inside the probe is not a storage verdict
// (C3).

// fillDuringFirstStep leaves the worker one free slot for the probe's first
// step (list keys, held for listMS by the fake restic) and, while that step
// runs, queues a goroutine for the slot it will free: Go's channel senders are
// served in order, so the queued goroutine, not the probe's next step, gets
// it. The worker is then full when the next step asks. The returned function
// frees every slot.
func fillDuringFirstStep(t *testing.T, e *tenv) (release func()) {
	t.Helper()
	for i := 0; i < cap(e.w.sem)-1; i++ {
		e.w.sem <- struct{}{}
	}
	took := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for len(e.w.sem) < cap(e.w.sem) && time.Now().Before(deadline) { // the first step has its slot
			time.Sleep(time.Millisecond)
		}
		e.w.sem <- struct{}{} // queued behind the first step's release
		close(took)
	}()
	return func() {
		<-took
		for i := 0; i < cap(e.w.sem); i++ {
			<-e.w.sem
		}
	}
}

// M3-T2 r3 LOW-2 (mutant m6c): a worker that fills up between the list and the
// cat config answers busy within ProbeSlotWait (C02b on the console), never a
// storage failure and never the console's deadline.
func TestC08ProbeCatStepWaitsForASlotOnlySoLong(t *testing.T) { // M3-INT C2
	e := newTenv(t, []fakeRule{{Cmd: "list", Stdout: "inline:0123abcd\n", SleepMS: 300}, {Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	e.w.probeSlotWait = 100 * time.Millisecond
	release := fillDuringFirstStep(t, e)
	defer release()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second) // stands in for the console's deadline
	defer cancel()
	start := time.Now()
	_, err := e.w.CatConfig(ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	var ae *adminpath.Error
	if !errors.As(err, &ae) || ae.Code != adminpath.CodeBusy {
		t.Fatalf("a worker full at the cat step: %v, want busy", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the cat step waited %v for a slot", took)
	}
	if got := strings.Join(e.argsOf(), ","); got != "list keys" {
		t.Fatalf("calls %s: the cat step ran without a slot", got)
	}
}

// M3-T2 r3 LOW-2: the same for the one-time init step.
func TestC08ProbeInitStepWaitsForASlotOnlySoLong(t *testing.T) { // M3-INT C2
	e := newTenv(t, []fakeRule{{Cmd: "list", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl", SleepMS: 300}, {Cmd: "init", Stdout: "init.json"},
		{Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	e.w.probeSlotWait = 100 * time.Millisecond
	release := fillDuringFirstStep(t, e)
	defer release()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := e.w.CatConfig(ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	var ae *adminpath.Error
	if !errors.As(err, &ae) || ae.Code != adminpath.CodeBusy {
		t.Fatalf("a worker full at the init step: %v, want busy", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the init step waited %v for a slot", took)
	}
	if _, err := e.st.GetSetting(e.ctx, SettingProbeRepo); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the guard was armed although init never ran: %v", err)
	}
}

// M3-T2 r3 LOW-4, the worker's half: the marker records the bucket and prefix
// the worker used, which `probe-repo reset` reads instead of its own environment.
func TestC08ProbeMarkerRecordsTheWorkersLocation(t *testing.T) { // M3-INT C4
	e := newTenv(t, []fakeRule{{Cmd: "list", Stdout: "inline:0123abcd\n"}, {Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	if _, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID}); err != nil {
		t.Fatalf("clean probe: %v", err)
	}
	s, err := e.st.GetSetting(e.ctx, SettingProbeRepo)
	if err != nil {
		t.Fatal(err)
	}
	var m ProbeRepoMarker
	if err := json.Unmarshal(s.Value, &m); err != nil {
		t.Fatal(err)
	}
	if loc, ok := m.Location(); !ok || loc != "test-bucket/pre/probe" || m.CreatedAt == 0 {
		t.Fatalf("marker %s: location %q (%v), want test-bucket/pre/probe and a creation time", s.Value, loc, ok)
	}
	if _, ok := (ProbeRepoMarker{CreatedAt: 1}).Location(); ok {
		t.Fatal("a marker without a bucket claims a location")
	}
}

// M3-T2 r3 LOW-3: after a clean list and cat config, a failure of the worker's
// settings store (arming the never-re-create guard) is unavailable with
// ProbeRecordClass, which the console reads as not checked (C02b), not the
// internal error it counted as a storage failure (C02). It is logged without
// any identifier.
func TestC08ProbeSettingsStoreErrorIsNotAStorageFailure(t *testing.T) { // M3-INT C3
	e := newTenv(t, []fakeRule{{Cmd: "list", Stdout: "inline:0123abcd\n"}, {Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`DROP TABLE settings`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	var ae *adminpath.Error
	if !errors.As(err, &ae) || ae.Code != adminpath.CodeUnavailable || ae.Class != ProbeRecordClass {
		t.Fatalf("settings store down after a clean probe: %v, want unavailable / %s", err, ProbeRecordClass)
	}
	if got := strings.Join(e.argsOf(), ","); got != "list keys,cat" {
		t.Fatalf("calls %s, want a clean list keys,cat first", got)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "storage probe not completed: the worker settings store failed") {
		t.Fatalf("the settings store failure was not logged: %s", logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "settings store failed") {
			continue
		}
		for _, leak := range []string{"test-bucket", "pre/probe", adminpath.ProbeRepoID + `"`, "settings", e.state, e.scratch} {
			if strings.Contains(strings.ReplaceAll(line, "settings store", ""), leak) {
				t.Errorf("the log line carries an identifier (%q): %s", leak, line)
			}
		}
	}
}
