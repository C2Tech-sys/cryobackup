package state

import (
	"testing"
	"time"
)

// P20-T2F item 2: "nothing delivered in 24 h and something pending" needs an age. A console that has
// just queued its first alert has nothing delivered and one thing pending for a minute, and must
// not show a critical banner for it. The oldest pending alert has to be SMTPPendingMinAge old.
func TestC09SMTPPendingNeedsAge(t *testing.T) {
	future := ahead(10 * time.Minute) // beyond the tolerated clock skew
	pending := func(n int, oldest, lastSend Millis) func(*ProbeFacts, *World) {
		return func(p *ProbeFacts, _ *World) {
			p.SMTP.PendingAlerts, p.SMTP.OldestPendingAt, p.SMTP.LastSendOKAt = n, oldest, lastSend
		}
	}
	runProbeCases(t, []probeCase{
		{name: "C09_smtp_pending_14_minutes_never_delivered_is_not_yet", mod: pending(1, ago(14*time.Minute), 0)},
		{name: "C09_smtp_pending_15_minutes_never_delivered_alarms", mod: pending(1, ago(15*time.Minute), 0),
			want: []string{"C03/critical"}, contains: "No alert e-mail has been delivered yet."},
		{name: "C09_smtp_pending_14_minutes_after_a_day_without_a_send_is_not_yet", mod: pending(2, ago(14*time.Minute), ago(25*hour))},
		{name: "C09_smtp_pending_15_minutes_after_a_day_without_a_send_alarms", mod: pending(2, ago(15*time.Minute), ago(25*hour)),
			want: []string{"C03/critical"}, contains: "Last send Wed 21 Oct 11:00 AM."},
		{name: "C09_smtp_pending_for_hours_never_delivered_alarms", mod: pending(3, ago(3*hour), 0), want: []string{"C03/critical"}},
		{name: "C09_smtp_pending_old_but_a_recent_send_is_evidence", mod: pending(3, ago(3*hour), ago(time.Minute))},
		// Fail loudly: an age that cannot be read is not a reason to stay quiet.
		{name: "C09_smtp_pending_with_no_age_alarms", mod: pending(1, 0, 0), want: []string{"C03/critical"}},
		{name: "C09_smtp_pending_with_an_age_in_the_future_alarms", mod: pending(1, future, 0), want: []string{"C03/critical"}},
		// Nothing pending: the age is not read.
		{name: "C09_smtp_nothing_pending_ignores_the_age", mod: pending(0, ago(time.Minute), 0)},
	})
}
