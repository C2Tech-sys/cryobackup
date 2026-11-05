package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// NameHealthProber is the health-prober job (architecture §5.7).
const NameHealthProber = "health-prober"

// The health-prober's job_state rows: one per probe, named after the job
// (architecture §4.6: "stored in job_state (last_status, last_error_class)").
// They are probe results, not jobs: the job listings (/healthz, GET
// /api/v1/system/jobs) leave them and ProbeLease out (store.IsJobRow).
const (
	ProbeRowGateway   = NameHealthProber + ".gateway"
	ProbeRowStorage   = NameHealthProber + ".storage"
	ProbeRowSMTP      = NameHealthProber + ".smtp"
	ProbeRowMaintain  = NameHealthProber + ".maintain-rpc"
	ProbeRowDatabase  = NameHealthProber + ".database"
	ProbeRowIntegrity = NameHealthProber + ".integrity"
	ProbeRowMasterKey = NameHealthProber + ".master-key"
	ProbeRowEndpoint  = NameHealthProber + ".agent-endpoint"
)

// ProbeLease is the job_state lease that guards the probes themselves. The
// runner's health-prober lease only covers Run, which returns as soon as the
// probes are started; this one is held by the process whose probes are running,
// renewed while any of them (the background integrity check included) is in
// flight, and taken again each tick, so a second console never probes (it shows
// the not-checked warnings). Losing it cancels every probe in flight.
const ProbeLease = NameHealthProber + ".probes"

// Bounds and cadences of the health-prober. Every probe but the integrity check
// runs on each 60 s tick unless its previous run is still in flight, and each
// is bounded well inside the period; the integrity check runs in the background.
const (
	// DefaultProberInterval is the §5.7 period.
	DefaultProberInterval = state.DefaultProberInterval
	// MaintainPingTimeout bounds the maintain RPC ping (the adminpath ping timeout).
	MaintainPingTimeout = 5 * time.Second
	// StorageProbeTimeout bounds the storage probe (list + cat config on the probe repository, ADR 0017).
	StorageProbeTimeout = 45 * time.Second
	// IntegrityEvery is how often PRAGMA integrity_check runs after a passing check (start-up, then weekly).
	IntegrityEvery = 7 * 24 * time.Hour
	// IntegrityRetryAfter is the back-off after a check that failed, errored or timed out: a day, not the
	// next tick, so a database too large for IntegrityTimeout is not scanned every minute.
	IntegrityRetryAfter = 24 * time.Hour
	// IntegrityTimeout bounds one integrity check. It runs in one read transaction whose context ends at
	// this deadline (the SQLite driver interrupts the statement), so it cannot hold back WAL checkpoints
	// for longer.
	IntegrityTimeout = 5 * time.Minute
	// DBMeasureTimeout bounds the escrow key query.
	DBMeasureTimeout = 10 * time.Second
	// StopWait bounds how long Stop waits for the probes in flight after cancelling them. A probe that
	// has not ended by then (a hung disk under the database probe, whose disk calls take no context) is
	// abandoned and logged by name, so a stuck probe never holds serve's shutdown.
	StopWait = 10 * time.Second
	// StopReleaseTimeout bounds the write that releases ProbeLease at shutdown.
	StopReleaseTimeout = 5 * time.Second
)

// errProbesCancelled is ProbeNow's answer when the probes it asked for ended
// without a result, so ran is false: the lease was lost, or Stop began (or had
// finished). It wraps context.Canceled because that is what happened to them.
var errProbesCancelled = fmt.Errorf("jobs: the probes were cancelled before they finished: %w", context.Canceled)

// ErrNothingProbed is ProbeNow's answer when the gateway and storage probes both
// ended skipped, so ran is false: neither could run (the gateway is not
// configured, the worker cannot be reached or is busy). Nothing was cancelled and
// nothing failed; there is simply no result. It is not a context error.
var ErrNothingProbed = errors.New("jobs: nothing was probed: neither the gateway nor the storage probe could run")

// ErrProbePanicked is ProbeNow's answer when a probe it waited for panicked: an
// internal failure, so ran is false. The probe's job row says internal_panic. It is
// not a context error: nothing was cancelled.
var ErrProbePanicked = errors.New("jobs: a health probe panicked")

// ClassWorkerStore is the storage probe row's class when the maintenance
// worker's own settings store failed during the probe (maintain.ProbeRecordClass
// on an unavailable answer): for storage it is not checked, never a failure. It
// is a fact about the database instead: two in a row raise C11 critical
// (state.DatabaseFacts.WorkerStoreFailures).
const ClassWorkerStore = "worker_store"

// Not-configured reasons, worded for the "not checked" banners (state ProbeRun.Unconfigured).
const (
	unconfGatewayURL = "gateway_url is not set"
	unconfMasterKey  = "the master key is not loaded, so the probe login has no password"
	unconfSMTP       = "SMTP is not configured"
)

