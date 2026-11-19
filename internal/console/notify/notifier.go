package notify

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// DefaultSendBudget bounds all the sends of one alert pass or retry run when SendBudget is zero.
// Rows not attempted inside it stay queued and the retry job sends them (store.QueuedStaleAfter).
const DefaultSendBudget = 40 * time.Second

// Dedupe-key prefixes of the two message kinds this package sends: "<prefix>:<batch id>:<rule id>".
const (
	keyAlert    = "alert"
	keyRecovery = "recovery"
)

// Notifier is the alerting step of every alarm-evaluator pass (ADR 0020 decisions 4 to 7). Pass
// syncs the episodes with the view the evaluator computed, then mails what is due. Package doc:
// "Batches and crash safety".
type Notifier struct {
	Store    *store.Store
	Dispatch *Dispatcher
	// Configured reports whether a relay is configured (mail.Settings.Configured). Without one the
	// episodes are still synced, but nothing is enqueued: they stay unnotified, and when mail is
	// set up later what is still open goes out as one message. Banner C03b says so meanwhile.
	Configured bool
	// Loc returns the operator's display zone for the message times; nil means UTC.
	Loc func() *time.Location
	// ConsoleURL is the console's public URL for the link in a message; empty means no link.
	ConsoleURL string
	// SendBudget bounds the sends of one pass; zero means DefaultSendBudget.
	SendBudget time.Duration
	// Log receives a summary line per pass that did something (counts only); nil means logx.Default().
	Log *logx.Logger

	ids batchIDs
	// beforeMark, when set, runs after a batch's delivery rows are written and before its episodes
	// are marked: the tests inject a crash there.
	beforeMark func(kind string) error
	// stopBeforeSend, when set, ends the pass after the marks without sending (the tests' crash there).
	stopBeforeSend bool
	// afterRecipients, when set, runs after the pass has read the recipients (the tests delete one there).
	afterRecipients func()
}

// PassResult counts what one pass did.
type PassResult struct {
	// Sync is what the episode sync opened, closed and skipped.
	Sync store.EpisodeSync
	// Rearmed counts alarms raised again because their snooze ended.
	Rearmed int
	// Alerted counts the episodes of the alert batch; Held the critical, unmailed episodes held back
	// because they are acknowledged or their customer is suspended.
	Alerted, Held int
	// Recovered counts the episodes of the recovery batch; RecoveryUnwanted those marked without a
	// message because no recipient receives recoveries.
	Recovered, RecoveryUnwanted int
	// RecoveryUnsent counts the closed episodes marked without a notice because no alert of their
	// batch was ever delivered (every row of it failed for good or was suppressed): nobody heard of
	// the alarm, so nobody is told it ended.
	RecoveryUnsent int
	// Messages counts the delivery rows written; Sent and Failed the outcomes of this pass's sends;
	// Deferred the rows left queued for the retry job because the send budget ran out.
	Messages, Sent, Failed, Deferred int
	// RecipientsGone counts the recipients deleted between the pass's read of the recipients and
	// their delivery row: skipped, the others mailed.
	RecipientsGone int
}

// outgoing is one message waiting to be sent: its delivery row and its content.
type outgoing struct {
	row store.Delivery
	msg mail.Message
}

func (n *Notifier) log() *logx.Logger {
	if n.Log != nil {
		return n.Log
	}
	return logx.Default()
}

func (n *Notifier) options() Options {
	o := Options{ConsoleURL: n.ConsoleURL}
	if n.Loc != nil {
		o.Loc = n.Loc()
	}
	return o
}

// batchIDs hands out batch ids. One process keeps one per Notifier or Retrier.
type batchIDs struct {
	mu   sync.Mutex
	last int64
}

// next returns a free batch id: starting at the store clock in milliseconds (and after the last id
// it looked at, so a frozen clock never repeats one), the first millisecond that no batch uses: no
// delivery row with the prefix "alert:<ms>:" or "recovery:<ms>:" (any recipient, any result) and
// no episode marked at it (notified_at or recovery_notified_at). Alert batches, recovery batches
// and the silent recovery marks all take their id here. It looks at maxBatchTries milliseconds at
// most, then fails with errBatchCollision. The check and the use are two store calls, so two
// processes (or a Notifier and a Retrier) that pick in the same instant can still meet; enqueue
// catches that for delivery rows.
func (b *batchIDs) next(ctx context.Context, st *store.Store) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := st.Now().UnixMilli()
	if id <= b.last {
		id = b.last + 1
	}
	for try := 0; try < maxBatchTries; try, id = try+1, id+1 {
		used, err := batchIDUsed(ctx, st, id)
		if err != nil {
			return 0, err
		}
		b.last = id
		if !used {
			return id, nil
		}
	}
	return 0, errBatchCollision
}

