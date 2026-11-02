package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// PinnedVersion is the restic release this agent drives (ADR 0003). Every flag
// in internal/restic is verified against it; any other version is refused.
const PinnedVersion = "0.19.1"

// Errors returned by Run and Verify. None carries a path or a secret.
var (
	ErrHashMismatch    = errors.New("supervisor: restic binary hash does not match the pinned value")
	ErrNotPinned       = errors.New("supervisor: no expected hash for the restic binary and unpinned mode is off")
	ErrVersionMismatch = errors.New("supervisor: restic reports a version other than the pinned one")
	ErrStart           = errors.New("supervisor: restic could not be started")
	ErrRunLog          = errors.New("supervisor: the run log could not be opened")
	// ErrCancelledBeforeStart: the context ended before the child was started
	// (during the first binary verification); there is no child and no exit.
	ErrCancelledBeforeStart = errors.New("supervisor: cancelled before restic was started")
)

// Cancellation causes. A caller cancels the context with
// context.WithCancelCause(...); the cause decides how the exit is classified.
var (
	// ErrServiceStop: the service is stopping or suspending; a 130 exit maps to
	// "interrupted" (restic.CancelService).
	ErrServiceStop = errors.New("supervisor: service stop")
	// ErrCancelled: the operator or a command asked for the cancel (also the
	// default for a plain context.Canceled); a 130 exit maps to "cancelled".
	ErrCancelled = errors.New("supervisor: cancelled")
)

// Cancel modes reported in Result.CancelMode.
const (
	CancelGraceful   = "graceful"   // the child exited after the interrupt (CTRL_BREAK or SIGINT)
	CancelTerminated = "terminated" // the child was killed (job object or Kill) after the grace period or a failed interrupt
)

// DefaultCancelGrace is how long a child gets to exit after the interrupt
// before it is terminated (architecture §6.6, L-CANCEL-1).
const DefaultCancelGrace = 10 * time.Second

// DefaultProgressInterval is the sampling period of the progress struct (§6.2).
const DefaultProgressInterval = 5 * time.Second

// maxStdout caps what is captured for document commands (snapshots, version).
const maxStdout = 64 << 20

// Spec is one invocation.
type Spec struct {
	// Binary is the path of the pinned restic executable.
	Binary string
	// ExpectedSHA256 is the lower-case hex digest the binary must have.
	ExpectedSHA256 string
	// AllowUnpinned lets a run proceed without ExpectedSHA256 (lab only). The
	// version check still applies.
	AllowUnpinned bool
	// Command is the argument list built by internal/restic.
	Command restic.Command
	// Env is the restic environment (DH3); secrets travel only here.
	Env restic.Env
	// BaseEnv is the environment the child inherits (restic-owned names are
	// replaced); nil means os.Environ().
	BaseEnv []string
	// RunLogPath is the verbatim local log (path-bearing, local-only); empty
	// disables it.
	RunLogPath string
	// MaxRuntime cancels the run when exceeded (Result.LongRun); zero = none.
	MaxRuntime time.Duration
	// NoWindow starts the child with CREATE_NO_WINDOW (service mode). In console
	// mode the child shares the console so CTRL_BREAK can reach it.
	NoWindow bool
	// MemoryLimitMiB caps the child's memory through the job object; zero = none.
	MemoryLimitMiB int
	// CancelGrace overrides DefaultCancelGrace.
	CancelGrace time.Duration
	// ProgressInterval overrides DefaultProgressInterval.
	ProgressInterval time.Duration
	// StallAfter marks the progress as stalled when no byte moved for this
	// long; zero disables the flag.
	StallAfter time.Duration
	// OnProgress is called with every published sample.
	OnProgress func(Progress)
	// Clock is the device clock; nil means time.Now.
	Clock func() time.Time
}

// Progress is the sampled state of a backup run: counts only, no paths.
type Progress struct {
	Kind           restic.Kind
	PercentDone    float64
	BytesDone      uint64
	TotalBytes     uint64
	FilesDone      uint64
	TotalFiles     uint64
	ErrorCount     uint64
	SecondsElapsed uint64
	Stalled        bool
	UpdatedAt      time.Time
}

