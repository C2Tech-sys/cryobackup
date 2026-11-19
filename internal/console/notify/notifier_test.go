package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// world is a real store with a site of devices, recipients, a FakeSender and a Notifier and
// Retrier wired to them. The view is built by hand: the state engine has its own tests, and here
// the question is what the notifier does with a given verdict.
type world struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	clk     *clock
	fake    *mail.FakeSender
	n       *Notifier
	r       *Retrier
	logs    *syncBuffer
	actor   store.AuditActor
	cust    map[string]string // name -> id
	devices []string          // ids, in creation order
	host    map[string]string // id -> host name
	devCust map[string]string // id -> customer id
	states  map[string]state.State
	missing map[string]bool
	muted   map[string]bool
}

// syncBuffer is a log sink the race detector accepts.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newWorld(t *testing.T) *world {
	t.Helper()
	st, clk := memStore(t, renderNow)
	ctx := context.Background()
	all := store.ScopeAll()
	if _, err := st.CreateStorageTarget(ctx, store.StorageTarget{Name: "Example Storage", Provider: "wasabi", Endpoint: "s3.example.test",
		Bucket: "cryo-test", Prefix: "fleet", GatewayURL: "https://gateway.example.test:9443", CredentialRef: "CRYO_TEST_S3", IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	secs, err := store.PolicySections(policy.Default(policy.ClassUnknown))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePolicy(ctx, all, store.PolicyInput{Scope: "default", Name: "Default", Sections: secs}); err != nil {
		t.Fatal(err)
	}
	admin, err := st.CreateAdmin(ctx, store.Admin{Username: "root", Email: "root@example.test", Role: "owner", PasswordHash: "$argon2id$test"})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	log := logx.New(logs, logx.LevelInfo)
	fake := mail.NewFakeSender()
	d := &Dispatcher{Store: st, Sender: fake, From: "CryoBackup <console@example.test>", Log: log}
	w := &world{t: t, ctx: ctx, st: st, clk: clk, fake: fake, logs: logs,
		actor: store.AuditActor{AdminID: admin.ID, Username: "root", Method: "api"},
		cust:  map[string]string{}, host: map[string]string{}, devCust: map[string]string{},
		states: map[string]state.State{}, missing: map[string]bool{}, muted: map[string]bool{}}
	w.n = &Notifier{Store: st, Dispatch: d, Configured: true, ConsoleURL: "https://console.example.test/", Log: log}
	w.r = &Retrier{Store: st, Dispatch: d, ConsoleURL: "https://console.example.test/", Log: log,
		View: func(context.Context) (state.FleetView, error) { return w.view(), nil }}
	return w
}

// site adds a customer with n devices, all OK.
func (w *world) site(name string, n int) []string {
	w.t.Helper()
	c, err := w.st.CreateCustomer(w.ctx, store.ScopeAll(), store.Customer{Name: name})
	if err != nil {
		w.t.Fatal(err)
	}
	w.cust[name] = c.ID
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("01TESTDEV%03d%014d", len(w.devices), i)
		host := fmt.Sprintf("%s-PC-%02d", strings.ToUpper(name[:3]), i+1)
		at := renderNow.Add(-30 * 24 * time.Hour).UnixMilli()
		if err := w.st.Write(w.ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO devices (id, customer_id, hostname, enrolled_at, enrolled_via, repo_path, created_at, updated_at)
				VALUES (?,?,?,?, 'token', ?, ?, ?)`, id, c.ID, host, at, "fleet/"+id+"/", at, at)
			return err
		}); err != nil {
			w.t.Fatal(err)
		}
		w.devices = append(w.devices, id)
		w.host[id], w.devCust[id], w.states[id] = host, c.ID, state.StateOK
		ids = append(ids, id)
	}
	return ids
}

func (w *world) recipient(addr string, alerts, recoveries bool) store.Recipient {
	w.t.Helper()
	r, err := w.st.CreateRecipient(w.ctx, store.ScopeAll(), store.RecipientInput{Name: "Ops " + addr, Address: addr, Alerts: alerts, Recoveries: recoveries}, w.actor)
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) set(s state.State, ids ...string) {
	for _, id := range ids {
		w.states[id] = s
	}
}

// view is the fleet as the evaluator would see it now.
func (w *world) view() state.FleetView {
	v := state.FleetView{ComputedAt: w.clk.Now(), Complete: true}
	for _, id := range w.devices {
		s := w.states[id]
		d := state.DeviceState{ID: id, CustomerID: w.devCust[id], Hostname: w.host[id], Primary: s, Flags: []state.State{s},
			Since: w.clk.Now().Add(-26 * time.Hour)}
		if s != state.StateOK && s != state.StateRetired && s != state.StatePaused {
			d.AlarmFlags = []state.State{s}
		}
		v.Devices = append(v.Devices, d)
	}
	for name, id := range w.cust {
		c := state.CustomerState{ID: id, Name: name, Muted: w.muted[id]}
		if w.missing[id] {
			c.Status = state.CustomerMissing
		}
		v.Customers = append(v.Customers, c)
	}
	return v
}

func (w *world) pass() PassResult {
	w.t.Helper()
	res, err := w.n.Pass(w.ctx, w.view())
	if err != nil {
		w.t.Fatalf("pass: %v", err)
	}
	return res
}

func (w *world) retry() RetryResult {
	w.t.Helper()
	res, err := w.r.Run(w.ctx)
	if err != nil {
		w.t.Fatalf("retry: %v", err)
	}
	return res
}

func (w *world) open() []store.Episode {
	w.t.Helper()
	eps, err := w.st.ListOpenEpisodes(w.ctx, store.ScopeAll())
	if err != nil {
		w.t.Fatal(err)
	}
	return eps
}

func (w *world) unnotified() []store.Episode {
	w.t.Helper()
	eps, err := w.st.ListUnnotifiedEpisodes(w.ctx, store.ScopeAll())
	if err != nil {
		w.t.Fatal(err)
	}
	return eps
}

func (w *world) deliveries() []store.Delivery {
	w.t.Helper()
	l, err := w.st.ListRecentDeliveries(w.ctx, store.ScopeAll(), store.DeliveryFilter{}, store.Page{Limit: 1000})
	if err != nil {
		w.t.Fatal(err)
	}
	return l.Items
}

// sentTo groups the accepted messages by recipient address.
func sentTo(msgs []mail.Message) map[string][]mail.Message {
	out := map[string][]mail.Message{}
	for _, m := range msgs {
		out[m.To[0]] = append(out[m.To[0]], m)
	}
	return out
}

// noLeaks fails when a log line names a host, a customer or an address.
func (w *world) noLeaks() {
	w.t.Helper()
	logs := w.logs.String()
	for _, h := range w.host {
		if strings.Contains(logs, h) {
			w.t.Fatalf("a log line names host %s:\n%s", h, logs)
		}
	}
	for name := range w.cust {
		if strings.Contains(logs, name) {
			w.t.Fatalf("a log line names customer %s", name)
		}
	}
	if strings.Contains(logs, "@") {
		t := logs
		w.t.Fatalf("a log line carries an address:\n%s", t)
	}
}

// A site of 40 devices going STALE in one pass gives ONE message per recipient; a second pass none.
func TestNotifyOneMessagePerRecipientForASite(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 40)
	w.recipient("ops@example.test", true, true)
	w.recipient("oncall@example.test", true, false)
	w.recipient("digest-only@example.test", false, false)
	w.pass() // all green: nothing
	if w.fake.Calls() != 0 {
		t.Fatalf("a green fleet sent %d messages", w.fake.Calls())
	}

	w.clk.Advance(time.Minute)
	w.set(state.StateStale, site...)
	res := w.pass()
	if len(res.Sync.Opened) != 40 || res.Alerted != 40 || res.Messages != 2 || res.Sent != 2 {
		t.Fatalf("pass result %+v", res)
	}
	by := sentTo(w.fake.Sent())
	if len(by) != 2 || len(by["ops@example.test"]) != 1 || len(by["oncall@example.test"]) != 1 {
		t.Fatalf("messages by recipient: %v", by)
	}
	m := by["ops@example.test"][0]
	if m.Subject != "[CryoBackup] 40 new alarms at 1 customer" || strings.Count(m.Text, "[STALE]") != 40 || m.Kind != mail.KindAlert {
		t.Fatalf("message: %q\n%s", m.Subject, m.Text)
	}
	if len(w.unnotified()) != 0 {
		t.Fatal("episodes left unnotified after the alert went out")
	}
	for _, d := range w.deliveries() {
		if d.Kind != store.DeliveryAlert || d.Result != store.DeliverySent || !strings.HasPrefix(d.DedupeKey, "alert:") || d.SubjectType != "console" {
			t.Fatalf("delivery row %+v", d)
		}
	}

	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Messages != 0 || w.fake.Calls() != 2 {
		t.Fatalf("second pass %+v, calls %d", res, w.fake.Calls())
	}
	w.noLeaks()
}

// The alarm ends: one recovery notice per recipient with the recoveries switch; an episode that was
// never mailed gets none.
func TestNotifyRecovery(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 3)
	w.recipient("ops@example.test", true, true)
	w.recipient("oncall@example.test", true, false)
	w.set(state.StateFailed, site[0], site[1])
	w.pass()
	w.fake.Reset()

	// An alarm-level state opens an episode but is never mailed, so it owes no recovery either.
	w.clk.Advance(time.Minute)
	w.set(state.StatePartial, site[2])
	if res := w.pass(); res.Messages != 0 || len(res.Sync.Opened) != 1 {
		t.Fatalf("alarm-level pass %+v", res)
	}

	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site...)
	res := w.pass()
	if len(res.Sync.Closed) != 3 || res.Recovered != 2 || res.Messages != 1 || res.Sent != 1 {
		t.Fatalf("recovery pass %+v", res)
	}
	sent := w.fake.Sent()
	if len(sent) != 1 || sent[0].To[0] != "ops@example.test" || sent[0].Kind != mail.KindRecovery ||
		sent[0].Subject != "[CryoBackup] 2 alarms resolved at 1 customer" || strings.Contains(sent[0].Text, w.host[site[2]]) {
		t.Fatalf("recovery messages %+v", sent)
	}
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Messages != 0 {
		t.Fatalf("a second recovery pass %+v", res)
	}
	w.noLeaks()
}

// Alarm-level states give no mail but have episodes, so they can be acknowledged.
func TestNotifyAlarmLevelOpensEpisodesWithoutMail(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 4)
	w.recipient("ops@example.test", true, true)
	w.set(state.StatePartial, site[0])
	w.set(state.StateMaintFailed, site[1])
	w.set(state.StateEscrowUnverified, site[2])
	w.set(state.StateOutdated, site[3])
	res := w.pass()
	if len(res.Sync.Opened) != 4 || res.Messages != 0 || w.fake.Calls() != 0 {
		t.Fatalf("pass %+v, calls %d", res, w.fake.Calls())
	}
	if _, err := w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: site[0], AlarmType: "partial"}, w.actor); err != nil {
		t.Fatalf("an alarm-level state cannot be acknowledged: %v", err)
	}
}

// Acknowledged subjects and suspended customers are held back; when that ends, the alarm goes out.
func TestNotifyAcknowledgedAndSuspendedGetNone(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	paused := w.site("Northwind Dental", 1)
	w.recipient("ops@example.test", true, true)
	w.muted[w.cust["Northwind Dental"]] = true
	w.set(state.StateFailed, site[0])
	w.set(state.StateStale, paused[0])
	w.missing[w.cust["Northwind Dental"]] = true

	// The FAILED device is acknowledged before the alert could go out: sync first, ack, then pass.
	w.n.Configured = false
	w.pass()
	w.n.Configured = true
	ack, err := w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: site[0], AlarmType: "failed", Note: "known"}, w.actor)
	if err != nil {
		t.Fatal(err)
	}
	w.clk.Advance(time.Minute)
	res := w.pass()
	if res.Messages != 0 || res.Held != 3 || w.fake.Calls() != 0 {
		t.Fatalf("held pass %+v, calls %d", res, w.fake.Calls())
	}

	// The operator clears the acknowledgement while the alarm persists: it goes out now.
	if _, err := w.st.ClearAck(w.ctx, store.ScopeAll(), ack.ID, w.actor); err != nil {
		t.Fatal(err)
	}
	w.clk.Advance(time.Minute)
	res = w.pass()
	sent := w.fake.Sent()
	if res.Alerted != 1 || res.Held != 2 || len(sent) != 1 || !strings.Contains(sent[0].Text, w.host[site[0]]) || strings.Contains(sent[0].Text, "Northwind") {
		t.Fatalf("after the ack was cleared: %+v\n%v", res, sent)
	}
	// The customer is resumed: its alarms go out.
	w.muted[w.cust["Northwind Dental"]] = false
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Alerted != 2 || res.Messages != 1 {
		t.Fatalf("after the customer resumed: %+v", res)
	}
}

// A snooze that ends while the alarm persists raises the alert again, once.
func TestNotifySnoozeExpiryReRaises(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateStale, site[0])
	w.pass()
	until := w.clk.Now().Add(2 * time.Hour)
	if _, err := w.st.CreateAck(w.ctx, store.ScopeAll(), store.AckInput{SubjectType: store.EpisodeDevice, SubjectID: site[0], AlarmType: "stale", SnoozeUntil: &until}, w.actor); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w.clk.Advance(30 * time.Minute)
		w.pass()
	}
	if w.fake.Calls() != 1 {
		t.Fatalf("%d messages while snoozed", w.fake.Calls())
	}
	w.clk.Advance(time.Hour) // the snooze ends; the device is still STALE
	res := w.pass()
	if res.Rearmed != 1 || res.Alerted != 1 || w.fake.Calls() != 2 {
		t.Fatalf("snooze expiry pass %+v, calls %d", res, w.fake.Calls())
	}
	for i := 0; i < 3; i++ {
		w.clk.Advance(time.Minute)
		w.pass()
	}
	if w.fake.Calls() != 2 {
		t.Fatalf("re-raised more than once: %d messages", w.fake.Calls())
	}
}

// SMTP not configured: episodes are synced, nothing is enqueued; configured later, what is still open
// goes out as one message.
func TestNotifyUnconfiguredThenConfigured(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 3)
	w.recipient("ops@example.test", true, true)
	w.n.Configured = false
	w.set(state.StateFailed, site...)
	res := w.pass()
	if len(res.Sync.Opened) != 3 || res.Messages != 0 || len(w.deliveries()) != 0 || len(w.unnotified()) != 3 {
		t.Fatalf("unconfigured pass %+v", res)
	}
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[2])
	w.pass()
	w.n.Configured = true
	w.clk.Advance(time.Minute)
	res = w.pass()
	sent := w.fake.Sent()
	if res.Alerted != 2 || len(sent) != 1 || strings.Count(sent[0].Text, "[FAILED]") != 2 {
		t.Fatalf("configured pass %+v %v", res, sent)
	}
}

// No recipient with the alerts switch: nothing is enqueued; a recipient added later gets one message.
func TestNotifyNoRecipientThenOne(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	w.recipient("digest-only@example.test", false, false)
	w.set(state.StateUnseen, site...)
	res := w.pass()
	if len(res.Sync.Opened) != 2 || res.Messages != 0 || len(w.deliveries()) != 0 || len(w.unnotified()) != 2 {
		t.Fatalf("no-recipient pass %+v", res)
	}
	w.recipient("ops@example.test", true, false)
	w.clk.Advance(time.Minute)
	if res := w.pass(); res.Alerted != 2 || res.Messages != 1 || w.fake.Calls() != 1 {
		t.Fatalf("pass with a recipient %+v", res)
	}
}

// A customer that goes MISSING is mailed with the customer's name; it sorts first.
func TestNotifyMissingCustomer(t *testing.T) {
	w := newWorld(t)
	w.site("Northwind Dental", 0)
	site := w.site("Alpha Clinic", 1)
	w.recipient("ops@example.test", true, true)
	w.missing[w.cust["Northwind Dental"]] = true
	w.set(state.StateBlank, site[0])
	w.pass()
	sent := w.fake.Sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "[MISSING] no active device") ||
		strings.Index(sent[0].Text, "Northwind Dental") > strings.Index(sent[0].Text, "Alpha Clinic") {
		t.Fatalf("missing customer message %v", sent)
	}
}

// A device whose facts could not be read keeps its open episodes: UNKNOWN is no recovery.
func TestNotifyUnknownKeepsOpenEpisodes(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateFailed, site[0])
	w.pass()
	w.clk.Advance(time.Minute)
	w.set(state.StateUnknown, site[0])
	res := w.pass()
	if len(res.Sync.Closed) != 0 || res.Recovered != 0 || len(res.Sync.Opened) != 1 || res.Sync.Opened[0].AlarmType != "UNKNOWN" {
		t.Fatalf("unknown pass %+v", res)
	}
	if n := len(w.open()); n != 2 {
		t.Fatalf("%d open episodes, want FAILED and UNKNOWN", n)
	}
}

// A sender that hangs does not stall the pass: each send has its deadline, the pass its budget, and
// the outcome is in the delivery log, not in the job result.
type hangingSender struct{ calls int }

func (h *hangingSender) Send(ctx context.Context, _ mail.Message) error {
	h.calls++
	<-ctx.Done()
	return &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true}
}

func TestNotifyHangingSenderDoesNotStallThePass(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 5)
	for i := 0; i < 4; i++ {
		w.recipient(fmt.Sprintf("ops%d@example.test", i), true, true)
	}
	hang := &hangingSender{}
	w.n.Dispatch.Sender = hang
	w.n.Dispatch.SendTimeout = 50 * time.Millisecond
	w.n.SendBudget = 120 * time.Millisecond
	w.set(state.StateFailed, site...)
	start := time.Now()
	res, err := w.n.Pass(w.ctx, w.view())
	if err != nil {
		t.Fatalf("mail trouble failed the pass: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the pass took %v", took)
	}
	if res.Alerted != 5 || res.Messages != 4 || res.Sent != 0 || res.Failed+res.Deferred != 4 || res.Deferred == 0 {
		t.Fatalf("pass %+v", res)
	}
	if len(w.unnotified()) != 0 {
		t.Fatal("the episodes are not marked: the delivery rows own the retry now")
	}
	failed, queued := 0, 0
	for _, d := range w.deliveries() {
		switch d.Result {
		case store.DeliveryFailed:
			failed++
			if d.ErrorClass != "timeout" || d.NextRetryAt == nil {
				t.Fatalf("failed row %+v", d)
			}
		case store.DeliveryQueued:
			queued++
		}
	}
	if failed != res.Failed || queued != res.Deferred {
		t.Fatalf("rows: %d failed %d queued; result %+v", failed, queued, res)
	}
}

// A store that cannot be read fails the pass: never a silent green.
func TestNotifyStoreErrorFailsThePass(t *testing.T) {
	w := newWorld(t)
	w.site("Summit Eye Care", 1)
	if err := w.st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.n.Pass(w.ctx, w.view()); err == nil {
		t.Fatal("a pass over a closed store succeeded")
	}
	if _, err := w.r.Run(w.ctx); err == nil {
		t.Fatal("a retry run over a closed store succeeded")
	}
}

// The crash between "delivery rows written" and "episodes marked": nothing is mailed twice and
// nothing is lost.
func TestNotifyCrashBetweenEnqueueAndMark(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 3)
	w.recipient("ops@example.test", true, true)
	w.recipient("oncall@example.test", true, false)
	w.set(state.StateFailed, site[0], site[1])
	crash := errors.New("crash")
	w.n.beforeMark = func(string) error { return crash }
	if _, err := w.n.Pass(w.ctx, w.view()); !errors.Is(err, crash) {
		t.Fatalf("injected crash: %v", err)
	}
	w.n.beforeMark = nil
	if len(w.unnotified()) != 2 || w.fake.Calls() != 0 || len(w.deliveries()) != 2 {
		t.Fatalf("after the crash: %d unnotified, %d sent, %d rows", len(w.unnotified()), w.fake.Calls(), len(w.deliveries()))
	}

	// The process restarts (a new Notifier: no memory of the last batch id) and the alarm grew meanwhile.
	w.n = &Notifier{Store: w.st, Dispatch: w.n.Dispatch, Configured: true, Log: w.n.Log}
	w.clk.Advance(time.Minute)
	w.set(state.StateFailed, site[2])
	res := w.pass()
	by := sentTo(w.fake.Sent())
	if res.Alerted != 3 || len(by["ops@example.test"]) != 1 || len(by["oncall@example.test"]) != 1 ||
		strings.Count(by["ops@example.test"][0].Text, "[FAILED]") != 3 {
		t.Fatalf("pass after the crash %+v, %v", res, by)
	}

	// The orphaned rows of the crashed pass come due and are suppressed: no episode carries their batch id.
	w.clk.Advance(store.QueuedStaleAfter + time.Minute)
	rr := w.retry()
	if rr.Due != 2 || rr.Suppressed != 2 || rr.Sent != 0 || w.fake.Calls() != 2 {
		t.Fatalf("retry of the orphans %+v, calls %d", rr, w.fake.Calls())
	}
	for _, d := range w.deliveries() {
		if d.Result == store.DeliveryQueued || d.Result == store.DeliveryFailed {
			t.Fatalf("a row is still waiting: %+v", d)
		}
	}
}

// The crash between "episodes marked" and the sends: the retry job sends the queued rows, once.
func TestNotifyCrashBeforeTheSends(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	w.recipient("ops@example.test", true, true)
	w.set(state.StateStale, site...)
	w.n.stopBeforeSend = true // as if the process stopped after the marks
	res := w.pass()
	w.n.stopBeforeSend = false
	if res.Alerted != 2 || res.Deferred != 1 || w.fake.Calls() != 0 || len(w.unnotified()) != 0 {
		t.Fatalf("pass %+v, calls %d", res, w.fake.Calls())
	}
	w.clk.Advance(time.Minute)
	if rr := w.retry(); rr.Due != 0 {
		t.Fatalf("a fresh queued row was retried: %+v", rr)
	}
	w.clk.Advance(store.QueuedStaleAfter)
	rr := w.retry()
	if rr.Sent != 1 || w.fake.Calls() != 1 || strings.Count(w.fake.Sent()[0].Text, "[STALE]") != 2 {
		t.Fatalf("retry %+v, sent %v", rr, w.fake.Sent())
	}
	w.clk.Advance(store.QueuedStaleAfter)
	if rr := w.retry(); rr.Due != 0 || w.fake.Calls() != 1 {
		t.Fatalf("second retry %+v", rr)
	}
}

// A failed alert is retried as the state is now: what closed meanwhile is left out; when nothing is
// left the row is suppressed instead of sent.
func TestRetryRerendersFromTheStateNow(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 2)
	w.recipient("ops@example.test", true, false)
	w.fake.FailNext(1, &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassRefused, Temporary: true})
	w.set(state.StateFailed, site...)
	if res := w.pass(); res.Failed != 1 {
		t.Fatalf("pass %+v", res)
	}
	w.clk.Advance(30 * time.Second)
	if rr := w.retry(); rr.Due != 0 {
		t.Fatalf("retried before its time: %+v", rr)
	}
	// One device recovers before the retry.
	w.clk.Advance(30 * time.Second)
	w.set(state.StateOK, site[1])
	w.pass()
	rr := w.retry()
	sent := w.fake.Sent()
	if rr.Sent != 1 || len(sent) != 1 || !strings.Contains(sent[0].Text, w.host[site[0]]) || strings.Contains(sent[0].Text, w.host[site[1]]) ||
		sent[0].Subject != "[CryoBackup] 1 new alarm at 1 customer" {
		t.Fatalf("retry %+v, %v", rr, sent)
	}

	// A second batch fails, and every alarm of it closes before the retry: suppressed.
	w.clk.Advance(time.Minute)
	w.fake.FailNext(1, &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassTimeout, Temporary: true})
	w.set(state.StateStale, site[1])
	w.pass()
	w.clk.Advance(time.Minute)
	w.set(state.StateOK, site[1])
	w.pass()
	calls := w.fake.Calls()
	rr = w.retry()
	if rr.Suppressed != 1 || w.fake.Calls() != calls {
		t.Fatalf("retry of a closed batch %+v", rr)
	}
	var reason string
	for _, d := range w.deliveries() {
		if d.Result == store.DeliverySuppressed {
			reason = d.SuppressReason
		}
	}
	if reason != "deduped" {
		t.Fatalf("suppress reason %q", reason)
	}
	w.noLeaks()
}

// A retry for a recipient that was deleted, or switched alerts off, is suppressed.
func TestRetryForAGoneRecipientIsSuppressed(t *testing.T) {
	w := newWorld(t)
	site := w.site("Summit Eye Care", 1)
	r := w.recipient("ops@example.test", true, false)
	w.fake.FailWith(&mail.SendError{Stage: mail.StageConnect, Class: mail.ClassRefused, Temporary: true})
	w.set(state.StateFailed, site...)
	w.pass()
	if err := w.st.DeleteRecipient(w.ctx, store.ScopeAll(), r.ID, w.actor); err != nil {
		t.Fatal(err)
	}
	w.fake.Reset()
	w.clk.Advance(2 * time.Minute)
	if rr := w.retry(); rr.Suppressed != 1 || w.fake.Calls() != 0 {
		t.Fatalf("retry %+v", rr)
	}
}

// A test send and a kind with no renderer: the test is suppressed, the other left for its owner.
func TestRetryOtherKinds(t *testing.T) {
	w := newWorld(t)
	for _, in := range []store.DeliveryInput{
		{Kind: store.DeliveryTest, DedupeKey: "test:1", SubjectType: "console"},
		{Kind: store.DeliveryDigest, DedupeKey: "digest:2026-11-17:x", SubjectType: "console"},
	} {
		if _, _, err := w.st.EnqueueDelivery(w.ctx, store.ScopeAll(), in); err != nil {
			t.Fatal(err)
		}
	}
	w.clk.Advance(store.QueuedStaleAfter)
	rr := w.retry()
	if rr.Due != 2 || rr.Suppressed != 1 || rr.Left != 1 {
		t.Fatalf("retry %+v", rr)
	}
	// With a renderer for the digest kind, it is sent to the row's recipient.
	rec := w.recipient("ops@example.test", false, false)
	if _, _, err := w.st.EnqueueDelivery(w.ctx, store.ScopeAll(), store.DeliveryInput{Kind: store.DeliveryDigest, RuleID: rec.ID, DedupeKey: "digest:2026-11-18:" + rec.ID, SubjectType: "console"}); err != nil {
		t.Fatal(err)
	}
	w.r.Rerender = map[string]RerenderFunc{store.DeliveryDigest: func(_ context.Context, d store.Delivery, to store.Recipient) (mail.Message, string, error) {
		if d.RuleID == "" {
			return mail.Message{}, "deduped", nil
		}
		return mail.Message{To: []string{to.Address}, Subject: "digest", Text: "digest body"}, "", nil
	}}
	w.clk.Advance(store.QueuedStaleAfter)
	rr = w.retry()
	if rr.Sent != 1 || rr.Suppressed != 1 || w.fake.Sent()[0].To[0] != "ops@example.test" || w.fake.Sent()[0].Kind != mail.KindDigest {
		t.Fatalf("retry with a digest renderer %+v, %v", rr, w.fake.Sent())
	}
}

func TestBatchKeys(t *testing.T) {
	k := batchKey("alert", 1763413440000, "01RULE")
	at, rule, err := parseBatchKey(k, "alert")
	if err != nil || rule != "01RULE" || at.UnixMilli() != 1763413440000 {
		t.Fatalf("round trip %q: %v %q %v", k, at, rule, err)
	}
	for _, bad := range []string{"alert:12:x", "alert:abc:x", "alert:1763413440000:", "recovery:1763413440000:x", "digest:2026-11-18:x", "alert"} {
		if _, _, err := parseBatchKey(bad, "alert"); err == nil {
			t.Errorf("parseBatchKey(%q) accepted", bad)
		}
	}
}
