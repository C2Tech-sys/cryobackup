package maintain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Errors New returns. They never carry a path or a value.
var (
	// ErrResticBinary: the restic executable is missing, unreadable or its
	// SHA-256 differs from Config.ResticSHA256.
	ErrResticBinary = errors.New("maintain: restic binary not found or hash mismatch")
	// ErrResticVersion: `restic version --json` did not report PinnedResticVersion.
	ErrResticVersion = errors.New("maintain: restic is not the pinned version")
)

// errScratchNotOurs: the scratch directory is a symlink, not a directory, or
// owned by another user (prepareScratch).
var errScratchNotOurs = errors.New("maintain: the scratch directory is not the worker's own")

// errScratchSymlink: a component of the scratch path is a symlink (it wraps
// errScratchNotOurs).
var errScratchSymlink = fmt.Errorf("%w: a component of its path is a symlink", errScratchNotOurs)

// Worker is the admin-path backend (architecture DH1, §7.3): it executes
// restic through the rclone: backend with the storage admin credential, for the
// console that holds neither. It implements adminpath.Backend (see doc.go for
// the method table) and runs the maintenance scheduler (Loop, RunOnce). Create
// one with New; it is safe for concurrent use.
type Worker struct {
	cfg     Config
	st      *store.Store
	keys    KeyUnwrapper
	clock   func() time.Time
	log     *logx.Logger
	run     *runner
	sem     chan struct{}
	streams chan struct{} // dump stream budget (Config.MaxStreams), taken before sem
	started time.Time
	holder  string // lease holder id of this process (random, path-free)

	storageOK atomic.Pointer[bool] // last storage probe result; nil until a call reached the repository
	seq       atomic.Uint64        // request ids of scheduler-originated calls
	// probeSlotWait overrides ProbeSlotWait (tests).
	probeSlotWait time.Duration
	// renewEvery is how often a pass renews the scheduler lease; zero means a third of leaseTTL (tests shorten it).
	renewEvery time.Duration

	mu         sync.Mutex
	retryAfter map[string]time.Time // step 1 reschedule, per device
	lastM5Log  time.Time
}

var _ adminpath.Backend = (*Worker)(nil)

// New builds a Worker. It validates cfg, prepares the in-memory scratch
// directory (ADR 0008), verifies the restic binary (SHA-256 when Config.ResticSHA256 is
// set, then `restic version --json` must report PinnedResticVersion) and
// returns an error when anything is off: the worker never starts against an
// unpinned engine (architecture §6.3). st and keys are required; clock may be
// nil (time.Now). New does not start any goroutine.
func New(cfg Config, st *store.Store, keys KeyUnwrapper, clock func() time.Time) (*Worker, error) {
	if st == nil {
		return nil, cfgErr("store is required")
	}
	if keys == nil {
		return nil, cfgErr("key unwrapper is required")
	}
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	lg := cfg.Logger
	if lg == nil {
		lg = logx.Default()
	}
	if cfg.Version == "" {
		cfg.Version = buildinfo.Version
	}

	if err := prepareScratch(cfg.ScratchDir, ownedByMe); err != nil {
		return nil, scratchErr(err)
	}
	if inMemory, known := onMemoryFS(cfg.ScratchDir); known && !inMemory {
		// Fail loudly, degrade gracefully: the worker still runs (restic has
		// --no-cache, so only temp files are at stake), but the operator is told.
		lg.Warn("maintain scratch directory is not a tmpfs: restic and rclone temp files would reach the disk, ADR 0008",
			logx.Class("scratch_not_tmpfs"))
	}

	w := &Worker{
		cfg:        cfg,
		st:         st,
		keys:       keys,
		clock:      clock,
		log:        lg,
		sem:        make(chan struct{}, cfg.MaxConcurrent),
		streams:    make(chan struct{}, cfg.MaxStreams),
		started:    clock(),
		retryAfter: make(map[string]time.Time),
	}
	var id [4]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("maintain: random: %w", err)
	}
	w.holder = "maintain-" + hex.EncodeToString(id[:])

	resolved, err := exec.LookPath(cfg.ResticProgram)
	if err != nil {
		return nil, ErrResticBinary
	}
	if cfg.ResticSHA256 != "" {
		_, got, err := hashFile(resolved)
		if err != nil || got != cfg.ResticSHA256 {
			return nil, ErrResticBinary
		}
	}
	w.run = &runner{program: resolved, dir: w.tmpDir()}
	if err := w.checkVersion(context.Background()); err != nil {
		return nil, err
	}
	w.log.Info("maintain worker ready")
	return w, nil
}

