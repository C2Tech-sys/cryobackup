package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/diag"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/api"
)

// L-STATUS-1 (b) end to end: a new device under a startup delay is "due now"
// by the schedule, but the running agent waits; it stores its planned start,
// "cryo-agent status" (diag.Status, another reader of the journal) shows it,
// and a clean stop removes it.
func TestStatusShowsTheRunningAgentsPlannedStart(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	pol := testPolicy()
	pol.Schedule.StartupDelayS = 3600
	fc.setPolicy(api.PolicyResponse{Policy: pol, PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`, ServerTime: time.Now()})
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	r := start(t, fakeOptions(t, dir))

	var ps journal.PlannedStart
	waitFor(t, "the planned start in the journal", 15*time.Second, func() bool {
		jr := openJournalRO(t, dir)
		raw, ok, _ := jr.KVGet(ctx, journal.KVPlannedStart)
		return ok && json.Unmarshal([]byte(raw), &ps) == nil
	})
	if ps.Reason != scheduler.ReasonStartupDelay || !ps.At.Equal(r.a.boot.Add(time.Hour).UTC()) {
		t.Fatalf("planned start = %+v, want the end of the startup delay %v", ps, r.a.boot.Add(time.Hour).UTC())
	}
	rep, err := diag.Status(ctx, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep.Render(&out)
	if want := "next due:          " + ps.At.Format(time.RFC3339) + " (startup_delay, as the running agent plans it)"; !strings.Contains(out.String(), want) {
		t.Fatalf("status lacks %q:\n%s", want, out.String())
	}

	r.stop()
	jr := openJournalRO(t, dir)
	if _, ok, _ := jr.KVGet(ctx, journal.KVPlannedStart); ok {
		t.Fatal("the planned start survived a clean stop")
	}
}
