package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/agentapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/server"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/console/ui"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// BuildOptions tune Build; the zero value is production.
type BuildOptions struct {
	// Clock replaces time.Now (tests inject one; fixtures mode freezes it).
	Clock func() time.Time
	// Backend replaces the admin-path client (tests use a fixtures.Backend).
	Backend adminpath.Backend
	// Log defaults to logx.Default().
	Log *logx.Logger
	// FixturesPassword is the fixture admin's password; empty generates one.
	FixturesPassword string
}

// App is a fully wired console: the store, the two route sets behind their
// listeners, the SSE hub and the job runner.
type App struct {
	Cfg      config.Config
	Store    *store.Store
	Master   *escrow.MasterKey
	Keys     *escrow.Keys
	Backend  adminpath.Backend
	Auth     *auth.Manager
	Server   *server.Server
	Hub      *ui.Hub
	Runner   *jobs.Runner
	Restores *restores.Service
	Agent    *agentapi.Service
	Admin    *adminapi.Service
	UI       *ui.Service
	Probe    *Probe
	// Endpoint is the self-probe of the public agent endpoint (ADR 0013, banners C04, C05, C04b).
	Endpoint *probe.Prober
	// Prober is the health-prober job (M3; nil in fixtures mode, which uses canned probe facts).
	Prober   *jobs.HealthProber
	Htpasswd *jobs.HtpasswdSync
	// Display is the live display zone and refresh interval (M2-W3A) shared by the UI and the API.
	Display *store.DisplayPrefs
	// FixturesPassword is the password of the fixtures admin ("admin").
	FixturesPassword string

	log     *logx.Logger
	clock   func() time.Time
	cleanup []func()
}

