package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The console's self-backup (ADR 0029, ticket P22-C). The worker holds what a
// backup of the console needs: the admin storage credential, the master key and
// the database. Every SelfBackupEvery it copies the database with VACUUM INTO
// into a 0700 staging folder under the state directory, puts config.json and a
// manifest beside it, backs the folder up with restic (host "console") to the
// repository rclone:<remote>:<bucket>/<self prefix> through the admin remote,
// applies the retention of ruling C4 with forget and prune, and deletes the
// staging folder. Every SelfBackupVerifyEvery (and right after the first backup)
// it restores the newest snapshot into a 0700 verify folder, checks the copy
// against its manifest (sha256, integrity_check, schema version, row counts,
// master key id), unwraps one escrowed key from it with the loaded master key,
// runs `restic check --read-data` on the repository and deletes the folder.
//
// The repository password is HKDF(master key, SelfBackupKeyInfo), never stored
// (as for the probe repository, ADR 0017); `restic init` runs once per location,
// guarded by store.SettingSelfBackupRepo. Not in the backup, ever: the master
// key, both rclone configurations, TLS keys, the mail password file and env
// files (the operator's offline kit, ADR 0029); only the three files below are
// written to the staging folder.
//
// What it writes, and only while it holds the db-self-backup lease
// (SelfBackupJob; one run at a time, here and across processes):
//
//   - job_state SelfBackupJob: ok, or error with a path-free class; error
//     "not_configured" every SelfBackupEvery while CRYO_MAINTAIN_SELF_PREFIX is
//     unset.
//   - job_state SelfBackupVerifyJob: ok, or error with a class.
//   - store.SettingSelfBackupLast: the newest snapshot written (C11's LastAt).
//   - store.SettingSelfBackupVerify: the newest verdict (C11's VerifyFailed)
//     and the newest attempt. An attempt that could not reach a verdict (the
//     storage was unreachable, the disk too small, a timeout) leaves the
//     verdict as it was. store.Store.SelfBackupFacts reads both rows.

// Job names, host and schedule (ruling C4).
const (
	SelfBackupJob         = "db-self-backup"
	SelfBackupVerifyJob   = "db-self-backup-verify"
	SelfBackupHost        = "console"
	SelfBackupEvery       = 6 * time.Hour
	SelfBackupVerifyEvery = 7 * 24 * time.Hour
	// SelfBackupDiskMargin is the free disk a run needs beyond twice the
	// database (the copy, and room for the database to grow meanwhile).
	SelfBackupDiskMargin = 256 << 20
)

// Retention (ruling C4): every snapshot of the last 7 days, one a day for 30
// days, one a month for 12 months.
const (
	selfKeepWithin  = "7d"
	selfKeepDaily   = 30
	selfKeepMonthly = 12
)

// Files and folders. The two folders live directly under Config.StateDir.
const (
	SelfBackupStagingDir     = "self-backup-staging"
	SelfBackupVerifyDir      = "self-backup-verify"
	SelfBackupDBFile         = "console.db"
	SelfBackupConfigFile     = "config.json"
	SelfBackupManifestFile   = "manifest.json"
	SelfBackupManifestFormat = "cryobackup-self-backup/v1"
)

// Tunables.
const (
	selfBackupRetry     = time.Hour     // a failed run is tried again after this
	selfVerifyRetry     = 6 * time.Hour // a failed or inconclusive verify is tried again after this
	selfLeaseTTL        = 30 * time.Minute
	selfLeaseRenewEvery = 10 * time.Minute
	selfSlotWait        = 5 * time.Minute // wait for a restic slot before "busy"
	selfBackupTimeout   = 3 * time.Hour
	selfForgetTimeout   = 30 * time.Minute
	selfPruneTimeout    = 3 * time.Hour
	selfRestoreTimeout  = 3 * time.Hour
	selfCheckTimeout    = 12 * time.Hour
	selfRepoLabel       = "self-backup" // the repository's id in log lines
)

