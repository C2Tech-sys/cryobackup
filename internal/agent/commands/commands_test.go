package commands

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func cid(n byte) string {
	return "01JB000000000000000000000" + string("0123456789ABCDEFGHJKMNPQRSTVWXYZ"[n])
}

func cmd(n byte, typ api.CommandType, params string) api.Command {
	c := api.Command{ID: cid(n), Type: typ, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	if params != "" {
		c.Params = json.RawMessage(params)
	}
	return c
}

type calls struct {
	backupNow, cancel, refresh, checkIn int
	started                             bool
	active                              bool
	cancelWhy                           string // the reason the Cancel handler gives when not active
	cancelRun                           string // the run_id the Cancel handler last received
	err                                 error
}

func fixture(t *testing.T) (*Executor, *calls, *journal.Journal) {
	t.Helper()
	jr, err := journal.Open(context.Background(), journal.Options{Path: filepath.Join(t.TempDir(), "j.db"), Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	c := &calls{started: true, active: true}
	return &Executor{
		Journal: jr, Clock: func() time.Time { return now },
		Handlers: Handlers{
			BackupNow: func(context.Context) (bool, error) { c.backupNow++; return c.started, c.err },
			CancelRun: func(_ context.Context, runID string) (bool, string) {
				c.cancel++
				c.cancelRun = runID
				return c.active, c.cancelWhy
			},
			RefreshPolicy: func(context.Context) error { c.refresh++; return c.err },
			CheckIn:       func(context.Context) error { c.checkIn++; return c.err },
		},
	}, c, jr
}

func result(t *testing.T, e *Executor, c api.Command) api.CommandResultRequest {
	t.Helper()
	res, err := e.Execute(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("the result does not validate: %v", err)
	}
	if err := api.AssertPathFree(res); err != nil {
		t.Fatalf("the result is not path-free: %v", err)
	}
	return res
}

func reason(t *testing.T, res api.CommandResultRequest) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(res.Result, &m); err != nil {
		t.Fatalf("result %q: %v", res.Result, err)
	}
	s, _ := m["reason"].(string)
	return s
}

func TestImplementedCommands(t *testing.T) {
	e, c, _ := fixture(t)

	res := result(t, e, cmd(1, api.CommandBackupNow, ""))
	if res.Status != api.CommandSucceeded || c.backupNow != 1 || string(res.Result) != `{"started":true}` {
		t.Errorf("backup_now: %+v calls %+v", res, c)
	}
	c.started = false
	res = result(t, e, cmd(2, api.CommandBackupNow, ""))
	if res.Status != api.CommandSucceeded || reason(t, res) != ReasonSkippedRunning {
		t.Errorf("backup_now while a run is active: %+v", res)
	}
	c.err = errors.New("boom")
	res = result(t, e, cmd(3, api.CommandBackupNow, ""))
	if res.Status != api.CommandFailed || reason(t, res) != ReasonStartFailed || res.ErrorClass != restic.ClassUnknown {
		t.Errorf("backup_now failing: %+v", res)
	}
	c.err = nil

	res = result(t, e, cmd(4, api.CommandCancel, ""))
	if res.Status != api.CommandSucceeded || c.cancel != 1 || string(res.Result) != `{"cancelled":true}` {
		t.Errorf("cancel: %+v", res)
	}
	c.active = false
	res = result(t, e, cmd(5, api.CommandCancel, ""))
	if res.Status != api.CommandSucceeded || reason(t, res) != ReasonNoRun {
		t.Errorf("cancel without a run: %+v", res)
	}

	res = result(t, e, cmd(6, api.CommandRefreshPolicy, ""))
	if res.Status != api.CommandSucceeded || c.refresh != 1 {
		t.Errorf("refresh_policy: %+v", res)
	}
	res = result(t, e, cmd(7, api.CommandCheckIn, ""))
	if res.Status != api.CommandSucceeded || c.checkIn != 1 {
		t.Errorf("check_in: %+v", res)
	}

	c.err = classed{restic.ClassAuth}
	res = result(t, e, cmd(8, api.CommandCheckIn, ""))
	if res.Status != api.CommandFailed || res.ErrorClass != restic.ClassAuth {
		t.Errorf("a failing check_in keeps the error's class: %+v", res)
	}
}

type classed struct{ c restic.ErrorClass }

func (e classed) Error() string            { return "classed" }
func (e classed) Class() restic.ErrorClass { return e.c }

func TestUnimplementedCommandsFailWithAFixedReason(t *testing.T) {
	e, _, jr := fixture(t)
	until := now.Add(time.Hour).Format(time.RFC3339)
	list := []api.Command{
		cmd(1, api.CommandPause, ""),
		cmd(2, api.CommandResume, ""),
		cmd(3, api.CommandHold, `{"until":"`+until+`"}`),
		cmd(4, api.CommandRestartService, ""),
		cmd(5, api.CommandUpdateAgent, `{"version":"1.2.3"}`),
		cmd(6, api.CommandRestore, `{"restore_request_id":"01JB0000000000000000000099"}`),
		cmd(7, api.CommandUninstall, ""),
	}
	for _, c := range list {
		res := result(t, e, c)
		if res.Status != api.CommandFailed || res.ErrorClass != restic.ClassUnknown || reason(t, res) != ReasonNotImplementedM0 {
			t.Errorf("%s: %+v", c.Type, res)
		}
	}
	// Never silently dropped: every one is in the command log with its result, waiting to be posted.
	un, _ := jr.UnpostedCommands(context.Background())
	if len(un) != len(list) {
		t.Fatalf("%d unposted results, want %d", len(un), len(list))
	}
	for _, row := range un {
		if row.Status != "failed" || row.ErrorClass != "unknown" {
			t.Errorf("logged as %+v", row)
		}
	}
}

func TestCommandIsExecutedOncePerID(t *testing.T) {
	e, c, jr := fixture(t)
	first := result(t, e, cmd(1, api.CommandBackupNow, ""))
	again := result(t, e, cmd(1, api.CommandBackupNow, ""))
	if c.backupNow != 1 {
		t.Fatalf("backup_now ran %d times for one command id", c.backupNow)
	}
	if first.Status != again.Status || string(first.Result) != string(again.Result) {
		t.Errorf("the stored result changed: %+v vs %+v", first, again)
	}
	// A result the console never acknowledged is offered again, not recomputed.
	un, _ := jr.UnpostedCommands(context.Background())
	if len(un) != 1 {
		t.Fatalf("unposted = %d", len(un))
	}
	rebuilt := ResultFromRow(un[0])
	if rebuilt.Status != first.Status || string(rebuilt.Result) != string(first.Result) {
		t.Errorf("ResultFromRow = %+v", rebuilt)
	}
}

func TestInvalidExpiredAndUnknownCommands(t *testing.T) {
	e, c, _ := fixture(t)

	expired := cmd(1, api.CommandBackupNow, "")
	expired.CreatedAt, expired.ExpiresAt = now.Add(-2*time.Hour), now.Add(-time.Hour)
	if res := result(t, e, expired); res.Status != api.CommandFailed || reason(t, res) != ReasonExpired || c.backupNow != 0 {
		t.Errorf("expired: %+v (backup_now ran: %d)", res, c.backupNow)
	}

	// expires_at is server time: a device clock two hours fast must not expire a fresh command.
	e.Offset = func() time.Duration { return 2 * time.Hour }
	e.Clock = func() time.Time { return now.Add(2 * time.Hour) }
	if res := result(t, e, cmd(2, api.CommandBackupNow, "")); res.Status != api.CommandSucceeded {
		t.Errorf("with a +2h device clock offset a fresh command expired: %+v", res)
	}
	e.Offset, e.Clock = nil, func() time.Time { return now }

	if res := result(t, e, cmd(3, api.CommandHold, "")); res.Status != api.CommandFailed || reason(t, res) != ReasonInvalid {
		t.Errorf("hold without params: %+v", res)
	}
	unknown := cmd(4, api.CommandType("from_the_future"), "")
	if res := result(t, e, unknown); res.Status != api.CommandFailed {
		t.Errorf("unknown type: %+v", res)
	}
}

func TestMissingHandlerFailsLoudly(t *testing.T) {
	e, _, _ := fixture(t)
	e.Handlers = Handlers{}
	for i, typ := range []api.CommandType{api.CommandBackupNow, api.CommandCancel, api.CommandRefreshPolicy, api.CommandCheckIn} {
		res := result(t, e, cmd(byte(i+1), typ, ""))
		if res.Status != api.CommandFailed || reason(t, res) != ReasonNotImplementedM0 {
			t.Errorf("%s without a handler: %+v", typ, res)
		}
	}
}

const runA = "01J9Y4K3A0B1C2D3E4F5G6H7J1"

// The executor hands the run_id of a cancel to the handler and reports a
// mismatch as the fixed reason; params that are not a run id are invalid.
func TestCancelRunID(t *testing.T) {
	e, c, _ := fixture(t)

	res := result(t, e, cmd(10, api.CommandCancel, `{"run_id":"`+runA+`"}`))
	if res.Status != api.CommandSucceeded || c.cancelRun != runA || string(res.Result) != `{"cancelled":true}` {
		t.Errorf("cancel naming the active run: %+v run=%q", res, c.cancelRun)
	}

	c.active, c.cancelWhy = false, ReasonRunMismatch
	res = result(t, e, cmd(11, api.CommandCancel, `{"run_id":"`+runA+`"}`))
	if res.Status != api.CommandSucceeded || string(res.Result) != `{"cancelled":false,"reason":"run_mismatch"}` {
		t.Errorf("cancel naming another run: %+v", res)
	}

	// No run_id (an older console): the handler is asked with "" and may stop any run.
	c.active, c.cancelWhy = true, ""
	res = result(t, e, cmd(12, api.CommandCancel, ""))
	if res.Status != api.CommandSucceeded || c.cancelRun != "" || string(res.Result) != `{"cancelled":true}` {
		t.Errorf("cancel without run_id: %+v run=%q", res, c.cancelRun)
	}
	res = result(t, e, cmd(13, api.CommandCancel, `{}`))
	if res.Status != api.CommandSucceeded || c.cancelRun != "" {
		t.Errorf("cancel with empty params: %+v", res)
	}

	// An unknown reason from a handler never reaches the wire.
	c.active, c.cancelWhy = false, "C:/secret"
	res = result(t, e, cmd(14, api.CommandCancel, ""))
	if reason(t, res) != ReasonNoRun {
		t.Errorf("unknown handler reason leaked: %s", res.Result)
	}

	before := c.cancel
	for i, bad := range []string{`{"run_id":""}`, `{"run_id":7}`, `[1]`} {
		res = result(t, e, cmd(byte(20+i), api.CommandCancel, bad))
		if res.Status != api.CommandFailed || reason(t, res) != ReasonInvalid {
			t.Errorf("cancel params %s: %+v", bad, res)
		}
	}
	if c.cancel != before {
		t.Error("an invalid cancel reached the handler")
	}
}
