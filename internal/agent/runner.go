package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// Run starts the loops (heartbeat, policy pull, run upload, command poll,
// scheduler) and blocks until ctx ends, then shuts down: the stop marker is
// written, a running backup is cancelled as "interrupted" and given time to
// close its journal row, and one last upload is attempted. It returns nil on a
// clean stop.
func (a *Agent) Run(ctx context.Context) error {
	runCtx, cancelRuns := context.WithCancelCause(context.Background())
	defer cancelRuns(nil)
	a.baseCtx = runCtx

	// A planned start left by a process that stopped uncleanly is not this
	// process's plan: clear it before the scheduler starts, so "status" never
	// shows a stale "next due" and recordPlan's change detection (a.planned
	// starts at "") matches the journal.
	if err := a.jr.KVDelete(context.Background(), journal.KVPlannedStart); err != nil {
		a.log.Warn("could not clear the previous process's planned start", "err", err.Error())
	}
	a.planned = ""

	a.sched = &scheduler.Loop{
		Clock: a.clock, Tick: a.o.schedTick,
		Input: a.plannerInput, Start: a.startBackup, Skip: a.skipBackup, Planned: a.recordPlan,
	}
	a.sched.Init()
	// Hand the scheduler to the handlers that Notify it.
	a.sched.Notify(scheduler.TriggerBoot)

	// Results queued by the previous process and the closed stale runs go out first.
	kick(a.kickOutbox)
	kick(a.kickPolicy)

	var loops sync.WaitGroup
	for _, f := range []func(context.Context){
		a.heartbeatLoop, a.policyLoop, a.outboxLoop, a.commandsLoop, a.sched.Run,
	} {
		loops.Add(1)
		go func(f func(context.Context)) {
			defer loops.Done()
			f(ctx)
		}(f)
	}

	<-ctx.Done()

	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	_ = a.jr.KVSet(context.Background(), journal.KVStopMarker, a.clock().UTC().Format(time.RFC3339))
	cancelRuns(supervisor.ErrServiceStop)
	a.runWG.Wait()
	loops.Wait()
	// No scheduler plans any more: "status" must not show a planned start.
	_ = a.jr.KVDelete(context.Background(), journal.KVPlannedStart)

	// One last, short attempt to hand over what the stop produced.
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = a.deliverOutbox(fctx)
	cancel()
	a.log.Info("agent stopped")
	a.eventLog("info", 1003, "CryoBackup agent stopped")
	return nil
}

// ---- scheduler glue --------------------------------------------------------

// recordPlan keeps the scheduler's planned start in the journal
// (KVPlannedStart) for "cryo-agent status", which reads the journal from
// another process and cannot know the startup delay or the jitter memo
// (L-STATUS-1: a new device looked "due now" for up to 30 minutes). A plan
// that waits sets it; a start, a running backup or no schedule at all
// deletes it. Only a change is written. It runs on the scheduler's goroutine.
func (a *Agent) recordPlan(ctx context.Context, out scheduler.Output) {
	v := ""
	if out.Action == scheduler.ActionNone && !out.NextCheck.IsZero() {
		b, err := json.Marshal(journal.PlannedStart{At: out.NextCheck.UTC(), Reason: out.Reason})
		if err != nil {
			return
		}
		v = string(b)
	}
	if v == a.planned {
		return
	}
	var err error
	if v == "" {
		err = a.jr.KVDelete(ctx, journal.KVPlannedStart)
	} else {
		err = a.jr.KVSet(ctx, journal.KVPlannedStart, v)
	}
	if err == nil {
		a.planned = v
	}
}

// pausedNow reports whether the policy response pauses the device at now.
func pausedNow(p *api.PolicyResponse, now time.Time) bool {
	if p == nil || !p.Paused {
		return false
	}
	return p.PausedUntil == nil || p.PausedUntil.After(now)
}