// HealthProber is the health-prober job (architecture §4.6, §5.7). Each tick,
// under its lease, it starts every probe whose previous run has finished: the
// gateway (TLS and an authenticated GET of /probe/config as the reserved probe
// login, with the certificate's notAfter), storage (list + cat config on the
// probe repository through the maintain RPC, ADR 0017), SMTP (connect and EHLO,
// no mail; not configured is the C03b warning), the maintain RPC (ping), the
// console database's free disk and WAL size, and the master key (loaded, and
// its generation against the active escrows). PRAGMA integrity_check runs in
// the background at start-up and weekly. The agent-endpoint self-probe keeps
// its own 3-minute timer (ADR 0013) and is mirrored into job_state here.
//
// The probes do not wait for each other: a slow one only ages its own facts.
// Results live in memory (Facts) and in one job_state row per probe. Logs are
// path-free and token-free: a probe name and a class, only on a change. A probe
// that cannot run is "not checked" (Unconfigured), never healthy and never a
// failure of the thing it would have probed.
type HealthProber struct {
	Store   *store.Store
	Backend adminpath.Backend
	Master  *escrow.MasterKey
	// Gateway is the gateway probe's settings; an empty URL is "not configured".
	Gateway probe.GatewaySettings
	// GatewayUnusable, when set, says why the gateway probe cannot run although a URL is set (an
	// unreadable health_prober.gateway_ca_file): it is the not-checked reason.
	GatewayUnusable string
	// SMTP is the SMTP probe's settings; an empty host is "not configured".
	SMTP probe.SMTPSettings
	// Endpoint, when set, is the agent-endpoint self-probe whose status is mirrored into job_state.
	Endpoint func() probe.Status
	// Interval is the period; zero means DefaultProberInterval.
	Interval time.Duration
	// StorageTimeout bounds the storage probe; zero means StorageProbeTimeout.
	StorageTimeout time.Duration
	// IntegrityCheck runs the integrity check; nil means Store.IntegrityCheck.
	IntegrityCheck func(ctx context.Context) (string, error)
	// IntegrityTimeout bounds it; zero means the IntegrityTimeout constant.
	IntegrityTimeout time.Duration
	// EscrowKeyIDs reads the active escrows' master key ids; nil means Store.ActiveEscrowKeyIDs.
	EscrowKeyIDs func(ctx context.Context) ([]string, error)
	// Clock is the console clock; nil means time.Now.
	Clock func() time.Time
	Log   *logx.Logger
	// Started is when this console process started (ProbeFacts.StartedAt).
	Started time.Time
	// Holder identifies this process in the ProbeLease (the runner's holder id); empty means a random one.
	Holder string

	mu            sync.Mutex
	gw, st, sm    state.ProbeRun
	rpc           state.ProbeRun
	db            state.DatabaseFacts
	escrowIDs     []string
	escrowReadAt  time.Time // zero: the escrow key ids are not known (never read, or the newest read failed)
	integrityNext time.Time
	inflight      map[string]*probeRun // a probe in flight
	logged        map[string]string
	probes        sync.WaitGroup // the per-tick probes
	background    sync.WaitGroup // the integrity check and the lease renewer
	holderOnce    sync.Once
	pctx          context.Context // the probes' context while this process holds ProbeLease
	pcancel       context.CancelFunc
	life          context.Context // this prober's lifetime: cancelled by Stop
	lifeCancel    context.CancelFunc
	stopped       bool           // Stop has begun: nothing takes the lease or starts a probe any more
	entries       sync.WaitGroup // Run and ProbeNow calls taking the lease and starting probes; Stop waits for them
	leaseUntil    time.Time      // when the lease this process last took or renewed runs out
	renewing      bool
	idle          chan struct{} // wakes the renewer when the last probe ends
	renewEvery    time.Duration // overrides renewInterval (tests)
	stopWait      time.Duration // overrides StopWait (tests)
}

func (h *HealthProber) interval() time.Duration {
	if h.Interval > 0 {
		return h.Interval
	}
	return DefaultProberInterval
}

func (h *HealthProber) now() time.Time {
	if h.Clock != nil {
		return h.Clock().UTC()
	}
	return time.Now().UTC()
}

func (h *HealthProber) log() *logx.Logger {
	if h.Log != nil {
		return h.Log
	}
	return logx.Default()
}

// holder is this process's ProbeLease holder id.
func (h *HealthProber) holder() string {
	h.holderOnce.Do(func() {
		if h.Holder == "" {
			var b [6]byte
			_, _ = rand.Read(b[:])
			h.Holder = "prober-" + hex.EncodeToString(b[:])
		}
	})
	return h.Holder
}

// leaseTTL outlives three ticks, so a missed tick never drops the lease of a live prober.
func (h *HealthProber) leaseTTL() time.Duration { return 3 * h.interval() }

// renewInterval is how often the renewer renews ProbeLease while a probe is in
// flight: a third of the lease, so two renewals can be missed before the lease
// lapses under a running probe.
func (h *HealthProber) renewInterval() time.Duration {
	if h.renewEvery > 0 {
		return h.renewEvery
	}
	return h.leaseTTL() / 3
}

