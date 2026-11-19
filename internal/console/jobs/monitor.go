package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The M3 monitoring jobs besides the health-prober (architecture §5.7).
const (
	NameAlarmEvaluator      = "alarm-evaluator"
	NameAbandonedRunSweeper = "abandoned-run-sweeper"
	NameHealthcheckPing     = "healthcheck-ping"
	// NameAlarmEvaluatorOK is the job_state row the alarm-evaluator writes only after an evaluation
	// that succeeded: its last_finished_at is the newest successful evaluation, which is what C06 reads
	// (a loop that runs but errors every time is stalled alerting, not healthy alerting).
	NameAlarmEvaluatorOK = NameAlarmEvaluator + ".ok"
)

// Schedules (architecture §5.7).
const (
	// AlarmEvaluatorEvery is the alarm-evaluator period; banner C06 fires after three of them.
	AlarmEvaluatorEvery = time.Minute
	// AbandonedRunSweeperEvery is the abandoned-run-sweeper period.
	AbandonedRunSweeperEvery = 5 * time.Minute
	// DefaultHealthcheckPingEvery is the healthcheck-ping default period.
	DefaultHealthcheckPingEvery = 5 * time.Minute
	// EvaluatorHealthyPeriods: the evaluator is healthy while its newest finish is within this many periods.
	EvaluatorHealthyPeriods = 3
)

// ClassEvaluatorUnhealthy is healthcheck-ping's error class when it withheld the ping (HeartbeatPing,
// healthping_m4.go).
const ClassEvaluatorUnhealthy = "evaluator_unhealthy"

// AlarmEvaluator is the alarm-evaluator loop (architecture §5.7, "loop from
// M3"): every 60 s it evaluates the whole fleet and records its run in
// job_state, which is what banner C06 and healthcheck-ping read. Since M4
// (ADR 0020) the pass also notifies: Notify syncs the alarm episodes with the
// view and mails what is due. Mail trouble never fails the pass (it ends in the
// delivery log and banner C03); a store error in the notify step does, so
// alerting that cannot read its own state shows as stalled (C06).
type AlarmEvaluator struct {
	// Store receives the NameAlarmEvaluatorOK row after each successful evaluation.
	Store *store.Store
	// View runs one evaluation and returns the view (health.Evaluator.Alarms); an error is the run's
	// error class. The view is handed to Notify.
	View func(ctx context.Context) (state.FleetView, error)
	// Notify is the alerting step of the pass (notify.Notifier.Pass); nil means none. Its error is the
	// run's error, and the evaluation does not count as successful.
	Notify func(ctx context.Context, v state.FleetView) error
}

// Job returns the schedule for the runner.
func (j *AlarmEvaluator) Job() Job {
	return Job{Name: NameAlarmEvaluator, Every: AlarmEvaluatorEvery, Lease: 2 * time.Minute, Run: j.Run}
}

// Run is one evaluation and its notify step; a successful one also stamps NameAlarmEvaluatorOK.
func (j *AlarmEvaluator) Run(ctx context.Context) error {
	start := j.Store.Now()
	v, err := j.View(ctx)
	if err != nil {
		return err
	}
	if j.Notify != nil {
		if err := j.Notify(ctx, v); err != nil {
			return err
		}
	}
	end := j.Store.Now()
	if end.Before(start) {
		end = start
	}
	return j.Store.RecordJobRun(context.WithoutCancel(ctx), NameAlarmEvaluatorOK, store.JobRun{Status: "ok", StartedAt: start, FinishedAt: end})
}

// AbandonedRunSweeper closes open agent runs the device stopped reporting
// (store.SweepAbandonedRuns) every five minutes.
type AbandonedRunSweeper struct {
	Store *store.Store
	// Options tune the sweep; the zero value takes the store defaults.
	Options store.SweepOptions
}

// Job returns the schedule for the runner.
func (j *AbandonedRunSweeper) Job() Job {
	return Job{Name: NameAbandonedRunSweeper, Every: AbandonedRunSweeperEvery, Run: func(ctx context.Context) error {
		_, err := j.Store.SweepAbandonedRuns(ctx, j.Options)
		return err
	}}
}

// EvaluatorHealthy reports whether the alarm-evaluator's newest run finished ok
// within periods x period of now.
func EvaluatorHealthy(ctx context.Context, st *store.Store, period time.Duration, now time.Time) (bool, error) {
	if period <= 0 {
		period = AlarmEvaluatorEvery
	}
	j, err := st.GetJobState(ctx, NameAlarmEvaluator)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if j.LastFinishedAt == nil || j.LastStatus != "ok" {
		return false, nil
	}
	age := now.Sub(*j.LastFinishedAt)
	return age >= -time.Minute && age <= time.Duration(EvaluatorHealthyPeriods)*period, nil
}
