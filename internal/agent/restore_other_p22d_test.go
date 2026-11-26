//go:build !windows

package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestRestoreRefusedOffWindows: a device restore is Windows only; elsewhere it
// is accepted, refused loudly and closed failed/other with a valid record.
func TestRestoreRefusedOffWindows(t *testing.T) {
	fc := newFakeConsole(t)
	fc.setPolicy(api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`, Paused: true})
	dir := enroll(t, fc)
	const id = "01JB0000000000000000000R01"
	fc.setRestore(id, restoreAnswer{sel: api.RestoreSelection{ID: id, SnapshotID: "c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd",
		Mode: api.DeviceRestoreOriginal, Overwrite: api.DeviceOverwriteNever, Paths: []string{"/C/a.txt"}, ExpiresAt: time.Now().Add(time.Hour)}})
	run := start(t, fakeOptions(t, dir))
	params, _ := json.Marshal(api.RestoreParams{RestoreRequestID: id})
	fc.enqueue(api.Command{ID: "01JB0000000000000000000C01", Type: api.CommandRestore, Params: params,
		CreatedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)})
	waitFor(t, "the closed restore", 30*time.Second, func() bool {
		rec, ok := fc.run(id)
		return ok && !rec.Open()
	})
	run.stop()
	rec, _ := fc.run(id)
	if err := rec.Validate(); err != nil || rec.Outcome != restic.OutcomeFailed || rec.Stats.RestoreResult != api.RestoreResultOther {
		t.Fatalf("record = %s / %+v (%v)", rec.Outcome, rec.Stats, err)
	}
}
