package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/commands"
	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/logs"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/agent/secrets"
	"github.com/c2tech-sys/cryobackup/internal/agent/service"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/ulid"
)

// PinnedResticSHA256 is the SHA-256 (lower-case hex) of the restic.exe this
// build ships with, injected at build time:
//
//	-ldflags "-X github.com/c2tech-sys/cryobackup/internal/agent.PinnedResticSHA256=<hex>"
//
// It is compared at every start (architecture §6.1 "restic supervisor"). A
// build without it refuses to run restic unless the operator passes the lab
// flag --insecure-unpinned-restic or an explicit --restic-sha256.
var PinnedResticSHA256 string

// eventServiceConfigRepaired is the Event Log id (warning) of a service start
// that found its own SCM configuration drifted and re-applied it (M1-T4).
const eventServiceConfigRepaired = 1009

// ensureServiceConfig is service.EnsureConfig (a variable for the tests).
var ensureServiceConfig = service.EnsureConfig

// Errors of the boot sequence.
var (
	ErrNotEnrolled = errors.New("agent: this device is not enrolled; run \"cryo-agent enroll\" first")
	ErrACLRefused  = errors.New("agent: the state directory could not be restricted to SYSTEM and Administrators")
)

// Options configures New.
type Options struct {
	// StateDir is the state directory; empty means config.DefaultStateDir().
	StateDir string
	// ConsoleURL overrides agent.json's server_url for this run.
	ConsoleURL string
	// ResticPath is restic.exe; empty means restic.exe next to the agent binary
	// (C:\Program Files\CryoBackup\ in the MSI layout).
	ResticPath string
	// ResticSHA256 overrides PinnedResticSHA256 (lab and tests).
	ResticSHA256 string
	// AllowUnpinnedRestic lets restic run without a pinned hash (lab only).
	AllowUnpinnedRestic bool
	// InsecurePlaintextSecrets selects the plaintext secret store on platforms
	// without DPAPI (CI only).
	InsecurePlaintextSecrets bool
	// AllowUnprotectedState is the lab flag --insecure-unprotected-state
	// (default false): outside service mode, a state directory that is not
	// verified as SYSTEM + Administrators only (an unelevated run, a platform
	// without ACLs) is used with a loud warning on every start instead of
	// refused, and secrets may be written into it. Service mode ignores it.
	AllowUnprotectedState bool
	// RepairACL is --repair-acl (default false): an elevated console-mode run
	// moves an untrusted state directory aside whole (to
	// <name>.quarantine-<UTC stamp>-<8 random hex> next to it), creates a
	// fresh one and clears the untrusted verdict (audited in the fresh
	// logs\agent.log and Event Log 1007). agent.json and
	// the secrets go with the quarantine, so New then refuses with
	// ErrNotEnrolled until an elevated enroll. Ignored in service mode.
	RepairACL bool
	// OneShot marks a short-lived command (check-in) that may run next to the
	// service: it must not close "stale" runs, empty tmp or prune logs, because
	// the open run it sees may belong to the live service.
	OneShot bool
	// ServiceMode is true when the SCM started the process: ACL failures refuse
	// to start and restic gets CREATE_NO_WINDOW.
	ServiceMode bool
	// Clock is the device clock; nil means time.Now.
	Clock func() time.Time
	// Logger replaces the device-local file logger (tests).
	Logger *slog.Logger
	// EventLog mirrors Info, Warning and Error lines to the Windows Event Log.
	EventLog *service.EventLog

	// Test hooks (unexported: only this package's tests set them).
	environment       func() policy.Environment
	insecureLocalRepo func(deviceID string) string
	httpClient        *http.Client
	baseEnv           []string
	heartbeatEvery    time.Duration
	heartbeatMinGap   time.Duration
	pollWait          time.Duration
	schedTick         time.Duration
	progressEvery     time.Duration
	cancelGrace       time.Duration
	backoffMin        time.Duration
	verifyState       func(root string) error
	leaseTimeout      time.Duration                                    // 0 = api.StartLeaseTimeout
	leaseWait         func(retry time.Duration) time.Duration          // nil = retry plus 0 to 20 % jitter
	resumeWait        time.Duration                                    // 0 = resumeNetWait (2 min)
	resumeProbe       time.Duration                                    // 0 = resumeProbeEvery
	resumeTimer       func(d time.Duration) (<-chan time.Time, func()) // nil = a real timer; tests drive the resume wait with it
	gatewayAddr       string                                           // "" = the host:port of the repository URL (gateway_watchdog.go)
	gatewayProbeEvery time.Duration                                    // 0 = gatewayProbeEvery
}

