package maintain

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Triggers of console runs (runs.trigger_kind). The admin-path RPC is an
// operator action; the scheduler's own calls are "schedule".
const (
	triggerManual   = "manual"
	triggerSchedule = "schedule"
)

// closeTimeout bounds the database write that closes a run row after the call
// itself was cancelled or timed out.
const closeTimeout = 10 * time.Second

// openRun inserts an open console run (architecture §7.5: forget, prune, check
// and unlock are recorded, read-only calls are not). A failure here is returned
// before anything destructive runs: an unrecorded unlock is worse than a
// refused one.
func (w *Worker) openRun(ctx context.Context, deviceID string, kind restic.Kind, trigger string) (string, error) {
	run, err := w.st.CreateConsoleRun(ctx, store.ScopeAll(), store.ConsoleRunInput{
		DeviceID: deviceID, Kind: string(kind), Trigger: trigger,
	})
	if err != nil {
		w.log.Error("run row not created", attrKind(string(kind)), attrDevice(deviceID))
		return "", w.storeErr(ctx, err)
	}
	return run.ID, nil
}

// closeRun finishes the run row with the outcome of res. A failure to write is
// logged (class and ids only) and swallowed: the restic operation already
// happened and its result still goes back to the caller.
func (w *Worker) closeRun(ctx context.Context, runID, deviceID string, kind restic.Kind, res execResult) {
	f := store.ConsoleRunResult{
		Outcome:        string(res.Result.Outcome),
		ErrorClass:     string(res.Result.ErrorClass),
		DeferredReason: string(res.Result.DeferredReason),
	}
	if res.Exit >= 0 {
		code := res.Exit
		f.ResticExitCode = &code
	}
	if len(res.Counts) > 0 {
		f.ErrorCounts = make(map[string]int)
		for c, n := range res.Counts.ByClass() {
			f.ErrorCounts[string(c)] = n
		}
	}
	w.finish(ctx, runID, deviceID, kind, f)
}

// abortRun closes a run whose process never finished (timeout or cancellation).
func (w *Worker) abortRun(ctx context.Context, runID, deviceID string, kind restic.Kind) {
	w.finish(ctx, runID, deviceID, kind, store.ConsoleRunResult{Outcome: string(restic.OutcomeInterrupted)})
}

func (w *Worker) finish(ctx context.Context, runID, deviceID string, kind restic.Kind, f store.ConsoleRunResult) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if _, err := w.st.FinishConsoleRun(fctx, store.ScopeAll(), runID, f); err != nil {
		w.log.Error("run row not closed", attrKind(string(kind)), attrDevice(deviceID))
	}
}

// Unlock implements adminpath.Backend: `restic unlock`, or `unlock --remove-all`
// when RemoveAll is set, which the contract allows only for a quiesced device
// (architecture §6.3). The call is recorded as a console run of kind unlock
// (architecture §7.5), trigger manual.
func (w *Worker) Unlock(ctx context.Context, req adminpath.UnlockRequest) (adminpath.UnlockResult, error) {
	if req.RemoveAll && !req.Quiesced {
		return adminpath.UnlockResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	res, err := w.unlock(ctx, req.DeviceID, req.RemoveAll, triggerManual)
	if err != nil {
		return adminpath.UnlockResult{}, err
	}
	return adminpath.UnlockResult{ResultMeta: res.meta()}, nil
}

// unlock is the shared body of the RPC and the scheduler's step 2.
func (w *Worker) unlock(ctx context.Context, deviceID string, removeAll bool, trigger string) (execResult, error) {
	t, err := w.prepare(ctx, deviceID)
	if err != nil {
		return execResult{}, err
	}
	defer t.drop()
	cmd, err := restic.Unlock(removeAll, w.common())
	if err != nil {
		return execResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	runID, err := w.openRun(ctx, deviceID, restic.KindUnlock, trigger)
	if err != nil {
		return execResult{}, err
	}
	res, err := w.exec(ctx, t, cmd, w.cfg.Timeouts.Unlock, nil)
	if err != nil {
		w.abortRun(ctx, runID, deviceID, restic.KindUnlock)
		return execResult{}, err
	}
	if abort := abortError(ctx, res); abort != nil {
		w.abortRun(ctx, runID, deviceID, restic.KindUnlock)
		return execResult{}, abort
	}
	w.closeRun(ctx, runID, deviceID, restic.KindUnlock, res)
	return res, nil
}
