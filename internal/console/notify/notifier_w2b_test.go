package notify

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ackDevice acknowledges the FAILED alarm of a device.
func (w *world) ackDevice(id string) store.Ack {
	w.t.Helper()
	a, err := w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: id, AlarmType: "failed"}, w.actor)
	if err != nil {
		w.t.Fatal(err)
	}
	return a
}

// P20-W2B item 7, the verifier's repro. The first send fails, dev0 is acknowledged, the retry mails
// dev1 only, dev0 closes: dev0 was in no delivered alert, so it gets no recovery either.
func TestRetryAcknowledgedEpisodeLeavesTheBatch(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	site := w.twoDeviceFailedBatch("ops@example.test")
	w.ackDevice(site[0])
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.Released != 1 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0])
	w.pass()
	w.clk.Advance(time.Minute)
	w.pass()
	h0, h1 := w.host[site[0]], w.host[site[1]]
	if w.mentions(mail.KindAlert, h0) != 0 || w.mentions(mail.KindRecovery, h0) != 0 || w.mentions(mail.KindAlert, h1) != 1 {
		t.Fatalf("dev0 alert %d recovery %d; dev1 alert %d", w.mentions(mail.KindAlert, h0), w.mentions(mail.KindRecovery, h0), w.mentions(mail.KindAlert, h1))
	}
	w.noLeaks()
}

// Item 7. The acknowledgement is cleared while dev0 is still failing: dev0 left the batch, so it is
// mailed normally, and its recovery follows that alert.
func TestRetryReleasedEpisodeIsMailedWhenTheMuteEnds(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	site := w.twoDeviceFailedBatch("ops@example.test")
	ack := w.ackDevice(site[0])
	w.clk.Advance(time.Minute)
	w.retry()
	if _, err := w.st.ClearAck(w.ctx, store.ScopeAll(), ack.ID, w.actor); err != nil {
		t.Fatal(err)
	}
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Alerted != 1 || res.Sent != 1 {
		t.Fatalf("pass after the ack cleared %+v", res)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0])
	w.pass()
	h0 := w.host[site[0]]
	if w.mentions(mail.KindAlert, h0) != 1 || w.mentions(mail.KindRecovery, h0) != 1 {
		t.Fatalf("dev0 alert %d recovery %d", w.mentions(mail.KindAlert, h0), w.mentions(mail.KindRecovery, h0))
	}
}

// Item 7. Every episode of the retried alert is muted (one acknowledged, the customer then
// suspended): the row is suppressed and both leave the batch; when the mutes end they are mailed.
func TestRetryAllMutedEpisodesLeaveTheBatch(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	site := w.twoDeviceFailedBatch("ops@example.test")
	ack := w.ackDevice(site[0])
	w.muted[w.cust["Summit Eye Care"]] = true
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 0 || rr.Suppressed != 1 || rr.Released != 2 {
		t.Fatalf("retry %+v", rr)
	}
	if len(w.unnotified()) != 2 {
		t.Fatalf("unnotified %d, want 2", len(w.unnotified()))
	}
	if _, err := w.st.ClearAck(w.ctx, store.ScopeAll(), ack.ID, w.actor); err != nil {
		t.Fatal(err)
	}
	w.muted[w.cust["Summit Eye Care"]] = false
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Alerted != 2 || res.Sent != 1 {
		t.Fatalf("pass after the mutes ended %+v", res)
	}
}

// Item 7. Another recipient received the full alert: nothing leaves the batch, and the
// acknowledged device's recovery goes out to the one who saw it.
func TestRetryReleasesNothingWhenAnotherRecipientSawTheAlert(t *testing.T) {
	w := newWorld(t)
	w.recipient("a@example.test", true, true)
	w.recipient("b@example.test", true, false)
	site := w.twoDeviceFailedBatch("b@example.test")
	w.ackDevice(site[0])
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.Released != 0 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0])
	if res := w.pass(); res.Recovered != 1 || w.mentions(mail.KindRecovery, w.host[site[0]]) != 1 {
		t.Fatalf("recovery pass %+v", res)
	}
}