// batchIDUsed reports whether a delivery row or an episode carries the batch id.
func batchIDUsed(ctx context.Context, st *store.Store, id int64) (bool, error) {
	all, ms := store.ScopeAll(), strconv.FormatInt(id, 10)
	for _, prefix := range []string{keyAlert, keyRecovery} {
		rows, err := st.ListDeliveriesByKeyPrefix(ctx, all, prefix+":"+ms+":")
		if err != nil || len(rows) > 0 {
			return len(rows) > 0, err
		}
	}
	at := time.UnixMilli(id).UTC()
	for _, list := range []func(context.Context, store.Scope, time.Time) ([]store.Episode, error){st.ListEpisodesNotifiedAt, st.ListEpisodesRecoveryNotifiedAt} {
		eps, err := list(ctx, all, at)
		if err != nil || len(eps) > 0 {
			return len(eps) > 0, err
		}
	}
	return false, nil
}

// Pass is the alerting step of one evaluator pass over the view v:
//
//  1. SyncEpisodes with every alarm in v that can be acknowledged (critical and alarm level, and
//     every MISSING customer). A device whose facts could not be read (UNKNOWN) keeps its other
//     open episodes: not knowing is no recovery.
//  2. With mail configured and a recipient with the alerts switch: the snoozes that ended are
//     re-armed (not those of a suspended customer: rearmScope), and the open, unmailed, critical
//     episodes that are neither acknowledged nor of a suspended customer go to each such recipient
//     as one message.
//  3. With mail configured: the closed episodes whose alert was delivered to someone go to each
//     recipient with the recoveries switch as one notice (marked without a message when there is
//     no such recipient, or when no alert of their batch was ever delivered; owedRecoveries).
//  4. The sends, under SendBudget.
//
// The error is a store error (or a rendering bug): the pass failed and the job says so. A send
// failure is never an error here; it is in the delivery log.
func (n *Notifier) Pass(ctx context.Context, v state.FleetView) (PassResult, error) {
	var res PassResult
	all := store.ScopeAll()
	open, err := n.Store.ListOpenEpisodes(ctx, all)
	if err != nil {
		return res, err
	}
	keys, err := episodeKeys(v, open)
	if err != nil {
		return res, err
	}
	if res.Sync, err = n.Store.SyncEpisodes(ctx, all, keys, v.ComputedAt); err != nil {
		return res, err
	}
	if k := len(res.Sync.Skipped); k > 0 {
		n.log().Warn("alarm episodes skipped: their subject is gone", logx.Count("skipped", k))
	}
	if !n.Configured {
		return res, nil
	}
	recipients, err := n.Store.ListRecipients(ctx, all)
	if err != nil {
		return res, err
	}
	if n.afterRecipients != nil {
		n.afterRecipients()
	}
	var alertTo, recoveryTo []store.Recipient
	for _, r := range recipients {
		if r.Alerts {
			alertTo = append(alertTo, r)
		}
		if r.Recoveries {
			recoveryTo = append(recoveryTo, r)
		}
	}
	var outs []outgoing
	if len(alertTo) > 0 {
		o, err := n.alertBatch(ctx, v, alertTo, &res)
		if err != nil {
			return res, err
		}
		outs = append(outs, o...)
	}
	o, err := n.recoveryBatch(ctx, v, recoveryTo, &res)
	if err != nil {
		return res, err
	}
	outs = append(outs, o...)
	if n.stopBeforeSend {
		res.Deferred += len(outs) // the tests' crash after the marks: every row stays queued
	} else {
		err = sendAll(ctx, n.Dispatch, n.SendBudget, outs, &res.Sent, &res.Failed, &res.Deferred)
	}
	if res.Messages > 0 || res.Rearmed > 0 || res.RecoveryUnwanted > 0 || res.RecoveryUnsent > 0 || res.RecipientsGone > 0 { // a held alarm alone is not news every minute
		n.log().Info("alert pass", logx.Count("opened", len(res.Sync.Opened)), logx.Count("closed", len(res.Sync.Closed)),
			logx.Count("alerted", res.Alerted), logx.Count("held", res.Held), logx.Count("rearmed", res.Rearmed),
			logx.Count("recovered", res.Recovered), logx.Count("recovery_unsent", res.RecoveryUnsent), logx.Count("messages", res.Messages), logx.Count("sent", res.Sent),
			logx.Count("failed", res.Failed), logx.Count("deferred", res.Deferred), logx.Count("recipients_gone", res.RecipientsGone))
	}
	return res, err
}

