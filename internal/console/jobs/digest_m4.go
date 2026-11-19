package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/notify"
)

// The daily-digest job (ADR 0020 decision 8).
const (
	NameDailyDigest = "daily-digest"
	// DailyDigestEvery is the daily-digest period: the digest time is a minute of the day, so the
	// job looks every minute whether today's digest is due and not yet sent.
	DailyDigestEvery = time.Minute
	// ClassDigestTime is the run's error class when digest_local_time is not a usable HH:MM.
	ClassDigestTime = "digest_time_invalid"
)

// DailyDigest is the daily-digest job: every minute, notify.Digester.Run, which sends today's digest
// once per recipient when it is due in the display zone (catch-up after a stop included; never
// yesterday's) and does nothing otherwise. A store or view error is the run's error; a send failure
// is in the delivery log, and notification-retry sends that recipient's digest again.
type DailyDigest struct {
	Digester *notify.Digester
}

// Job returns the schedule for the runner. The lease outlives one run's send budget.
func (j *DailyDigest) Job() Job {
	return Job{Name: NameDailyDigest, Every: DailyDigestEvery, Lease: 2 * time.Minute, Run: func(ctx context.Context) error {
		_, err := j.Digester.Run(ctx)
		if errors.Is(err, notify.ErrDigestTime) {
			return &ClassError{Class: ClassDigestTime, Err: err}
		}
		return err
	}}
}
