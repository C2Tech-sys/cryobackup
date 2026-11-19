package notify

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The fixes of P20-W2AF, one block per item of the ticket.

// Item 5. Every state of the engine is classified as mails, episode only or no episode. The list
// comes from state.Precedence(), the engine's own exported list of its states: a state added there
// fails this test until the notifier says what to do with it.
func TestNotifyEveryEngineStateIsClassified(t *testing.T) {
	var mails, episodes []string
	for _, s := range state.Precedence() {
		c, ok := classOf(s)
		if !ok {
			t.Errorf("state %s is not classified (mails, episode only or no episode)", s)
			continue
		}
		switch c {
		case classMails:
			mails = append(mails, string(s))
			episodes = append(episodes, string(s))
		case classEpisodeOnly:
			episodes = append(episodes, string(s))
		}
	}
	if len(stateClasses) != len(state.Precedence()) {
		t.Errorf("%d states classified, the engine has %d", len(stateClasses), len(state.Precedence()))
	}
	// The classification agrees with the renderer (what mails) and the store (what has an episode).
	var critical []string
	for _, a := range criticalAlarms {
		if a != store.AlarmMissing {
			critical = append(critical, a)
		}
	}
	if !sameSet(mails, critical) {
		t.Errorf("mails %v, the renderer's critical alarms %v", mails, critical)
	}
	if want := store.EpisodeAlarmTypes(store.EpisodeDevice); !sameSet(episodes, want) {
		t.Errorf("episode states %v, the store's device episode types %v", episodes, want)
	}
}

func sameSet(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// Item 5. A state the notifier does not know fails the pass, naming the state word only.
func TestNotifyUnclassifiedStateFailsThePass(t *testing.T) {
	for _, where := range []string{"primary", "flag", "alarm flag"} {
		w := newWorld(t)
		site := w.site("Summit Eye Care", 2)
		w.recipient("ops@example.test", true, true)
		w.set(state.StateFailed, site[1])
		v := w.view()
		d := &v.Devices[0]
		switch where {
		case "primary":
			d.Primary = "NEWLY_INVENTED"
		case "flag":
			d.Flags = append(d.Flags, "NEWLY_INVENTED")
		default:
			d.AlarmFlags = append(d.AlarmFlags, "NEWLY_INVENTED")
		}
		_, err := w.n.Pass(w.ctx, v)
		if err == nil || !strings.Contains(err.Error(), "NEWLY_INVENTED") {
			t.Fatalf("%s: an unclassified state gave %v", where, err)
		}
		for _, h := range w.host {
			if strings.Contains(err.Error(), h) {
				t.Fatalf("%s: the error names a host: %v", where, err)
			}
		}
		if w.fake.Calls() != 0 || len(w.open()) != 0 {
			t.Fatalf("%s: a pass over an unclassified state synced or mailed", where)
		}
	}
}

// Item 2. 150 due rows of a kind nobody renders here do not keep a failed alert from its retry.
func TestRetryRowsOfAnotherKindDoNotStarveTheWindow(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, false)
	for i := 0; i < 150; i++ {
		if _, _, err := w.st.EnqueueDelivery(w.ctx, store.ScopeAll(), store.DeliveryInput{Kind: store.DeliveryNag,
			DedupeKey: fmt.Sprintf("nag:%d", i), SubjectType: "console"}); err != nil {
			t.Fatal(err)
		}
	}
	w.clk.Advance(10 * time.Minute)
	w.fake.FailNext(1, &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true})
	w.set(state.StateFailed, site...)
	if res := w.pass(); res.Failed != 1 {
		t.Fatalf("pass %+v", res)
	}
	w.clk.Advance(time.Minute)
	rr := w.retry()
	if rr.Sent != 1 || w.fake.Calls() != 2 || len(w.fake.Sent()) != 1 {
		t.Fatalf("retry %+v, calls %d", rr, w.fake.Calls())
	}
	for _, d := range w.deliveries() {
		if d.Kind == store.DeliveryNag && d.Result != store.DeliveryQueued {
			t.Fatalf("a nag row was touched: %+v", d)
		}
	}
}

