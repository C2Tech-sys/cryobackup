package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/commands"
	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/agent/secrets"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Default periods of the loops (architecture §5.1, §5.5, §6.1).
const (
	defaultHeartbeatEvery = 60 * time.Second
	heartbeatJitter       = 10 * time.Second
	defaultPollWait       = api.CommandsLongPollMax
	policySafetyEvery     = 15 * time.Minute
	outboxIdleEvery       = 2 * time.Minute
	forceNowTTL           = 10 * time.Minute
)

// ---- policy ---------------------------------------------------------------

// decodePolicy decodes and validates a stored or received policy response.
func decodePolicy(raw []byte) (api.PolicyResponse, error) {
	var resp api.PolicyResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, errors.New("policy response is not valid JSON")
	}
	if err := resp.Policy.Validate(); err != nil {
		return resp, err
	}
	return resp, nil
}

// applyPolicy validates a policy response and, when valid, makes it the
// effective policy: stored as last-known-good, a pending gateway password
// applied (DH13), the scheduler told. A policy that fails Validate is rejected
// and the previous one kept (§6.7): the failure is logged and the heartbeat
// reports last_error_class = parse until a valid policy arrives.
func (a *Agent) applyPolicy(ctx context.Context, resp api.PolicyResponse) error {
	// A rotated gateway password is applied first and whatever the policy
	// looks like: the console has already changed the login (DH13), so an
	// invalid policy must not leave the device without a working credential.
	if ps := resp.PendingGatewaySecret; ps != nil && ps.Password != "" {
		pw := string(ps.Password)
		if err := a.sec.Put(secrets.NameGatewayCred, []byte(pw)); err != nil {
			a.log.Error("could not store the rotated gateway credential", "err", err.Error())
			return err
		}
		a.secMu.Lock()
		a.gatewayPW = pw
		a.secMu.Unlock()
		a.mu.Lock()
		a.gwAck = true
		a.mu.Unlock()
		_ = a.jr.KVSet(ctx, journal.KVGatewayAckPend, "1")
		a.log.Info("gateway credential rotated by the console")
	}
	if err := resp.Policy.Validate(); err != nil {
		a.mu.Lock()
		a.polParseErr = true
		a.mu.Unlock()
		a.log.Error("policy rejected: it does not validate; keeping the previous policy", "version", resp.Version, "err", err.Error())
		a.eventLog("error", 1002, "a policy from the console was rejected as invalid")
		return errors.New("agent: policy does not validate")
	}
	etag := resp.ETag
	if etag == "" {
		etag, _ = policy.ETag(resp.Policy)
	}
	stored, err := persistPolicy(ctx, a.jr, a.layout, resp, etag)
	if err != nil {
		return err
	}

	a.mu.Lock()
	changed := a.pol == nil || a.polETag != etag
	a.pol = &stored
	a.noRunOnWake.Store(!stored.Policy.Schedule.RunOnWake)
	a.polETag = etag
	a.polParseErr = false
	a.mu.Unlock()
	if changed {
		a.log.Info("policy applied", "version", stored.Version)
		a.sched.Notify(scheduler.TriggerPolicyChange)
	}
	return nil
}

// persistPolicy stores a validated policy response as the last-known-good
// policy: the journal's kv table is authoritative, the files of §6.5
// (policy\policy.json, policy\etag) mirror it for the operator reading the
// device. A pending gateway secret is never stored here.
func persistPolicy(ctx context.Context, jr *journal.Journal, layout config.Layout, resp api.PolicyResponse, etag string) (api.PolicyResponse, error) {
	stored := resp
	stored.PendingGatewaySecret = nil
	raw, err := json.Marshal(stored)
	if err != nil {
		return stored, err
	}
	if err := jr.KVSet(ctx, journal.KVPolicyJSON, string(raw)); err != nil {
		return stored, err
	}
	if err := jr.KVSet(ctx, journal.KVPolicyETag, etag); err != nil {
		return stored, err
	}
	_ = config.WriteFileAtomic(layout.PolicyFile(), raw, 0o600)
	_ = config.WriteFileAtomic(layout.ETagFile(), []byte(etag), 0o600)
	return stored, nil
}

