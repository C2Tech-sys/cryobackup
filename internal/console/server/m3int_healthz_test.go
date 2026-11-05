package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/server"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T2 r3 LOW-6: /healthz lists jobs only. The health-prober's probe lease
// (which never records a run, so it read as a job that never ran) and its
// per-probe result rows are not jobs.
func TestHealthzListsJobsOnly(t *testing.T) { // M3-INT C6
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if ok, err := st.AcquireJobLease(ctx, jobs.ProbeLease, "console-a", time.Minute); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	run := store.JobRun{Status: "ok", StartedAt: now, FinishedAt: now}
	for _, name := range []string{jobs.ProbeRowGateway, jobs.ProbeRowStorage, jobs.NameHtpasswdSync, jobs.NameHealthProber} {
		if err := st.RecordJobRun(ctx, name, run); err != nil {
			t.Fatal(err)
		}
	}
	var k [32]byte
	mk, _ := escrow.NewMasterKey(k[:])
	h := server.StoreHealth(st, mk, false)(ctx)
	for _, name := range []string{jobs.ProbeLease, jobs.ProbeRowGateway, jobs.ProbeRowStorage} {
		if _, listed := h.Jobs[name]; listed {
			t.Errorf("/healthz lists %s as a job: %v", name, h.Jobs)
		}
	}
	for _, name := range []string{jobs.NameHtpasswdSync, jobs.NameHealthProber} {
		if h.Jobs[name] != "ok" {
			t.Errorf("/healthz does not list the job %s: %v", name, h.Jobs)
		}
	}
}
