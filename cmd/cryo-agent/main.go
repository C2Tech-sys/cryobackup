// Command cryo-agent is the CryoBackup endpoint agent: a Windows service that
// owns the backup schedule and runs a pinned restic.exe for all data work
// (ADR 0002 D11). It never holds a storage credential: it holds its own device
// token, its gateway login and the repository password, in a DPAPI store.
//
// Subcommands:
//
//	run        run the agent: as the Windows service when started by the service
//	           control manager, in the foreground (console mode) otherwise
//	install    install the Windows service (LocalSystem, delayed automatic start)
//	uninstall  remove the Windows service
//	enroll     one-time enrollment with the console (stores the credentials,
//	           the connection facts and the first policy)
//	status     print the device state as the agent sees it (path-free)
//	check-in   send one heartbeat, pull the policy and deliver queued results
//	version    print the build stamp
//
// Lab loop, unprivileged, against a console and a gateway you control:
//
//	cryo-agent enroll --server https://console.example:8443 --token-file token.txt --state-dir D:\lab\state --insecure-unprotected-state
//	cryo-agent run --state-dir D:\lab\state --restic C:\tools\restic.exe --insecure-unpinned-restic --insecure-unprotected-state
//	cryo-agent status --state-dir D:\lab\state
//
// An unprivileged run cannot restrict the state directory to SYSTEM and
// Administrators, so enroll, run and check-in refuse it unless the lab flag
// --insecure-unprotected-state is given (warned about on every start; the
// service ignores it). An elevated enroll or install restricts it. A state
// directory that is untrusted (someone else created it first, or its ACL was
// opened, or an owner cannot be read) is never adopted: an elevated start
// refuses it and records the untrusted verdict in
// HKLM\SOFTWARE\CryoBackup\Agent (value UntrustedRepairRequired-<id>, one per
// state directory, which only an elevated process can write), and every start
// on that directory refuses while it exists ("status" names the value). An
// elevated --repair-acl moves the tree aside whole to
// <name>.quarantine-<UTC stamp>-<8 random hex> (a taken name is retried with a
// fresh suffix, up to 5 times), creates a fresh one, clears the verdict, and
// the device must then be enrolled again.
//
// Exit codes: 0 success, 2 usage, 3 not implemented (none left in M0), 4 a
// configuration or state error, 5 a failure talking to the console.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent"
	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/diag"
	"github.com/c2tech-sys/cryobackup/internal/agent/service"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
)

// Exit codes.
const (
	exitOK       = 0
	exitUsage    = 2
	exitNotImpl  = 3
	exitConfig   = 4
	exitNetwork  = 5
	enrollEnvVar = "CRYO_ENROLL_TOKEN"
)

