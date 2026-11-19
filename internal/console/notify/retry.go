package notify

import (
	"context"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// RetryBatch bounds the rows one retry run reads.
const RetryBatch = 100

// Suppress reasons the retry job records. The vocabulary is the schema's (notification_log CHECK,
// migration 0001), which has no word for "the alarm is over" or "the recipient is gone": a message
// whose content no longer applies is recorded as deduped (what it said is said by another row, a
// recovery notice, or nobody wants it any more).
const (
	reasonDeduped = "deduped"
)

// RerenderFunc rebuilds, for a retry, a message of a kind this package does not render (the
// digest's), as it should read now, addressed to the row's recipient. A non-empty suppress reason
// (the schema's vocabulary) records the decision not to send instead; an error is a store error.
type RerenderFunc func(ctx context.Context, d store.Delivery, to store.Recipient) (m mail.Message, suppress string, err error)

// Retrier is the work of the notification-retry job (ADR 0020 decision 9): every minute it takes the
// delivery rows that are due (failed with a retry time that has come, or left queued by a sender
// that stopped) and sends each again, re-rendered from the state as it is now. An alert whose
// episodes have all closed, or are now acknowledged or of a suspended customer, is not sent: the
// row is suppressed. The schedule (three retries, at 1, 5 and 15 minutes) is the store's and the
// dispatcher's.
type Retrier struct {
	Store    *store.Store
	Dispatch *Dispatcher
	// View evaluates the fleet as it is now (health.Evaluator.Alarms). It runs only in a run that has
	// an alert or recovery row to re-render.
	View func(ctx context.Context) (state.FleetView, error)
	// Loc returns the display zone; nil means UTC. ConsoleURL is the link of a message.
	Loc        func() *time.Location
	ConsoleURL string
	// SendBudget bounds the sends of one run; zero means DefaultSendBudget.
	SendBudget time.Duration
	// Rerender renders a retry of another kind, by store delivery kind. A due row of a kind with
	// neither a renderer here nor an entry is left alone (and logged): its owner retries it.
	Rerender map[string]RerenderFunc
	Log      *logx.Logger
}

// RetryResult counts what one run did.
type RetryResult struct {
	Due, Sent, Failed, Suppressed, Settled, Left, Deferred int
}

func (r *Retrier) log() *logx.Logger {
	if r.Log != nil {
		return r.Log
	}
	return logx.Default()
}

// retryView loads the fleet view at most once per run.
type retryView struct {
	load func(ctx context.Context) (state.FleetView, error)
	v    *state.FleetView
	m    *mutes
}

func (rv *retryView) get(ctx context.Context, st *store.Store) (state.FleetView, mutes, error) {
	if rv.v == nil {
		if rv.load == nil {
			return state.FleetView{}, mutes{}, errors.New("notify: the retry job has no view")
		}
		v, err := rv.load(ctx)
		if err != nil {
			return state.FleetView{}, mutes{}, err
		}
		m, err := loadMutes(ctx, st, v)
		if err != nil {
			return state.FleetView{}, mutes{}, err
		}
		rv.v, rv.m = &v, &m
	}
	return *rv.v, *rv.m, nil
}

// Run is one retry run. The error is a store (or view) error; send failures are in the log rows.
func (r *Retrier) Run(ctx context.Context) (RetryResult, error) {
	var res RetryResult
	due, err := r.Store.ListDueRetries(ctx, store.ScopeAll(), r.Store.Now(), RetryBatch)
	if err != nil || len(due) == 0 {
		return res, err
	}
	res.Due = len(due)
	rv := &retryView{load: r.View}
	o := Options{ConsoleURL: r.ConsoleURL}
	if r.Loc != nil {
		o.Loc = r.Loc()
	}
	budget := r.SendBudget
	if budget <= 0 {
		budget = DefaultSendBudget
	}
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var first error
	keep := func(err error) {
		if first == nil {
			first = err
		}
	}
	for _, d := range due {
		if spent(sctx) {
			res.Deferred++
			continue
		}
		m, suppress, err := r.rerender(ctx, d, rv, o)
		if err != nil {
			keep(err)
			continue
		}
		var out Result
		switch {
		case suppress != "":
			out, err = r.Dispatch.Suppress(ctx, d, suppress)
			if err == nil && !out.Settled {
				res.Suppressed++
			}
		case m == nil:
			res.Left++
			continue
		default:
			out, err = r.Dispatch.Deliver(sctx, d, *m)
			switch {
			case err != nil:
			case out.Sent:
				res.Sent++
			case out.Err != nil:
				res.Failed++
			}
		}
		if err != nil {
			keep(err)
		} else if out.Settled {
			res.Settled++
		}
	}
	if res.Left > 0 {
		r.log().Warn("notifications due for retry have no renderer here", logx.Count("left", res.Left))
	}
	if res.Due == res.Left {
		return res, first
	}
	r.log().Info("notification retry", logx.Count("due", res.Due), logx.Count("sent", res.Sent), logx.Count("failed", res.Failed),
		logx.Count("suppressed", res.Suppressed), logx.Count("settled", res.Settled), logx.Count("deferred", res.Deferred))
	return res, first
}

// rerender is the message a due row should carry now, or a suppress reason, or (nil, "") for a row
// this retrier leaves alone.
func (r *Retrier) rerender(ctx context.Context, d store.Delivery, rv *retryView, o Options) (*mail.Message, string, error) {
	switch d.Kind {
	case store.DeliveryAlert, store.DeliveryRecovery:
	case store.DeliveryTest:
		return nil, reasonDeduped, nil // a test is answered on the page at once, never retried in the background
	default:
		f := r.Rerender[d.Kind]
		if f == nil {
			return nil, "", nil
		}
		to, reason, err := r.recipient(ctx, d)
		if err != nil || reason != "" {
			return nil, reason, err
		}
		m, suppress, err := f(ctx, d, to)
		if err != nil || suppress != "" {
			return nil, suppress, err
		}
		return &m, "", nil
	}
	prefix := keyAlert
	if d.Kind == store.DeliveryRecovery {
		prefix = keyRecovery
	}
	at, _, err := parseBatchKey(d.DedupeKey, prefix)
	if err != nil {
		r.log().Warn("notification not retried: its dedupe key is not a batch key", logx.ID("delivery", d.ID), logx.Kind(d.Kind))
		return nil, reasonDeduped, nil
	}
	to, reason, err := r.recipient(ctx, d)
	if err != nil || reason != "" {
		return nil, reason, err
	}
	if d.Kind == store.DeliveryAlert && !to.Alerts || d.Kind == store.DeliveryRecovery && !to.Recoveries {
		return nil, reasonDeduped, nil // the recipient switched it off
	}
	var eps []store.Episode
	if d.Kind == store.DeliveryAlert {
		eps, err = r.Store.ListEpisodesNotifiedAt(ctx, store.ScopeAll(), at)
	} else {
		eps, err = r.Store.ListEpisodesRecoveryNotifiedAt(ctx, store.ScopeAll(), at)
	}
	if err != nil {
		return nil, "", err
	}
	if len(eps) == 0 {
		return nil, reasonDeduped, nil // an orphan of a pass that stopped before it marked: the next pass mailed them
	}
	v, m, err := rv.get(ctx, r.Store)
	if err != nil {
		return nil, "", err
	}
	var c Content
	if d.Kind == store.DeliveryAlert {
		var still []store.Episode
		reason := reasonDeduped // every episode closed: the alarm is over, a recovery notice follows
		for _, e := range eps {
			if !e.Open() {
				continue
			}
			if why := m.reason(e); why != "" {
				reason = why
				continue
			}
			still = append(still, e)
		}
		if len(still) == 0 {
			return nil, reason, nil
		}
		c, err = RenderAlert(v, still, o)
	} else {
		c, err = RenderRecovery(v, eps, o)
	}
	if err != nil {
		return nil, "", err
	}
	msg := c.Message(to.Address)
	return &msg, "", nil
}

// recipient is the row's recipient, or a suppress reason when it no longer exists.
func (r *Retrier) recipient(ctx context.Context, d store.Delivery) (store.Recipient, string, error) {
	if d.RuleID == "" {
		return store.Recipient{}, reasonDeduped, nil // the rule was deleted: the log outlives it
	}
	to, err := r.Store.GetRecipient(ctx, store.ScopeAll(), d.RuleID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Recipient{}, reasonDeduped, nil
	case err != nil:
		return store.Recipient{}, "", err
	}
	return to, "", nil
}
