package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The storage probe of banner C02 (architecture §4.6, ADR 0017): the console's
// health-prober asks for cat config of the reserved repository
// adminpath.ProbeRepoID, and the worker answers it from the probe repository
// <bucket>/<prefix>/probe, the same location the gateway probe login requests
// as /probe/config. The probe is two steps over the admin path:
//
//  1. list:   `restic list keys --no-lock` (a LIST under the prefix, no lock file written);
//  2. config: `restic cat config` (a GET that also proves the key).
//
// When the repository does not exist and has never been created, the worker
// creates it once with `restic init` (storage-side only, nothing on disk, ADR
// 0008) and records that in the settings row SettingProbeRepo. After that a
// missing repository is a failure (repo_missing), never re-created: an emptied
// or re-pointed prefix raises C02 instead of being papered over.

// ProbeSlotWait bounds how long each step of the storage probe waits for a
// concurrency slot (Config.MaxConcurrent, shared with dumps, browsing and the
// scheduler). A worker whose slots stay taken answers adminpath.CodeBusy, which
// the console reads as "not checked" (C02b), never as a storage failure.
const ProbeSlotWait = 3 * time.Second

func (w *Worker) probeSlot() time.Duration {
	if w.probeSlotWait > 0 {
		return w.probeSlotWait
	}
	return ProbeSlotWait
}

// SettingProbeRepo is the settings key that records the probe repository's
// creation: a ProbeRepoMarker.
const SettingProbeRepo = "storage_probe_repo"