// Agent is the endpoint: config, secrets, journal, uplink, scheduler,
// supervisor and command executor wired together (architecture §6.1).
type Agent struct {
	o      Options
	layout config.Layout
	cfg    config.Config
	log    *slog.Logger
	clock  func() time.Time
	ids    *ulid.Generator
	host   hostFacts
	boot   time.Time

	closers []io.Closer
	jr      *journal.Journal
	sec     *secrets.Store
	up      *uplink.Client
	sup     *supervisor.Supervisor
	exec    *commands.Executor
	sched   *scheduler.Loop
	events  *service.EventLog

	secMu       sync.Mutex
	deviceToken string
	gatewayPW   string
	repoPW      string

	mu          sync.Mutex
	pol         *api.PolicyResponse
	polETag     string
	running     *activeRun
	forceNow    bool
	forceAt     time.Time
	bootPending bool
	resticVer   string
	resticErr   error
	polParseErr bool
	repoMissing bool
	gwAck       bool
	stopping    bool
	hbLast      time.Time
	// hbOKAt is the last heartbeat the console answered; consoleReached is
	// kicked with it (resume_wait.go).
	hbOKAt time.Time
	// gwStalled: the running backup is not moving and the gateway cannot be
	// reached (gateway_watchdog.go); the heartbeat reports it.
	gwStalled bool
	// Start lease (lease.go, ADR 0005): no scheduled start before
	// leaseNotBefore; leaseCapped says the console's fleet cap refused (the
	// heartbeat reports deferred/fleet_cap meanwhile); leaseBO is the backoff
	// after a refused call (4xx).
	leaseNotBefore time.Time
	leaseCapped    bool
	leaseBO        *uplink.Backoff
	// stateVerified: the state root read back as config.SDDLStateDir at start
	// (re-checked before every heartbeat). stateErr: it no longer does.
	stateVerified bool
	stateErr      error

	baseCtx context.Context
	runWG   sync.WaitGroup

	// HandleEvent state (events_power.go): schedUp is closed by the first
	// facts() call made after Run has set a.sched (the heartbeat's or the
	// scheduler's, whichever runs first; markSchedulerUp refuses while a.sched
	// is nil), which publishes a.sched to the service's control goroutine;
	// noRunOnWake mirrors the effective policy's run_on_wake so the handler
	// needs no lock.
	schedUp     chan struct{}
	schedUpOnce sync.Once
	noRunOnWake atomic.Bool

	kickHeartbeat chan struct{}
	kickPolicy    chan struct{}
	kickOutbox    chan struct{}
	kickCommands  chan struct{}

	consoleReached chan struct{}

	// planned is the last value recordPlan wrote to KVPlannedStart ("" =
	// none); only the scheduler's goroutine touches it, and Run after the
	// loops have ended.
	planned string
}

// activeRun is the backup in progress.
type activeRun struct {
	id     string
	cancel context.CancelCauseFunc
	start  time.Time
}

