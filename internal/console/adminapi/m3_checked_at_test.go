package adminapi

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
)

// M3-POLISH-B: checked_at (the System page's CHECKED column) is the time of the newest attempt that
// produced evidence, a pass or a failure that really ran. A skipped run produced none, and a row that
// is warn or failed shows the failure's time. judgeRow documents the rule; these tests pin it.

// cat is the time d before r5Now, so a table reads as a timeline.
func cat(d time.Duration) time.Time { return r5Now.Add(-d) }

func fmtAt(t *time.Time) string {
	if t == nil {
		return "nil"
	}
	return r5Now.Sub(*t).String() + " ago"
}

func TestCheckedAtIsTheNewestEvidence(t *testing.T) {
	const maxAge = time.Hour
	pass := func(ago time.Duration) attempt {
		return attempt{Result: passed, At: cat(ago), MaxAge: maxAge, What: "a check"}
	}
	fail := func(ago time.Duration) attempt {
		return attempt{Result: failed, At: cat(ago), MaxAge: maxAge, What: "a check", Why: "it failed"}
	}
	// skip is a run at skipAgo that could not run, after a pass at passAgo (0: no pass on record).
	skip := func(skipAgo, passAgo time.Duration) attempt {
		a := attempt{Result: skipped, At: cat(skipAgo), MaxAge: maxAge, What: "a check", Why: "it could not run"}
		if passAgo > 0 {
			a.LastPass = cat(passAgo)
		}
		return a
	}
	// failThenSkip is a failure at failAgo that a skip at skipAgo followed (what withRow builds).
	failThenSkip := func(failAgo, skipAgo time.Duration) attempt {
		a := fail(skipAgo)
		a.FailedAt = cat(failAgo)
		return a
	}
	failAfterPass := fail(time.Minute)
	failAfterPass.LastPass = cat(10 * time.Minute)
	notInBuild := pass(time.Minute)
	notInBuild.Outside, notInBuild.Why = statusNotInBuild, "not here"

	for _, tc := range []struct {
		name     string
		attempts []attempt
		status   string
		want     time.Duration // how long before r5Now checked_at must be; ignored when nilAt
		nilAt    bool
		before   string // what judgeRow said before this ticket, for the record
	}{
		{name: "skipped after a pass keeps the pass time", attempts: []attempt{skip(5*time.Second, 10*time.Minute)}, status: statusOK,
			want: 10 * time.Minute, before: "5s ago (the skipped run)"},
		{name: "a later skip moves nothing", attempts: []attempt{skip(time.Second, 10*time.Minute)}, status: statusOK,
			want: 10 * time.Minute, before: "1s ago"},
		{name: "failed after a pass gives the failure time", attempts: []attempt{failAfterPass}, status: statusWarn,
			want: time.Minute, before: "1m ago"},
		{name: "failed, then skipped: the failure's time, not the skip's", attempts: []attempt{failThenSkip(time.Minute, 5*time.Second)}, status: statusWarn,
			want: time.Minute, before: "1m ago (the earliest)"},
		{name: "pass and fail in one row give the fail time", attempts: []attempt{pass(10 * time.Minute), fail(2 * time.Minute)}, status: statusWarn,
			want: 2 * time.Minute, before: "10m ago (the earliest)"},
		{name: "fail and pass in one row, pass the newer: still the fail time", attempts: []attempt{fail(2 * time.Minute), pass(time.Second)}, status: statusWarn,
			want: 2 * time.Minute, before: "2m ago (the earliest)"},
		{name: "two failures: the newest", attempts: []attempt{fail(2 * time.Minute), fail(30 * time.Second)}, status: statusWarn,
			want: 30 * time.Second, before: "2m ago (the earliest)"},
		{name: "two passes: the newest", attempts: []attempt{pass(10 * time.Minute), pass(time.Minute)}, status: statusOK,
			want: time.Minute, before: "10m ago (the earliest)"},
		{name: "a pass beside a skip: the pass is the newer evidence", attempts: []attempt{pass(time.Minute), skip(5*time.Second, 10*time.Minute)}, status: statusOK,
			want: time.Minute, before: "1m ago (the earliest)"},
		{name: "no attempt gives nil", attempts: []attempt{{Why: "nothing on record"}}, status: statusNotChecked, nilAt: true, before: "nil"},
		{name: "a skip with no pass on record gives nil", attempts: []attempt{skip(5*time.Second, 0)}, status: statusNotChecked, nilAt: true,
			before: "5s ago (the skipped run)"},
		{name: "a check that is not in this build gives nil", attempts: []attempt{notInBuild}, status: statusNotInBuild, nilAt: true, before: "nil"},
		{name: "no attempts gives nil", attempts: nil, status: statusNotChecked, nilAt: true, before: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The order of a row's attempts decides nothing.
			for _, order := range [][]attempt{tc.attempts, reversed(tc.attempts)} {
				status, _, at := judgeRow(r5Now, order, "", false)
				if status != tc.status {
					t.Errorf("status = %s, want %s: a verdict moved", status, tc.status)
				}
				switch {
				case tc.nilAt && at != nil:
					t.Errorf("checked_at = %s, want nil", fmtAt(at))
				case !tc.nilAt && (at == nil || !at.Equal(cat(tc.want))):
					t.Errorf("checked_at = %s, want %s ago", fmtAt(at), tc.want)
				}
			}
			status, _, at := judgeRow(r5Now, tc.attempts, "", false)
			t.Logf("checked_at: before %s, now %s (%s)", tc.before, fmtAt(at), status)
		})
	}
}

