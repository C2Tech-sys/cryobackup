package server

import (
	"context"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/migrations"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// StoreHealth builds the /healthz function for a store and master key: a
// database read, the applied schema version against the newest embedded
// migration, the master key and the last status of every job (store.IsJobRow:
// the health-prober's lease and per-probe rows are not jobs). Nothing in the
// answer is a path or a name from the data.
func StoreHealth(st *store.Store, master *escrow.MasterKey, fixtures bool) func(context.Context) Health {
	return func(ctx context.Context) Health {
		h := Health{Status: "ok", Database: "ok", Migrations: "current", MasterKey: "loaded", FixturesMode: fixtures}
		v, err := st.SchemaVersion(ctx)
		if err != nil {
			h.Database, h.Migrations, h.Status = "unavailable", "unknown", "degraded"
		} else if want := newestMigration(); want == 0 || v != want {
			h.Migrations, h.Status = "behind_or_ahead", "degraded"
		}
		if !master.Loaded() {
			h.MasterKey, h.Status = "unavailable", "degraded"
		}
		if jobs, err := st.ListJobStates(ctx); err == nil && len(jobs) > 0 {
			h.Jobs = map[string]string{}
			for _, j := range jobs {
				if !store.IsJobRow(j.Name) {
					continue // the health-prober's lease and per-probe rows are not jobs
				}
				status := j.LastStatus
				if status == "" {
					status = "never_ran"
				}
				h.Jobs[j.Name] = status
			}
		}
		return h
	}
}

func newestMigration() int {
	ms, err := migrations.List()
	if err != nil || len(ms) == 0 {
		return 0
	}
	max := 0
	for _, m := range ms {
		if m.Version > max {
			max = m.Version
		}
	}
	return max
}