// pullPolicy performs the ETag pull and applies a changed policy.
func (a *Agent) pullPolicy(ctx context.Context) error {
	a.mu.Lock()
	etag := a.polETag
	a.mu.Unlock()
	resp, changed, err := a.up.PullPolicy(ctx, etag)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return a.applyPolicy(ctx, resp)
}

func (a *Agent) policyLoop(ctx context.Context) {
	bo := a.backoff()
	wait := time.Duration(0)
	for {
		if !sleepOrKick(ctx, wait, a.kickPolicy) {
			return
		}
		err := a.pullPolicy(ctx)
		if err != nil {
			wait = bo.Delay(err)
			a.uplinkFailed(err)
			a.log.Warn("policy pull failed", "err", err.Error(), "retry_in_s", int(wait/time.Second))
			continue
		}
		if bo.Failures() > 0 {
			a.consoleBack(loopPolicy)
		}
		bo.Reset()
		wait = policySafetyEvery
	}
}

// ---- heartbeat ------------------------------------------------------------

// facts derives the scheduling facts from the journal.
func (a *Agent) facts(ctx context.Context) scheduler.Facts {
	// The heartbeat, the planner input and a backup's start all call this on
	// goroutines Run started after it set a.sched; markSchedulerUp checks that
	// a.sched is set before it publishes it, so a call before Run cannot.
	a.markSchedulerUp()
	rows, err := a.jr.RecentRuns(ctx, restic.KindBackup, 100)
	if err != nil {
		a.log.Error("journal read failed", "err", err.Error())
		return scheduler.Facts{}
	}
	var pol policy.Policy
	a.mu.Lock()
	if a.pol != nil {
		pol = a.pol.Policy
	}
	a.mu.Unlock()
	return scheduler.FactsFromRuns(rows, pol)
}

// serviceState maps the agent's state to the heartbeat vocabulary (§6.2).
func (a *Agent) serviceState(f scheduler.Facts) (api.ServiceState, restic.DeferredReason) {
	a.mu.Lock()
	now := a.clock()
	running, paused := a.running != nil, pausedNow(a.pol, now)
	capped := a.leaseCapped && now.Before(a.leaseNotBefore)
	a.mu.Unlock()
	switch {
	case running:
		return api.ServiceRunning, ""
	case paused:
		return api.ServicePaused, ""
	case capped:
		return api.ServiceDeferred, restic.DeferredFleetCap
	case f.Retry.Pending && f.Last != nil && f.Last.Outcome == restic.OutcomeDeferred && f.Last.DeferredReason != "":
		return api.ServiceDeferred, f.Last.DeferredReason
	}
	return api.ServiceIdle, ""
}

// lastErrorClass is what the heartbeat reports: a state directory whose owner
// or DACL changed (permission) first, then a refusal to run (restic
// verification, no repository key), then an invalid policy, then the class of
// the newest failed or deferred run.
func (a *Agent) lastErrorClass(f scheduler.Facts) restic.ErrorClass {
	a.mu.Lock()
	state, guard, parse := a.stateErr != nil, a.resticErr != nil || a.repoMissing, a.polParseErr
	gateway := a.gwStalled && a.running != nil
	a.mu.Unlock()
	switch {
	case state:
		return restic.ClassPermission
	case guard:
		return restic.ClassGuardRefused
	case parse:
		return restic.ClassParse
	case gateway:
		// The running backup cannot reach the gateway (gateway_watchdog.go).
		return restic.ClassNetwork
	case f.Last != nil && (f.Last.Outcome == restic.OutcomeFailed || f.Last.Outcome == restic.OutcomeDeferred):
		return f.Last.ErrorClass
	}
	return ""
}

