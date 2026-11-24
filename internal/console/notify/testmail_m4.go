package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The test mail (ADR 0020 decision 11): one message to an address an admin types, sent at once
// and answered on the page (or in the API response) with the stage and class word of a failure.
// The admin API (POST /api/v1/settings/smtp/test) and the console's e-mail page share one
// TestMailer, so the rate limit is per console whichever of the two asks.
//
// A test is a delivery-log row of kind test like every other message, so it is in the log and the
// newest one feeds banner C03 ("a test failed"). It is never counted in the consecutive-failure
// counter or among the real sends (store.MarkDeliveryFailed and store.LoadSendFacts leave kind
// test out), and it is never retried: a failure is final at once (no next_retry_at), and a test
// row the retry job finds anyway (a console that stopped mid-send) is suppressed there.

// TestMailInterval is the least time between two test messages of one console.
const TestMailInterval = 10 * time.Second

// testMailGrace is how long Send waits past its send deadline for a sender that does not honour
// its context. The request is answered either way; such a sender's goroutine ends when it returns.
// A variable so a test can shorten it.
var testMailGrace = 2 * time.Second

// ErrTestMailNotConfigured is what TestMailer.Send returns when no relay is configured: nothing is
// recorded and nothing is sent.
var ErrTestMailNotConfigured = errors.New("notify: no mail relay is configured")

// TestMailTooSoonError is what TestMailer.Send returns when the console sent a test less than its
// interval ago. RetryAfter is how long until the next one is accepted.
type TestMailTooSoonError struct{ RetryAfter time.Duration }

// Error implements error.
func (e *TestMailTooSoonError) Error() string {
	return "notify: a test message was sent less than " + TestMailInterval.String() + " ago"
}

// testMailAddressRule is the sentence of a refused address: the store's recipient rule, never the input.
const testMailAddressRule = "Use one plain e-mail address such as name@example.com, without a display name."

// TestMailResult is the outcome of one test message.
type TestMailResult struct {
	// To is the address the message was for, as store.CleanMailAddress cleaned it.
	To string
	// Delivery is the delivery-log row (kind test) as it is after the attempt.
	Delivery store.Delivery
	// Sent is true when the relay accepted the message.
	Sent bool
	// Failure is the stage and class word of a failed attempt; nil when Sent.
	Failure *mail.SendError
}

// TestMailer sends test messages. The zero value is not usable: Dispatch is required. It is safe
// for concurrent use; one TestMailer per console carries the rate limit.
type TestMailer struct {
	// Dispatch supplies the store, the sender, the default From and the send deadline
	// (Dispatcher.SendTimeout, DefaultSendTimeout when zero).
	Dispatch *Dispatcher
	// Configured reports whether a relay is configured (mail.Settings.Configured; true in fixtures
	// mode, where the sender is a mail.FakeSender). Without one Send refuses with ErrTestMailNotConfigured.
	Configured bool
	// Loc returns the operator's display zone for the time in the message; nil means UTC.
	Loc func() *time.Location
	// ConsoleURL is the console address (config admin_url, never public_url) for the link in the message; empty means no link.
	ConsoleURL string
	// Interval is the least time between two tests; zero means TestMailInterval.
	Interval time.Duration
	// Log receives one line per test (delivery id, stage and class); nil means logx.Default().
	Log *logx.Logger

	mu   sync.Mutex
	last time.Time // when the newest admitted test was admitted (the store clock)
}

func (t *TestMailer) log() *logx.Logger {
	if t.Log != nil {
		return t.Log
	}
	return logx.Default()
}

func (t *TestMailer) interval() time.Duration {
	if t.Interval > 0 {
		return t.Interval
	}
	return TestMailInterval
}

// reserve takes the console's test slot at now, or says how long until it is free. The returned
// release gives the slot back, for a caller whose admission was refused before anything was sent.
func (t *TestMailer) reserve(now time.Time) (release func(), wait time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.last.IsZero() {
		if elapsed := now.Sub(t.last); elapsed >= 0 && elapsed < t.interval() {
			return nil, t.interval() - elapsed
		}
	}
	prev := t.last
	t.last = now
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.last.Equal(now) {
			t.last = prev
		}
	}, 0
}