// Path-free classes of the self-backup (job_state.last_error_class and the
// verify row). restic's own classes (network, storage, tls, lock, repo_missing,
// wrong_password, ...) are used as they come.
const (
	SelfClassNotConfigured = "not_configured" // CRYO_MAINTAIN_SELF_PREFIX unset
	SelfClassNoKey         = "no_key"         // the master key (or the derived password) is not available
	SelfClassBusy          = "busy"           // no restic slot came free in selfSlotWait
	SelfClassDisk          = string(restic.ClassDisk)
	SelfClassStaging       = "staging"     // the copy, config.json or the manifest could not be written
	SelfClassPartial       = "partial"     // restic backup could not read every staged file (exit 3)
	SelfClassTimeout       = "timeout"     // a restic run hit its deadline
	SelfClassInterrupted   = "interrupted" // the worker stopped mid-run
	SelfClassNoSnapshot    = "no_snapshot" // a backup was recorded but the repository has none
	SelfClassRestore       = "restore"     // the newest snapshot could not be restored
	SelfClassManifest      = "manifest"    // the restored copy does not match its manifest
	SelfClassIntegrity     = "integrity"   // the copy fails PRAGMA integrity_check (or is not a database)
	SelfClassMasterKey     = "master_key"  // the copy was made under another master key
	SelfClassEscrow        = "escrow"      // an escrowed key of the copy does not unwrap
	SelfClassCheck         = "check"       // check --read-data found damage
)

// SelfBackupManifest is manifest.json beside the database copy.
type SelfBackupManifest struct {
	Format        string           `json:"format"`
	CreatedAt     time.Time        `json:"created_at"` // UTC
	SchemaVersion int              `json:"schema_version"`
	Build         string           `json:"build"`
	Database      SelfBackupFile   `json:"database"`
	Tables        map[string]int64 `json:"tables"`
	MasterKeyID   string           `json:"master_key_id"`
	Bucket        string           `json:"bucket"`
	Prefix        string           `json:"prefix"`        // the self-backup prefix
	DevicePrefix  string           `json:"device_prefix"` // where the device repositories live
	// ConfigIncluded says whether config.json is in the snapshot.
	ConfigIncluded bool `json:"config_included"`
}

// SelfBackupFile names a file of the snapshot with its size and sha256.
type SelfBackupFile struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// SelfBackupReport is what one SelfBackupStep did.
type SelfBackupReport struct {
	// Skipped: another step runs in this process, or another process holds the
	// lease. Nothing was written.
	Skipped bool
	// NothingDue: neither the backup nor the verification was due.
	NothingDue bool
	// NotConfigured: CRYO_MAINTAIN_SELF_PREFIX is unset; the job row says so.
	NotConfigured bool
	// LeaseLost: the lease was taken over mid-step; the step stopped and wrote
	// nothing more.
	LeaseLost bool

	BackupRan   bool
	BackupOK    bool
	BackupClass string
	SnapshotID  string

	VerifyRan bool
	// VerifyVerdict: the verification reached a result about the backup
	// (VerifyOK says which); false: inconclusive (VerifyClass says why).
	VerifyVerdict bool
	VerifyOK      bool
	VerifyClass   string
	EscrowChecked bool
}