// Item 6. A recipient deleted between the pass's read of the recipients and its delivery rows is
// skipped and counted; the others are mailed. When it was the only one, nothing is marked: the
// episodes wait for the next pass instead of being marked as mailed to nobody.
func TestNotifyRecipientDeletedMidPass(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	gone := w.recipient("gone@example.test", true, true)
	w.recipient("ops@example.test", true, true)
	w.n.afterRecipients = func() {
		if err := w.st.DeleteRecipient(w.ctx, store.ScopeAll(), gone.ID, w.actor); err != nil {
			t.Fatal(err)
		}
	}
	w.set(state.StateFailed, site...)
	res, err := w.n.Pass(w.ctx, w.view())
	if err != nil {
		t.Fatalf("a deleted recipient failed the pass: %v", err)
	}
	by := sentTo(w.fake.Sent())
	if res.RecipientsGone != 1 || res.Alerted != 2 || len(by) != 1 || len(by["ops@example.test"]) != 1 {
		t.Fatalf("pass %+v, sent %v", res, by)
	}
	if !strings.Contains(w.logs.String(), `"recipients_gone":1`) {
		t.Fatalf("the skip is not logged:\n%s", w.logs.String())
	}
	w.noLeaks()
}

func TestNotifyOnlyRecipientDeletedMidPass(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	gone := w.recipient("gone@example.test", true, true)
	w.n.afterRecipients = func() {
		if err := w.st.DeleteRecipient(w.ctx, store.ScopeAll(), gone.ID, w.actor); err != nil {
			t.Fatal(err)
		}
	}
	w.set(state.StateFailed, site...)
	res, err := w.n.Pass(w.ctx, w.view())
	if err != nil || res.RecipientsGone != 1 || res.Messages != 0 || res.Alerted != 0 || len(w.unnotified()) != 1 {
		t.Fatalf("pass %+v %v, %d unnotified", res, err, len(w.unnotified()))
	}
	w.n.afterRecipients = nil
	w.recipient("ops@example.test", true, true)
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Alerted != 1 || w.fake.Calls() != 1 {
		t.Fatalf("next pass %+v", res)
	}
}

// Item 3a. Two passes that overlap (the second runs while the first is between its delivery rows
// and its marks) mail once per recipient: the first marks nothing, suppresses its own rows and
// sends nothing.
func TestNotifyOverlappingPassesMailOnce(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 3)
	w.recipient("ops@example.test", true, true)
	w.recipient("oncall@example.test", true, false)
	w.set(state.StateStale, site...)
	var second PassResult
	w.n.beforeMark = func(string) error {
		w.n.beforeMark = nil
		var err error
		second, err = w.n.Pass(w.ctx, w.view())
		return err
	}
	first := w.pass()
	by := sentTo(w.fake.Sent())
	if len(by["ops@example.test"]) != 1 || len(by["oncall@example.test"]) != 1 || w.fake.Calls() != 2 {
		t.Fatalf("overlapping passes sent %v", by)
	}
	if second.Alerted != 3 || second.Sent != 2 || first.Alerted != 0 || first.Sent != 0 || first.Messages != 0 {
		t.Fatalf("first %+v second %+v", first, second)
	}
	suppressed := 0
	for _, d := range w.deliveries() {
		switch d.Result {
		case store.DeliverySuppressed:
			suppressed++
		case store.DeliverySent:
		default:
			t.Fatalf("a row is left waiting: %+v", d)
		}
	}
	if suppressed != 2 {
		t.Fatalf("%d rows suppressed, want the first pass's 2", suppressed)
	}
	w.clk.Advance(store.QueuedStaleAfter + time.Minute)
	if rr := w.retry(); rr.Due != 0 || w.fake.Calls() != 2 {
		t.Fatalf("retry after the overlap %+v", rr)
	}
}

// collide writes a suppressed delivery row under the batch id at for the recipient, as a crashed
// pass of an earlier process would have left it.
func (w *world) collide(at time.Time, r store.Recipient) {
	w.t.Helper()
	if _, created, err := w.st.EnqueueDelivery(w.ctx, store.ScopeAll(), store.DeliveryInput{Kind: store.DeliveryAlert, RuleID: r.ID,
		DedupeKey: batchKey(keyAlert, at.UnixMilli(), r.ID), SubjectType: "console", SuppressReason: "deduped"}); err != nil || !created {
		w.t.Fatalf("collide: %v %v", created, err)
	}
}