// P20-W2B item 8. The mark step of a recovery batch changes fewer episodes than the batch holds
// (another writer settled one first): the notice lists only the episodes this pass marked.
func TestNotifyRecoveryBatchSendsOnlyWhatItMarked(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, site...)
	w.pass()
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site...)
	w.n.beforeMark = func(kind string) error {
		if kind != keyRecovery {
			return nil
		}
		w.n.beforeMark = nil
		due, err := w.st.ListRecoveryDueEpisodes(w.ctx, store.ScopeAll())
		if err != nil {
			return err
		}
		for _, e := range due {
			if e.SubjectID == site[0] {
				_, err = w.st.MarkRecoveryNotified(w.ctx, store.ScopeAll(), []string{e.ID}, w.clk.Now().Add(-time.Second))
			}
		}
		return err
	}
	res := w.pass()
	if res.Recovered != 1 || res.Sent != 1 {
		t.Fatalf("recovery pass %+v", res)
	}
	h0, h1 := w.host[site[0]], w.host[site[1]]
	if w.mentions(mail.KindRecovery, h0) != 0 || w.mentions(mail.KindRecovery, h1) != 1 {
		t.Fatalf("recovery names dev0 %d, dev1 %d", w.mentions(mail.KindRecovery, h0), w.mentions(mail.KindRecovery, h1))
	}
	last := w.fake.Sent()[len(w.fake.Sent())-1]
	if !strings.Contains(last.Subject, "1 alarm resolved") {
		t.Fatalf("subject %q", last.Subject)
	}
}

// P20-W2B item 9. A Notifier and a Retrier on one store, with a frozen clock, picking batch ids
// at the same time: no two picks are ever equal.
func TestBatchIDsAreUniqueAcrossNotifierAndRetrier(t *testing.T) {
	w := newWorld(t)
	other := &Notifier{Store: w.st, Dispatch: w.n.Dispatch}
	pickers := []func(context.Context) (int64, error){w.n.batchID, w.r.batchID, other.batchID}
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for _, pick := range pickers {
		wg.Add(1)
		go func(pick func(context.Context) (int64, error)) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id, err := pick(w.ctx)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[id] {
					t.Errorf("batch id %d picked twice", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}(pick)
	}
	wg.Wait()
	if len(seen) != 150 {
		t.Fatalf("%d distinct ids", len(seen))
	}
}

// The mid-pass acknowledgement re-read leaves nothing to mark: the suppressed rows say so in the
// log (why muted_mid_pass), not "overlap".
func TestNotifyAckMidPassLeavingNothingSaysSo(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, site...)
	w.n.beforeMark = func(string) error {
		w.n.beforeMark = nil
		w.ackDevice(site[0])
		return nil
	}
	if res := w.pass(); res.Messages != 0 || res.Sent != 0 {
		t.Fatalf("pass %+v", res)
	}
	logs := w.logs.String()
	if !strings.Contains(logs, "muted_mid_pass") || strings.Contains(logs, "overlap") {
		t.Fatalf("log:\n%s", logs)
	}
}

// P20-W2BF item 6: a muted episode that closes between the retry's read of its batch and the
// release is settled in the same step (recovery done with no message), never left in the batch
// owing a recovery for an alert it was not in.
func TestRetryReleaseRaceWithAClose(t *testing.T) {
	w := newWorld(t)
	w.recipient("ops@example.test", true, true)
	site := w.twoDeviceFailedBatch("ops@example.test")
	w.ackDevice(site[0])
	w.clk.Advance(time.Minute)
	w.r.beforeLeave = func() {
		w.r.beforeLeave = nil
		w.set(state.StateOK, site[0]) // dev0 closes between the read and the release
		w.n.Configured = false
		defer func() { w.n.Configured = true }()
		w.pass()
	}
	if rr := w.retry(); rr.Sent != 1 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	h0 := w.host[site[0]]
	if w.mentions(mail.KindAlert, h0) != 0 || w.mentions(mail.KindRecovery, h0) != 0 || w.recoveryOwed() != 0 {
		t.Fatalf("dev0 alert %d recovery %d, owed %d", w.mentions(mail.KindAlert, h0), w.mentions(mail.KindRecovery, h0), w.recoveryOwed())
	}
}