// lifetime is this prober's own context, cancelled by Stop. ProbeNow starts its
// probes under it, never under the caller's request.
func (h *HealthProber) lifetime() context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.life == nil {
		h.life, h.lifeCancel = context.WithCancel(context.Background())
	}
	return h.life
}

// Job returns the schedule for the runner.
func (h *HealthProber) Job() Job {
	return Job{Name: NameHealthProber, Every: h.interval(), Lease: 2 * h.interval(), Run: h.Run}
}

func ms(t time.Time) state.Millis {
	if t.IsZero() {
		return 0
	}
	return state.Millis(t.UnixMilli())
}

// record folds one probe outcome into r: ok resets the failure streak, a
// failure extends it; unconfigured replaces the run (the probe could not run).
func record(r *state.ProbeRun, at time.Time, ok bool, class string, notAfter time.Time) {
	r.Unconfigured = ""
	r.LastRunAt = ms(at)
	if ok {
		r.LastOKAt, r.ConsecutiveFailures, r.LastErrorClass = ms(at), 0, ""
	} else {
		r.ConsecutiveFailures++
		r.LastErrorClass = class
	}
	if !notAfter.IsZero() {
		r.CertNotAfter = ms(notAfter)
	} else if ok {
		r.CertNotAfter = 0 // a passing run without a handshake (plain http) recorded no expiry
	}
}

// result is one probe's job_state row and log line.
type result struct {
	row, status, class string
	started, finished  time.Time
}

// probeRun is one probe in flight. done closes once it has ended and written its
// facts and its row. ran and skipped are set before done closes. ran is false when
// it was cancelled or panicked, so nothing was learned; skipped is true when it
// could not run at all (status "skipped"), so it probed nothing either.
type probeRun struct {
	done                   chan struct{}
	ran, skipped, panicked bool
}

// probed reports whether r is a probe's result, not a cancellation or a panic.
func (r result) probed() bool { return r.row != "" && r.class != ClassPanic }

// claimOrWait marks a probe in flight and returns claimed true or, when its
// previous run has not finished, returns that run and claimed false.
func (h *HealthProber) claimOrWait(row string) (run *probeRun, claimed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight == nil {
		h.inflight = map[string]*probeRun{}
	}
	if r, busy := h.inflight[row]; busy {
		return r, false
	}
	r := &probeRun{done: make(chan struct{})}
	h.inflight[row] = r
	return r, true
}

// claim marks a probe in flight; false when its previous run has not finished.
func (h *HealthProber) claim(row string) bool {
	_, claimed := h.claimOrWait(row)
	return claimed
}

func (h *HealthProber) release(row string, res result) {
	h.mu.Lock()
	if r, ok := h.inflight[row]; ok {
		r.ran, r.skipped, r.panicked = res.probed(), res.status == "skipped", res.class == ClassPanic
		close(r.done)
	}
	delete(h.inflight, row)
	idle := len(h.inflight) == 0
	h.mu.Unlock()
	if idle {
		select {
		case h.idleC() <- struct{}{}:
		default:
		}
	}
}

func (h *HealthProber) idleC() chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.idle == nil {
		h.idle = make(chan struct{}, 1)
	}
	return h.idle
}

// probeContext is the context the probes run under while this process holds
// ProbeLease: one per holding period, cancelled when the lease is lost or by
// Stop. It is derived from the prober's own lifetime only, never from a
// caller's context (a request, one Run, a run-now): a caller that ends must not
// cancel probes in flight, nor leave a cancelled context to a later tick. The
// caller's context only bounds how long it waits (ProbeNow) or how long it
// takes the lease (Run).
func (h *HealthProber) probeContext() context.Context {
	life := h.lifetime()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pctx == nil || h.pctx.Err() != nil {
		h.pctx, h.pcancel = context.WithCancel(life)
	}
	return h.pctx
}

// loseLease cancels every probe in flight: another process holds ProbeLease.
func (h *HealthProber) loseLease() {
	h.mu.Lock()
	cancel := h.pcancel
	h.pctx, h.pcancel = nil, nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
		h.log().Warn("health probe lease lost; probes in flight cancelled", logx.Class("lease_lost"))
	}
}

// enter admits one Run or ProbeNow, which Stop then waits for before it releases
// the lease. False once Stop has begun: nothing may take the lease after that.
func (h *HealthProber) enter() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return false
	}
	h.entries.Add(1)
	return true
}

// acquire takes or renews ProbeLease and notes how long it now lasts. A Stop
// whose bounded wait gave up on this call has already released the lease, so a
// take that lands after it is undone and answered errProbesCancelled.
func (h *HealthProber) acquire(ctx context.Context) (got bool, err error) {
	asked := h.now()
	got, err = h.Store.AcquireJobLease(ctx, ProbeLease, h.holder(), h.leaseTTL())
	if err != nil || !got {
		return false, err
	}
	h.mu.Lock()
	stopped := h.stopped
	h.leaseUntil = asked.Add(h.leaseTTL())
	h.mu.Unlock()
	if stopped {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopReleaseTimeout)
		defer cancel()
		if err := h.Store.ReleaseJobLease(rctx, ProbeLease, h.holder()); err != nil {
			h.log().Warn("health probe lease not released at shutdown; it expires on its own", logx.Class(ClassStore))
		}
		return false, errProbesCancelled
	}
	return true, nil
}

