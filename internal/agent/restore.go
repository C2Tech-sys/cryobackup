package agent

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/commands"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/restoreplace"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The restore executor (ADR 0027): the restore command carries only the
// request id; the agent accepts it at once, fetches the selection with its
// device token, waits for a running backup, runs restic once per volume
// through the gateway with its own login and DPAPI repository password, and
// places the files itself (package restoreplace). The run is journaled with
// kind restore, trigger manual and the request id as its run id, so a cancel
// names it and it runs at most once; it never resumes after a restart.
//
// Nothing here logs a path, a file name or the selection: device log lines,
// Event Log entries, journal rows and the record carry ids, drive letters,
// classes and counts only.

// Event Log ids of a restore.
const (
	eventRestoreDone   = 1020 // info: restored, or restored with errors
	eventRestoreFailed = 1021 // error: failed, cancelled or interrupted
)

// Defaults of the restore executor; tests shorten them through Options.
const (
	// restoreIdleBound is how long a restic restore may go without moving
	// (no change in the files or bytes of its status lines) before the agent
	// stops it and the restore fails with a storage class (ADR 0027: "A
	// restore that stops moving is stopped"; with the gateway gone restic
	// retried for more than 20 minutes, p22-spike.md:241). It also covers the
	// time before the first status line, when restic loads the index (10-60 s
	// per call on a loaded PC, spike item 7).
	restoreIdleBound = 15 * time.Minute
	// restoreSlotPoll is how often a waiting restore looks whether the
	// running backup has ended.
	restoreSlotPoll = time.Second
	// restoreFetchFor bounds the retries of a fetch the console did not
	// answer (network, 5xx, 401, 403, 429); a final refusal is never retried.
	restoreFetchFor = 30 * time.Minute
)

// errRestoreStalled is the cancel cause of the restore watchdog.
var errRestoreStalled = errors.New("agent: the restore stopped moving")

// restoreRun is the restore in progress; there is at most one.
type restoreRun struct {
	id     string
	cancel context.CancelCauseFunc
}

// cmdRestore is the executor's Restore handler: it accepts the request at
// once and runs it on its own goroutine. The open run row is written before it
// answers, so a restart after the answer finds the run (closed as interrupted
// by OpenRunsOnStart) and a second delivery of the command is refused with
// already_run: a restore never runs twice by accident.
func (a *Agent) cmdRestore(ctx context.Context, id string) (bool, string) {
	a.mu.Lock()
	switch {
	case a.stopping || a.baseCtx == nil:
		a.mu.Unlock()
		return false, commands.ReasonStopping
	case a.restoring != nil:
		a.mu.Unlock()
		return false, commands.ReasonRestoreActive
	}
	rctx, cancel := context.WithCancelCause(a.baseCtx)
	run := &restoreRun{id: id, cancel: cancel}
	a.restoring = run
	a.mu.Unlock()
	fail := func(why string) (bool, string) {
		cancel(nil)
		a.mu.Lock()
		if a.restoring == run {
			a.restoring = nil
		}
		a.mu.Unlock()
		return false, why
	}

	jctx := context.WithoutCancel(ctx)
	if _, seen, err := a.jr.Run(jctx, id); err != nil {
		a.log.Error("restore not accepted: the journal could not be read", "restore", id, "err", err.Error())
		return fail(commands.ReasonFailed)
	} else if seen {
		a.log.Warn("restore not accepted: this restore has already run on this device", "restore", id)
		return fail(commands.ReasonAlreadyRun)
	}
	rec := api.RunRecord{
		ID: id, Kind: restic.KindRestore, Trigger: api.TriggerManual, StartedAt: a.clock().UTC(),
		Attempt: 1, AgentVersion: versionLabel(),
	}
	a.mu.Lock()
	if a.pol != nil {
		rec.PolicyVersion = a.pol.Version
	}
	a.mu.Unlock()
	if err := a.jr.StartRun(jctx, rec); err != nil {
		a.log.Error("restore not accepted: the run row could not be written", "restore", id, "err", err.Error())
		return fail(commands.ReasonFailed)
	}
	a.mu.Lock()
	a.runWG.Add(1)
	a.mu.Unlock()
	a.log.Info("restore accepted", "restore", id)
	kick(a.kickOutbox) // the open record goes out at once
	go func() {
		defer a.runWG.Done()
		defer cancel(nil)
		a.runRestore(rctx, rec)
	}()
	return true, ""
}

