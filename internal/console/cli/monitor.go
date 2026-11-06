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
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The M3 monitoring wiring of serve: the health-prober, the alarm-evaluator
// loop, the abandoned-run sweeper and the optional healthcheck ping
// (architecture §5.7), and the World's probe facts (health.Evaluator.Probes).

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
	a.Runner.Add((&jobs.AlarmEvaluator{Store: a.Store, Evaluate: func(ctx context.Context) error {
		_, err := fleet.Alarms(ctx)
		return err
	}}).Job())
	a.Runner.Add((&jobs.AbandonedRunSweeper{Store: a.Store}).Job())
	if hp := cfg.HealthcheckPing; hp.URL != "" {
		a.Runner.Add((&jobs.HealthcheckPing{Store: a.Store, URL: hp.URL, Method: hp.Method,
			Every: time.Duration(hp.IntervalS) * time.Second, Started: a.clock().UTC()}).Job())
	}
}