// startRenewer renews ProbeLease while any probe is in flight (the background
// integrity check can outlive several ticks), and cancels the probes when the
// store says another process has it, or when store errors have run past the end
// of the lease: by then another console may hold it, and nothing here can tell.
// Like the probes, it lives under the prober's lifetime (Stop ends it), never
// under a caller's context.
func (h *HealthProber) startRenewer() {
	life := h.lifetime()
	every := h.renewInterval()
	h.mu.Lock()
	if h.renewing {
		h.mu.Unlock()
		return
	}
	h.renewing = true
	h.mu.Unlock()
	idle := h.idleC()
	h.background.Add(1)
	go func() {
		defer h.background.Done()
		defer func() { h.mu.Lock(); h.renewing = false; h.mu.Unlock() }()
		t := time.NewTicker(every)
		defer t.Stop()
		failing := false
		for {
			h.mu.Lock()
			busy := len(h.inflight) > 0
			h.mu.Unlock()
			if !busy {
				return // the next tick takes the lease again
			}
			select {
			case <-life.Done():
				return // Stop: the lease is released and never renewed again
			case <-idle:
				continue
			case <-t.C:
			}
			if life.Err() != nil {
				return // both were ready: Stop wins
			}
			// Bounded, so a store that hangs counts against the lease as an error does.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(life), h.leaseTTL()/3)
			got, err := h.acquire(rctx)
			cancel()
			switch {
			case errors.Is(err, errProbesCancelled):
				return
			case err != nil:
				if !failing {
					failing = true
					h.log().Warn("health probe lease not renewed", logx.Class(ClassStore))
				}
				h.mu.Lock()
				expired := !h.now().Before(h.leaseUntil)
				h.mu.Unlock()
				if expired {
					h.loseLease()
					return
				}
			case !got:
				h.loseLease()
				return
			default:
				failing = false
			}
		}
	}()
}

// Stop is serve's shutdown of the prober: it refuses every later Run and
// ProbeNow, cancels every probe in flight (they all run under the prober's
// lifetime, never under a caller's or the runner's context), waits at most
// StopWait for them and for the calls already taking the lease, logs each probe
// it abandons (by name, never a path), and releases ProbeLease so another console
// can take over at once. A probe that ignores its context (the database probe's
// disk calls take none) cannot hold the shutdown.
func (h *HealthProber) Stop() {
	h.lifetime() // a prober that never ran has a lifetime to end too
	h.mu.Lock()
	h.stopped = true
	cancel, endLife := h.pcancel, h.lifeCancel
	h.pctx, h.pcancel = nil, nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	endLife()
	wait := h.stopWait
	if wait <= 0 {
		wait = StopWait
	}
	done := make(chan struct{})
	go func() { h.entries.Wait(); h.WaitAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		for _, row := range h.inFlight() {
			h.log().Warn("health probe abandoned at shutdown", probeAttr(row))
		}
	}
	rctx, rcancel := context.WithTimeout(context.Background(), StopReleaseTimeout)
	defer rcancel()
	if err := h.Store.ReleaseJobLease(rctx, ProbeLease, h.holder()); err != nil {
		h.log().Warn("health probe lease not released at shutdown; it expires on its own", logx.Class(ClassStore))
	}
}

// inFlight lists the probes in flight, sorted.
func (h *HealthProber) inFlight() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.inflight))
	for row := range h.inflight {
		out = append(out, row)
	}
	slices.Sort(out)
	return out
}

// probeAttr names a probe in a log line by its short name ("gateway",
// "maintain-rpc"): the row name's dot before a letter is outside logx's token
// alphabet, so logging the row itself printed "invalid".
func probeAttr(row string) logx.Attr {
	return logx.Str("probe", strings.TrimPrefix(row, NameHealthProber+"."))
}

// safeProbe runs one probe and turns a panic into an internal_panic result for
// that probe alone: the other probes and the console keep running. The log
// carries the probe and the class, never the panic value or a stack.
func (h *HealthProber) safeProbe(ctx context.Context, row string, fn func(context.Context) result) (r result) {
	start := h.now()
	defer func() {
		if p := recover(); p != nil {
			r = result{row: row, status: "error", class: ClassPanic, started: start, finished: h.now()}
			h.log().Error("health probe panicked", probeAttr(row), logx.Class(ClassPanic))
		}
	}()
	return fn(ctx)
}

