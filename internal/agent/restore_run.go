package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/restoreplace"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// restoreRun runs restic restore for one group into target and judges it
// (ADR 0027: nothing matched is judged per restic run; errors are classified
// on the device). The command line is ADR 0027's:
//
//	restic restore --json -vv <snapshot>:/<L> --target <T> --include-file <F>
//
// (the builder renders it as "restore --json --target T --include-file F -vv
// SNAP"; it never emits --delete). It reports true when there are files to
// place or count; false when the run matched nothing (out.fatal unset) or the
// restore must stop (out.fatal set).
func (a *Agent) restoreRun(ctx context.Context, id, snap string, g restoreGroup, include, target string, out *restoreOutcome) bool {
	cmd, err := restic.Restore(restic.RestoreOptions{
		SnapshotID: snap + ":/" + g.key, Target: target, IncludeFile: include,
		Common: restic.Common{Extra: []string{"-vv"}},
	})
	if err != nil {
		a.log.Error("restore command could not be built", "restore", id, "drive", g.letter)
		out.fatal = api.RestoreResultOther
		return false
	}
	res, stopped, fatal := a.superviseRestore(ctx, id, cmd)
	exit := res.ExitCode
	out.exit = &exit
	if fatal != "" {
		out.fatal = fatal
		if stopped {
			a.restoreUnlock(id)
		}
		return false
	}
	out.runs++
	if matchedNothing(res.ExitCode, res.RestoreSummary) {
		out.nothing++
		a.log.Warn("restore run matched nothing", "restore", id, "drive", g.letter)
		return false
	}
	resticRestoreErrors(res.Counts, &out.counts)
	s := res.RestoreSummary
	a.log.Info("restore run finished", "restore", id, "drive", g.letter, "exit", res.ExitCode,
		"restic_files_restored", s.FilesRestored, "restic_files_skipped", s.FilesSkipped, "restic_total_bytes", s.TotalBytes,
		"restic_bytes_restored", s.BytesRestored, "restic_bytes_skipped", s.BytesSkipped)
	return true
}

// restoreDryRun runs the same restore with --dry-run for the free-space check
// and returns total_bytes of its summary: the selection's size as restic will
// write it. It writes nothing and the target need not exist.
func (a *Agent) restoreDryRun(ctx context.Context, id, snap string, g restoreGroup, include, target string) (uint64, api.RestoreResultClass, *int) {
	cmd, err := restic.Restore(restic.RestoreOptions{
		SnapshotID: snap + ":/" + g.key, Target: target, IncludeFile: include, DryRun: true,
	})
	if err != nil {
		a.log.Error("restore size command could not be built", "restore", id, "drive", g.letter)
		return 0, api.RestoreResultOther, nil
	}
	res, stopped, fatal := a.superviseRestore(ctx, id, cmd)
	exit := res.ExitCode
	if fatal != "" {
		if stopped {
			a.restoreUnlock(id)
		}
		return 0, fatal, &exit
	}
	return res.RestoreSummary.TotalBytes, "", &exit
}

// superviseRestore runs one restic restore child under the restore watchdog
// and maps how it ended to a run-level class: cancelled (the operator),
// interrupted (the service stopping), repo_unreachable (the watchdog, or a
// repository that cannot be reached), auth (the gateway refused the login),
// guard_refused (the binary guard refused restic), other (anything else that
// left no summary). On "" the result has a restore summary. stopped reports
// that the agent stopped restic (watchdog or cancel).
func (a *Agent) superviseRestore(ctx context.Context, id string, cmd restic.Command) (res supervisor.Result, stopped bool, fatal api.RestoreResultClass) {
	spec, err := a.baseSpec(cmd)
	if err != nil {
		return res, false, api.RestoreResultOther
	}
	spec.RunLogPath = a.layout.RunLog(id) // restic's own stream: path-bearing, device-local only
	a.mu.Lock()
	if a.pol != nil {
		spec.MemoryLimitMiB = a.memoryCap(a.pol.Policy.Throttle)
	}
	a.mu.Unlock()

	wctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	bound := a.o.restoreIdle
	if bound <= 0 {
		bound = restoreIdleBound
	}
	moved := make(chan struct{}, 1)
	var lastFiles, lastBytes uint64
	spec.OnProgress = func(p supervisor.Progress) {
		_ = a.jr.Touch(context.WithoutCancel(ctx), id)
		if p.FilesDone != lastFiles || p.BytesDone != lastBytes {
			lastFiles, lastBytes = p.FilesDone, p.BytesDone
			select {
			case moved <- struct{}{}:
			default:
			}
		}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTimer(bound)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-moved:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(bound)
			case <-t.C:
				stop(errRestoreStalled)
				return
			}
		}
	}()
	res, runErr := a.sup.Run(wctx, spec)
	close(done)
	switch {
	case runErr != nil:
		if errors.Is(runErr, supervisor.ErrCancelledBeforeStart) {
			if ctx.Err() != nil {
				return res, false, cancelClass(ctx)
			}
			return res, false, api.RestoreResultRepoUnreachable
		}
		a.log.Error("restic could not be run for the restore", "restore", id, "err", runErr.Error())
		if errors.Is(runErr, supervisor.ErrHashMismatch) || errors.Is(runErr, supervisor.ErrNotPinned) || errors.Is(runErr, supervisor.ErrVersionMismatch) {
			a.mu.Lock()
			a.resticErr = runErr
			a.mu.Unlock()
			return res, false, api.RestoreResultGuardRefused
		}
		return res, false, api.RestoreResultOther
	case ctx.Err() != nil:
		return res, true, cancelClass(ctx)
	case errors.Is(context.Cause(wctx), errRestoreStalled):
		a.log.Error("restore stopped: restic made no progress within the bound; the repository did not answer", "restore", id,
			"bound_s", int(bound/time.Second))
		return res, true, api.RestoreResultRepoUnreachable
	case res.RestoreSummary == nil:
		class := restic.StderrClass(res.ExitError, res.Counts)
		a.log.Error("restic restore failed", "restore", id, "exit", res.ExitCode, "class", string(class))
		if res.ExitCode == 0 {
			return res, false, api.RestoreResultOther // exit 0 without a summary: an answer the agent does not understand
		}
		return res, false, fatalOfRestic(class)
	}
	return res, false, ""
}