func (a *Agent) buildHeartbeat(ctx context.Context) api.HeartbeatRequest {
	f := a.facts(ctx)
	state, reason := a.serviceState(f)
	pw := readPower()
	queue, _ := a.jr.OutboxDepth(ctx)
	open, _ := a.jr.OpenRunCount(ctx)
	now := a.clock().UTC()

	a.mu.Lock()
	req := api.HeartbeatRequest{
		AgentVersion: versionLabel(), ResticVersion: a.resticVer,
		UptimeS: int64(now.Sub(a.boot.UTC()).Seconds()),
		OnAC:    pw.OnAC, BatteryPct: pw.BatteryPct,
		QueueDepth: queue, JournalDepth: open, DeviceTime: now,
		ServiceState: state, DeferredReason: reason, PolicyETag: a.polETag,
		HWFingerprint: a.host.HWFingerprint, GatewaySecretAcked: a.gwAck,
	}
	if a.pol != nil {
		req.PolicyVersionApplied = a.pol.Version
	}
	run, gwStalled := a.running, a.gwStalled
	a.mu.Unlock()
	if req.UptimeS < 0 {
		req.UptimeS = 0
	}
	req.LastErrorClass = a.lastErrorClass(f)
	if run != nil && state == api.ServiceRunning {
		req.ActiveRunID = run.id // renews the run's start lease (ADR 0005); never on a heartbeat that says idle
	}
	if run != nil {
		if p, ok := a.sup.Progress(); ok && !p.UpdatedAt.IsZero() {
			req.Progress = &api.Progress{
				RunID: run.id, Kind: restic.KindBackup, PercentDone: p.PercentDone, BytesDone: p.BytesDone,
				TotalBytes: p.TotalBytes, FilesDone: p.FilesDone, TotalFiles: p.TotalFiles, ErrorCount: p.ErrorCount,
				SecondsElapsed: p.SecondsElapsed, Stalled: p.Stalled, UpdatedAt: p.UpdatedAt.UTC(),
			}
		}
		// The gateway watchdog's condition (gateway unreachable, nothing
		// moving) is reported on the fields the heartbeat already has:
		// progress.stalled, with last_error_class network (lastErrorClass).
		// A run whose restic has not printed a status yet gets a progress
		// object of its own so the flag has somewhere to go.
		if gwStalled && state == api.ServiceRunning {
			if req.Progress == nil {
				req.Progress = &api.Progress{RunID: run.id, Kind: restic.KindBackup, UpdatedAt: now}
			}
			req.Progress.Stalled = true
		}
	}
	return req
}

// sendHeartbeat sends one heartbeat and handles the answer: clock offset,
// gateway-credential acknowledgement, policy etag hint, command hint.
func (a *Agent) sendHeartbeat(ctx context.Context) error {
	a.recheckState()
	req := a.buildHeartbeat(ctx)
	if err := req.Validate(); err != nil {
		a.log.Error("heartbeat is not valid; not sent", "err", err.Error())
		return errors.New("agent: heartbeat does not validate")
	}
	resp, err := a.up.Heartbeat(ctx, req)
	a.mu.Lock()
	a.hbLast = a.clock()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.noteConsoleReached()
	if off, ok := a.up.OffsetMS(); ok {
		_ = a.jr.KVSet(ctx, journal.KVClockOffsetMS, itoa(off))
	}
	_ = a.jr.KVSet(ctx, journal.KVLastHeartbeat, req.DeviceTime.Format(time.RFC3339))
	_ = a.jr.KVSet(ctx, journal.KVServiceState, string(req.ServiceState))
	_ = a.jr.KVSet(ctx, journal.KVLastErrorClass, string(req.LastErrorClass))
	if req.GatewaySecretAcked {
		a.mu.Lock()
		a.gwAck = false
		a.mu.Unlock()
		_ = a.jr.KVDelete(ctx, journal.KVGatewayAckPend)
	}
	a.mu.Lock()
	etag := a.polETag
	a.mu.Unlock()
	if resp.PolicyETag != "" && resp.PolicyETag != etag {
		kick(a.kickPolicy)
	}
	if resp.CommandsPending {
		kick(a.kickCommands)
	}
	return nil
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	every, minGap := a.o.heartbeatEvery, a.o.heartbeatMinGap
	if every <= 0 {
		every = defaultHeartbeatEvery
	}
	if minGap <= 0 {
		minGap = api.HeartbeatMinInterval
	}
	bo := a.backoff()
	wait := time.Duration(0)
	for {
		if !sleepOrKick(ctx, wait, a.kickHeartbeat) {
			return
		}
		// Never closer than the server's minimum gap, whatever kicked us.
		a.mu.Lock()
		last := a.hbLast
		since := a.clock().Sub(last)
		a.mu.Unlock()
		if !last.IsZero() && since < minGap {
			if !sleepOrKick(ctx, minGap-since, nil) {
				return
			}
			// The heartbeat about to go out answers every kick that came in
			// while the gap ran: a burst of kicks (one answer waking every
			// loop, ADR 0018) is one heartbeat, not one per kick.
			select {
			case <-a.kickHeartbeat:
			default:
			}
		}
		err := a.sendHeartbeat(ctx)
		if err != nil {
			wait = bo.Delay(err)
			a.uplinkFailed(err)
			a.log.Warn("heartbeat failed", "err", err.Error(), "retry_in_s", int(wait/time.Second))
			continue
		}
		if bo.Failures() > 0 {
			a.consoleBack(loopHeartbeat) // the console is back: wake the others, look at the schedule again
		}
		bo.Reset()
		wait = every + time.Duration((rand.Float64()*2-1)*float64(heartbeatJitter))
		if every < 2*heartbeatJitter {
			wait = every
		}
		if wait < minGap {
			wait = minGap
		}
	}
}

