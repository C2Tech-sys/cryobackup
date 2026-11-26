package maintain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Drill mode (ADR 0029, ruling C8). A console restored from its self-backup for a drill runs with
// "drill": true in config.json (or CRYO_DRILL=true). Its worker may hold the production admin
// credential, so in drill mode it must read and never write:
//
//   - DrillLoop serves the admin-path socket and nothing else: no scheduler pass (no device
//     maintenance, no usage sample, no stale-lock unlock step, no escrow verify), no self-backup;
//   - the socket answers only the methods drillServed lists; every other method is refused with
//     adminpath.CodeUnavailable before any restic or rclone process starts (drillBackend);
//   - PurgeRepo refuses on a worker in drill mode even when called in process (purge_adr19.go);
//   - Run refuses --once in drill mode, which is also what `self-backup run` and `verify` use;
//   - every restic run carries --no-lock and no --retry-lock (Worker.common), so a read leaves no
//     lock object in the device repository (in production restic holds one while a read runs);
//   - the idle watchdog's stale-lock unlock after a stalled dump never starts (enterDrill closes the
//     worker's background work, so goBackground refuses it): with --no-lock there is no lock to
//     remove, and an unlock is a storage call that deletes.
//
// A drill copy therefore writes nothing to storage.

// drillServed lists the admin-path methods a drill worker serves: the four read methods the
// console's browse and download use, and ping, which runs no restic and reads nothing.
var drillServed = []string{
	adminpath.MethodPing,
	adminpath.MethodCatConfig,
	adminpath.MethodLs,
	adminpath.MethodSnapshots,
	adminpath.MethodDump,
}

// DrillServes reports whether a worker in drill mode serves the admin-path method.
func DrillServes(method string) bool {
	for _, m := range drillServed {
		if m == method {
			return true
		}
	}
	return false
}

// errDrillEnv is loadFileConfig's answer to a CRYO_DRILL that is not a boolean.
var errDrillEnv = errors.New("maintain: CRYO_DRILL must be true or false")

// errDrill is the in-process message of a refusal. On the wire the admin-path Handler replaces it
// with the fixed sentence of adminpath.CodeUnavailable (as for the M5 methods, methods.go).
const errDrill = "drill mode: this worker belongs to a restored console copy and serves only ls, dump, cat_config and snapshots"

// drillRefused returns the refusal of method and logs it (the method name only).
func (w *Worker) drillRefused(method string) error {
	w.log.Warn("admin-path method refused: drill mode", attrMethod(method), attrClass("drill_refused"))
	e := adminpath.NewError(adminpath.CodeUnavailable, "")
	e.Message = errDrill
	return e
}

// drillWorkers marks the workers in drill mode. It lives beside Worker rather than in it so that
// this file alone owns the mechanism (the pattern of backgroundOf in dump_multi.go); a Worker is
// one per process, so the map stays small.
var (
	drillMu      sync.Mutex
	drillWorkers = map[*Worker]bool{}
)

// drillNoLockFlag is restic's global --no-lock: Worker.common adds it in drill mode.
const drillNoLockFlag = "--no-lock"

// enterDrill puts w in drill mode for the rest of its life: there is no way back. It also closes
// w's background work (dump_multi.go), so the stale-lock unlock after a stalled dump never starts.
// dumpIdleStopped then logs its "stale lock not removed" warning (class dump_idle_unlock, worded
// for a shutdown); in drill mode there is no lock to remove, because every read ran with --no-lock.
func (w *Worker) enterDrill() {
	drillMu.Lock()
	drillWorkers[w] = true
	drillMu.Unlock()
	backgroundMu.Lock()
	w.background().closed = true
	backgroundMu.Unlock()
}

// inDrill reports whether w is in drill mode.
func (w *Worker) inDrill() bool {
	drillMu.Lock()
	defer drillMu.Unlock()
	return drillWorkers[w]
}

// DrillLoop is Loop for a worker in drill mode: it serves the admin-path socket through
// drillBackend until ctx is done, and runs nothing else. No ticker exists, so no scheduler pass and
// no self-backup step can start. It returns nil on a clean shutdown and an error when the socket
// server fails (the process then exits and is restarted, as for Loop).
func (w *Worker) DrillLoop(ctx context.Context) error {
	if w.cfg.SocketPath == "" {
		return cfgErr("SocketPath is required for DrillLoop")
	}
	w.enterDrill()
	w.log.Warn("drill mode: serving read methods only; no scheduler pass, no self-backup", attrClass("drill"))
	err := adminpath.Serve(ctx, w.cfg.SocketPath, adminpath.Handler(drillBackend{w}))
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("maintain: admin-path server: %w", err)
	}
	return nil
}