func (a *Agent) plannerInput(ctx context.Context, now time.Time) (scheduler.Input, bool) {
	a.mu.Lock()
	pol := a.pol
	refused := a.resticErr != nil || a.repoMissing || a.stopping || a.stateErr != nil
	force := a.forceNow && now.Sub(a.forceAt) < forceNowTTL
	if a.forceNow && !force {
		a.forceNow = false
	}
	running, bootPending := a.running != nil, a.bootPending
	a.mu.Unlock()
	if pol == nil || refused {
		return scheduler.Input{}, false
	}
	f := a.facts(ctx)
	in := scheduler.Input{
		Now: now.In(time.Local), DeviceID: a.cfg.DeviceID, Policy: pol.Policy,
		Paused: pausedNow(pol, now), Running: running,
		LastSuccess: f.LastSuccess, LastAttemptStart: f.LastAttemptStart, Retry: f.Retry,
		BootAt: a.boot, BootPending: bootPending, ForceNow: force,
		// A refused agent's runs are turned away by the gateway (ADR 0018,
		// M3-L10): an auth deferral waits for the policy's next slot, not the
		// ladder. The end of REFUSED re-plans (consoleBack) with this false.
		AuthDeferralsWait: a.isRefused(),
	}
	a.mu.Lock()
	in.LeaseNotBefore = a.leaseNotBefore
	a.mu.Unlock()
	if pol.HoldUntil != nil {
		in.HoldUntil = *pol.HoldUntil
	}
	return in, true
}

func (a *Agent) skipBackup(ctx context.Context, out scheduler.Output) {
	a.mu.Lock()
	a.forceNow = false
	a.mu.Unlock()
	a.journalSkipped(ctx, out.Trigger)
}

// journalSkipped records a refused start as a "skipped" run (§6.2): the
// console's run list shows that the request arrived and why nothing happened.
func (a *Agent) journalSkipped(ctx context.Context, trigger api.Trigger) {
	id, err := a.ids.New()
	if err != nil {
		return
	}
	now := a.clock().UTC()
	rec := api.RunRecord{
		ID: id, Kind: restic.KindBackup, Trigger: trigger, StartedAt: now, FinishedAt: &now,
		Outcome: restic.OutcomeSkipped, Attempt: 1, AgentVersion: versionLabel(),
	}
	a.mu.Lock()
	if a.pol != nil {
		rec.PolicyVersion = a.pol.Version
	}
	a.mu.Unlock()
	if err := a.jr.InsertFinished(ctx, rec); err != nil {
		a.log.Error("could not journal a skipped run", "err", err.Error())
		return
	}
	kick(a.kickOutbox)
}

// startBackup is the scheduler's Start: it marks a run active synchronously
// (so the next plan sees it) and runs the backup on its own goroutine.
func (a *Agent) startBackup(ctx context.Context, out scheduler.Output) {
	a.mu.Lock()
	if a.running != nil || a.stopping || a.baseCtx == nil {
		a.mu.Unlock()
		return
	}
	id, err := a.ids.New()
	if err != nil {
		a.mu.Unlock()
		a.log.Error("could not generate a run id", "err", err.Error())
		return
	}
	rctx, cancel := context.WithCancelCause(a.baseCtx)
	a.running = &activeRun{id: id, cancel: cancel, start: a.clock()}
	a.forceNow = false
	a.bootPending = false
	a.runWG.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.runWG.Done()
		defer cancel(nil)
		a.runBackup(rctx, id, out)
	}()
}

// ---- the run ---------------------------------------------------------------

