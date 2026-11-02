package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// The store keeps a partial run classed unsupported_file (the schema's CHECK
// list includes it since migration 0002) and refuses a class outside the
// vocabulary. The wire-to-store path (POST /runs and its conversion) is tested
// through the real handler in internal/console/agentapi
// (TestUnsupportedFileRunRoundTripsThroughTheRunsHandler).
func TestUnsupportedFileRunIsStoredAndABogusClassIsNot(t *testing.T) {
	w := seed(t)
	end := t0.Add(-time.Hour)
	exit := 3
	added := int64(0)
	in := RunInput{
		ID: runID1, Kind: "backup", Trigger: "schedule", StartedAt: end.Add(-10 * time.Minute), FinishedAt: &end,
		Outcome: "partial", ResticExitCode: &exit, Stats: json.RawMessage(`{"total_files_processed":1000,"failed_files":14}`),
		DataAddedBytes: &added, ErrorCounts: map[string]int{"unsupported_file": 14}, ErrorClass: "unsupported_file",
		Attempt: 1, PolicyVersion: 1, AgentVersion: "0.1.0",
	}
	wantStatus(t, ingest(t, w, w.devA.ID, in)[0], IngestCreated, "")

	got := must[Run](t)(w.s.GetRun(w.ctx, ScopeAll(), runID1))
	if got.Outcome != "partial" || got.ErrorClass != "unsupported_file" || got.ResticExitCode == nil || *got.ResticExitCode != 3 {
		t.Fatalf("stored run = %+v", got)
	}
	if want := map[string]int{"unsupported_file": 14}; !reflect.DeepEqual(got.ErrorCounts, want) {
		t.Fatalf("stored error_counts = %v, want %v", got.ErrorCounts, want)
	}
	f := facts(t, w, w.devA.ID)
	if f.LastBackup == nil || f.LastBackup.ErrorClass != "unsupported_file" || f.LastBackup.ErrorCounts["unsupported_file"] != 14 {
		t.Fatalf("LastBackup facts = %+v", f.LastBackup)
	}
	wantStatus(t, ingest(t, w, w.devA.ID, in)[0], IngestDuplicate, "")

	bad := in
	bad.ID = runID2
	bad.ErrorClass = "unsupported_files"
	if r := ingest(t, w, w.devA.ID, bad)[0]; r.Status != IngestRejected || r.Reason != ReasonInvalidValue {
		t.Fatalf("the store accepted a bogus class: %+v", r)
	}
}
