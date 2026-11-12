package scheduler

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M3-L10 (ADR 0018): while the agent is refused, a run the gateway turned away
// (deferred, class auth) is next tried at the policy's interval, counted from
// that deferral, and not on the 5 to 60 minute ladder. Everything else keeps
// the ladder.

func deferredWith(class restic.ErrorClass) func(*journal.RunRow) {
	return func(r *journal.RunRow) {
		r.DeferredReason, r.ErrorClass = restic.DeferredGateway, class
	}
}

// authInput is the planner input after one run that was deferred with class,
// started 10 minutes before t0 (so finished at t0), on top of a success 25
// hours before t0, with the facts computed the way the agent computes them. It
// also returns the deferral's finish time.
func authInput(pol policy.Policy, class restic.ErrorClass, wait bool) (Input, time.Time) {
	rows := []journal.RunRow{
		row("D", t0.Add(-10*time.Minute), restic.OutcomeDeferred, deferredWith(class)),
		row("S", t0.Add(-25*time.Hour), restic.OutcomeSuccess, nil),
	}
	f := FactsFromRuns(rows, pol)
	in := input()
	in.Policy = pol
	in.LastSuccess, in.LastAttemptStart, in.Retry = f.LastSuccess, f.LastAttemptStart, f.Retry
	in.AuthDeferralsWait = wait
	return in, *rows[0].FinishedAt
}

func authPolicy() policy.Policy {
	pol := policy.Default(policy.ClassDesktop)
	pol.Schedule.JitterMinutes = 0
	return pol
}

func TestAuthDeferralWaitsForTheNextSlotWhileRefused(t *testing.T) {
	for _, catchup := range []bool{false, true} {
		pol := authPolicy()
		pol.Schedule.Catchup = catchup
		offset := policy.StableOffset(dev, time.Hour)

		t.Run("refused and auth: the next slot, not the ladder", func(t *testing.T) {
			in, end := authInput(pol, restic.ClassAuth, true)
			// Past the ladder gate (a few minutes after the deferral) the ladder
			// would start the run; the interval does not.
			in.Now = in.Retry.NotBefore.Add(time.Minute)
			out := Plan(in)
			if out.Action != ActionNone || out.Reason != policy.ReasonNotYet {
				t.Fatalf("catchup=%v: %s %q, want none/%s", catchup, out.Action, out.Reason, policy.ReasonNotYet)
			}
			if want := end.Add(24*time.Hour + offset); !out.NextCheck.Equal(want) {
				t.Errorf("NextCheck = %v, want the deferral plus one interval and the device offset: %v", out.NextCheck, want)
			}
			// Once the interval has run out the run starts as a scheduled one.
			in.Now = end.Add(24*time.Hour + offset + time.Minute)
			out = Plan(in)
			if out.Action != ActionStart || out.Reason != ReasonDue || out.Trigger == api.TriggerRetry {
				t.Errorf("after the interval: %s %q trigger %q, want a scheduled start", out.Action, out.Reason, out.Trigger)
			}
		})

		t.Run("refused and another class: the ladder", func(t *testing.T) {
			for _, class := range []restic.ErrorClass{restic.ClassNetwork, restic.ClassTLS, restic.ClassStorage, restic.ClassLock, ""} {
				in, _ := authInput(pol, class, true)
				if !in.Retry.AuthDeferredAt.IsZero() {
					t.Fatalf("class %q marked as an auth deferral", class)
				}
				out := Plan(in)
				if out.Action != ActionNone || out.Reason != ReasonRetryWait || !out.NextCheck.Equal(in.Retry.NotBefore) {
					t.Errorf("class %q: %s %q next %v, want the ladder gate %v", class, out.Action, out.Reason, out.NextCheck, in.Retry.NotBefore)
				}
				in.Now = in.Retry.NotBefore.Add(time.Second)
				if out := Plan(in); out.Action != ActionStart || out.Trigger != api.TriggerRetry {
					t.Errorf("class %q after the gate: %s trigger %q, want a retry", class, out.Action, out.Trigger)
				}
			}
		})

		t.Run("not refused and auth: the ladder", func(t *testing.T) {
			in, _ := authInput(pol, restic.ClassAuth, false)
			if out := Plan(in); out.Action != ActionNone || out.Reason != ReasonRetryWait || !out.NextCheck.Equal(in.Retry.NotBefore) {
				t.Errorf("%s %q next %v, want the ladder gate %v", out.Action, out.Reason, out.NextCheck, in.Retry.NotBefore)
			}
			in.Now = in.Retry.NotBefore.Add(time.Second)
			if out := Plan(in); out.Action != ActionStart || out.Trigger != api.TriggerRetry {
				t.Errorf("after the gate: %s trigger %q, want a retry", out.Action, out.Trigger)
			}
		})
	}
}

// The end of REFUSED needs no state of its own: the same facts with the flag
// cleared are on the ladder again, and a gate that has passed makes the run due
// at the next plan.
func TestAuthDeferralLadderReturnsWhenRefusedEnds(t *testing.T) {
	in, _ := authInput(authPolicy(), restic.ClassAuth, true)
	in.Now = in.Retry.NotBefore.Add(time.Minute)
	if out := Plan(in); out.Action != ActionNone {
		t.Fatalf("refused: %s, want none", out.Action)
	}
	in.AuthDeferralsWait = false
	if out := Plan(in); out.Action != ActionStart || out.Trigger != api.TriggerRetry {
		t.Fatalf("no longer refused: %s trigger %q, want a retry at once", out.Action, out.Trigger)
	}
}