// checkVersion runs `restic version --json` and requires the pinned version.
func (w *Worker) checkVersion(ctx context.Context) error {
	var info restic.VersionInfo
	cmd := restic.Version()
	args := append(append([]string(nil), cmd.Args...), restic.NoCacheFlag)
	res, err := w.run.run(ctx, w.cfg.Timeouts.Version, args, w.baseEnv(), func(r io.Reader) error {
		var derr error
		info, derr = restic.DecodeVersion(r)
		return derr
	})
	if err != nil || res.Exit != 0 || res.StdoutErr != nil || info.Version != PinnedResticVersion {
		return ErrResticVersion
	}
	return nil
}

// baseEnv is the minimal environment base of every child: PATH (and SystemRoot
// on Windows, which Go programs need to use the network), RCLONE_CONFIG when
// configured, and the in-memory scratch locations: TMPDIR (restic's and
// rclone's temp files on Linux), RCLONE_TEMP_DIR and RCLONE_CACHE_DIR. The
// parent's other variables never reach restic or rclone. restic.Env.Environ
// appends the RESTIC_* entries (and TEMP/TMP, the Windows names).
func (w *Worker) baseEnv() []string {
	base := []string{"PATH=" + os.Getenv("PATH")}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("SystemRoot"); v != "" {
			base = append(base, "SystemRoot="+v)
		}
	}
	if w.cfg.RcloneConfig != "" {
		base = append(base, "RCLONE_CONFIG="+w.cfg.RcloneConfig)
	}
	return append(base, "TMPDIR="+w.tmpDir(), "RCLONE_TEMP_DIR="+w.tmpDir(), "RCLONE_CACHE_DIR="+w.rcloneDir())
}

// tmpDir and rcloneDir are the two subdirectories of the scratch space.
func (w *Worker) tmpDir() string    { return filepath.Join(w.cfg.ScratchDir, "tmp") }
func (w *Worker) rcloneDir() string { return filepath.Join(w.cfg.ScratchDir, "rclone") }

