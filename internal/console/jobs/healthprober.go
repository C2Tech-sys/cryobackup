package jobs

import (
	"context"
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
// Only the lease holder of health-prober writes them.
const (
	ProbeRowGateway   = NameHealthProber + ".gateway"
	ProbeRowStorage   = NameHealthProber + ".storage"
	ProbeRowSMTP      = NameHealthProber + ".smtp"
	ProbeRowMaintain  = NameHealthProber + ".maintain-rpc"
	ProbeRowDatabase  = NameHealthProber + ".database"
	ProbeRowMasterKey = NameHealthProber + ".master-key"
	ProbeRowEndpoint  = NameHealthProber + ".agent-endpoint"
)

// Bounds of the health-prober (every one well inside its 60 s period, except the
// integrity check, which runs at start-up and weekly).
const (
	// DefaultProberInterval is the §5.7 period.
	DefaultProberInterval = state.DefaultProberInterval
	// MaintainPingTimeout bounds the maintain RPC ping (the adminpath ping timeout).
	MaintainPingTimeout = 5 * time.Second
	// StorageProbeTimeout bounds the storage probe (restic cat config through the maintain RPC).
	StorageProbeTimeout = 45 * time.Second
	// IntegrityEvery is how often PRAGMA integrity_check runs after the start-up check.
	IntegrityEvery = 7 * 24 * time.Hour
	// IntegrityTimeout bounds one integrity check.
	IntegrityTimeout = 5 * time.Minute
	// DBMeasureTimeout bounds the escrow key query.
	DBMeasureTimeout = 10 * time.Second
)

// Not-configured reasons, worded for the "not checked" banners (state ProbeRun.Unconfigured).
const (
	unconfGatewayURL  = "gateway_url is not set"
	unconfMasterKey   = "the master key is not loaded, so the probe login has no password"
	unconfSMTP        = "SMTP is not configured"
	unconfNoRepoProbe = "no device repository with an escrowed key exists yet to probe"
)

// HealthProber is the health-prober job (architecture §4.6, §5.7): every 60 s,
// under its lease, it probes the gateway (TLS and an authenticated GET of
// /probe/config as the reserved probe login, with the certificate's notAfter),
// storage (restic cat config on a device repository through the maintain RPC,
// rotating over the fleet), SMTP (connect and EHLO, no mail; not configured is
// the C03b warning), the maintain RPC (ping), the console database (integrity
// at start-up and weekly, free disk and WAL size every run) and the master key
// (loaded, and its generation against the active escrows). It mirrors the
// agent-endpoint self-probe (internal/console/probe, its own 3-minute timer,
// ADR 0013) into job_state so every probe result is in one place.
//
// Results live in memory (Facts) and in one job_state row per probe. Logs are
// path-free and token-free: a probe name and a class, only on a change. A probe
// that cannot run is "not checked" (Unconfigured), never healthy and never a
// failure of the thing it would have probed. Run never blocks start-up: the
// runner starts it after the listeners are bound.
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
	// Clock is the console clock; nil means time.Now.
	Clock func() time.Time
	Log   *logx.Logger
	// Started is when this console process started (ProbeFacts.StartedAt).
	Started time.Time

	mu           sync.Mutex
	gw, st, sm   state.ProbeRun
	rpc          state.ProbeRun
	db           state.DatabaseFacts
	escrowIDs    []string
	integrityAt  time.Time
	storageAfter string
	logged       map[string]string
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

// Job returns the schedule for the runner. The lease outlives one run's longest
// bound (the integrity check) so a second console cannot probe concurrently.
func (h *HealthProber) Job() Job {
	return Job{Name: NameHealthProber, Every: h.interval(), Lease: IntegrityTimeout + time.Minute, Run: h.Run}
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

// Run is one health-prober pass: every probe concurrently, each under its own
// bound, then one job_state row per probe. It returns an error only when the
// rows cannot be written; a failing probe is a result, not a job failure.
func (h *HealthProber) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	results := make(chan result, 8)
	for _, p := range []func(context.Context) result{h.probeGateway, h.probeStorage, h.probeSMTP, h.probeMaintain, h.probeDatabase, h.probeMasterKey, h.mirrorEndpoint} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := p(ctx); r.row != "" {
				results <- r
			}
		}()
	}
	wg.Wait()
	close(results)
	var errs []error
	for r := range results {
		h.logChange(r)
		if r.finished.Before(r.started) {
			r.finished = r.started
		}
		if err := h.Store.RecordJobRun(context.WithoutCancel(ctx), r.row, store.JobRun{Status: r.status, ErrorClass: r.class,
			StartedAt: r.started, FinishedAt: r.finished}); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return &ClassError{Class: ClassStore, Err: errors.Join(errs...)}
	}
	return ctx.Err()
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

// storageVerdict classifies a cat config answer for the storage probe: reached
// is true when restic got to the repository (success, or an answer only the
// repository could give: missing, wrong key, locked); probed is false when the
// probe could not run at all (the worker is down or refused the device), which
// is "not checked", never a storage failure.
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
		case adminpath.CodeRepoMissing, adminpath.CodeWrongPassword, adminpath.CodeLocked:
			return true, true, ""
		case adminpath.CodeStorageUnreachable, adminpath.CodeTimeout, adminpath.CodeResticFailed:
			c := ae.Code
			if ae.Class != "" {
				c = string(ae.Class)
			}
			return false, true, c
		}
		return false, false, ae.Code
	}
	return false, false, "internal"
}

