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

// P20-W2BF D3 (the scenario of the W2B test of this name, under the C1 rule): A received the full
// alert, B's first send failed and dev0 is acknowledged before B's retry. B's retry still carries
// dev0; when dev0 closes its recovery goes to A, the one recipient with the recoveries switch.
func TestRetryReleasesNothingWhenAnotherRecipientSawTheAlert(t *testing.T) {
	w := newWorld(t)
	w.recipient(addrA, true, true)
	w.recipient(addrB, true, false)
	site := w.twoDeviceFailedBatch(addrB)
	w.ackDevice(site[0])
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.RecoveryUnsent != 0 {
		t.Fatalf("B's retry %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[0])
	if res := w.pass(); res.Recovered != 1 {
		t.Fatalf("recovery pass %+v", res)
	}
	w.expect(site[0], each(1), map[string]int{addrA: 1, addrB: 0})
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

// P20-W2BF D3: the rows that are queued and never attempted are made the way the product makes
// them, never by rewriting a row in the store: a send still in flight (passInFlight), a pass that
// stops between its marks and its sends (a restart there: Notifier.stopBeforeSend, the product's
// own early return), and a row deferred because an earlier send used up the pass's send budget
// (passDeferring).

// hookSender is the world's FakeSender behind a hook that runs before each send and may block it.
type hookSender struct {
	inner mail.Sender
	hook  func(ctx context.Context, m mail.Message)
}

func (h hookSender) Send(ctx context.Context, m mail.Message) error {
	h.hook(ctx, m)
	return h.inner.Send(ctx, m)
}

// firstAlertTo returns a test that is true for the first alert to addr it sees, false otherwise.
func firstAlertTo(addr string) func(mail.Message) bool {
	var mu sync.Mutex
	seen := false
	return func(m mail.Message) bool {
		mu.Lock()
		defer mu.Unlock()
		if seen || m.Kind != mail.KindAlert || m.To[0] != addr {
			return false
		}
		seen = true
		return true
	}
}

// inFlight is a pass running in its own goroutine whose send of the alert to one recipient is held
// before it reaches the relay: that row is queued and has never been attempted, as long as the send
// is held. Everything else (other passes, retry runs) may run meanwhile.
type inFlight struct {
	w       *world
	release chan struct{}
	done    chan passOutcome
	res     *passOutcome
}

type passOutcome struct {
	res PassResult
	err error
}

// passInFlight starts a pass over the view as it is now and returns once its send of the alert to
// addr is held (ok), or once the pass ended without such a send (not ok: no row for addr, e.g. the
// whole batch was muted when it formed). Call finish in both cases.
func (w *world) passInFlight(addr string) (*inFlight, bool) {
	w.t.Helper()
	f := &inFlight{w: w, release: make(chan struct{}), done: make(chan passOutcome, 1)}
	held := make(chan struct{})
	first := firstAlertTo(addr)
	d := w.n.Dispatch
	d.Sender = hookSender{inner: w.fake, hook: func(_ context.Context, m mail.Message) {
		if first(m) {
			close(held)
			<-f.release
		}
	}}
	v := w.view() // read here: the test goroutine owns the world's maps
	go func() {
		res, err := w.n.Pass(w.ctx, v)
		f.done <- passOutcome{res, err}
	}()
	select {
	case <-held:
		return f, true
	case o := <-f.done:
		f.res = &o
		d.Sender = w.fake
		return f, false
	}
}

// finish lets the held send go to the relay (FakeSender: what it answers is what FailWhen says at
// this moment) and waits for the pass.
func (f *inFlight) finish() PassResult {
	f.w.t.Helper()
	if f.res == nil {
		close(f.release)
		o := <-f.done
		f.res = &o
		f.w.n.Dispatch.Sender = f.w.fake
	}
	if f.res.err != nil {
		f.w.t.Fatalf("pass with a send in flight: %v", f.res.err)
	}
	return f.res.res
}

// passUnconfigured is a pass that only syncs the episodes (no relay), on a copy of the notifier:
// it may run beside a pass in flight without writing a field that pass reads.
func (w *world) passUnconfigured() {
	w.t.Helper()
	n := *w.n
	n.Configured = false
	if _, err := n.Pass(w.ctx, w.view()); err != nil {
		w.t.Fatalf("pass: %v", err)
	}
}

// passDeferring is a pass whose send of the alert to addr (the first recipient by name) takes the
// pass's whole send budget, as a slow relay takes DefaultSendBudget: the rows after it are deferred,
// queued and never attempted, for the retry job (store.QueuedStaleAfter). The slow send itself goes
// through, or fails as FailWhen says.
func (w *world) passDeferring(addr string) PassResult {
	w.t.Helper()
	first := firstAlertTo(addr)
	d := w.n.Dispatch
	d.Sender = hookSender{inner: w.fake, hook: func(ctx context.Context, m mail.Message) {
		if first(m) {
			<-ctx.Done() // the pass's budget ends while this send is under way
		}
	}}
	w.n.SendBudget = 50 * time.Millisecond
	res := w.pass()
	w.n.SendBudget = 0
	d.Sender = w.fake
	return res
}

// passStoppingBeforeSend is a pass that writes its rows and marks its episodes, then stops: the
// process restarts there. Every row it wrote stays queued, never attempted.
func (w *world) passStoppingBeforeSend() PassResult {
	w.t.Helper()
	w.n.stopBeforeSend = true
	res := w.pass()
	w.n.stopBeforeSend = false
	return res
}

func temporary() *mail.SendError {
	return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
}

// A's first send fails; B's send is in flight while dev0 closes and A retries; then B's send goes
// out. B received dev0 in its alert and gets its recovery; A gets dev0's recovery only (the batch
// rule: an alert of the batch reached someone).
func TestC1SiblingInFlightThenSent(t *testing.T) {
	w := newWorld(t)
	w.recipient(addrA, true, true)
	w.recipient(addrB, true, true)
	site := w.site("Summit Eye Care", 2)
	w.set(state.StateFailed, site...)
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == addrA {
			return temporary()
		}
		return nil
	})
	f, ok := w.passInFlight(addrB)
	if !ok {
		t.Fatal("B's send was never in flight")
	}
	w.fake.FailWhen(nil)
	w.set(state.StateOK, site[0])
	w.passUnconfigured()
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.RecoveryUnsent != 0 {
		t.Fatalf("A's retry %+v: dev0 must stay unsettled while B's send is in flight", rr)
	}
	if res := f.finish(); res.Sent != 1 {
		t.Fatalf("B's in-flight send %+v", res)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	w.expect(site[0], map[string]int{addrA: 0, addrB: 1}, each(1))
	w.expect(site[1], each(1), each(0))
}

// The residual case (package doc), way in 1: B's in-flight send fails, and is retried after dev0
// closed. Nobody received dev0's alert, yet A's retry was delivered, so the batch counts as
// delivered and both get dev0's recovery: a spurious "resolved", never a lost alert.
func TestC1ResidualSiblingInFlightThenFailedAndRetried(t *testing.T) {
	w := newWorld(t)
	w.recipient(addrA, true, true)
	w.recipient(addrB, true, true)
	site := w.site("Summit Eye Care", 2)
	w.set(state.StateFailed, site...)
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == addrA {
			return temporary()
		}
		return nil
	})
	f, ok := w.passInFlight(addrB)
	if !ok {
		t.Fatal("B's send was never in flight")
	}
	w.fake.FailWhen(nil)
	w.set(state.StateOK, site[0])
	w.passUnconfigured()
	w.clk.Advance(time.Minute)
	w.retry()
	w.fake.FailWhen(func(mail.Message) *mail.SendError { return temporary() })
	if res := f.finish(); res.Sent != 0 || res.Failed != 2 { // A's first send and B's, both failed
		t.Fatalf("B's in-flight send %+v", res)
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

// The residual case, way in 2 (the verifier's k2): the pass marks the batch and stops before
// sending, the process restarts, dev0 closes, and the stale queued rows go out with dev1 only.
// Each recipient gets a "resolved" for dev0, which nobody was mailed.
func TestC1ResidualRestartBetweenMarkAndSend(t *testing.T) {
	w := newWorld(t)
	w.recipient(addrA, true, true)
	w.recipient(addrB, true, true)
	site := w.site("Summit Eye Care", 2)
	w.set(state.StateFailed, site...)
	if res := w.passStoppingBeforeSend(); res.Messages != 2 || res.Deferred != 2 || res.Sent != 0 {
		t.Fatalf("pass stopped before its sends %+v", res)
	}
	w.set(state.StateOK, site[0])
	w.clk.Advance(time.Minute)
	w.pass()
	w.clk.Advance(store.QueuedStaleAfter)
	if rr := w.retry(); rr.Sent != 2 || rr.RecoveryUnsent != 0 {
		t.Fatalf("retry of the queued rows %+v", rr)
	}
	w.clk.Advance(time.Minute)
	w.pass()
	w.expect(site[0], each(0), each(1))
	w.expect(site[1], each(1), each(0))
}

// The residual case, way in 3 (the verifier's d2): A's send fails after using up the pass's send
// budget, B's row is deferred (queued, never attempted), dev0 closes before either goes out. Each
// recipient gets a "resolved" for dev0, which nobody was mailed.
func TestC1ResidualRowDeferredBySendBudget(t *testing.T) {
	w := newWorld(t)
	w.recipient(addrA, true, true)
	w.recipient(addrB, true, true)
	site := w.site("Summit Eye Care", 2)
	w.set(state.StateFailed, site...)
	w.fake.FailWhen(func(m mail.Message) *mail.SendError {
		if m.To[0] == addrA {
			return temporary()
		}
		return nil
	})
	if res := w.passDeferring(addrA); res.Failed != 1 || res.Deferred != 1 {
		t.Fatalf("pass whose first send used up the budget %+v", res)
	}
	w.fake.FailWhen(nil)
	w.set(state.StateOK, site[0])
	w.passUnconfigured()
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Sent != 1 || rr.RecoveryUnsent != 0 {
		t.Fatalf("A's retry %+v: dev0 must stay unsettled while B's row is queued", rr)
	}
	w.clk.Advance(store.QueuedStaleAfter)
	if rr := w.retry(); rr.Sent != 1 {
		t.Fatalf("B's deferred row %+v", rr)
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