// ---- outbox ---------------------------------------------------------------

// deliverOutbox sends due run records. A record is delivered only after a
// created, updated or duplicate answer; rejected is journaled and not retried;
// any failure schedules a retry with the bounded backoff.
func (a *Agent) deliverOutbox(ctx context.Context) error {
	now := a.clock()
	items, err := a.jr.OutboxDue(ctx, now, 50)
	if err != nil || len(items) == 0 {
		return err
	}
	var batch []journal.OutboxItem
	var req api.RunsRequest
	for _, it := range items {
		if verr := it.Body.Validate(); verr != nil {
			// A record the console would refuse: journal it as rejected here
			// instead of retrying forever (and say so loudly).
			a.log.Error("run record does not validate; not sent", "run", it.RunID, "err", verr.Error())
			_ = a.jr.MarkRejected(ctx, it.RunID, it.Seq, api.ReasonInvalid)
			continue
		}
		batch = append(batch, it)
		req.Runs = append(req.Runs, it.Body)
	}
	if len(batch) == 0 {
		return nil
	}
	resp, err := a.up.UploadRuns(ctx, req)
	if err != nil {
		bo := a.backoff()
		for _, it := range batch {
			d := bo.DelayFor(it.Attempts)
			var ue *uplink.Error
			if errors.As(err, &ue) && ue.RetryAfter > 0 {
				d = ue.RetryAfter
			}
			_ = a.jr.OutboxFailed(ctx, it.RunID, it.Seq, now.Add(d))
		}
		return err
	}
	results := make(map[string]api.RunResult, len(resp.Results))
	for _, r := range resp.Results {
		results[r.ID] = r
	}
	bo := a.backoff()
	for _, it := range batch {
		r, ok := results[it.RunID]
		switch {
		case ok && (r.Status == api.RunCreated || r.Status == api.RunUpdated || r.Status == api.RunDuplicate):
			_ = a.jr.MarkDelivered(ctx, it.RunID, it.Seq)
		case ok && r.Status == api.RunRejected:
			a.log.Error("the console rejected a run record", "run", it.RunID, "reason", sanitizeReason(r.Reason))
			_ = a.jr.MarkRejected(ctx, it.RunID, it.Seq, sanitizeReason(r.Reason))
		default:
			_ = a.jr.OutboxFailed(ctx, it.RunID, it.Seq, now.Add(bo.DelayFor(it.Attempts)))
		}
	}
	return nil
}

func sanitizeReason(s string) string {
	switch s {
	case api.ReasonInvalid, api.ReasonConflict, api.ReasonInternal, "wrong_device":
		return s
	}
	return "unknown"
}

func (a *Agent) outboxLoop(ctx context.Context) {
	bo := a.backoff()
	wait := time.Duration(0)
	for {
		if !sleepOrKick(ctx, wait, a.kickOutbox) {
			return
		}
		err := a.deliverOutbox(ctx)
		if err != nil {
			wait = bo.Delay(err)
			a.uplinkFailed(err)
			a.log.Warn("run upload failed", "err", err.Error(), "retry_in_s", int(wait/time.Second))
			continue
		}
		if bo.Failures() > 0 {
			a.consoleBack(loopOutbox)
		}
		bo.Reset()
		wait = outboxIdleEvery
		if next, ok, _ := a.jr.OutboxNext(ctx); ok {
			if d := next.Sub(a.clock()); d > 0 && d < wait {
				wait = d
			} else if d <= 0 {
				wait = time.Second
			}
		}
	}
}

