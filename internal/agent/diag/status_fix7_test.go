package diag

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// L-STATUS-1 (a): with no untrusted verdict, "state verdict" says none; the
// registry location is printed only when a verdict is set (or unreadable).
func TestStatusVerdictNoneWithoutAVerdict(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	stubVerdict(t, config.Verdict{}, false, nil)
	rep, err := Status(context.Background(), dir, now)
	if err != nil {
		t.Fatal(err)
	}
	out := render(rep)
	if !strings.Contains(out, "state verdict:     none\n") {
		t.Errorf("no verdict is not shown as none:\n%s", out)
	}
	if strings.Contains(out, config.VerdictLocation(dir)) {
		t.Errorf("the verdict location is printed although no verdict exists:\n%s", out)
	}
}

// enrolledWithPolicy writes agent.json and a journal holding a policy and no
// run (a new device), and returns the state directory and the open journal.
func enrolledWithPolicy(t *testing.T) (string, *journal.Journal) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	layout := config.Layout{Root: dir}
	if _, err := layout.Ensure(config.EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(layout.ConfigFile(), config.Config{ServerURL: "https://console.example:8443", DeviceID: "01JB0000000000000000000001", Custody: "escrowed"}); err != nil {
		t.Fatal(err)
	}
	jr, err := journal.Open(ctx, journal.Options{Path: layout.JournalFile(), Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	raw, _ := json.Marshal(api.PolicyResponse{Policy: policy.Default(policy.ClassDesktop), Version: 1})
	if err := jr.KVSet(ctx, journal.KVPolicyJSON, string(raw)); err != nil {
		t.Fatal(err)
	}
	return dir, jr
}

// L-STATUS-1 (b): a new device is "due now" by the schedule alone, but the
// running agent waits out its jitter; status shows the planned start the
// agent stored, and falls back to the schedule when there is no plan or the
// plan is in the past.
func TestStatusShowsThePlannedStart(t *testing.T) {
	ctx := context.Background()
	stubVerdict(t, config.Verdict{}, false, nil)
	dir, jr := enrolledWithPolicy(t)

	rep, err := Status(ctx, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(rep); !strings.Contains(out, "next due:          now (") {
		t.Fatalf("without a plan the schedule's estimate is shown:\n%s", out)
	}

	at := now.Add(17*time.Minute + 30*time.Second)
	b, _ := json.Marshal(journal.PlannedStart{At: at, Reason: "jitter"})
	if err := jr.KVSet(ctx, journal.KVPlannedStart, string(b)); err != nil {
		t.Fatal(err)
	}
	rep, err = Status(ctx, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if out := render(rep); !strings.Contains(out, "next due:          2026-10-22T12:17:30Z (jitter, as the running agent plans it)") {
		t.Fatalf("the planned start is not shown:\n%s", out)
	}

	// A plan already in the past (the agent stopped without clearing it) is
	// not believed.
	b, _ = json.Marshal(journal.PlannedStart{At: now.Add(-time.Minute), Reason: "jitter"})
	_ = jr.KVSet(ctx, journal.KVPlannedStart, string(b))
	rep, _ = Status(ctx, dir, now)
	if !rep.PlannedAt.IsZero() || !strings.Contains(render(rep), "next due:          now (") {
		t.Fatalf("a past plan was shown: %+v", rep)
	}
}