// runBackup assembles and supervises one backup and journals its outcome
// (architecture §6.2, §6.3).
func (a *Agent) runBackup(ctx context.Context, id string, out scheduler.Output) {
	// Journal writes must survive a cancelled run context: the outcome of a
	// cancelled run is exactly what has to be recorded.
	jctx := context.WithoutCancel(ctx)
	// resticStarted is set once the run row is open and restic is next. Until
	// then this process's boot run has not happened (startBackup cleared
	// bootPending optimistically), so every return before it gives the label
	// back to the next start: a cap deferral, a 4xx refusal of the lease, a
	// stop or cancel during the lease call, a refusal before start, a journal
	// failure. It is cleared again when the supervisor reports that restic
	// never ran (cancelled during the binary check, or refused by the
	// hash, pin or version guard).
	resticStarted := false
	defer func() {
		if !resticStarted && out.Trigger == api.TriggerBoot {
			a.mu.Lock()
			a.bootPending = true
			a.mu.Unlock()
		}
		a.clearRunning(id)
		a.sched.Notify(scheduler.TriggerTick)
		kick(a.kickOutbox)
		kick(a.kickHeartbeat)
	}()

	// A start a resume's plan decided waits for the console first (up to 2
	// minutes, resume_wait.go); the run's facts are read after the wait.
	stoppedWaiting := out.Resume && !a.waitAfterResume(ctx, id)

	a.mu.Lock()
	pol := *a.pol
	a.mu.Unlock()
	f := a.facts(jctx)
	hasSnap, _ := a.jr.HasSnapshot(jctx)
	started := a.clock().UTC()
	rec := api.RunRecord{
		ID: id, Kind: restic.KindBackup, Trigger: out.Trigger, StartedAt: started,
		Attempt: f.Attempts + 1, PolicyVersion: pol.Version, AgentVersion: versionLabel(), Initial: !hasSnap,
	}
	if stoppedWaiting {
		a.journalStoppedBeforeStart(ctx, jctx, &rec)
		return
	}
	// The plan that started this run was made when the machine woke, against
	// the policy held before the sleep. A pause, a maintenance hold or a closed
	// window found after the wait wins (M1-FIX4): nothing starts and nothing
	// is journaled, as for any plan that holds back; the scheduler re-plans
	// when this returns and the heartbeat shows the state.
	if out.Resume {
		if why := resumeGate(&pol, a.clock()); why != "" {
			a.log.Info("resume: the backup does not start after the wait", "run", id, "why", why)
			return
		}
	}

	refuse := func(class restic.ErrorClass, why string) {
		a.log.Error("backup refused before start", "run", id, "class", string(class), "why", why)
		done := a.clock().UTC()
		rec.FinishedAt, rec.Outcome, rec.ErrorClass = &done, restic.OutcomeFailed, class
		if err := a.jr.InsertFinished(jctx, rec); err != nil {
			a.log.Error("could not journal a refused run", "err", err.Error())
		}
	}

	cmd, spec, err := a.assemble(ctx, pol, id, rec.Initial)
	if err != nil {
		var ae *assembleError
		class := restic.ClassInternal
		if errors.As(err, &ae) {
			class = ae.class
		}
		refuse(class, err.Error())
		return
	}
	_ = cmd

	// The fleet start lease (lease.go, ADR 0005): granted, bypassed (fail
	// open) or not required starts restic; a refusal does not.
	lease, verdict := a.acquireLease(ctx, jctx, &rec)
	if verdict != leaseStart {
		return
	}
	rec.Stats.Lease = lease

	if err := a.jr.StartRun(jctx, rec); err != nil {
		a.log.Error("run row could not be written; not starting restic", "run", id, "err", err.Error())
		return
	}
	resticStarted = true
	a.log.Info("backup starting", "run", id, "trigger", string(out.Trigger), "attempt", rec.Attempt)
	if err := a.jr.MarkRunning(jctx, id); err != nil {
		a.log.Error("journal update failed", "err", err.Error())
	}
	kick(a.kickOutbox) // the open record goes out at start

	// restic runs on its own child context so the gateway watchdog
	// (gateway_watchdog.go) can stop it with a cause of its own.
	wctx, stopRestic := context.WithCancelCause(ctx)
	defer stopRestic(nil)
	watch := a.startGatewayWatch(wctx, stopRestic, id, spec.Env.Repository, pol.Policy.Throttle.GatewayDownGrace())
	spec.OnProgress = func(p supervisor.Progress) {
		// The row keeps the last progress values, so a run the next process
		// closes as interrupted or abandoned (a reboot that killed it, a
		// crash) still reports how far it got (L-STATS-1).
		st := api.RunStats{Lease: lease}
		applyProgress(&st, p)
		_ = a.jr.TouchProgress(jctx, id, st)
		watch.progress(p, a.clock())
	}
	res, runErr := a.sup.Run(wctx, spec)
	gatewayDown := watch.end()
	finished := a.clock().UTC()
	rec.FinishedAt = &finished

	if runErr != nil {
		class := restic.ClassInternal
		switch {
		case errors.Is(runErr, supervisor.ErrCancelledBeforeStart):
			// Stopped or cancelled before the child existed: not a failure,
			// and restic did not run, so a boot run's label goes back.
			resticStarted = false
			rec.Outcome = restic.OutcomeCancelled
			if errors.Is(context.Cause(ctx), supervisor.ErrServiceStop) {
				rec.Outcome = restic.OutcomeInterrupted
			}
			class = ""
		case errors.Is(runErr, supervisor.ErrHashMismatch), errors.Is(runErr, supervisor.ErrNotPinned),
			errors.Is(runErr, supervisor.ErrVersionMismatch):
			// The binary guard refused: restic did not run either.
			resticStarted = false
			class = restic.ClassGuardRefused
			rec.Outcome = restic.OutcomeFailed
			a.mu.Lock()
			a.resticErr = runErr
			a.mu.Unlock()
		default:
			rec.Outcome = restic.OutcomeFailed
		}
		rec.ErrorClass = class
		a.log.Error("restic could not be run", "run", id, "outcome", string(rec.Outcome), "class", string(class), "err", runErr.Error())
	} else {
		fillRecord(&rec, res)
		rec.Stats.Lease = lease
	}
	if gatewayDown && rec.Outcome == restic.OutcomeCancelled {
		// The watchdog stopped restic (the supervisor reports that as the
		// agent's own cancel): the gateway is the reason, and a deferral is
		// what it is. A child that exited on its own with a real code while
		// the stop was pending keeps that code's outcome.
		rec.Outcome, rec.DeferredReason, rec.ErrorClass = restic.OutcomeDeferred, restic.DeferredGateway, restic.ClassNetwork
	}
	if err := a.jr.FinishRun(jctx, rec); err != nil {
		a.log.Error("run could not be closed in the journal", "run", id, "err", err.Error())
	}
	a.log.Info("backup finished", "run", id, "outcome", string(rec.Outcome), "class", string(rec.ErrorClass),
		"exit", exitOf(rec), "files_failed", rec.Stats.FailedFiles, "cancel_mode", res.CancelMode)
	switch rec.Outcome {
	case restic.OutcomeSuccess, restic.OutcomePartial:
		a.eventLog("info", 1010, "backup finished: "+string(rec.Outcome))
	case restic.OutcomeFailed:
		a.eventLog("error", 1011, "backup failed, class "+string(rec.ErrorClass))
	case restic.OutcomeDeferred:
		a.eventLog("warning", 1012, "backup deferred: "+string(rec.DeferredReason))
	}
	_ = emptyDir(a.layout.TempDir())

	if (rec.Outcome == restic.OutcomeSuccess || rec.Outcome == restic.OutcomePartial) && rec.SnapshotID != "" && ctx.Err() == nil {
		a.clearRunning(id)
		a.uploadSnapshots(ctx, spec)
	}
}