// Build wires the console from cfg. In fixtures mode (cfg.Fixtures) it opens a
// private in-memory store, loads the fictional fleet with a frozen clock, runs
// with an in-process fake admin path and starts no agent listener: the same
// code paths as production, no second one.
func Build(ctx context.Context, cfg config.Config, o BuildOptions) (*App, error) {
	a := &App{Cfg: cfg, log: o.Log, clock: o.Clock}
	if a.log == nil {
		a.log = logx.Default()
	}
	fix := cfg.Fixtures
	if a.clock == nil {
		a.clock = time.Now
		if fix {
			a.clock = fixtures.Clock()
		}
	}
	if _, err := time.LoadLocation(cfg.DisplayTimezone); err != nil {
		return nil, errors.New("cli: display_timezone is not an IANA zone")
	}

	var err2 error
	if fix {
		a.Store, err2 = store.OpenMemory(ctx, a.clock)
	} else {
		a.Store, err2 = store.Open(ctx, store.Options{Path: cfg.DBPath, Clock: a.clock})
	}
	if err2 != nil {
		return nil, fmt.Errorf("cli: cannot open the database: %w", sanitize(err2))
	}
	a.cleanup = append(a.cleanup, func() { _ = a.Store.Close() })

	// The master key. Without it the console still starts and says so (banner C12, 503 on enroll).
	switch {
	case fix:
		var k [32]byte
		_, _ = rand.Read(k[:])
		a.Master, _ = escrow.NewMasterKey(k[:])
	default:
		mk, err := escrow.LoadMasterKey(cfg.MasterKeyPath)
		if err != nil {
			a.log.Error("master key not loaded", logx.Class("master_key"))
		} else {
			a.Master = mk
		}
	}
	a.Keys = &escrow.Keys{Master: a.Master, Store: a.Store}

	// ADR 0008: restores stream and are never staged, so nothing of the restore
	// path writes to the artifacts directory; the restore-purge job only scrubs
	// archives an earlier version staged there. Fixtures mode has none.
	artifacts := cfg.ArtifactsDir
	if fix {
		artifacts = ""
	}

	switch {
	case o.Backend != nil:
		a.Backend = o.Backend
	case fix:
		a.Backend = &fixtures.Backend{}
	default:
		c := adminpath.NewClient(cfg.AdminpathSocket, adminpath.ClientOptions{})
		a.Backend = c
		a.cleanup = append(a.cleanup, c.Close)
	}

	if fix {
		pw := o.FixturesPassword
		if pw == "" {
			pw, _ = auth.GeneratePassword(16)
		}
		a.FixturesPassword = pw
		h, err := auth.HashPassword(pw)
		if err != nil {
			return nil, err
		}
		if err := store.LoadFixtures(ctx, a.Store, a.clock(), store.WithAdminPasswordHash(h)); err != nil {
			return nil, errors.New("cli: cannot load the fixtures")
		}
	}

	// M2-W3A, ADR 0012: the display zone and the refresh interval are settings an
	// owner can change. A value the operator stored (init, or /settings and the
	// API) is authoritative from then on; while a key has never been stored by the
	// operator, config.json supplies it. The migration's seed ("UTC") is nobody's
	// choice, so a fresh database follows the config (fixtures mode included).
	disp, derr := a.Store.LoadDisplayPrefs(ctx, cfg.DisplayTimezone, cfg.RefreshIntervalS)
	if derr != nil {
		a.log.Error("display settings not loaded; the config values are used", logx.Class("settings"))
	} else if n := disp.Note(); n != "" {
		a.log.Warn("stored display settings not used; the config values are used", logx.Class("settings"))
	}
	if len(disp.ConfigNotes()) > 0 { // one WARN; zone names and numbers only (a slash is logged as an underscore: logx tokens)
		snap := disp.Snapshot()
		a.log.Warn("the configured display settings differ from the ones saved in the console; the saved values win", logx.Class("settings"),
			logx.Str("config_zone", strings.ReplaceAll(cfg.DisplayTimezone, "/", "_")), logx.Str("saved_zone", strings.ReplaceAll(snap.Zone, "/", "_")),
			logx.Int("config_refresh_s", cfg.RefreshIntervalS), logx.Int("saved_refresh_s", snap.RefreshS))
	}
	a.Display = disp

	a.Endpoint = newEndpointProbe(cfg, a.clock, a.log)
	if !fix {
		a.Prober = newHealthProber(cfg, a)
		a.Probe = NewProbe(a.Prober.MaintainRPC, a.clock, false)
	} else {
		a.Probe = NewProbe(nil, a.clock, true)
	}
	fleet := &health.Evaluator{Store: a.Store, Clock: a.clock, MasterLoaded: a.Master.Loaded, Maintain: a.Probe.Status, Endpoint: a.Endpoint.Status,
		Probes: a.probeSource(fix), Refresh: func() time.Duration { return time.Duration(disp.RefreshS()) * time.Second }}
	idle := time.Duration(cfg.Session.IdleH) * time.Hour
	abs := time.Duration(cfg.Session.AbsoluteH) * time.Hour
	a.Auth = auth.NewManager(a.Store, idle, abs, cfg.RateLimits.LoginPerMinPerIP, cfg.RateLimits.LoginLockAfter,
		time.Duration(cfg.RateLimits.LoginLockMinutes)*time.Minute, a.clock)
	if cfg.PublicURL != "" {
		a.Auth.PublicHost = hostOf(cfg.PublicURL)
	}
	a.Auth.Master = a.Master // M2-W1A: TOTP secrets are wrapped under the master key
	a.Restores = &restores.Service{Store: a.Store, Backend: a.Backend, Master: a.Master, ArtifactsDir: artifacts, Clock: a.clock, Log: a.log,
		MinRateBytes: int64(cfg.RestoreStream.MinBytes), MinRateWindow: time.Duration(cfg.RestoreStream.WindowS) * time.Second,
		FirstByteTimeout: time.Duration(cfg.RestoreStream.FirstByteS) * time.Second}

	refresh := time.Duration(disp.RefreshS()) * time.Second
	a.Hub = ui.NewHub(refresh, a.log)
	a.Htpasswd = &jobs.HtpasswdSync{Store: a.Store, Master: a.Master, Dir: cfg.GatewayAuthDir, Sig: &jobs.Signal{}}
	a.Agent = &agentapi.Service{Store: a.Store, Keys: a.Keys, Backend: a.Backend, GatewayURL: cfg.GatewayURL, Clock: a.clock, Log: a.log, Pub: a.Hub,
		HtpasswdSync: a.Htpasswd.Sig.Notify, Limits: cfg.RateLimits, Notifier: agentapi.NewNotifier()}
	wake := a.Agent.Notifier.Notify // M2-W2C: a queued command wakes the device's open long poll
	a.Admin = &adminapi.Service{Auth: a.Auth, Store: a.Store, Fleet: fleet, Restores: a.Restores, Backend: a.Backend, Clock: a.clock, Log: a.log, Loc: disp.Loc(), Display: disp,
		PublicURL: cfg.PublicURL, ArtifactsDir: cfg.ArtifactsDir, WakeAgent: wake, SyncGateway: a.Htpasswd.Sig.Notify,
		Events: a.Hub, MaxEventStreams: cfg.RateLimits.EventStreamsMax, MaxEventStreamsPerPrincipal: cfg.RateLimits.EventStreamsPerAccount}
	u, err := ui.New(&ui.Service{Auth: a.Auth, Store: a.Store, Fleet: fleet, Restores: a.Restores, Backend: a.Backend, Hub: a.Hub, Clock: a.clock, Loc: disp.Loc(), Display: disp,
		Zone: disp.Zone(), Refresh: disp.RefreshS(), Fixtures: fix, Log: a.log, MasterLoaded: a.Master.Loaded, Maintain: a.Probe.Status,
		PublicURL: cfg.PublicURL, ArtifactsDir: cfg.ArtifactsDir, WakeAgent: wake, SyncGateway: a.Htpasswd.Sig.Notify})
	if err != nil {
		return nil, err
	}
	a.UI = u

	srv, err := server.New(cfg, server.Options{
		RegisterAgent: func(r httpx.Registrar) { a.Agent.Register(r) },
		RegisterAdmin: func(r httpx.Registrar) { a.Admin.Register(r); a.UI.Register(r) },
		AgentDisabled: fix,
		Health:        server.StoreHealth(a.Store, a.Master, fix),
		Log:           a.log,
		Clock:         a.clock,
	})
	if err != nil {
		return nil, err
	}
	a.Server = srv

	a.Runner = jobs.NewRunner(a.Store, holderID(), a.log)
	a.Admin.Jobs = a.Runner // run-now (M3-T3); the runner exists only after the server
	if a.Prober != nil {
		a.Admin.Prober = a.Prober // the storage probe runs the probes now and waits for them (M3-INT); never a typed nil
	}
	if !fix {
		a.Runner.Add(a.Htpasswd.Job())
		a.Runner.Add((&jobs.HeartbeatRingTrim{Store: a.Store}).Job())
		a.Runner.Add((&jobs.RestorePurge{Svc: a.Restores}).Job())
		a.Runner.Add((&jobs.SessionExpire{Store: a.Store}).Job())
		a.Runner.Add((&jobs.CommandExpiry{Store: a.Store}).Job())
		a.addMonitorJobs(cfg, fleet)
	}
	return a, nil
}

