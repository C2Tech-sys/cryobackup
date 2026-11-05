package adminapi

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
)

// M3-STORE1 E: a worker that cannot read its database twice in a row shows on
// /system/health as the database row failed, by the existing C11 mapping, and the
// storage row is not failed. A clean probe clears it.
func TestC17SystemHealthDatabaseRowFailsOnAWorkerStoreStreak(t *testing.T) { // M3-STORE1 E
	e := newEnv(t)
	owner := e.login("admin", pw)
	be := &fixtures.Backend{}
	hp, _ := e.realProber(be, "console-a")
	row := func(id string) SystemComponentJSON {
		t.Helper()
		h := decode[SystemHealthResponse](t, mustGet(e, "/api/v1/system/health", owner))
		for _, c := range h.Components {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("no %s row in %+v", id, h.Components)
		return SystemComponentJSON{}
	}
	tick := func(errs map[string]error) {
		t.Helper()
		e.clk.Advance(time.Minute)
		be.Errors = errs
		if err := hp.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		hp.WaitAll()
	}
	down := map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeUnavailable, maintain.ProbeRecordClass)}

	tick(down)
	if c := row("database"); c.Status == statusFailed {
		t.Fatalf("one failure: database row %+v, want not failed", c)
	}
	tick(down)
	c := row("database")
	if c.Status != statusFailed || !slices.Contains(c.Banners, "C11") {
		t.Fatalf("two failures: database row %+v, want failed with C11", c)
	}
	if s := row("storage"); s.Status == statusFailed || slices.Contains(s.Banners, "C02") {
		t.Fatalf("two failures: storage row %+v, want not checked, never failed", s)
	}
	tick(nil)
	if c := row("database"); c.Status == statusFailed {
		t.Fatalf("after a clean probe: database row %+v, want not failed", c)
	}
}