// alertBatch writes the delivery rows of this pass's alert and marks its episodes.
func (n *Notifier) alertBatch(ctx context.Context, v state.FleetView, to []store.Recipient, res *PassResult) ([]outgoing, error) {
	all := store.ScopeAll()
	var err error
	if res.Rearmed, err = n.Store.RearmLapsedSnoozes(ctx, rearmScope(v)); err != nil {
		return nil, err
	}
	unmailed, err := n.Store.ListUnnotifiedEpisodes(ctx, all)
	if err != nil {
		return nil, err
	}
	m, err := loadMutes(ctx, n.Store, v)
	if err != nil {
		return nil, err
	}
	var batch []store.Episode
	for _, e := range unmailed {
		switch {
		case !IsCritical(e.AlarmType):
		case m.reason(e) != "":
			res.Held++
		default:
			batch = append(batch, e)
		}
	}
	if len(batch) == 0 {
		return nil, nil
	}
	content, err := RenderAlert(v, batch, n.options()) // before anything is written: a template bug writes nothing
	if err != nil {
		return nil, err
	}
	outs, at, err := n.enqueue(ctx, keyAlert, store.DeliveryAlert, content, to, res)
	if err != nil || len(outs) == 0 {
		return nil, err // no row: nothing is marked, the episodes wait for the next pass
	}
	if n.beforeMark != nil {
		if err := n.beforeMark(keyAlert); err != nil {
			return nil, err
		}
	}
	marked, err := n.Store.MarkEpisodesNotified(ctx, all, episodeIDs(batch), at)
	if err != nil {
		return nil, err
	}
	if marked == 0 {
		// An overlapping pass marked them first: its rows carry this alert, these would mail it twice.
		res.Messages -= len(outs)
		return nil, n.suppressAll(ctx, outs, "overlap")
	}
	res.Alerted = len(batch)
	return outs, nil
}

// rearmScope is where a lapsed snooze may be re-armed in this pass: the customers that are not
// suspended. The caller has a relay and an alert recipient. Re-arming resets the episode's
// notified_at, so re-arming one that cannot be mailed now would lose the recovery notice of the
// alert that was mailed; it waits for a pass that can mail it.
func rearmScope(v state.FleetView) store.Scope {
	ids := make([]string, 0, len(v.Customers))
	suspended := false
	for _, c := range v.Customers {
		if c.Muted {
			suspended = true
			continue
		}
		ids = append(ids, c.ID)
	}
	if !suspended {
		return store.ScopeAll()
	}
	return store.ScopeOf(ids...)
}

// recoveryBatch writes the delivery rows of this pass's recovery notice and marks its episodes.
// Only the episodes whose alert reached someone are in the notice (owedRecoveries).
func (n *Notifier) recoveryBatch(ctx context.Context, v state.FleetView, to []store.Recipient, res *PassResult) ([]outgoing, error) {
	all := store.ScopeAll()
	due, err := n.Store.ListRecoveryDueEpisodes(ctx, all)
	if err != nil || len(due) == 0 {
		return nil, err
	}
	due, unsent, err := n.owedRecoveries(ctx, due)
	if err != nil {
		return nil, err
	}
	if len(unsent) > 0 {
		if err := n.markWithoutNotice(ctx, unsent); err != nil {
			return nil, err
		}
		res.RecoveryUnsent = len(unsent)
	}
	if len(due) == 0 {
		return nil, nil
	}
	if len(to) == 0 {
		// Nobody receives recoveries: the notices are owed to no one. Marking them keeps a recipient who
		// switches recoveries on later from receiving a backlog of old ones.
		if err := n.markWithoutNotice(ctx, due); err != nil {
			return nil, err
		}
		res.RecoveryUnwanted = len(due)
		return nil, nil
	}
	content, err := RenderRecovery(v, due, n.options())
	if err != nil {
		return nil, err
	}
	outs, at, err := n.enqueue(ctx, keyRecovery, store.DeliveryRecovery, content, to, res)
	if err != nil || len(outs) == 0 {
		return nil, err
	}
	if n.beforeMark != nil {
		if err := n.beforeMark(keyRecovery); err != nil {
			return nil, err
		}
	}
	marked, err := n.Store.MarkRecoveryNotified(ctx, all, episodeIDs(due), at)
	if err != nil {
		return nil, err
	}
	if marked == 0 { // an overlapping pass marked them first
		res.Messages -= len(outs)
		return nil, n.suppressAll(ctx, outs, "overlap")
	}
	res.Recovered = len(due)
	return outs, nil
}