// Item 3b. A batch id that an earlier row already holds (a suppressed orphan of another process)
// moves to the next free millisecond: the alert is not lost to the collision.
func TestNotifyBatchIDCollisionStillMails(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	r := w.recipient("ops@example.test", true, true)
	w.collide(w.clk.Now(), r)
	w.set(state.StateFailed, site...)
	res := w.pass()
	if res.Alerted != 1 || res.Sent != 1 || res.Messages != 1 || w.fake.Calls() != 1 {
		t.Fatalf("pass over a colliding batch id %+v, calls %d", res, w.fake.Calls())
	}
	eps, err := w.st.ListEpisodesNotifiedAt(w.ctx, store.ScopeAll(), w.clk.Now().Add(time.Millisecond))
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes of the next millisecond: %v %v", eps, err)
	}
}

// Item 3b. The search for a free batch id is bounded: five collisions fail the pass, nothing is
// marked and nothing sent; the next pass, past the collisions, mails.
func TestNotifyBatchIDCollisionIsBounded(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	free := w.recipient("ops@example.test", true, true)    // listed first (by name): its row is written, then abandoned
	r := w.recipient("zz-oncall@example.test", true, true) // collides at every try
	for i := 0; i < 5; i++ {
		w.collide(w.clk.Now().Add(time.Duration(i)*time.Millisecond), r)
	}
	w.set(state.StateFailed, site...)
	if _, err := w.n.Pass(w.ctx, w.view()); err == nil {
		t.Fatal("five collisions did not fail the pass")
	}
	if w.fake.Calls() != 0 || len(w.unnotified()) != 1 {
		t.Fatalf("after the failed pass: %d sent, %d unnotified", w.fake.Calls(), len(w.unnotified()))
	}
	abandoned := 0
	for _, d := range w.deliveries() {
		if d.RuleID == free.ID {
			if d.Result != store.DeliverySuppressed {
				t.Fatalf("a row of an abandoned batch id is waiting: %+v", d)
			}
			abandoned++
		}
	}
	if abandoned != 5 {
		t.Fatalf("%d abandoned rows suppressed, want one per try", abandoned)
	}
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Alerted != 1 || res.Sent != 2 {
		t.Fatalf("next pass %+v", res)
	}
}

// snoozedAlert mails a STALE alert for one device, snoozes it for an hour and lets the snooze lapse
// while the alarm persists. It returns the device id.
func (w *world) snoozedAlert(customer string) string {
	w.t.Helper()
	site := w.site(customer, 1)
	w.set(state.StateStale, site...)
	if res := w.pass(); res.Sent != 1 {
		w.t.Fatalf("the first alert: %+v", res)
	}
	until := w.clk.Now().Add(time.Hour)
	if _, err := w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: site[0], AlarmType: "stale", SnoozeUntil: &until}, w.actor); err != nil {
		w.t.Fatal(err)
	}
	w.clk.Advance(2 * time.Hour)
	return site[0]
}

// Item 4. A lapsed snooze of a suspended customer is not re-armed: nothing could be mailed. When
// the customer is resumed it is re-armed and mailed once.
func TestNotifyLapsedSnoozeOfASuspendedCustomerWaits(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	w.snoozedAlert("Summit Eye Care")
	w.muted[w.cust["Summit Eye Care"]] = true
	if res := w.pass(); res.Rearmed != 0 || res.Alerted != 0 || len(w.unnotified()) != 0 {
		t.Fatalf("suspended pass %+v, %d unnotified", res, len(w.unnotified()))
	}
	w.muted[w.cust["Summit Eye Care"]] = false
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Rearmed != 1 || res.Alerted != 1 || w.fake.Calls() != 2 {
		t.Fatalf("resumed pass %+v, calls %d", res, w.fake.Calls())
	}
	for i := 0; i < 3; i++ {
		w.clk.Advance(time.Minute)
		w.pass()
	}
	if w.fake.Calls() != 2 {
		t.Fatalf("re-raised more than once: %d messages", w.fake.Calls())
	}
}

