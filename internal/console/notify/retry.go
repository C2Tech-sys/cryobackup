package notify

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// RetryBatch bounds the rows one retry run reads.
const RetryBatch = 100

// Suppress reasons this package records: the schema's vocabulary (the CHECK on
// notification_log.suppress_reason, migration 0001: acknowledged, snoozed, paused, deduped,
// customer_suspended, below_threshold), the best fitting word for each case. The schema has no word
// for some cases, so the "notification suppressed" log line also names the precise case (why, a
// class word):
//
//	case                                         reason            why
//	orphan of a crashed pass, overlapping pass   deduped           orphan, overlap, batch_id_taken
//	every alarm of the alert already resolved    below_threshold   alarm_resolved
//	recipient deleted                            deduped           recipient_deleted
//	recipient switched the kind off              deduped           recipient_switched_off
//	dedupe key that is not a batch key           deduped           bad_key
//	a test send left queued                      deduped           test_not_retried
//	the whole batch muted while the pass ran     deduped           muted_mid_pass
//
// "paused" is not used: the rest of the product means the PAUSED device state by it.
const (
	reasonDeduped  = "deduped"
	reasonResolved = "below_threshold" // the alarm no longer meets the alerting condition
)

// skip is a decision not to send a due row: the schema's reason and the precise case for the log.
type skip struct{ reason, why string }

// RerenderFunc rebuilds, for a retry, a message of a kind this package does not render (the
// digest's), as it should read now, addressed to the row's recipient. A non-empty suppress reason
// (the schema's vocabulary) records the decision not to send instead; an error is a store error.
type RerenderFunc func(ctx context.Context, d store.Delivery, to store.Recipient) (m mail.Message, suppress string, err error)

// Retrier is the work of the notification-retry job (ADR 0020 decision 9): every minute it takes the
// delivery rows that are due (failed with a retry time that has come, or left queued by a sender
// that stopped) and sends each again, re-rendered from the state as it is now. A retried alert is
// the original notification, delayed: it carries every episode of its batch that is still open,
// acknowledged, snoozed or suspended since or not; an alert whose episodes have all closed is not
// sent: the row is suppressed. The schedule (three retries, at 1, 5 and 15 minutes) is the store's and the
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
	// neither a renderer here nor an entry is not read (kinds): its owner retries it.
	Rerender map[string]RerenderFunc
	Log      *logx.Logger

	// afterSettle, when set, runs after a re-render settled the recovery of closed episodes and
	// before the send (the tests inject a crash there).
	afterSettle func() error
}

// RetryResult counts what one run did.
type RetryResult struct {
	Due, Sent, Failed, Suppressed, Settled, Left, Deferred int
	// RecoveryUnsent counts the closed episodes whose recovery a re-rendered alert settled without a
	// message: the retry left them out and no alert of their batch had been delivered.
	RecoveryUnsent int
}

// batchID is a free batch id from the process-wide source (shared with every Notifier).
func (r *Retrier) batchID(ctx context.Context) (int64, error) {
	return processBatchIDs.next(ctx, r.Store)
}

func (r *Retrier) log() *logx.Logger {
	if r.Log != nil {
		return r.Log
	}
	return logx.Default()
}

