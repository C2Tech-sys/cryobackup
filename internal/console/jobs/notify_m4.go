package jobs

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The M4 notification jobs (ADR 0020 decision 9). The alerting itself is a step of the
// alarm-evaluator (AlarmEvaluator.Notify); notification-retry sends again what failed.
const (
	NameNotificationRetry = "notification-retry"
	// NotificationRetryEvery is the notification-retry period: the shortest retry delay (1 minute).
	NotificationRetryEvery = time.Minute
)

// NotificationRetry is the notification-retry job: every minute, notify.Retrier.Run over the
// delivery rows that are due. A store error is the run's error; a send failure is in the row.
type NotificationRetry struct {
	Retrier *notify.Retrier
}

// Job returns the schedule for the runner. The lease outlives one run's send budget.
func (j *NotificationRetry) Job() Job {
	return Job{Name: NameNotificationRetry, Every: NotificationRetryEvery, Lease: 2 * time.Minute, Run: func(ctx context.Context) error {
		_, err := j.Retrier.Run(ctx)
		return err
	}}
}

// NotifyStep adapts a Notifier to AlarmEvaluator.Notify.
func NotifyStep(n *notify.Notifier) func(ctx context.Context, v state.FleetView) error {
	return func(ctx context.Context, v state.FleetView) error {
		_, err := n.Pass(ctx, v)
		return err
	}
}
