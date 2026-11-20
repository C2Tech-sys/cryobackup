package notify

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// keyDigestForced is the dedupe-key prefix of a forced "send now": "digest-forced:<id>:<rule id>".
// It can never equal the daily key ("digest:<local date>:<rule id>", state.DigestKey), so a forced
// send neither blocks nor counts as the day's digest.
const keyDigestForced = "digest-forced"

// ErrDigestTime is what the daily-digest job returns when digest_local_time is not a usable HH:MM:
// no due time can be computed. Banner C14b says the same on every page.
var ErrDigestTime = errors.New("notify: digest_local_time is not a usable time")

// Why a digest run or a send-now sent nothing (DigestResult.Skipped).
const (
	DigestSkippedDisabled      = "disabled"       // the operator turned the digest off: silent by choice
	DigestSkippedNotConfigured = "not_configured" // no relay: banner C14b ("not set up") says so
	DigestSkippedNoRecipient   = "no_recipient"   // nobody has the digest switch: banner C20 says so
	DigestSkippedNotDue        = "not_due"        // today's time has not come yet (yesterday's is never sent late)
	DigestSkippedAlreadySent   = "already_sent"   // every recipient's row for today exists
)

// Per-recipient outcomes of a digest run or send-now (DigestRecipient.Outcome).
const (
	DigestSent          = "sent"           // delivered by this call
	DigestFailed        = "failed"         // this call's attempt failed (retried by notification-retry when temporary)
	DigestAlreadySent   = "already_sent"   // today's digest was delivered to this recipient before
	DigestInRetry       = "in_retry"       // today's digest is queued or waiting for its retry
	DigestGaveUp        = "gave_up"        // today's digest failed for good or was suppressed (a forced send can still go)
	DigestRecipientGone = "recipient_gone" // deleted while the call ran
	DigestDeferred      = "deferred"       // the send budget ran out; the next run sends it
)

// DigestRecipient is what became of one recipient's digest.
type DigestRecipient struct {
	RuleID  string
	Outcome string
}

// DigestResult is what a digest run or a send-now did.
type DigestResult struct {
	// Date is the local date of the digest in the display zone ("2006-01-02"); empty when the call
	// stopped before it knew one.
	Date string
	// Skipped says why nothing was attempted; empty when the recipients were looked at.
	Skipped string
	// Forced is true for a forced send-now.
	Forced bool
	// Recipients lists each recipient with the digest switch and its outcome, in name order.
	Recipients []DigestRecipient
	// Sent and Failed count this call's attempts.
	Sent, Failed int
}

func (r *DigestResult) add(ruleID, outcome string) {
	r.Recipients = append(r.Recipients, DigestRecipient{RuleID: ruleID, Outcome: outcome})
	switch outcome {
	case DigestSent:
		r.Sent++
	case DigestFailed:
		r.Failed++
	}
}

// Digester renders and sends the daily digest (ADR 0020 decision 8). It is the work of the
// daily-digest job (Run), the console's "send now" and "preview" (SendDigestNow, PreviewDigest),
// and the re-render of a digest the retry job sends again (Rerender, a RerenderFunc).
//
// ONE DIGEST PER LOCAL DATE PER RECIPIENT. Each recipient's daily digest is one delivery row
// whose dedupe key is state.DigestKey: "digest:<local date>:<rule id>", the local date of the
// day's due time in the display zone. The store keeps one row per key whatever becomes of it, and
// Dispatcher.Send sends nothing for a key that has a row, so the date can be reached any number of
// times (every minute, after a restart, on a daylight-saving day whose due time is skipped or
// repeated, after the digest time or the zone was changed during the day) and still names one
// message. The job only ever sends for TODAY: when the newest due time (state.DigestDue) falls on
// an earlier local date, today's time has not come, and yesterday's digest is never sent late.
// A console that was up at or after the day's time sends that day's digest in the first run that
// sees it due (catch-up after a stop included). A retry of one recipient's row (notification-retry,
// Rerender) never touches another recipient's row.
type Digester struct {
	Store    *store.Store
	Dispatch *Dispatcher
	// View evaluates the fleet as it is now (health.Evaluator.Alarms). A run loads it only when a
	// digest is to be rendered.
	View func(ctx context.Context) (state.FleetView, error)
	// Configured reports whether a relay is configured (mail.Settings.Configured). Without one
	// nothing is enqueued; banner C14b says the digest is not set up.
	Configured bool
	// Loc returns the operator's display zone; nil means UTC. The digest time is local to it.
	Loc func() *time.Location
	// ConsoleURL is the console's public URL for the link in the digest; empty means no link.
	ConsoleURL string
	// SendBudget bounds the sends of one call; zero means DefaultSendBudget. A recipient not
	// attempted inside it gets no row, so the next run sends it.
	SendBudget time.Duration
	// Log receives one line per call that did something (counts only); nil means logx.Default().
	Log *logx.Logger
}

