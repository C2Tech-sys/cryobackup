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
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
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
	// FixtureMailWorld (fixtures mode only) adds the mail world of fixtures.SeedM4: recipients, a
	// delivery log of every outcome and an acknowledged alarm (P20-W3B). serve sets it; tests that
	// count the base fixture world leave it off.
	FixtureMailWorld bool
	// GatewaySyncWait bounds a retire's wait for the login to leave the gateway's file, in the admin API
	// and in the pages' in-process one. Zero is adminapi.DefaultGatewaySyncWait (5 s), which serve uses.
	GatewaySyncWait time.Duration
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
	// Mail sends the operator mail (ADR 0020): the relay of config.json, or a mail.FakeSender in
	// fixtures mode, so nothing ever leaves a fixture console. Dispatch sends through it and records
	// every message in the delivery log.
	Mail     mail.Sender
	Dispatch *notify.Dispatcher
	// Digest renders and sends the daily digest: the daily-digest job's work, and the send-now and
	// preview of the API (SendDigestNow, PreviewDigest). Nil in fixtures mode, which runs no jobs
	// (the API's preview and send-now then use a digester of their own over the fixture world).
	Digest *notify.Digester
	// TestMail sends the test message of ADR 0020 decision 11 and carries its per-console rate
	// limit: the admin API and the e-mail page share it.
	TestMail *notify.TestMailer
	// Notifier and Retrier are the alarm-evaluator's mail step and the notification-retry job's
	// re-sender (serve only; nil in fixtures mode). Held here so that a test can read the console
	// link every sender was given.
	Notifier *notify.Notifier
	Retrier  *notify.Retrier
	// FixturesPassword is the password of the fixtures admin ("admin").
	FixturesPassword string

	log      *logx.Logger
	clock    func() time.Time
	drill    bool // cfg.Drill outside fixtures mode: see drillJobs
	cleanup  []func()
	closeErr error // what closing the database answered; Run returns it
}