// Run is one health-prober tick. It takes (or renews) ProbeLease; without it
// nothing is probed on this console. Holding it, it starts every probe that is
// not still in flight from an earlier tick, each in its own goroutine under its
// own bound and the lease's context, and the integrity check when it is due,
// and returns without waiting for them: a slow probe only ages its own facts.
// Each probe records its own job_state row when it ends. Wait and WaitAll wait
// for them (tests, shutdown). After Stop it does nothing and answers errDidNotRun:
// a late run-now must not take the lease back, or it would block a takeover for up
// to one TTL, and the runner must not record a tick that never happened.
func (h *HealthProber) Run(ctx context.Context) error {
	if !h.enter() {
		return errDidNotRun
	}
	defer h.entries.Done()
	got, err := h.acquire(ctx)
	if errors.Is(err, errProbesCancelled) {
		return errDidNotRun
	}
	if err != nil {
		return err
	}
	if !got {
		h.loseLease() // another console probes; nothing runs here
		return nil
	}
	pctx := h.probeContext() // the prober's, not ctx: Run returns before its probes end
	defer h.startRenewer()
	for _, p := range []struct {
		row string
		fn  func(context.Context) result
	}{
		{ProbeRowGateway, h.probeGateway}, {ProbeRowStorage, h.probeStorage}, {ProbeRowSMTP, h.probeSMTP},
		{ProbeRowMaintain, h.probeMaintain}, {ProbeRowDatabase, h.probeDatabase}, {ProbeRowMasterKey, h.probeMasterKey},
		{ProbeRowEndpoint, h.mirrorEndpoint},
	} {
		h.start(pctx, p.row, p.fn) // one still running from an earlier tick is not doubled: only its own facts age
	}
	h.mu.Lock()
	due := !h.now().Before(h.integrityNext)
	h.mu.Unlock()
	if due && h.claim(ProbeRowIntegrity) {
		h.background.Add(1)
		go func() {
			defer h.background.Done()
			var r result
			defer func() { h.release(ProbeRowIntegrity, r) }()
			r = h.safeProbe(pctx, ProbeRowIntegrity, h.probeIntegrity)
			h.finish(pctx, r)
		}()
	}
	return ctx.Err()
}

// start runs one per-tick probe in its own goroutine under pctx, unless its
// previous run is still in flight. It returns the run in flight (this one or
// the earlier one), whose done closes when it has ended and written its facts
// and its row.
func (h *HealthProber) start(pctx context.Context, row string, fn func(context.Context) result) *probeRun {
	run, claimed := h.claimOrWait(row)
	if !claimed {
		return run
	}
	h.probes.Add(1)
	go func() {
		defer h.probes.Done()
		var r result
		defer func() { h.release(row, r) }() // after finish: done closes once the facts and the row are written
		r = h.safeProbe(pctx, row, fn)
		h.finish(pctx, r)
	}()
	return run
}

