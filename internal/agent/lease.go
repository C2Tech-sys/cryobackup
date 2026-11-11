package agent

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Fleet concurrency cap by console start lease (ADR 0005, feature row r30).
//
// Before restic starts, a scheduled backup (triggers schedule, catchup, boot,
// wake, retry) asks the console for a start lease with its run id:
//
//   - granted: the run starts; RunStats.Lease = granted (not_required when the
//     console answers granted without a lease: the target has no cap);
//   - refused: the run is journaled deferred with reason fleet_cap and does not
//     start; the scheduler waits retry_after_s plus up to 20 % jitter
//     (Input.LeaseNotBefore). The deferral is not an attempt: it does not move
//     the retry ladder (scheduler.FactsFromRuns ignores it);
//   - the console unreachable, a timeout (api.StartLeaseTimeout), 5xx, 429, or
//     a console without the route (404, 405, 501): the run starts anyway,
//     RunStats.Lease = bypassed, logged at warn (fail open, loud);
//   - any other 4xx (401 revoked token, 403, 400, 409): the run does not start,
//     nothing is journaled, logged at error, and the scheduler waits the
//     uplink backoff before asking again, like every other refused call.
//
// backup_now (trigger manual) does not ask and records not_required. The call
// runs on the run's context, so a service stop or a cancel command aborts it at
// once; the run is then journaled interrupted or cancelled like a run stopped
// before restic started.

// leaseVerdict is what the start-lease step decided.
type leaseVerdict int

const (
	leaseStart  leaseVerdict = iota // start restic
	leaseDefer                      // refused by the cap: journaled deferred/fleet_cap
	leaseRefuse                     // refused by the console (4xx): nothing journaled
	leaseAbort                      // stop or cancel during the call: journaled interrupted/cancelled
)

// failOpen reports whether a start-lease failure lets the run start anyway.
func failOpen(err error) bool {
	var ue *uplink.Error
	if !errors.As(err, &ue) {
		return false // no token, a body that is not path-free: our own fault, never bypassed
	}
	switch {
	case ue.Network, ue.TLS:
		return true
	case ue.Status < 400: // a 2xx the agent could not read is a console fault, like a 5xx
		return true
	case ue.Status >= 500, ue.Status == http.StatusTooManyRequests:
		return true
	case ue.Status == http.StatusNotFound, ue.Status == http.StatusMethodNotAllowed:
		// A console that predates ADR 0005 has no such route: no cap exists there.
		return true
	}
	return false
}

func (a *Agent) leaseTimeout() time.Duration {
	if a.o.leaseTimeout > 0 {
		return a.o.leaseTimeout
	}
	return api.StartLeaseTimeout
}

// leaseWait is how long a refused start waits: the console's retry_after_s
// (clamped to the protocol's bounds) plus 0 to 20 % jitter, so the devices a
// full cap turned away do not come back in the same second.
func (a *Agent) leaseWait(retryAfterS int) time.Duration {
	d := time.Duration(retryAfterS) * time.Second
	if d < api.StartLeaseRetryMin {
		d = api.StartLeaseRetryMin
	}
	if d > api.StartLeaseRetryMax {
		d = api.StartLeaseRetryMax
	}
	if a.o.leaseWait != nil {
		return a.o.leaseWait(d)
	}
	return d + time.Duration(rand.Float64()*float64(d)/5)
}

// setLeaseWait holds scheduled starts until t; capped says the console's cap
// refused (the heartbeat then reports deferred/fleet_cap).
func (a *Agent) setLeaseWait(t time.Time, capped bool) {
	a.mu.Lock()
	a.leaseNotBefore, a.leaseCapped = t, capped
	a.mu.Unlock()
}

