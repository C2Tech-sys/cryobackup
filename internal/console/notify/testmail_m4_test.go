package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P20-W3A: the test mail of ADR 0020 decision 11, shared by the admin API and the e-mail page.

func testMailer(t *testing.T, s mail.Sender) (*TestMailer, *store.Store, *clock) {
	t.Helper()
	st, clk := memStore(t, renderNow)
	d := &Dispatcher{Store: st, Sender: s, From: "CryoBackup <console@example.com>"}
	return &TestMailer{Dispatch: d, Configured: true, ConsoleURL: "https://console.example.test/"}, st, clk
}

func deliveries(t *testing.T, st *store.Store) []store.Delivery {
	t.Helper()
	l, err := st.ListRecentDeliveries(context.Background(), store.ScopeAll(), store.DeliveryFilter{}, store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestTestMailSentIsLoggedAsATestAndNamesNothing(t *testing.T) {
	fake := mail.NewFakeSender()
	tm, st, _ := testMailer(t, fake)
	res, err := tm.Send(context.Background(), "  ops@example.com ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Sent || res.Failure != nil || res.To != "ops@example.com" || res.Delivery.Kind != store.DeliveryTest || res.Delivery.Result != store.DeliverySent {
		t.Fatalf("result %+v", res)
	}
	sent := fake.Sent()
	if len(sent) != 1 || sent[0].Kind != mail.KindTest || sent[0].From != "CryoBackup <console@example.com>" || len(sent[0].To) != 1 || sent[0].To[0] != "ops@example.com" {
		t.Fatalf("sent %+v", sent)
	}
	if !strings.Contains(sent[0].Text, "test message") || !strings.Contains(sent[0].HTML, "https://console.example.test/") {
		t.Fatalf("message text %q", sent[0].Text)
	}
	f := sendFacts(t, st)
	if f.LastTest == nil || !f.LastTest.OK || f.ConsecutiveFailures != 0 || !f.LastSentAt.IsZero() {
		t.Fatalf("a test is reported on its own, never as a real send: %+v", f)
	}
}

func TestTestMailFailureIsFinalAndNeverCounted(t *testing.T) {
	fake := mail.NewFakeSender()
	fake.FailWith(&mail.SendError{Stage: mail.StageAuth, Class: mail.ClassAuthRejected, Temporary: true}) // temporary: a real send would be retried
	tm, st, clk := testMailer(t, fake)
	res, err := tm.Send(context.Background(), "ops@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent || res.Failure == nil || res.Failure.Stage != mail.StageAuth || res.Failure.Class != mail.ClassAuthRejected {
		t.Fatalf("result %+v", res)
	}
	if d := res.Delivery; d.Result != store.DeliveryFailed || d.NextRetryAt != nil || d.ErrorClass != "auth_rejected" || d.Attempt != 1 {
		t.Fatalf("a failed test is final at once: %+v", d)
	}
	f := sendFacts(t, st)
	if f.ConsecutiveFailures != 0 || f.PendingAlerts != 0 || f.LastTest == nil || f.LastTest.OK || f.LastTest.ErrorClass != "auth_rejected" {
		t.Fatalf("facts %+v", f)
	}
	clk.Advance(time.Hour)
	due, err := st.ListDueRetries(context.Background(), store.ScopeAll(), clk.Now(), 0)
	if err != nil || len(due) != 0 {
		t.Fatalf("a failed test is never due for a retry: %v %v", due, err)
	}
}

func TestTestMailRefusesAnAddressWithoutEchoingIt(t *testing.T) {
	fake := mail.NewFakeSender()
	tm, st, _ := testMailer(t, fake)
	for _, bad := range []string{"", "Ops <ops@example.com>", "ops@localhost", "ops@example.com\r\nBcc: x@example.com", "a@b@example.com", `"q"@example.com`} {
		_, err := tm.Send(context.Background(), bad, nil)
		fe := store.FieldErrors(err)
		if len(fe) != 1 || fe[0].Field != "to" || !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("%q: %v", bad, err)
		}
		if bad != "" && strings.Contains(fe[0].Message, bad) {
			t.Fatalf("%q: the message echoes the input: %q", bad, fe[0].Message)
		}
	}
	if fake.Calls() != 0 || len(deliveries(t, st)) != 0 {
		t.Fatalf("a refused address sends and records nothing: %d calls", fake.Calls())
	}
}

func TestTestMailNotConfigured(t *testing.T) {
	fake := mail.NewFakeSender()
	tm, st, _ := testMailer(t, fake)
	tm.Configured = false
	admitted := false
	if _, err := tm.Send(context.Background(), "ops@example.com", func(context.Context) error { admitted = true; return nil }); !errors.Is(err, ErrTestMailNotConfigured) {
		t.Fatalf("err %v", err)
	}
	if admitted || fake.Calls() != 0 || len(deliveries(t, st)) != 0 {
		t.Fatal("without a relay nothing is admitted, sent or recorded")
	}
}

func TestTestMailRateLimitIsPerConsoleAndARefusedAdmissionGivesTheSlotBack(t *testing.T) {
	fake := mail.NewFakeSender()
	tm, st, clk := testMailer(t, fake)
	refuse := errors.New("not audited")
	if _, err := tm.Send(context.Background(), "ops@example.com", func(context.Context) error { return refuse }); !errors.Is(err, refuse) {
		t.Fatalf("admission refusal: %v", err)
	}
	if fake.Calls() != 0 || len(deliveries(t, st)) != 0 {
		t.Fatal("a refused admission sends and records nothing")
	}
	if _, err := tm.Send(context.Background(), "ops@example.com", nil); err != nil {
		t.Fatalf("the refused admission gave its slot back: %v", err)
	}
	clk.Advance(4 * time.Second)
	_, err := tm.Send(context.Background(), "other@example.com", nil)
	var soon *TestMailTooSoonError
	if !errors.As(err, &soon) || soon.RetryAfter != 6*time.Second {
		t.Fatalf("a second test 4 s later: %v", err)
	}
	clk.Advance(6 * time.Second)
	if _, err := tm.Send(context.Background(), "other@example.com", nil); err != nil {
		t.Fatalf("10 s later: %v", err)
	}
	if fake.Calls() != 2 || len(deliveries(t, st)) != 2 {
		t.Fatalf("calls %d", fake.Calls())
	}
}

// deafSender ignores its context and returns only when released.
type deafSender struct{ release chan struct{} }

func (s deafSender) Send(context.Context, mail.Message) error { <-s.release; return nil }

func TestTestMailHangingSenderDoesNotHoldTheCaller(t *testing.T) {
	tm, _, _ := testMailer(t, rudeSender{block: true}) // honours its context
	tm.Dispatch.SendTimeout = 50 * time.Millisecond
	start := time.Now()
	res, err := tm.Send(context.Background(), "ops@example.com", nil)
	if err != nil || res.Sent || res.Failure == nil || res.Failure.Class != mail.ClassTimeout || res.Delivery.NextRetryAt != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("hanging sender: %+v %v after %v", res, err, time.Since(start))
	}

	old := testMailGrace
	testMailGrace = 50 * time.Millisecond
	t.Cleanup(func() { testMailGrace = old })
	deaf := deafSender{release: make(chan struct{})}
	t.Cleanup(func() { close(deaf.release) })
	tm2, _, _ := testMailer(t, deaf)
	tm2.Dispatch.SendTimeout = 50 * time.Millisecond
	start = time.Now()
	res, err = tm2.Send(context.Background(), "ops@example.com", nil)
	if err != nil || res.Sent || res.Failure == nil || res.Failure.Class != mail.ClassTimeout || time.Since(start) > 5*time.Second {
		t.Fatalf("a sender that ignores its context: %+v %v after %v", res, err, time.Since(start))
	}
}