// Send sends one test message to to, synchronously, and records it in the delivery log.
//
// The checks come first and in this order: to must pass store.CleanMailAddress (otherwise a
// *store.ValidationError on field "to" with the store's fixed sentence; the address is never
// echoed), a relay must be configured (ErrTestMailNotConfigured), and the console must not have
// admitted a test within Interval (*TestMailTooSoonError). Then admit, when not nil, is called:
// the caller writes its audit row there, and an error from it refuses the test (the slot is given
// back, nothing is recorded or sent, and Send returns that error).
//
// The send runs under the dispatcher's send deadline; a sender that ignores its context is waited
// for at most a short grace beyond it and the attempt is then a timeout. The row is marked sent,
// or failed with its class word and no retry. The error is a store error (or admit's); a send
// failure is in the result, never an error. Log lines carry the delivery id, stage and class only.
func (t *TestMailer) Send(ctx context.Context, to string, admit func(context.Context) error) (TestMailResult, error) {
	addr, ok := store.CleanMailAddress(to)
	if !ok {
		return TestMailResult{}, &store.ValidationError{Fields: []store.FieldError{{Field: "to", Message: testMailAddressRule}}}
	}
	if !t.Configured {
		return TestMailResult{}, ErrTestMailNotConfigured
	}
	d := t.Dispatch
	now := d.Store.Now()
	release, wait := t.reserve(now)
	if release == nil {
		return TestMailResult{}, &TestMailTooSoonError{RetryAfter: wait}
	}
	if admit != nil {
		if err := admit(ctx); err != nil {
			release()
			return TestMailResult{}, err
		}
	}
	key, err := testMailKey(now)
	if err != nil {
		return TestMailResult{}, err
	}
	row, _, err := d.Enqueue(ctx, store.DeliveryInput{Kind: store.DeliveryTest, DedupeKey: key, SubjectType: "console"})
	if err != nil {
		return TestMailResult{}, err
	}
	res := TestMailResult{To: addr, Delivery: row}
	m := t.message(now, addr)
	if m.From == "" {
		m.From = d.From
	}
	timeout := d.SendTimeout
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	failure := sendFailure(sctx, sendBounded(sctx, d.Sender, m, timeout+testMailGrace))
	cancel()
	mctx := context.WithoutCancel(ctx) // the outcome is recorded even when the caller went away
	if failure == nil {
		upd, err := d.Store.MarkDeliverySent(mctx, store.ScopeAll(), row.ID)
		if err != nil {
			t.log().Error("test mail sent but not recorded", logx.ID("delivery", row.ID), logx.Kind(store.DeliveryTest), logx.Class("store"))
			res.Sent = true
			return res, err
		}
		res.Delivery, res.Sent = upd, true
		t.log().Info("test mail sent", logx.ID("delivery", row.ID), logx.Kind(store.DeliveryTest))
		return res, nil
	}
	res.Failure = failure
	upd, err := d.Store.MarkDeliveryFailed(mctx, store.ScopeAll(), row.ID, string(failure.Class), nil) // nil: never retried
	if err != nil {
		t.log().Error("test mail failed and not recorded", logx.ID("delivery", row.ID), logx.Kind(store.DeliveryTest), logx.Class("store"))
		return res, err
	}
	res.Delivery = upd
	t.log().Warn("test mail not delivered", logx.ID("delivery", row.ID), logx.Kind(store.DeliveryTest),
		logx.Str("stage", string(failure.Stage)), logx.Class(string(failure.Class)))
	return res, nil
}

// sendBounded runs s.Send and waits for it at most limit. A sender that has not returned by then
// is reported as a timeout; its goroutine finishes on its own (the channel is buffered).
func sendBounded(ctx context.Context, s mail.Sender, m mail.Message, limit time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- s.Send(ctx, m) }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
	}
}

// testMailKey is a dedupe key no other message can have: "test:<ms>:<random>".
func testMailKey(now time.Time) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "test:" + strconv.FormatInt(now.UnixMilli(), 10) + ":" + hex.EncodeToString(b[:]), nil
}

// testMailSource is the HTML part of the test message (P20-MD: the console's dark look, the mark
// as an inline image, no remote content).
//
//go:embed templates/test.html.tmpl
var testMailSource string

var testMailHTML = mdTemplate("test", testMailSource)

// message is the test message to addr at now. It names no customer, host or path.
func (t *TestMailer) message(now time.Time, addr string) mail.Message {
	loc := time.UTC
	if t.Loc != nil {
		if l := t.Loc(); l != nil {
			loc = l
		}
	}
	page := struct{ When, ConsoleURL string }{When: when(now, loc), ConsoleURL: consoleURL(t.ConsoleURL)}
	text := "CRYOBACKUP\n\nTest message\n\nThis is a test message from the CryoBackup console, sent " + page.When + ".\n" +
		"It shows that the console can deliver e-mail to this address through its relay. Nothing needs to be done.\n\n"
	if page.ConsoleURL != "" {
		text += "Open the console: " + page.ConsoleURL + "\n"
	}
	text += "You receive this because an administrator of the console asked for a test message to this address.\n"
	var html bytes.Buffer
	var inline []mail.Inline
	if err := testMailHTML.Execute(&html, page); err != nil {
		html.Reset() // the text part alone is a complete message
	} else {
		inline = markInline()
	}
	return mail.Message{To: []string{addr}, Subject: "[CryoBackup] Test message", Text: text, HTML: html.String(), Kind: mail.KindTest, Inline: inline}
}