// A device that never succeeded (a fresh enrollment retired before its first
// backup) waits an interval too: "never" is not a reason to knock hourly.
func TestAuthDeferralWaitsEvenWithoutAnySuccess(t *testing.T) {
	pol := authPolicy()
	rows := []journal.RunRow{row("D", t0.Add(-10*time.Minute), restic.OutcomeDeferred, deferredWith(restic.ClassAuth))}
	f := FactsFromRuns(rows, pol)
	in := input()
	in.Policy = pol
	in.LastSuccess, in.LastAttemptStart, in.Retry, in.AuthDeferralsWait = f.LastSuccess, f.LastAttemptStart, f.Retry, true
	in.Now = f.Retry.NotBefore.Add(time.Minute)
	if out := Plan(in); out.Action != ActionNone || out.Reason != policy.ReasonNotYet {
		t.Fatalf("%s %q, want none/%s", out.Action, out.Reason, policy.ReasonNotYet)
	}
}

// A fixed-time schedule keeps its own rule: the slot of the deferral is
// attempted, so the run waits for the next fixed time.
func TestAuthDeferralWaitsForTheNextFixedTime(t *testing.T) {
	pol := authPolicy()
	pol.Schedule.Mode, pol.Schedule.FixedTimes, pol.Schedule.Catchup = policy.ModeFixed, []string{"11:00", "23:00"}, true
	in, _ := authInput(pol, restic.ClassAuth, true) // the deferral ran at 11:50 UTC, inside the 11:00 slot
	in.Now = in.Retry.NotBefore.Add(time.Minute)
	out := Plan(in)
	if out.Action != ActionNone || out.Reason != policy.ReasonSlotAttempted {
		t.Fatalf("%s %q, want none/%s", out.Action, out.Reason, policy.ReasonSlotAttempted)
	}
	if want := time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC); !out.NextCheck.Equal(want) {
		t.Errorf("NextCheck = %v, want %v", out.NextCheck, want)
	}
}

// Pause, a closed window and backup_now keep their meaning.
func TestAuthDeferralWaitKeepsTheOtherGates(t *testing.T) {
	pol := authPolicy()

	in, _ := authInput(pol, restic.ClassAuth, true)
	in.ForceNow = true
	if out := Plan(in); out.Action != ActionStart || out.Trigger != api.TriggerManual {
		t.Errorf("backup_now: %s trigger %q, want a manual start", out.Action, out.Trigger)
	}

	in, end := authInput(pol, restic.ClassAuth, true)
	in.Now = end.Add(25 * time.Hour)
	in.Paused = true
	if out := Plan(in); out.Action != ActionNone || out.Reason != ReasonPaused {
		t.Errorf("paused: %s %q", out.Action, out.Reason)
	}

	in, end = authInput(pol, restic.ClassAuth, true)
	in.Now = end.Add(25 * time.Hour)
	in.Policy.Schedule.Window = "Mon/8:00-9:00" // t0 is a Thursday
	if out := Plan(in); out.Action != ActionNone || out.Reason != policy.ReasonWindow {
		t.Errorf("closed window: %s %q, want none/%s", out.Action, out.Reason, policy.ReasonWindow)
	}
}

func TestFactsMarkAnAuthDeferralOnlyWhenItIsTheNewestRun(t *testing.T) {
	pol := policy.Default(policy.ClassDesktop)
	h := func(n int) time.Time { return t0.Add(time.Duration(-n) * time.Hour) }
	auth := deferredWith(restic.ClassAuth)

	rows := []journal.RunRow{row("D2", h(1), restic.OutcomeDeferred, auth), row("D1", h(2), restic.OutcomeDeferred, auth), row("S", h(30), restic.OutcomeSuccess, nil)}
	f := FactsFromRuns(rows, pol)
	if want := *rows[0].FinishedAt; !f.Retry.AuthDeferredAt.Equal(want) || f.Attempts != 2 {
		t.Errorf("two auth deferrals: %+v, want AuthDeferredAt %v and 2 attempts (they climb the ladder)", f, want)
	}

	rows = []journal.RunRow{row("F", h(1), restic.OutcomeFailed, nil), row("D", h(2), restic.OutcomeDeferred, auth), row("S", h(30), restic.OutcomeSuccess, nil)}
	if f := FactsFromRuns(rows, pol); !f.Retry.AuthDeferredAt.IsZero() {
		t.Errorf("a failure after the auth deferral: AuthDeferredAt = %v, want zero", f.Retry.AuthDeferredAt)
	}

	rows = []journal.RunRow{row("S2", h(1), restic.OutcomeSuccess, nil), row("D", h(2), restic.OutcomeDeferred, auth)}
	if f := FactsFromRuns(rows, pol); !f.Retry.AuthDeferredAt.IsZero() || f.Retry.Pending {
		t.Errorf("a success after the auth deferral: %+v", f.Retry)
	}

	if !AuthDeferral(row("X", t0, restic.OutcomeDeferred, auth)) ||
		AuthDeferral(row("Y", t0, restic.OutcomeDeferred, deferredWith(restic.ClassNetwork))) ||
		AuthDeferral(row("Z", t0, restic.OutcomeFailed, auth)) {
		t.Error("AuthDeferral")
	}
}

// The ladder gate still holds while refused: the interval is counted only once
// the gate has passed, so a policy whose interval is shorter than the ladder
// never tries more often than the ladder would.
func TestAuthDeferralWaitKeepsTheLadderGate(t *testing.T) {
	in, _ := authInput(authPolicy(), restic.ClassAuth, true)
	in.Now = in.Retry.NotBefore.Add(-time.Second)
	out := Plan(in)
	if out.Action != ActionNone || out.Reason != ReasonRetryWait || !out.NextCheck.Equal(in.Retry.NotBefore) {
		t.Fatalf("%s %q next %v, want the ladder gate %v", out.Action, out.Reason, out.NextCheck, in.Retry.NotBefore)
	}
}