func holderID() string {
	h, _ := os.Hostname()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("serve-%x", b) + "-" + sanitizeToken(h)
}

// Run serves until ctx is cancelled: it starts the SSE hub, the job runner
// (the health-prober among its jobs) and the agent-endpoint self-probe, binds
// both listeners and serves. Close is called on return.
func (a *App) Run(ctx context.Context) error {
	defer a.Close()
	if err := a.Server.Listen(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Hub.Run(ctx) }()
	go func() { defer wg.Done(); a.Endpoint.Run(ctx) }() // after Listen: the first probe meets a bound listener
	a.Runner.Start(ctx)
	err := a.Server.Serve(ctx)
	a.Runner.Stop()
	if a.Prober != nil {
		a.Prober.Stop() // its probes run under its own lifetime, not the runner's ctx: Stop cancels them (bounded) and frees the probe lease
	}
	wg.Wait()
	return err
}

// Close releases everything Build opened. It is safe to call twice.
func (a *App) Close() {
	for i := len(a.cleanup) - 1; i >= 0; i-- {
		a.cleanup[i]()
	}
	a.cleanup = nil
}

// ---- maintenance worker reachability for the UI (banner C07) ----

// Probe reports the maintenance worker's reachability to the UI, read from the
// health-prober's maintain RPC probe (the same run that feeds banner C07; the
// prober pings the worker every period, so there is no second timer here).
type Probe struct {
	src   func() state.ProbeRun
	clock func() time.Time
	fixed bool
}

// NewProbe returns a Probe over src; with assumeUp (fixtures) it always reports reachable.
func NewProbe(src func() state.ProbeRun, clock func() time.Time, assumeUp bool) *Probe {
	return &Probe{src: src, clock: clock, fixed: assumeUp}
}

// Status is the newest maintain RPC probe: Known once one completed, Reachable when it passed.
func (p *Probe) Status() health.MaintainStatus {
	if p.fixed {
		return health.MaintainStatus{Known: true, Reachable: true, LastOK: p.clock()}
	}
	if p.src == nil {
		return health.MaintainStatus{}
	}
	r := p.src()
	st := health.MaintainStatus{Known: r.LastRunAt != 0, Reachable: r.LastRunAt != 0 && r.ConsecutiveFailures == 0}
	if t, ok := r.LastOKAt.Time(); ok {
		st.LastOK = t
	}
	return st
}
