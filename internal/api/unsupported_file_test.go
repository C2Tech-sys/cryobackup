package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The unsupported_file class (M1-T3) is part of the wire vocabulary: it is in
// Enums() (which a test holds against replica/schema.sql), a run carrying it
// validates and survives a JSON round trip unchanged and path-free, and the other
// two places an error class appears on the wire accept it.
func TestUnsupportedFileOnTheWire(t *testing.T) {
	found := false
	for _, v := range Enums()[EnumErrorClass] {
		found = found || v == "unsupported_file"
	}
	if !found {
		t.Fatal("unsupported_file is not in Enums()[error_class]")
	}

	start := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	exit := 3
	rec := RunRecord{
		ID: "01JAZK3Q8W5N6X7Y8Z9A0B1C2D", Kind: restic.KindBackup, Trigger: "schedule",
		StartedAt: start, FinishedAt: &end, Outcome: restic.OutcomePartial, ExitCode: &exit,
		Stats:       RunStats{TotalFilesProcessed: 1000, FailedFiles: 14},
		ErrorCounts: []restic.ErrorProjection{{During: "archival", Class: restic.ClassUnsupportedFile, Count: 14}},
		ErrorClass:  restic.ClassUnsupportedFile, Attempt: 1, PolicyVersion: 1, AgentVersion: "0.1.0",
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := AssertPathFree(RunsRequest{Runs: []RunRecord{rec}}); err != nil {
		t.Fatalf("AssertPathFree: %v", err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"error_class":"unsupported_file"`) || !strings.Contains(string(raw), `"class":"unsupported_file"`) {
		t.Fatalf("wire JSON does not carry the class: %s", raw)
	}
	var back RunRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, rec) {
		t.Fatalf("round trip changed the record:\n%+v\n%+v", rec, back)
	}

	bad := rec
	bad.ErrorClass = "unsupported"
	if err := bad.Validate(); err == nil {
		t.Fatal("a near-miss class validated")
	}

	hb := okHeartbeat()
	hb.LastErrorClass = restic.ClassUnsupportedFile
	if err := hb.Validate(); err != nil {
		t.Fatalf("heartbeat last_error_class: %v", err)
	}
}
