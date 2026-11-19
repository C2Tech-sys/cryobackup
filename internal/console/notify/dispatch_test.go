package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// clock is an advanceable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func memStore(t *testing.T, at time.Time) (*store.Store, *clock) {
	t.Helper()
	c := &clock{t: at}
	st, err := store.OpenMemory(context.Background(), c.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Each test starts its batch ids at its own clock: the process-wide source would otherwise carry
	// the last id of an earlier test (the tests of this package do not run in parallel).
	processBatchIDs.mu.Lock()
	processBatchIDs.last = 0
	processBatchIDs.mu.Unlock()
	return st, c
}

func testMessage() mail.Message {
	return mail.Message{To: []string{"ops@example.com"}, Subject: "[CryoBackup] test", Text: "body"}
}

func consoleRow(key string) store.DeliveryInput {
	return store.DeliveryInput{Kind: store.DeliveryAlert, DedupeKey: key, SubjectType: "console"}
}

func sendFacts(t *testing.T, st *store.Store) store.SendFacts {
	t.Helper()
	f, err := st.LoadSendFacts(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDispatcherSent(t *testing.T) {
	st, clk := memStore(t, renderNow)
	fake := mail.NewFakeSender()
	d := &Dispatcher{Store: st, Sender: fake, From: "CryoBackup <console@example.com>"}
	r, err := d.Send(context.Background(), consoleRow("alert:1:a"), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Sent || r.Err != nil || r.Settled || r.Delivery.Result != store.DeliverySent || !r.Delivery.SentAt.Equal(clk.Now()) {
		t.Fatalf("result %+v", r)
	}
	sent := fake.Sent()
	if len(sent) != 1 || sent[0].From != "CryoBackup <console@example.com>" || sent[0].Kind != mail.KindAlert {
		t.Fatalf("sent %+v", sent)
	}
	// The same dedupe key again: settled, nothing sent.
	r, err = d.Send(context.Background(), consoleRow("alert:1:a"), testMessage())
	if err != nil || !r.Settled || r.Sent || fake.Calls() != 1 {
		t.Fatalf("second send %+v, %v, calls %d", r, err, fake.Calls())
	}
}

func TestDispatcherTemporaryFailureSchedulesTheRetry(t *testing.T) {
	st, clk := memStore(t, renderNow)
	fake := mail.NewFakeSender()
	fake.FailWith(&mail.SendError{Stage: mail.StageConnect, Class: mail.ClassRefused, Temporary: true})
	d := &Dispatcher{Store: st, Sender: fake, From: "console@example.com"}
	r, err := d.Send(context.Background(), consoleRow("alert:1:t"), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	row := r.Delivery
	if r.Sent || r.Err == nil || row.Result != store.DeliveryFailed || row.ErrorClass != "refused" || row.NextRetryAt == nil ||
		!row.NextRetryAt.Equal(clk.Now().Add(time.Minute)) || row.Attempt != 1 {
		t.Fatalf("first failure %+v", r)
	}
	// The retries follow the schedule: 1, 5, 15 minutes, then the row gives up.
	for i, want := range []time.Duration{5 * time.Minute, 15 * time.Minute} {
		clk.Advance(time.Hour)
		r, err = d.Deliver(context.Background(), row, testMessage())
		if err != nil {
			t.Fatal(err)
		}
		row = r.Delivery
		if row.Attempt != i+2 || row.NextRetryAt == nil || !row.NextRetryAt.Equal(clk.Now().Add(want)) {
			t.Fatalf("retry %d: %+v", i+1, row)
		}
	}
	clk.Advance(time.Hour)
	r, err = d.Deliver(context.Background(), row, testMessage())
	if err != nil {
		t.Fatal(err)
	}
	if r.Delivery.Attempt != 4 || r.Delivery.NextRetryAt != nil || r.Delivery.Result != store.DeliveryFailed {
		t.Fatalf("final attempt %+v", r.Delivery)
	}
	if f := sendFacts(t, st); f.ConsecutiveFailures != 4 || f.PendingAlerts != 0 {
		t.Fatalf("facts after four failed attempts: %+v", f)
	}
	// A final row is settled: nothing is sent again.
	calls := fake.Calls()
	r, err = d.Deliver(context.Background(), r.Delivery, testMessage())
	if err != nil || !r.Settled || fake.Calls() != calls {
		t.Fatalf("deliver a final row: %+v %v", r, err)
	}
}

func TestDispatcherPermanentFailureIsFinal(t *testing.T) {
	st, _ := memStore(t, renderNow)
	fake := mail.NewFakeSender()
	fake.FailWith(&mail.SendError{Stage: mail.StageAuth, Class: mail.ClassAuthRejected})
	d := &Dispatcher{Store: st, Sender: fake, From: "console@example.com"}
	r, err := d.Send(context.Background(), consoleRow("alert:1:p"), testMessage())
	if err != nil {
		t.Fatal(err)
	}
	if r.Delivery.Result != store.DeliveryFailed || r.Delivery.NextRetryAt != nil || r.Delivery.ErrorClass != "auth_rejected" {
		t.Fatalf("permanent failure %+v", r.Delivery)
	}
	if due, _ := st.ListDueRetries(context.Background(), store.ScopeAll(), renderNow.Add(time.Hour), 0); len(due) != 0 {
		t.Fatalf("a permanent failure is due for retry: %+v", due)
	}
}

// A row someone else finished between the read and the mark is settled, not an error.
func TestDispatcherConflictIsSettled(t *testing.T) {
	st, _ := memStore(t, renderNow)
	fake := mail.NewFakeSender()
	d := &Dispatcher{Store: st, Sender: fake, From: "console@example.com"}
	row, _, err := d.Enqueue(context.Background(), consoleRow("alert:1:c"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkDeliverySent(context.Background(), store.ScopeAll(), row.ID); err != nil {
		t.Fatal(err)
	}
	r, err := d.Deliver(context.Background(), row, testMessage()) // row still reads queued
	if err != nil || !r.Settled {
		t.Fatalf("result %+v, %v", r, err)
	}
}

// A sender that breaks the contract with another error type is classed by its deadline; its text
// never reaches the row.
type rudeSender struct{ block bool }

func (s rudeSender) Send(ctx context.Context, _ mail.Message) error {
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return errors.New(`dial tcp relay.example.com:25: C:\secret`)
}

func TestDispatcherClassesForeignErrors(t *testing.T) {
	st, _ := memStore(t, renderNow)
	d := &Dispatcher{Store: st, Sender: rudeSender{}, From: "console@example.com"}
	r, err := d.Send(context.Background(), consoleRow("alert:1:r"), testMessage())
	if err != nil || r.Delivery.ErrorClass != "network" || r.Delivery.NextRetryAt == nil {
		t.Fatalf("foreign error: %+v %v", r, err)
	}
	d = &Dispatcher{Store: st, Sender: rudeSender{block: true}, From: "console@example.com", SendTimeout: 20 * time.Millisecond}
	start := time.Now()
	r, err = d.Send(context.Background(), consoleRow("alert:1:h"), testMessage())
	if err != nil || r.Delivery.ErrorClass != "timeout" || time.Since(start) > 5*time.Second {
		t.Fatalf("hanging sender: %+v %v after %v", r, err, time.Since(start))
	}
}

func TestDispatcherSuppress(t *testing.T) {
	st, _ := memStore(t, renderNow)
	d := &Dispatcher{Store: st, Sender: mail.NewFakeSender(), From: "console@example.com"}
	row, _, err := d.Enqueue(context.Background(), consoleRow("alert:1:s"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Suppress(context.Background(), row, "deduped")
	if err != nil || r.Delivery.Result != store.DeliverySuppressed {
		t.Fatalf("suppress %+v %v", r, err)
	}
	if r, err := d.Deliver(context.Background(), r.Delivery, testMessage()); err != nil || !r.Settled {
		t.Fatalf("deliver a suppressed row: %+v %v", r, err)
	}
}