// SelfBackupStep is one self-backup step of the scheduler (Loop runs it every
// tick beside the device pass; `maintain --once` runs it once). It does what is
// due, under the db-self-backup lease; see the top of this file. The error is
// non-nil only when the store could not be read or the lease not asked for.
func (w *Worker) SelfBackupStep(ctx context.Context) (SelfBackupReport, error) {
	var rep SelfBackupReport
	if !w.selfRunning.CompareAndSwap(false, true) {
		rep.Skipped = true
		return rep, nil
	}
	defer w.selfRunning.Store(false)

	now := w.clock()
	backupDue, err := w.selfJobDue(ctx, SelfBackupJob, now)
	if err != nil {
		return rep, w.storeErr(ctx, err)
	}
	verifyDue := false
	if w.cfg.SelfPrefix != "" {
		if verifyDue, err = w.selfJobDue(ctx, SelfBackupVerifyJob, now); err != nil {
			return rep, w.storeErr(ctx, err)
		}
	}
	if !backupDue && !verifyDue {
		rep.NothingDue = true
		return rep, nil
	}

	got, err := w.st.AcquireJobLease(ctx, SelfBackupJob, w.holder, selfLeaseTTL)
	if err != nil {
		return rep, w.storeErr(ctx, err)
	}
	if !got {
		rep.Skipped = true
		w.log.Info("self backup lease held by another process; step skipped, job state untouched", attrOutcome("skipped"))
		return rep, nil
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		_ = w.st.ReleaseJobLease(rctx, SelfBackupJob, w.holder)
	}()
	ctx, stop := context.WithCancel(ctx)
	var lost atomic.Bool
	renewed := make(chan struct{})
	every := w.selfLeaseRenew
	if every <= 0 {
		every = selfLeaseRenewEvery
	}
	go func() {
		defer close(renewed)
		w.renewJobLease(ctx, SelfBackupJob, selfLeaseTTL, every, stop, &lost)
	}()
	defer func() { stop(); <-renewed }()
	held := func() bool {
		if lost.Load() {
			if !rep.LeaseLost {
				rep.LeaseLost = true
				w.log.Warn("self backup lease lost during the step; step stopped, job state untouched", attrOutcome("skipped"))
			}
			return false
		}
		return true
	}

	if w.cfg.SelfPrefix == "" {
		rep.NotConfigured = true
		// Free text must be plain words (logx); the variable goes in a typed field.
		w.log.Warn("self backup is not configured, so no copy of the console database is written: set the variable named in the setting field to a prefix that does not begin with the device prefix, ADR 0029",
			attrClass(SelfClassNotConfigured), logx.Str("setting", "CRYO_MAINTAIN_SELF_PREFIX"))
		w.recordSelfJob(ctx, SelfBackupJob, "error", SelfClassNotConfigured, now, SelfBackupEvery)
		return rep, nil
	}

	// Leftovers of a run that died (a kill, a power cut) go first.
	w.sweepSelfDir(w.selfStagingPath())
	w.sweepSelfDir(w.selfVerifyPath())

	state, err := w.st.SelfBackupState(ctx)
	if err != nil {
		if held() {
			w.recordSelfJob(ctx, SelfBackupJob, "error", string(restic.ClassInternal), now, selfBackupRetry)
		}
		return rep, w.storeErr(ctx, err)
	}

	if backupDue {
		start := w.clock()
		rep.BackupRan = true
		snap, class := w.selfBackupRun(ctx, state.Repo, held)
		if !held() {
			return rep, nil
		}
		rep.SnapshotID = snap
		rep.BackupClass = class
		rep.BackupOK = class == ""
		if rep.BackupOK {
			w.recordSelfJob(ctx, SelfBackupJob, "ok", "", start, SelfBackupEvery)
			w.log.Info("self backup written", attrKind(SelfBackupJob), attrElapsed(w.clock().Sub(start)))
		} else {
			w.recordSelfJob(ctx, SelfBackupJob, "error", class, start, selfBackupRetry)
			w.log.Warn("self backup failed", attrKind(SelfBackupJob), attrClass(class), w.attrStorageHostIf(class))
		}
		if ctx.Err() != nil {
			return rep, nil // stopped: the job row says interrupted; nothing more this step
		}
		if state, err = w.st.SelfBackupState(ctx); err != nil {
			return rep, w.storeErr(ctx, err)
		}
	}

	if verifyDue && state.Last != nil && ctx.Err() == nil {
		start := w.clock()
		rep.VerifyRan = true
		v := w.selfVerifyRun(ctx, held)
		if !held() {
			return rep, nil
		}
		rep.VerifyVerdict, rep.VerifyOK, rep.VerifyClass, rep.EscrowChecked = v.verdict, v.ok, v.class, v.escrow
		w.recordSelfVerify(ctx, state.Verify, v, start)
	}
	return rep, nil
}

// selfJobDue reports whether job name is due: no row, no next_due_at, or
// next_due_at reached.
func (w *Worker) selfJobDue(ctx context.Context, name string, now time.Time) (bool, error) {
	js, err := w.st.GetJobState(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	return js.NextDueAt == nil || !now.Before(*js.NextDueAt), nil
}

// recordSelfJob writes job_state name for a run that started at start; the
// next one is due after next.
func (w *Worker) recordSelfJob(ctx context.Context, name, status, class string, start time.Time, next time.Duration) {
	end := w.clock()
	if end.Before(start) {
		end = start
	}
	due := end.Add(next)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if err := w.st.RecordJobRun(rctx, name, store.JobRun{Status: status, ErrorClass: class, StartedAt: start, FinishedAt: end, NextDueAt: &due}); err != nil {
		w.log.Error("job state not recorded", attrKind(name))
	}
}

// attrStorageHostIf adds the storage host to a failure line whose class says
// the storage could not be reached.
func (w *Worker) attrStorageHostIf(class string) logx.Attr {
	if storageClass(restic.ErrorClass(class)) {
		return w.attrStorageHost()
	}
	return attrOutcome("failed")
}

func (w *Worker) selfStagingPath() string { return filepath.Join(w.cfg.StateDir, SelfBackupStagingDir) }
func (w *Worker) selfVerifyPath() string  { return filepath.Join(w.cfg.StateDir, SelfBackupVerifyDir) }

// sweepSelfDir removes a staging or verify folder. A symlink in its place is
// removed as a link, never followed. A folder that cannot be removed is logged
// (the next run then fails to create it, loudly).
func (w *Worker) sweepSelfDir(dir string) {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return
	case err != nil:
	case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
		err = os.Remove(dir)
	default:
		err = os.RemoveAll(dir)
	}
	if err != nil {
		w.log.Error("self backup folder could not be removed", attrClass("sweep"))
	}
}