// Build wires the console from cfg. In fixtures mode (cfg.Fixtures) it opens a
// private in-memory store, loads the fictional fleet with a frozen clock, runs
// with an in-process fake admin path and starts no agent listener: the same
// code paths as production, no second one.
//
// In drill mode (cfg.Drill, a console restored from its self-backup, ADR 0029 ruling C8) it opens
// the real database and the admin listener as production does, and opens no agent listener, sends
// no mail (an in-process fake sender, mail not configured), offers no probe-now and schedules only
// the jobs drillJobs lists as on.
func Build(ctx context.Context, cfg config.Config, o BuildOptions) (*App, error) {
	a := &App{Cfg: cfg, log: o.Log, clock: o.Clock}
	if a.log == nil {
		a.log = logx.Default()
	}
	fix := cfg.Fixtures
	// drill: a console restored from its self-backup (ADR 0029, ruling C8). config.Validate refuses
	// drill with fixtures; a caller that skipped Validate gets fixtures, which touches nothing either.
	drill := cfg.Drill && !fix
	a.drill = drill
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
	a.cleanup = append(a.cleanup, func() {
		a.closeErr = a.Store.Close()
		if errors.Is(a.closeErr, store.ErrCloseAbandoned) {
			a.log.Error("database not closed cleanly: work still in flight; exiting, the next start recovers it", logx.Class("store_close_abandoned"))
		} else if a.closeErr != nil {
			a.log.Error("database not closed cleanly", logx.Class(jobs.ClassStore))
		}
	})

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
		if err := fixtures.SeedM3(ctx, a.Store, a.clock()); err != nil {
			return nil, errors.New("cli: cannot load the fixtures")
		}
		if o.FixtureMailWorld {
			if err := fixtures.SeedM4(ctx, a.Store, a.clock()); err != nil {
				return nil, errors.New("cli: cannot load the fixtures")
			}
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
	a.Mail, a.Dispatch = newMailer(cfg, fix, a)
	if drill {
		// A drill copy sends no mail. Every sender ends at an in-process fake whatever the smtp block
		// says, and mail counts as not configured, so the console offers no test message and no digest.
		a.Mail = mail.NewFakeSender()
		a.Dispatch = &notify.Dispatcher{Store: a.Store, Sender: a.Mail, From: cfg.SMTP.From, Log: a.log}
	}
	smtpSet := mail.SettingsFromConfig(cfg.SMTP)
	mailOn := fix || (smtpSet.Configured() && !drill) // fixtures: the FakeSender is the relay and nothing leaves the process
	// The link in every e-mail is admin_url (the address an operator types to open the console), never
	// public_url: that is the agent API and serves no console page. Unset: no link and no button.
	link := cfg.AdminLink()
	if mailOn && !fix && link == "" {
		a.log.Warn("admin url is not set; mail carries no console link", logx.Class("admin_url"))
	}
	a.TestMail = &notify.TestMailer{Dispatch: a.Dispatch, Configured: mailOn, Loc: disp.Loc, ConsoleURL: link, Log: a.log}
	// One MailService for the admin API and the pages' in-process API (P20-W3B): the same TestMailer,
	// so its one-test-every-10-s limit is the console's whichever of the two asks. Its Digest is set
	// below, once the digester exists; no request is served before Build returns.
	mailSvc := &adminapi.MailService{Test: a.TestMail, SMTP: smtpSet, Configured: mailOn, Fixtures: fix, AdminLink: link}

	a.Endpoint = newEndpointProbe(cfg, a.clock, a.log)
	if !fix {
		a.Prober = newHealthProber(cfg, a)
		a.Probe = NewProbe(a.Prober.MaintainRPC, a.clock, false)
	} else {
		a.Probe = NewProbe(nil, a.clock, true)
	}
	fleet := &health.Evaluator{Store: a.Store, Clock: a.clock, MasterLoaded: a.Master.Loaded, Maintain: a.Probe.Status, Endpoint: a.Endpoint.Status,
		Probes: a.probeSource(fix), Refresh: func() time.Duration { return time.Duration(disp.RefreshS()) * time.Second },
		PurgeBanner: true} // ADR 0019: "Purge not finished", in serve and in fixtures mode
	idle := time.Duration(cfg.Session.IdleH) * time.Hour
	abs := time.Duration(cfg.Session.AbsoluteH) * time.Hour
	a.Auth = auth.NewManager(a.Store, idle, abs, cfg.RateLimits.LoginPerMinPerIP, cfg.RateLimits.LoginLockAfter,
		time.Duration(cfg.RateLimits.LoginLockMinutes)*time.Minute, a.clock)
	if cfg.PublicURL != "" {
		a.Auth.PublicHost = hostOf(cfg.PublicURL)
	}
	// P21-T7: an Origin or Referer may name admin_url's host too ("" when admin_url is unset).
	a.Auth.AdminHost = cfg.AdminHost()
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
	if a.Prober != nil {
		a.Prober.Logins = a.Htpasswd // each tick reads the deployed login file (C17)
	}
	checkLogins := a.Htpasswd.Check
	if fix { // no gateway and no file in fixtures mode: the fictional file holds no retired login
		checkLogins = func(context.Context) jobs.LoginFileCheck { return jobs.LoginFileCheck{At: a.clock()} }
	}
	a.Admin = &adminapi.Service{Auth: a.Auth, Store: a.Store, Fleet: fleet, Restores: a.Restores, Backend: a.Backend, Clock: a.clock, Log: a.log, Loc: disp.Loc(), Display: disp,
		PublicURL: cfg.PublicURL, ArtifactsDir: cfg.ArtifactsDir, WakeAgent: wake, SyncGateway: a.Htpasswd.Sig.Notify, CheckGatewayLogins: checkLogins,
		Events: a.Hub, MaxEventStreams: cfg.RateLimits.EventStreamsMax, MaxEventStreamsPerPrincipal: cfg.RateLimits.EventStreamsPerAccount}
	a.Admin.GatewaySyncWait = o.GatewaySyncWait // zero is the 5 s default; the pages' API below gets the same value
	u, err := ui.New(&ui.Service{Auth: a.Auth, Store: a.Store, Fleet: fleet, Restores: a.Restores, Backend: a.Backend, Hub: a.Hub, Clock: a.clock, Loc: disp.Loc(), Display: disp,
		Zone: disp.Zone(), Refresh: disp.RefreshS(), Fixtures: fix, Drill: drill, Log: a.log, MasterLoaded: a.Master.Loaded, Maintain: a.Probe.Status,

		GatewaySyncWait: o.GatewaySyncWait,

		PublicURL: cfg.PublicURL, ArtifactsDir: cfg.ArtifactsDir, WakeAgent: wake, SyncGateway: a.Htpasswd.Sig.Notify, CheckGatewayLogins: checkLogins, Mail: mailSvc})
	if err != nil {
		return nil, err
	}
	a.UI = u

	proxies, err := config.ParseTrustedProxies(cfg.TrustedProxies) // validated at load; empty: the admin routes are registered unwrapped
	if err != nil {
		return nil, err
	}
	srv, err := server.New(cfg, server.Options{
		RegisterAgent: func(r httpx.Registrar) { a.Agent.Register(r) }, // never ForwardedFor: the agent listener believes no forwarded header
		RegisterAdmin: func(r httpx.Registrar) {
			r = httpx.ForwardedForRegistrar(r, proxies, a.log)
			a.Admin.Register(r)
			a.UI.Register(r)
		},
		AgentDisabled: cfg.AgentListenerOff(), // fixtures and drill: the agent port is never opened
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
	a.UI.Jobs = a.Runner    // the System page's Run now (M3-T4)
	if fix {
		a.Admin.Jobs = a.UI.PageJobs() // the fictional jobs the page offers, so the API and the page agree (the App's runner has none)
	}
	if a.Prober != nil && !drill {
		// The storage probe runs the probes now and waits for them (M3-INT); never a typed nil. A drill
		// copy has none: its probes would reach the production gateway and mail relay (drillJobs).
		a.Admin.Prober = a.Prober
	}
	if !fix {
		for _, j := range []jobs.Job{
			a.Htpasswd.Job(),
			(&jobs.HeartbeatRingTrim{Store: a.Store}).Job(),
			(&jobs.RestorePurge{Svc: a.Restores}).Job(),
			(&jobs.SessionExpire{Store: a.Store}).Job(),
			(&jobs.CommandExpiry{Store: a.Store}).Job(),
			(&jobs.DevicePurge{Store: a.Store, Purger: a.Backend, ArtifactsDir: artifacts, Log: a.log}).Job(), // ADR 0019
		} {
			if drill && !DrillRunsJob(j.Name) {
				continue
			}
			a.Runner.Add(j)
		}
		if !drill {
			a.addMonitorJobs(cfg, fleet) // every one of them is off in a drill copy (drillJobs)
		}
	}
	if drill {
		a.log.Warn("drill mode: this console is a restored copy; no agent listener, no mail, no job that pings outside or writes to storage",
			logx.Class("drill"), logx.Int("jobs", len(a.Runner.Names())))
	}
	// The M4 routes (P20-W3A). In fixtures mode no job runs and a.Digest stays nil, so the preview and
	// send-now get a digester of their own over the fixture world, sending through the FakeSender.
	digest := a.Digest
	if fix {
		digest = &notify.Digester{Store: a.Store, Dispatch: a.Dispatch, Configured: true, Loc: a.Display.Loc, ConsoleURL: link, Log: a.log,
			View: func(ctx context.Context) (state.FleetView, error) { f, err := fleet.Alarms(ctx); return f.View, err }}
	}
	if fix {
		// P20-W3B F2: no evaluator runs in fixtures mode, so record an open episode for every alarm the
		// fixture pages show, once, by the evaluator's own rule: the sync step of its notifier pass (with
		// mail switched off it syncs the episodes and sends nothing). Every Acknowledge a fixture page
		// offers then works. (It lives here, not in package fixtures: notify's tests import fixtures.)
		if err := syncFixtureEpisodes(ctx, a, fleet); err != nil {
			return nil, errors.New("cli: cannot load the fixtures")
		}
	}
	a.Admin.Mail = mailSvc // the pages' in-process API holds the same one
	if digest != nil {
		a.Admin.Mail.Digest = digest // never a typed nil in the interface
	}
	return a, nil
}

// DrillJob is one job serve schedules and whether a drill copy runs it.
type DrillJob struct {
	Name string
	On   bool
	Why  string
}

// drillJobs lists every job serve schedules, whether a drill copy (config "drill", ADR 0029 ruling
// C8) runs it, and why. A drill copy may point at the production storage key and the production
// mail relay, so it runs only what touches nothing but its own database and its own disk. A job
// that is not listed here does not run in a drill copy (DrillRunsJob): a job added to serve later
// stays off there until someone decides otherwise in this table. Not a job, and off as well: the
// self-probe of the public agent endpoint (it would connect to the production agent listener).
var drillJobs = []DrillJob{
	{jobs.NameHtpasswdSync, false, "writes the gateway's login file; a drill copy has no gateway and must not touch the real one's"},
	{jobs.NameHeartbeatRingTrim, true, "deletes old heartbeat rows in the drill copy's own database"},
	{jobs.NameRestorePurge, true, "expires spent restores in the drill copy's own database and scrubs only its own artifacts directory"},
	{jobs.NameSessionExpire, true, "deletes expired sign-in sessions in the drill copy's own database"},
	{jobs.NameCommandExpiry, true, "expires queued agent commands in the drill copy's own database; no agent can fetch them"},
	{jobs.NameDevicePurge, false, "deletes retired devices' repositories from storage (purge_repo)"},
	{jobs.NameHealthProber, false, "connects to the production gateway and mail relay, and its storage probe may create the probe repository"},
	{jobs.NameAlarmEvaluator, false, "its notify step sends alert mail"},
	{jobs.NameDailyDigest, false, "sends the daily digest"},
	{jobs.NameNotificationRetry, false, "sends failed mail again"},
	{jobs.NameAbandonedRunSweeper, false, "no agent can report to a drill copy, so it would mark open runs abandoned because of the drill itself"},
	{jobs.NameHealthcheckPing, false, "pings the external heartbeat monitor"},
}

// DrillJobs returns the drill job table (a copy), for the tests and the record.
func DrillJobs() []DrillJob { return append([]DrillJob(nil), drillJobs...) }

// DrillRunsJob reports whether a drill copy runs the job named name: false for every job the
// table does not list as on, unknown names included.
func DrillRunsJob(name string) bool {
	for _, j := range drillJobs {
		if j.Name == name {
			return j.On
		}
	}
	return false
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
//
// It returns non-nil, so serve exits non-zero and its supervisor restarts it, when
// a listener fails (logged by class: it must not stay up serving nothing and running
// no jobs) or when the database could not be closed cleanly.
func (a *App) Run(parent context.Context) (err error) {
	defer func() {
		a.Close()
		err = errors.Join(err, a.closeErr)
	}()
	if err := a.Server.Listen(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent) // ends with parent, or when the servers end on their own
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.Hub.Run(ctx) }()
	if !a.drill { // a drill copy never connects to the production agent endpoint (drillJobs)
		wg.Add(1)
		go func() { defer wg.Done(); a.Endpoint.Run(ctx) }() // after Listen: the first probe meets a bound listener
	}
	a.Runner.Start(ctx)
	// Every step below is bounded: shutdownWorstCase is the sum. Serve returns when ctx
	// ends or when a listener fails, which it reports as its error. Either way ctx ends
	// here, so the Hub and the self-probe below stop too.
	err = a.Server.Serve(ctx)
	stopBegan := time.Now()
	if err != nil {
		a.log.Error("the console is shutting down after a listener failure", logx.Class("listener_failed"))
		err = fmt.Errorf("cli: a listener failed: %w", err)
	} else {
		// A clean stop says so in the console's own log, not only in systemd's (M3 lab, L8).
		a.log.Info("the console is stopping: shutdown was requested", logx.Class("shutdown"))
	}
	cancel()
	a.Runner.Stop()
	a.Admin.BeginShutdown() // no new run-now or POST probe; the manual job runs in flight are cancelled
	a.UI.BeginShutdown()    // the same for the pages' private admin API (M3-T4)
	if a.Prober != nil {
		a.Prober.Stop() // its probes run under its own lifetime, not the runner's ctx: Stop cancels them (bounded) and frees the probe lease
	}
	// Before Close (deferred above): a POST probe that outlived the server's grace writes its audit row now.
	a.Admin.WaitBackground(backgroundShutdownWait)
	a.UI.WaitBackground(backgroundShutdownWait)
	wg.Wait()
	a.log.Info("the console has stopped", logx.Class("shutdown"), logx.Dur("took_ms", time.Since(stopBegan)))
	return err
}

// backgroundShutdownWait bounds how long Run waits for manual job runs and POST
// probes after cancelling them. One that ignores its cancellation is abandoned and
// logged. If it is not inside a store call it then meets a closed store; if it is
// stuck inside a Write, Store.Close gives up after its own bound and the process
// exits with that transaction unfinished. Either way it cannot hold the shutdown.
const backgroundShutdownWait = 10 * time.Second

// shutdownWorstCase is the longest Run takes to return once its context ends or a
// listener fails, with every step using its whole bound, each one after the last.
// The Hub and the endpoint self-probe take the context and are not counted.
const shutdownWorstCase = server.ShutdownGrace + // the HTTP server drains
	jobs.RunnerStopWait + // the scheduled jobs stop
	jobs.StopWait + jobs.StopReleaseTimeout + // the prober stops and frees its lease
	backgroundShutdownWait + // manual job runs and POST probes end
	backgroundShutdownWait + // the same for the pages' private admin API
	store.CloseWait // the database closes (65 s in all)

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

// syncFixtureEpisodes records the open episodes of the fixture fleet as one evaluator pass would.
func syncFixtureEpisodes(ctx context.Context, a *App, fleet *health.Evaluator) error {
	f, err := fleet.Alarms(ctx)
	if err != nil {
		return err
	}
	_, err = (&notify.Notifier{Store: a.Store, Log: a.log}).Pass(ctx, f.View)
	return err
}