// ---- commands -------------------------------------------------------------

func (a *Agent) postResults(ctx context.Context) error {
	rows, err := a.jr.UnpostedCommands(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := a.postResult(ctx, row.ID, commands.ResultFromRow(row)); err != nil {
			return err
		}
	}
	return nil
}

// postResult posts one command result and settles it in the command log. It
// returns nil when the result is settled: acknowledged (2xx) or refused with a
// final 4xx (uplink.Refused), in which case it is dropped, journaled as
// result_refused and never re-posted. A refused result must not wedge the
// agent: the console would have to answer every future result the same way, and
// until a result is settled the agent fetches no new command (ADR 0010).
// Anything else (401/403, 408, 429, 5xx, network) returns the error for the
// caller to back off on; the result stays unposted.
func (a *Agent) postResult(ctx context.Context, id string, res api.CommandResultRequest) error {
	err := a.up.CommandResult(ctx, id, res)
	switch {
	case err == nil:
		_ = a.jr.MarkCommandPosted(ctx, id)
		return nil
	case uplink.Refused(err):
		// Ids, the type-free status and the fixed error text only: no result
		// body (it is not needed to diagnose and stays off the log).
		a.log.Warn("result_refused", "command", id, "status", string(res.Status), "err", err.Error())
		if jerr := a.jr.MarkCommandRefused(ctx, id); jerr != nil {
			// Could not settle it: return the original error so the loop
			// backs off and tries again rather than spinning. Accepted: while
			// the journal cannot be written the agent also cannot record a
			// command, so polling stops until writes work (M2-W3C review).
			a.log.Error("result_refused could not be journaled", "command", id, "err", jerr.Error())
			return err
		}
		return nil
	}
	return err
}

func (a *Agent) commandsLoop(ctx context.Context) {
	bo := a.backoff()
	wait := time.Duration(0)
	pollWait := a.o.pollWait
	if pollWait <= 0 {
		pollWait = defaultPollWait
	}
	for {
		if !sleepOrKick(ctx, wait, a.kickCommands) {
			return
		}
		wait = 0
		if err := a.postResults(ctx); err != nil {
			wait = bo.Delay(err)
			a.uplinkFailed(err)
			a.log.Warn("command result post failed", "err", err.Error(), "retry_in_s", int(wait/time.Second))
			continue
		}
		resp, err := a.up.PollCommands(ctx, pollWait)
		if err != nil {
			if errors.Is(err, uplink.ErrPollInFlight) {
				wait = time.Second
				continue
			}
			if ctx.Err() != nil {
				return
			}
			wait = bo.Delay(err)
			a.uplinkFailed(err)
			a.log.Warn("command poll failed", "err", err.Error(), "retry_in_s", int(wait/time.Second))
			continue
		}
		if bo.Failures() > 0 {
			a.consoleBack(loopCommands)
		}
		bo.Reset()
		for _, cmd := range resp.Commands {
			res, err := a.exec.Execute(ctx, cmd)
			if err != nil {
				a.log.Error("command could not be journaled", "command", cmd.ID, "err", err.Error())
				continue
			}
			a.log.Info("command executed", "command", cmd.ID, "type", string(cmd.Type), "status", string(res.Status))
			// Post at once, also for an id seen before (the console may deliver
			// again); a lost post stays unposted in the log and postResults
			// retries it before the next poll.
			if err := a.postResult(ctx, cmd.ID, res); err != nil {
				a.log.Warn("command result post failed", "command", cmd.ID, "err", err.Error())
				continue
			}
		}
	}
}

// Command handlers (commands.Handlers).

func (a *Agent) cmdBackupNow(ctx context.Context) (bool, error) {
	a.mu.Lock()
	running := a.running != nil
	skip := a.pol == nil || a.pol.Policy.Schedule.SkipIfRunning
	a.mu.Unlock()
	if running && skip {
		a.journalSkipped(ctx, api.TriggerManual)
		return false, nil
	}
	a.mu.Lock()
	a.forceNow, a.forceAt = true, a.clock()
	a.mu.Unlock()
	a.sched.Notify(scheduler.TriggerCommand)
	return true, nil
}

