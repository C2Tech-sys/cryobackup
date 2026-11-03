package maintain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Names and tunables of the scheduler (architecture §7.3, §5.7).
const (
	// SchedulerJob is the job_state name of the maintenance scheduler; its lease
	// keeps two maintain processes from working at once, and its job_state row
	// is what banner C07 reads.
	SchedulerJob = "maintenance-scheduler"
	// lockDeferralLimit is the number of consecutive lock deferrals after which
	// step 2 runs `restic unlock` (stale locks only).
	lockDeferralLimit = 3
	// preflightMin and preflightSpan give the step 1 reschedule: 15 minutes plus
	// a per-device jitter of up to 45 more.
	preflightMin  = 15 * time.Minute
	preflightSpan = 45 * time.Minute
	// m5LogEvery limits the "scheduled for M5" line to once an hour.
	m5LogEvery = time.Hour
)

// CycleReport is what one scheduler pass did, in counts only.
type CycleReport struct {
	// Skipped is set when another process held the scheduler lease. A skipped pass
	// writes nothing to job_state.
	Skipped bool
	// Considered is the number of devices step 0 let through.
	Considered int
	// Preflight is the number of devices step 1 skipped (a backup is running or
	// a reschedule is pending).
	Preflight int
	// Sampled is the number of usage samples written (step 8).
	Sampled int
	// Deferred is the number of lock deferrals counted (step 2).
	Deferred int
	// Unlocked is the number of stale-lock unlocks run after repeated deferrals.
	Unlocked int
	// Failed is the number of devices whose usage refresh failed.
	Failed int
	// M5Due is the number of forget, prune and check operations that are due and
	// that this version does not run.
	M5Due int
	// EscrowVerified is the number of never-verified escrowed keys this pass
	// proved with cat config (M2-FIX-B1, see verifyEscrow).
	EscrowVerified int
	// EscrowUnverified is the number of those verifications that did not succeed.
	EscrowUnverified int
	// ErrorClass is the dominant failure class (path-free), "" when none.
	ErrorClass restic.ErrorClass
}

// Loop runs the maintenance worker until ctx is done: it serves the admin-path
// socket (adminpath.Serve over the Worker as the Backend) and runs the
// scheduler every Config.TickEvery. It returns nil on a clean shutdown and an
// error when the socket server fails (the scheduler is then stopped too: a
// worker that cannot be reached by the console must not go quietly on, so the
// process exits and is restarted).
func (w *Worker) Loop(ctx context.Context) error {
	if w.cfg.SocketPath == "" {
		return cfgErr("SocketPath is required for Loop")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	serveErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := adminpath.Serve(ctx, w.cfg.SocketPath, adminpath.Handler(w))
		serveErr <- err
		cancel()
	}()

	tick := time.NewTicker(w.cfg.TickEvery)
	defer tick.Stop()
	w.cycle(ctx) // one pass straight away: a restart must not wait a minute
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
			w.cycle(ctx)
		}
	}
	cancel()
	wg.Wait()
	if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("maintain: admin-path server: %w", err)
	}
	return nil
}

// cycle is RunOnce with the error logged: the loop must keep ticking.
func (w *Worker) cycle(ctx context.Context) {
	if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
		w.log.Error("maintenance cycle failed")
	}
}