// New runs the boot sequence of architecture §6.1 up to, not including,
// starting the loops: validate and create the layout and its ACLs, open the log,
// load agent.json and the secrets, verify the restic binary, open the journal
// and close stale runs, load the last-known-good policy, build the uplink. An
// error here is a refusal to start; conditions the agent can report to the
// console (a restic binary that fails verification, a missing repository key,
// no policy yet) do not stop it: it keeps the uplink alive, reports
// last_error_class and does not schedule.
func New(ctx context.Context, o Options) (*Agent, error) {
	if o.StateDir == "" {
		o.StateDir = config.DefaultStateDir()
	}
	a := &Agent{
		o: o, layout: config.Layout{Root: o.StateDir}, clock: o.Clock, sup: supervisor.New(),
		events: o.EventLog, bootPending: true,
		kickHeartbeat: make(chan struct{}, 1), kickPolicy: make(chan struct{}, 1),
		kickOutbox: make(chan struct{}, 1), kickCommands: make(chan struct{}, 1),
		consoleReached: make(chan struct{}, 1),
		schedUp:        make(chan struct{}),
	}
	if a.clock == nil {
		a.clock = time.Now
	}
	a.boot = a.clock()
	a.ids = ulid.NewGenerator(a.clock, nil)
	a.leaseBO = a.backoff()

	// 1. layout, owner and ACLs, every layout directory verified before and
	// after: refuse, quarantine (--repair-acl, elevated console mode), or (lab
	// flag, console mode, unelevated only) warn. In service mode an unverified
	// state directory is a refusal written to the Event Log: agent.json, the
	// journal and the cache in it are not trusted.
	st, err := PrepareState(o.StateDir, StateOptions{
		Strict: o.ServiceMode, AllowUnprotected: o.AllowUnprotectedState,
		Repair: o.RepairACL && !o.ServiceMode, EventLog: o.EventLog,
	})
	if err != nil {
		return nil, err
	}
	a.stateVerified = st.Verified

	// 2. the device-local log (path-bearing, local-only).
	a.log = o.Logger
	if a.log == nil {
		l, c, err := openDeviceLog(a.layout)
		if err != nil {
			return nil, err
		}
		a.closers = append(a.closers, c)
		a.log = l
	}
	if o.AllowUnprotectedState {
		a.log.Warn(lineInsecureFlag)
		a.eventLog("warning", eventStateInsecure, lineInsecureFlag)
	}
	if st.Warning != "" {
		a.log.Warn("state directory not verified: " + st.Warning)
	}
	if st.EventLogErr != "" {
		a.log.Warn(st.EventLogErr)
	}
	// 2b. the service's own SCM configuration (service mode, before the
	// enrollment check so an unenrolled device repairs it too): settings a
	// rolled-back MSI upgrade or a manual change dropped are re-applied and
	// said so in agent.log and Event Log 1009. Never a reason to refuse.
	if o.ServiceMode {
		if msg := service.DriftReport(ensureServiceConfig()); msg != "" {
			a.log.Warn(msg)
			a.eventLog("warning", eventServiceConfigRepaired, msg)
		}
	}

	fail := func(err error) (*Agent, error) {
		a.Close()
		return nil, err
	}

	// 3. agent.json.
	cfg, err := config.Load(a.layout.ConfigFile())
	if errors.Is(err, config.ErrNotFound) {
		if st.Quarantined != "" {
			return fail(fmt.Errorf("%w (the untrusted state directory was quarantined to %s; enrol this device again from an elevated prompt)", ErrNotEnrolled, st.Quarantined))
		}
		return fail(ErrNotEnrolled)
	}
	if err != nil {
		return fail(err)
	}
	if o.ConsoleURL != "" {
		cfg.ServerURL = o.ConsoleURL
		if err := cfg.Validate(); err != nil {
			return fail(err)
		}
	}
	if !cfg.Enrolled() {
		return fail(ErrNotEnrolled)
	}
	if cfg.ServerURL == "" {
		return fail(errors.New("agent: agent.json has no server_url"))
	}
	a.cfg = cfg
	a.host = collectHost()

	// 4. secrets.
	a.sec, err = secrets.New(secrets.Options{
		Dir: a.layout.SecretsDir(), InsecurePlaintext: o.InsecurePlaintextSecrets,
		AllowUnprotected: o.AllowUnprotectedState && !o.ServiceMode,
	})
	if err != nil {
		return fail(err)
	}
	tok, err := a.sec.Get(secrets.NameDeviceToken)
	if err != nil {
		return fail(fmt.Errorf("agent: device token: %w", err))
	}
	a.deviceToken = secrets.CleanString(tok)
	gw, err := a.sec.Get(secrets.NameGatewayCred)
	if err != nil {
		return fail(fmt.Errorf("agent: gateway credential: %w", err))
	}
	a.gatewayPW = secrets.CleanString(gw)
	if rk, err := a.sec.Get(secrets.NameRepoKey); err == nil {
		a.repoPW = secrets.CleanString(rk)
	} else if errors.Is(err, secrets.ErrNotFound) {
		a.repoMissing = true
		a.log.Error("no repository key on this device (customer-held custody or incomplete enrollment): backups are refused")
	} else {
		return fail(fmt.Errorf("agent: repository key: %w", err))
	}

	// 5. journal, then stale runs.
	a.jr, err = journal.Open(ctx, journal.Options{Path: a.layout.JournalFile(), Clock: a.clock})
	if err != nil {
		return fail(fmt.Errorf("agent: open journal: %w", err))
	}
	a.closers = append(a.closers, a.jr)
	// "cryo-agent status" shows whether the last start ran on an unverified
	// state directory.
	if st.Warning != "" || o.AllowUnprotectedState {
		_ = a.jr.KVSet(ctx, journal.KVInsecureState, strings.TrimSpace("--insecure-unprotected-state "+st.Warning))
	} else {
		_ = a.jr.KVDelete(ctx, journal.KVInsecureState)
	}
	if !o.OneShot {
		_, marker, err := a.jr.KVGet(ctx, journal.KVStopMarker)
		if err != nil {
			return fail(err)
		}
		closed, err := a.jr.OpenRunsOnStart(ctx, marker, versionLabel())
		if err != nil {
			return fail(fmt.Errorf("agent: close stale runs: %w", err))
		}
		if marker {
			_ = a.jr.KVDelete(ctx, journal.KVStopMarker)
		}
		for _, r := range closed {
			a.log.Warn("closed a run left open by the previous process", "run", r.ID, "outcome", string(r.Outcome))
		}
		_ = emptyDir(a.layout.TempDir())
		if n, _ := logs.PruneRunLogs(a.layout.RunLogsDir(), a.clock(), logs.RunLogRetention); n > 0 {
			a.log.Info("pruned old run logs", "count", n)
		}
	}

	// 6. restic: hash and version (a failure is reported, not fatal).
	if _, err := a.verifyRestic(ctx); err != nil {
		a.resticErr = err
		a.log.Error("restic verification failed; backups are refused", "err", err.Error())
		a.eventLog("error", 1001, "restic binary verification failed; backups are refused")
	}

	// 7. last-known-good policy.
	if raw, ok, err := a.jr.KVGet(ctx, journal.KVPolicyJSON); err == nil && ok {
		if resp, err := decodePolicy([]byte(raw)); err == nil {
			a.pol = &resp
			a.noRunOnWake.Store(!resp.Policy.Schedule.RunOnWake)
			a.polETag, _, _ = a.jr.KVGet(ctx, journal.KVPolicyETag)
		} else {
			a.log.Error("stored policy is not usable; waiting for the next pull", "err", err.Error())
		}
	}
	if a.pol == nil {
		a.log.Warn("no policy yet: the agent will not schedule until the first policy pull succeeds")
	}

	// 8. uplink.
	a.up, err = uplink.New(uplink.Options{
		BaseURL: cfg.ServerURL, Token: a.token, AgentVersion: versionLabel(),
		InsecureLab: cfg.InsecureLab, SPKIPin: cfg.SPKIPin, Proxy: cfg.Proxy, Clock: a.clock, HTTPClient: o.httpClient,
		StartLeaseTimeout: o.leaseTimeout, // 0 = api.StartLeaseTimeout; the run context bounds it too
	})
	if err != nil {
		return fail(err)
	}
	if v, ok, _ := a.jr.KVGet(ctx, journal.KVGatewayAckPend); ok && v == "1" {
		a.gwAck = true
	}

	// 9. commands.
	a.exec = &commands.Executor{
		Journal: a.jr, Clock: a.clock,
		Offset: func() time.Duration {
			ms, _ := a.up.OffsetMS()
			return time.Duration(ms) * time.Millisecond
		},
		Handlers: commands.Handlers{
			BackupNow: a.cmdBackupNow, CancelRun: a.cmdCancel, RefreshPolicy: a.pullPolicy, CheckIn: a.cmdCheckIn,
		},
	}

	a.eventLog("info", 1000, "CryoBackup agent started")
	a.log.Info("agent started", "version", versionLabel(), "service_mode", o.ServiceMode)
	return a, nil
}