// A banner, or C00, changes a row's status and never its checked_at.
func TestCheckedAtIgnoresBanners(t *testing.T) {
	a := []attempt{{Result: passed, At: cat(3 * time.Minute), MaxAge: time.Hour, What: "a check"}}
	for _, tc := range []struct {
		banner   string
		notWired bool
		status   string
	}{{statusWarn, false, statusWarn}, {statusFailed, false, statusFailed}, {"", true, statusNotChecked}} {
		status, _, at := judgeRow(r5Now, a, tc.banner, tc.notWired)
		if status != tc.status || at == nil || !at.Equal(cat(3*time.Minute)) {
			t.Errorf("banner %q, notWired %v: %s at %s, want %s at 3m0s ago", tc.banner, tc.notWired, status, fmtAt(at), tc.status)
		}
	}
}

// The rows, built from facts as the page builds them (withRow, probeAttempt).

// L7: the Storage row while the worker is down. Its probe skips every minute; the row keeps reading OK
// on the last real pass, and CHECKED stays on that pass however many skips follow.
func TestCheckedAtASkippedProbeNeverRefreshesTheRow(t *testing.T) { // M3 lab L7
	for _, skipAgo := range []time.Duration{time.Minute, 30 * time.Second, 5 * time.Second} {
		f := r5Passing()
		f.Probes.Storage = r5Run(3 * time.Minute)
		r5Row(jobs.ProbeRowStorage, "skipped", "busy", skipAgo)(&f)
		c := rowsOf(f)["storage"]
		if c.Status != statusOK {
			t.Fatalf("skipped %s ago: storage = %s %q, want ok on the older pass", skipAgo, c.Status, c.Detail)
		}
		if c.CheckedAt == nil || !c.CheckedAt.Equal(cat(3*time.Minute)) {
			t.Errorf("skipped %s ago: checked_at = %s, want the pass, 3m0s ago", skipAgo, fmtAt(c.CheckedAt))
		}
		t.Logf("storage passed 3m ago, skipped %s ago: checked_at now %s", skipAgo, fmtAt(c.CheckedAt))
	}
}

// A failure that a skip follows: the row says warn, and CHECKED is when the failure ran.
func TestCheckedAtAFailureThenASkipShowsTheFailure(t *testing.T) {
	f := r5Passing()
	f.Probes.Storage = r5Failed("repo_missing") // failed 10 s ago, a pass 70 s ago
	r5Row(jobs.ProbeRowStorage, "skipped", "busy", 2*time.Second)(&f)
	c := rowsOf(f)["storage"]
	if c.Status != statusWarn {
		t.Fatalf("storage = %s %q, want warn naming the failure", c.Status, c.Detail)
	}
	if c.CheckedAt == nil || !c.CheckedAt.Equal(cat(10*time.Second)) {
		t.Errorf("checked_at = %s, want the failure, 10s ago", fmtAt(c.CheckedAt))
	}
	t.Logf("storage failed 10s ago, skipped 2s ago: checked_at before 2s ago (the skip), now %s", fmtAt(c.CheckedAt))
}

// A newer failed row than the probe's facts: the failure's time, not an older pass's (L6's shape).
func TestCheckedAtAnErrorRowIsTheFailureTime(t *testing.T) {
	f := r5Passing()
	f.Probes.Storage = r5Run(3 * time.Minute)
	r5Row(jobs.ProbeRowStorage, "error", "timeout", time.Minute)(&f)
	c := rowsOf(f)["storage"]
	if c.Status != statusWarn {
		t.Fatalf("storage = %s %q, want warn", c.Status, c.Detail)
	}
	if c.CheckedAt == nil || !c.CheckedAt.Equal(cat(time.Minute)) {
		t.Errorf("checked_at = %s, want the failure, 1m0s ago", fmtAt(c.CheckedAt))
	}
}

// A probe that was never supplied has no evidence, and no checked_at.
func TestCheckedAtAProbeThatNeverRanHasNone(t *testing.T) {
	f := r5Passing()
	f.Probes.Storage = nil
	c := rowsOf(f)["storage"]
	if c.Status != statusNotChecked || c.CheckedAt != nil {
		t.Errorf("no storage probe supplied: %s at %s, want not_checked at nil", c.Status, fmtAt(c.CheckedAt))
	}
}