// acquireLease runs the start-lease step for rec (ADR 0005) on the run
// context ctx; journal writes use jctx. It returns the lease outcome for
// RunStats.Lease and what to do.
func (a *Agent) acquireLease(ctx, jctx context.Context, rec *api.RunRecord) (api.LeaseOutcome, leaseVerdict) {
	if rec.Trigger == api.TriggerManual {
		return api.LeaseNotRequired, leaseStart
	}
	lctx, cancel := context.WithTimeout(ctx, a.leaseTimeout())
	resp, err := a.up.StartLease(lctx, api.StartLeaseRequest{RunID: rec.ID})
	cancel()
	if ctx.Err() != nil {
		a.journalStoppedBeforeStart(ctx, jctx, rec)
		return "", leaseAbort
	}
	if err == nil {
		if !resp.Granted {
			a.deferForCap(jctx, rec, resp.RetryAfterS)
			return "", leaseDefer
		}
		a.mu.Lock()
		a.leaseNotBefore, a.leaseCapped = time.Time{}, false
		a.leaseBO.Reset()
		a.mu.Unlock()
		if a.consoleWasDown() {
			a.consoleBack(callLease) // an answer after a failed call: wake the uplink loops (ADR 0018)
		}
		if resp.LeaseUntil == nil {
			return api.LeaseNotRequired, leaseStart
		}
		return api.LeaseGranted, leaseStart
	}
	a.uplinkFailed(err)
	if failOpen(err) {
		a.log.Warn("start lease not available; starting without it (fail open, ADR 0005)", "run", rec.ID, "err", err.Error())
		a.setLeaseWait(time.Time{}, false)
		return api.LeaseBypassed, leaseStart
	}
	a.mu.Lock()
	wait := a.leaseBO.Delay(err)
	a.leaseNotBefore, a.leaseCapped = a.clock().Add(wait), false
	a.mu.Unlock()
	a.log.Error("the console refused the start lease; the backup does not start", "run", rec.ID, "err", err.Error(),
		"retry_in_s", int(wait/time.Second))
	return "", leaseRefuse
}

// deferForCap journals a run the cap refused as deferred/fleet_cap and holds
// scheduled starts until the console's retry time plus jitter.
func (a *Agent) deferForCap(jctx context.Context, rec *api.RunRecord, retryAfterS int) {
	wait := a.leaseWait(retryAfterS)
	now := a.clock().UTC()
	rec.FinishedAt, rec.Outcome, rec.DeferredReason = &now, restic.OutcomeDeferred, restic.DeferredFleetCap
	a.mu.Lock()
	a.leaseNotBefore, a.leaseCapped = now.Add(wait), true
	a.mu.Unlock()
	// The boot label goes back to the next start in runBackup, as on every path
	// on which restic did not start.
	if err := a.jr.InsertFinished(jctx, *rec); err != nil {
		a.log.Error("could not journal a deferred run", "run", rec.ID, "err", err.Error())
	}
	a.log.Info("backup deferred: the fleet concurrency cap is full", "run", rec.ID, "retry_in_s", int(wait/time.Second))
	kick(a.kickOutbox)
	kick(a.kickHeartbeat)
}

// journalStoppedBeforeStart closes a run stopped while it waited for its
// lease or for the console after a resume (resume_wait.go): interrupted on a
// service stop, cancelled on a cancel command, as for a
// run stopped before restic started (supervisor.ErrCancelledBeforeStart).
func (a *Agent) journalStoppedBeforeStart(ctx, jctx context.Context, rec *api.RunRecord) {
	now := a.clock().UTC()
	rec.FinishedAt, rec.Outcome = &now, restic.OutcomeCancelled
	if errors.Is(context.Cause(ctx), supervisor.ErrServiceStop) {
		rec.Outcome = restic.OutcomeInterrupted
	}
	if err := a.jr.InsertFinished(jctx, *rec); err != nil {
		a.log.Error("could not journal a run stopped before it started", "run", rec.ID, "err", err.Error())
	}
	a.log.Info("backup stopped before restic started (it waited for its start lease or for the console)", "run", rec.ID, "outcome", string(rec.Outcome))
}
