package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P20-W2A: serve sends through the relay of config.json and registers notification-retry beside
// the notifying alarm-evaluator.
func TestServeWiresTheNotifier(t *testing.T) {
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	names := strings.Join(app.Runner.Names(), ",")
	for _, want := range []string{jobs.NameAlarmEvaluator, jobs.NameNotificationRetry} {
		if !strings.Contains(names, want) {
			t.Errorf("job %s not registered: %s", want, names)
		}
	}
	if _, ok := app.Mail.(*mail.SMTPSender); !ok || app.Dispatch == nil || app.Dispatch.Sender != app.Mail {
		t.Fatalf("serve sender %T, dispatcher %+v", app.Mail, app.Dispatch)
	}
}

// Fixtures mode uses a FakeSender: nothing ever leaves a fixture console. A send through the
// dispatcher lands in the fake and in the delivery log.
func TestFixturesModeNeverSendsMail(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	cfg.SMTP = config.SMTP{Host: "relay.example.test", Port: 587, Security: "starttls", From: "console@example.test"}
	app, err := Build(context.Background(), cfg, BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	fake, ok := app.Mail.(*mail.FakeSender)
	if !ok || app.Dispatch.Sender != app.Mail {
		t.Fatalf("fixtures sender %T", app.Mail)
	}
	if strings.Contains(strings.Join(app.Runner.Names(), ","), jobs.NameNotificationRetry) {
		t.Fatal("fixtures mode registered notification-retry")
	}
	r, err := app.Dispatch.Send(context.Background(), storeAlert(), mail.Message{To: []string{"ops@example.test"}, Subject: "x", Text: "y"})
	if err != nil || !r.Sent || len(fake.Sent()) != 1 {
		t.Fatalf("fixture send %+v %v", r, err)
	}
}

func storeAlert() store.DeliveryInput {
	return store.DeliveryInput{Kind: store.DeliveryAlert, DedupeKey: "alert:fixture-test", SubjectType: "console"}
}
