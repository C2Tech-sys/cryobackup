package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

type m4Clock struct{ t time.Time }

func (c *m4Clock) now() time.Time { return c.t }

// m4Rig is the fixture world with the real evaluator, a FakeSender, the notifier and the retrier.
type m4Rig struct {
	st    *store.Store
	clk   *m4Clock
	fake  *mail.FakeSender
	eval  *AlarmEvaluator
	retry *NotificationRetry
	n     *notify.Notifier
	logs  *strings.Builder
}

func newM4Rig(t *testing.T) *m4Rig {
	t.Helper()
	ctx := context.Background()
	clk := &m4Clock{t: fixtures.Now()}
	st, err := store.OpenMemory(ctx, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, clk.t); err != nil {
		t.Fatal(err)
	}
	fleet := &health.Evaluator{Store: st, Clock: clk.now, MasterLoaded: func() bool { return true }}
	view := func(ctx context.Context) (state.FleetView, error) {
		f, err := fleet.Alarms(ctx)
		return f.View, err
	}
	logs := &strings.Builder{}
	log := logx.New(logs, logx.LevelInfo)
	fake := mail.NewFakeSender()
	d := &notify.Dispatcher{Store: st, Sender: fake, From: "console@example.test", Log: log}
	n := &notify.Notifier{Store: st, Dispatch: d, Configured: true, Log: log}
	return &m4Rig{st: st, clk: clk, fake: fake, n: n, logs: logs,
		eval:  &AlarmEvaluator{Store: st, View: view, Notify: NotifyStep(n)},
		retry: &NotificationRetry{Retrier: &notify.Retrier{Store: st, Dispatch: d, View: view, Log: log}},
	}
}

func (r *m4Rig) okStamp(t *testing.T) *time.Time {
	t.Helper()
	j, err := r.st.GetJobState(context.Background(), NameAlarmEvaluatorOK)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return j.LastFinishedAt
}

// The evaluator pass notifies: over the fixture world (which has critical devices) one message per
// recipient, the episodes synced, the pass stamped ok. A second pass sends nothing.
func TestC06AlarmEvaluatorNotifies(t *testing.T) {
	r := newM4Rig(t)
	ctx := context.Background()
	actor := store.AuditActor{Username: "root", Method: "api"}
	for _, a := range []string{"ops@example.test", "oncall@example.test"} {
		if _, err := r.st.CreateRecipient(ctx, store.ScopeAll(), store.RecipientInput{Name: a, Address: a, Alerts: true}, actor); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.eval.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if r.fake.Calls() != 2 || r.okStamp(t) == nil {
		t.Fatalf("first pass: %d messages, ok stamp %v", r.fake.Calls(), r.okStamp(t))
	}
	open, err := r.st.ListOpenEpisodes(ctx, store.ScopeAll())
	if err != nil || len(open) == 0 {
		t.Fatalf("no episode opened: %v", err)
	}
	r.clk.t = r.clk.t.Add(time.Minute)
	if err := r.eval.Run(ctx); err != nil || r.fake.Calls() != 2 {
		t.Fatalf("second pass: %v, %d messages", err, r.fake.Calls())
	}
}

// Mail trouble never fails the pass: episodes are synced, the failure is in the delivery log, and
// notification-retry sends it a minute later.
func TestC06MailTroubleDoesNotFailThePass(t *testing.T) {
	r := newM4Rig(t)
	ctx := context.Background()
	if _, err := r.st.CreateRecipient(ctx, store.ScopeAll(), store.RecipientInput{Name: "ops", Address: "ops@example.test", Alerts: true},
		store.AuditActor{Username: "root", Method: "api"}); err != nil {
		t.Fatal(err)
	}
	r.fake.FailNext(1, &mail.SendError{Stage: mail.StageConnect, Class: mail.ClassRefused, Temporary: true})
	if err := r.eval.Run(ctx); err != nil {
		t.Fatalf("mail trouble failed the pass: %v", err)
	}
	if r.okStamp(t) == nil {
		t.Fatal("a pass with mail trouble is not a successful evaluation")
	}
	sf, err := r.st.LoadSendFacts(ctx, store.ScopeAll())
	if err != nil || sf.ConsecutiveFailures != 1 || sf.PendingAlerts != 1 {
		t.Fatalf("send facts %+v %v", sf, err)
	}
	r.clk.t = r.clk.t.Add(time.Minute)
	if err := r.retry.Job().Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.fake.Sent()) != 1 {
		t.Fatalf("the retry sent %d", len(r.fake.Sent()))
	}
	if sf, _ := r.st.LoadSendFacts(ctx, store.ScopeAll()); sf.ConsecutiveFailures != 0 || sf.PendingAlerts != 0 {
		t.Fatalf("after the retry: %+v", sf)
	}
	for _, host := range []string{"FRONTDESK", "@"} {
		if strings.Contains(r.logs.String(), host) {
			t.Fatalf("a log line carries %q:\n%s", host, r.logs.String())
		}
	}
}