// What the subcommands call outside this package. Production uses the real
// functions; the cmd-layer tests (wiring_test.go) replace them to check how a
// command wires them (the service host's event relay, the Event Log failures
// printed by install and enroll) without a service manager, an elevated
// prompt or a console.
var (
	isService           = service.IsService
	runService          = service.Run
	openEventLog        = service.OpenEventLog
	prepareInstallState = agent.PrepareInstallState
	installService      = service.Install
	enrollDevice        = agent.Enroll
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, buildinfo.String("cryo-agent"))
		return exitOK
	case "run":
		return cmdRun(args[1:], stderr)
	case "install":
		return cmdInstall(args[1:], stdout, stderr)
	case "uninstall":
		return cmdUninstall(stdout, stderr)
	case "enroll":
		return cmdEnroll(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "check-in":
		return cmdCheckIn(args[1:], stdout, stderr)
	case "msi-configure", "msi-rollback", "msi-commit": // custom actions of the MSI (msi.go)
		return cmdMSI(args[0], args[1:], os.Stdin, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	}
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: cryo-agent <command> [flags]

commands:
  run        run the agent (Windows service under the SCM, console mode otherwise)
               --state-dir D  --console-url U  --restic PATH  --restic-sha256 HEX
               --insecure-unpinned-restic  --insecure-plaintext-secrets
               --insecure-unprotected-state  --repair-acl
  install    install the Windows service (needs an elevated prompt)
               --state-dir D  --restic PATH  --restic-sha256 HEX  --insecure-unpinned-restic
               --repair-acl
  uninstall  remove the Windows service
  enroll     enroll this device with the console
               --server URL  --token T (or --token-file F, or $CRYO_ENROLL_TOKEN)
               --state-dir D  --insecure-tls-for-lab  --spki-pin B64  --proxy URL
               --insecure-plaintext-secrets  --insecure-unprotected-state  --repair-acl
  status     print the device state (path-free)    --state-dir D
  check-in   send a heartbeat, pull the policy, deliver queued results
               --state-dir D  --restic PATH  --restic-sha256 HEX  --insecure-unpinned-restic
               --insecure-unprotected-state

  --insecure-unprotected-state  lab only, never in service mode: use a state directory that is not
                                verified as SYSTEM + Administrators only (an unelevated run), with a
                                warning on every start; secrets in it are readable by local users
  --repair-acl                  elevated only: if the state directory is untrusted (foreign or unreadable
                                owner, open ACL, or the untrusted verdict in HKLM\SOFTWARE\CryoBackup\Agent),
                                move it aside whole to <name>.quarantine-<UTC stamp>-<8 random hex> (a taken
                                name is retried with a fresh suffix, up to 5 times; nothing in it is used or
                                re-ACLed), create a fresh one, clear the verdict, and require a new elevated
                                enroll (audited in agent.log and Event Log 1007; review, then delete the
                                quarantine). While the verdict exists every start refuses.
  version    print the build stamp

exit codes: 0 ok, 2 usage, 3 not implemented, 4 configuration or state error, 5 console unreachable or refused
`)
}

// common flags shared by run, install and check-in.
type commonFlags struct {
	stateDir        string
	restic          string
	resticSHA       string
	unpinned        bool
	plaintextSecret bool
	unprotected     bool
	repairACL       bool
}

// Help texts of the state-directory flags (shared by several subcommands).
const (
	helpUnprotected = "lab only, ignored by the service: use a state directory that is not verified as SYSTEM + Administrators only, with a warning on every start"
	helpRepairACL   = "elevated only: move an untrusted state directory aside whole to <name>.quarantine-<UTC stamp>-<8 random hex>, create a fresh one, clear the HKLM untrusted verdict, and require a new elevated enroll (audited in agent.log and Event Log 1007)"
)

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.stateDir, "state-dir", "", "state directory (default %ProgramData%\\CryoBackup)")
	fs.StringVar(&c.restic, "restic", "", "path of restic.exe (default: next to the agent)")
	fs.StringVar(&c.resticSHA, "restic-sha256", "", "expected SHA-256 of restic.exe (default: the build's pin)")
	fs.BoolVar(&c.unpinned, "insecure-unpinned-restic", false, "lab only: run restic without a pinned hash (the version is still checked)")
	fs.BoolVar(&c.plaintextSecret, "insecure-plaintext-secrets", false, "CI only, not on Windows: store secrets as plain files")
}

func (c *commonFlags) registerUnprotected(fs *flag.FlagSet) {
	fs.BoolVar(&c.unprotected, "insecure-unprotected-state", false, helpUnprotected)
}

func (c *commonFlags) registerRepair(fs *flag.FlagSet) {
	fs.BoolVar(&c.repairACL, "repair-acl", false, helpRepairACL)
}

func (c *commonFlags) options() agent.Options {
	return agent.Options{
		StateDir: c.stateDir, ResticPath: c.restic, ResticSHA256: c.resticSHA,
		AllowUnpinnedRestic: c.unpinned, InsecurePlaintextSecrets: c.plaintextSecret,
		AllowUnprotectedState: c.unprotected, RepairACL: c.repairACL,
	}
}

// warnUnprotected prints the loud warning of --insecure-unprotected-state.
func warnUnprotected(stderr io.Writer, on bool) {
	if on {
		fmt.Fprintln(stderr, "WARNING: --insecure-unprotected-state: the state directory is not verified as SYSTEM + Administrators only; secrets in it are readable by local users. Lab use only.")
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("cryo-agent "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "cryo-agent: "+err.Error())
	var ue *uplink.Error
	if errors.As(err, &ue) || errors.Is(err, uplink.ErrPathLeak) {
		return exitNetwork
	}
	return exitConfig
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdRun(args []string, stderr io.Writer) int {
	fs := newFlagSet("run", stderr)
	var cf commonFlags
	cf.register(fs)
	cf.registerUnprotected(fs)
	cf.registerRepair(fs)
	consoleURL := fs.String("console-url", "", "override the console URL in agent.json for this run")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	opts := cf.options()
	opts.ConsoleURL = *consoleURL

	isSvc, err := isService()
	if err != nil {
		return fail(stderr, err)
	}
	if isSvc {
		opts.ServiceMode = true
		if el, err := openEventLog(); err == nil {
			opts.EventLog = el
			defer el.Close()
		}
		// The agent is created inside Run, after the SCM handler exists; the
		// relay hands it the power and session events (resume re-plans the
		// schedule, a session change is recorded and ignored; neither blocks
		// the control handler, so Stop is never delayed).
		var relay agent.EventRelay
		host := service.Host{
			StopCause: supervisor.ErrServiceStop,
			OnEvent:   relay.Handle,
			Run: func(ctx context.Context) error {
				a, err := agent.New(ctx, opts)
				if err != nil {
					return err
				}
				defer a.Close()
				relay.Set(a)
				defer relay.Set(nil)
				return a.Run(ctx)
			},
		}
		if err := runService(host); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}

	ctx, stop := signalContext()
	defer stop()
	warnUnprotected(stderr, opts.AllowUnprotectedState)
	a, err := agent.New(ctx, opts)
	if err != nil {
		return fail(stderr, err)
	}
	defer a.Close()
	if err := a.Run(ctx); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func cmdInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("install", stderr)
	var cf commonFlags
	cf.register(fs)
	cf.registerRepair(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	stateDir := cf.stateDir
	if stateDir == "" {
		stateDir = config.DefaultStateDir()
	}
	// Strict: the service will refuse anything less than a verified state
	// directory, so install does too.
	// FIX20: with --repair-acl the Event Log source is registered first and the
	// quarantine writes Event Log 1007 like "run" does.
	st, err := prepareInstallState(stateDir, cf.repairACL)
	if st.EventLogErr != "" {
		fmt.Fprintln(stderr, st.EventLogErr)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if st.Quarantined != "" {
		fmt.Fprintln(stderr, "the untrusted state directory was quarantined to "+st.Quarantined+"; enroll this device again (elevated \"cryo-agent enroll\") before starting the service; review, then delete the quarantine")
	}
	svcArgs := []string{"run", "--state-dir", stateDir}
	if cf.restic != "" {
		svcArgs = append(svcArgs, "--restic", cf.restic)
	}
	if cf.resticSHA != "" {
		svcArgs = append(svcArgs, "--restic-sha256", cf.resticSHA)
	}
	if cf.unpinned {
		svcArgs = append(svcArgs, "--insecure-unpinned-restic")
	}
	if err := installService(service.InstallOptions{Args: svcArgs}); err != nil {
		if errors.Is(err, service.ErrUnsupported) {
			fmt.Fprintln(stderr, "cryo-agent: the Windows service can only be installed on Windows")
			return exitConfig
		}
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "service "+service.Name+" installed (LocalSystem, delayed automatic start); start it with: sc start "+service.Name)
	return exitOK
}

func cmdUninstall(stdout, stderr io.Writer) int {
	if err := service.Uninstall(); err != nil {
		if errors.Is(err, service.ErrUnsupported) {
			fmt.Fprintln(stderr, "cryo-agent: the Windows service can only be removed on Windows")
			return exitConfig
		}
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "service "+service.Name+" removed (state in the state directory is kept)")
	return exitOK
}

func cmdEnroll(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("enroll", stderr)
	stateDir := fs.String("state-dir", "", "state directory (default %ProgramData%\\CryoBackup)")
	server := fs.String("server", "", "console agent URL, https://host[:port]")
	token := fs.String("token", "", "enrollment token (prefer --token-file or $"+enrollEnvVar+": argv is visible to other processes)")
	tokenFile := fs.String("token-file", "", "file holding the enrollment token")
	lab := fs.Bool("insecure-tls-for-lab", false, "lab only: allow http and skip TLS verification")
	pin := fs.String("spki-pin", "", "base64 SHA-256 of the console's public key info")
	proxy := fs.String("proxy", "", "http(s) proxy URL for the uplink")
	plain := fs.Bool("insecure-plaintext-secrets", false, "CI only, not on Windows: store secrets as plain files")
	unprotected := fs.Bool("insecure-unprotected-state", false, helpUnprotected)
	repair := fs.Bool("repair-acl", false, helpRepairACL)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *server == "" {
		fmt.Fprintln(stderr, "cryo-agent enroll: --server is required")
		return exitUsage
	}
	tok := strings.TrimSpace(*token)
	if tok == "" && *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile) //nolint:gosec // path from the agent's own state directory or configuration
		if err != nil {
			fmt.Fprintln(stderr, "cryo-agent enroll: the token file could not be read")
			return exitConfig
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv(enrollEnvVar))
	}
	if tok == "" {
		fmt.Fprintln(stderr, "cryo-agent enroll: an enrollment token is required (--token, --token-file or $"+enrollEnvVar+")")
		return exitUsage
	}

	ctx, stop := signalContext()
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, agent.EnrollTimeout)
	defer cancel()
	warnUnprotected(stderr, *unprotected)
	res, err := enrollDevice(ctx, agent.EnrollOptions{
		StateDir: *stateDir, ServerURL: *server, Token: tok, InsecureLab: *lab, SPKIPin: *pin, Proxy: *proxy,
		InsecurePlaintextSecrets: *plain, AllowUnprotectedState: *unprotected, RepairACL: *repair, Log: stderr,
	})
	if res.EventLogErr != "" {
		fmt.Fprintln(stderr, res.EventLogErr)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if res.Quarantined != "" {
		fmt.Fprintln(stderr, "the untrusted state directory was quarantined to "+res.Quarantined+" before enrolling; review, then delete it")
	}
	fmt.Fprintf(stdout, "enrolled: device %s, key custody %s, policy version %d\n", res.DeviceID, res.Custody, res.PolicyVersion)
	if res.StateWarning != "" {
		fmt.Fprintln(stderr, "warning: state directory not verified: "+res.StateWarning+" (lab use only)")
	}
	return exitOK
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	stateDir := fs.String("state-dir", "", "state directory (default %ProgramData%\\CryoBackup)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	ctx, stop := signalContext()
	defer stop()
	rep, err := diag.Status(ctx, *stateDir, time.Now())
	if err != nil {
		return fail(stderr, err)
	}
	rep.Render(stdout)
	return exitOK
}

func cmdCheckIn(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("check-in", stderr)
	var cf commonFlags
	cf.register(fs)
	cf.registerUnprotected(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	warnUnprotected(stderr, cf.unprotected)
	ctx, stop := signalContext()
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := agent.CheckIn(ctx, cf.options()); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "checked in: heartbeat sent, policy current, queued results delivered")
	return exitOK
}