// kinds are the delivery kinds this retrier decides about: alert, recovery and test here, and every
// kind of Rerender. Only their due rows are read, so rows of another kind (whose owner retries
// them) cannot fill the window of RetryBatch rows and starve these.
func (r *Retrier) kinds() []string {
	ks := []string{store.DeliveryAlert, store.DeliveryRecovery, store.DeliveryTest}
	for k, f := range r.Rerender {
		if f != nil && k != store.DeliveryAlert && k != store.DeliveryRecovery && k != store.DeliveryTest {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

// retryView loads the fleet view at most once per run.
type retryView struct {
	load func(ctx context.Context) (state.FleetView, error)
	v    *state.FleetView
}

func (rv *retryView) get(ctx context.Context) (state.FleetView, error) {
	if rv.v == nil {
		if rv.load == nil {
			return state.FleetView{}, errors.New("notify: the retry job has no view")
		}
		v, err := rv.load(ctx)
		if err != nil {
			return state.FleetView{}, err
		}
		rv.v = &v
	}
	return *rv.v, nil
}

// Run is one retry run. The error is a store (or view) error; send failures are in the log rows.
func (r *Retrier) Run(ctx context.Context) (RetryResult, error) {
	var res RetryResult
	due, err := r.Store.ListDueRetriesOfKinds(ctx, store.ScopeAll(), r.Store.Now(), r.kinds(), RetryBatch)
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
	linkLogged := false
	for _, d := range due {
		if spent(sctx) {
			res.Deferred++
			continue
		}
		m, sk, err := r.rerender(ctx, d, rv, o, &res)
		if err != nil {
			keep(err)
			continue
		}
		var out Result
		switch {
		case sk.reason != "":
			out, err = r.Dispatch.suppress(ctx, d, sk.reason, sk.why)
			if err == nil && !out.Settled {
				res.Suppressed++
			}
		case m == nil:
			res.Left++
			continue
		default:
			if !linkLogged && (d.Kind == store.DeliveryAlert || d.Kind == store.DeliveryRecovery) && consoleLinkDropped(r.ConsoleURL) {
				linkLogged = true
				logConsoleLink(r.log(), "notification_retry") // once per run; the messages go without the link
			}
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
		logx.Count("suppressed", res.Suppressed), logx.Count("settled", res.Settled), logx.Count("deferred", res.Deferred),
		logx.Count("recovery_unsent", res.RecoveryUnsent))
	return res, first
}

// rerender is the message a due row should carry now, or a decision not to send it, or neither
// (nil and no reason) for a row this retrier leaves alone.
func (r *Retrier) rerender(ctx context.Context, d store.Delivery, rv *retryView, o Options, res *RetryResult) (*mail.Message, skip, error) {
	switch d.Kind {
	case store.DeliveryAlert, store.DeliveryRecovery:
	case store.DeliveryTest:
		return nil, skip{reasonDeduped, "test_not_retried"}, nil // a test is answered on the page at once, never retried in the background
	default:
		f := r.Rerender[d.Kind]
		if f == nil {
			return nil, skip{}, nil
		}
		to, sk, err := r.recipient(ctx, d)
		if err != nil || sk.reason != "" {
			return nil, sk, err
		}
		m, suppress, err := f(ctx, d, to)
		if err != nil || suppress != "" {
			return nil, skip{suppress, suppress}, err
		}
		return &m, skip{}, nil
	}
	prefix := keyAlert
	if d.Kind == store.DeliveryRecovery {
		prefix = keyRecovery
	}
	at, _, err := parseBatchKey(d.DedupeKey, prefix)
	if err != nil {
		r.log().Warn("notification not retried: its dedupe key is not a batch key", logx.ID("delivery", d.ID), logx.Kind(d.Kind))
		return nil, skip{reasonDeduped, "bad_key"}, nil
	}
	to, sk, err := r.recipient(ctx, d)
	if err != nil || sk.reason != "" {
		return nil, sk, err
	}
	if d.Kind == store.DeliveryAlert && !to.Alerts || d.Kind == store.DeliveryRecovery && !to.Recoveries {
		return nil, skip{reasonDeduped, "recipient_switched_off"}, nil
	}
	var eps []store.Episode
	if d.Kind == store.DeliveryAlert {
		eps, err = r.Store.ListEpisodesNotifiedAt(ctx, store.ScopeAll(), at)
	} else {
		eps, err = r.Store.ListEpisodesRecoveryNotifiedAt(ctx, store.ScopeAll(), at)
	}
	if err != nil {
		return nil, skip{}, err
	}
	if len(eps) == 0 {
		return nil, skip{reasonDeduped, "orphan"}, nil // an orphan of a pass that stopped before it marked: the next pass mailed them
	}
	v, err := rv.get(ctx)
	if err != nil {
		return nil, skip{}, err
	}
	var c Content
	if d.Kind == store.DeliveryAlert {
		// The retried alert is the original notification, delayed: it carries every episode of its
		// batch that is still open, whether or not it has been acknowledged, snoozed or its customer
		// suspended since (the mute rules form batches; they never thin a retry). Only a closed episode
		// is left out, and its recovery is settled unless another row of the batch may carry it.
		var open, closed []store.Episode
		for _, e := range eps {
			switch {
			case e.Open():
				open = append(open, e)
			case e.RecoveryNotifiedAt == nil:
				closed = append(closed, e)
			}
		}
		if err := r.settleClosed(ctx, at, d.ID, closed, res); err != nil {
			return nil, skip{}, err
		}
		if len(open) == 0 {
			return nil, skip{reasonResolved, "alarm_resolved"}, nil // every alarm of the alert is over
		}
		c, err = RenderAlert(v, open, o)
	} else {
		c, err = RenderRecovery(v, eps, o)
	}
	if err != nil {
		return nil, skip{}, err
	}
	msg := c.Message(to.Address)
	return &msg, skip{}, nil
}

// settleClosed settles, without a message, the recovery of the closed episodes the retried alert
// row retried of batch at leaves out (store.SettleClosedLeftOut, one transaction with the check of
// the batch's rows): unless another row of the batch is sent, or queued and never attempted (in
// flight from the pass with them in it, left by a pass that stopped before sending, or deferred by
// its send budget: the residual class of the package doc). It runs before the send; the mark takes
// a free batch id, like Notifier's silent marks.
func (r *Retrier) settleClosed(ctx context.Context, at time.Time, retried string, closed []store.Episode, res *RetryResult) error {
	if len(closed) == 0 {
		return nil
	}
	id, err := r.batchID(ctx)
	if err != nil {
		return err
	}
	n, err := r.Store.SettleClosedLeftOut(ctx, store.ScopeAll(), episodeIDs(closed), at, time.UnixMilli(id).UTC(), retried)
	if err != nil {
		return err
	}
	res.RecoveryUnsent += n
	if r.afterSettle != nil {
		return r.afterSettle()
	}
	return nil
}

// recipient is the row's recipient, or a decision not to send when it no longer exists.
func (r *Retrier) recipient(ctx context.Context, d store.Delivery) (store.Recipient, skip, error) {
	gone := skip{reasonDeduped, "recipient_deleted"}
	if d.RuleID == "" {
		return store.Recipient{}, gone, nil // the rule was deleted: the log outlives it
	}
	to, err := r.Store.GetRecipient(ctx, store.ScopeAll(), d.RuleID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Recipient{}, gone, nil
	case err != nil:
		return store.Recipient{}, skip{}, err
	}
	return to, skip{}, nil
}