// RunOnce performs one scheduler pass (`cryo-console maintain --once`): it takes
// the maintenance-scheduler lease, applies steps 0 to 2 of architecture §7.3 to
// every device, performs step 8 (usage refresh) when due, notes the forget,
// prune and check operations that are due but belong to M5, and records the
// pass with store.RecordJobRun. A pass that finds the lease held elsewhere
// returns a CycleReport with Skipped set and writes nothing to job_state (C07
// reads that row, so only the lease holder may write it); it is logged locally.
// The returned error is non-nil only when the scheduler could not run at all
// (lease or database failure).
func (w *Worker) RunOnce(ctx context.Context) (CycleReport, error) {
	start := w.clock()
	var rep CycleReport
	got, err := w.st.AcquireJobLease(ctx, SchedulerJob, w.holder, w.leaseTTL())
	if err != nil {
		return rep, w.storeErr(ctx, err)
	}
	if !got {
		// The lease is held elsewhere. job_state is what banner C07 reads, and only
		// the lease holder may write it: a "skipped" row from this process would
		// refresh last_finished_at and overwrite the holder's "error" status, so a
		// failing scheduler would look alive. Log locally and write nothing.
		rep.Skipped = true
		w.log.Info("scheduler lease held by another process; pass skipped, job state untouched", attrOutcome("skipped"))
		return rep, nil
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		_ = w.st.ReleaseJobLease(rctx, SchedulerJob, w.holder)
	}()

	cands, err := w.candidates(ctx)
	if err != nil {
		w.recordJob(ctx, "error", string(restic.ClassInternal), start)
		return rep, w.storeErr(ctx, err)
	}
	classes := make(restic.ErrorCounts)
	for _, c := range cands {
		if ctx.Err() != nil {
			break
		}
		w.step(ctx, c, &rep, classes)
	}
	rep.ErrorClass = ""
	if rep.Failed > 0 {
		rep.ErrorClass = classes.Dominant()
	}
	if rep.M5Due > 0 {
		w.logM5(rep.M5Due)
	}
	status, class := "ok", ""
	if rep.Failed > 0 {
		status, class = "error", string(rep.ErrorClass)
	}
	w.recordJob(ctx, status, class, start)
	return rep, ctx.Err()
}

// leaseTTL is how long one pass may hold the scheduler lease: long enough for a
// run of every per-method timeout, short enough that a crashed worker's lease
// frees itself.
func (w *Worker) leaseTTL() time.Duration {
	ttl := 10 * w.cfg.TickEvery
	if ttl < 10*time.Minute {
		ttl = 10 * time.Minute
	}
	return ttl
}

func (w *Worker) logM5(n int) {
	w.mu.Lock()
	now := w.clock()
	due := now.Sub(w.lastM5Log) >= m5LogEvery
	if due {
		w.lastM5Log = now
	}
	w.mu.Unlock()
	if due {
		w.log.Info("forget, prune and check are scheduled for milestone five and not run by this version", attrCount(n))
	}
}

// recordJob stores the pass in job_state (architecture §7.5: maintenance health
// is job_state rows, which feed C07).
func (w *Worker) recordJob(ctx context.Context, status, class string, start time.Time) {
	end := w.clock()
	next := end.Add(w.cfg.TickEvery)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	err := w.st.RecordJobRun(rctx, SchedulerJob, store.JobRun{
		Status: status, ErrorClass: class, StartedAt: start, FinishedAt: end, NextDueAt: &next,
	})
	if err != nil {
		w.log.Error("job state not recorded")
	}
}

// candidate is one device row of step 0.
type candidate struct {
	deviceID        string
	nextForget      sql.NullInt64
	nextPrune       sql.NullInt64
	nextCheck       sql.NullInt64
	deferredCount   int
	lastUsageSample sql.NullInt64
	// escrowUnverified: the active escrowed key has never been verified
	// (repo_key_escrow.verified_at NULL); lastVerifyAttempt is its last attempt.
	escrowUnverified  bool
	lastVerifyAttempt sql.NullInt64
}

