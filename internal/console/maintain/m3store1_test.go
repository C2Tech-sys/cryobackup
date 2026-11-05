package maintain

import (
	"errors"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-STORE1 E, the repo_missing branch of probeStorage: the list answers exit 10
// and the settings row cannot be read, so the worker cannot tell "never created"
// from "lost". It must answer the worker-store class, create nothing, and not
// report repo_missing.
func TestC17ProbeRepoMissingWithSettingsDownIsAWorkerStoreAnswer(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "list", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"}, {Cmd: "init", Stdout: "init.json"}}, nil)
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
		t.Fatalf("exit 10 with the settings store down: %v, want unavailable / %s", err, ProbeRecordClass)
	}
	if got := strings.Join(e.argsOf(), ","); got != "list keys" {
		t.Fatalf("calls %s, want only the list: nothing may be created", got)
	}
}
