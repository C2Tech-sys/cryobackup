package health

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P22-F: the loader reads the self-backup from the maintenance worker's rows
// (store.SelfBackupFacts and the db-self-backup job row) instead of marking it
// not in this build. Each test writes the rows the worker writes and reads the
// C11 banner the fixture board then shows (everything else on that board is clear).

// selfEval is the fixture evaluator with the prober's facts as the prober supplies
// them: the Database check present, its SelfBackup left for the loader.
func selfEval(t *testing.T) (*Evaluator, *clk) {
	t.Helper()
	e, c := newEval(t)
	src := FixtureProbes(c.now)
	e.Probes = func() state.ProbeFacts {
		p := src()
		d := *p.Database
		d.SelfBackup = nil
		p.Database = &d
		return p
	}
	return e, c
}

// c11 returns the C11 banner of the board (zero value when none) and fails on any
// other banner.
func c11(t *testing.T, e *Evaluator) state.Banner {
	t.Helper()
	f, err := e.Fleet(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	var out state.Banner
	for _, b := range f.View.Banners {
		if b.ID != "C11" {
			t.Fatalf("unexpected banner %s: %q", b.ID, b.Text)
		}
		out = b
	}
	return out
}

func recordBackup(t *testing.T, st *store.Store, at, first time.Time) {
	t.Helper()
	if err := st.RecordSelfBackup(context.Background(), store.SelfBackupLast{At: at.UnixMilli(), FirstAt: first.UnixMilli(), SnapshotID: "0123abcd"}); err != nil {
		t.Fatal(err)
	}
}

func recordVerify(t *testing.T, st *store.Store, v store.SelfBackupVerify) {
	t.Helper()
	if err := st.RecordSelfBackupVerify(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func recordSelfJob(t *testing.T, st *store.Store, status, class string, at time.Time) {
	t.Helper()
	if err := st.RecordJobRun(context.Background(), maintain.SelfBackupJob, store.JobRun{Status: status, ErrorClass: class, StartedAt: at, FinishedAt: at}); err != nil {
		t.Fatal(err)
	}
}

func wantC11(t *testing.T, b state.Banner, sev state.Severity, parts ...string) {
	t.Helper()
	if b.ID != "C11" || b.Severity != sev {
		t.Fatalf("C11 = %+v, want severity %v", b, sev)
	}
	for _, p := range parts {
		if !strings.Contains(b.Text, p) {
			t.Errorf("C11 text %q lacks %q", b.Text, p)
		}
	}
}

const day = 24 * time.Hour

// The store and the engine name the same staleness (store.SelfBackupVerifyStale is
// what the worker's tests check; state.DBSelfBackupVerifyMaxAge is what C11 judges).
func TestSelfBackupVerifyStaleAgrees(t *testing.T) {
	if state.DBSelfBackupVerifyMaxAge != store.SelfBackupVerifyStale {
		t.Fatalf("state %v, store %v", state.DBSelfBackupVerifyMaxAge, store.SelfBackupVerifyStale)
	}
}

// Rule 1: no row is the existing "never written" alarm; the job row's
// not_configured or no_slot class is in the text.
func TestSelfBackupLoaderNeverWritten(t *testing.T) {
	e, c := selfEval(t)
	wantC11(t, c11(t, e), state.SeverityAlarm, "no self-backup has been written")

	recordSelfJob(t, e.Store, "error", "not_configured", c.now().Add(-time.Minute))
	wantC11(t, c11(t, e), state.SeverityAlarm, "no self-backup has been written (not_configured: CRYO_MAINTAIN_SELF_PREFIX is not set)")

	recordSelfJob(t, e.Store, "error", "no_slot", c.now())
	wantC11(t, c11(t, e), state.SeverityAlarm, "no self-backup has been written (no_slot: CRYO_MAINTAIN_MAX_STREAMS")
}

// Rule 2: a failed verdict is critical at once.
func TestSelfBackupLoaderVerifyFailed(t *testing.T) {
	e, c := selfEval(t)
	now := c.now()
	recordBackup(t, e.Store, now.Add(-time.Hour), now.Add(-2*day))
	recordVerify(t, e.Store, store.SelfBackupVerify{At: now.Add(-time.Hour).UnixMilli(), OK: false, Class: "manifest", SnapshotID: "0123abcd", AttemptAt: now.Add(-time.Hour).UnixMilli()})
	wantC11(t, c11(t, e), state.SeverityCritical, "self-backup verify failed")
}

// Rule 3: no verdict for more than 8 days, from the newest verdict, naming the
// class of the later attempt; and, with no verdict yet, from the first backup.
func TestSelfBackupLoaderVerdictAge(t *testing.T) {
	e, c := selfEval(t)
	now := c.now()
	recordBackup(t, e.Store, now.Add(-time.Hour), now.Add(-30*day))
	recordVerify(t, e.Store, store.SelfBackupVerify{At: now.Add(-2 * day).UnixMilli(), OK: true, SnapshotID: "0123abcd", AttemptAt: now.Add(-2 * day).UnixMilli()})
	if b := c11(t, e); b.ID != "" {
		t.Fatalf("a fresh backup and a verdict two days old raised %+v", b)
	}

	recordVerify(t, e.Store, store.SelfBackupVerify{At: now.Add(-9 * day).UnixMilli(), OK: true, SnapshotID: "0123abcd",
		AttemptAt: now.Add(-time.Hour).UnixMilli(), AttemptClass: "network"})
	wantC11(t, c11(t, e), state.SeverityCritical, "self-backup not verified for 9 days (latest attempt: network)")

	e2, c2 := selfEval(t)
	now = c2.now()
	recordBackup(t, e2.Store, now.Add(-time.Hour), now.Add(-9*day))
	recordVerify(t, e2.Store, store.SelfBackupVerify{AttemptAt: now.Add(-3 * time.Hour).UnixMilli(), AttemptClass: "no_slot"})
	wantC11(t, c11(t, e2), state.SeverityCritical, "self-backup never verified in the 9 days since the first backup (latest attempt: no_slot)")
}

// The storage always unreachable: every run fails with a storage class, LastAt goes
// stale, and the existing 26 h alarm says so with the class (a warning while the
// verdict is fresh, not critical).
func TestSelfBackupLoaderStorageUnreachable(t *testing.T) {
	e, c := selfEval(t)
	now := c.now()
	recordBackup(t, e.Store, now.Add(-30*time.Hour), now.Add(-30*day))
	recordVerify(t, e.Store, store.SelfBackupVerify{At: now.Add(-3 * day).UnixMilli(), OK: true, SnapshotID: "0123abcd", AttemptAt: now.Add(-3 * day).UnixMilli()})
	recordSelfJob(t, e.Store, "error", "network", now.Add(-time.Hour))
	b := c11(t, e)
	wantC11(t, b, state.SeverityAlarm, "self-backup older than 26 h (latest run: network)")
	if strings.Contains(b.Text, "verified") {
		t.Fatalf("a fresh verdict named in %q", b.Text)
	}
	// A later good run clears the class (and, with a fresh backup, the banner).
	recordBackup(t, e.Store, now.Add(-time.Minute), now.Add(-30*day))
	recordSelfJob(t, e.Store, "ok", "", now)
	if b := c11(t, e); b.ID != "" {
		t.Fatalf("after a good run: %+v", b)
	}
}

// A settings row the store cannot read is not supplied (a C11 cause), never quiet;
// a job_state read that failed only leaves the class out.
func TestSelfBackupLoaderUnreadable(t *testing.T) {
	e, _ := selfEval(t)
	// Valid JSON (PutSetting refuses anything else), but not a SelfBackupLast.
	if err := e.Store.PutSetting(context.Background(), store.SettingSelfBackupLast, []byte(`"garbled"`), ""); err != nil {
		t.Fatal(err)
	}
	wantC11(t, c11(t, e), state.SeverityAlarm, "self-backup not supplied")

	e2, c2 := selfEval(t)
	recordSelfJob(t, e2.Store, "error", "no_slot", c2.now())
	jobs, err := e2.Store.ListJobStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := e2.ProbeFacts(jobs, nil, false); p.Database.SelfBackup == nil || p.Database.SelfBackup.JobClass != "no_slot" || p.Database.SelfBackup.NotInBuild {
		t.Fatalf("facts with the job rows = %+v", p.Database.SelfBackup)
	}
	if p := e2.ProbeFacts(jobs, context.DeadlineExceeded, false); p.Database.SelfBackup == nil || p.Database.SelfBackup.JobClass != "" {
		t.Fatalf("facts with a failed job read = %+v", p.Database.SelfBackup)
	}
	// An ok newest run carries no class even after failed ones.
	recordSelfJob(t, e2.Store, "ok", "", c2.now())
	jobs, _ = e2.Store.ListJobStates(context.Background())
	if p := e2.ProbeFacts(jobs, nil, false); p.Database.SelfBackup.JobClass != "" {
		t.Fatalf("class after an ok run = %q", p.Database.SelfBackup.JobClass)
	}
}

// Every field of store.SelfBackupFacts reaches the engine.
func TestSelfBackupLoaderCopiesEveryField(t *testing.T) {
	e, c := selfEval(t)
	now := c.now()
	recordBackup(t, e.Store, now.Add(-time.Hour), now.Add(-20*day))
	recordVerify(t, e.Store, store.SelfBackupVerify{At: now.Add(-5 * day).UnixMilli(), OK: false, Class: "check", SnapshotID: "0123abcd",
		AttemptAt: now.Add(-2 * time.Hour).UnixMilli(), AttemptClass: "busy"})
	sb := e.ProbeFacts(nil, nil, false).Database.SelfBackup
	want := state.SelfBackupFacts{LastAt: state.Millis(now.Add(-time.Hour).UnixMilli()), VerifyFailed: true,
		FirstBackupAt: state.Millis(now.Add(-20 * day).UnixMilli()), VerifyAt: state.Millis(now.Add(-5 * day).UnixMilli()),
		VerifyAttemptAt: state.Millis(now.Add(-2 * time.Hour).UnixMilli()), VerifyAttemptClass: "busy"}
	if sb == nil || *sb != want {
		t.Fatalf("facts %+v, want %+v", sb, want)
	}
}