func (g *Digester) log() *logx.Logger {
	if g.Log != nil {
		return g.Log
	}
	return logx.Default()
}

func (g *Digester) loc() *time.Location {
	if g.Loc != nil {
		if l := g.Loc(); l != nil {
			return l
		}
	}
	return time.UTC
}

// render is the digest as the fleet is now, for the local date date ("2006-01-02"; empty: the
// date of the view).
func (g *Digester) render(ctx context.Context, date string) (Content, error) {
	if g.View == nil {
		return Content{}, errors.New("notify: the digest has no view")
	}
	v, err := g.View(ctx)
	if err != nil {
		return Content{}, err
	}
	acks, err := g.Store.ListActiveAcks(ctx, store.ScopeAll())
	if err != nil {
		return Content{}, err
	}
	return RenderDigest(v, acks, Options{Loc: g.loc(), ConsoleURL: g.ConsoleURL, DigestDate: date})
}

// PreviewDigest renders the digest as it would go out now, for the console's preview. Nothing is
// written and nothing is sent; it works whether or not mail is set up or the digest is on.
func (g *Digester) PreviewDigest(ctx context.Context) (Content, error) {
	return g.render(ctx, "")
}

// digestRecipients are the recipients with the digest switch, by name.
func (g *Digester) digestRecipients(ctx context.Context) ([]store.Recipient, error) {
	all, err := g.Store.ListRecipients(ctx, store.ScopeAll())
	if err != nil {
		return nil, err
	}
	var out []store.Recipient
	for _, r := range all {
		if r.Digest {
			out = append(out, r)
		}
	}
	return out, nil
}