// clearRunning marks the run id as no longer active (and only that run: a
// newer run may already have started while the snapshot list was uploaded).
func (a *Agent) clearRunning(id string) {
	a.mu.Lock()
	if a.running != nil && a.running.id == id {
		a.running = nil
	}
	a.mu.Unlock()
}

func exitOf(r api.RunRecord) int {
	if r.ExitCode == nil {
		return -1
	}
	return *r.ExitCode
}

// fillRecord turns a supervisor result into the closed wire record (§6.2,
// §6.4, DH15): outcome and class from restic.Map, snapshot id and stats from
// the summary, error counts as the path-free projection.
//
// VSS per-volume fallback (r211, §9 "VSS fallback classification"): the drive
// letters restic named in "failed to create snapshot for [<volume>]" go to
// stats.vss_fallback_volumes, which the console intersects with the policy
// roots. restic exits 0 when the fallback is the only problem (see
// restic.VSSFallbackVolume), which restic.Map calls success; a backup that
// read a volume live is never a success, so such a run is reported partial
// with class vss, and a partial run with a fallback carries class vss too
// (the fallback, not a locked file, is what the console must see first).
// restic reads a volume only to back up a root on it, so every fallback it
// reports is on a volume that holds a configured root.
func fillRecord(rec *api.RunRecord, res supervisor.Result) {
	exit := res.ExitCode
	rec.ExitCode = &exit
	rec.Outcome = res.Restic.Outcome
	rec.DeferredReason = res.Restic.DeferredReason
	rec.ErrorClass = res.Restic.ErrorClass
	rec.ErrorCounts = res.Counts.Projection()
	if rec.ErrorClass == "" && len(res.Counts) > 0 && rec.Outcome == restic.OutcomePartial {
		rec.ErrorClass = res.Counts.Dominant()
	}
	if res.VSSFallbacks > 0 && (rec.Outcome == restic.OutcomeSuccess || rec.Outcome == restic.OutcomePartial) {
		rec.Outcome, rec.ErrorClass = restic.OutcomePartial, restic.ClassVSS
	}
	rec.Stats = api.RunStats{FailedFiles: res.FailedFiles, LongRun: res.LongRun, TotalDurationS: res.Elapsed.Seconds()}
	if len(res.VSSFallbackVolumes) > 0 {
		rec.Stats.VSSFallbackVolumes = append([]string(nil), res.VSSFallbackVolumes...)
	}
	if s := res.Summary; s != nil {
		rec.Stats.FilesNew, rec.Stats.FilesChanged, rec.Stats.FilesUnmodified = s.FilesNew, s.FilesChanged, s.FilesUnmodified
		rec.Stats.DirsNew, rec.Stats.DirsChanged, rec.Stats.DirsUnmodified = s.DirsNew, s.DirsChanged, s.DirsUnmodified
		rec.Stats.TotalFilesProcessed, rec.Stats.TotalBytesProcessed = s.TotalFilesProcessed, s.TotalBytesProcessed
		rec.Stats.DataAdded, rec.Stats.DataAddedPacked = s.DataAdded, s.DataAddedPacked
		if s.TotalDuration > 0 {
			rec.Stats.TotalDurationS = s.TotalDuration
		}
		if len(s.SnapshotID) == 64 && (rec.Outcome == restic.OutcomeSuccess || rec.Outcome == restic.OutcomePartial) {
			rec.SnapshotID = s.SnapshotID
		}
	} else {
		// No summary: restic was stopped (service stop, reboot, cancel, the
		// gateway watchdog) or died before it wrote one. The last progress
		// sample is what is known of the run, so it is reported instead of
		// zeros (L-STATS-1: a run that uploaded 3.4 GB reported 0). The
		// outcome (interrupted, cancelled, deferred, failed) already says that
		// these are progress values, not restic's totals.
		applyProgress(&rec.Stats, res.Progress)
	}
}

