package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-W2BF C1: a retried alert is the original notification, delayed. It carries every episode of
// its batch that is still open, muted since or not; only a closed episode is left out. These tests
// assert, per recipient, how often each device was alerted and recovered.

const (
	addrA = "a@example.test"
	addrB = "b@example.test"
)

// count is how many messages of the kind to addr name host.
func (w *world) count(addr string, kind mail.Kind, host string) int {
	n := 0
	for _, m := range w.fake.Sent() {
		if m.To[0] == addr && m.Kind == kind && strings.Contains(m.Text, host) {
			n++
		}
	}
	return n
}

// expect checks, for each recipient, the alert and recovery counts of each device.
func (w *world) expect(dev string, alerts, recoveries map[string]int) {
	w.t.Helper()
	h := w.host[dev]
	for addr, n := range alerts {
		if got := w.count(addr, mail.KindAlert, h); got != n {
			w.t.Errorf("%s: %d alerts naming %s, want %d", addr, got, h, n)
		}
	}
	for addr, n := range recoveries {
		if got := w.count(addr, mail.KindRecovery, h); got != n {
			w.t.Errorf("%s: %d recoveries naming %s, want %d", addr, got, h, n)
		}
	}
}

// bothFailOnce mails a FAILED batch of two devices to A and B; both first sends fail temporarily,
// then the relay accepts everything.
func (w *world) bothFailOnce() []string {
	w.t.Helper()
	w.recipient(addrA, true, true)
	w.recipient(addrB, true, true)
	site := w.twoDeviceFailedBatch(addrA, addrB)
	return site
}

func (w *world) snooze(dev string, d time.Duration) {
	w.t.Helper()
	until := w.clk.Now().Add(d)
	if _, err := w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: dev, AlarmType: "failed", SnoozeUntil: &until}, w.actor); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) clearAcks() {
	w.t.Helper()
	for _, a := range w.activeAcks() {
		if _, err := w.st.ClearAck(w.ctx, store.ScopeAll(), a.ID, w.actor); err != nil {
			w.t.Fatal(err)
		}
	}
}

func each(n int) map[string]int { return map[string]int{addrA: n, addrB: n} }

// Acknowledged before the retries: both retries carry dev0; when it closes both get its recovery.
func TestC1RetryCarriesAnEpisodeAcknowledgedSince(t *testing.T) {
	w := newWorld(t)
	site := w.bothFailOnce()
	w.ackDevice(site[0])
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 2 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0])
	w.pass()
	w.expect(site[0], each(1), each(1))
	w.expect(site[1], each(1), each(0))
}

// Customer suspended, then resumed while still failing: alerted once to both, by the retries.
func TestC1RetryCarriesASuspendedCustomer(t *testing.T) {
	w := newWorld(t)
	site := w.bothFailOnce()
	w.muted[w.cust["Summit Eye Care"]] = true
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 2 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	w.muted[w.cust["Summit Eye Care"]] = false
	w.clk.Advance(time.Minute)
	w.pass()
	w.expect(site[0], each(1), each(0))
	w.expect(site[1], each(1), each(0))
}

// A plain acknowledgement, then cleared while still failing: alerted once.
func TestC1PlainAckClearedAfterTheRetry(t *testing.T) {
	w := newWorld(t)
	site := w.bothFailOnce()
	w.ackDevice(site[0])
	w.clk.Advance(time.Minute)
	w.retry()
	w.clearAcks()
	w.clk.Advance(time.Minute)
	w.pass()
	w.expect(site[0], each(1), each(0))
}

// A snooze lapsing after the retries were sent, while the alarm persists: re-armed and mailed
// again. ADR 0020 decision 7 asks for exactly that ("A snooze that ends while the alarm persists
// raises the alert again"), so the second alert is wanted.
func TestC1SnoozeLapsingAfterTheRetriesReRaises(t *testing.T) {
	w := newWorld(t)
	site := w.bothFailOnce()
	w.snooze(site[0], 10*time.Minute)
	w.clk.Advance(time.Minute)
	w.retry()
	w.clk.Advance(15 * time.Minute)
	if res := w.pass(); res.Rearmed != 1 || res.Alerted != 1 {
		t.Fatalf("pass after the snooze lapsed %+v", res)
	}
	w.expect(site[0], each(2), each(0))
	w.expect(site[1], each(1), each(0))
}

