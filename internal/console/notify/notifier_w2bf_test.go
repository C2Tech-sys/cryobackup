package notify

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// siblingInFlight mails one FAILED batch of two devices to A and B; both sends fail temporarily.
// dev0 is then acknowledged and the first retry runs while B's send fails again: A's retry leaves
// dev0 out while B's row is still waiting for its own retry (not final). It returns the devices.
func (w *world) siblingInFlight() []string {
	w.t.Helper()
	site := w.twoDeviceFailedBatch("a@example.test", "b@example.test")
	w.ackDevice(site[0])
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == "b@example.test" {
			return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
		}
		return nil
	})
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.Failed != 1 || rr.Released != 0 {
		w.t.Fatalf("first retry %+v: dev0 must stay in the batch while B's row is not final", rr)
	}
	if len(w.unnotified()) != 0 {
		w.t.Fatal("dev0 left the batch while a sibling delivery was still in flight")
	}
	return site
}

// P20-W2BF B3. B's row then goes out: the batch was delivered, so dev0's recovery is owed to both
// recipients when it closes, as the batch rule says.
func TestRetryMutedEpisodeStaysWhileASiblingIsInFlightThenSent(t *testing.T) {
	w := newWorld(t)
	w.recipient("a@example.test", true, true)
	w.recipient("b@example.test", true, true)
	site := w.siblingInFlight()
	w.fake.FailWhen(nil)
	w.clk.Advance(5 * time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.Released != 0 {
		t.Fatalf("B's retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0])
	if res := w.pass(); res.Recovered != 1 || res.Sent != 2 {
		t.Fatalf("recovery pass %+v", res)
	}
	got := sentTo(w.fake.Sent())
	for _, to := range []string{"a@example.test", "b@example.test"} {
		last := got[to][len(got[to])-1]
		if last.Kind != mail.KindRecovery {
			t.Fatalf("%s did not get dev0's recovery", to)
		}
	}
}

// P20-W2BF B3. B's row fails for good instead. dev0 stays in the batch: A's row of the batch was
// delivered (without dev0, which was muted then), and the batch rule is per batch, not per
// message. So when dev0's acknowledgement is cleared while it still fails it is not mailed again,
// and when it closes a recovery notice is sent for it. This is the documented limit of the
// per-batch rule (Retrier.leaveBatch).
func TestRetryMutedEpisodeStaysWhenTheSiblingThenFailsForGood(t *testing.T) {
	w := newWorld(t)
	w.recipient("a@example.test", true, true)
	w.recipient("b@example.test", true, true)
	site := w.siblingInFlight()
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == "b@example.test" {
			return &mail.SendError{Stage: mail.StageRecipient, Class: mail.ClassRecipientRejected, Temporary: false}
		}
		return nil
	})
	w.clk.Advance(5 * time.Minute)
	if rr := w.retry(); rr.Failed != 1 || rr.Released != 0 {
		t.Fatalf("B's retry %+v", rr)
	}
	for _, a := range w.activeAcks() {
		if _, err := w.st.ClearAck(w.ctx, store.ScopeAll(), a.ID, w.actor); err != nil {
			t.Fatal(err)
		}
	}
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Alerted != 0 {
		t.Fatalf("dev0 mailed again after its acknowledgement ended: %+v", res)
	}
	w.fake.FailWhen(nil)
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0])
	if res := w.pass(); res.Recovered != 1 {
		t.Fatalf("recovery pass %+v", res)
	}
	if w.mentions(mail.KindAlert, w.host[site[0]]) != 0 || w.mentions(mail.KindRecovery, w.host[site[0]]) == 0 {
		t.Fatalf("dev0 alert %d recovery %d", w.mentions(mail.KindAlert, w.host[site[0]]), w.mentions(mail.KindRecovery, w.host[site[0]]))
	}
}

func (w *world) activeAcks() []store.Ack {
	w.t.Helper()
	acks, err := w.st.ListActiveAcks(w.ctx, store.ScopeAll())
	if err != nil {
		w.t.Fatal(err)
	}
	return acks
}