// ProbeRepoMarker is the value of SettingProbeRepo: when the worker recorded the
// probe repository, and the bucket and prefix it used then (its own
// configuration: CRYO_MAINTAIN_BUCKET and CRYO_MAINTAIN_PREFIX, else the default
// storage target). `cryo-console probe-repo reset` reads the location from here
// rather than guessing it from its own environment. A marker written before the
// location was recorded has only created_at.
type ProbeRepoMarker struct {
	CreatedAt int64  `json:"created_at"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
}

// Location is <bucket>/<prefix>/probe as recorded, and false when the marker
// names no bucket (an older marker).
func (m ProbeRepoMarker) Location() (string, bool) {
	b := strings.Trim(m.Bucket, "/")
	if b == "" {
		return "", false
	}
	if p := strings.Trim(m.Prefix, "/"); p != "" {
		b += "/" + p
	}
	return b + "/" + adminpath.ProbeRepoID, true
}

// ProbeRepoKeyer is the optional interface of a KeyUnwrapper that can supply
// the probe repository's password (derived from the master key; the console
// binary's unwrapper implements it). A worker whose unwrapper does not cannot
// probe storage and answers the probe with an internal error.
type ProbeRepoKeyer interface {
	ProbeRepoPassword() (string, error)
}

// errProbeKey: the probe repository's password is not available.
var errProbeKey = errors.New("maintain: no probe repository password")

// ProbeRecordClass is the class of the answer when the worker's own settings
// store fails during the storage probe (reading or arming the SettingProbeRepo
// guard): the code is adminpath.CodeUnavailable, so the console reads it as "not
// checked" (C02b), never as a storage failure (C02). It says nothing about
// storage: the list and cat config may well have passed.
const ProbeRecordClass = restic.ClassInternal

// probeStoreErr is the answer for a settings-store failure inside the probe. It
// is logged without any identifier (no device, bucket or path); a cancelled
// probe stays a cancellation.
func (w *Worker) probeStoreErr(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w.log.Warn("storage probe not completed: the worker settings store failed", attrClass("store"))
	return adminpath.NewError(adminpath.CodeUnavailable, ProbeRecordClass)
}

func (w *Worker) probeTarget() (*target, error) {
	k, ok := w.keys.(ProbeRepoKeyer)
	if !ok {
		return nil, errProbeKey
	}
	pw, err := k.ProbeRepoPassword()
	if err != nil || pw == "" {
		return nil, errProbeKey
	}
	repo, err := restic.AdminRepository(w.cfg.Remote, w.cfg.Bucket, w.cfg.Prefix, adminpath.ProbeRepoID)
	if err != nil {
		return nil, err
	}
	return &target{dev: store.Device{ID: adminpath.ProbeRepoID}, env: restic.Env{
		Repository: repo, Password: pw, NoCache: true, TempDir: w.tmpDir(),
	}}, nil
}

// listKeysCommand builds `restic list keys --no-lock`: list takes a
// non-exclusive lock by default, which would write a lock object every probe.
func listKeysCommand(c restic.Common) (restic.Command, error) {
	if _, err := restic.CatConfig(c); err != nil {
		return restic.Command{}, err
	}
	args := append([]string{"list", "keys", "--no-lock"}, c.Extra...)
	return restic.Command{Kind: restic.KindCatConfig, Args: args}, nil
}

// probeRepoCreated reports whether the probe repository was created before.
func (w *Worker) probeRepoCreated(ctx context.Context) (bool, error) {
	_, err := w.st.GetSetting(ctx, SettingProbeRepo)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	}
	return false, err
}

// probeStorage answers CatConfig for adminpath.ProbeRepoID: list, then cat
// config, creating the repository on the very first probe. Every restic
// outcome other than success is an *adminpath.Error (repo_missing included).
func (w *Worker) probeStorage(ctx context.Context) (restic.Config, error) {
	t, err := w.probeTarget()
	if err != nil {
		return restic.Config{}, adminpath.NewError(adminpath.CodeInternal, "")
	}
	defer t.drop()
	list, err := listKeysCommand(w.common())
	if err != nil {
		return restic.Config{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	lres, err := w.execSlot(ctx, t, list, w.cfg.Timeouts.CatConfig, w.probeSlot(), func(r io.Reader) error {
		_, cerr := io.Copy(io.Discard, io.LimitReader(r, 1<<20))
		return cerr
	})
	if err != nil {
		return restic.Config{}, err
	}
	if lerr := readError(ctx, lres); lerr != nil {
		var ae *adminpath.Error
		if !errors.As(lerr, &ae) || ae.Code != adminpath.CodeRepoMissing {
			return restic.Config{}, lerr
		}
		created, serr := w.probeRepoCreated(ctx)
		if serr != nil {
			return restic.Config{}, w.probeStoreErr(ctx) // not a storage verdict: "not checked"
		}
		if created {
			// It existed and is gone: the prefix was emptied or re-pointed. Never re-create silently.
			return restic.Config{}, lerr
		}
		if err := w.initProbeRepo(ctx, t); err != nil {
			return restic.Config{}, err
		}
	}
	cat, err := restic.CatConfig(w.common())
	if err != nil {
		return restic.Config{}, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	var cfg restic.Config
	cres, err := w.execSlot(ctx, t, cat, w.cfg.Timeouts.CatConfig, w.probeSlot(), func(r io.Reader) error {
		var derr error
		cfg, derr = restic.DecodeConfig(r)
		return derr
	})
	if err != nil {
		return restic.Config{}, err
	}
	if err := readError(ctx, cres); err != nil {
		return restic.Config{}, err
	}
	// Arm the "never re-create" guard on the first clean probe too: a repository
	// that already existed (a rebuilt database, an init cut short after its config
	// was written) is recorded now, so a later loss is repo_missing.
	if err := w.recordProbeRepo(ctx); err != nil {
		return restic.Config{}, err
	}
	return cfg, nil
}

// recordProbeRepo writes SettingProbeRepo unless it is already there. A
// settings-store failure is probeStoreErr ("not checked"), never an internal
// error the console would count as a storage failure.
func (w *Worker) recordProbeRepo(ctx context.Context) error {
	created, err := w.probeRepoCreated(ctx)
	if err != nil {
		return w.probeStoreErr(ctx)
	}
	if created {
		return nil
	}
	val, _ := json.Marshal(ProbeRepoMarker{CreatedAt: w.clock().UnixMilli(), Bucket: w.cfg.Bucket, Prefix: w.cfg.Prefix})
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if err := w.st.PutSetting(rctx, SettingProbeRepo, val, ""); err != nil {
		return w.probeStoreErr(ctx)
	}
	return nil
}

// initProbeRepo creates the probe repository once and records it.
func (w *Worker) initProbeRepo(ctx context.Context, t *target) error {
	cmd, err := initCommand(w.common())
	if err != nil {
		return adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	ires, err := w.execSlot(ctx, t, cmd, w.cfg.Timeouts.Init, w.probeSlot(), func(r io.Reader) error {
		_, cerr := io.Copy(io.Discard, io.LimitReader(r, 1<<20))
		return cerr
	})
	if err != nil {
		return err
	}
	if abort := abortError(ctx, ires); abort != nil {
		return abort
	}
	if ires.Exit != 0 {
		return readError(ctx, ires)
	}
	if err := w.recordProbeRepo(ctx); err != nil {
		return err
	}
	w.log.Info("storage probe repository created", attrDevice(adminpath.ProbeRepoID))
	return nil
}