// prepareScratch creates the scratch directory 0700 and empties its tmp and
// rclone subdirectories (left over by a worker that died with runs in flight).
// Only those two names are removed: the scratch directory itself is
// configuration and is never deleted. Before anything is changed the directory
// must be a real directory (not a symlink) owned by the worker's user (owned
// reports that; known false skips the check where ownership cannot be read):
// otherwise errScratchNotOurs, and nothing is touched. Config.withDefaults has
// already refused a filesystem root or a top-level directory.
func prepareScratch(dir string, owned func(os.FileInfo) (mine, known bool)) error {
	// Check before creating anything: an existing scratch directory must be
	// ours, and a missing one is created only under a real directory (not a
	// symlink that would put it somewhere else).
	if err := checkScratchPath(dir, owned); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// And again once it exists: nothing may have replaced it in between.
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errScratchNotOurs
	}
	if mine, known := owned(fi); known && !mine {
		return errScratchNotOurs
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, sub := range []string{"tmp", "rclone"} {
		p := filepath.Join(dir, sub)
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// scratchErr is New's configuration error for a prepareScratch failure: a
// fixed sentence that says what to do, never the path.
func scratchErr(err error) error {
	switch {
	case errors.Is(err, errScratchSymlink):
		return cfgErr("ScratchDir (CRYO_MAINTAIN_SCRATCH_DIR) has a symlink in its path (for example /var/run, which links to /run): refusing to create or empty it there; use the real directory, such as /run/cryo-maintain")
	case errors.Is(err, errScratchNotOurs):
		return cfgErr("ScratchDir (CRYO_MAINTAIN_SCRATCH_DIR) is not a directory owned by the worker's user: refusing to empty its tmp and rclone subdirectories")
	}
	return cfgErr("cannot prepare the scratch directory")
}

// checkScratchPath is prepareScratch's look before it creates anything. When
// dir exists it must be a real directory (Lstat: not a symlink) owned by the
// worker's user. When it does not, the nearest existing ancestor - the
// directory the missing components would be created under - must be a real
// directory too; every component between them is missing and is created 0700
// by the worker itself. No existing component of the path, from the root
// down, may be a symlink (errScratchSymlink: /var/run/x is refused, /run/x is
// not). Anything else is errScratchNotOurs, and nothing on disk has changed.
func checkScratchPath(dir string, owned func(os.FileInfo) (mine, known bool)) error {
	return checkScratchPathWith(dir, owned, os.Lstat)
}

// checkScratchPathWith is checkScratchPath with the Lstat injected (tests
// show it a symlink or ENOTDIR where the platform cannot make one).
func checkScratchPathWith(dir string, owned func(os.FileInfo) (mine, known bool), lstat func(string) (os.FileInfo, error)) error {
	target := filepath.Clean(dir)
	p := target
	for {
		fi, err := lstat(p)
		switch {
		case err == nil:
			if fi.Mode()&os.ModeSymlink != 0 {
				return errScratchSymlink
			}
			if !fi.IsDir() { // a file where the directory or a parent must be
				return errScratchNotOurs
			}
			if p == target {
				if mine, known := owned(fi); known && !mine {
					return errScratchNotOurs
				}
			}
			return checkAncestors(p, lstat)
		case errors.Is(err, os.ErrNotExist):
			// Missing: look at the parent. (Checked before ENOTDIR: Windows
			// answers "path not found" for a missing parent, which Go may also
			// match as ENOTDIR.)
		case notDirErr(err):
			// Linux: a path below a regular file is ENOTDIR, not ENOENT.
			return errScratchNotOurs
		default:
			return err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return errScratchNotOurs // no existing ancestor at all
		}
		p = parent
	}
}

// checkAncestors refuses when any ancestor of the existing directory p, up to
// the filesystem root, is a symlink.
func checkAncestors(p string, lstat func(string) (os.FileInfo, error)) error {
	for {
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
		fi, err := lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errScratchSymlink
		}
	}
}

// notDirErr reports an Lstat error that means "a component of the path is not
// a directory" (ENOTDIR).
func notDirErr(err error) bool { return errors.Is(err, syscall.ENOTDIR) }

// common returns the restic.Common of every repository call: --retry-lock,
// --no-cache (ADR 0008: no repository data on the console host's disk) and the
// rclone admin-path options (restic.AdminOptions, BypassGovernance is always
// false in v0).
func (w *Worker) common() restic.Common {
	return restic.Common{
		RetryLock: w.cfg.RetryLock,
		Extra:     append([]string{restic.NoCacheFlag}, restic.AdminOptions(w.cfg.RcloneProgram, false)...),
	}
}

// target is one device repository ready to address: the device, the restic
// environment with the unwrapped password, and nothing else. It lives for one
// call.
type target struct {
	dev store.Device
	env restic.Env
}

// drop forgets the repository password. Go strings are immutable, so this
// releases the reference instead of overwriting the bytes; see doc.go.
func (t *target) drop() { t.env.Password = ""; t.env.Repository = "" }

// prepare resolves the device, refuses customer-held custody (architecture
// DH22), unwraps the repository password and builds the restic environment.
// The returned error is an *adminpath.Error, a context error, or nil.
func (w *Worker) prepare(ctx context.Context, deviceID string) (*target, error) {
	scope := store.ScopeAll()
	dev, err := w.st.GetDevice(ctx, scope, deviceID)
	if err != nil {
		return nil, w.storeErr(ctx, err)
	}
	cust, err := w.st.GetCustomer(ctx, scope, dev.CustomerID)
	if err != nil {
		return nil, w.storeErr(ctx, err)
	}
	if cust.KeyCustody == "customer_held" {
		return nil, adminpath.NewError(adminpath.CodeCustodyCustomerHeld, "")
	}
	if esc, err := w.st.ActiveEscrow(ctx, scope, deviceID); err == nil {
		if esc.Custody == "customer_held" {
			return nil, adminpath.NewError(adminpath.CodeCustodyCustomerHeld, "")
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, w.storeErr(ctx, err)
	}
	pw, err := w.keys.Unwrap(ctx, deviceID)
	switch {
	case errors.Is(err, ErrCustodyCustomerHeld):
		return nil, adminpath.NewError(adminpath.CodeCustodyCustomerHeld, "")
	case errors.Is(err, store.ErrNotFound):
		return nil, adminpath.NewError(adminpath.CodeNotFound, "")
	case err != nil:
		return nil, w.ctxOr(ctx, adminpath.NewError(adminpath.CodeInternal, ""))
	case pw == "":
		return nil, adminpath.NewError(adminpath.CodeInternal, "")
	}
	repo, err := restic.AdminRepository(w.cfg.Remote, w.cfg.Bucket, w.cfg.Prefix, deviceID)
	if err != nil {
		return nil, adminpath.NewError(adminpath.CodeInvalidRequest, "")
	}
	return &target{dev: dev, env: restic.Env{
		Repository: repo,
		Password:   pw,
		NoCache:    true,
		TempDir:    w.tmpDir(),
	}}, nil
}

// storeErr maps a store error to the admin-path error model.
func (w *Worker) storeErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return adminpath.NewError(adminpath.CodeNotFound, "")
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return adminpath.NewError(adminpath.CodeInternal, "")
}

// ctxOr returns the context error when ctx is done, else e.
func (w *Worker) ctxOr(ctx context.Context, e error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return e
}

// execResult is one finished restic run, reduced to what the callers need.
type execResult struct {
	procResult
	// Result is restic.Map of the exit code and the evidence.
	Result restic.Result
	// Elapsed is the wall time of the run by the worker's clock.
	Elapsed time.Duration
}

// exec runs one restic command against t: it takes a concurrency slot, builds
// the environment (restic.Env.Environ over the minimal base), starts the child
// through the runner and maps the exit code (restic.Map). The returned error is
// non-nil when the call could not run at all: context done, a slot never
// freed, or a start failure (an internal admin-path error).
func (w *Worker) exec(ctx context.Context, t *target, cmd restic.Command, timeout time.Duration, onStdout func(io.Reader) error) (execResult, error) {
	return w.execSlot(ctx, t, cmd, timeout, 0, onStdout)
}

// execSlot is exec with a bounded wait for the concurrency slot: with
// slotWait > 0, a slot that does not come free within slotWait is busy
// (adminpath.CodeBusy) instead of a wait until ctx ends. The storage probe uses
// it (ADR 0017), so a worker whose slots are all taken answers "busy" (not
// checked) and never makes the probe time out as if storage were down.
func (w *Worker) execSlot(ctx context.Context, t *target, cmd restic.Command, timeout, slotWait time.Duration, onStdout func(io.Reader) error) (execResult, error) {
	var busy <-chan time.Time
	if slotWait > 0 {
		tm := time.NewTimer(slotWait)
		defer tm.Stop()
		busy = tm.C
	}
	select {
	case w.sem <- struct{}{}:
		defer func() { <-w.sem }()
	case <-busy:
		return execResult{}, adminpath.NewError(adminpath.CodeBusy, "")
	case <-ctx.Done():
		return execResult{}, ctx.Err()
	}
	environ, err := t.env.Environ(w.baseEnv())
	if err != nil {
		return execResult{}, adminpath.NewError(adminpath.CodeInternal, "")
	}
	t0 := w.clock()
	pr, err := w.run.run(ctx, timeout, cmd.Args, environ, onStdout)
	if err != nil {
		return execResult{}, w.ctxOr(ctx, adminpath.NewError(adminpath.CodeInternal, ""))
	}
	res := execResult{procResult: pr, Elapsed: w.clock().Sub(t0)}
	res.Result = restic.Map(cmd.Kind, pr.Exit, restic.Evidence{Stderr: pr.Stderr})
	if pr.Canceled {
		res.Result = restic.Result{Outcome: restic.OutcomeInterrupted}
	}
	w.noteStorage(res)
	w.log.Info("restic run finished",
		attrKind(string(cmd.Kind)), attrDevice(t.dev.ID), attrExit(res.Exit),
		attrOutcome(string(res.Result.Outcome)), attrElapsed(res.Elapsed))
	if class := res.errorClass(); storageClass(class) {
		// Free text is opaque in the log, so the storage endpoint the run could
		// not reach and the class (network, tls, storage) go in typed fields:
		// an expired certificate or a DNS outage names itself here.
		w.log.Warn("restic could not reach the storage",
			attrKind(string(cmd.Kind)), attrDevice(t.dev.ID), attrClass(string(class)), attrExit(res.Exit), w.attrStorageHost())
	}
	return res, nil
}

// attrStorageHost is the configured storage host as a typed field.
func (w *Worker) attrStorageHost() logx.Attr {
	if w.cfg.StorageHost == "" {
		return logx.Str(keyStorageHost, "unconfigured")
	}
	return logx.Host(keyStorageHost, w.cfg.StorageHost)
}

// StorageHostOf returns the host of a storage endpoint as configured on a
// storage target: a URL ("https://s3.example:9000") or a bare host[:port].
func StorageHostOf(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if strings.Contains(e, "://") {
		if u, err := url.Parse(e); err == nil {
			return u.Hostname()
		}
		return ""
	}
	if h, _, err := net.SplitHostPort(e); err == nil {
		return h
	}
	return e
}

// noteStorage records the last storage probe for Ping: success when a run
// exited 0, failure when its stderr class says the storage target was
// unreachable.
func (w *Worker) noteStorage(r execResult) {
	var ok bool
	switch {
	case r.Exit == 0:
		ok = true
	case storageClass(r.errorClass()):
		ok = false
	default:
		return
	}
	w.storageOK.Store(&ok)
}

// errorClass is the path-free class of a failed run: Map's class, else the
// dominant stderr class.
func (r execResult) errorClass() restic.ErrorClass {
	if r.Result.ErrorClass != "" {
		return r.Result.ErrorClass
	}
	return r.Stderr
}

// storageClass reports whether c means the storage target (or the path to it)
// is the problem.
func storageClass(c restic.ErrorClass) bool {
	switch c {
	case restic.ClassNetwork, restic.ClassTLS, restic.ClassStorage:
		return true
	}
	return false
}

// readError converts a finished run of a read-only call into the error the
// Backend contract asks for, or nil when the run and its stdout consumer
// succeeded (architecture §6.4, adminpath doc "Errors").
func readError(ctx context.Context, r execResult) error {
	switch {
	case r.Canceled:
		if err := ctx.Err(); err != nil {
			return err
		}
		return adminpath.NewError(adminpath.CodeUnavailable, "")
	case r.TimedOut:
		return adminpath.NewError(adminpath.CodeTimeout, "")
	}
	switch r.Exit {
	case 0:
		if r.StdoutErr != nil {
			return adminpath.NewError(adminpath.CodeResticFailed, restic.ClassParse)
		}
		return nil
	case 10:
		return adminpath.NewError(adminpath.CodeRepoMissing, restic.ClassRepoMissing)
	case 12:
		return adminpath.NewError(adminpath.CodeWrongPassword, restic.ClassWrongPassword)
	case 11:
		return adminpath.NewError(adminpath.CodeLocked, restic.ClassLock)
	}
	class := r.errorClass()
	if class == "" {
		class = restic.ClassUnknown
	}
	if storageClass(class) {
		return adminpath.NewError(adminpath.CodeStorageUnreachable, class)
	}
	return adminpath.NewError(adminpath.CodeResticFailed, class)
}

// meta builds the ResultMeta of a result-style call (Init, Unlock, ...).
func (r execResult) meta() adminpath.ResultMeta {
	return adminpath.ResultMeta{
		Elapsed:        r.Elapsed,
		ExitCode:       r.Exit,
		Outcome:        r.Result.Outcome,
		DeferredReason: r.Result.DeferredReason,
		ErrorClass:     r.Result.ErrorClass,
	}
}

// okMeta is the ResultMeta of a successful read-only call that returns a
// result type with metadata (Ls, Dump).
func okMeta(elapsed time.Duration) adminpath.ResultMeta {
	return adminpath.ResultMeta{Elapsed: elapsed, Outcome: restic.OutcomeSuccess}
}

// hashFile returns the size and the lower-case hex SHA-256 of a file (the
// restic binary check in New).
func hashFile(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
