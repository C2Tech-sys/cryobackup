package agentapi

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestT5ConcurrentDuplicateDeliveriesYieldOneRow: the same run id arrives
// several times at once (an agent whose first request is still in the console
// when its retry timer fires, or two requests of a restarted agent). Exactly
// one is "created", the others "duplicate", and the store holds one row with
// the counts of the one record, not a sum. The sequential half of the M1
// acceptance (the answer lost after the commit) is in internal/agent
// (TestT5DuplicateDeliveryAfterALostResponse), against this same handler.
func TestT5ConcurrentDuplicateDeliveriesYieldOneRow(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	ctx := context.Background()

	rec := h.run(restic.OutcomePartial, 5*time.Minute)
	rec.Stats.FailedFiles = 3
	rec.ErrorClass = restic.ClassPermission
	rec.ErrorCounts = []restic.ErrorProjection{{During: "archival", Class: restic.ClassPermission, Ext: "docx", Count: 3}}
	rec.SnapshotID = strings.Repeat("cd", 32)

	const n = 8 // below the runs limiter's burst of 30
	results := make([]api.RunStatus, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := h.do("POST", api.RouteRuns, tok, api.RunsRequest{Runs: []api.RunRecord{rec}})
			var out api.RunsResponse
			if r.code != 200 {
				t.Errorf("delivery %d: %d %s", i, r.code, r.body)
				return
			}
			r.json(t, &out)
			if len(out.Results) != 1 {
				t.Errorf("delivery %d: %+v", i, out.Results)
				return
			}
			results[i] = out.Results[0].Status
		}()
	}
	close(start)
	wg.Wait()

	created, duplicate := 0, 0
	for _, s := range results {
		switch s {
		case api.RunCreated:
			created++
		case api.RunDuplicate:
			duplicate++
		}
	}
	if created != 1 || duplicate != n-1 {
		t.Fatalf("answers = %v, want one created and %d duplicate", results, n-1)
	}
	l, err := h.st.ListRuns(ctx, store.ScopeAll(), e.DeviceID, store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, r := range l.Items {
		if r.ID == rec.ID {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("the store holds %d rows for the run id", rows)
	}
	got, err := h.st.GetRun(ctx, store.ScopeAll(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "partial" || got.ErrorCounts["permission"] != 3 || got.SnapshotID != rec.SnapshotID {
		t.Errorf("stored run = %+v", got)
	}
}