// Result is what a finished child left behind.
type Result struct {
	ExitCode  int
	Restic    restic.Result
	Summary   *restic.BackupSummary
	Counts    restic.ErrorCounts
	ExitError *restic.ExitError
	// FailedFiles is the sum of error counts during archival.
	FailedFiles int64
	// Unparsed counts lines that were not JSON objects.
	Unparsed   int
	Elapsed    time.Duration
	LongRun    bool
	Cancel     restic.CancelCause
	CancelMode string
	Progress   Progress
	// Stdout is the captured output of a document command (snapshots, version).
	Stdout []byte
	// VSSFallbacks counts restic's "failed to create snapshot for [<volume>]"
	// errors: volumes read live because their VSS snapshot failed
	// (restic.VSSFallbackVolume). They are counted in Counts as class vss and
	// are not failed files.
	VSSFallbacks int
	// VSSFallbackVolumes are the drive letters of those volumes, upper case,
	// sorted, each once ("C"); a fallback on a volume without a letter is in
	// VSSFallbacks only.
	VSSFallbackVolumes []string
	// MemoryLimitHit is true when the job object reported that the child hit
	// Spec.MemoryLimitMiB (JOB_OBJECT_MSG_PROCESS_MEMORY_LIMIT). A child that
	// then died on its own is classified failed/memory.
	MemoryLimitHit bool
}

// Supervisor runs restic children and remembers which binary it has verified.
type Supervisor struct {
	mu       sync.Mutex
	verified map[string]verifiedBinary

	progMu   sync.Mutex
	progress Progress
	active   atomic.Bool
}

type verifiedBinary struct {
	size    int64
	modTime time.Time
	sha     string
	version string
	pinned  bool
}

// New returns a Supervisor.
func New() *Supervisor { return &Supervisor{verified: map[string]verifiedBinary{}} }

// Progress returns the latest published sample of the run in progress.
func (s *Supervisor) Progress() (Progress, bool) {
	if !s.active.Load() {
		return Progress{}, false
	}
	s.progMu.Lock()
	defer s.progMu.Unlock()
	return s.progress, true
}

// Verify checks the binary once per process (re-checked if the file changes):
// its SHA-256 against expectedSHA256 (unless allowUnpinned) and the version
// reported by "restic version --json" against PinnedVersion. It returns the
// version.
func (s *Supervisor) Verify(ctx context.Context, binary, expectedSHA256 string, allowUnpinned bool, noWindow bool) (string, error) {
	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	if expectedSHA256 == "" && !allowUnpinned {
		return "", ErrNotPinned
	}
	st, err := os.Stat(binary)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrStart, "binary not found")
	}
	s.mu.Lock()
	v, ok := s.verified[binary]
	s.mu.Unlock()
	if ok && v.size == st.Size() && v.modTime.Equal(st.ModTime()) && (expectedSHA256 == "" || v.sha == expectedSHA256) &&
		(v.pinned || expectedSHA256 == "") {
		return v.version, nil
	}

	sum, err := fileSHA256(binary)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrStart, "binary not readable")
	}
	if expectedSHA256 != "" && sum != expectedSHA256 {
		return "", ErrHashMismatch
	}
	version, err := queryVersion(ctx, binary, noWindow)
	if err != nil {
		return "", err
	}
	if version != PinnedVersion {
		return "", fmt.Errorf("%w (found %s, want %s)", ErrVersionMismatch, safeVersion(version), PinnedVersion)
	}
	s.mu.Lock()
	s.verified[binary] = verifiedBinary{size: st.Size(), modTime: st.ModTime(), sha: sum, version: version, pinned: expectedSHA256 != ""}
	s.mu.Unlock()
	return version, nil
}

func safeVersion(v string) string {
	if len(v) > 16 {
		return "?"
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c == '.' || c >= 'a' && c <= 'z' || c == '-') {
			return "?"
		}
	}
	return v
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path from the agent's own state directory or configuration
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func queryVersion(ctx context.Context, binary string, noWindow bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, restic.Version().Args...) //nolint:gosec // the pinned binary, verified before it is run
	cmd.Env = baseEnvWithout(os.Environ())
	prepare(cmd, Spec{NoWindow: noWindow})
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: version query failed", ErrStart)
	}
	info, err := restic.DecodeVersion(&out)
	if err != nil {
		return "", fmt.Errorf("%w: version output not understood", ErrStart)
	}
	return info.Version, nil
}

