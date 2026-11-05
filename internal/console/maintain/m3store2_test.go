package maintain

import (
	"errors"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Mutant m43, the settings-WRITE half of the worker-store answer: the row can be
// read (it is absent) but not written. A trigger makes the write fail and leaves
// the reads alone, which DROP TABLE settings cannot do.
func TestC17ProbeSettingsWriteFailureIsAWorkerStoreAnswer(t *testing.T) { // M3-STORE2 3
	for _, c := range []struct {
		name  string
		rules []fakeRule
		calls string
	}{
		{name: "after a clean list and cat", rules: []fakeRule{{Cmd: "list", Stdout: "inline:0123abcd\n"}, {Cmd: "cat", Stdout: "cat_config.json"}},
			calls: "list keys,cat"},
		{name: "on the first run, after init", rules: []fakeRule{{Cmd: "list", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"},
			{Cmd: "init", Stdout: "init.json"}, {Cmd: "cat", Stdout: "cat_config.json"}},
			calls: "list keys,init"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTenv(t, c.rules, nil)
			e.withProbeKey()
			if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER no_settings_write BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT, 'the settings store is down'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			_, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
			var ae *adminpath.Error
			if !errors.As(err, &ae) || ae.Code != adminpath.CodeUnavailable || ae.Class != ProbeRecordClass {
				t.Fatalf("the settings row cannot be written: %v, want unavailable / %s", err, ProbeRecordClass)
			}
			if got := strings.Join(e.argsOf(), ","); got != c.calls {
				t.Fatalf("calls %s, want %s", got, c.calls)
			}
			if logs := e.logs.String(); !strings.Contains(logs, "storage probe not completed: the worker settings store failed") {
				t.Fatalf("the settings store failure was not logged: %s", logs)
			}
			if _, gerr := e.st.GetSetting(e.ctx, SettingProbeRepo); !errors.Is(gerr, store.ErrNotFound) {
				t.Fatalf("the settings row after a failed write: %v, want not found", gerr)
			}
		})
	}
}