// openDeviceLog opens logs\agent.log, the device-local log the agent writes in
// service and console mode alike (step 2 of New): appending, rotating at
// 10 MiB x 5, Info and above, every line flushed (logs.Rotating, L-LOG-1).
func openDeviceLog(layout config.Layout) (*slog.Logger, io.Closer, error) {
	rot, err := logs.OpenRotating(layout.AgentLog(), logs.DefaultMaxBytes, logs.DefaultKeep)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: open log: %w", err)
	}
	return logs.NewLogger(rot, slog.LevelInfo), rot, nil
}

// Close releases the journal and the log. It is safe to call twice.
func (a *Agent) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		_ = a.closers[i].Close()
	}
	a.closers = nil
}

func (a *Agent) token() string {
	a.secMu.Lock()
	defer a.secMu.Unlock()
	return a.deviceToken
}

func (a *Agent) eventLog(level string, id uint32, msg string) {
	if a.events != nil {
		a.events.Log(level, id, msg)
	}
}

// resticPath is the configured binary or restic.exe next to the agent binary.
func (a *Agent) resticPath() string {
	if a.o.ResticPath != "" {
		return a.o.ResticPath
	}
	exe, err := os.Executable()
	if err != nil {
		return "restic.exe"
	}
	name := "restic"
	if strings.EqualFold(filepath.Ext(exe), ".exe") {
		name = "restic.exe"
	}
	return filepath.Join(filepath.Dir(exe), name)
}

// expectedSHA is the pinned hash: the option, else the build-time pin.
func (a *Agent) expectedSHA() string {
	if a.o.ResticSHA256 != "" {
		return a.o.ResticSHA256
	}
	return PinnedResticSHA256
}

func (a *Agent) verifyRestic(ctx context.Context) (string, error) {
	v, err := a.sup.Verify(ctx, a.resticPath(), a.expectedSHA(), a.o.AllowUnpinnedRestic, a.o.ServiceMode)
	if err == nil {
		a.mu.Lock()
		a.resticVer = v
		a.mu.Unlock()
	}
	return v, err
}

func (a *Agent) environment() policy.Environment {
	if a.o.environment != nil {
		return a.o.environment()
	}
	return discoverEnvironment()
}

func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	return nil
}

func kick(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