// baseEnvWithout drops restic-owned variables so a stray RESTIC_REPOSITORY in
// the agent's own environment cannot reach a version query.
func baseEnvWithout(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if strings.HasPrefix(strings.ToUpper(kv), "RESTIC_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// runState is the mutable state of one Run, shared by the reader goroutines.
type runState struct {
	mu      sync.Mutex
	kind    restic.Kind
	summary *restic.BackupSummary
	counts  restic.ErrorCounts
	// plain counts the plain-text (non-JSON) stderr lines that classify. They
	// are kept apart from counts so that a memory death can replace them:
	// after the cap, they are the Go runtime's traceback (Run, memDeath).
	plain       restic.ErrorCounts
	exitErr     *restic.ExitError
	failedFiles int64
	unparsed    int
	latest      Progress
	lastBytes   uint64
	lastMoved   time.Time
	// VSS per-volume fallbacks seen on stderr (vssFilter).
	vssFallbacks int
	vssVolumes   map[string]bool
}

// vssFallback records one "failed to create snapshot for [<volume>]" line: a
// vss error of the run, not a failed file. vol is a drive letter or "".
func (r *runState) vssFallback(vol string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vssFallbacks++
	r.counts.Add(restic.ErrorEvent{During: "archival", Class: restic.ClassVSS})
	if vol != "" {
		if r.vssVolumes == nil {
			r.vssVolumes = map[string]bool{}
		}
		r.vssVolumes[vol] = true
	}
}

func (r *runState) handle(m restic.Message, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch v := m.(type) {
	case restic.BackupStatus:
		r.latest = Progress{
			Kind: r.kind, PercentDone: clamp01(v.PercentDone), BytesDone: v.BytesDone, TotalBytes: v.TotalBytes,
			FilesDone: v.FilesDone, TotalFiles: v.TotalFiles, ErrorCount: v.ErrorCount,
			SecondsElapsed: v.SecondsElapsed, UpdatedAt: now,
		}
		if v.BytesDone != r.lastBytes {
			r.lastBytes, r.lastMoved = v.BytesDone, now
		}
	case restic.BackupSummary:
		sum := v
		r.summary = &sum
	case restic.ErrorEvent:
		r.counts.Add(v)
		if v.During == "archival" {
			r.failedFiles++
		}
	case restic.ExitError:
		e := v
		r.exitErr = &e
	case restic.Unparsed:
		r.unparsed++
		if v.Class != restic.ClassUnknown && v.Class != "" {
			// A plain-text line that classifies (a missing root, a lock
			// message) still counts, without its text.
			r.plain.Add(restic.ErrorEvent{Class: v.Class})
		}
	}
}

func clamp01(f float64) float64 {
	switch {
	case f < 0 || f != f:
		return 0
	case f > 1:
		return 1
	}
	return f
}

// Run starts the child described by spec, supervises it and returns what it
// did. The error is non-nil only when the child could not be started (binary
// refused, environment invalid, run log not writable); every outcome of a
// started child, including a kill, is a Result.
func (s *Supervisor) Run(ctx context.Context, spec Spec) (Result, error) {
	clock := spec.Clock
	if clock == nil {
		clock = time.Now
	}
	if len(spec.Command.Args) == 0 {
		return Result{}, errors.New("supervisor: empty command")
	}
	if _, err := s.Verify(ctx, spec.Binary, spec.ExpectedSHA256, spec.AllowUnpinned, spec.NoWindow); err != nil {
		if ctx.Err() != nil {
			return Result{}, ErrCancelledBeforeStart
		}
		return Result{}, err
	}
	base := spec.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	environ, err := spec.Env.Environ(base)
	if err != nil {
		return Result{}, err
	}

	var runLog *lockedFile
	if spec.RunLogPath != "" {
		f, err := os.OpenFile(spec.RunLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path from the agent's own state directory or configuration
		if err != nil {
			return Result{}, ErrRunLog
		}
		runLog = &lockedFile{f: f}
		defer runLog.close()
	}

	cmd := exec.Command(spec.Binary, spec.Command.Args...) //nolint:gosec // the pinned binary, verified by Verify above
	cmd.Env = environ
	prepare(cmd, spec)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, ErrStart
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, ErrStart
	}

	started := clock()
	if err := cmd.Start(); err != nil {
		return Result{}, ErrStart
	}
	// newControl always returns a usable control; a job object that could not
	// be set up degrades the child to interrupt and kill supervision only.
	ctrl, _ := newControl(cmd, spec)
	defer ctrl.close()

	st := &runState{kind: spec.Command.Kind, counts: make(restic.ErrorCounts), plain: make(restic.ErrorCounts), lastMoved: started}
	s.active.Store(true)
	defer s.active.Store(false)

	interval := spec.ProgressInterval
	if interval <= 0 {
		interval = DefaultProgressInterval
	}
	stopSampler := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-t.C:
				s.publish(st, spec, clock())
			}
		}
	}()

	// Readers: both streams are consumed concurrently, teed verbatim to the run
	// log and decoded through the path-free parse boundary.
	documentKind := isDocumentKind(spec.Command.Kind)
	var captured bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var r io.Reader = stdout
		if runLog != nil {
			lt := newLineTee(runLog)
			defer lt.flush()
			r = io.TeeReader(r, lt)
		}
		if documentKind {
			_, _ = io.Copy(&limitedBuffer{buf: &captured, max: maxStdout}, r)
			return
		}
		consume(restic.NewDecoder(r).ForKind(spec.Command.Kind), st, clock)
	}()
	go func() {
		defer wg.Done()
		var r io.Reader = stderr
		if runLog != nil {
			lt := newLineTee(runLog)
			defer lt.flush()
			r = io.TeeReader(r, lt)
		}
		// The run log keeps restic's VSS fallback lines verbatim; the decoder
		// does not see them (they are not failed files): vssFilter reports
		// the volume letter instead.
		r = newVSSFilter(r, st.vssFallback)
		consume(restic.NewDecoder(r).ForKind(spec.Command.Kind), st, clock)
	}()

	// Watcher: context cancel and max runtime both go through the same
	// interrupt-then-terminate path.
	var (
		cancelCause atomic.Int32 // restic.CancelCause
		longRun     atomic.Bool
		terminated  atomic.Bool
		interrupted atomic.Bool
		memKilled   atomic.Bool
	)
	exited := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		var timer <-chan time.Time
		if spec.MaxRuntime > 0 {
			t := time.NewTimer(spec.MaxRuntime)
			defer t.Stop()
			timer = t.C
		}
		causeOf := func() restic.CancelCause {
			if errors.Is(context.Cause(ctx), ErrServiceStop) {
				return restic.CancelService
			}
			return restic.CancelAgent
		}
		select {
		case <-exited:
			return
		case <-ctrl.memoryLimit():
			// The child hit the memory cap: its next commit failed, and a Go
			// program does not survive that but may not exit either (see
			// newControl). Give it memoryKillGrace to write its last lines
			// and exit, then end it. This is not a cancel, but a cancel that
			// arrives during the grace (a service stop) is acted on at once:
			// the child is ended now, still as a memory death, and the cancel
			// is recorded (Result.Cancel, CancelMode, the run log note).
			if testHookMemoryGrace != nil {
				testHookMemoryGrace()
			}
			select {
			case <-exited:
			case <-ctx.Done():
				cancelCause.Store(int32(causeOf()))
				memKilled.Store(true)
				_ = ctrl.terminateMemory()
			case <-time.After(memoryKillGrace):
				memKilled.Store(true)
				_ = ctrl.terminateMemory()
			}
			return
		case <-ctx.Done():
			cancelCause.Store(int32(causeOf()))
		case <-timer:
			longRun.Store(true)
			cancelCause.Store(int32(restic.CancelAgent))
		}
		grace := spec.CancelGrace
		if grace <= 0 {
			grace = DefaultCancelGrace
		}
		interrupted.Store(true)
		if ctrl.interrupt() != nil {
			// No way to ask politely (no console in service mode): go
			// straight to the hard stop.
			terminated.Store(true)
			_ = ctrl.terminate()
			return
		}
		select {
		case <-exited:
			return
		case <-time.After(grace):
		}
		terminated.Store(true)
		_ = ctrl.terminate()
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	close(exited)
	<-watcherDone
	close(stopSampler)
	samplerWG.Wait()
	finished := clock()

	exit := 0
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	} else if waitErr != nil {
		exit = -1
	}
	cause := restic.CancelCause(cancelCause.Load())
	switch {
	case cause == restic.CancelNone:
	case memKilled.Load():
		// The cancel arrived while a child that had hit its memory cap was in
		// its grace: it was ended at once with STATUS_NO_MEMORY. The cap, not
		// the cancel, decides the outcome (failed/memory below); the cancel
		// stays in Result.Cancel, CancelMode and this note.
		if runLog != nil {
			runLog.write([]byte(fmt.Sprintf(`{"message_type":"cryo_supervisor_note","note":"cancel requested during the memory-cap grace; the child was terminated at once and the memory cap decides the outcome","cancel_cause":%q,"exit_code":%d}`+"\n",
				cancelCauseName(cause), exit)))
		}
	case terminated.Load(), exit == 130, interrupted.Load() && (exit < 0 || exit > 255):
		// The supervisor stopped it: terminated through the job object (or
		// Kill), restic's own 130 after honouring the interrupt, or a death
		// by the interrupt itself (an NTSTATUS such as STATUS_CONTROL_C_EXIT
		// is above 255; a signal is -1). That is the 130 of the map.
		exit = 130
	default:
		// The child ended on its own with a real exit code (1 and 2
		// included) while a cancel was pending: that code, through
		// restic.Map, is the outcome. The pending cancel is recorded in the
		// run log, not allowed to turn a failure into "cancelled".
		if runLog != nil {
			runLog.write([]byte(fmt.Sprintf(`{"message_type":"cryo_supervisor_note","note":"cancel requested but the child exited on its own; its exit code decides the outcome","cancel_cause":%q,"exit_code":%d}`+"\n",
				cancelCauseName(cause), exit)))
		}
	}

	memHit := ctrl.memoryLimitHit() || memKilled.Load()
	memDeath := memHit && memoryDeath(exit)

	st.mu.Lock()
	// The run's counts: every JSON error event plus the plain-text lines that
	// classify. After a memory death the plain-text lines are the Go runtime's
	// "fatal error: out of memory" and its goroutine traceback (dozens of
	// lines that classify as internal); they are replaced by one memory row,
	// and the traceback stays verbatim in the local run log only.
	counts := make(restic.ErrorCounts, len(st.counts)+len(st.plain))
	for k, n := range st.counts {
		counts[k] += n
	}
	if memDeath {
		counts.Add(restic.ErrorEvent{Class: restic.ClassMemory})
	} else {
		for k, n := range st.plain {
			counts[k] += n
		}
	}
	res := Result{
		ExitCode: exit, Summary: st.summary, Counts: counts, ExitError: st.exitErr,
		FailedFiles: st.failedFiles, Unparsed: st.unparsed, Elapsed: finished.Sub(started),
		LongRun: longRun.Load(), Cancel: cause, Progress: st.latest, Stdout: captured.Bytes(),
		VSSFallbacks: st.vssFallbacks, MemoryLimitHit: memHit,
	}
	for v := range st.vssVolumes {
		res.VSSFallbackVolumes = append(res.VSSFallbackVolumes, v)
	}
	st.mu.Unlock()
	sort.Strings(res.VSSFallbackVolumes)
	switch {
	case interrupted.Load():
		res.CancelMode = CancelGraceful
		if terminated.Load() {
			res.CancelMode = CancelTerminated
		}
	case memKilled.Load() && cause != restic.CancelNone:
		// The cancel was honoured by ending the child at once (no interrupt).
		res.CancelMode = CancelTerminated
	}
	res.Restic = restic.Map(spec.Command.Kind, exit, restic.Evidence{
		SummarySeen: res.Summary != nil,
		Cancel:      cause,
		Stderr:      restic.StderrClass(res.ExitError, res.Counts),
	})
	if memDeath {
		// The cap stopped it. Its stderr (a Go runtime traceback) would
		// otherwise classify as internal or unknown.
		res.Restic = restic.Result{Outcome: restic.OutcomeFailed, ErrorClass: restic.ClassMemory}
	}
	return res, nil
}