// owedRecoveries splits the closed, mailed episodes by what became of the alert of their batch
// (its delivery rows, "alert:<notified_at ms>:"): owed when a row of the batch was delivered;
// unsent when every row is final without having been delivered (failed for good, or suppressed),
// or the batch has no row; neither, so the decision waits for a later pass, while a row is still
// queued or has a retry scheduled. A recovery notice for an alert nobody received would be the
// only message its recipient ever got about the alarm.
func (n *Notifier) owedRecoveries(ctx context.Context, due []store.Episode) (owed, unsent []store.Episode, err error) {
	type outcome struct{ sent, pending bool }
	batches := map[int64]outcome{}
	for _, e := range due {
		ms := e.NotifiedAt.UnixMilli() // ListRecoveryDueEpisodes returns mailed episodes only
		o, ok := batches[ms]
		if !ok {
			rows, err := n.Store.ListDeliveriesByKeyPrefix(ctx, store.ScopeAll(), keyAlert+":"+strconv.FormatInt(ms, 10)+":")
			if err != nil {
				return nil, nil, err
			}
			for _, d := range rows {
				o.sent = o.sent || d.Result == store.DeliverySent
				o.pending = o.pending || d.Result == store.DeliveryQueued || d.Result == store.DeliveryFailed && d.NextRetryAt != nil
			}
			batches[ms] = o
		}
		switch {
		case o.sent:
			owed = append(owed, e)
		case !o.pending:
			unsent = append(unsent, e)
		}
	}
	return owed, unsent, nil
}

// markWithoutNotice settles the recovery of eps with no message. The mark takes a free batch id,
// so a retry of a real recovery batch, which finds its episodes by their mark, never picks them up.
func (n *Notifier) markWithoutNotice(ctx context.Context, eps []store.Episode) error {
	id, err := n.ids.next(ctx, n.Store)
	if err != nil {
		return err
	}
	_, err = n.Store.MarkRecoveryNotified(ctx, store.ScopeAll(), episodeIDs(eps), time.UnixMilli(id).UTC())
	return err
}

// maxBatchTries bounds the search for a free batch id (enqueue).
const maxBatchTries = 5

// errBatchCollision is a pass that found no free batch id in maxBatchTries milliseconds.
var errBatchCollision = errors.New("notify: no free batch id")

// enqueue writes one queued delivery row per recipient under a fresh batch id and returns the rows
// with the batch id as a time (what the episodes are marked with). Every row returned was created
// by this call. A recipient deleted since the pass read the list is skipped and counted
// (PassResult.RecipientsGone); with no row written the caller marks nothing.
//
// The batch id is a free one (batchIDs.next). A row found under it anyway (another process took
// the same millisecond meanwhile) is a collision: the rows this try wrote are suppressed and a new
// id is taken, at most maxBatchTries times, then the pass fails (errBatchCollision). An alert is
// never sent under, nor its episodes marked with, an id another batch holds.
func (n *Notifier) enqueue(ctx context.Context, prefix, kind string, c Content, to []store.Recipient, res *PassResult) ([]outgoing, time.Time, error) {
	for try := 0; try < maxBatchTries; try++ {
		id, err := n.ids.next(ctx, n.Store)
		if err != nil {
			return nil, time.Time{}, err
		}
		outs := make([]outgoing, 0, len(to))
		gone, collided := 0, false
		for _, r := range to {
			row, created, err := n.Dispatch.Enqueue(ctx, store.DeliveryInput{Kind: kind, RuleID: r.ID, DedupeKey: batchKey(prefix, id, r.ID), SubjectType: "console"})
			switch {
			case errors.Is(err, store.ErrNotFound): // deleted since the pass read the recipients: skip it, mail the others
				gone++
				continue
			case err != nil:
				return nil, time.Time{}, err
			}
			if !created {
				collided = true
				break
			}
			outs = append(outs, outgoing{row: row, msg: c.Message(r.Address)})
		}
		if !collided {
			res.Messages += len(outs)
			res.RecipientsGone += gone
			return outs, time.UnixMilli(id).UTC(), nil
		}
		if err := n.suppressAll(ctx, outs, "batch_id_taken"); err != nil {
			return nil, time.Time{}, err
		}
	}
	n.log().Error("alert pass found no free batch id", logx.Kind(kind), logx.Count("tries", maxBatchTries))
	return nil, time.Time{}, errBatchCollision
}

