package commands

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// TestRestoreIsAcceptedAtOnce: the executor hands the request id to the
// handler and answers at once (ADR 0027: the agent accepts the command at
// once and runs the restore in its own goroutine); a refusal carries only a
// fixed reason; a second delivery of the same command id is answered from the
// command log without calling the handler again.
func TestRestoreIsAcceptedAtOnce(t *testing.T) {
	e, _, _ := fixture(t)
	var got []string
	answer, why := true, ""
	e.Handlers.Restore = func(_ context.Context, id string) (bool, string) {
		got = append(got, id)
		return answer, why
	}
	const req = "01JB0000000000000000000099"
	res := result(t, e, cmd(1, api.CommandRestore, `{"restore_request_id":"`+req+`"}`))
	if res.Status != api.CommandSucceeded {
		t.Fatalf("accepted restore = %+v", res)
	}
	var m map[string]any
	if err := json.Unmarshal(res.Result, &m); err != nil || m["accepted"] != true {
		t.Fatalf("result = %s", res.Result)
	}
	if len(got) != 1 || got[0] != req {
		t.Fatalf("handler got %v", got)
	}
	// The same command id again: from the log, the handler is not called.
	_ = result(t, e, cmd(1, api.CommandRestore, `{"restore_request_id":"`+req+`"}`))
	if len(got) != 1 {
		t.Fatalf("the handler ran %d times for one command id", len(got))
	}

	for i, tc := range []struct{ why, want string }{
		{ReasonRestoreActive, ReasonRestoreActive},
		{ReasonAlreadyRun, ReasonAlreadyRun},
		{ReasonStopping, ReasonStopping},
		{"some free text C:\\Users", ReasonFailed},
	} {
		answer, why = false, tc.why
		res := result(t, e, cmd(byte(2+i), api.CommandRestore, `{"restore_request_id":"`+req+`"}`))
		if res.Status != api.CommandFailed || reason(t, res) != tc.want {
			t.Errorf("refusal %q = %+v", tc.why, res)
		}
	}

	// Params that do not name a restore request never reach the handler.
	before := len(got)
	res = result(t, e, cmd(9, api.CommandRestore, `{"restore_request_id":"not-a-ulid"}`))
	if res.Status != api.CommandFailed || reason(t, res) != ReasonInvalid || len(got) != before {
		t.Errorf("invalid params = %+v (handler calls %d -> %d)", res, before, len(got))
	}
}