// candidates is architecture §7.3 step 0 (applicability): devices with
// maintenance_schedule.enabled = 1, not retired, whose customer holds escrowed
// keys (customer-held custody has no server-side maintenance, DH22), and whose
// enrollment was confirmed (an unconfirmed device has no repository content).
// The store has no method for this read, so it is a query through store.Read.
func (w *Worker) candidates(ctx context.Context) ([]candidate, error) {
	var out []candidate
	err := w.st.Read(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query(`
			SELECT d.id, m.next_forget_after, m.next_prune_after, m.next_check_after, m.deferred_count,
			       (SELECT MAX(u.sampled_at) FROM usage_samples u WHERE u.device_id = d.id),
			       (e.id IS NOT NULL AND e.verified_at IS NULL), e.last_verify_attempt_at
			FROM maintenance_schedule m
			JOIN devices d   ON d.id = m.device_id
			JOIN customers c ON c.id = d.customer_id
			LEFT JOIN repo_key_escrow e ON e.device_id = d.id AND e.retired_at IS NULL AND e.custody = 'escrowed'
			WHERE m.enabled = 1 AND d.retired_at IS NULL AND d.enroll_confirmed_at IS NOT NULL
			  AND c.key_custody = 'escrowed' AND c.state <> 'archived'
			ORDER BY d.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.deviceID, &c.nextForget, &c.nextPrune, &c.nextCheck, &c.deferredCount, &c.lastUsageSample,
				&c.escrowUnverified, &c.lastVerifyAttempt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// step applies steps 1, 2 and 8 to one candidate, verifies a never-verified
// escrowed key, and counts the M5 operations that are due.
func (w *Worker) step(ctx context.Context, c candidate, rep *CycleReport, classes restic.ErrorCounts) {
	now := w.clock()
	rep.Considered++
	for _, n := range []sql.NullInt64{c.nextForget, c.nextPrune, c.nextCheck} {
		if !n.Valid || n.Int64 <= now.UnixMilli() {
			rep.M5Due++
		}
	}
	if escrowVerifyDue(c, now) {
		w.verifyEscrow(ctx, c.deviceID, rep)
	}
	usageDue := !c.lastUsageSample.Valid || now.Sub(time.UnixMilli(c.lastUsageSample.Int64)) >= w.cfg.UsageInterval
	if !usageDue {
		return
	}

	// Step 1: pre-flight. A running backup or a pending reschedule skips the
	// device; the retry time is 15 to 60 minutes out, jittered per device.
	if w.deferredByPreflight(ctx, c.deviceID, now) {
		rep.Preflight++
		return
	}
	// Per-device lease (maintenance_schedule.lease_*).
	leased, err := w.leaseDevice(ctx, c.deviceID, now)
	if err != nil || !leased {
		return
	}
	defer w.releaseDevice(ctx, c.deviceID)

	// Step 8: usage refresh. Step 2: a lock (exit 11) is a deferral, never a failure.
	stats, err := w.Stats(ctx, adminpath.StatsRequest{
		DeviceRequest: adminpath.DeviceRequest{DeviceID: c.deviceID, RequestID: w.schedulerRequestID()},
		Mode:          restic.StatsRawData,
	})
	var ae *adminpath.Error
	switch {
	case err == nil:
		if err := w.recordUsage(ctx, c.deviceID, stats); err != nil {
			rep.Failed++
			classes.Add(restic.ErrorEvent{Class: restic.ClassInternal})
			return
		}
		rep.Sampled++
		if c.deferredCount > 0 {
			w.setDeferred(ctx, c.deviceID, 0, false)
		}
	case errors.As(err, &ae) && ae.Code == adminpath.CodeLocked:
		rep.Deferred++
		n := c.deferredCount + 1
		w.setDeferred(ctx, c.deviceID, n, true)
		if n >= lockDeferralLimit {
			if _, uerr := w.unlock(ctx, c.deviceID, false, triggerSchedule); uerr == nil {
				rep.Unlocked++
				w.setDeferred(ctx, c.deviceID, 0, false)
			}
		}
	case ctx.Err() != nil:
		return
	default:
		class := restic.ClassUnknown
		if errors.As(err, &ae) {
			switch {
			case ae.Code == adminpath.CodeCustodyCustomerHeld:
				return // the key is the customer's: not a failure (step 0 already filters these)
			case ae.Class != "":
				class = ae.Class
			default:
				class = restic.ClassInternal
			}
		}
		rep.Failed++
		classes.Add(restic.ErrorEvent{Class: class})
		w.log.Warn("usage refresh failed", attrDevice(c.deviceID), attrClass(string(class)))
	}
}

func (w *Worker) schedulerRequestID() string {
	return fmt.Sprintf("sched-%d", w.seq.Add(1))
}

// deferredByPreflight is architecture §7.3 step 1: it reports whether the device
// must be skipped now, and arms the reschedule when it must. The checks are the
// latest heartbeat's service_state = running and an open backup run.
func (w *Worker) deferredByPreflight(ctx context.Context, deviceID string, now time.Time) bool {
	w.mu.Lock()
	until, pending := w.retryAfter[deviceID]
	w.mu.Unlock()
	if pending {
		if now.Before(until) {
			return true
		}
		w.mu.Lock()
		delete(w.retryAfter, deviceID)
		w.mu.Unlock()
	}
	busy := false
	err := w.st.Read(ctx, func(tx *store.Tx) error {
		var state string
		switch err := tx.QueryRow(`SELECT service_state FROM device_presence WHERE device_id = ?`, deviceID).Scan(&state); {
		case err == nil:
			busy = state == "running"
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if busy {
			return nil
		}
		var one int
		switch err := tx.QueryRow(`SELECT 1 FROM runs WHERE device_id = ? AND kind = 'backup' AND finished_at IS NULL LIMIT 1`, deviceID).Scan(&one); {
		case err == nil:
			busy = true
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		return nil
	})
	if err != nil {
		return true // cannot tell: do not touch the repository this pass
	}
	if busy {
		w.mu.Lock()
		w.retryAfter[deviceID] = now.Add(preflightMin + jitter(deviceID, now, preflightSpan))
		w.mu.Unlock()
	}
	return busy
}

// jitter is a stable pseudo-random duration in [0, span) from the device id and
// the time's hour, so a fleet does not retry in lockstep.
func jitter(deviceID string, now time.Time, span time.Duration) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(deviceID))
	_, _ = fmt.Fprintf(h, "/%d", now.Unix()/3600)
	return time.Duration(h.Sum64() % uint64(span))
}

// leaseDevice takes the per-device lease on maintenance_schedule.
func (w *Worker) leaseDevice(ctx context.Context, deviceID string, now time.Time) (bool, error) {
	var got bool
	until := now.Add(w.leaseTTL()).UnixMilli()
	err := w.st.Write(ctx, func(tx *store.Tx) error {
		res, err := tx.Exec(`UPDATE maintenance_schedule SET lease_holder = ?, lease_until = ?, updated_at = ?
			WHERE device_id = ? AND (lease_holder IS NULL OR lease_until <= ? OR lease_holder = ?)`,
			w.holder, until, tx.Now().UnixMilli(), deviceID, now.UnixMilli(), w.holder)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		got = n == 1
		return err
	})
	return got, err
}

func (w *Worker) releaseDevice(ctx context.Context, deviceID string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	_ = w.st.Write(rctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE maintenance_schedule SET lease_holder = NULL, lease_until = NULL, updated_at = ?
			WHERE device_id = ? AND lease_holder = ?`, tx.Now().UnixMilli(), deviceID, w.holder)
		return err
	})
}

