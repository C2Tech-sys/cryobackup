package notify

import (
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