func (h *HealthProber) probeStorage(ctx context.Context) result {
	start := h.now()
	h.mu.Lock()
	after := h.storageAfter
	h.mu.Unlock()
	dev, err := h.Store.NextStorageProbeDevice(ctx, after)
	if err != nil {
		return result{row: ProbeRowStorage, status: "skipped", class: ClassStore, started: start, finished: h.now()}
	}
	if dev == "" {
		h.mu.Lock()
		h.st = state.ProbeRun{Unconfigured: unconfNoRepoProbe}
		h.mu.Unlock()
		return result{row: ProbeRowStorage, status: "skipped", class: "no_repository", started: start, finished: start}
	}
	cctx, cancel := context.WithTimeout(ctx, StorageProbeTimeout)
	_, err = h.Backend.CatConfig(cctx, adminpath.DeviceRequest{DeviceID: dev})
	cancel()
	end := h.now()
	if ctx.Err() != nil {
		return result{}
	}
	reached, probed, class := storageVerdict(err)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.storageAfter = dev
	if !probed {
		// Not a run: the newest real result stays and ages into "not checked" (C02b); C07 names the worker.
		if h.st.Unconfigured != "" {
			h.st = state.ProbeRun{}
		}
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

func (h *HealthProber) probeDatabase(ctx context.Context) result {
	start := h.now()
	h.mu.Lock()
	due := h.integrityAt.IsZero() || start.Sub(h.integrityAt) >= IntegrityEvery
	h.mu.Unlock()
	var integrityOK, integrityRan bool
	var problems []string
	if due {
		ictx, cancel := context.WithTimeout(ctx, IntegrityTimeout)
		res, err := h.Store.IntegrityCheck(ictx)
		cancel()
		if ctx.Err() != nil {
			return result{}
		}
		if err == nil {
			integrityRan, integrityOK = true, res == "ok"
		} else {
			problems = append(problems, "integrity_unavailable")
		}
	}
	free, total, derr := probe.DiskSpace(filepath.Dir(h.Store.Path()))
	var wal int64
	walErr := error(nil)
	if fi, err := os.Stat(h.Store.Path() + "-wal"); err == nil {
		wal = fi.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		walErr = err // a WAL that exists but cannot be read is not "0 bytes"
	}
	end := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if integrityRan {
		h.integrityAt = end
		h.db.IntegrityCheckedAt, h.db.IntegrityOK = ms(end), integrityOK
		if !integrityOK {
			problems = append(problems, "integrity_failed")
		}
	} else if due {
		// The check could not run: retry next pass, and keep the previous result ageing towards C11.
		h.integrityAt = time.Time{}
	}
	if derr == nil && walErr == nil {
		h.db.MeasuredAt, h.db.FreeBytes, h.db.TotalBytes, h.db.WALBytes = ms(end), free, total, wal
	} else {
		problems = append(problems, "disk_unmeasured")
	}
	if len(problems) == 0 {
		return result{row: ProbeRowDatabase, status: "ok", started: start, finished: end}
	}
	return result{row: ProbeRowDatabase, status: "error", class: problems[0], started: start, finished: end}
}

func (h *HealthProber) probeMasterKey(ctx context.Context) result {
	start := h.now()
	cctx, cancel := context.WithTimeout(ctx, DBMeasureTimeout)
	ids, err := h.Store.ActiveEscrowKeyIDs(cctx)
	cancel()
	end := h.now()
	if err != nil {
		if ctx.Err() != nil {
			return result{}
		}
		return result{row: ProbeRowMasterKey, status: "error", class: ClassStore, started: start, finished: end}
	}
	h.mu.Lock()
	h.escrowIDs = ids
	h.mu.Unlock()
	mk := h.masterKeyFacts(ids)
	switch {
	case !mk.Loaded:
		return result{row: ProbeRowMasterKey, status: "error", class: ClassMasterKey, started: start, finished: end}
	default:
		for _, id := range ids {
			if id != mk.KeyID {
				return result{row: ProbeRowMasterKey, status: "error", class: "generation_mismatch", started: start, finished: end}
			}
		}
	}
	return result{row: ProbeRowMasterKey, status: "ok", started: start, finished: end}
}

func (h *HealthProber) masterKeyFacts(ids []string) state.MasterKeyFacts {
	mk := state.MasterKeyFacts{Loaded: h.Master.Loaded(), EscrowKeyIDs: append([]string(nil), ids...)}
	if mk.Loaded {
		mk.KeyID = h.Master.KeyID()
	}
	return mk
}

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
// zero value, which the engine never reads as healthy. The job-derived checks
// (alarm-evaluator, maintenance scheduler) and the later-milestone markers are
// added by internal/console/health, which assembles the World.
func (h *HealthProber) Facts() state.ProbeFacts {
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
	mk := h.masterKeyFacts(h.escrowIDs)
	return state.ProbeFacts{
		Wired:          true,
		StartedAt:      ms(h.Started),
		ProberInterval: h.interval(),
		Gateway:        &gw,
		Storage:        &st,
		SMTP:           &state.SMTPFacts{Probe: sm},
		Maintain:       &state.MaintainFacts{RPC: h.rpc},
		Database:       &db,
		MasterKey:      &mk,
	}
}

// MaintainRPC is the newest maintain RPC probe (the C07 input), for callers
// that show the worker's reachability outside the banner (the UI).
func (h *HealthProber) MaintainRPC() state.ProbeRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rpc
}