// ProbeNow is the on-demand probe (POST /api/v1/storage-targets/{id}/probe).
// It takes ProbeLease like a tick; when another console holds it nothing is
// probed here and the answer is (false, nil). Holding it, it starts the gateway
// probe (which also reads the gateway certificate's expiry: the certificates
// check) and the storage probe, and waits until both have ended and written
// their facts, bounded by ctx. A probe already in flight from a tick is waited
// for, not started twice. The probes run under the lease's context, not ctx: a
// caller that gives up stops waiting (true, ctx.Err()), it never cuts a probe
// short (whose result still lands in Facts). The SMTP, maintain RPC, database,
// master-key and integrity probes keep their own schedule.
//
// ran is true only when the probes ran or were left running. Probes that ended
// without a result (the lease was lost mid-wait, Stop began or had finished, or the
// run waited for was cancelled under an earlier lease) answer (false,
// errProbesCancelled); a probe that panicked answers (false, ErrProbePanicked). Both probes skipped answer (false, ErrNothingProbed);
// one skipped beside one that really ran is a run, and its check says not checked.
func (h *HealthProber) ProbeNow(ctx context.Context) (ran bool, err error) {
	pctx, runs, err := h.startProbeNow(ctx)
	if err != nil || runs == nil {
		return false, err
	}
	allSkipped := true
	for _, run := range runs {
		select {
		case <-run.done:
			if run.panicked {
				return false, ErrProbePanicked
			}
			if !run.ran {
				return false, errProbesCancelled
			}
			allSkipped = allSkipped && run.skipped
		case <-pctx.Done():
			return false, errProbesCancelled
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
	if allSkipped {
		return false, ErrNothingProbed
	}
	return true, nil
}

// startProbeNow is the part of ProbeNow that Stop waits for: it takes the lease
// and starts the probes. nil runs and a nil error: another console holds the lease.
func (h *HealthProber) startProbeNow(ctx context.Context) (context.Context, []*probeRun, error) {
	if !h.enter() {
		return nil, nil, errProbesCancelled
	}
	defer h.entries.Done()
	got, err := h.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !got {
		h.loseLease() // another console probes; nothing runs here
		return nil, nil, nil
	}
	pctx := h.probeContext() // the prober's, not ctx: ctx bounds only ProbeNow's wait
	runs := []*probeRun{h.start(pctx, ProbeRowGateway, h.probeGateway), h.start(pctx, ProbeRowStorage, h.probeStorage)}
	h.startRenewer()
	return pctx, runs, nil
}

// Wait waits for the per-tick probes started so far (not the integrity check).
func (h *HealthProber) Wait() { h.probes.Wait() }

// WaitAll waits for every probe, the integrity check included.
func (h *HealthProber) WaitAll() { h.probes.Wait(); h.background.Wait() }

// finish logs a change and writes the probe's job_state row.
func (h *HealthProber) finish(ctx context.Context, r result) {
	if r.row == "" {
		return
	}
	h.logChange(r)
	if r.finished.Before(r.started) {
		r.finished = r.started
	}
	if err := h.Store.RecordJobRun(context.WithoutCancel(ctx), r.row, store.JobRun{Status: r.status, ErrorClass: r.class,
		StartedAt: r.started, FinishedAt: r.finished}); err != nil {
		h.log().Error("health probe state not recorded", probeAttr(r.row), logx.Class(ClassStore))
	}
}

// logChange logs a probe's status only when it changes (path-free: the probe and its class).
func (h *HealthProber) logChange(r result) {
	key := r.status + ":" + r.class
	h.mu.Lock()
	if h.logged == nil {
		h.logged = map[string]string{}
	}
	prev, seen := h.logged[r.row]
	h.logged[r.row] = key
	h.mu.Unlock()
	if seen && prev == key {
		return
	}
	switch r.status {
	case "ok":
		if seen {
			h.log().Info("health probe passing", probeAttr(r.row))
		}
	case "skipped":
		h.log().Warn("health probe not checked", probeAttr(r.row), logx.Class(r.class))
	default:
		h.log().Warn("health probe failing", probeAttr(r.row), logx.Class(r.class))
	}
}

func statusOf(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}

// gatewayReason is why the gateway probe cannot run, "" when it can.
func (h *HealthProber) gatewayReason() string {
	switch {
	case h.Gateway.URL == "":
		return unconfGatewayURL
	case h.GatewayUnusable != "":
		return h.GatewayUnusable
	case !h.Master.Loaded():
		return unconfMasterKey
	}
	return ""
}

func (h *HealthProber) probeGateway(ctx context.Context) result {
	start := h.now()
	if reason := h.gatewayReason(); reason != "" {
		h.mu.Lock()
		h.gw = state.ProbeRun{Unconfigured: reason}
		h.mu.Unlock()
		return result{row: ProbeRowGateway, status: "skipped", class: "not_configured", started: start, finished: start}
	}
	pw, err := gateway.ProbePassword(h.Master.Bytes())
	if err != nil {
		h.mu.Lock()
		h.gw = state.ProbeRun{Unconfigured: unconfMasterKey}
		h.mu.Unlock()
		return result{row: ProbeRowGateway, status: "skipped", class: "master_key", started: start, finished: start}
	}
	gs := h.Gateway
	if gs.Clock == nil {
		gs.Clock = h.now
	}
	o := probe.CheckGateway(ctx, gs, gateway.ProbeUsername, pw)
	end := h.now()
	if ctx.Err() != nil {
		return result{} // shutdown: says nothing about the gateway
	}
	h.mu.Lock()
	record(&h.gw, end, o.OK, o.Class, o.NotAfter)
	h.mu.Unlock()
	return result{row: ProbeRowGateway, status: statusOf(o.OK), class: o.Class, started: start, finished: end}
}

// storageVerdict classifies the probe repository's answer (ADR 0017): only a
// clean list + cat config passes. repo_missing is a failure of its own class
// (the probe repository was created once and is gone: an emptied or re-pointed
// prefix), and so is every other restic verdict. probed is false only when the
// probe could not reach the worker at all (down, shutting down, busy), or the
// worker's own settings store failed during the probe (ClassWorkerStore), which
// is "not checked" (C02b, and C07 names a worker that is down), never a storage
// pass and never a storage failure.
func storageVerdict(err error) (reached, probed bool, class string) {
	if err == nil {
		return true, true, ""
	}
	var ae *adminpath.Error
	switch {
	case errors.Is(err, adminpath.ErrUnavailable):
		return false, false, "worker_unreachable"
	case errors.Is(err, context.DeadlineExceeded):
		return false, true, "timeout"
	case errors.As(err, &ae):
		switch ae.Code {
		case adminpath.CodeUnavailable:
			if ae.Class == maintain.ProbeRecordClass {
				return false, false, ClassWorkerStore
			}
			return false, false, ae.Code
		case adminpath.CodeBusy:
			return false, false, ae.Code
		case adminpath.CodeNotFound:
			return false, true, "probe_unsupported" // a worker that predates the probe repository
		case adminpath.CodeStorageUnreachable, adminpath.CodeResticFailed:
			if ae.Class != "" {
				return false, true, string(ae.Class)
			}
		}
		return false, true, ae.Code
	}
	return false, true, "internal"
}

func (h *HealthProber) probeStorage(ctx context.Context) result {
	start := h.now()
	timeout := h.StorageTimeout
	if timeout <= 0 {
		timeout = StorageProbeTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	_, err := h.Backend.CatConfig(cctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	cancel()
	end := h.now()
	if ctx.Err() != nil {
		return result{}
	}
	reached, probed, class := storageVerdict(err)
	h.mu.Lock()
	defer h.mu.Unlock()
	// The worker's own database reads, apart from storage (C11, operator ruling 2026-11-05): a failure
	// extends the streak, a clean probe (which read the settings row) ends it, and an answer that never
	// got as far as the database says nothing about it.
	switch {
	case class == ClassWorkerStore:
		if h.db.WorkerStoreFailures == 0 {
			h.db.WorkerStoreFailingSince = ms(end)
		}
		h.db.WorkerStoreFailures++
	case err == nil:
		h.db.WorkerStoreFailures, h.db.WorkerStoreFailingSince = 0, 0
	}
	if !probed {
		// Not a run: the newest real result stays and ages into "not checked" (C02b); C07 names the worker.
		return result{row: ProbeRowStorage, status: "skipped", class: class, started: start, finished: end}
	}
	record(&h.st, end, reached, class, time.Time{})
	return result{row: ProbeRowStorage, status: statusOf(reached), class: class, started: start, finished: end}
}

func (h *HealthProber) probeSMTP(ctx context.Context) result {
	start := h.now()
	if !h.SMTP.Configured() {
		h.mu.Lock()
		h.sm = state.ProbeRun{Unconfigured: unconfSMTP}
		h.mu.Unlock()
		return result{row: ProbeRowSMTP, status: "skipped", class: "not_configured", started: start, finished: start}
	}
	o := probe.CheckSMTP(ctx, h.SMTP)
	end := h.now()
	if ctx.Err() != nil {
		return result{}
	}
	h.mu.Lock()
	record(&h.sm, end, o.OK, o.Class, time.Time{})
	h.mu.Unlock()
	return result{row: ProbeRowSMTP, status: statusOf(o.OK), class: o.Class, started: start, finished: end}
}

func (h *HealthProber) probeMaintain(ctx context.Context) result {
	start := h.now()
	cctx, cancel := context.WithTimeout(ctx, MaintainPingTimeout)
	_, err := h.Backend.Ping(cctx)
	cancel()
	end := h.now()
	if ctx.Err() != nil {
		return result{}
	}
	class := ""
	if err != nil {
		class = "unreachable"
		if errors.Is(err, context.DeadlineExceeded) {
			class = "timeout"
		}
	}
	h.mu.Lock()
	record(&h.rpc, end, err == nil, class, time.Time{})
	h.mu.Unlock()
	return result{row: ProbeRowMaintain, status: statusOf(err == nil), class: class, started: start, finished: end}
}

// probeDatabase measures free disk and the WAL size every tick (C11).
func (h *HealthProber) probeDatabase(context.Context) result {
	start := h.now()
	free, total, derr := probe.DiskSpace(filepath.Dir(h.Store.Path()))
	var wal int64
	walErr := error(nil)
	if fi, err := os.Stat(h.Store.Path() + "-wal"); err == nil {
		wal = fi.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		walErr = err // a WAL that exists but cannot be read is not "0 bytes"
	}
	end := h.now()
	if derr != nil || walErr != nil {
		return result{row: ProbeRowDatabase, status: "error", class: "disk_unmeasured", started: start, finished: end}
	}
	h.mu.Lock()
	h.db.MeasuredAt, h.db.FreeBytes, h.db.TotalBytes, h.db.WALBytes = ms(end), free, total, wal
	h.mu.Unlock()
	return result{row: ProbeRowDatabase, status: "ok", started: start, finished: end}
}

// probeIntegrity is PRAGMA integrity_check (C11), in the background: at
// start-up, then IntegrityEvery after a passing check, and IntegrityRetryAfter
// after a failing, erroring or timed-out one. A result other than "ok" is a
// failed check. A check that could not finish keeps the previous result, which
// ages into the "integrity not run for N days" cause.
func (h *HealthProber) probeIntegrity(ctx context.Context) result {
	start := h.now()
	check := h.IntegrityCheck
	if check == nil {
		check = h.Store.IntegrityCheck
	}
	timeout := h.IntegrityTimeout
	if timeout <= 0 {
		timeout = IntegrityTimeout
	}
	ictx, cancel := context.WithTimeout(ctx, timeout)
	res, err := check(ictx)
	cancel()
	end := h.now()
	if ctx.Err() != nil {
		return result{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.integrityNext = end.Add(IntegrityRetryAfter)
		class := "integrity_unavailable"
		if errors.Is(err, context.DeadlineExceeded) || ictx.Err() != nil {
			class = ClassTimeout
		}
		return result{row: ProbeRowIntegrity, status: "error", class: class, started: start, finished: end}
	}
	ok := res == "ok"
	h.db.IntegrityCheckedAt, h.db.IntegrityOK = ms(end), ok
	if !ok {
		h.integrityNext = end.Add(IntegrityRetryAfter)
		return result{row: ProbeRowIntegrity, status: "error", class: "integrity_failed", started: start, finished: end}
	}
	h.integrityNext = end.Add(IntegrityEvery)
	return result{row: ProbeRowIntegrity, status: "ok", started: start, finished: end}
}

func (h *HealthProber) probeMasterKey(ctx context.Context) result {
	start := h.now()
	read := h.EscrowKeyIDs
	if read == nil {
		read = h.Store.ActiveEscrowKeyIDs
	}
	cctx, cancel := context.WithTimeout(ctx, DBMeasureTimeout)
	ids, err := read(cctx)
	cancel()
	end := h.now()
	if err != nil {
		if ctx.Err() != nil {
			return result{}
		}
		// The comparison was not made: forget the previous ids, so Facts reports "not checked" (C12b).
		h.mu.Lock()
		h.escrowIDs, h.escrowReadAt = nil, time.Time{}
		h.mu.Unlock()
		return result{row: ProbeRowMasterKey, status: "error", class: ClassStore, started: start, finished: end}
	}
	h.mu.Lock()
	h.escrowIDs, h.escrowReadAt = ids, end
	h.mu.Unlock()
	if !h.Master.Loaded() {
		return result{row: ProbeRowMasterKey, status: "error", class: ClassMasterKey, started: start, finished: end}
	}
	for _, id := range ids {
		if id != h.Master.KeyID() {
			return result{row: ProbeRowMasterKey, status: "error", class: "generation_mismatch", started: start, finished: end}
		}
	}
	return result{row: ProbeRowMasterKey, status: "ok", started: start, finished: end}
}

// masterKeyFacts is the C12 input, or nil when the generation comparison is not
// known to be current: never read, the newest read failed, or the newest read
// is older than escrowFresh. nil is reported as C12b ("not checked"), never as
// "loaded, no mismatch". A key that is not loaded is C12 whatever the escrows say.
func (h *HealthProber) masterKeyFacts(now time.Time) *state.MasterKeyFacts {
	if !h.Master.Loaded() {
		return &state.MasterKeyFacts{}
	}
	if h.escrowReadAt.IsZero() || now.Sub(h.escrowReadAt) > h.escrowFresh() {
		return nil
	}
	return &state.MasterKeyFacts{Loaded: true, KeyID: h.Master.KeyID(), EscrowKeyIDs: append([]string(nil), h.escrowIDs...)}
}

// escrowFresh is how old the escrow key ids may be: one interval plus half of
// one for the runner's jitter and the query itself.
func (h *HealthProber) escrowFresh() time.Duration { return h.interval() + h.interval()/2 }

// mirrorEndpoint copies the agent-endpoint self-probe's status into job_state.
func (h *HealthProber) mirrorEndpoint(context.Context) result {
	if h.Endpoint == nil {
		return result{}
	}
	st := h.Endpoint()
	at := h.now()
	switch {
	case !st.Runs:
		return result{row: ProbeRowEndpoint, status: "skipped", class: st.Limit, started: at, finished: at}
	case !st.Known:
		return result{} // no run completed yet: nothing to mirror
	case st.Last.OK:
		return result{row: ProbeRowEndpoint, status: "ok", class: st.Limit, started: st.Last.At, finished: st.Last.At}
	}
	return result{row: ProbeRowEndpoint, status: "error", class: string(st.Last.Stage) + "_" + st.Last.Class, started: st.Last.At, finished: st.Last.At}
}

// Facts is the probe-derived part of state.ProbeFacts at this instant: Wired,
// the start time, the interval and the gateway, storage, SMTP, maintain RPC,
// database and master-key checks. A probe that has not completed yet is its
// zero value, which the engine never reads as healthy; MasterKey is nil (C12b)
// until a current generation comparison exists. The job-derived checks
// (alarm-evaluator, maintenance scheduler) and the later-milestone markers are
// added by internal/console/health, which assembles the World.
func (h *HealthProber) Facts() state.ProbeFacts {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	gw, st, sm, db := h.gw, h.st, h.sm, h.db
	// Not-configured is known before the first run: say so at once instead of "not completed yet".
	if gw.LastRunAt == 0 && gw.Unconfigured == "" {
		gw.Unconfigured = h.gatewayReason()
	}
	if sm.LastRunAt == 0 && sm.Unconfigured == "" && !h.SMTP.Configured() {
		sm.Unconfigured = unconfSMTP
	}
	return state.ProbeFacts{
		Wired:          true,
		StartedAt:      ms(h.Started),
		ProberInterval: h.interval(),
		Gateway:        &gw,
		Storage:        &st,
		SMTP:           &state.SMTPFacts{Probe: sm},
		Maintain:       &state.MaintainFacts{RPC: h.rpc},
		Database:       &db,
		MasterKey:      h.masterKeyFacts(now),
	}
}

// MaintainRPC is the newest maintain RPC probe (the C07 input), for callers
// that show the worker's reachability outside the banner (the UI).
func (h *HealthProber) MaintainRPC() state.ProbeRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rpc
}