// runRestore runs one accepted restore to its end and closes its run.
func (a *Agent) runRestore(ctx context.Context, rec api.RunRecord) {
	jctx := context.WithoutCancel(ctx)
	defer func() {
		a.mu.Lock()
		if a.restoring != nil && a.restoring.id == rec.ID {
			a.restoring = nil
		}
		a.mu.Unlock()
		a.clearRunning(rec.ID)
		_ = a.jr.KVDelete(jctx, journal.KVRestoreWaiting)
		if a.sched != nil {
			a.sched.Notify(scheduler.TriggerTick)
		}
		kick(a.kickOutbox)
		kick(a.kickHeartbeat)
	}()

	out := a.executeRestore(ctx, rec.ID)
	outcome, stats := restoreVerdict(out)
	finished := a.clock().UTC()
	if finished.Before(rec.StartedAt) {
		finished = rec.StartedAt
	}
	closed := rec
	closed.FinishedAt, closed.Outcome, closed.Stats, closed.ExitCode = &finished, outcome, stats, out.exit
	if err := closed.Validate(); err != nil {
		// A record the console would refuse would be journaled as refused and
		// never sent: the console would never learn the outcome. This is a
		// bug; it is said loudly and the restore is closed as failed/other,
		// which always validates.
		a.log.Error("restore record does not validate; closing it as failed/other", "restore", rec.ID, "err", err.Error())
		a.eventLog("error", eventRestoreFailed, "a restore record did not validate and was closed as failed/other (agent bug)")
		closed.Outcome, closed.Stats = restic.OutcomeFailed, api.RunStats{RestoreResult: api.RestoreResultOther}
	}
	if err := a.jr.FinishRun(jctx, closed); err != nil {
		a.log.Error("restore run could not be closed in the journal", "restore", rec.ID, "err", err.Error())
	}
	a.log.Info("restore finished", "restore", rec.ID, "outcome", string(closed.Outcome), "result", string(closed.Stats.RestoreResult),
		"restored", closed.Stats.FilesRestored, "skipped", closed.Stats.FilesSkipped, "runs", out.runs, "matched_nothing", out.nothing)
	switch closed.Outcome {
	case restic.OutcomeSuccess, restic.OutcomePartial:
		a.eventLog("info", eventRestoreDone, "restore finished: "+string(closed.Outcome))
	default:
		a.eventLog("error", eventRestoreFailed, "restore "+string(closed.Outcome)+": "+string(closed.Stats.RestoreResult))
	}
}

// cancelClass maps how ctx ended to a run-level class: the operator's cancel
// is cancelled, the service stopping is interrupted.
func cancelClass(ctx context.Context) api.RestoreResultClass {
	if errors.Is(context.Cause(ctx), supervisor.ErrServiceStop) {
		return api.RestoreResultInterrupted
	}
	return api.RestoreResultCancelled
}

// executeRestore does the work and reports what happened; it never closes the
// run itself.
func (a *Agent) executeRestore(ctx context.Context, id string) (out restoreOutcome) {
	sel, fatal := a.fetchSelection(ctx, id)
	if fatal != "" {
		out.fatal = fatal
		return out
	}
	if runtime.GOOS != "windows" {
		a.log.Error("restore refused: device restore is only implemented on Windows", "restore", id)
		out.fatal = api.RestoreResultOther
		return out
	}
	a.mu.Lock()
	missing := a.repoMissing
	a.mu.Unlock()
	if missing {
		a.log.Error("restore refused: no repository key on this device", "restore", id)
		out.fatal = api.RestoreResultOther
		return out
	}
	groups := groupSelection(sel.Paths)
	mode := restoreplace.Keep
	if sel.Overwrite == api.DeviceOverwriteIfChanged {
		mode = restoreplace.Replace
	}
	side := sel.Mode == api.DeviceRestoreSide

	if !a.acquireRestoreSlot(ctx, id) {
		out.fatal = cancelClass(ctx)
		a.mu.Lock()
		if a.stopping {
			out.fatal = api.RestoreResultInterrupted // the stop is seen before its cancel reaches ctx
		}
		a.mu.Unlock()
		return out
	}
	err := withIncludeFiles(a.layout.TempDir(), id, groups, func(files []string) error {
		a.restoreGroups(ctx, id, sel.SnapshotID, groups, files, side, mode, &out)
		return nil
	})
	if err != nil && out.fatal == "" {
		a.log.Error("restore failed: the include files could not be handled", "restore", id, "err", err.Error())
		out.fatal = api.RestoreResultOther
	}
	return out
}

