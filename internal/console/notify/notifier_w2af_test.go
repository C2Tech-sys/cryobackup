package notify

import (
	"context"
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
	for _, d := range w.deliveries() {
		if d.RuleID == free.ID {
			t.Fatalf("a row was written under a taken batch id: %+v", d) // batchIDs.next sees the ids are taken first
		}
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
	if rr := w.retry(); rr.Suppressed != 1 || rr.RecoveryUnsent != 1 { // P20-W2BF C1: the retry settles the closed episode it leaves out
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	res := w.pass()
	if len(w.fake.Sent()) != 0 || res.Recovered != 0 || res.RecoveryUnsent != 0 || w.recoveryOwed() != 0 {
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
		{"recipient switched off", "deduped", "recipient_switched_off", func(w *world, r store.Recipient, _ string) {
			if _, err := w.st.UpdateRecipient(w.ctx, store.ScopeAll(), r.ID, store.RecipientInput{Name: r.Name, Address: r.Address, Recoveries: true}, w.actor); err != nil {
				w.t.Fatal(err)
			}
		}},
		{"recipient deleted", "deduped", "recipient_deleted", func(w *world, r store.Recipient, _ string) {
			if err := w.st.DeleteRecipient(w.ctx, store.ScopeAll(), r.ID, w.actor); err != nil {
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

// Batch 2 item 4. A RerenderFunc that answers with a suppress word the schema does not allow is
// recorded as deduped, with the word (sanitised) in the log; the run does not fail.
func TestRetryUnknownSuppressWordFromARerenderFunc(t *testing.T) {
	w := newWorld(t)
	rec := w.recipient("ops@example.test", false, false)
	for i, word := range []string{"resolved", "Bad Word/../x" + strings.Repeat("y", 60)} {
		key := fmt.Sprintf("digest:2026-11-1%d:%s", i, rec.ID)
		if _, _, err := w.st.EnqueueDelivery(w.ctx, store.ScopeAll(), store.DeliveryInput{Kind: store.DeliveryDigest, RuleID: rec.ID, DedupeKey: key, SubjectType: "console"}); err != nil {
			t.Fatal(err)
		}
		w.r.Rerender = map[string]RerenderFunc{store.DeliveryDigest: func(context.Context, store.Delivery, store.Recipient) (mail.Message, string, error) {
			return mail.Message{}, word, nil
		}}
		w.clk.Advance(store.QueuedStaleAfter)
		rr, err := w.r.Run(w.ctx)
		if err != nil || rr.Suppressed != 1 {
			t.Fatalf("%q: retry %+v %v", word, rr, err)
		}
	}
	for _, reason := range w.suppressed() {
		if reason != "deduped" {
			t.Fatalf("reason %q", reason)
		}
	}
	logs := w.logs.String()
	if !strings.Contains(logs, `"why":"resolved"`) || !strings.Contains(logs, `"why":"badwordxyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"`) {
		t.Fatalf("log:\n%s", logs)
	}
	if strings.Contains(logs, "../") || strings.Contains(logs, "Bad Word") {
		t.Fatalf("the raw word reached the log:\n%s", logs)
	}
}

// Batch 2 item 3. A SENT row at the pass's millisecond for a recipient that does not receive alerts
// takes the id: the pass uses another, so that row cannot make a recovery look owed.
func TestNotifyBatchIDTakenByAnotherRecipientsSentRow(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	off := w.recipient("digest-only@example.test", false, false)
	row, _, err := w.st.EnqueueDelivery(w.ctx, store.ScopeAll(), store.DeliveryInput{Kind: store.DeliveryAlert, RuleID: off.ID,
		DedupeKey: batchKey(keyAlert, w.clk.Now().UnixMilli(), off.ID), SubjectType: "console"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.MarkDeliverySent(w.ctx, store.ScopeAll(), row.ID); err != nil {
		t.Fatal(err)
	}
	w.fake.FailNext(1, &mail.SendError{Stage: mail.StageRecipient, Class: mail.ClassRecipientRejected})
	w.set(state.StateFailed, site...)
	if res := w.pass(); res.Failed != 1 {
		t.Fatalf("pass %+v", res)
	}
	if eps, _ := w.st.ListEpisodesNotifiedAt(w.ctx, store.ScopeAll(), w.clk.Now()); len(eps) != 0 {
		t.Fatalf("the batch took a millisecond another row holds: %+v", eps)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site...)
	if res := w.pass(); res.Recovered != 0 || len(w.fake.Sent()) != 0 {
		t.Fatalf("recovery pass %+v, sent %d", res, len(w.fake.Sent()))
	}
}

// Batch 2 item 3. A silent recovery mark holds its millisecond: a real recovery batch of a second
// Notifier (no memory of the first's ids) in the same millisecond takes another, so its retry
// never mails the silently settled episode.
func TestNotifySilentRecoveryMarkHoldsItsBatchID(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, site[1])
	w.pass() // dev1's alert is delivered
	w.clk.Advance(time.Minute)
	w.fake.FailNext(1, &mail.SendError{Stage: mail.StageRecipient, Class: mail.ClassRecipientRejected})
	w.set(state.StateFailed, site[0])
	w.pass() // dev0's alert fails for good
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0])
	if res := w.pass(); res.RecoveryUnsent != 1 {
		t.Fatalf("dev0 settled silently? %+v", res)
	}
	// A second Notifier in the same millisecond mails dev1's recovery; the send fails and is retried.
	w.n = &Notifier{Store: w.st, Dispatch: w.n.Dispatch, Configured: true, Log: w.n.Log}
	w.set(state.StateOK, site[1])
	w.fake.FailNext(1, &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true})
	if res := w.pass(); res.Recovered != 1 || res.Failed != 1 {
		t.Fatalf("dev1 recovery pass %+v", res)
	}
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 {
		t.Fatalf("retry %+v", rr)
	}
	last := w.fake.Sent()[len(w.fake.Sent())-1]
	if last.Kind != mail.KindRecovery || strings.Contains(last.Text, w.host[site[0]]) || !strings.Contains(last.Text, w.host[site[1]]) {
		t.Fatalf("the recovery retry lists the silently settled device:\n%s", last.Text)
	}
}

// mentions counts the delivered messages of a kind that name the host.
func (w *world) mentions(kind mail.Kind, host string) int {
	n := 0
	for _, m := range w.fake.Sent() {
		if m.Kind == kind && strings.Contains(m.Text, host) {
			n++
		}
	}
	return n
}

// twoDeviceFailedBatch mails one alert batch of two FAILED devices whose send fails temporarily
// for the recipients failFor names. It returns the two device ids.
func (w *world) twoDeviceFailedBatch(failFor ...string) []string {
	w.t.Helper()
	site := w.site("Summit Eye Care", 2)
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		for _, a := range failFor {
			if m.To[0] == a && m.Kind == mail.KindAlert {
				return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
			}
		}
		return nil
	})
	w.set(state.StateFailed, site...)
	w.pass()
	w.fake.FailWhen(nil)
	return site
}

// Batch 2 item 1, the verifier's repro. dev0 closes while the batch's only alert is in retry; the
// retry mails dev1 only and settles dev0's recovery without a message, before it sends.
func TestRetrySettlesTheRecoveryOfEpisodesItLeavesOut(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	site := w.twoDeviceFailedBatch("ops@example.test")
	w.clk.Advance(30 * time.Second)
	w.set(state.StateOK, site[0])
	w.n.Configured = false // the pass that closes dev0 decides nothing about mail
	w.pass()
	w.n.Configured = true
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.RecoveryUnsent != 1 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Recovered != 0 || res.Messages != 0 {
		t.Fatalf("pass after the retry %+v", res)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[1])
	if res := w.pass(); res.Recovered != 1 || res.Sent != 1 {
		t.Fatalf("dev1 recovery pass %+v", res)
	}
	h0, h1 := w.host[site[0]], w.host[site[1]]
	if w.mentions(mail.KindAlert, h0) != 0 || w.mentions(mail.KindRecovery, h0) != 0 ||
		w.mentions(mail.KindAlert, h1) != 1 || w.mentions(mail.KindRecovery, h1) != 1 {
		t.Fatalf("dev0 alert %d recovery %d; dev1 alert %d recovery %d", w.mentions(mail.KindAlert, h0), w.mentions(mail.KindRecovery, h0),
			w.mentions(mail.KindAlert, h1), w.mentions(mail.KindRecovery, h1))
	}
}

// Batch 2 item 1. A received the full alert; B is retried after dev0 closed: nothing is settled,
// dev0's recovery goes out to A.
func TestRetrySettlesNothingWhenAnotherRecipientSawTheAlert(t *testing.T) {
	w := newWorld(t)
	w.recipient("a@example.test", true, true)
	w.recipient("b@example.test", true, false)
	site := w.twoDeviceFailedBatch("b@example.test")
	w.clk.Advance(30 * time.Second)
	w.set(state.StateOK, site[0])
	w.n.Configured = false
	w.pass()
	w.n.Configured = true
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.RecoveryUnsent != 0 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Recovered != 1 || w.mentions(mail.KindRecovery, w.host[site[0]]) != 1 {
		t.Fatalf("recovery pass %+v", res)
	}
}

// Batch 2 item 1. A recovery pass between the close and the retry waits; then nothing for dev0.
func TestRetrySettlesAfterARecoveryPassWaited(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	site := w.twoDeviceFailedBatch("ops@example.test")
	w.clk.Advance(20 * time.Second)
	w.set(state.StateOK, site[0])
	for i := 0; i < 2; i++ {
		if res := w.pass(); res.Recovered != 0 || res.RecoveryUnsent != 0 || w.recoveryOwed() != 1 {
			t.Fatalf("pass before the retry %+v", res)
		}
		w.clk.Advance(20 * time.Second)
	}
	w.clk.Advance(time.Minute)
	w.retry()
	w.clk.Advance(time.Minute)
	w.pass()
	if w.mentions(mail.KindRecovery, w.host[site[0]]) != 0 || w.mentions(mail.KindAlert, w.host[site[1]]) != 1 || w.recoveryOwed() != 0 {
		t.Fatalf("after the retry: sent %d", len(w.fake.Sent()))
	}
}

// Batch 2 item 1. A crash between the settle and the send: dev0 stays settled (no recovery), and
// dev1's alert is still retried.
func TestRetryCrashBetweenSettleAndSend(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	site := w.twoDeviceFailedBatch("ops@example.test")
	w.clk.Advance(30 * time.Second)
	w.set(state.StateOK, site[0])
	w.pass()
	w.clk.Advance(time.Minute)
	crash := errors.New("crash")
	w.r.afterSettle = func() error { return crash }
	if _, err := w.r.Run(w.ctx); !errors.Is(err, crash) {
		t.Fatalf("injected crash: %v", err)
	}
	w.r.afterSettle = nil
	if w.fake.Calls() != 1 || w.recoveryOwed() != 0 {
		t.Fatalf("after the crash: %d calls, %d recoveries owed", w.fake.Calls(), w.recoveryOwed())
	}
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.RecoveryUnsent != 0 {
		t.Fatalf("retry after the crash %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	if w.mentions(mail.KindRecovery, w.host[site[0]]) != 0 || w.mentions(mail.KindAlert, w.host[site[0]]) != 0 || w.mentions(mail.KindAlert, w.host[site[1]]) != 1 {
		t.Fatalf("sent %d", len(w.fake.Sent()))
	}
}

// Batch 2 item 2. A pass that marks only part of its batch (another pass marked the rest first)
// sends only the episodes it marked, read back by its batch id.
func TestNotifyPartialOverlapSendsOnlyWhatItMarked(t *testing.T) {
	w := newWorld(t)
	alpha := w.site("Alpha Clinic", 1)
	summit := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, alpha[0], summit[0])
	w.n.beforeMark = func(string) error {
		w.n.beforeMark = nil
		w.muted[w.cust["Summit Eye Care"]] = true // the other pass sees Summit suspended: it mails Alpha only
		defer func() { w.muted[w.cust["Summit Eye Care"]] = false }()
		_, err := w.n.Pass(w.ctx, w.view())
		return err
	}
	w.pass()
	ha, hs := w.host[alpha[0]], w.host[summit[0]]
	if w.mentions(mail.KindAlert, ha) != 1 || w.mentions(mail.KindAlert, hs) != 1 || w.fake.Calls() != 2 {
		t.Fatalf("alpha mailed %d, summit mailed %d, %d messages", w.mentions(mail.KindAlert, ha), w.mentions(mail.KindAlert, hs), w.fake.Calls())
	}
	if len(w.unnotified()) != 0 {
		t.Fatal("episodes left unnotified")
	}
}

// Batch 2 item 2, the verifier's case. An acknowledgement lands mid-pass and another pass mails
// e0: e0 is mailed once in total, and no message lists the acknowledged e1. When the
// acknowledgement is cleared, e1 goes out.
func TestNotifyAckMidPassIsNotMailed(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, site...)
	var ack store.Ack
	w.n.beforeMark = func(string) error {
		w.n.beforeMark = nil
		var err error
		if ack, err = w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: site[1], AlarmType: "failed"}, w.actor); err != nil {
			return err
		}
		_, err = w.n.Pass(w.ctx, w.view())
		return err
	}
	w.pass()
	h0, h1 := w.host[site[0]], w.host[site[1]]
	if w.mentions(mail.KindAlert, h0) != 1 || w.mentions(mail.KindAlert, h1) != 0 {
		t.Fatalf("e0 mailed %d, acknowledged e1 mailed %d", w.mentions(mail.KindAlert, h0), w.mentions(mail.KindAlert, h1))
	}
	if _, err := w.st.ClearAck(w.ctx, store.ScopeAll(), ack.ID, w.actor); err != nil {
		t.Fatal(err)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	if w.mentions(mail.KindAlert, h0) != 1 || w.mentions(mail.KindAlert, h1) != 1 {
		t.Fatalf("after the ack cleared: e0 %d, e1 %d", w.mentions(mail.KindAlert, h0), w.mentions(mail.KindAlert, h1))
	}
}