// Every episode closed before the retry: nothing is sent, the recoveries are settled, none mailed.
func TestC1EveryEpisodeClosedBeforeTheRetry(t *testing.T) {
	w := newWorld(t)
	site := w.bothFailOnce()
	w.set(state.StateOK, site...)
	w.n.Configured = false
	w.pass()
	w.n.Configured = true
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 0 || rr.Suppressed != 2 {
		t.Fatalf("retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	if w.recoveryOwed() != 0 {
		t.Fatal("recoveries still owed")
	}
	w.expect(site[0], each(0), each(0))
	w.expect(site[1], each(0), each(0))
}

// inFlight puts B's row of the batch back to queued, never attempted: a send the pass that formed
// the batch still has in flight. It returns the row.
func (w *world) inFlight(addr string) store.Delivery {
	w.t.Helper()
	d, ok := w.inFlightRow(addr)
	if !ok {
		w.t.Fatalf("no alert row for %s", addr)
	}
	return d
}

// inFlightRow is inFlight that reports, instead of failing, when the recipient has no alert row.
func (w *world) inFlightRow(addr string) (store.Delivery, bool) {
	w.t.Helper()
	for _, d := range w.deliveries() {
		r, err := w.st.GetRecipient(w.ctx, store.ScopeAll(), d.RuleID)
		if err != nil || r.Address != addr || d.Kind != store.DeliveryAlert {
			continue
		}
		if err := w.st.Write(w.ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE notification_log SET result = 'queued', next_retry_at = NULL, error_class = NULL WHERE id = ?`, d.ID)
			return err
		}); err != nil {
			w.t.Fatal(err)
		}
		d.Result, d.NextRetryAt = store.DeliveryQueued, nil
		return d, true
	}
	return store.Delivery{}, false
}

// finishInFlight completes the pass's in-flight send of row with the message as the pass rendered
// it: every episode of the batch.
func (w *world) finishInFlight(row store.Delivery, addr string) Result {
	w.t.Helper()
	at, _, err := parseBatchKey(row.DedupeKey, keyAlert)
	if err != nil {
		w.t.Fatal(err)
	}
	eps, err := w.st.ListEpisodesNotifiedAt(w.ctx, store.ScopeAll(), at)
	if err != nil {
		w.t.Fatal(err)
	}
	c, err := RenderAlert(w.view(), eps, Options{})
	if err != nil {
		w.t.Fatal(err)
	}
	out, err := w.n.Dispatch.Deliver(w.ctx, row, c.Message(addr))
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

// B's send is in flight while A retries after dev0 closed; then B's send goes out. B received dev0
// in its alert and gets its recovery; A gets dev0's recovery only (the batch rule: an alert of the
// batch reached someone).
func TestC1SiblingInFlightThenSent(t *testing.T) {
	w := newWorld(t)
	site := w.bothFailOnce()
	row := w.inFlight(addrB)
	w.set(state.StateOK, site[0])
	w.n.Configured = false
	w.pass()
	w.n.Configured = true
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.RecoveryUnsent != 0 {
		t.Fatalf("A's retry %+v: dev0 must stay unsettled while B's send is in flight", rr)
	}
	if out := w.finishInFlight(row, addrB); !out.Sent {
		t.Fatalf("B's in-flight send %+v", out)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	w.expect(site[0], map[string]int{addrA: 0, addrB: 1}, each(1))
	w.expect(site[1], each(1), each(0))
}

// The residual case (package doc): B's in-flight send fails, and is retried after dev0 closed.
// Nobody received dev0's alert, yet A's retry was delivered, so the batch counts as delivered and
// both get dev0's recovery: a spurious "resolved", never a lost alert.
func TestC1ResidualSiblingInFlightThenFailedAndRetried(t *testing.T) {
	w := newWorld(t)
	site := w.bothFailOnce()
	row := w.inFlight(addrB)
	w.set(state.StateOK, site[0])
	w.n.Configured = false
	w.pass()
	w.n.Configured = true
	w.clk.Advance(time.Minute)
	w.retry()
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
	})
	if out := w.finishInFlight(row, addrB); out.Sent {
		t.Fatal("B's in-flight send went out")
	}
	w.fake.FailWhen(nil)
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 {
		t.Fatalf("B's retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	w.expect(site[0], each(0), each(1))
	w.expect(site[1], each(1), each(0))
}

func (w *world) activeAcks() []store.Ack {
	w.t.Helper()
	acks, err := w.st.ListActiveAcks(w.ctx, store.ScopeAll())
	if err != nil {
		w.t.Fatal(err)
	}
	return acks
}
