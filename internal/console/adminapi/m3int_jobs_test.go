package adminapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T2 r3 LOW-6: GET /api/v1/system/jobs lists jobs only: not the
// health-prober's probe lease (a "job that never ran") nor its per-probe rows.
func TestSystemJobsListsJobsOnly(t *testing.T) { // M3-INT C6
	e := newEnv(t)
	owner := e.login("admin", pw)
	ctx := context.Background()
	now := e.clk.Now()
	if ok, err := e.st.AcquireJobLease(ctx, jobs.ProbeLease, "console-a", time.Minute); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	for _, name := range []string{jobs.ProbeRowGateway, jobs.ProbeRowStorage, jobs.NameHealthProber} {
		if err := e.st.RecordJobRun(ctx, name, store.JobRun{Status: "ok", StartedAt: now, FinishedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	jl := decode[JobListResponse](t, mustGet(e, "/api/v1/system/jobs", owner))
	var names []string
	for _, j := range jl.Jobs {
		names = append(names, j.Name)
		if strings.HasPrefix(j.Name, jobs.NameHealthProber+".") {
			t.Errorf("/system/jobs lists %s, which is not a job", j.Name)
		}
	}
	found := false
	for _, n := range names {
		found = found || n == jobs.NameHealthProber
	}
	if !found {
		t.Errorf("/system/jobs does not list the health-prober job itself: %v", names)
	}
}
