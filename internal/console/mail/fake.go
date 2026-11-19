package mail

import (
	"context"
	"sync"
	"time"
)

// FakeSender is a Sender for tests and fixtures mode. It records every message
// it accepts, validates each one the way the real sender does (so a template
// that would put a line break in a header fails in a test, not in production),
// and fails on demand with a given SendError. It sends nothing anywhere.
type FakeSender struct {
	mu     sync.Mutex
	sent   []Message
	calls  int
	fail   *SendError
	failN  int // failures still to deliver; -1 means every call
	failFn func(Message) *SendError
}

// NewFakeSender returns a FakeSender that accepts everything.
func NewFakeSender() *FakeSender { return &FakeSender{} }

// Send records m, or fails as told. The default From is not filled in: a fake
// has no settings. A message that cannot be built fails with ClassMessageInvalid.
func (f *FakeSender) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failFn != nil {
		if e := f.failFn(m); e != nil {
			c := *e
			return &c
		}
	}
	if f.fail != nil && f.failN != 0 {
		if f.failN > 0 {
			f.failN--
		}
		e := *f.fail
		return &e
	}
	if _, err := m.Build(time.Unix(0, 0)); err != nil {
		return newErr(StageConfig, ClassMessageInvalid, false)
	}
	m.To = append([]string(nil), m.To...)
	f.sent = append(f.sent, m)
	return nil
}

// FailWith makes every following Send fail with e until FailWith(nil).
func (f *FakeSender) FailWith(e *SendError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail, f.failN, f.failFn = e, -1, nil
}

// FailNext makes the next n Sends fail with e, then the fake accepts again.
func (f *FakeSender) FailNext(n int, e *SendError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail, f.failN, f.failFn = e, n, nil
}

// FailWhen makes a Send fail with the error fn returns for its message (nil:
// accept), for a test that fails one recipient and not another.
func (f *FakeSender) FailWhen(fn func(Message) *SendError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail, f.failN, f.failFn = nil, 0, fn
}

// Sent returns a copy of the messages accepted so far, oldest first.
func (f *FakeSender) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}

// Calls is the number of Send calls, accepted or not.
func (f *FakeSender) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Reset forgets the recorded messages, the call count and any failure.
func (f *FakeSender) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent, f.calls, f.fail, f.failN, f.failFn = nil, 0, nil, 0, nil
}

var (
	_ Sender = (*SMTPSender)(nil)
	_ Sender = (*FakeSender)(nil)
)