// setDeferred writes the lock-deferral counter (architecture §7.3 step 2);
// stamp also sets last_deferred_at.
func (w *Worker) setDeferred(ctx context.Context, deviceID string, n int, stamp bool) {
	err := w.st.Write(ctx, func(tx *store.Tx) error {
		now := tx.Now().UnixMilli()
		if stamp {
			_, err := tx.Exec(`UPDATE maintenance_schedule SET deferred_count = ?, last_deferred_at = ?, updated_at = ? WHERE device_id = ?`,
				n, now, now, deviceID)
			return err
		}
		_, err := tx.Exec(`UPDATE maintenance_schedule SET deferred_count = ?, updated_at = ? WHERE device_id = ?`, n, now, deviceID)
		return err
	})
	if err != nil {
		w.log.Error("deferral counter not written", attrDevice(deviceID))
	}
}

// recordUsage is architecture §7.3 step 8: one usage_samples row per device per
// hour bucket (an idempotent upsert), source restic_stats. stats --mode
// raw-data gives the repository size, the blob count and the snapshot count;
// the restore size is not measured in v0.
func (w *Worker) recordUsage(ctx context.Context, deviceID string, s restic.StatsResult) error {
	return w.st.Write(ctx, func(tx *store.Tx) error {
		now := tx.Now()
		bucket := now.Truncate(time.Hour).UnixMilli()
		_, err := tx.Exec(`INSERT INTO usage_samples (device_id, sampled_at, repo_bytes, restore_size_bytes, snapshot_count, blob_count, source, created_at, updated_at)
			VALUES (?, ?, ?, NULL, ?, ?, 'restic_stats', ?, ?)
			ON CONFLICT(device_id, sampled_at) DO UPDATE SET repo_bytes = excluded.repo_bytes, snapshot_count = excluded.snapshot_count,
				blob_count = excluded.blob_count, updated_at = excluded.updated_at`,
			deviceID, bucket, int64(s.TotalSize), s.SnapshotsCount, int64(s.TotalBlobCount), now.UnixMilli(), now.UnixMilli())
		return err
	})
}