// existing returns the delivery row of key, or nil when there is none.
func (g *Digester) existing(ctx context.Context, key string) (*store.Delivery, error) {
	rows, err := g.Store.ListDeliveriesByKeyPrefix(ctx, store.ScopeAll(), key)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].DedupeKey == key {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// outcomeOfRow says what an existing row of today's digest means for its recipient.
func outcomeOfRow(d store.Delivery) string {
	switch {
	case d.Result == store.DeliverySent:
		return DigestAlreadySent
	case d.Result == store.DeliveryQueued, d.Result == store.DeliveryFailed && d.NextRetryAt != nil:
		return DigestInRetry
	}
	return DigestGaveUp
}

// Run is one pass of the daily-digest job: when the digest is on, mail is set up and today's digest
// is due in the display zone, it sends it to every recipient with the digest switch that has no row
// for today yet. The error is a store or view error (the run failed), or ErrDigestTime; a send
// failure is in the delivery log and the recipient's outcome.
func (g *Digester) Run(ctx context.Context) (DigestResult, error) {
	var res DigestResult
	set, err := g.Store.ReadDigestSettings(ctx, store.ScopeAll())
	if err != nil {
		return res, err
	}
	switch {
	case !set.Enabled:
		res.Skipped = DigestSkippedDisabled
		return res, nil
	case !g.Configured:
		res.Skipped = DigestSkippedNotConfigured
		return res, nil
	}
	loc, now := g.loc(), g.Store.Now()
	due, ok := state.DigestDue(set.LocalTime, loc, now)
	if !ok {
		return res, ErrDigestTime
	}
	today := now.In(loc).Format("2006-01-02")
	if due.In(loc).Format("2006-01-02") != today {
		res.Skipped = DigestSkippedNotDue
		return res, nil
	}
	res.Date = today
	to, err := g.digestRecipients(ctx)
	if err != nil {
		return res, err
	}
	if len(to) == 0 {
		res.Skipped = DigestSkippedNoRecipient
		return res, nil
	}
	keys := make(map[string]string, len(to))
	var missing []store.Recipient
	for _, r := range to {
		keys[r.ID] = state.DigestKey(due, loc, r.ID)
		if row, err := g.existing(ctx, keys[r.ID]); err != nil {
			return res, err
		} else if row == nil {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		res.Skipped = DigestSkippedAlreadySent // the common case all day: no view is loaded
		return res, nil
	}
	err = g.send(ctx, missing, func(r store.Recipient) string { return keys[r.ID] }, today, &res)
	g.logResult("daily digest", res)
	return res, err
}

// SendDigestNow sends today's digest at once, outside the schedule, to every recipient with the
// digest switch. It is the console's "send now" (ADR 0020 decision 8): an explicit request, so it
// does not look at the digest's on/off switch or time.
//
//   - Not forced, it sends today's digest under today's key (state.DigestKey of now's local date):
//     it is today's digest, and the job does not send another. A recipient whose row for today
//     exists gets nothing and the outcome says why (already_sent, in_retry or gave_up).
//   - Forced, it sends to every recipient under a key of its own ("digest-forced:<id>:<rule id>"),
//     which never collides with the daily key and does not count as today's digest: the job still
//     sends the day's digest at its time.
//
// Without a relay nothing is attempted (Skipped not_configured); with no recipient, Skipped
// no_recipient. The error is a store or view error.
func (g *Digester) SendDigestNow(ctx context.Context, force bool) (DigestResult, error) {
	res := DigestResult{Forced: force}
	if !g.Configured {
		res.Skipped = DigestSkippedNotConfigured
		return res, nil
	}
	loc, now := g.loc(), g.Store.Now()
	res.Date = now.In(loc).Format("2006-01-02")
	to, err := g.digestRecipients(ctx)
	if err != nil {
		return res, err
	}
	if len(to) == 0 {
		res.Skipped = DigestSkippedNoRecipient
		return res, nil
	}
	var keyOf func(store.Recipient) string
	var send []store.Recipient
	if force {
		id, err := processBatchIDs.next(ctx, g.Store)
		if err != nil {
			return res, err
		}
		ms := strconv.FormatInt(id, 10)
		keyOf = func(r store.Recipient) string { return keyDigestForced + ":" + ms + ":" + r.ID }
		send = to
	} else {
		keyOf = func(r store.Recipient) string { return state.DigestKey(now, loc, r.ID) }
		for _, r := range to {
			row, err := g.existing(ctx, keyOf(r))
			switch {
			case err != nil:
				return res, err
			case row != nil:
				res.add(r.ID, outcomeOfRow(*row))
			default:
				send = append(send, r)
			}
		}
		if len(send) == 0 {
			g.logResult("digest send-now", res)
			return res, nil
		}
	}
	err = g.send(ctx, send, keyOf, res.Date, &res)
	g.logResult("digest send-now", res)
	return res, err
}

// send renders the digest once and sends it to each recipient under its key, under the send
// budget. One recipient failing does not stop the others; a temporary failure is retried alone by
// notification-retry (Rerender).
func (g *Digester) send(ctx context.Context, to []store.Recipient, keyOf func(store.Recipient) string, date string, res *DigestResult) error {
	content, err := g.render(ctx, date)
	if err != nil {
		return err
	}
	budget := g.SendBudget
	if budget <= 0 {
		budget = DefaultSendBudget
	}
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var first error
	for _, r := range to {
		if spent(sctx) {
			res.add(r.ID, DigestDeferred)
			continue
		}
		out, err := g.Dispatch.Send(sctx, store.DeliveryInput{Kind: store.DeliveryDigest, RuleID: r.ID, DedupeKey: keyOf(r), SubjectType: "console"},
			content.Message(r.Address))
		switch {
		case errors.Is(err, store.ErrNotFound):
			res.add(r.ID, DigestRecipientGone)
		case err != nil:
			if first == nil {
				first = err
			}
		case out.Settled:
			res.add(r.ID, outcomeOfRow(out.Delivery)) // another run wrote the row first
		case out.Sent:
			res.add(r.ID, DigestSent)
		default:
			res.add(r.ID, DigestFailed)
		}
	}
	return first
}

func (g *Digester) logResult(what string, res DigestResult) {
	gone, deferred := 0, 0
	for _, r := range res.Recipients {
		switch r.Outcome {
		case DigestRecipientGone:
			gone++
		case DigestDeferred:
			deferred++
		}
	}
	g.log().Info(what, logx.Count("recipients", len(res.Recipients)), logx.Count("sent", res.Sent), logx.Count("failed", res.Failed),
		logx.Count("deferred", deferred), logx.Count("recipients_gone", gone), logx.Bool("forced", res.Forced))
}

// Rerender is the digest's RerenderFunc for the retry job: a digest row that failed is sent again
// as the fleet is at retry time, to the row's recipient, under the date of its key (a daily row
// "digest:<date>:<rule>" keeps its date in the subject; a forced row takes the date of the view).
// It is a decision not to send when the recipient switched the digest off meanwhile (why
// recipient_switched_off) or the digest was turned off meanwhile (why digest_disabled); both are
// recorded with the schema's reason deduped. A send-now ignores the switch when it is made, but a
// retry still pending once the digest is off is not sent.
func (g *Digester) Rerender(ctx context.Context, d store.Delivery, to store.Recipient) (mail.Message, string, error) {
	if !to.Digest {
		return mail.Message{}, "recipient_switched_off", nil
	}
	set, err := g.Store.ReadDigestSettings(ctx, store.ScopeAll())
	if err != nil {
		return mail.Message{}, "", err
	}
	if !set.Enabled {
		return mail.Message{}, "digest_disabled", nil
	}
	c, err := g.render(ctx, digestKeyDate(d.DedupeKey))
	if err != nil {
		return mail.Message{}, "", err
	}
	return c.Message(to.Address), "", nil
}

// digestKeyDate is the local date of a daily digest key ("digest:2006-01-02:<rule>"), or "".
func digestKeyDate(key string) string {
	rest, ok := strings.CutPrefix(key, "digest:")
	if !ok {
		return ""
	}
	date, _, ok := strings.Cut(rest, ":")
	if _, err := time.Parse("2006-01-02", date); !ok || err != nil {
		return ""
	}
	return date
}
