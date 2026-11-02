// Package jobs is the console's in-process job runner (architecture §5.7): every
// job is a function on a schedule that takes a lease in job_state before it
// runs, records its outcome there when it ends (job_state feeds the health
// banners and /healthz), is isolated from panics and is safe to run twice.
// M0 jobs: htpasswd-sync, heartbeat-ring-trim, restore-purge, session-expire and
// command-expiry.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Job names (job_state.name).
const (
	NameHtpasswdSync      = "htpasswd-sync"
	NameHeartbeatRingTrim = "heartbeat-ring-trim"
	NameRestorePurge      = "restore-purge"
	NameSessionExpire     = "session-expire"
	NameCommandExpiry     = "command-expiry"
)

// Error classes recorded in job_state.last_error_class. They are fixed words:
// an error message never reaches the database or a log line from here.
const (
	ClassPanic         = "internal_panic"
	ClassInternal      = "internal"
	ClassMasterKey     = "master_key_unavailable"
	ClassNewerGen      = "newer_generation"
	ClassTimeout       = "timeout"
	ClassCancelled     = "cancelled"
	ClassStore         = "store"
	ClassIO            = "io"
	defaultLease       = 5 * time.Minute
	jitterFraction     = 0.1
	maxRunErrClassSize = 64
)

// ClassError carries an error class through a job's return value.
type ClassError struct {
	Class string
	Err   error
}

// Error implements error without exposing the wrapped text.
func (e *ClassError) Error() string { return "job failed: " + e.Class }

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *ClassError) Unwrap() error { return e.Err }

func classOf(err error) string {
	var ce *ClassError
	switch {
	case errors.As(err, &ce):
		if len(ce.Class) <= maxRunErrClassSize {
			return ce.Class
		}
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, context.Canceled):
		return ClassCancelled
	case errors.Is(err, store.ErrBusy), errors.Is(err, store.ErrClosed), errors.Is(err, store.ErrCorrupt):
		return ClassStore
	}
	return ClassInternal
}

// Job is one scheduled task.
type Job struct {
	Name  string
	Every time.Duration
	// Lease is how long the lease lasts; a crashed runner's lease expires and the
	// job runs again. Zero means five minutes.
	Lease time.Duration
	// Signal, when non-nil, runs the job early whenever it fires.
	Signal <-chan struct{}
	// Run is the work. It must be idempotent.
	Run func(ctx context.Context) error
}

// Runner schedules jobs.
type Runner struct {
	Store  *store.Store
	Holder string // identifies this process in job_state.lease_holder
	Log    *logx.Logger

	mu     sync.Mutex
	jobs   map[string]Job
	order  []string
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewRunner returns a runner whose leases are held under holder.
func NewRunner(st *store.Store, holder string, log *logx.Logger) *Runner {
	if log == nil {
		log = logx.Default()
	}
	return &Runner{Store: st, Holder: holder, Log: log, jobs: map[string]Job{}}
}

// Add registers a job. It panics on a duplicate name (a programming error).
func (r *Runner) Add(j Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.jobs[j.Name]; dup || j.Name == "" || j.Run == nil || j.Every <= 0 {
		panic("jobs: bad or duplicate job " + j.Name)
	}
	r.jobs[j.Name] = j
	r.order = append(r.order, j.Name)
}

// Names lists the registered jobs in registration order.
func (r *Runner) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// RunOnce runs the named job now under its lease. It returns (false, nil) when
// another holder has the lease, and (true, err) with the job's error otherwise.
// The outcome is recorded in job_state; a panic is recovered and recorded as
// internal_panic.
func (r *Runner) RunOnce(ctx context.Context, name string) (ran bool, err error) {
	r.mu.Lock()
	j, ok := r.jobs[name]
	r.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("jobs: unknown job %q", name)
	}
	lease := j.Lease
	if lease <= 0 {
		lease = defaultLease
	}
	got, lerr := r.Store.AcquireJobLease(ctx, name, r.Holder, lease)
	if lerr != nil {
		return false, lerr
	}
	if !got {
		return false, nil
	}
	defer func() { _ = r.Store.ReleaseJobLease(context.WithoutCancel(ctx), name, r.Holder) }()
	started := r.Store.Now()
	runErr := safeRun(ctx, j.Run)
	finished := r.Store.Now()
	if finished.Before(started) {
		finished = started
	}
	next := finished.Add(j.Every)
	rec := store.JobRun{Status: "ok", StartedAt: started, FinishedAt: finished, NextDueAt: &next}
	if runErr != nil {
		rec.Status, rec.ErrorClass = "error", classOf(runErr)
		r.Log.Warn("job failed", logx.Str("job", name), logx.Class(rec.ErrorClass))
	}
	if rerr := r.Store.RecordJobRun(context.WithoutCancel(ctx), name, rec); rerr != nil {
		r.Log.Error("job state not recorded", logx.Str("job", name), logx.Err(rerr))
	}
	return true, runErr
}

func safeRun(ctx context.Context, f func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &ClassError{Class: ClassPanic}
		}
	}()
	return f(ctx)
}

// Start launches one goroutine per job and returns. Jobs first run after a short
// random delay (so a restart does not stampede), then every Every plus or minus
// 10 % jitter, or when the job's Signal fires. Stop ends them.
func (r *Runner) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancel = cancel
	jobs := make([]Job, 0, len(r.order))
	for _, n := range r.order {
		jobs = append(jobs, r.jobs[n])
	}
	r.mu.Unlock()
	for _, j := range jobs {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.loop(ctx, j)
		}()
	}
}

// Stop cancels the jobs and waits for them to finish.
func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.wg.Wait()
}

func jittered(d time.Duration) time.Duration {
	span := float64(d) * jitterFraction
	return d + time.Duration((rand.Float64()*2-1)*span)
}

func (r *Runner) loop(ctx context.Context, j Job) {
	initial := time.Duration(rand.Int64N(int64(min(j.Every, 5*time.Second)) + 1))
	timer := time.NewTimer(initial)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-j.Signal:
			// run early; the timer keeps its schedule
			if _, err := r.RunOnce(ctx, j.Name); err != nil && ctx.Err() != nil {
				return
			}
			continue
		}
		if _, err := r.RunOnce(ctx, j.Name); err != nil && ctx.Err() != nil {
			return
		}
		timer.Reset(jittered(j.Every))
	}
}