// cmdCancel stops the active run when runID names it. An empty runID (a cancel
// that carries none, as from a console that predates run_id) stops whatever is
// running, as before. A runID that is not the active run's - a stale or
// finished run - stops nothing and answers run_mismatch, so a late cancel
// cannot kill a newer run.
func (a *Agent) cmdCancel(ctx context.Context, runID string) (bool, string) {
	a.mu.Lock()
	run := a.running
	a.mu.Unlock()
	switch {
	case runID != "" && (run == nil || run.id != runID):
		return false, commands.ReasonRunMismatch
	case run == nil:
		return false, commands.ReasonNoRun
	}
	run.cancel(supervisor.ErrCancelled)
	return true, ""
}

func (a *Agent) cmdCheckIn(ctx context.Context) error {
	if err := a.sendHeartbeat(ctx); err != nil {
		return err
	}
	return a.pullPolicy(ctx)
}

// backoff returns a fresh bounded backoff (1 min doubling to 5 min, ADR 0018);
// tests shorten the minimum.
func (a *Agent) backoff() *uplink.Backoff {
	b := uplink.NewBackoff()
	if a.o.backoffMin > 0 {
		b.Min = a.o.backoffMin
	}
	return b
}

// The uplink callers that report a recovery (consoleBack's from).
const (
	loopHeartbeat = "heartbeat"
	loopPolicy    = "policy"
	loopOutbox    = "outbox"
	loopCommands  = "commands"
	callLease     = "lease"
	callSnapshots = "snapshots"
)

// uplinkFailed notes that an uplink call failed because the console could not
// be reached or could not answer, so the next success is a recovery worth one
// log line (consoleBack). Only an outage counts: a 4xx, or an answer whose body
// does not validate, proves the console is up (a lease route the console does
// not have, a policy the agent rejects), and must not make the next success
// read as "reachable again".
func (a *Agent) uplinkFailed(err error) {
	if !outageError(err) {
		return
	}
	a.mu.Lock()
	a.consoleDown = true
	a.mu.Unlock()
}

// outageError reports whether err says the console was unreachable or unable
// to answer: a transport or TLS failure, a 5xx, or a 429.
func outageError(err error) bool {
	var ue *uplink.Error
	if !errors.As(err, &ue) {
		return false
	}
	return ue.Network || ue.TLS || ue.Status >= 500 || ue.Status == http.StatusTooManyRequests
}

// consoleWasDown reports whether an uplink call failed since the last
// consoleBack. Callers that have no backoff of their own (the start lease, the
// snapshot upload) ask it before calling consoleBack.
func (a *Agent) consoleWasDown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.consoleDown
}

// consoleBack is called when an uplink call succeeded after failures (ADR 0018
// decision 2): from names the caller. Any answer proves the console is up, so
// it wakes the other loops (heartbeat first, since the board turns ONLINE on
// it, then policy, outbox, commands) and tells the scheduler to look at the
// schedule again. A kick is a non-blocking send on a one-slot channel and the
// heartbeat loop keeps its minimum gap, so a burst of successes is not a burst
// of requests. The caller's own loop is not kicked: it carries on by itself.
// It logs one line per recovery, however many callers see it: only the first
// call after a failure finds consoleDown set.
func (a *Agent) consoleBack(from string) {
	a.mu.Lock()
	was := a.consoleDown
	a.consoleDown = false
	a.mu.Unlock()
	if was {
		a.log.Info("console reachable again", "from", from)
	}
	for _, k := range [...]struct {
		name string
		ch   chan struct{}
	}{
		{loopHeartbeat, a.kickHeartbeat}, {loopPolicy, a.kickPolicy},
		{loopOutbox, a.kickOutbox}, {loopCommands, a.kickCommands},
	} {
		if k.name != from {
			kick(k.ch)
		}
	}
	a.sched.Notify(scheduler.TriggerConnectivity)
}

// ---- helpers --------------------------------------------------------------

// sleepOrKick waits d, a kick, or ctx; it reports false when ctx ended. A nil
// kick channel only waits.
func sleepOrKick(ctx context.Context, d time.Duration, kickCh chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-kickCh:
		return true
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
