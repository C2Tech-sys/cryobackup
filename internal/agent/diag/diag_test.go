package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func render(r Report) string {
	var b bytes.Buffer
	r.Render(&b)
	return b.String()
}

func TestNotEnrolled(t *testing.T) {
	rep, err := Status(context.Background(), filepath.Join(t.TempDir(), "state"), now)
	if err != nil || rep.Enrolled {
		t.Fatalf("%+v %v", rep, err)
	}
	if out := render(rep); !strings.Contains(out, "no (run \"cryo-agent enroll\")") {
		t.Errorf("output:\n%s", out)
	}
}

// stubVerdict makes Status see the given verdict (the real one is HKLM on
// Windows, which an unelevated test cannot write).
func stubVerdict(t *testing.T, v config.Verdict, present bool, err error) {
	t.Helper()
	old := readVerdict
	t.Cleanup(func() { readVerdict = old })
	readVerdict = func(string) (config.Verdict, bool, error) { return v, present, err }
}

// After a refusal or a quarantine the device is not enrolled, and status still
// says why (no path: the verdict's stamp and reason, quarantine stamps only).
func TestStatusShowsTheUntrustedVerdictAndQuarantines(t *testing.T) {
	layout := config.Layout{Root: filepath.Join(t.TempDir(), "state")}
	since := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)
	stubVerdict(t, config.Verdict{Since: since, Reason: "secrets: owner S-1-5-21-1"}, true, nil)
	for _, id := range []string{"20261021T090000Z", "20261022T120000Z-0a1b2c3d"} {
		if err := os.Mkdir(filepath.Join(filepath.Dir(layout.Root), "state.quarantine-"+id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Status(context.Background(), layout.Root, now)
	if err != nil || rep.Enrolled || !rep.Untrusted || !rep.UntrustedSince.Equal(since) || rep.Quarantines != 2 || rep.NewestQuarantine != "20261022T120000Z-0a1b2c3d" {
		t.Fatalf("%+v %v", rep, err)
	}
	out := render(rep)
	for _, want := range []string{
		"state trust:       UNTRUSTED (" + config.VerdictSource + " since 2026-10-22T15:30:00Z: secrets: owner S-1-5-21-1)",
		"--repair-acl", "2 quarantined state directories", "newest 20261022T120000Z-0a1b2c3d", "enrolled:",
		"state verdict:     " + config.VerdictLocation(layout.Root) + " (the --state-dir given)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, filepath.Dir(layout.Root)) {
		t.Errorf("status names a path:\n%s", out)
	}
}

// A verdict that cannot be read is shown as untrusted (fail loudly), and no
// verdict shows no trust line.
func TestStatusVerdictUnreadableAndAbsent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	stubVerdict(t, config.Verdict{}, false, errors.New("Access is denied."))
	rep, err := Status(context.Background(), dir, now)
	if err != nil || rep.VerdictErr == "" {
		t.Fatalf("%+v %v", rep, err)
	}
	if out := render(rep); !strings.Contains(out, "state trust:       UNTRUSTED (the "+config.VerdictSource+" cannot be read: Access is denied.)") {
		t.Errorf("an unreadable verdict is not shown as untrusted:\n%s", out)
	}
	stubVerdict(t, config.Verdict{}, false, nil)
	rep, err = Status(context.Background(), dir, now)
	if err != nil || rep.Untrusted || rep.VerdictErr != "" {
		t.Fatalf("%+v %v", rep, err)
	}
	if out := render(rep); strings.Contains(out, "state trust") {
		t.Errorf("a trust line without a verdict:\n%s", out)
	}
	// The default state directory's verdict is named as such (one verdict per
	// directory).
	var checked string
	readVerdict = func(root string) (config.Verdict, bool, error) {
		checked = root
		return config.Verdict{Since: now, Reason: "root: owner S-1-5-21-1"}, true, nil
	}
	rep, err = Status(context.Background(), "", now)
	if err != nil || !rep.DefaultStateDir || config.VerdictID(checked) != config.VerdictID(config.DefaultStateDir()) {
		t.Fatalf("default: %+v %v (checked id %s)", rep, err, config.VerdictID(checked))
	}
	if out := render(rep); !strings.Contains(out, "(the default state directory)") || strings.Contains(out, config.DefaultStateDir()) {
		t.Errorf("status does not name the default state directory's verdict, or names its path:\n%s", out)
	}
}

func TestStatusFromAJournal(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	layout := config.Layout{Root: dir}
	if _, err := layout.Ensure(config.EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ServerURL: "https://console.example:8443", DeviceID: "01JB0000000000000000000001", Custody: "escrowed"}
	if err := config.Save(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	clk := func() time.Time { return now }
	jr, err := journal.Open(ctx, journal.Options{Path: layout.JournalFile(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.Default(policy.ClassDesktop)
	pol.Selection.Roots, pol.Selection.Presets = []string{`C:\Users\PHI-sentinel\Documents`}, []string{}
	raw, _ := json.Marshal(api.PolicyResponse{Policy: pol, Version: 5, ETag: `"e5"`})
	_ = jr.KVSet(ctx, journal.KVPolicyJSON, string(raw))
	_ = jr.KVSet(ctx, journal.KVPolicyETag, `"e5"`)
	_ = jr.KVSet(ctx, journal.KVLastHeartbeat, now.Add(-30*time.Second).Format(time.RFC3339))
	_ = jr.KVSet(ctx, journal.KVClockOffsetMS, "-1200")
	_ = jr.KVSet(ctx, journal.KVLastErrorClass, "wrong_password")
	_ = jr.KVSet(ctx, journal.KVInsecureState, "--insecure-unprotected-state")

	start := now.Add(-2 * time.Hour)
	rec := api.RunRecord{ID: "01JB000000000000000000000A", Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: start, Attempt: 2, PolicyVersion: 5, AgentVersion: "t"}
	if err := jr.StartRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	fin := start.Add(time.Minute)
	rec.FinishedAt, rec.Outcome, rec.ErrorClass = &fin, restic.OutcomeFailed, restic.ClassWrongPassword
	if err := jr.FinishRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	_ = jr.Close()

	rep, err := Status(ctx, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Enrolled || rep.PolicyVersion != 5 || rep.QueueDepth != 1 || rep.LastRun == nil || rep.LastRun.Outcome != "failed" || !rep.LastSuccess.IsZero() {
		t.Fatalf("report = %+v", rep)
	}
	if !rep.HaveClockOffset || rep.ClockOffsetMS != -1200 {
		t.Errorf("clock offset = %d %v", rep.ClockOffsetMS, rep.HaveClockOffset)
	}
	out := render(rep)
	for _, want := range []string{
		"01JB0000000000000000000001", "console.example:8443", "version 5", `"e5"`, "failed (wrong_password)",
		"attempt 2", "last success:      never", "now (retry)", "1 undelivered", "last error class:  wrong_password", "-1200 ms",
		"state acl:         NOT VERIFIED", "insecure state:    the agent last started with --insecure-unprotected-state",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Path-free: not the state directory, not the policy's roots.
	for _, bad := range []string{dir, "PHI-sentinel", `C:\Users`} {
		if strings.Contains(out, bad) {
			t.Errorf("output carries %q:\n%s", bad, out)
		}
	}
}
