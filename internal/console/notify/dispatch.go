package notify

import (
	"context"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// DefaultSendTimeout bounds one send when Dispatcher.SendTimeout is zero. It is shorter than the
// sender's own two-minute ceiling so that an evaluator pass (every minute) is never held by a
// relay that stopped answering.
const DefaultSendTimeout = 30 * time.Second

// Dispatcher owns one delivery end to end: the delivery-log row, the send and the row's outcome.
// It is the only code that calls Sender.Send for the console's operator mail, so every message
// is in the log (ADR 0020 decision 9) and every failure feeds the consecutive-failure counter of
// banner C03.
type Dispatcher struct {
	Store  *store.Store
	Sender mail.Sender
	// From is the sender address put on a message that has none (config smtp.from). The SMTP sender
	// has the same default; a FakeSender has none, so fixtures mode sets one here.
	From string
	// SendTimeout bounds one send; zero means DefaultSendTimeout.
	SendTimeout time.Duration
	// Log receives one line per failed or refused delivery (ids, kind, class); nil means logx.Default().
	Log *logx.Logger
}

// Result is the outcome of one delivery.
type Result struct {
	// Delivery is the log row as it is after this call.
	Delivery store.Delivery
	// Sent is true when this call delivered the message.
	Sent bool
	// Err is the failure of this attempt; nil when it was delivered or not attempted.
	Err *mail.SendError
	// Settled is true when the row was already decided by someone else: Send found it existing, or the
	// mark answered that it is final. Nothing was sent by this call and nothing is wrong.
	Settled bool
}

func (d *Dispatcher) log() *logx.Logger {
	if d.Log != nil {
		return d.Log
	}
	return logx.Default()
}

// Enqueue records the message as queued (store.EnqueueDelivery). created is false when a row with
// the same dedupe key exists: the message was already decided and must not be sent again.
func (d *Dispatcher) Enqueue(ctx context.Context, in store.DeliveryInput) (store.Delivery, bool, error) {
	return d.Store.EnqueueDelivery(ctx, store.ScopeAll(), in)
}

// Send is Enqueue then Deliver: the whole life of a new message (the digest sends this way). When
// the dedupe key already has a row, nothing is sent and the Result is Settled. The error is a store
// error only; a send failure is in Result.Err and in the log row.
func (d *Dispatcher) Send(ctx context.Context, in store.DeliveryInput, m mail.Message) (Result, error) {
	row, created, err := d.Enqueue(ctx, in)
	if err != nil {
		return Result{}, err
	}
	if !created {
		return Result{Delivery: row, Settled: true}, nil
	}
	return d.Deliver(ctx, row, m)
}

// Deliver sends m for the existing row (queued, or failed with a retry due) and records the
// outcome: MarkDeliverySent, or MarkDeliveryFailed with the class word of the failure and the next
// retry time of store.DeliveryRetryDelay when the failure is temporary and the schedule has one
// left (none for a permanent failure: the row is then final). A row the store calls final, by
// this call's mark or before it, is Settled: someone else finished it. The send runs under its own
// deadline (SendTimeout); the mark runs even when ctx has ended, so a message that went out is
// never recorded as unsent and mailed twice. The error is a store error only.
func (d *Dispatcher) Deliver(ctx context.Context, row store.Delivery, m mail.Message) (Result, error) {
	if row.Result != store.DeliveryQueued && !(row.Result == store.DeliveryFailed && row.NextRetryAt != nil) {
		return Result{Delivery: row, Settled: true}, nil
	}
	if m.Kind == "" {
		m.Kind = mail.Kind(row.Kind)
	}
	if m.From == "" {
		m.From = d.From
	}
	timeout := d.SendTimeout
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	serr := d.Sender.Send(sctx, m)
	failure := sendFailure(sctx, serr)
	cancel()
	mctx := context.WithoutCancel(ctx)
	// This attempt's number: 1 for a queued row, one more than the row's for a retry of a failed one.
	attempt := row.Attempt
	if row.Result == store.DeliveryFailed {
		attempt++
	}
	if failure == nil {
		upd, err := d.Store.MarkDeliverySent(mctx, store.ScopeAll(), row.ID)
		switch {
		case errors.Is(err, store.ErrConflict):
			return Result{Delivery: row, Settled: true}, nil
		case err != nil:
			d.log().Error("notification sent but not recorded", logx.ID("delivery", row.ID), logx.Kind(row.Kind), logx.Class("store"))
			return Result{Delivery: row, Sent: true}, err
		}
		return Result{Delivery: upd, Sent: true}, nil
	}
	var next *time.Time
	if failure.Temporary {
		if delay, ok := store.DeliveryRetryDelay(attempt); ok {
			t := d.Store.Now().Add(delay)
			next = &t
		}
	}
	upd, err := d.Store.MarkDeliveryFailed(mctx, store.ScopeAll(), row.ID, string(failure.Class), next)
	switch {
	case errors.Is(err, store.ErrConflict):
		return Result{Delivery: row, Settled: true, Err: failure}, nil
	case err != nil:
		return Result{Delivery: row, Err: failure}, err
	}
	d.log().Warn("notification not delivered", logx.ID("delivery", row.ID), logx.Kind(row.Kind), logx.Str("stage", string(failure.Stage)),
		logx.Class(string(failure.Class)), logx.Int("attempt", attempt), logx.Bool("retry", upd.NextRetryAt != nil))
	return Result{Delivery: upd, Err: failure}, nil
}

// Suppress records the decision not to send a waiting row (store.SuppressDelivery). A row that is
// already final is Settled.
func (d *Dispatcher) Suppress(ctx context.Context, row store.Delivery, reason string) (Result, error) {
	upd, err := d.Store.SuppressDelivery(ctx, store.ScopeAll(), row.ID, reason)
	switch {
	case errors.Is(err, store.ErrConflict):
		return Result{Delivery: row, Settled: true}, nil
	case err != nil:
		return Result{Delivery: row}, err
	}
	d.log().Info("notification suppressed", logx.ID("delivery", row.ID), logx.Kind(row.Kind), logx.Str("reason", reason))
	return Result{Delivery: upd}, nil
}

// sendFailure turns what Send returned into a *mail.SendError, nil for a success. Every error of a
// mail.Sender should already be one; another error (a sender that broke the contract) is classed
// by the deadline it ran under, so its text never reaches the log row.
func sendFailure(ctx context.Context, err error) *mail.SendError {
	if err == nil {
		return nil
	}
	var se *mail.SendError
	if errors.As(err, &se) {
		return se
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassCanceled, Temporary: true}
	}
	return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassNetwork, Temporary: true}
}
