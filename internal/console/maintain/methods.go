package maintain

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Ping implements adminpath.Backend. It runs no restic: it reports the build
// version, the uptime, whether the key unwrapper has its master key and the
// result of the last call that reached the storage target.
func (w *Worker) Ping(ctx context.Context) (adminpath.PingResult, error) {
	loaded := true
	if r, ok := w.keys.(masterKeyReporter); ok {
		loaded = r.MasterKeyLoaded()
	}
	var reach *bool
	if p := w.storageOK.Load(); p != nil {
		v := *p
		reach = &v
	}
	return adminpath.PingResult{
		Version:          w.cfg.Version,
		Uptime:           w.clock().Sub(w.started),
		MasterKeyLoaded:  loaded,
		StorageReachable: reach,
	}, nil
}

// readCall is the shared shape of the read-only methods: resolve and unlock the
// device, build the command, run it, translate a failed run into the admin-path
// error model. build may return an error for an option the builder rejects.
func (w *Worker) readCall(ctx context.Context, deviceID string, timeout time.Duration,
	build func(restic.Common) (restic.Command, error), onStdout func(io.Reader) error) (execResult, error) {
	t, err := w.prepare(ctx, deviceID)
	if err != nil {
		return execResult{}, err
	}
	defer t.drop()
	cmd, err := build(w.common())
	if err != nil {
		return execResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	res, err := w.exec(ctx, t, cmd, timeout, onStdout)
	if err != nil {
		return execResult{}, err
	}
	if err := readError(ctx, res); err != nil {
		return res, err
	}
	return res, nil
}

// initCommand builds `restic init --json`. internal/restic has no Init builder
// (restic's own live test runs it as raw argv), so this assembles the one
// command it needs and validates the shared options with a real builder first,
// so a forbidden flag in Common.Extra is rejected exactly as for every other
// command.
func initCommand(c restic.Common) (restic.Command, error) {
	if _, err := restic.CatConfig(c); err != nil {
		return restic.Command{}, err
	}
	args := append([]string{"init", "--json"}, c.Extra...)
	return restic.Command{Kind: restic.KindInit, Args: args}, nil
}

// Init implements adminpath.Backend (architecture DH17). It runs `restic init`;
// if that fails for any reason it runs `cat config`, and a cat config that
// succeeds proves the repository exists and the key verifies: the result is
// success with Created false and the existing repository id. A cat config that
// reports a wrong password makes the result a wrong_password failure. Any other
// outcome returns the init run's own failure as a result (a restic run that
// reached an exit code is a result, not an error).
func (w *Worker) Init(ctx context.Context, req adminpath.InitRequest) (adminpath.InitResult, error) {
	t, err := w.prepare(ctx, req.DeviceID)
	if err != nil {
		return adminpath.InitResult{}, err
	}
	defer t.drop()
	ctx, cancel := context.WithTimeout(ctx, w.cfg.Timeouts.Init)
	defer cancel()

	cmd, err := initCommand(w.common())
	if err != nil {
		return adminpath.InitResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	var created restic.InitResult
	ires, err := w.exec(ctx, t, cmd, w.cfg.Timeouts.Init, func(r io.Reader) error {
		var derr error
		created, derr = restic.DecodeInit(r)
		return derr
	})
	if err != nil {
		return adminpath.InitResult{}, err
	}
	if abort := abortError(ctx, ires); abort != nil {
		return adminpath.InitResult{}, abort
	}
	if ires.Exit == 0 && ires.StdoutErr == nil && created.ID != "" {
		return adminpath.InitResult{ResultMeta: ires.meta(), RepositoryID: created.ID, Created: true}, nil
	}

	// Either init failed (most often because the repository exists) or it
	// succeeded without a readable id: cat config settles which.
	cat, cres, err := w.catConfig(ctx, t)
	if err != nil {
		return adminpath.InitResult{}, err
	}
	if abort := abortError(ctx, cres); abort != nil {
		return adminpath.InitResult{}, abort
	}
	total := ires.Elapsed + cres.Elapsed
	switch {
	case cres.Exit == 0 && cres.StdoutErr == nil && cat.ID != "":
		m := cres.meta()
		m.Elapsed = total
		return adminpath.InitResult{ResultMeta: m, RepositoryID: cat.ID, Created: ires.Exit == 0}, nil
	case cres.Exit == 12:
		m := cres.meta()
		m.Elapsed = total
		return adminpath.InitResult{ResultMeta: m}, nil
	}
	m := ires.meta()
	if ires.Exit == 0 { // init worked but nothing could read the id back
		m = cres.meta()
	}
	m.Elapsed = total
	return adminpath.InitResult{ResultMeta: m}, nil
}

// abortError reports a run that never completed (timeout, cancel) as the error
// a result-style call returns; nil when the run reached an exit code.
func abortError(ctx context.Context, r execResult) error {
	switch {
	case r.Canceled:
		if err := ctx.Err(); err != nil {
			return err
		}
		return adminpath.NewError(adminpath.CodeUnavailable, "")
	case r.TimedOut:
		return adminpath.NewError(adminpath.CodeTimeout, "")
	}
	return nil
}

// catConfig runs `restic cat config` for t and decodes the document.
func (w *Worker) catConfig(ctx context.Context, t *target) (restic.Config, execResult, error) {
	cmd, err := restic.CatConfig(w.common())
	if err != nil {
		return restic.Config{}, execResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	var cfg restic.Config
	res, err := w.exec(ctx, t, cmd, w.cfg.Timeouts.CatConfig, func(r io.Reader) error {
		var derr error
		cfg, derr = restic.DecodeConfig(r)
		return derr
	})
	return cfg, res, err
}

// CatConfig implements adminpath.Backend: escrow verification and the storage
// probe (architecture §4.6 C02). exit 10 is repo_missing, 12 wrong_password.
func (w *Worker) CatConfig(ctx context.Context, req adminpath.DeviceRequest) (restic.Config, error) {
	t, err := w.prepare(ctx, req.DeviceID)
	if err != nil {
		return restic.Config{}, err
	}
	defer t.drop()
	cfg, res, err := w.catConfig(ctx, t)
	if err != nil {
		return restic.Config{}, err
	}
	if err := readError(ctx, res); err != nil {
		return restic.Config{}, err
	}
	return cfg, nil
}

// Ls implements adminpath.Backend. It always passes a directory to restic
// ("/" for the snapshot roots; without one restic 0.19.1 walks the whole
// snapshot, architecture §13.3). restic prints the directory node itself before
// its children; that node is dropped so Nodes are exactly the children of Dir.
// At most Limit nodes (DefaultLsLimit when zero) are kept and the rest of the
// stream is discarded: Truncated says so. The nodes are PATH-BEARING and are
// returned, never logged.
func (w *Worker) Ls(ctx context.Context, req adminpath.LsRequest) (adminpath.LsResult, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = adminpath.DefaultLsLimit
	}
	if limit > adminpath.MaxLsLimit {
		limit = adminpath.MaxLsLimit
	}
	dir := req.Dir
	if dir == "" {
		dir = "/"
	}
	self := strings.TrimRight(dir, "/") // "" for the roots: no node to drop
	var out adminpath.LsResult
	out.Nodes = []restic.Node{}
	res, err := w.readCall(ctx, req.DeviceID, w.cfg.Timeouts.Ls,
		func(c restic.Common) (restic.Command, error) { return restic.Ls(req.SnapshotID, dir, c) },
		func(r io.Reader) error {
			dec := restic.NewDecoder(r)
			for {
				msg, derr := dec.Next()
				if derr != nil {
					if errors.Is(derr, io.EOF) {
						return nil
					}
					return derr
				}
				switch m := msg.(type) {
				case restic.LsSnapshot:
					out.Snapshot = m
				case restic.Node:
					if self != "" && m.Path == self {
						continue
					}
					if len(out.Nodes) >= limit {
						out.Truncated = true
						continue
					}
					out.Nodes = append(out.Nodes, m)
				}
			}
		})
	if err != nil {
		return adminpath.LsResult{}, err
	}
	out.ResultMeta = okMeta(res.Elapsed)
	return out, nil
}

// Find implements adminpath.Backend. The total number of matches over all
// snapshots is capped at Limit (DefaultFindLimit when zero); results that lose
// all their matches to the cap are dropped.
func (w *Worker) Find(ctx context.Context, req adminpath.FindRequest) ([]restic.FindResult, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = adminpath.DefaultFindLimit
	}
	if limit > adminpath.MaxFindLimit {
		limit = adminpath.MaxFindLimit
	}
	var found []restic.FindResult
	_, err := w.readCall(ctx, req.DeviceID, w.cfg.Timeouts.Find,
		func(c restic.Common) (restic.Command, error) {
			return restic.Find(restic.FindOptions{
				Common: c, Pattern: req.Pattern, IgnoreCase: req.IgnoreCase, Snapshots: req.SnapshotIDs,
			})
		},
		func(r io.Reader) error {
			var derr error
			found, derr = restic.DecodeFind(r)
			return derr
		})
	if err != nil {
		return nil, err
	}
	out := make([]restic.FindResult, 0, len(found))
	left := limit
	for _, fr := range found {
		if left <= 0 {
			break
		}
		if len(fr.Matches) > left {
			fr.Matches = fr.Matches[:left]
		}
		left -= len(fr.Matches)
		out = append(out, fr)
	}
	return out, nil
}

// Snapshots implements adminpath.Backend: every snapshot of the device repository.
func (w *Worker) Snapshots(ctx context.Context, req adminpath.DeviceRequest) ([]restic.Snapshot, error) {
	var snaps []restic.Snapshot
	_, err := w.readCall(ctx, req.DeviceID, w.cfg.Timeouts.Snapshots,
		func(c restic.Common) (restic.Command, error) { return restic.Snapshots("", c) },
		func(r io.Reader) error {
			var derr error
			snaps, derr = restic.DecodeSnapshots(r)
			return derr
		})
	if err != nil {
		return nil, err
	}
	if snaps == nil {
		snaps = []restic.Snapshot{}
	}
	return snaps, nil
}

// Stats implements adminpath.Backend: `restic stats --json --mode <mode>`. Mode
// raw-data feeds usage_samples (architecture §7.3 step 8); "debug" is not
// accepted (internal/restic does not offer it).
func (w *Worker) Stats(ctx context.Context, req adminpath.StatsRequest) (restic.StatsResult, error) {
	var stats restic.StatsResult
	_, err := w.readCall(ctx, req.DeviceID, w.cfg.Timeouts.Stats,
		func(c restic.Common) (restic.Command, error) { return restic.Stats(req.Mode, "", c) },
		func(r io.Reader) error {
			var derr error
			stats, derr = restic.DecodeStats(r)
			return derr
		})
	if err != nil {
		return restic.StatsResult{}, err
	}
	return stats, nil
}

// errM5 names the milestone that delivers the operations v0 does not run. The
// admin-path Handler replaces Message with the fixed sentence of the code on the
// wire; the sentence below is what in-process callers and the log see.
const errM5 = "this operation is delivered in milestone M5 and is not available in worker v0"

func m5Unavailable() error {
	e := adminpath.NewError(adminpath.CodeUnavailable, "")
	e.Message = errM5
	return e
}

// Forget implements adminpath.Backend. v0 returns "unavailable" naming M5
// (architecture §7.3 steps 3-4).
func (w *Worker) Forget(context.Context, adminpath.ForgetRequest) (adminpath.ForgetResult, error) {
	return adminpath.ForgetResult{}, m5Unavailable()
}

// Prune implements adminpath.Backend. v0 returns "unavailable" naming M5
// (architecture §7.3 step 5).
func (w *Worker) Prune(context.Context, adminpath.PruneRequest) (adminpath.PruneResult, error) {
	return adminpath.PruneResult{}, m5Unavailable()
}

// Check implements adminpath.Backend. v0 returns "unavailable" naming M5
// (architecture §7.3 step 6).
func (w *Worker) Check(context.Context, adminpath.CheckRequest) (adminpath.CheckResult, error) {
	return adminpath.CheckResult{}, m5Unavailable()
}
