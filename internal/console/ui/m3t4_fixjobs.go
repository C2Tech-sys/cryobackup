package ui

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// newFixtureJobs is the job runner of fixtures mode: the fictional jobs of
// fixtures.JobNames, registered on a real jobs.Runner. A job does no work:
// running one takes the lease and records an ok run, the same path a real runner
// takes, so "Run now" can be pressed and its audit row, its result in the table
// and the busy check seen. cli registers no job in fixtures mode.
func newFixtureJobs(st *store.Store, log *logx.Logger) jobRunner {
	r := jobs.NewRunner(st, "fixtures", log)
	for _, n := range fixtures.JobNames {
		r.Add(jobs.Job{Name: n, Every: 15 * time.Minute, Run: func(context.Context) error { return nil }})
	}
	return r
}
