package cli

import (
	"context"
	"crypto/x509"
	"net/url"
	"os"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The M3 monitoring wiring of serve: the health-prober, the alarm-evaluator
// loop, the abandoned-run sweeper and the optional healthcheck ping
// (architecture §5.7), and the World's probe facts (health.Evaluator.Probes).
// M4 (ADR 0020): the operator mail sender, the evaluator's notify step, the
// daily-digest job and the notification-retry job.

// fixturesFrom is the sender address of a fixture console without smtp.from: its FakeSender has no
// settings to take one from, and nothing it accepts leaves the process.
const fixturesFrom = "CryoBackup <console@fixtures.example.test>"

// newMailer builds the operator mail sender and its dispatcher: the relay of the smtp block in
// serve (a console without one still starts; every send then says not_configured), a FakeSender in
// fixtures mode.
func newMailer(cfg config.Config, fix bool, a *App) (mail.Sender, *notify.Dispatcher) {
	from := cfg.SMTP.From
	var s mail.Sender
	if fix {
		s = mail.NewFakeSender()
		if from == "" {
			from = fixturesFrom
		}
	} else {
		set := mail.SettingsFromConfig(cfg.SMTP)
		set.HeloName, set.Log = heloName(cfg.PublicURL), a.log
		s = mail.NewSMTPSender(set)
	}
	return s, &notify.Dispatcher{Store: a.Store, Sender: s, From: from, Log: a.log}
}

// gatewayCAUnusable is the not-checked reason of a gateway CA file that cannot be used.
const gatewayCAUnusable = "health_prober.gateway_ca_file cannot be read or holds no certificate"

// newHealthProber builds the health-prober from the configuration. It never
// fails: a probe that cannot run is a prober whose facts say so.
func newHealthProber(cfg config.Config, a *App) *jobs.HealthProber {
	hp := &jobs.HealthProber{
		Store: a.Store, Backend: a.Backend, Master: a.Master, Endpoint: a.Endpoint.Status,
		Interval: time.Duration(cfg.HealthProber.IntervalS) * time.Second, Clock: a.clock, Log: a.log, Started: a.clock().UTC(),
		Gateway: probe.GatewaySettings{URL: cfg.GatewayURL},
		SMTP:    probe.SMTPSettings{Host: cfg.SMTP.Host, Port: cfg.SMTP.Port, Security: cfg.SMTP.Security, HeloName: heloName(cfg.PublicURL)},
	}
	if f := cfg.HealthProber.GatewayCAFile; f != "" {
		pem, err := os.ReadFile(f)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			hp.GatewayUnusable = gatewayCAUnusable
		} else {
			hp.Gateway.Roots = pool
		}
	}
	return hp
}

// heloName is the EHLO name of the SMTP probe: the console's public host name,
// else "localhost".
func heloName(publicURL string) string {
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "localhost"
}

// probeSource is the World's probe source: the health-prober's facts in
// production, healthy canned facts in fixtures mode.
func (a *App) probeSource(fix bool) func() state.ProbeFacts {
	if fix {
		return health.FixtureProbes(a.clock)
	}
	return a.Prober.Facts
}

// addMonitorJobs registers the M3 monitoring jobs (serve only, not fixtures).
func (a *App) addMonitorJobs(cfg config.Config, fleet *health.Evaluator) {
	a.Prober.Holder = a.Runner.Holder // its probe lease names this process like every other lease
	a.Runner.Add(a.Prober.Job())
	view := func(ctx context.Context) (state.FleetView, error) {
		f, err := fleet.Alarms(ctx)
		return f.View, err
	}
	n := &notify.Notifier{Store: a.Store, Dispatch: a.Dispatch, Configured: mail.SettingsFromConfig(cfg.SMTP).Configured(),
		Loc: a.Display.Loc, ConsoleURL: cfg.PublicURL, Log: a.log}
	a.Runner.Add((&jobs.AlarmEvaluator{Store: a.Store, View: view, Notify: jobs.NotifyStep(n)}).Job())
	// The daily digest (ADR 0020 decision 8): its job, and its re-render for the retry job.
	a.Digest = &notify.Digester{Store: a.Store, Dispatch: a.Dispatch, View: view, Configured: mail.SettingsFromConfig(cfg.SMTP).Configured(),
		Loc: a.Display.Loc, ConsoleURL: cfg.PublicURL, Log: a.log}
	a.Runner.Add((&jobs.DailyDigest{Digester: a.Digest}).Job())
	a.Runner.Add((&jobs.NotificationRetry{Retrier: &notify.Retrier{Store: a.Store, Dispatch: a.Dispatch, View: view,
		Loc: a.Display.Loc, ConsoleURL: cfg.PublicURL, Log: a.log,
		Rerender: map[string]notify.RerenderFunc{store.DeliveryDigest: a.Digest.Rerender}}}).Job())
	a.Runner.Add((&jobs.AbandonedRunSweeper{Store: a.Store}).Job())
	// The outbound heartbeat ping (ADR 0020 decision 12): registered only with a URL; its facts feed
	// banner C19 through the prober either way (no URL: not configured, nothing raised).
	hp := cfg.HealthcheckPing
	ping := jobs.NewHeartbeatPing(a.Store, hp.URL, hp.Method, time.Duration(hp.IntervalS)*time.Second, a.clock().UTC())
	a.Prober.Heartbeat = ping.Facts
	jobs.RegisterHeartbeatPing(a.Runner, ping)
}