// memoryKillGrace is how long a child that hit its memory cap gets to exit on
// its own before the supervisor ends it.
const memoryKillGrace = 5 * time.Second

// testHookMemoryGrace, when set by a test, is called as the watcher enters the
// memory-kill grace, so a test can act inside that window deterministically.
var testHookMemoryGrace func()

// memoryDeath reports whether exit is how a child that hit its memory cap
// ends: anything but restic's own success, partial, repository, lock and
// password codes and the supervisor's 130 (a cancel keeps its own outcome).
// The Go runtime exits 2 after "fatal error: out of memory"; a child the
// supervisor ended has STATUS_NO_MEMORY.
func memoryDeath(exit int) bool {
	switch exit {
	case 0, 3, 10, 11, 12, 130:
		return false
	}
	return true
}

// vssFilter passes a restic stderr stream through line by line, except
// restic's per-volume VSS fallback lines (restic.VSSFallbackVolume), which it
// drops and reports as the volume letter. Lines longer than the buffer pass
// through uninspected (a fallback line is short).
type vssFilter struct {
	r       *bufio.Reader
	report  func(volume string)
	buf     []byte
	pending []byte
	midLine bool // the next chunk continues a line already passed through
	err     error
}

func newVSSFilter(r io.Reader, report func(volume string)) io.Reader {
	return &vssFilter{r: bufio.NewReaderSize(r, 64<<10), report: report}
}