// makeSelfDir creates a fresh 0700 folder (after a sweep).
func makeSelfDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// selfTarget is the self-backup repository with the derived password.
func (w *Worker) selfTarget() (*target, string) {
	if w.selfKeys == nil {
		return nil, SelfClassNoKey
	}
	pw, err := w.selfKeys.SelfBackupPassword()
	if err != nil || pw == "" {
		return nil, SelfClassNoKey
	}
	parent, last := path.Split(w.cfg.SelfPrefix)
	repo, err := restic.AdminRepository(w.cfg.Remote, w.cfg.Bucket, strings.Trim(parent, "/"), last)
	if err != nil {
		return nil, string(restic.ClassInternal)
	}
	return &target{dev: store.Device{ID: selfRepoLabel}, env: restic.Env{
		Repository: repo, Password: pw, NoCache: true, TempDir: w.tmpDir(),
	}}, ""
}

// selfExec runs one restic command on the self-backup repository and returns
// the run and its path-free class ("" for success). onStdout and ev as execEv.
func (w *Worker) selfExec(ctx context.Context, t *target, cmd restic.Command, timeout time.Duration, onStdout func(io.Reader) error, ev *restic.Evidence) (execResult, string) {
	if onStdout == nil {
		onStdout = func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }
	}
	res, err := w.execEv(ctx, t, cmd, timeout, selfSlotWait, onStdout, ev)
	if err != nil {
		var ae *adminpath.Error
		switch {
		case ctx.Err() != nil:
			return res, SelfClassInterrupted
		case errors.As(err, &ae) && ae.Code == adminpath.CodeBusy:
			return res, SelfClassBusy
		}
		return res, string(restic.ClassInternal)
	}
	return res, selfClass(res)
}

// selfClass is the class of a finished run, "" when it succeeded.
func selfClass(r execResult) string {
	switch {
	case r.Canceled:
		return SelfClassInterrupted
	case r.TimedOut:
		return SelfClassTimeout
	}
	switch r.Result.Outcome {
	case restic.OutcomeSuccess:
		if r.StdoutErr != nil {
			return string(restic.ClassParse)
		}
		return ""
	case restic.OutcomePartial:
		return SelfClassPartial
	}
	if c := r.errorClass(); c != "" {
		return string(c)
	}
	return string(restic.ClassUnknown)
}