// Item 4. Without a relay the lapsed snooze is not re-armed either.
func TestNotifyLapsedSnoozeWithoutARelayWaits(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	w.snoozedAlert("Summit Eye Care")
	w.n.Configured = false
	if res := w.pass(); res.Rearmed != 0 || len(w.unnotified()) != 0 {
		t.Fatalf("unconfigured pass %+v", res)
	}
	w.n.Configured = true
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Rearmed != 1 || res.Sent != 1 {
		t.Fatalf("configured pass %+v", res)
	}
}

// Item 4. The alarm of a suspended customer closes while its lapsed snooze waits: the recovery of
// the alert that WAS mailed still goes out.
func TestNotifyRecoveryOfTheOriginalAlertAfterALapsedSnooze(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	dev := w.snoozedAlert("Summit Eye Care")
	w.muted[w.cust["Summit Eye Care"]] = true
	w.pass()
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, dev)
	res := w.pass()
	sent := w.fake.Sent()
	if res.Recovered != 1 || len(sent) != 2 || sent[1].Kind != mail.KindRecovery {
		t.Fatalf("recovery pass %+v, sent %d", res, len(sent))
	}
}

// recoveryOwed is how many closed, mailed episodes still wait for their recovery decision.
func (w *world) recoveryOwed() int {
	w.t.Helper()
	eps, err := w.st.ListRecoveryDueEpisodes(w.ctx, store.ScopeAll())
	if err != nil {
		w.t.Fatal(err)
	}
	return len(eps)
}

// Item 1. The alert fails, the device recovers, the retry suppresses the alert: no recovery notice,
// so the recipient receives nothing at all.
func TestNotifyNoRecoveryAfterAFailedThenSuppressedAlert(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.fake.FailNext(1, &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true})
	w.set(state.StateFailed, site...)
	w.pass()
	w.clk.Advance(30 * time.Second)
	w.set(state.StateOK, site...)
	if res := w.pass(); res.Recovered != 0 || res.Messages != 0 || w.recoveryOwed() != 1 {
		t.Fatalf("recovery while the alert is in retry %+v, owed %d", res, w.recoveryOwed())
	}
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Suppressed != 1 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	res := w.pass()
	if len(w.fake.Sent()) != 0 || res.Recovered != 0 || res.RecoveryUnsent != 1 || w.recoveryOwed() != 0 {
		t.Fatalf("pass after the suppressed alert %+v, sent %d, owed %d", res, len(w.fake.Sent()), w.recoveryOwed())
	}
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Messages != 0 || res.RecoveryUnsent != 0 {
		t.Fatalf("a later pass %+v", res)
	}
}

// Item 1. A permanent failure: the alert row is final unsent, so the recovery is marked done with
// no message.
func TestNotifyNoRecoveryAfterAPermanentFailure(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.fake.FailNext(1, &mail.SendError{Stage: mail.StageRecipient, Class: mail.ClassRecipientRejected})
	w.set(state.StateFailed, site...)
	if res := w.pass(); res.Failed != 1 {
		t.Fatalf("pass %+v", res)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site...)
	res := w.pass()
	if len(w.fake.Sent()) != 0 || res.Recovered != 0 || res.RecoveryUnsent != 1 || w.recoveryOwed() != 0 {
		t.Fatalf("recovery pass %+v, sent %d", res, len(w.fake.Sent()))
	}
}

// Item 1. One of two recipients received the alert: the recovery goes out.
func TestNotifyRecoveryWhenOneRecipientReceivedTheAlert(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.recipient("refused@example.test", true, false)
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == "refused@example.test" {
			return &mail.SendError{Stage: mail.StageRecipient, Class: mail.ClassRecipientRejected}
		}
		return nil
	})
	w.set(state.StateFailed, site...)
	if res := w.pass(); res.Sent != 1 || res.Failed != 1 {
		t.Fatalf("pass %+v", res)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site...)
	res := w.pass()
	sent := w.fake.Sent()
	if res.Recovered != 1 || len(sent) != 2 || sent[1].Kind != mail.KindRecovery || sent[1].To[0] != "ops@example.test" {
		t.Fatalf("recovery pass %+v, sent %v", res, sent)
	}
}

