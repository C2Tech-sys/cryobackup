package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// M3-T4 r3 M2: a backup record is never lost over its version label. The run is
// stored with its outcome and stats; the label is stored as "unknown", and the log
// says so by class, without the value.
func TestR3ARunWithAnUnusableVersionLabelIsStoredAndTheLabelIsNot(t *testing.T) {
	for name, bad := range map[string]string{
		"git describe past an rc, dirty": "v1.0.0-rc1-3-gaf518e2-dirty",
		"a full commit hash":             strings.Repeat("af518e2c", 5),
		"underscore in a tag":            "release_1.2",
		"slash in a tag":                 "release/1.2",
		"a file name":                    "Patient_Chart.docx",
		"a sentence":                     "John Smith MRI.pdf",
		"far too long":                   strings.Repeat("v1", 400),
		"a control character":            "1.4.2\nx",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			var logs syncBuf
			h.svc.Log = logx.New(&logs, logx.LevelDebug)
			e := h.enrolled()
			rec := h.run(restic.OutcomePartial, time.Minute)
			rec.AgentVersion = bad
			rec.Stats.FailedFiles, rec.Stats.TotalFilesProcessed = 3, 1000
			rec.ErrorClass = restic.ClassPermission
			rec.ErrorCounts = []restic.ErrorProjection{{During: "scan", Class: restic.ClassPermission, Count: 3}}
			if err := rec.Validate(); err != nil {
				t.Fatalf("the agent would journal this record as rejected, losing it: %v", err)
			}
			r := h.do("POST", api.RouteRuns, string(e.DeviceToken), api.RunsRequest{Runs: []api.RunRecord{rec}})
			if r.code != 200 {
				t.Fatalf("%d %s", r.code, r.body)
			}
			var out api.RunsResponse
			r.json(t, &out)
			if out.Results[0].Status != api.RunCreated {
				t.Fatalf("result %+v, want created", out.Results[0])
			}
			got, err := h.st.GetRun(context.Background(), store.ScopeAll(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != string(restic.OutcomePartial) || got.ErrorClass != string(restic.ClassPermission) || got.ErrorCounts["permission"] != 3 {
				t.Errorf("the outcome did not survive: %+v", got)
			}
			var stats map[string]any
			if err := json.Unmarshal(got.Stats, &stats); err != nil || stats["failed_files"] != float64(3) || stats["total_files_processed"] != float64(1000) {
				t.Errorf("the stats did not survive: %s %v", got.Stats, err)
			}
			if got.AgentVersion != "unknown" {
				t.Errorf("the stored version is %q, want unknown", got.AgentVersion)
			}
			out2 := logs.String()
			if !strings.Contains(out2, `"class":"agent_version_unusable"`) {
				t.Errorf("no class in the log: %s", out2)
			}
			for _, leak := range []string{bad, "Patient", "John", "rc1-3", "af518e2c"} {
				if strings.Contains(out2, leak) {
					t.Errorf("the log carries the value (%q): %s", leak, out2)
				}
			}
		})
	}
}

// A good label is kept as it came, and no class is logged.
func TestR3AGoodVersionLabelIsKept(t *testing.T) {
	h := newHarness(t)
	var logs syncBuf
	h.svc.Log = logx.New(&logs, logx.LevelDebug)
	e := h.enrolled()
	rec := h.run(restic.OutcomeSuccess, time.Minute)
	rec.AgentVersion = "v1.2.0-3-gabc1234-dirty"
	if r := h.do("POST", api.RouteRuns, string(e.DeviceToken), api.RunsRequest{Runs: []api.RunRecord{rec}}); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	got, err := h.st.GetRun(context.Background(), store.ScopeAll(), rec.ID)
	if err != nil || got.AgentVersion != rec.AgentVersion {
		t.Fatalf("%q %v", got.AgentVersion, err)
	}
	if strings.Contains(logs.String(), "agent_version_unusable") {
		t.Error("a good label was logged as unusable")
	}
}