// ensureSelfRepo makes sure the repository exists at the configured location.
// When store.SettingSelfBackupRepo already names this location nothing runs:
// a repository that is gone shows up as repo_missing on the backup, never
// re-created. Otherwise cat config decides: present (a rebuilt database, an
// init cut short) is recorded; missing is created with `restic init` and
// recorded; anything else is the run's failure.
func (w *Worker) ensureSelfRepo(ctx context.Context, t *target, marker *store.SelfBackupRepoMarker, held func() bool) string {
	if marker != nil && marker.Bucket == w.cfg.Bucket && marker.Prefix == w.cfg.SelfPrefix {
		return ""
	}
	cat, err := restic.CatConfig(w.common())
	if err != nil {
		return string(restic.ClassInternal)
	}
	res, class := w.selfExec(ctx, t, cat, w.cfg.Timeouts.CatConfig, nil, nil)
	switch {
	case class == "":
	case res.Exit == 10:
		icmd, err := initCommand(w.common())
		if err != nil {
			return string(restic.ClassInternal)
		}
		if _, class := w.selfExec(ctx, t, icmd, w.cfg.Timeouts.Init, nil, nil); class != "" {
			return class
		}
		w.log.Info("self backup repository created", attrDevice(selfRepoLabel))
	default:
		return class
	}
	if !held() {
		return SelfClassInterrupted
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if err := w.st.PutSelfBackupRepo(rctx, store.SelfBackupRepoMarker{
		CreatedAt: w.clock().UnixMilli(), Bucket: w.cfg.Bucket, Prefix: w.cfg.SelfPrefix,
	}); err != nil {
		return string(restic.ClassInternal)
	}
	return ""
}

// selfBackupRun is one backup: repository, staging, restic backup, the
// SettingSelfBackupLast row, retention, and the staging folder removed on
// every way out. It returns the snapshot id (when one was written) and the
// class ("" when everything succeeded).
func (w *Worker) selfBackupRun(ctx context.Context, marker *store.SelfBackupRepoMarker, held func() bool) (string, string) {
	t, class := w.selfTarget()
	if class != "" {
		return "", class
	}
	defer t.drop()
	if class := w.ensureSelfRepo(ctx, t, marker, held); class != "" {
		return "", class
	}
	staging := w.selfStagingPath()
	defer w.sweepSelfDir(staging)
	m, class := w.stageSelfBackup(ctx, staging)
	if class != "" {
		return "", class
	}
	if w.selfBeforeBackup != nil {
		w.selfBeforeBackup(staging)
	}

	bcmd, err := restic.Backup(restic.BackupOptions{Common: w.common(), Host: SelfBackupHost, Roots: []string{staging}})
	if err != nil {
		return "", string(restic.ClassInternal)
	}
	var ev restic.Evidence
	var snap string
	_, class = w.selfExec(ctx, t, bcmd, selfBackupTimeout, func(r io.Reader) error {
		dec := restic.NewDecoder(r).ForKind(restic.KindBackup)
		for {
			msg, err := dec.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if s, ok := msg.(restic.BackupSummary); ok {
				ev.SummarySeen = true
				snap = s.SnapshotID
			}
		}
	}, &ev)
	if class == "" && snap == "" {
		class = string(restic.ClassParse)
	}
	if class != "" {
		w.selfUnlockAfter(ctx, t, class)
		return "", class
	}
	if !held() {
		return snap, SelfClassInterrupted
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	err = w.st.RecordSelfBackup(rctx, store.SelfBackupLast{
		At: w.clock().UnixMilli(), SnapshotID: snap, DBBytes: m.Database.Bytes, DBSHA256: m.Database.SHA256, SchemaVersion: m.SchemaVersion,
	})
	cancel()
	if err != nil {
		return snap, string(restic.ClassInternal)
	}
	return snap, w.selfRetention(ctx, t)
}

// selfRetention is `restic forget` with the C4 rules, grouped by host only (a
// moved state directory must not start a second history), then `restic prune`.
func (w *Worker) selfRetention(ctx context.Context, t *target) string {
	fcmd, err := restic.Forget(restic.ForgetOptions{
		Common: w.common(), Host: SelfBackupHost, GroupBy: "host",
		KeepWithin: selfKeepWithin, KeepDaily: selfKeepDaily, KeepMonthly: selfKeepMonthly,
	})
	if err != nil {
		return string(restic.ClassInternal)
	}
	if _, class := w.selfExec(ctx, t, fcmd, selfForgetTimeout, nil, nil); class != "" {
		w.selfUnlockAfter(ctx, t, class)
		return class
	}
	pcmd, err := restic.Prune(restic.PruneOptions{Common: w.common()})
	if err != nil {
		return string(restic.ClassInternal)
	}
	if _, class := w.selfExec(ctx, t, pcmd, selfPruneTimeout, nil, nil); class != "" {
		w.selfUnlockAfter(ctx, t, class)
		return class
	}
	return ""
}

// selfUnlockAfter runs `restic unlock` (stale locks only) after a run that
// failed on a lock or was cut off: the worker is this repository's only
// writer, so a lock left behind is a dead run's, and the next attempt must not
// meet it.
func (w *Worker) selfUnlockAfter(ctx context.Context, t *target, class string) {
	switch class {
	case string(restic.ClassLock), SelfClassTimeout, SelfClassInterrupted:
	default:
		return
	}
	cmd, err := restic.Unlock(false, w.common())
	if err != nil {
		return
	}
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.Timeouts.Unlock+selfSlotWait)
	defer cancel()
	_, _ = w.selfExec(uctx, t, cmd, w.cfg.Timeouts.Unlock, nil, nil)
}

// stageSelfBackup fills the staging folder: the free-disk check, VACUUM INTO,
// config.json and manifest.json. Nothing else is ever written there.
func (w *Worker) stageSelfBackup(ctx context.Context, staging string) (SelfBackupManifest, string) {
	var m SelfBackupManifest
	dbPath := w.st.Path()
	dbBytes := fileSize(dbPath) + fileSize(dbPath+"-wal")
	need := uint64(2*dbBytes) + SelfBackupDiskMargin
	switch free, known, err := w.freeDisk(w.cfg.StateDir); {
	case err != nil:
		w.log.Warn("self backup: the free disk of the state directory could not be read", attrClass(SelfClassDisk))
		return m, SelfClassDisk
	case !known:
		w.log.Info("self backup: this platform cannot report free disk; the check is skipped", attrClass(SelfClassDisk))
	case free < need:
		w.log.Warn("self backup: not enough free disk in the state directory for a copy of the database",
			attrClass(SelfClassDisk), logx.Int("need_mib", int(need>>20)), logx.Int("free_mib", int(free>>20)))
		return m, SelfClassDisk
	}
	if err := makeSelfDir(staging); err != nil {
		return m, SelfClassStaging
	}
	copyPath := filepath.Join(staging, SelfBackupDBFile)
	if err := w.st.VacuumInto(ctx, copyPath); err != nil {
		if ctx.Err() != nil {
			return m, SelfClassInterrupted
		}
		return m, SelfClassStaging
	}
	facts, err := store.InspectDatabaseFile(ctx, copyPath)
	switch {
	case ctx.Err() != nil:
		return m, SelfClassInterrupted
	case err != nil || facts.Integrity != "ok":
		// The live database itself is damaged: never back that up as good.
		w.log.Error("self backup: the copy of the database fails its integrity check", attrClass(SelfClassIntegrity))
		return m, SelfClassIntegrity
	}
	size, sum, err := hashFile(copyPath)
	if err != nil {
		return m, SelfClassStaging
	}
	included := false
	if w.cfg.ConfigFile != "" {
		switch err := copyFile(w.cfg.ConfigFile, filepath.Join(staging, SelfBackupConfigFile)); {
		case err == nil:
			included = true
		case errors.Is(err, os.ErrNotExist):
		default:
			return m, SelfClassStaging
		}
	}
	keyID := ""
	if w.selfKeys != nil {
		keyID = w.selfKeys.MasterKeyID()
	}
	m = SelfBackupManifest{
		Format:         SelfBackupManifestFormat,
		CreatedAt:      w.clock().UTC(),
		SchemaVersion:  facts.SchemaVersion,
		Build:          w.cfg.Version,
		Database:       SelfBackupFile{Name: SelfBackupDBFile, Bytes: size, SHA256: sum},
		Tables:         facts.Tables,
		MasterKeyID:    keyID,
		Bucket:         w.cfg.Bucket,
		Prefix:         w.cfg.SelfPrefix,
		DevicePrefix:   strings.Trim(w.cfg.Prefix, "/"),
		ConfigIncluded: included,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, SelfClassStaging
	}
	if err := os.WriteFile(filepath.Join(staging, SelfBackupManifestFile), append(b, '\n'), 0o600); err != nil {
		return m, SelfClassStaging
	}
	return m, ""
}

// fileSize is the size of a regular file, 0 when it is absent.
func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	return fi.Size()
}