// drillBackend is the adminpath.Backend of a drill worker: the methods drillServed lists go to the
// Worker, every other one is refused without starting a process. CatConfig of the storage probe
// repository is refused too: the probe creates that repository once when it is missing (probe.go),
// which is a write, and the drill console runs no health prober that would ask for it.
type drillBackend struct{ w *Worker }

var (
	_ adminpath.Backend = drillBackend{}
	_ adminpath.Drainer = drillBackend{} // adminpath.Serve drains the Worker's background work at shutdown
)

// Ping implements adminpath.Backend (served).
func (d drillBackend) Ping(ctx context.Context) (adminpath.PingResult, error) { return d.w.Ping(ctx) }

// CatConfig implements adminpath.Backend (served, except for the storage probe repository).
func (d drillBackend) CatConfig(ctx context.Context, req adminpath.DeviceRequest) (restic.Config, error) {
	if req.DeviceID == adminpath.ProbeRepoID {
		return restic.Config{}, d.w.drillRefused(adminpath.MethodCatConfig)
	}
	return d.w.CatConfig(ctx, req)
}

// Ls implements adminpath.Backend (served).
func (d drillBackend) Ls(ctx context.Context, req adminpath.LsRequest) (adminpath.LsResult, error) {
	return d.w.Ls(ctx, req)
}

// Snapshots implements adminpath.Backend (served).
func (d drillBackend) Snapshots(ctx context.Context, req adminpath.DeviceRequest) ([]restic.Snapshot, error) {
	return d.w.Snapshots(ctx, req)
}

// Dump implements adminpath.Backend (served).
func (d drillBackend) Dump(ctx context.Context, req adminpath.DumpRequest, dst io.Writer) (adminpath.DumpResult, error) {
	return d.w.Dump(ctx, req, dst)
}

// Drain implements adminpath.Drainer.
func (d drillBackend) Drain(ctx context.Context) { d.w.Drain(ctx) }

// Init implements adminpath.Backend (refused: it creates a repository).
func (d drillBackend) Init(context.Context, adminpath.InitRequest) (adminpath.InitResult, error) {
	return adminpath.InitResult{}, d.w.drillRefused(adminpath.MethodInit)
}

// Find implements adminpath.Backend (refused: not one of the drill's read methods).
func (d drillBackend) Find(context.Context, adminpath.FindRequest) ([]restic.FindResult, error) {
	return nil, d.w.drillRefused(adminpath.MethodFind)
}

// Stats implements adminpath.Backend (refused: it feeds the scheduler's usage samples).
func (d drillBackend) Stats(context.Context, adminpath.StatsRequest) (restic.StatsResult, error) {
	return restic.StatsResult{}, d.w.drillRefused(adminpath.MethodStats)
}

// Unlock implements adminpath.Backend (refused: it deletes lock objects).
func (d drillBackend) Unlock(context.Context, adminpath.UnlockRequest) (adminpath.UnlockResult, error) {
	return adminpath.UnlockResult{}, d.w.drillRefused(adminpath.MethodUnlock)
}

// Forget implements adminpath.Backend (refused: it removes snapshots).
func (d drillBackend) Forget(context.Context, adminpath.ForgetRequest) (adminpath.ForgetResult, error) {
	return adminpath.ForgetResult{}, d.w.drillRefused(adminpath.MethodForget)
}

// Prune implements adminpath.Backend (refused: it deletes data).
func (d drillBackend) Prune(context.Context, adminpath.PruneRequest) (adminpath.PruneResult, error) {
	return adminpath.PruneResult{}, d.w.drillRefused(adminpath.MethodPrune)
}

// Check implements adminpath.Backend (refused: not one of the drill's read methods).
func (d drillBackend) Check(context.Context, adminpath.CheckRequest) (adminpath.CheckResult, error) {
	return adminpath.CheckResult{}, d.w.drillRefused(adminpath.MethodCheck)
}

// PurgeRepo implements adminpath.RepoPurger (refused: it deletes a repository prefix). The Worker's
// own PurgeRepo refuses in drill mode as well.
func (d drillBackend) PurgeRepo(context.Context, adminpath.PurgeRepoRequest) (adminpath.PurgeRepoResult, error) {
	return adminpath.PurgeRepoResult{}, d.w.drillRefused(adminpath.MethodPurgeRepo)
}
