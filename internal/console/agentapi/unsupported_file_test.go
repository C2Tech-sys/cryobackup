package agentapi

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// A partial run whose errors are unsupported files travels the real path:
// agent JSON -> POST /runs -> validation -> runInput -> store -> read back and
// facts. It is stored with its class and summed counts, it shows in the run
// list and the facts, a resend is a duplicate, and a class outside the
// vocabulary is still rejected at the wire.
func TestUnsupportedFileRunRoundTripsThroughTheRunsHandler(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	tok := string(e.DeviceToken)
	ctx := context.Background()

	rec := h.run(restic.OutcomePartial, time.Hour)
	exit := 3
	rec.ExitCode = &exit
	rec.Stats.FailedFiles = 14
	rec.ErrorCounts = []restic.ErrorProjection{
		{During: "archival", Class: restic.ClassUnsupportedFile, Ext: "", Count: 12},
		{During: "archival", Class: restic.ClassUnsupportedFile, Ext: "docx", Count: 2},
	}
	rec.ErrorClass = restic.ClassUnsupportedFile

	post := func(runs ...api.RunRecord) api.RunsResponse {
		t.Helper()
		r := h.do("POST", api.RouteRuns, tok, api.RunsRequest{Runs: runs})
		if r.code != 200 {
			t.Fatalf("POST runs: %d %s", r.code, r.body)
		}
		var out api.RunsResponse
		r.json(t, &out)
		if len(out.Results) != len(runs) {
			t.Fatalf("results = %+v", out.Results)
		}
		return out
	}

	if out := post(rec); out.Results[0].Status != api.RunCreated {
		t.Fatalf("ingest: %+v", out.Results[0])
	}
	got, err := h.st.GetRun(ctx, store.ScopeAll(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "partial" || got.ErrorClass != "unsupported_file" || got.ResticExitCode == nil || *got.ResticExitCode != 3 {
		t.Fatalf("stored run = %+v", got)
	}
	if want := map[string]int{"unsupported_file": 14}; !reflect.DeepEqual(got.ErrorCounts, want) {
		t.Fatalf("stored error_counts = %v, want %v (summed by class)", got.ErrorCounts, want)
	}
	list, err := h.st.ListRuns(ctx, store.ScopeAll(), e.DeviceID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range list.Items {
		if r.ID == rec.ID {
			found = r.ErrorClass == "unsupported_file"
		}
	}
	if !found {
		t.Fatalf("ListRuns does not show the run with its class: %+v", list.Items)
	}
	f, err := h.st.LoadDeviceFacts(ctx, store.ScopeAll(), e.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if f.LastBackup == nil || f.LastBackup.ErrorClass != "unsupported_file" || f.LastBackup.ErrorCounts["unsupported_file"] != 14 {
		t.Fatalf("LastBackup facts = %+v", f.LastBackup)
	}

	// Resending is a duplicate, not a second row or a rejection.
	h.clk.Advance(3 * time.Second)
	if out := post(rec); out.Results[0].Status != api.RunDuplicate {
		t.Fatalf("resend: %+v", out.Results[0])
	}

	// A class that is not in the vocabulary is refused at the wire.
	bad := h.run(restic.OutcomePartial, 2*time.Hour)
	bad.ErrorClass = restic.ErrorClass("unsupported_files")
	if out := post(bad); out.Results[0].Status != api.RunRejected || out.Results[0].Reason != api.ReasonInvalid {
		t.Fatalf("bogus class: %+v", out.Results[0])
	}
	if _, err := h.st.GetRun(ctx, store.ScopeAll(), bad.ID); err == nil {
		t.Fatal("a run with a bogus class was stored")
	}
}
