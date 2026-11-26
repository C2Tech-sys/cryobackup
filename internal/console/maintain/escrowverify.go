package maintain

import (
	"context"
	"errors"
	"hash/fnv"
	"sort"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The escrow verifier (ADR 0030, completing ADR 0014). Every scheduler pass, on
// the lease holder only (RunOnce calls it after taking the scheduler lease), it
// re-proves the escrowed keys that are due: it unwraps the key and runs
// `restic cat config` through the admin path, as enrollment does, and records
// the outcome on the key's escrow row. What that proves: the escrowed key opens
// the repository's config. It does not prove that the data is intact (that is
// `restic check`, which the pilot runs without; ADR 0026).
//
// Its candidates are its own (store.EscrowVerifyKeys): every device with an
// active escrowed key and a confirmed enrollment that is not being purged,
// retired devices included, whether or not maintenance applies to it.

// escrowVerifyRetry is how long a key waits after an attempt, whatever its
// outcome, before the scheduler tries cat config on it again: a key that could
// not be proven (storage unreachable, a timeout, a busy worker) or whose last
// verdict was a failure is retried hourly, not every pass.
const escrowVerifyRetry = time.Hour

// escrowVerifyStagger is the widest per-device advance of the due time: a key is
// due once its newest proof is state.EscrowVerifyEvery old less a stable offset
// in [0, escrowVerifyStagger) from its device id, so a fleet that was proven on
// one pass (a first deploy, a restored database) comes due over a day, not on
// one pass.
const escrowVerifyStagger = 24 * time.Hour

// escrowVerifyPerPass and escrowVerifyBudget bound the escrow phase of one
// scheduler pass. The first pass after deploy may find many due keys, and
// against a blackholed storage endpoint every cat config runs to its timeout
// (55 s by default). Each check may first wait for a free concurrency slot
// (Config.MaxConcurrent, shared with the console's admin-path calls), at most
// schedulerSlotWait (30 s); a check that gets none in time records only the
// attempt. The checks run one at a time, so the phase holds at most one slot:
// it shares the last free one with device maintenance, first come, and never
// starves it (config.go). The phase starts at most escrowVerifyPerPass checks,
// and none after escrowVerifyBudget has passed, so it ends within the budget
// plus one slot wait plus one cat-config timeout (about 3.5 minutes), inside the
// scheduler lease (at least 10 minutes, renewed while the pass runs). The rest
// wait for the next pass.
const (
	escrowVerifyPerPass = 5
	escrowVerifyBudget  = 2 * time.Minute
)

// escrowVerifyOffset is the device's stable advance of its due time, in whole
// seconds in [0, escrowVerifyStagger).
func escrowVerifyOffset(deviceID string) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(deviceID))
	return time.Duration(h.Sum64()%uint64(escrowVerifyStagger/time.Second)) * time.Second
}

// escrowVerifyDue reports whether k is due at now: it was never proven, or its
// newest proof is state.EscrowVerifyEvery old less its stagger offset, and no
// attempt was made within escrowVerifyRetry. A proof time that does not parse is
// due (it proves nothing); an attempt time more than a few minutes in the future
// does not hold the key back.
func escrowVerifyDue(k state.EscrowKeyFacts, now time.Time) bool {
	if at, ok := k.LastAttemptAt.Time(); ok && !at.After(now.Add(state.FutureSkewAllowed)) && now.Sub(at) < escrowVerifyRetry {
		return false
	}
	if k.Escrow.VerifiedAt == 0 {
		return true
	}
	proven, ok := k.Escrow.VerifiedAt.Time()
	if !ok {
		return true
	}
	return now.Sub(proven) >= state.EscrowVerifyEvery-escrowVerifyOffset(k.DeviceID)
}

// escrowVerifyOrder sorts the due keys: never proven first (never attempted,
// then the oldest attempt), then the oldest proof, then by device id.
func escrowVerifyOrder(due []state.EscrowKeyFacts) {
	sort.SliceStable(due, func(i, j int) bool {
		a, b := due[i], due[j]
		an, bn := a.Escrow.VerifiedAt == 0, b.Escrow.VerifiedAt == 0
		switch {
		case an != bn:
			return an
		case an && (a.LastAttemptAt == 0) != (b.LastAttemptAt == 0):
			return a.LastAttemptAt == 0
		case an && a.LastAttemptAt != b.LastAttemptAt:
			return a.LastAttemptAt < b.LastAttemptAt
		case !an && a.Escrow.VerifiedAt != b.Escrow.VerifiedAt:
			return a.Escrow.VerifiedAt < b.Escrow.VerifiedAt
		}
		return a.DeviceID < b.DeviceID
	})
}