// fetchSelection GETs the selection with the device token and validates it
// with the api validator. A refusal by code is final (no retry wedge): the
// request is gone, expired, for another device, retired, purging or already
// active elsewhere, or this agent is too old. A console that does not answer
// is retried for restoreFetchFor. The selection is never logged.
func (a *Agent) fetchSelection(ctx context.Context, id string) (api.RestoreSelection, api.RestoreResultClass) {
	bo := a.backoff()
	deadline := a.clock().Add(a.restoreFetchFor())
	for {
		sel, err := a.up.GetRestore(ctx, id)
		if err == nil {
			if verr := sel.Validate(); verr != nil || sel.ID != id {
				// The validation message names fields, never a path.
				msg := "id mismatch"
				if verr != nil {
					msg = verr.Error()
				}
				a.log.Error("restore refused: the selection does not validate", "restore", id, "err", msg)
				return api.RestoreSelection{}, api.RestoreResultOther
			}
			serverNow := a.clock()
			if off, ok := a.up.OffsetMS(); ok {
				serverNow = serverNow.Add(-time.Duration(off) * time.Millisecond)
			}
			if serverNow.After(sel.ExpiresAt) {
				a.log.Warn("restore dropped: the selection has expired", "restore", id)
				return api.RestoreSelection{}, api.RestoreResultOther
			}
			if a.consoleWasDown() {
				a.consoleBack("restore")
			}
			return sel, ""
		}
		if ctx.Err() != nil {
			return api.RestoreSelection{}, cancelClass(ctx)
		}
		if code, final := uplink.RestoreRefusal(err); final {
			if code == "" {
				code = "refused"
			}
			a.log.Warn("restore refused by the console", "restore", id, "code", code)
			return api.RestoreSelection{}, api.RestoreResultOther
		}
		a.uplinkFailed(err)
		wait := bo.Delay(err)
		if a.clock().Add(wait).After(deadline) {
			a.log.Error("restore failed: the console did not answer the selection fetch", "restore", id, "err", err.Error())
			return api.RestoreSelection{}, api.RestoreResultOther
		}
		a.log.Warn("restore selection fetch failed; retrying", "restore", id, "err", err.Error(), "retry_in_s", int(wait/time.Second))
		if !sleepOrKick(ctx, wait, nil) {
			return api.RestoreSelection{}, cancelClass(ctx)
		}
	}
}

// acquireRestoreSlot waits until no restic child runs on the device and then
// takes the slot (a.running) for the restore: one restic child at a time
// (ADR 0027: a restore waits for a running backup). While it waits it says so
// in the device log and in the journal (KVRestoreWaiting), once. It reports
// false when ctx ended or the agent is stopping.
func (a *Agent) acquireRestoreSlot(ctx context.Context, id string) bool {
	poll := a.o.restoreSlotPoll
	if poll <= 0 {
		poll = restoreSlotPoll
	}
	said := false
	for {
		a.mu.Lock()
		if a.stopping {
			a.mu.Unlock()
			return false
		}
		if a.running == nil {
			cancel := func(error) {}
			if a.restoring != nil && a.restoring.id == id {
				cancel = a.restoring.cancel
			}
			a.running = &activeRun{id: id, kind: restic.KindRestore, cancel: cancel, start: a.clock()}
			a.mu.Unlock()
			if said {
				_ = a.jr.KVDelete(context.WithoutCancel(ctx), journal.KVRestoreWaiting)
				a.log.Info("restore starting after the backup", "restore", id)
			}
			return true
		}
		a.mu.Unlock()
		if !said {
			said = true
			a.log.Info("restore waiting for the running backup", "restore", id)
			_ = a.jr.KVSet(context.WithoutCancel(ctx), journal.KVRestoreWaiting, id+" "+a.clock().UTC().Format(time.RFC3339))
		}
		if !sleepOrKick(ctx, poll, nil) {
			return false
		}
	}
}