// restoreUnlock runs "restic unlock" after a restic the agent stopped: a
// killed restic leaves its lock object in the repository
// (p22-spike.md:347-351), and the append-only gateway allows removing it. A
// failure is logged only.
func (a *Agent) restoreUnlock(id string) {
	cmd, err := restic.Unlock(false, restic.Common{})
	if err != nil {
		return
	}
	spec, err := a.baseSpec(cmd)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := a.sup.Run(ctx, spec)
	if err != nil || res.ExitCode != 0 {
		a.log.Warn("restic unlock after the stopped restore did not succeed", "restore", id, "exit", res.ExitCode)
		return
	}
	a.log.Info("restic unlock after the stopped restore", "restore", id)
}

// ---- places on the device ---------------------------------------------------

// driveRoot is "<L>:\", the root a placement walks down from.
func (a *Agent) driveRoot(letter string) string {
	if a.o.restoreDriveRoot != nil {
		return a.o.restoreDriveRoot(letter)
	}
	return letter + `:\`
}

// stagingRoot is "<L>:\.cryobackup-restore" on the destination volume.
func (a *Agent) stagingRoot(letter string) string {
	if a.o.restoreStagingRoot != nil {
		return a.o.restoreStagingRoot(letter)
	}
	return a.driveRoot(letter) + ".cryobackup-restore"
}

// sideVolume is the system drive's letter, the volume the side folder is on.
func (a *Agent) sideVolume() string {
	sd := os.Getenv("SystemDrive")
	if len(sd) >= 1 && (sd[0] >= 'A' && sd[0] <= 'Z' || sd[0] >= 'a' && sd[0] <= 'z') {
		return strings.ToUpper(sd[:1])
	}
	return "C"
}

// sideRoot is "<SystemDrive>\CryoBackup-Restore" (ruling C2).
func (a *Agent) sideRoot() string {
	if a.o.restoreSideRoot != "" {
		return a.o.restoreSideRoot
	}
	return a.sideVolume() + `:\CryoBackup-Restore`
}

// freeBytes is the free space of the volume restic writes to: the destination
// volume, or for the side folder the volume of the side root.
func (a *Agent) freeBytes(vol string, side bool) (uint64, error) {
	if a.o.restoreFreeBytes != nil {
		return a.o.restoreFreeBytes(vol)
	}
	if side {
		return restoreplace.FreeBytes(sideParent(a.sideRoot()))
	}
	return restoreplace.FreeBytes(a.driveRoot(vol))
}

// sideParent is the folder holding the side root (which may not exist yet).
func sideParent(root string) string {
	if i := strings.LastIndexByte(root, '\\'); i > 0 {
		p := root[:i]
		if strings.HasSuffix(p, ":") {
			p += `\`
		}
		return p
	}
	return root
}

func (a *Agent) placeOptions() restoreplace.Options {
	return restoreplace.Options{LabTrustee: a.o.restoreLabTrustee}
}

func (a *Agent) restoreFetchFor() time.Duration {
	if a.o.restoreFetchFor > 0 {
		return a.o.restoreFetchFor
	}
	return restoreFetchFor
}

// errText renders a placement error for the device log. restoreplace errors
// carry no path; Windows errors are a number and a fixed text. A path error
// from the os package would carry its path, so only its inner error is kept.
func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	return err.Error()
}

// sweepRestoreStaging is the start-up sweep (ADR 0027): a staging folder left
// by a restore that was cut short is removed by the clean-up walk, after the
// root rule is applied to its root; a root that fails the rule is left alone
// and logged. The run itself was closed as interrupted by OpenRunsOnStart.
// Side folders are restore results and are never swept.
func (a *Agent) sweepRestoreStaging() {
	letters := restoreplace.FixedDrives()
	if a.o.restoreSweepLetters != nil {
		letters = a.o.restoreSweepLetters()
	}
	for _, l := range letters {
		root, err := restoreplace.VerifyRoot(a.stagingRoot(l), a.placeOptions())
		if errors.Is(err, restoreplace.ErrNotExist) || errors.Is(err, restoreplace.ErrUnsupported) {
			continue
		}
		if err != nil {
			a.rootRefused("sweep", "staging", l, err)
			continue
		}
		names, err := root.Children()
		if err != nil {
			a.log.Warn("restore staging root could not be listed at start", "drive", l, "err", errText(err))
			root.Close()
			continue
		}
		for _, n := range names {
			links, err := restoreplace.RemoveTree(root.Path() + `\` + n)
			if err != nil {
				a.log.Warn("a staging folder left by an interrupted restore could not be removed", "drive", l, "err", errText(err))
				continue
			}
			a.log.Warn("removed a staging folder left by an interrupted restore", "drive", l, "links_deleted", links, "folder", safeID(n))
		}
		root.Close()
	}
}

// safeID returns n when it looks like a restore id (a ULID), else "?": a
// folder name in the staging root is logged only when it is one of the
// agent's own.
func safeID(n string) string {
	if len(n) != 26 {
		return "?"
	}
	for i := 0; i < len(n); i++ {
		if c := n[i]; !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
			return "?"
		}
	}
	return n
}
