package cli

import (
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// FixturesEndpointHost is the fictional agent address fixtures mode reports
// when public_url is unset.
const FixturesEndpointHost = "agents.example.com:8443"

// FixturesCertDays is the remaining lifetime of the fixtures-mode agent
// certificate: inside the C05 warning window, so the board shows the banner.
const FixturesCertDays = 12

// newEndpointProbe builds the self-probe of the public agent endpoint (ADR
// 0013). Fixtures mode has no agent listener and no network: it reports a
// canned healthy probe whose certificate expires in FixturesCertDays days, so
// the board renders an ADR 0013 banner (C05) from the same code path.
func newEndpointProbe(cfg config.Config, clock func() time.Time, log *logx.Logger) *probe.Prober {
	if cfg.Fixtures {
		now := clock().UTC()
		host := hostOf(cfg.PublicURL)
		if host == "" {
			host = FixturesEndpointHost
		}
		return probe.Fixed(probe.Status{Runs: true, Host: host, Interval: probe.DefaultInterval, Started: now, Known: true, LastOK: now,
			Last: probe.Result{At: now, OK: true, NotAfter: now.Add(FixturesCertDays*24*time.Hour + time.Hour)}})
	}
	return probe.New(probe.Settings{PublicURL: cfg.PublicURL, CAFile: cfg.AgentProbe.CAFile, ConnectAddress: cfg.AgentProbe.ConnectAddress,
		Interval: time.Duration(cfg.AgentProbe.IntervalS) * time.Second}, clock, log)
}
