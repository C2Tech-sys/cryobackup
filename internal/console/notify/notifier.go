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

	mu        sync.Mutex
	lastBatch int64
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

// batchID is a fresh batch id: the store clock in milliseconds, strictly increasing within this
// process so that two batches can never share one (a frozen clock, two passes in one millisecond).
func (n *Notifier) batchID() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := n.Store.Now().UnixMilli()
	if id <= n.lastBatch {
		id = n.lastBatch + 1
	}
	n.lastBatch = id
	return id
}

// Pass is the alerting step of one evaluator pass over the view v:
//
//  1. SyncEpisodes with every alarm in v that can be acknowledged (critical and alarm level, and
//     every MISSING customer). A device whose facts could not be read (UNKNOWN) keeps its other
//     open episodes: not knowing is no recovery.
//  2. With mail configured and a recipient with the alerts switch: the snoozes that ended are
//     re-armed, and the open, unmailed, critical episodes that are neither acknowledged nor of a
//     suspended customer go to each such recipient as one message.
//  3. With mail configured: the closed episodes that were mailed go to each recipient with the
//     recoveries switch as one notice (marked without a message when there is no such recipient).
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
	if res.Messages > 0 || res.Rearmed > 0 || res.RecoveryUnwanted > 0 || res.RecipientsGone > 0 { // a held alarm alone is not news every minute
		n.log().Info("alert pass", logx.Count("opened", len(res.Sync.Opened)), logx.Count("closed", len(res.Sync.Closed)),
			logx.Count("alerted", res.Alerted), logx.Count("held", res.Held), logx.Count("rearmed", res.Rearmed),
			logx.Count("recovered", res.Recovered), logx.Count("messages", res.Messages), logx.Count("sent", res.Sent),
			logx.Count("failed", res.Failed), logx.Count("deferred", res.Deferred), logx.Count("recipients_gone", res.RecipientsGone))
	}
	return res, err
}

// alertBatch writes the delivery rows of this pass's alert and marks its episodes.
func (n *Notifier) alertBatch(ctx context.Context, v state.FleetView, to []store.Recipient, res *PassResult) ([]outgoing, error) {
	all := store.ScopeAll()
	var err error
	if res.Rearmed, err = n.Store.RearmLapsedSnoozes(ctx, all); err != nil {
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
	if _, err := n.Store.MarkEpisodesNotified(ctx, all, episodeIDs(batch), at); err != nil {
		return nil, err
	}
	res.Alerted = len(batch)
	return outs, nil
}

// recoveryBatch writes the delivery rows of this pass's recovery notice and marks its episodes.
func (n *Notifier) recoveryBatch(ctx context.Context, v state.FleetView, to []store.Recipient, res *PassResult) ([]outgoing, error) {
	all := store.ScopeAll()
	due, err := n.Store.ListRecoveryDueEpisodes(ctx, all)
	if err != nil || len(due) == 0 {
		return nil, err
	}
	if len(to) == 0 {
		// Nobody receives recoveries: the notices are owed to no one. Marking them keeps a recipient who
		// switches recoveries on later from receiving a backlog of old ones.
		if _, err := n.Store.MarkRecoveryNotified(ctx, all, episodeIDs(due), time.Time{}); err != nil {
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
	if _, err := n.Store.MarkRecoveryNotified(ctx, all, episodeIDs(due), at); err != nil {
		return nil, err
	}
	res.Recovered = len(due)
	return outs, nil
}

// enqueue writes one queued delivery row per recipient under a fresh batch id and returns the rows
// with the batch id as a time (what the episodes are marked with). A recipient deleted since the
// pass read the list is skipped and counted (PassResult.RecipientsGone); with no row written the
// caller marks nothing.
func (n *Notifier) enqueue(ctx context.Context, prefix, kind string, c Content, to []store.Recipient, res *PassResult) ([]outgoing, time.Time, error) {
	id := n.batchID()
	outs := make([]outgoing, 0, len(to))
	for _, r := range to {
		row, created, err := n.Dispatch.Enqueue(ctx, store.DeliveryInput{Kind: kind, RuleID: r.ID, DedupeKey: batchKey(prefix, id, r.ID), SubjectType: "console"})
		switch {
		case errors.Is(err, store.ErrNotFound): // deleted since the pass read the recipients: skip it, mail the others
			res.RecipientsGone++
			continue
		case err != nil:
			return nil, time.Time{}, err
		}
		if created {
			res.Messages++
		}
		outs = append(outs, outgoing{row: row, msg: c.Message(r.Address)})
	}
	return outs, time.UnixMilli(id).UTC(), nil
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