// Item 1. One recipient received the alert while another's is still in retry: the recovery does
// not wait for the retry.
func TestNotifyRecoveryDoesNotWaitWhenOneAlertWasDelivered(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.recipient("slow@example.test", true, false)
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == "slow@example.test" {
			return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
		}
		return nil
	})
	w.set(state.StateFailed, site...)
	w.pass()
	w.clk.Advance(30 * time.Second)
	w.set(state.StateOK, site...)
	if res := w.pass(); res.Recovered != 1 || res.Sent != 1 {
		t.Fatalf("recovery pass %+v", res)
	}
}

// suppressed returns the suppress reason of every suppressed row, by delivery id.
func (w *world) suppressed() map[string]string {
	w.t.Helper()
	out := map[string]string{}
	for _, d := range w.deliveries() {
		if d.Result == store.DeliverySuppressed {
			out[d.ID] = d.SuppressReason
		}
	}
	return out
}

// Item 7. Each case a retry suppresses records the schema word that fits it best, and the log line
// names the precise case as a class word.
func TestRetrySuppressReasons(t *testing.T) {
	cases := []struct {
		name, reason, why string
		arrange           func(w *world, r store.Recipient, dev string)
	}{
		{"alarm resolved", "below_threshold", "alarm_resolved", func(w *world, _ store.Recipient, dev string) {
			w.set(state.StateOK, dev)
			w.pass()
		}},
		{"recipient switched off", "paused", "recipient_switched_off", func(w *world, r store.Recipient, _ string) {
			if _, err := w.st.UpdateRecipient(w.ctx, store.ScopeAll(), r.ID, store.RecipientInput{Name: r.Name, Address: r.Address, Recoveries: true}, w.actor); err != nil {
				w.t.Fatal(err)
			}
		}},
		{"recipient deleted", "deduped", "recipient_deleted", func(w *world, r store.Recipient, _ string) {
			if err := w.st.DeleteRecipient(w.ctx, store.ScopeAll(), r.ID, w.actor); err != nil {
				w.t.Fatal(err)
			}
		}},
		{"acknowledged", "acknowledged", "acknowledged", func(w *world, _ store.Recipient, dev string) {
			if _, err := w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: dev, AlarmType: "failed"}, w.actor); err != nil {
				w.t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			site := w.site("Summit Eye Care", 1)
			r := w.recipient("ops@example.test", true, true)
			w.fake.FailNext(1, &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true})
			w.set(state.StateFailed, site...)
			w.pass()
			w.clk.Advance(30 * time.Second)
			c.arrange(w, r, site[0])
			w.clk.Advance(time.Minute)
			if rr := w.retry(); rr.Suppressed != 1 {
				t.Fatalf("retry %+v", rr)
			}
			got := w.suppressed()
			if len(got) != 1 {
				t.Fatalf("suppressed rows %v", got)
			}
			for _, reason := range got {
				if reason != c.reason {
					t.Fatalf("reason %q, want %q", reason, c.reason)
				}
			}
			if !strings.Contains(w.logs.String(), `"why":"`+c.why+`"`) {
				t.Fatalf("no log line names %s:\n%s", c.why, w.logs.String())
			}
			w.noLeaks()
		})
	}
}

// Item 7. The orphan of a crashed pass and a row whose key is not a batch key are deduped, each
// with its own class word in the log.
func TestRetrySuppressReasonsOfOrphansAndBadKeys(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	r := w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, site...)
	w.n.beforeMark = func(string) error { return errors.New("crash") }
	if _, err := w.n.Pass(w.ctx, w.view()); err == nil {
		t.Fatal("no crash")
	}
	w.n.beforeMark = nil
	if _, _, err := w.st.EnqueueDelivery(w.ctx, store.ScopeAll(), store.DeliveryInput{Kind: store.DeliveryAlert, RuleID: r.ID,
		DedupeKey: "alert:not-a-batch:" + r.ID, SubjectType: "console"}); err != nil {
		t.Fatal(err)
	}
	w.clk.Advance(store.QueuedStaleAfter)
	if rr := w.retry(); rr.Suppressed != 2 {
		t.Fatalf("retry %+v", rr)
	}
	for id, reason := range w.suppressed() {
		if reason != "deduped" {
			t.Fatalf("row %s reason %q", id, reason)
		}
	}
	for _, why := range []string{"orphan", "bad_key"} {
		if !strings.Contains(w.logs.String(), `"why":"`+why+`"`) {
			t.Fatalf("no log line names %s:\n%s", why, w.logs.String())
		}
	}
}