// restoreGroups runs the free-space check over every group first (nothing is
// written before it), then restic and the placement group by group.
func (a *Agent) restoreGroups(ctx context.Context, id, snap string, groups []restoreGroup, files []string, side bool, mode restoreplace.Mode, out *restoreOutcome) {
	targets := make([]string, len(groups))
	sideTop := ""
	if side {
		sideTop = a.sideRoot() + `\` + id
	}
	for i, g := range groups {
		if side {
			targets[i] = sideTop + `\` + g.key
		} else {
			targets[i] = a.stagingRoot(g.letter) + `\` + id
		}
	}

	// 1. Free space (ADR 0027, "Free space"): the selection's size per group
	// from a dry run of the very restore restic will do, summed per volume
	// restic writes to (each destination volume, or the system drive for the
	// side folder), against that volume's free space. Short: FAILED disk,
	// before anything is written; the numbers go to the device log only.
	//
	// Why a dry run and not "restic ls" as ADR 0027 words it (foreman ruling
	// R-size, 2026-11-25): the builder lists one folder level only
	// (--recursive is a forbidden flag), so ls cannot sum a selected folder;
	// the dry run is restic's own sizing with exactly the include semantics
	// of the real run, and writes nothing. A dry run that fails maps like the
	// real run (auth, repo_unreachable, other: superviseRestore), and one
	// without a summary is other: the agent never assumes the selection fits.
	need := map[string]uint64{}
	for i, g := range groups {
		size, fatal, exit := a.restoreDryRun(ctx, id, snap, g, files[i], targets[i])
		out.exit = exit
		if fatal != "" {
			out.fatal = fatal
			return
		}
		vol := g.letter
		if side {
			vol = a.sideVolume()
		}
		need[vol] += size
	}
	for vol, n := range need {
		free, err := a.freeBytes(vol, side)
		if err != nil {
			a.log.Error("restore refused: the free space could not be read", "restore", id, "volume", vol, "err", err.Error())
			out.fatal = api.RestoreResultDisk
			return
		}
		if n > free {
			a.log.Error("restore refused: not enough free space", "restore", id, "volume", vol,
				"needed_bytes", n, "free_bytes", free)
			out.fatal = api.RestoreResultDisk
			return
		}
	}

	// 2. The roots, the fresh folders, restic and the placement.
	if side {
		r, err := restoreplace.OpenRoot(a.sideRoot(), a.placeOptions())
		if err != nil {
			a.rootRefused(id, "side", a.sideVolume(), err)
			out.fatal = api.RestoreResultGuardRefused
			return
		}
		defer r.Close() // held for the whole restore
		if err := restoreplace.CreateFresh(sideTop); err != nil {
			a.log.Error("restore refused: the restore's own side folder already exists or cannot be made", "restore", id, "err", errText(err))
			out.fatal = api.RestoreResultGuardRefused
			return
		}
	}
	roots := map[string]*restoreplace.Root{}
	defer func() {
		for _, r := range roots {
			r.Close()
		}
	}()
	for i, g := range groups {
		if ctx.Err() != nil {
			out.fatal = cancelClass(ctx)
			return
		}
		var ok bool
		if side {
			ok = a.restoreSideGroup(ctx, id, snap, g, files[i], targets[i], out)
		} else {
			root := roots[g.letter]
			if root == nil {
				r, err := restoreplace.OpenRoot(a.stagingRoot(g.letter), a.placeOptions())
				if err != nil {
					a.rootRefused(id, "staging", g.letter, err)
					out.fatal = api.RestoreResultGuardRefused
					return
				}
				roots[g.letter], root = r, r
			}
			ok = a.restoreOriginalGroup(ctx, id, snap, g, files[i], targets[i], mode, out)
		}
		if !ok {
			return
		}
	}
}

// rootRefused writes the device log line the root rule asks for: which root,
// which drive, which check failed. The agent does not repair the root.
func (a *Agent) rootRefused(id, which, letter string, err error) {
	var re *restoreplace.RootError
	check := "unknown"
	if errors.As(err, &re) {
		check = re.Check
	} else if err != nil {
		check = errText(err)
	}
	a.log.Error("restore refused: the "+which+" root fails the root rule; remove it and run the restore again (the agent does not repair it)",
		"restore", id, "drive", letter, "check", check)
}

// restoreOriginalGroup restores one group into its fresh staging folder and
// places the files (ADR 0027, "Original location: restore into staging, then
// place"). The staging folder is removed by the clean-up walk whatever
// happens. It reports false when the restore must stop (out.fatal is set).
func (a *Agent) restoreOriginalGroup(ctx context.Context, id, snap string, g restoreGroup, include, staging string, mode restoreplace.Mode, out *restoreOutcome) bool {
	if err := restoreplace.CreateFresh(staging); err != nil {
		a.log.Error("restore refused: the restore's own staging folder already exists or cannot be made", "restore", id, "drive", g.letter, "err", errText(err))
		out.fatal = api.RestoreResultGuardRefused
		return false
	}
	defer func() {
		if _, err := restoreplace.RemoveTree(staging); err != nil {
			a.log.Error("restore staging folder could not be removed; the next agent start removes it", "restore", id, "drive", g.letter, "err", errText(err))
		}
	}()
	if !a.restoreRun(ctx, id, snap, g, include, staging, out) {
		return out.fatal == ""
	}
	err := restoreplace.WithPrivileges(func() error {
		links, err := restoreplace.RemoveReparsePoints(staging)
		out.counts.Skipped += int64(links)
		if err != nil {
			return err
		}
		c, err := restoreplace.Place(staging, a.driveRoot(g.letter), g.rels, mode)
		out.counts.Merge(c)
		return err
	})
	if err != nil {
		a.log.Error("restore placement failed", "restore", id, "drive", g.letter, "err", errText(err))
		out.fatal = api.RestoreResultOther
		return false
	}
	return true
}

// restoreSideGroup restores one group into the side folder, deletes every
// reparse point in it, resets it to inherit from the side folder with the
// agent's own walk, checks one file and counts what is there (ADR 0027, "Side
// folder").
func (a *Agent) restoreSideGroup(ctx context.Context, id, snap string, g restoreGroup, include, target string, out *restoreOutcome) bool {
	if err := restoreplace.CreateFresh(target); err != nil {
		// Two drive segments that differ only in case share one folder name:
		// the second cannot have a fresh folder, so its items are refused.
		a.log.Error("restore group refused: its side folder already exists", "restore", id, "drive", g.letter, "err", errText(err))
		out.counts.Add(restoreplace.ClassGuardRefused, int64(len(g.rels)))
		return true
	}
	if !a.restoreRun(ctx, id, snap, g, include, target, out) {
		return out.fatal == ""
	}
	err := restoreplace.WithPrivileges(func() error {
		links, err := restoreplace.RemoveReparsePoints(target)
		out.counts.Skipped += int64(links)
		if err != nil {
			return err
		}
		check, more, err := restoreplace.ResetInherit(target, a.placeOptions())
		out.counts.Skipped += int64(more)
		if err != nil {
			return err
		}
		if check != "" {
			if err := restoreplace.CheckInherited(check, a.placeOptions()); err != nil {
				return err
			}
		}
		c, err := restoreplace.CountSide(target, g.rels)
		out.counts.Merge(c)
		return err
	})
	if err != nil {
		a.log.Error("restore side folder could not be secured; the restore failed", "restore", id, "drive", g.letter, "err", errText(err))
		a.eventLog("error", eventRestoreFailed, "a side-folder restore could not reset its folder to Administrators and SYSTEM only")
		out.fatal = api.RestoreResultOther
		return false
	}
	return true
}
