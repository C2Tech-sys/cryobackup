package maintain

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// escrowVerifyRetry is how long a never-verified key waits after an attempt
// that did not succeed before the scheduler tries cat config again.
const escrowVerifyRetry = time.Hour

// escrowVerifyPerPass and escrowVerifyBudget bound the escrow phase of one
// scheduler pass. The first pass after deploy may find many never-verified
// keys, and against a blackholed storage endpoint every cat config runs to its
// timeout (55 s by default): the phase starts at most escrowVerifyPerPass
// checks, and none after escrowVerifyBudget has passed, so it ends within the
// budget plus one cat-config timeout (about 3 minutes), well inside the
// scheduler lease (at least 10 minutes). The rest wait for the next pass.
const (
	escrowVerifyPerPass = 5
	escrowVerifyBudget  = 2 * time.Minute
)

// escrowVerifyDue reports whether the candidate's active escrowed key has never
// been verified and no attempt was made within escrowVerifyRetry.
//
// Why this exists (M2-FIX-B1, lab finding F7, ADR 0014): since the fix, an
// enrollment records its successful cat config as the escrow verification, but
// devices enrolled before it have verified_at NULL and turn ESCROW_UNVERIFIED
// after the 24 h grace. The only trace of their enrollment-time check is the
// agent.enroll audit row, which says the enrollment finished, not which key
// opened which repository; rather than backfill verified_at from it, the
// scheduler proves each such key once, now, the same way enrollment does. It
// is not the 30-day recheck: a key verified once is not checked again here
// (that job is M5's).
func escrowVerifyDue(c candidate, now time.Time) bool {
	if !c.escrowUnverified {
		return false
	}
	return !c.lastVerifyAttempt.Valid || now.Sub(time.UnixMilli(c.lastVerifyAttempt.Int64)) >= escrowVerifyRetry
}

// verifyEscrows is the escrow phase of a pass: it verifies the due keys
// among cands, never-attempted first, then the oldest attempt, then by device
// id, within escrowVerifyPerPass and escrowVerifyBudget. It runs before the
// usage refresh so that a long usage pass cannot starve it.
func (w *Worker) verifyEscrows(ctx context.Context, cands []candidate, rep *CycleReport) {
	start := w.clock()
	var due []candidate
	for _, c := range cands {
		if escrowVerifyDue(c, start) {
			due = append(due, c)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		a, b := due[i].lastVerifyAttempt, due[j].lastVerifyAttempt
		switch {
		case a.Valid != b.Valid:
			return !a.Valid // never attempted first
		case a.Valid && a.Int64 != b.Int64:
			return a.Int64 < b.Int64
		}
		return due[i].deviceID < due[j].deviceID
	})
	for i, c := range due {
		if i >= escrowVerifyPerPass || ctx.Err() != nil || w.clock().Sub(start) >= escrowVerifyBudget {
			rep.EscrowDeferred = len(due) - i
			return
		}
		w.verifyEscrow(ctx, c.deviceID, rep)
	}
}

// verifyEscrow runs cat config with the device's escrowed key through this
// worker and records the outcome. Success sets verified_at. A verdict on the key
// or the repository (wrong_password, repo_missing) is recorded as
// last_verify_error_class, which keeps ESCROW_UNVERIFIED raised with the
// "verify failed" reason. Anything else (storage unreachable, a timeout, a busy
// worker) records only the attempt: the key was not proven, so the device stays
// "never verified" and alarms after the grace, and the next attempt is an hour
// later. cat config takes no repository lock, so step 1 does not apply.
func (w *Worker) verifyEscrow(ctx context.Context, deviceID string, rep *CycleReport) {
	_, err := w.CatConfig(ctx, adminpath.DeviceRequest{DeviceID: deviceID, RequestID: w.schedulerRequestID()})
	if ctx.Err() != nil {
		return
	}
	v := store.EscrowVerification{OK: err == nil}
	var ae *adminpath.Error
	if err != nil && errors.As(err, &ae) {
		switch ae.Code {
		case adminpath.CodeCustodyCustomerHeld:
			return // not escrowed: nothing to verify (step 0 already filters these)
		case adminpath.CodeWrongPassword, adminpath.CodeRepoMissing:
			v.ErrorClass = ae.Code
		}
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
	class := v.ErrorClass
	if class == "" {
		class = "unverifiable"
		if ae != nil {
			class = ae.Code
		}
	}
	w.log.Warn("escrow verification failed", attrDevice(deviceID), attrClass(class))
}