func (f *vssFilter) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		if f.err != nil {
			return 0, f.err
		}
		chunk, err := f.r.ReadSlice('\n')
		whole := !f.midLine
		f.midLine = errors.Is(err, bufio.ErrBufferFull)
		if err != nil && !f.midLine {
			f.err = err
		}
		if len(chunk) == 0 {
			continue
		}
		if whole && !f.midLine {
			if vol, ok := vssFallbackLine(chunk); ok {
				f.report(vol)
				continue
			}
		}
		f.buf = append(f.buf[:0], chunk...)
		f.pending = f.buf
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

// vssFallbackLine decodes one stderr line and reports whether it is restic's
// VSS fallback error, with the volume letter ("" if not a drive letter).
func vssFallbackLine(line []byte) (string, bool) {
	if !bytes.Contains(line, []byte("snapshot for [")) {
		return "", false
	}
	var m struct {
		MessageType string `json:"message_type"`
		Error       *struct {
			Message string `json:"message"`
		} `json:"error"`
		Item string `json:"item"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &m) != nil || m.MessageType != "error" || m.Error == nil {
		return "", false
	}
	return restic.VSSFallbackVolume(m.Error.Message, m.Item)
}

// cancelCauseName names a cancel cause for the run log.
func cancelCauseName(c restic.CancelCause) string {
	switch c {
	case restic.CancelService:
		return "service_stop"
	case restic.CancelAgent:
		return "agent"
	}
	return "none"
}

func isDocumentKind(k restic.Kind) bool {
	switch k {
	case restic.KindSnapshots, restic.KindVersion, restic.KindCatConfig, restic.KindStats, restic.KindFind:
		return true
	}
	return false
}

// consume reads a decoded stream to its end.
func consume(dec *restic.Decoder, st *runState, clock func() time.Time) {
	for {
		m, err := dec.Next()
		if err != nil {
			return // io.EOF or a failed pipe: both end the stream
		}
		st.handle(m, clock())
	}
}

// publish copies the latest status into the shared progress struct and calls
// the callback.
func (s *Supervisor) publish(st *runState, spec Spec, now time.Time) {
	st.mu.Lock()
	p := st.latest
	if p.UpdatedAt.IsZero() {
		st.mu.Unlock()
		return
	}
	p.Stalled = spec.StallAfter > 0 && now.Sub(st.lastMoved) >= spec.StallAfter
	st.mu.Unlock()
	s.progMu.Lock()
	s.progress = p
	s.progMu.Unlock()
	if spec.OnProgress != nil {
		spec.OnProgress(p)
	}
}

// limitedBuffer writes into buf up to max bytes and silently drops the rest.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// lockedFile serialises the two streams' writes to one run log.
type lockedFile struct {
	mu sync.Mutex
	f  *os.File
}

func (l *lockedFile) write(p []byte) {
	l.mu.Lock()
	_, _ = l.f.Write(p)
	l.mu.Unlock()
}

func (l *lockedFile) close() {
	l.mu.Lock()
	_ = l.f.Close()
	l.mu.Unlock()
}

// maxPartialLine bounds the buffer of an unterminated line; a longer line is
// written in pieces.
const maxPartialLine = 1 << 20

// lineTee collects a stream into whole lines and writes each, verbatim plus a
// newline, to the shared run log, so lines of the two streams never interleave
// mid-line.
type lineTee struct {
	out *lockedFile
	buf []byte
}

func newLineTee(out *lockedFile) *lineTee { return &lineTee{out: out} }

func (t *lineTee) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		t.out.write(t.buf[:i+1])
		t.buf = t.buf[i+1:]
	}
	if len(t.buf) > maxPartialLine {
		t.out.write(append(t.buf, '\n'))
		t.buf = nil
	}
	return len(p), nil
}

// flush writes a trailing unterminated line.
func (t *lineTee) flush() {
	if len(t.buf) > 0 {
		t.out.write(append(t.buf, '\n'))
		t.buf = nil
	}
}