// suppressAll records the decision not to send rows this pass wrote and will not send (reason
// deduped: another batch carries the alert; why says which case).
func (n *Notifier) suppressAll(ctx context.Context, outs []outgoing, why string) error {
	for _, o := range outs {
		if _, err := n.Dispatch.suppress(ctx, o.row, reasonDeduped, why); err != nil {
			return err
		}
	}
	return nil
}

// sendAll delivers every outgoing message under one budget. A row not attempted before the budget
// ends stays queued for the retry job. It returns the first store error and still tries the rest.
func sendAll(ctx context.Context, d *Dispatcher, budget time.Duration, outs []outgoing, sent, failed, deferred *int) error {
	if len(outs) == 0 {
		return nil
	}
	if budget <= 0 {
		budget = DefaultSendBudget
	}
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var first error
	for _, o := range outs {
		if spent(sctx) {
			*deferred++
			continue
		}
		r, err := d.Deliver(sctx, o.row, o.msg)
		switch {
		case err != nil:
			if first == nil {
				first = err
			}
		case r.Sent:
			*sent++
		case r.Err != nil:
			*failed++
		}
	}
	return first
}

// spent reports whether the send budget of ctx is used up (its deadline passed or it was cancelled).
func spent(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	dl, ok := ctx.Deadline()
	return ok && !time.Now().Before(dl)
}

// stateClass is what the notifier does with a device state (ADR 0020 decision 4).
type stateClass int

const (
	// classNoEpisode: no alarm (OK, PENDING_FIRST, MISSED, DEFERRED) or one suppressed by its device
	// (RETIRED, PAUSED); nothing to acknowledge, nothing to mail.
	classNoEpisode stateClass = iota + 1
	// classEpisodeOnly: an alarm-level state. It opens an episode, so it can be acknowledged, and is
	// never mailed here: it belongs to the digest.
	classEpisodeOnly
	// classMails: a critical state. It opens an episode and mails at once.
	classMails
)

// stateClasses lists every state of the engine (state.Precedence). A state missing from it fails
// the pass (episodeKeys): a new state is never dropped silently. A test holds it equal to the
// engine's list, to criticalAlarms and to store.EpisodeAlarmTypes.
var stateClasses = map[state.State]stateClass{
	state.StateRetired:          classNoEpisode,
	state.StatePaused:           classNoEpisode,
	state.StateUnknown:          classMails,
	state.StateIdentityConflict: classMails,
	state.StatePendingFirst:     classNoEpisode,
	state.StateBlank:            classMails,
	state.StateStuck:            classMails,
	state.StateFailed:           classMails,
	state.StatePartial:          classEpisodeOnly,
	state.StateUnseen:           classMails,
	state.StateStale:            classMails,
	state.StateEscrowUnverified: classEpisodeOnly,
	state.StateMaintFailed:      classEpisodeOnly,
	state.StateMissed:           classNoEpisode,
	state.StateDeferred:         classNoEpisode,
	state.StateOutdated:         classEpisodeOnly,
	state.StateOK:               classNoEpisode,
}

// classOf is the class of s; false for a state this package does not know.
func classOf(s state.State) (stateClass, bool) {
	c, ok := stateClasses[s]
	return c, ok
}

