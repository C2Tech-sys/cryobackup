package scheduler

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

func fleetCap(r *journal.RunRow) { r.DeferredReason = restic.DeferredFleetCap }

// ADR 0005: a run the console's start lease refused is not an attempt. It
// neither climbs the backoff ladder nor marks the slot attempted nor becomes Last.
func TestFactsIgnoreFleetCapDeferrals(t *testing.T) {
	pol := policy.Default(policy.ClassDesktop)
	h := func(n int) time.Time { return t0.Add(time.Duration(-n) * time.Hour) }

	t.Run("after a success", func(t *testing.T) {
		rows := []journal.RunRow{
			row("F2", h(1), restic.OutcomeDeferred, fleetCap),
			row("F1", h(2), restic.OutcomeDeferred, fleetCap),
			row("A", h(30), restic.OutcomeSuccess, nil),
		}
		f := FactsFromRuns(rows, pol)
		if f.Attempts != 0 || f.Retry.Pending || f.Last == nil || f.Last.ID != "A" || !f.LastAttemptStart.Equal(h(30)) {
			t.Fatalf("%+v: fleet-cap deferrals were counted", f)
		}
	})
	t.Run("between failures the ladder does not move", func(t *testing.T) {
		rows := []journal.RunRow{
			row("F", h(1), restic.OutcomeDeferred, fleetCap),
			row("C", h(2), restic.OutcomeFailed, nil),
			row("A", h(30), restic.OutcomeSuccess, nil),
		}
		f := FactsFromRuns(rows, pol)
		want := FactsFromRuns(rows[1:], pol)
		if f.Attempts != 1 || f.Last == nil || f.Last.ID != "C" || !f.Retry.NotBefore.Equal(want.Retry.NotBefore) || !f.Retry.Pending {
			t.Fatalf("%+v, want the facts without the deferral: %+v", f, want)
		}
	})
	t.Run("other deferrals still count", func(t *testing.T) {
		rows := []journal.RunRow{row("G", h(1), restic.OutcomeDeferred, func(r *journal.RunRow) { r.DeferredReason = restic.DeferredGateway })}
		if f := FactsFromRuns(rows, pol); f.Attempts != 1 || !f.Retry.Pending {
			t.Fatalf("a gateway deferral must stay an attempt: %+v", f)
		}
	})
	t.Run("a fixed-time slot is not lost to a deferral", func(t *testing.T) {
		p := pol
		p.Schedule.FixedTimes, p.Schedule.Catchup = []string{"11:00"}, false
		p.Schedule.JitterMinutes = 0
		rows := []journal.RunRow{
			row("F", t0.Add(-30*time.Minute), restic.OutcomeDeferred, fleetCap), // 11:30, inside today's slot
			row("A", t0.Add(-25*time.Hour), restic.OutcomeSuccess, nil),
		}
		f := FactsFromRuns(rows, p)
		in := input()
		in.Policy = p
		in.LastSuccess, in.LastAttemptStart, in.Retry = f.LastSuccess, f.LastAttemptStart, f.Retry
		if out := Plan(in); out.Action != ActionStart {
			t.Fatalf("Plan after a fleet-cap deferral = %s %q: the slot was marked attempted", out.Action, out.Reason)
		}
	})
	if !FleetCapDeferral(row("X", t0, restic.OutcomeDeferred, fleetCap)) || FleetCapDeferral(row("Y", t0, restic.OutcomeDeferred, nil)) {
		t.Fatal("FleetCapDeferral")
	}
}

func TestPlanHonoursTheLeaseWait(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Input)
		action  Action
		trigger api.Trigger
		reason  string
	}{
		{"a due run waits for the lease time", func(in *Input) { in.LeaseNotBefore = t0.Add(7 * time.Minute) }, ActionNone, "", ReasonLeaseWait},
		{"a due retry waits too", func(in *Input) {
			in.LeaseNotBefore = t0.Add(time.Minute)
			in.Retry = RetryState{Pending: true, NotBefore: t0.Add(-time.Minute)}
		}, ActionNone, "", ReasonLeaseWait},
		{"backup_now is not held back", func(in *Input) {
			in.LeaseNotBefore, in.ForceNow = t0.Add(7*time.Minute), true
		}, ActionStart, api.TriggerManual, ReasonForced},
		{"a lease time in the past is ignored", func(in *Input) { in.LeaseNotBefore = t0.Add(-time.Second) }, ActionStart, api.TriggerSchedule, ReasonDue},
		{"exactly at the lease time the run starts", func(in *Input) { in.LeaseNotBefore = t0 }, ActionStart, api.TriggerSchedule, ReasonDue},
		{"a pause still wins", func(in *Input) { in.LeaseNotBefore, in.Paused = t0.Add(time.Minute), true }, ActionNone, "", ReasonPaused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			tc.mutate(&in)
			out := Plan(in)
			if out.Action != tc.action || out.Trigger != tc.trigger || out.Reason != tc.reason {
				t.Fatalf("Plan = %s %q %q, want %s %q %q", out.Action, out.Trigger, out.Reason, tc.action, tc.trigger, tc.reason)
			}
			if tc.reason == ReasonLeaseWait && !out.NextCheck.Equal(in.LeaseNotBefore) {
				t.Errorf("NextCheck = %v, want the lease time %v", out.NextCheck, in.LeaseNotBefore)
			}
		})
	}
}