// verifyEscrows is the escrow phase of a pass: it reads the verifier's own
// candidates and verifies the due ones in escrowVerifyOrder, within
// escrowVerifyPerPass and escrowVerifyBudget. It runs before the usage refresh
// so that a long usage pass cannot starve it.
//
// A verifier that cannot run says so loudly and records nothing: when the
// candidates cannot be read, or the worker has no master key (no key can be
// unwrapped). Its keys then age, and an escrowed key not proven for the recheck
// interval raises ESCROW_UNVERIFIED (state.EscrowRecheckEnforced), C22 for a
// retired device; a stale proof never stays green.
func (w *Worker) verifyEscrows(ctx context.Context, rep *CycleReport) {
	start := w.clock()
	keys, err := w.st.EscrowVerifyKeys(ctx, store.ScopeAll())
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("escrow verification cannot run: its candidates could not be read", attrClass("store"))
		}
		return
	}
	var due []state.EscrowKeyFacts
	for _, k := range keys {
		if escrowVerifyDue(k, start) {
			due = append(due, k)
		}
	}
	if len(due) == 0 {
		return
	}
	if r, ok := w.keys.(masterKeyReporter); ok && !r.MasterKeyLoaded() {
		rep.EscrowDeferred = len(due)
		w.log.Error("escrow verification cannot run: the master key is not loaded", attrClass("master_key_unavailable"), attrCount(len(due)))
		return
	}
	escrowVerifyOrder(due)
	for i, k := range due {
		if i >= escrowVerifyPerPass || ctx.Err() != nil || w.clock().Sub(start) >= escrowVerifyBudget {
			rep.EscrowDeferred = len(due) - i
			return
		}
		w.verifyEscrow(ctx, k.DeviceID, rep)
	}
}

// ClassifyEscrowCheck maps one cat config outcome onto the verifier's classes,
// for the scheduler here and for the console's on-demand check (adminapi
// escrow_p22.go), so both record the same thing: success is a proof (v.OK);
// wrong_password and repo_missing are verdicts on the key or the repository
// (v.ErrorClass, failures); anything else (storage unreachable, a timeout, a
// busy worker, an internal error) is an attempt only, and class names it. skip
// is set for a customer-held key: there is nothing to verify and nothing to
// record. Every class is a fixed adminpath code, never text from the error.
func ClassifyEscrowCheck(err error) (v store.EscrowVerification, class string, skip bool) {
	if err == nil {
		v.OK = true
		return v, "", false
	}
	var ae *adminpath.Error
	switch {
	case errors.As(err, &ae) && ae.Code == adminpath.CodeCustodyCustomerHeld:
		return v, "", true
	case errors.As(err, &ae) && (ae.Code == adminpath.CodeWrongPassword || ae.Code == adminpath.CodeRepoMissing):
		v.ErrorClass = ae.Code
		return v, ae.Code, false
	case errors.As(err, &ae):
		return v, ae.Code, false
	case errors.Is(err, context.DeadlineExceeded):
		return v, adminpath.CodeTimeout, false
	}
	return v, "unverifiable", false
}

// verifyEscrow runs cat config with the device's escrowed key through this
// worker and records the outcome (ClassifyEscrowCheck). Success sets
// verified_at. A verdict on the key or the repository is recorded as
// last_verify_error_class, which raises ESCROW_UNVERIFIED with the "verify
// failed" reason (C22 for a retired device). Anything else records only the
// attempt: the key was not proven, so its previous proof keeps ageing, and the
// next attempt is an hour later. cat config takes no repository lock, so step 1
// does not apply.
func (w *Worker) verifyEscrow(ctx context.Context, deviceID string, rep *CycleReport) {
	cctx, cancel := context.WithTimeout(ctx, schedulerSlotWait+w.cfg.Timeouts.CatConfig)
	_, err := w.CatConfig(cctx, adminpath.DeviceRequest{DeviceID: deviceID, RequestID: w.schedulerRequestID()})
	cancel()
	if ctx.Err() != nil {
		return
	}
	v, class, skip := ClassifyEscrowCheck(err)
	if skip {
		return // not escrowed: nothing to verify (the candidate query already filters these)
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if rerr := w.st.RecordEscrowVerification(rctx, store.ScopeAll(), deviceID, v); rerr != nil {
		w.log.Error("escrow verification not recorded", attrDevice(deviceID))
		return
	}
	if v.OK {
		rep.EscrowVerified++
		w.log.Info("escrow verified", attrDevice(deviceID))
		return
	}
	rep.EscrowUnverified++
	w.log.Warn("escrow verification failed", attrDevice(deviceID), attrClass(class))
}