// applyProgress copies the counts of a progress sample into run stats: files
// and bytes done as the files and bytes processed, and restic's elapsed time
// when the stats have no duration of their own. A sample that was never
// published (zero UpdatedAt) changes nothing.
func applyProgress(st *api.RunStats, p supervisor.Progress) {
	if p.UpdatedAt.IsZero() {
		return
	}
	if p.FilesDone <= math.MaxInt64 {
		st.TotalFilesProcessed = int64(p.FilesDone)
	}
	st.TotalBytesProcessed = p.BytesDone
	if st.TotalDurationS <= 0 && p.SecondsElapsed > 0 {
		st.TotalDurationS = float64(p.SecondsElapsed)
	}
}

// assembleError carries the class of a refusal to start.
type assembleError struct {
	class restic.ErrorClass
	msg   string
}

func (e *assembleError) Error() string { return e.msg }

// assemble builds the restic command and the supervisor spec from the effective
// policy (architecture §6.3): roots and the exclude file from policy.Roots and
// policy.ExcludeFile with the discovered environment; flags per the policy;
// the repository, password, cache and temp directories through restic.Env.
//
// M0 limits (documented in doc.go): selection.includes and path_flags are not
// applied (restic backup has no include patterns; a missing root is a restic
// exit 3), conditions are absent, throttle windows are evaluated once at start
// (the child is not restarted at a boundary), and the only resource cap in the
// policy document is throttle.memory_limit_mib (memoryCap), so read
// concurrency and pack size are left to restic.
func (a *Agent) assemble(ctx context.Context, pol api.PolicyResponse, runID string, initial bool) (restic.Command, supervisor.Spec, error) {
	// The snapshot tags carry the real ids (architecture §6.3): the customer
	// from enrollment (agent.json), the policy from the applied policy
	// response. Without them the snapshot could not be attributed, so the run
	// is refused, loudly, rather than tagged with a placeholder.
	if !ulid.Valid(a.cfg.CustomerID) {
		return restic.Command{}, supervisor.Spec{}, &assembleError{restic.ClassGuardRefused, "agent.json has no customer id (enrolled before the console sent one): re-enroll the device"}
	}
	if !ulid.Valid(pol.PolicyID) {
		return restic.Command{}, supervisor.Spec{}, &assembleError{restic.ClassGuardRefused, "the applied policy carries no policy id"}
	}
	sel := pol.Policy.Selection
	env := a.environment()
	roots, err := policy.Roots(sel, env)
	if err != nil {
		return restic.Command{}, supervisor.Spec{}, &assembleError{restic.ClassGuardRefused, "the selection cannot be resolved to backup roots"}
	}
	if err := config.WriteFileAtomic(a.layout.ExcludeFile(), policy.ExcludeFile(sel, env), 0o600); err != nil {
		return restic.Command{}, supervisor.Spec{}, &assembleError{restic.ClassInternal, "exclude file not written"}
	}
	_ = config.WriteFileAtomic(a.layout.RootsFile(), []byte(strings.Join(roots, "\n")+"\n"), 0o600)

	now := a.clock()
	up, down := pol.Policy.Throttle.Effective(now.In(time.Local))
	opts := restic.BackupOptions{
		Common: restic.Common{
			RetryLock:     time.Duration(pol.Policy.Schedule.LockWaitS) * time.Second,
			LimitUpload:   int(up),
			LimitDownload: int(down),
		},
		Host:         a.cfg.DeviceID,
		Tags:         restic.StandardTags(a.cfg.CustomerID, pol.PolicyID, pol.Version, runID),
		IExcludeFile: a.layout.ExcludeFile(),
		Roots:        roots,
	}
	if runtime.GOOS == "windows" {
		opts.ExcludeCloudFiles = sel.ExcludeCloudFiles
		if v := sel.VSS; v.Enabled {
			opts.VSS = &restic.VSSOptions{
				Timeout: time.Duration(v.TimeoutS) * time.Second, ExcludeAllMountPoints: v.ExcludeAllMountPoints,
				ExcludeVolumes: v.ExcludeVolumes,
			}
		}
	}
	if sel.ExcludeLargerThan != "" {
		opts.ExcludeLargerThan = parseSize(sel.ExcludeLargerThan)
	}
	cmd, err := restic.Backup(opts)
	if err != nil {
		return restic.Command{}, supervisor.Spec{}, &assembleError{restic.ClassInternal, "backup command could not be built"}
	}

	spec, err := a.baseSpec(cmd)
	if err != nil {
		return restic.Command{}, supervisor.Spec{}, err
	}
	spec.RunLogPath = a.layout.RunLog(runID)
	hours := pol.Policy.Schedule.MaxRuntimeHours
	if initial && pol.Policy.SLA.InitialMaxRunHours > 0 {
		hours = pol.Policy.SLA.InitialMaxRunHours
	}
	if hours > 0 {
		spec.MaxRuntime = time.Duration(hours) * time.Hour
	}
	if m := pol.Policy.SLA.StallAfterMinutes; m > 0 {
		spec.StallAfter = time.Duration(m) * time.Minute
	}
	spec.MemoryLimitMiB = a.memoryCap(pol.Policy.Throttle)
	return cmd, spec, nil
}

