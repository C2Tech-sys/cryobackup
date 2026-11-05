package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// NameHealthProber is the health-prober job (architecture §5.7).
const NameHealthProber = "health-prober"

// The health-prober's job_state rows: one per probe, named after the job, so
// /system/jobs and the system-health page read every probe result in one place
// (architecture §4.6: "stored in job_state (last_status, last_error_class)").
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
)

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
	inflight      map[string]bool
	logged        map[string]string
	probes        sync.WaitGroup // the per-tick probes
	background    sync.WaitGroup // the integrity check and the lease renewer
	holderOnce    sync.Once
	pctx          context.Context // the probes' context while this process holds ProbeLease
	pcancel       context.CancelFunc
	renewing      bool
	idle          chan struct{} // wakes the renewer when the last probe ends
	renewEvery    time.Duration // overrides leaseTTL/3 (tests)
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

// claim marks a probe in flight; false when its previous run has not finished.
func (h *HealthProber) claim(row string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight == nil {
		h.inflight = map[string]bool{}
	}
	if h.inflight[row] {
		return false
	}
	h.inflight[row] = true
	return true
}

func (h *HealthProber) release(row string) {
	h.mu.Lock()
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
// ProbeLease: one per holding period, cancelled when the lease is lost.
func (h *HealthProber) probeContext(ctx context.Context) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pctx == nil || h.pctx.Err() != nil {
		h.pctx, h.pcancel = context.WithCancel(ctx)
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

// startRenewer renews ProbeLease while any probe is in flight (the background
// integrity check can outlive several ticks), and cancels the probes when the
// store says another process has it.
func (h *HealthProber) startRenewer(ctx context.Context) {
	h.mu.Lock()
	if h.renewing {
		h.mu.Unlock()
		return
	}
	h.renewing = true
	h.mu.Unlock()
	idle := h.idleC()
	every := h.renewEvery
	if every <= 0 {
		every = h.leaseTTL() / 3
	}
	h.background.Add(1)
	go func() {
		defer h.background.Done()
		defer func() { h.mu.Lock(); h.renewing = false; h.mu.Unlock() }()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			h.mu.Lock()
			busy := len(h.inflight) > 0
			h.mu.Unlock()
			if !busy {
				return // the next tick takes the lease again
			}
			select {
			case <-ctx.Done():
				return
			case <-idle:
				continue
			case <-t.C:
			}
			got, err := h.Store.AcquireJobLease(context.WithoutCancel(ctx), ProbeLease, h.holder(), h.leaseTTL())
			if err == nil && !got {
				h.loseLease()
				return
			}
		}
	}()
}

// Stop waits for every probe and releases ProbeLease (serve's shutdown).
func (h *HealthProber) Stop() {
	h.WaitAll()
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Store.ReleaseJobLease(rctx, ProbeLease, h.holder())
}

// safeProbe runs one probe and turns a panic into an internal_panic result for
// that probe alone: the other probes and the console keep running. The log
// carries the probe and the class, never the panic value or a stack.
func (h *HealthProber) safeProbe(ctx context.Context, row string, fn func(context.Context) result) (r result) {
	start := h.now()
	defer func() {
		if p := recover(); p != nil {
			r = result{row: row, status: "error", class: ClassPanic, started: start, finished: h.now()}
			h.log().Error("health probe panicked", logx.Str("probe", row), logx.Class(ClassPanic))
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
// for them (tests, shutdown).
func (h *HealthProber) Run(ctx context.Context) error {
	got, err := h.Store.AcquireJobLease(ctx, ProbeLease, h.holder(), h.leaseTTL())
	if err != nil {
		return err
	}
	if !got {
		h.loseLease() // another console probes; nothing runs here
		return nil
	}
	pctx := h.probeContext(ctx)
	defer h.startRenewer(ctx)
	for _, p := range []struct {
		row string
		fn  func(context.Context) result
	}{
		{ProbeRowGateway, h.probeGateway}, {ProbeRowStorage, h.probeStorage}, {ProbeRowSMTP, h.probeSMTP},
		{ProbeRowMaintain, h.probeMaintain}, {ProbeRowDatabase, h.probeDatabase}, {ProbeRowMasterKey, h.probeMasterKey},
		{ProbeRowEndpoint, h.mirrorEndpoint},
	} {
		if !h.claim(p.row) {
			continue // still running: only its own facts age
		}
		h.probes.Add(1)
		go func() {
			defer h.probes.Done()
			defer h.release(p.row)
			h.finish(pctx, h.safeProbe(pctx, p.row, p.fn))
		}()
	}
	h.mu.Lock()
	due := !h.now().Before(h.integrityNext)
	h.mu.Unlock()
	if due && h.claim(ProbeRowIntegrity) {
		h.background.Add(1)
		go func() {
			defer h.background.Done()
			defer h.release(ProbeRowIntegrity)
			h.finish(pctx, h.safeProbe(pctx, ProbeRowIntegrity, h.probeIntegrity))
		}()
	}
	return ctx.Err()
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
		h.log().Error("health probe state not recorded", logx.Str("probe", r.row), logx.Class(ClassStore))
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
			h.log().Info("health probe passing", logx.Str("probe", r.row))
		}
	case "skipped":
		h.log().Warn("health probe not checked", logx.Str("probe", r.row), logx.Class(r.class))
	default:
		h.log().Warn("health probe failing", logx.Str("probe", r.row), logx.Class(r.class))
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
// probe could not reach the worker at all (down, shutting down, busy), which is
// "not checked" and C07's to report, never a storage pass.
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
		case adminpath.CodeUnavailable, adminpath.CodeBusy:
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