// A store error in the notify step fails the pass, and the pass is not stamped ok (C06 then rises).
func TestC06NotifyStoreErrorFailsThePass(t *testing.T) {
	r := newM4Rig(t)
	boom := errors.New("store down")
	r.eval.Notify = func(context.Context, state.FleetView) error { return boom }
	if err := r.eval.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("run = %v", err)
	}
	if r.okStamp(t) != nil {
		t.Fatal("a failed notify step stamped the evaluation ok")
	}
	r.eval.View = func(context.Context) (state.FleetView, error) { return state.FleetView{}, boom }
	r.eval.Notify = func(context.Context, state.FleetView) error { t.Fatal("notify ran without a view"); return nil }
	if err := r.eval.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("run with a failing view = %v", err)
	}
}

// The job schedules: notification-retry every minute.
func TestNotificationRetryJob(t *testing.T) {
	j := (&NotificationRetry{}).Job()
	if j.Name != "notification-retry" || j.Every != time.Minute || j.Lease < notify.DefaultSendBudget {
		t.Fatalf("job %+v", j)
	}
}

// The prober's SMTP facts come from the delivery log and the recipients: C03 counts real sends and
// C18 warns when a configured relay has no alert recipient.
func TestC09ProberMailFacts(t *testing.T) {
	ctx := context.Background()
	clk := &m4Clock{t: fixtures.Now()}
	st, err := store.OpenMemory(ctx, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &HealthProber{Store: st, Clock: clk.now, SMTP: probe.SMTPSettings{Host: "relay.example.test", Port: 587, Security: "starttls"}}
	run := state.ProbeRun{LastRunAt: state.Millis(clk.t.UnixMilli()), LastOKAt: state.Millis(clk.t.UnixMilli())}

	smtp, digest := h.mailFacts(run)
	if !smtp.NoAlertRecipient || smtp.NotRead != "" || smtp.Probe != run || digest == nil || !digest.NotInBuild {
		t.Fatalf("no recipient: %+v %+v", smtp, digest)
	}
	actor := store.AuditActor{Username: "root", Method: "api"}
	if _, err := st.CreateRecipient(ctx, store.ScopeAll(), store.RecipientInput{Name: "d", Address: "digest@example.test", Digest: true}, actor); err != nil {
		t.Fatal(err)
	}
	if smtp, _ := h.mailFacts(run); !smtp.NoAlertRecipient {
		t.Fatal("a digest-only recipient counts as an alert recipient")
	}
	if _, err := st.CreateRecipient(ctx, store.ScopeAll(), store.RecipientInput{Name: "o", Address: "ops@example.test", Alerts: true}, actor); err != nil {
		t.Fatal(err)
	}
	// Three failed sends in a row: C03 from the delivery log.
	for i := 0; i < 3; i++ {
		row, _, err := st.EnqueueDelivery(ctx, store.ScopeAll(), store.DeliveryInput{Kind: store.DeliveryAlert, DedupeKey: "alert:" + string(rune('a'+i)), SubjectType: "console"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.MarkDeliveryFailed(ctx, store.ScopeAll(), row.ID, "auth_rejected", nil); err != nil {
			t.Fatal(err)
		}
	}
	smtp, _ = h.mailFacts(run)
	if smtp.NoAlertRecipient || smtp.ConsecutiveSendFailures != 3 {
		t.Fatalf("with an alert recipient and three failures: %+v", smtp)
	}
	// Unconfigured relay: the recipients are not judged (C03b says it).
	if smtp, _ := (&HealthProber{Store: st}).mailFacts(state.ProbeRun{Unconfigured: "SMTP is not configured"}); smtp.NoAlertRecipient {
		t.Fatal("NoAlertRecipient judged without a relay")
	}
	// A store that cannot be read: not checked, never healthy.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	h.SMTP = probe.SMTPSettings{Host: "relay.example.test", Port: 587}
	if smtp, _ := h.mailFacts(run); smtp.NotRead == "" {
		t.Fatalf("closed store: %+v", smtp)
	}
	if smtp, _ := (&HealthProber{}).mailFacts(run); smtp.NotRead == "" {
		t.Fatal("no store: the facts read as healthy")
	}
}