// memoryCap resolves throttle.memory_limit_mib for this machine (r209,
// policy.Throttle.MemoryCapMiB): the supervisor applies it through the job
// object, and a child that hits it is failed/memory. Auto on a Windows machine
// whose physical memory cannot be read runs without a cap and says so.
func (a *Agent) memoryCap(t policy.Throttle) int {
	phys, ok := supervisor.PhysicalMemoryMiB()
	if !ok && t.MemoryLimitMiB == policy.MemoryLimitAuto && runtime.GOOS == "windows" {
		a.log.Warn("physical memory could not be read; restic runs without a memory cap")
	}
	return t.MemoryCapMiB(phys)
}

// baseSpec is the part of a supervisor spec every restic call shares: binary,
// pin, environment (the only place the secrets go), directories.
func (a *Agent) baseSpec(cmd restic.Command) (supervisor.Spec, error) {
	a.secMu.Lock()
	gw, repoPW := a.gatewayPW, a.repoPW
	a.secMu.Unlock()
	var repo string
	if a.o.insecureLocalRepo != nil {
		repo = a.o.insecureLocalRepo(a.cfg.DeviceID)
	} else {
		var err error
		repo, err = restic.GatewayRepository(a.cfg.GatewayURL, a.cfg.DeviceID, gw, a.cfg.InsecureLab)
		if err != nil {
			return supervisor.Spec{}, &assembleError{restic.ClassInternal, "the gateway repository could not be built"}
		}
	}
	return supervisor.Spec{
		Binary: a.resticPath(), ExpectedSHA256: a.expectedSHA(), AllowUnpinned: a.o.AllowUnpinnedRestic,
		Command: cmd,
		Env: restic.Env{
			Repository: repo, Password: repoPW, CacheDir: a.layout.CacheDir(), TempDir: a.layout.TempDir(),
		},
		BaseEnv: a.o.baseEnv, NoWindow: a.o.ServiceMode, Clock: a.clock,
		CancelGrace: a.o.cancelGrace, ProgressInterval: a.o.progressEvery,
	}, nil
}

