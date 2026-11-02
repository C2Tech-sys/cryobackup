package commands

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Fixed reasons carried in command results (never free text).
const (
	ReasonNotImplementedM0 = "not_implemented_m0"
	ReasonUnknownCommand   = "unknown_command"
	ReasonInvalid          = "invalid"
	ReasonExpired          = "expired"
	ReasonSkippedRunning   = "skipped_running"
	ReasonNoRun            = "no_run"
	ReasonRunMismatch      = "run_mismatch"
	ReasonStartFailed      = "start_failed"
	ReasonFailed           = "failed"
)

// Handlers is what the agent offers the executor. A nil handler makes the
// command fail with ReasonNotImplementedM0.
type Handlers struct {
	// BackupNow starts a backup that bypasses window and conditions. started is
	// false when it was skipped because a run is active (skip_if_running).
	BackupNow func(ctx context.Context) (started bool, err error)
	// CancelRun asks the run named by runID to stop. runID is the run_id of the
	// command; empty means the command named none (any active run). It reports
	// whether a run was stopped and, when not, why: ReasonNoRun (nothing is
	// running and the command named no run) or ReasonRunMismatch (the command
	// named a run that is not the active one, or that has finished).
	CancelRun func(ctx context.Context, runID string) (cancelled bool, reason string)
	// RefreshPolicy pulls the policy now.
	RefreshPolicy func(ctx context.Context) error
	// CheckIn sends a heartbeat now.
	CheckIn func(ctx context.Context) error
}

// Executor runs commands exactly once per id and records every result in the
// journal's command log, so a result lost on the wire is re-posted, never
// recomputed (architecture §6.1 "command executor").
type Executor struct {
	Journal  *journal.Journal
	Handlers Handlers
	// Clock is the device clock; nil means time.Now.
	Clock func() time.Time
	// Offset returns device minus server clock, used to judge expires_at
	// (server time); nil means zero.
	Offset func() time.Duration
}

// Execute runs one command and returns the result to post. A command id seen
// before is not run again: its stored result is returned (a command that was
// interrupted mid-run by a restart is run again, and the commands that can
// reach that state are idempotent). The returned error is only a journal
// failure.
func (e *Executor) Execute(ctx context.Context, cmd api.Command) (api.CommandResultRequest, error) {
	row, created, err := e.Journal.RecordCommand(ctx, cmd.ID, string(cmd.Type), cmd.Params)
	if err != nil {
		return api.CommandResultRequest{}, err
	}
	if !created && row.Status != journal.CommandReceived {
		return ResultFromRow(row), nil
	}

	res := e.run(ctx, cmd)
	if err := e.Journal.FinishCommand(ctx, cmd.ID, string(res.Status), string(res.ErrorClass), res.Result); err != nil {
		return res, err
	}
	return res, nil
}

// ResultFromRow rebuilds the result of a finished command-log entry.
func ResultFromRow(row journal.CommandRow) api.CommandResultRequest {
	res := api.CommandResultRequest{Status: api.CommandStatus(row.Status), ErrorClass: restic.ErrorClass(row.ErrorClass)}
	if row.Result != "" {
		res.Result = json.RawMessage(row.Result)
	}
	return res
}

func (e *Executor) now() time.Time {
	if e.Clock != nil {
		return e.Clock()
	}
	return time.Now()
}

func (e *Executor) run(ctx context.Context, cmd api.Command) api.CommandResultRequest {
	if err := cmd.Validate(); err != nil {
		return failed(ReasonInvalid)
	}
	if !cmd.ExpiresAt.IsZero() {
		serverNow := e.now()
		if e.Offset != nil {
			serverNow = serverNow.Add(-e.Offset())
		}
		if serverNow.After(cmd.ExpiresAt) {
			return failed(ReasonExpired)
		}
	}
	h := e.Handlers
	switch cmd.Type {
	case api.CommandBackupNow:
		if h.BackupNow == nil {
			return failed(ReasonNotImplementedM0)
		}
		started, err := h.BackupNow(ctx)
		switch {
		case err != nil:
			return failedClass(ReasonStartFailed, classOf(err))
		case !started:
			return succeeded(map[string]any{"started": false, "reason": ReasonSkippedRunning})
		}
		return succeeded(map[string]any{"started": true})
	case api.CommandCancel:
		if h.CancelRun == nil {
			return failed(ReasonNotImplementedM0)
		}
		runID, ok := cancelRunID(cmd.Params)
		if !ok {
			return failed(ReasonInvalid)
		}
		cancelled, why := h.CancelRun(ctx, runID)
		if cancelled {
			return succeeded(map[string]any{"cancelled": true})
		}
		if why != ReasonRunMismatch {
			why = ReasonNoRun // only the two fixed reasons ever reach the wire
		}
		return succeeded(map[string]any{"cancelled": false, "reason": why})
	case api.CommandRefreshPolicy:
		if h.RefreshPolicy == nil {
			return failed(ReasonNotImplementedM0)
		}
		if err := h.RefreshPolicy(ctx); err != nil {
			return failedClass(ReasonFailed, classOf(err))
		}
		return succeeded(nil)
	case api.CommandCheckIn:
		if h.CheckIn == nil {
			return failed(ReasonNotImplementedM0)
		}
		if err := h.CheckIn(ctx); err != nil {
			return failedClass(ReasonFailed, classOf(err))
		}
		return succeeded(nil)
	case api.CommandPause, api.CommandResume, api.CommandHold, api.CommandRestartService,
		api.CommandUpdateAgent, api.CommandRestore, api.CommandUninstall:
		// Journaled as failed, never silently dropped (milestones M1, M5, M6, M8, M11).
		return failed(ReasonNotImplementedM0)
	}
	return failed(ReasonUnknownCommand)
}

// cancelRunID reads the optional run_id of a cancel command. ok is false when
// the params are not an object, or run_id is present but not a non-empty
// string. A command with no params (or no run_id) yields "": cancel whatever
// run is active, as every cancel did before the console sent run_id.
func cancelRunID(params json.RawMessage) (runID string, ok bool) {
	if len(params) == 0 {
		return "", true
	}
	var p struct {
		RunID *string `json:"run_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", false
	}
	if p.RunID == nil {
		return "", true
	}
	return *p.RunID, *p.RunID != ""
}

func succeeded(result map[string]any) api.CommandResultRequest {
	return api.CommandResultRequest{Status: api.CommandSucceeded, Result: raw(result)}
}

func failed(reason string) api.CommandResultRequest {
	return failedClass(reason, restic.ClassUnknown)
}

func failedClass(reason string, class restic.ErrorClass) api.CommandResultRequest {
	return api.CommandResultRequest{
		Status: api.CommandFailed, ErrorClass: class,
		Result: raw(map[string]any{"reason": reason}),
	}
}

func raw(m map[string]any) json.RawMessage {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// classOf maps an error to a class when the error knows one (the uplink's
// errors do), else unknown.
func classOf(err error) restic.ErrorClass {
	var c interface{ Class() restic.ErrorClass }
	if errors.As(err, &c) {
		if class := c.Class(); class != "" {
			return class
		}
	}
	return restic.ClassUnknown
}