// copyFile copies src to a new file dst, mode 0600.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// selfVerdict is the outcome of one verification.
type selfVerdict struct {
	verdict bool // false: inconclusive, class says why
	ok      bool
	class   string
	snap    string
	escrow  bool
}

func failed(class, snap string) selfVerdict {
	return selfVerdict{verdict: true, class: class, snap: snap}
}
func inconclusive(class string) selfVerdict { return selfVerdict{class: class} }

// inconclusiveClass reports whether a restic failure says nothing about the
// backup itself: the path to the storage, the worker, a lock or the clock.
func inconclusiveClass(class string) bool {
	switch class {
	case string(restic.ClassNetwork), string(restic.ClassTLS), string(restic.ClassStorage), string(restic.ClassAuth),
		string(restic.ClassLock), string(restic.ClassDisk), string(restic.ClassMemory), string(restic.ClassPermission),
		SelfClassTimeout, SelfClassInterrupted, SelfClassBusy, SelfClassNoKey, string(restic.ClassInternal):
		return true
	}
	return false
}

// selfVerifyRun is the weekly tested restore (see the top of this file).
func (w *Worker) selfVerifyRun(ctx context.Context, held func() bool) selfVerdict {
	t, class := w.selfTarget()
	if class != "" {
		return inconclusive(class)
	}
	defer t.drop()
	dir := w.selfVerifyPath()
	w.sweepSelfDir(dir)
	defer w.sweepSelfDir(dir)

	// The newest snapshot of host console.
	scmd, err := restic.Snapshots(SelfBackupHost, w.common())
	if err != nil {
		return inconclusive(string(restic.ClassInternal))
	}
	var snaps []restic.Snapshot
	_, class = w.selfExec(ctx, t, scmd, w.cfg.Timeouts.Snapshots, func(r io.Reader) error {
		var derr error
		snaps, derr = restic.DecodeSnapshots(r)
		return derr
	}, nil)
	switch {
	case class == "":
	case inconclusiveClass(class):
		return inconclusive(class)
	default: // repo_missing, wrong_password, a listing restic cannot read
		return failed(class, "")
	}
	if len(snaps) == 0 {
		return failed(SelfClassNoSnapshot, "")
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
	newest := snaps[0]
	if len(newest.Paths) != 1 || newest.ID == "" {
		return failed(SelfClassManifest, newest.ID)
	}
	snap := newest.ID

	if free, known, err := w.freeDisk(w.cfg.StateDir); err != nil {
		return inconclusive(SelfClassDisk)
	} else if need := uint64(2*fileSize(w.st.Path())) + SelfBackupDiskMargin; known && free < need {
		w.log.Warn("self backup verify: not enough free disk in the state directory for the restored copy", attrClass(SelfClassDisk))
		return inconclusive(SelfClassDisk)
	}
	if err := makeSelfDir(dir); err != nil {
		return inconclusive(SelfClassStaging)
	}
	// "<id>:<staging path>" puts the staged files at the target's root, not
	// under the full absolute path (spike P22-S1 item 9).
	rcmd, err := restic.Restore(restic.RestoreOptions{Common: w.common(), SnapshotID: snap + ":" + snapshotTreePath(newest.Paths[0]), Target: dir})
	if err != nil {
		return failed(SelfClassManifest, snap)
	}
	_, class = w.selfExec(ctx, t, rcmd, selfRestoreTimeout, nil, nil)
	switch {
	case class == "":
	case inconclusiveClass(class):
		w.selfUnlockAfter(ctx, t, class)
		return inconclusive(class)
	case class == string(restic.ClassRepoMissing) || class == string(restic.ClassWrongPassword):
		return failed(class, snap)
	default:
		return failed(SelfClassRestore, snap)
	}
	if w.selfAfterRestore != nil {
		w.selfAfterRestore(dir)
	}

	v := w.selfCompare(ctx, dir, snap)
	if v.class != "" || !held() {
		return v
	}

	// The whole repository, every byte (restic decrypts and hashes each pack).
	ccmd, err := restic.Check(restic.CheckOptions{Common: w.common()})
	if err != nil || len(ccmd.Args) < 2 {
		return inconclusive(string(restic.ClassInternal))
	}
	ccmd.Args = append([]string{"check", "--json", "--read-data"}, ccmd.Args[2:]...)
	var ev restic.Evidence
	_, class = w.selfExec(ctx, t, ccmd, selfCheckTimeout, func(r io.Reader) error {
		dec := restic.NewDecoder(r).ForKind(restic.KindCheck)
		for {
			msg, err := dec.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if s, ok := msg.(restic.CheckSummary); ok {
				ev.CheckErrors = s.NumErrors + s.BrokenPacks
			}
		}
	}, &ev)
	switch {
	case ev.CheckErrors > 0:
		return selfVerdict{verdict: true, class: SelfClassCheck, snap: snap, escrow: v.escrow}
	case class == "":
	case inconclusiveClass(class):
		w.selfUnlockAfter(ctx, t, class)
		return inconclusive(class)
	default:
		return selfVerdict{verdict: true, class: SelfClassCheck, snap: snap, escrow: v.escrow}
	}
	return selfVerdict{verdict: true, ok: true, snap: snap, escrow: v.escrow}
}

// selfCompare checks the restored folder against its manifest: the database's
// bytes and sha256, PRAGMA integrity_check, the schema version, the row count
// of every table, the master key id, and one escrowed key unwrapped from the
// copy. A class of "" means everything matched (escrow says whether a key was
// there to unwrap).
func (w *Worker) selfCompare(ctx context.Context, dir, snap string) selfVerdict {
	raw, err := os.ReadFile(filepath.Join(dir, SelfBackupManifestFile))
	if err != nil {
		return failed(SelfClassManifest, snap)
	}
	var m SelfBackupManifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Format != SelfBackupManifestFormat || m.Database.Name != SelfBackupDBFile {
		return failed(SelfClassManifest, snap)
	}
	dbPath := filepath.Join(dir, SelfBackupDBFile)
	size, sum, err := hashFile(dbPath)
	if err != nil || size != m.Database.Bytes || sum != m.Database.SHA256 {
		return failed(SelfClassManifest, snap)
	}
	facts, err := store.InspectDatabaseFile(ctx, dbPath)
	switch {
	case ctx.Err() != nil:
		return inconclusive(SelfClassInterrupted)
	case err != nil || facts.Integrity != "ok":
		return failed(SelfClassIntegrity, snap)
	case facts.SchemaVersion != m.SchemaVersion || !sameCounts(facts.Tables, m.Tables):
		return failed(SelfClassManifest, snap)
	}
	if w.selfKeys == nil {
		return inconclusive(SelfClassNoKey)
	}
	if m.MasterKeyID != w.selfKeys.MasterKeyID() {
		return failed(SelfClassMasterKey, snap)
	}
	rs, _, err := store.OpenNoMigrate(ctx, store.Options{Path: dbPath})
	if err != nil {
		if ctx.Err() != nil {
			return inconclusive(SelfClassInterrupted)
		}
		return failed(SelfClassIntegrity, snap)
	}
	defer rs.Close()
	id, err := rs.AnyEscrowedDevice(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return selfVerdict{verdict: true, snap: snap} // a console without escrowed devices: nothing to unwrap
	case err != nil:
		return failed(SelfClassIntegrity, snap)
	}
	if err := w.selfKeys.VerifyEscrow(ctx, rs, id); err != nil {
		if ctx.Err() != nil {
			return inconclusive(SelfClassInterrupted)
		}
		return failed(SelfClassEscrow, snap)
	}
	return selfVerdict{verdict: true, snap: snap, escrow: true}
}

func sameCounts(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// recordSelfVerify writes the verify job row and SettingSelfBackupVerify. An
// inconclusive attempt keeps the previous verdict.
func (w *Worker) recordSelfVerify(ctx context.Context, prev *store.SelfBackupVerify, v selfVerdict, start time.Time) {
	now := w.clock()
	row := store.SelfBackupVerify{AttemptAt: now.UnixMilli()}
	if prev != nil {
		row.At, row.OK, row.Class, row.SnapshotID, row.EscrowChecked = prev.At, prev.OK, prev.Class, prev.SnapshotID, prev.EscrowChecked
	}
	switch {
	case !v.verdict:
		row.AttemptClass = v.class
		w.recordSelfJob(ctx, SelfBackupVerifyJob, "error", v.class, start, selfVerifyRetry)
		w.log.Warn("self backup verify could not reach a verdict", attrKind(SelfBackupVerifyJob), attrClass(v.class), w.attrStorageHostIf(v.class))
	case v.ok:
		row.At, row.OK, row.Class, row.SnapshotID, row.EscrowChecked = now.UnixMilli(), true, "", v.snap, v.escrow
		w.recordSelfJob(ctx, SelfBackupVerifyJob, "ok", "", start, SelfBackupVerifyEvery)
		w.log.Info("self backup verified", attrKind(SelfBackupVerifyJob), attrElapsed(now.Sub(start)))
	default:
		row.At, row.OK, row.Class, row.SnapshotID, row.EscrowChecked = now.UnixMilli(), false, v.class, v.snap, v.escrow
		w.recordSelfJob(ctx, SelfBackupVerifyJob, "error", v.class, start, selfVerifyRetry)
		w.log.Error("self backup verify FAILED: the newest self backup did not restore intact", attrKind(SelfBackupVerifyJob), attrClass(v.class))
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if err := w.st.RecordSelfBackupVerify(rctx, row); err != nil {
		w.log.Error("self backup verify result not recorded", attrKind(SelfBackupVerifyJob))
	}
}

// snapshotTreePath turns a backed-up root as restic lists it into the path of
// that folder inside the snapshot tree: forward slashes, and a Windows drive
// path "C:\x\y" becomes "/C/x/y" (the form `restore <id>:<path>` takes).
func snapshotTreePath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p[:1] + p[2:]
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