// maxSnapshotsUploaded keeps the snapshot cache upload well inside the body cap.
const maxSnapshotsUploaded = 2000

// uploadSnapshots refreshes the console's snapshot-list cache after a
// successful backup (§6.3 "snapshots", §5.1): "restic snapshots --json --host
// <device>", the account name blanked, roots only in Paths. A failure is
// logged by class and does not affect the run.
func (a *Agent) uploadSnapshots(ctx context.Context, backupSpec supervisor.Spec) {
	cmd, err := restic.Snapshots(a.cfg.DeviceID, restic.Common{RetryLock: 2 * time.Minute})
	if err != nil {
		return
	}
	spec := backupSpec
	spec.Command = cmd
	spec.RunLogPath = ""
	spec.MaxRuntime = 10 * time.Minute
	spec.StallAfter, spec.OnProgress = 0, nil
	res, err := a.sup.Run(ctx, spec)
	if err != nil || res.ExitCode != 0 {
		a.log.Warn("snapshot list could not be read", "class", string(res.Restic.ErrorClass))
		return
	}
	snaps, err := restic.DecodeSnapshots(bytes.NewReader(res.Stdout))
	if err != nil {
		a.log.Warn("snapshot list could not be decoded")
		return
	}
	// Newest first, bounded; the account name never leaves the device.
	for i, j := 0, len(snaps)-1; i < j; i, j = i+1, j-1 {
		snaps[i], snaps[j] = snaps[j], snaps[i]
	}
	if len(snaps) > maxSnapshotsUploaded {
		snaps = snaps[:maxSnapshotsUploaded]
	}
	for i := range snaps {
		snaps[i].Username = ""
		snaps[i].Summary = nil
	}
	req := api.SnapshotsRequest{Snapshots: snaps}
	if err := req.Validate(); err != nil {
		a.log.Warn("snapshot list does not validate; not sent", "err", err.Error())
		return
	}
	if _, err := a.up.UploadSnapshots(ctx, req); err != nil {
		a.uplinkFailed(err)
		a.log.Warn("snapshot list upload failed", "err", err.Error())
		return
	}
	if a.consoleWasDown() {
		a.consoleBack(callSnapshots) // an answer after a failed call: wake the uplink loops (ADR 0018)
	}
}

// parseSize reads restic's size syntax (bytes, or k, m, g, t suffix, 1024-based).
func parseSize(s string) int64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g':
		mult, s = 1<<30, s[:len(s)-1]
	case 't':
		mult, s = 1<<40, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n * mult
}