// episodeKeys is the input of SyncEpisodes: every alarm of v that can be acknowledged (classMails
// and classEpisodeOnly). A device whose facts could not be read (UNKNOWN among its alarm flags)
// also keeps the episodes it has open: the evaluator cannot see them, and closing them would mail a
// recovery nobody observed. A state of v that stateClasses does not list is an error naming the
// state word, and nothing is synced.
func episodeKeys(v state.FleetView, open []store.Episode) ([]store.EpisodeKey, error) {
	openOf := map[string][]store.Episode{}
	for _, e := range open {
		if e.SubjectType == store.EpisodeDevice {
			openOf[e.SubjectID] = append(openOf[e.SubjectID], e)
		}
	}
	var keys []store.EpisodeKey
	for i := range v.Devices {
		d := &v.Devices[i]
		for _, s := range append(append([]state.State{d.Primary}, d.Flags...), d.AlarmFlags...) {
			if _, ok := classOf(s); !ok {
				return nil, fmt.Errorf("notify: the engine reported a state the notifier does not classify: %q", string(s))
			}
		}
		if d.Primary == state.StateRetired {
			continue
		}
		unknown := false
		for _, s := range d.AlarmFlags {
			unknown = unknown || s == state.StateUnknown
			if c, _ := classOf(s); c == classNoEpisode {
				continue
			}
			k := store.EpisodeKey{SubjectType: store.EpisodeDevice, SubjectID: d.ID, AlarmType: string(s)}
			if s == d.Primary {
				k.Since = d.Since
			}
			keys = append(keys, k)
		}
		if unknown {
			for _, e := range openOf[d.ID] {
				keys = append(keys, store.EpisodeKey{SubjectType: store.EpisodeDevice, SubjectID: d.ID, AlarmType: e.AlarmType})
			}
		}
	}
	for _, c := range v.Customers {
		if c.Status == state.CustomerMissing {
			keys = append(keys, store.EpisodeKey{SubjectType: store.EpisodeCustomer, SubjectID: c.ID, AlarmType: store.AlarmMissing})
		}
	}
	return keys, nil
}

// mutes says why an episode is held back from mail now: acknowledged or snoozed (an active
// acknowledgement of its subject and type), or its customer suspended (DH10).
type mutes struct {
	acks      map[string]string // subject type/id/ack type -> suppress reason
	suspended map[string]bool   // customer id
}

func loadMutes(ctx context.Context, st *store.Store, v state.FleetView) (mutes, error) {
	m := mutes{acks: map[string]string{}, suspended: map[string]bool{}}
	acks, err := st.ListActiveAcks(ctx, store.ScopeAll())
	if err != nil {
		return m, err
	}
	for _, a := range acks {
		reason := "acknowledged"
		if a.SnoozeUntil != nil || a.UntilNextSuccess {
			reason = "snoozed"
		}
		m.acks[a.SubjectType+"/"+a.SubjectID+"/"+a.AlarmType] = reason
	}
	for _, c := range v.Customers {
		if c.Muted {
			m.suspended[c.ID] = true
		}
	}
	return m, nil
}

// reason is the store's suppress reason that holds e back, or "" when it may be mailed.
func (m mutes) reason(e store.Episode) string {
	if t, ok := store.AckAlarmType(e.AlarmType); ok {
		if r := m.acks[e.SubjectType+"/"+e.SubjectID+"/"+t]; r != "" {
			return r
		}
	}
	if m.suspended[e.CustomerID] {
		return "customer_suspended"
	}
	return ""
}

func episodeIDs(eps []store.Episode) []string {
	ids := make([]string, len(eps))
	for i, e := range eps {
		ids[i] = e.ID
	}
	return ids
}

func batchKey(prefix string, id int64, ruleID string) string {
	return prefix + ":" + strconv.FormatInt(id, 10) + ":" + ruleID
}

// errBadKey is a dedupe key this package did not write.
var errBadKey = errors.New("notify: not a batch key")

// parseBatchKey splits "<prefix>:<batch id>:<rule id>".
func parseBatchKey(key, prefix string) (time.Time, string, error) {
	rest, ok := strings.CutPrefix(key, prefix+":")
	if !ok {
		return time.Time{}, "", errBadKey
	}
	ms, rule, ok := strings.Cut(rest, ":")
	id, err := strconv.ParseInt(ms, 10, 64)
	if !ok || err != nil || id < 1000000000000 || rule == "" {
		return time.Time{}, "", errBadKey
	}
	return time.UnixMilli(id).UTC(), rule, nil
}
