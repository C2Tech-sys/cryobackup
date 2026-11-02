package main

// The MSI's custom actions (deploy/windows). They are not for interactive use
// and are not listed in the usage text.
//
//	cryo-agent msi-configure  deferred, as SYSTEM, after InstallServices and
//	                          before StartServices: prepare the state
//	                          directory and configure the service exactly as
//	                          "install" does, then enroll when the MSI was
//	                          given TOKEN and SERVER
//	cryo-agent msi-rollback   rollback of msi-configure: remove the state
//	                          directory if, and only if, the same install run
//	                          created it
//	cryo-agent msi-commit     the install succeeded: drop the creation marker
//
// The custom-action DLL (deploy/windows/ca/cryoca.c) starts these with nothing
// on the command line but the subcommand and writes the request to stdin as
// key=value lines (server, token, lab, repair, nonce). Stdin is the one channel
// that is not readable by other processes (argv is), not inherited by
// children (the environment is) and not left on disk (a file is): the
// enrollment token travels there and nowhere else. Nothing this file prints
// names the token. The RMM wrapper (deploy/windows/rmm) uses msi-configure the
// same way to enroll a device that is installed but not enrolled.
//
// Rollback safety: every install run has its own random nonce (the DLL's
// CryoNewNonce action, before the script is built), passed to configure and
// to rollback. Configure writes it into the creation marker only when it
// created the state directory itself; rollback removes the directory only when
// the marker holds its own run's nonce. A marker left by any other run, or no
// marker, means the directory is kept, whatever path configure took to exit.
//
// The flags exist for the tests only (an unelevated, cross-platform run); the
// MSI passes none.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent"
	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/service"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
)

// msiCreatedMarker is written into the state directory by msi-configure when
// this run created the directory, holding the run's nonce, and removed by
// msi-commit. Only a marker holding the rollback's own nonce lets msi-rollback
// remove the directory: a directory that existed before the install (an
// upgrade, a reinstall, an enrolled device) is never removed.
const msiCreatedMarker = ".msi-install-created"

// msiPrepareState and msiPrepareInstallState are agent.PrepareState and
// agent.PrepareInstallState (variables for the tests: a --repair-acl
// quarantine needs an elevated process and an untrusted directory).
var (
	msiPrepareState        = agent.PrepareState
	msiPrepareInstallState = agent.PrepareInstallState
)

// msiRequestLimit bounds the request read from stdin.
const msiRequestLimit = 64 << 10

// msiNonceRE is the form of a run nonce: 16 random bytes in lower-case hex.
var msiNonceRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// msiRequest is what the MSI passes on stdin.
type msiRequest struct {
	server string
	token  string
	lab    bool   // CRYO_INSECURE_LAB=1: http allowed, TLS not verified
	repair bool   // CRYO_REPAIR_ACL=1: quarantine an untrusted state directory
	nonce  string // this install run's nonce (CryoRunNonce)
}

// parseMSIRequest reads key=value lines. An empty value is an unset MSI
// property. An unknown key is refused: it means the DLL and the agent were
// built from different sources, which must not pass silently.
func parseMSIRequest(r io.Reader) (msiRequest, error) {
	var req msiRequest
	b, err := io.ReadAll(io.LimitReader(r, msiRequestLimit+1))
	if err != nil {
		return req, errors.New("the request could not be read from stdin")
	}
	if len(b) > msiRequestLimit {
		return req, errors.New("the request on stdin is too large")
	}
	// A UTF-8 byte-order mark is what some writers (Windows PowerShell's
	// pipeline) put first; it is not part of the first key.
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 0, 4096), msiRequestLimit)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return req, errors.New("a request line is not key=value")
		}
		v = strings.TrimSpace(v)
		switch k {
		case "server":
			req.server = v
		case "token":
			req.token = v
		case "nonce":
			if v != "" && !msiNonceRE.MatchString(v) {
				return req, errors.New("the run nonce is malformed")
			}
			req.nonce = v
		case "lab", "repair":
			on, err := msiFlag(v)
			if err != nil {
				return req, fmt.Errorf("%s: %w", msiPropertyName(k), err)
			}
			if k == "lab" {
				req.lab = on
			} else {
				req.repair = on
			}
		default:
			return req, fmt.Errorf("unknown request key %q (the MSI and cryo-agent.exe do not match)", k)
		}
	}
	return req, sc.Err()
}

// msiFlag reads a 0/1 MSI property: unset or "0" is off, "1" is on; anything
// else is refused rather than guessed.
func msiFlag(v string) (bool, error) {
	switch v {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, errors.New("must be 1 or unset")
}

func msiPropertyName(key string) string {
	if key == "lab" {
		return "CRYO_INSECURE_LAB"
	}
	return "CRYO_REPAIR_ACL"
}

// validate applies the rules the MSI's launch conditions also check, so the
// agent refuses the same things whatever the package says.
func (r msiRequest) validate() error {
	switch {
	case r.nonce == "":
		return errors.New("the request has no run nonce (the MSI and cryo-agent.exe do not match)")
	case r.token != "" && r.server == "":
		return errors.New("TOKEN was given without SERVER: give both, or neither to install unenrolled")
	case r.server != "" && r.token == "":
		return errors.New("SERVER was given without TOKEN: give both, or neither to install unenrolled")
	case r.server != "":
		if err := config.ValidateBaseURL(r.server, r.lab); err != nil {
			return fmt.Errorf("SERVER: %v (plain http needs CRYO_INSECURE_LAB=1, lab only)", err)
		}
	}
	return nil
}

type msiFlags struct {
	stateDir    string
	unprotected bool
	plaintext   bool
}

func cmdMSI(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	set := newFlagSet(name, stderr)
	var f msiFlags
	set.StringVar(&f.stateDir, "state-dir", "", "tests only: state directory (default %ProgramData%\\CryoBackup)")
	set.BoolVar(&f.unprotected, "insecure-unprotected-state", false, "tests only: "+helpUnprotected)
	set.BoolVar(&f.plaintext, "insecure-plaintext-secrets", false, "tests only, not on Windows: store secrets as plain files")
	if err := set.Parse(args); err != nil {
		return exitUsage
	}
	if f.stateDir == "" {
		f.stateDir = config.DefaultStateDir()
	}
	switch name {
	case "msi-configure":
		req, err := parseMSIRequest(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "cryo-agent msi-configure: "+err.Error())
			return exitUsage
		}
		code, quarantined := msiConfigure(req, f, stdout, stderr)
		if code != exitOK && quarantined != "" {
			fmt.Fprintln(stderr, "cryo-agent msi-configure: the install failed after CRYO_REPAIR_ACL=1 moved the untrusted state directory to "+quarantined+"; it stays there (the rollback does not move it back: it was refused as untrusted and would be refused again), and the fresh state directory this run created is removed; fix the reason above and install again (the device is then enrolled as new), and review, then delete the quarantine")
		}
		return code
	case "msi-rollback":
		req, err := parseMSIRequest(stdin)
		if err != nil {
			// A rollback that cannot read its request removes nothing.
			fmt.Fprintln(stdout, "rollback: the request could not be read; the state directory is kept")
			return exitOK
		}
		return msiRollback(f.stateDir, req.nonce, stdout, stderr)
	case "msi-commit":
		return msiCommit(f.stateDir, stdout, stderr)
	}
	usage(stderr)
	return exitUsage
}

// msiConfigure returns the exit code and, when --repair-acl quarantined the
// old state directory, where it went.
func msiConfigure(req msiRequest, f msiFlags, stdout, stderr io.Writer) (int, string) {
	if err := req.validate(); err != nil {
		fmt.Fprintln(stderr, "cryo-agent msi-configure: "+err.Error())
		return exitUsage, ""
	}
	stateDir := f.stateDir
	marker := filepath.Join(stateDir, msiCreatedMarker)
	_, statErr := os.Lstat(stateDir)
	created := errors.Is(statErr, fs.ErrNotExist)
	if statErr != nil && !created {
		fmt.Fprintln(stderr, "cryo-agent msi-configure: the state directory could not be examined")
		return exitConfig, ""
	}
	// cleanup undoes this run on a failure: a directory this run created is
	// removed whole, so a failed install leaves no half-configured state.
	cleanup := func() {
		if created {
			if err := os.RemoveAll(stateDir); err != nil {
				fmt.Fprintln(stderr, "cryo-agent msi-configure: the state directory this install created could not be removed")
			}
		}
	}

	var st agent.StateReport
	var err error
	if f.unprotected {
		warnUnprotected(stderr, true)
		st, err = msiPrepareState(stateDir, agent.StateOptions{AllowUnprotected: true, Repair: req.repair})
	} else {
		// Exactly what "cryo-agent install" does: strict, so the service will
		// accept what the MSI leaves behind.
		st, err = msiPrepareInstallState(stateDir, req.repair)
	}
	if st.EventLogErr != "" {
		fmt.Fprintln(stderr, st.EventLogErr)
	}
	if st.Quarantined != "" {
		// The directory now in place is the fresh one PrepareState created
		// after moving the untrusted tree aside: this run's.
		created = true
	}
	if err != nil {
		// A refused directory was not created by this run: kept as it is.
		cleanup()
		return fail(stderr, err), st.Quarantined
	}
	if st.Quarantined != "" {
		fmt.Fprintln(stderr, "the untrusted state directory was quarantined to "+st.Quarantined+"; review, then delete it")
	}
	if created {
		if err := os.WriteFile(marker, []byte(req.nonce), 0o600); err != nil {
			cleanup()
			fmt.Fprintln(stderr, "cryo-agent msi-configure: the install marker could not be written")
			return exitConfig, st.Quarantined
		}
		fmt.Fprintln(stdout, "state directory created")
	} else {
		fmt.Fprintln(stdout, "state directory exists: configuration, identity and journal are kept")
	}

	// The MSI created the service (ServiceInstall); its configuration beyond
	// that comes from the same code "cryo-agent install" runs, because
	// Windows Installer's own MsiServiceConfig tables are documented not to
	// work as expected: delayed automatic start, restart after 1, 5 and 15
	// minutes, the pre-shutdown budget, the Event Log source. The tests run
	// without a service (the lab flag) and skip this.
	if !f.unprotected {
		if err := service.Install(service.InstallOptions{Args: []string{"run", "--state-dir", stateDir}}); err != nil {
			cleanup()
			return fail(stderr, err), st.Quarantined
		}
		fmt.Fprintln(stdout, "service "+service.Name+" configured: LocalSystem, delayed automatic start, restart on failure")
	}

	cfg, err := config.Load(config.Layout{Root: stateDir}.ConfigFile())
	switch {
	case errors.Is(err, config.ErrNotFound):
	case err != nil:
		cleanup()
		return fail(stderr, err), st.Quarantined
	case cfg.Enrolled():
		if req.token != "" {
			fmt.Fprintln(stdout, "TOKEN ignored: this device is already enrolled as device "+cfg.DeviceID+" (to enroll it as a new device, uninstall, move the state directory aside, and install again)")
		} else {
			fmt.Fprintln(stdout, "enrolled: device "+cfg.DeviceID)
		}
		return exitOK, ""
	}
	if req.token == "" {
		fmt.Fprintln(stdout, "no TOKEN given: the service is installed but this device is NOT ENROLLED and the service will refuse to run until it is; enroll it from an elevated prompt with \"cryo-agent enroll --server URL --token-file FILE\", then \"sc start CryoBackup\"")
		return exitOK, ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := agent.Enroll(ctx, agent.EnrollOptions{
		StateDir: stateDir, ServerURL: req.server, Token: req.token, InsecureLab: req.lab,
		InsecurePlaintextSecrets: f.plaintext, AllowUnprotectedState: f.unprotected,
	})
	if res.EventLogErr != "" {
		fmt.Fprintln(stderr, res.EventLogErr)
	}
	if err != nil {
		fmt.Fprintln(stderr, "cryo-agent msi-configure: enrollment failed, error class: "+enrollErrorClass(err))
		cleanup()
		return fail(stderr, err), st.Quarantined
	}
	fmt.Fprintf(stdout, "enrolled: device %s, key custody %s, policy version %d\n", res.DeviceID, res.Custody, res.PolicyVersion)
	if req.lab {
		fmt.Fprintln(stderr, "WARNING: CRYO_INSECURE_LAB=1: this device talks to its console without TLS verification. Lab use only.")
	}
	return exitOK, ""
}

// enrollErrorClass names the kind of an enrollment failure for the MSI log:
// the uplink's class (tls, network, auth) or "console" for another refusal by
// the console, "state" for anything local.
func enrollErrorClass(err error) string {
	var ue *uplink.Error
	if errors.As(err, &ue) {
		if c := string(ue.Class()); c != "" {
			return c
		}
		return "console"
	}
	if errors.Is(err, uplink.ErrPathLeak) {
		return "console"
	}
	return "state"
}

// msiRollback removes the state directory only when its creation marker
// holds this run's nonce.
func msiRollback(stateDir, nonce string, stdout, stderr io.Writer) int {
	marker := filepath.Join(stateDir, msiCreatedMarker)
	fi, err := os.Lstat(marker)
	if nonce == "" || err != nil || !fi.Mode().IsRegular() || fi.Size() > 64 {
		fmt.Fprintln(stdout, "rollback: the state directory was not created by this install and is kept")
		return exitOK
	}
	b, err := os.ReadFile(marker) //nolint:gosec // the marker inside the agent's own state directory
	if err != nil || !bytes.Equal(bytes.TrimSpace(b), []byte(nonce)) {
		fmt.Fprintln(stdout, "rollback: the state directory was not created by this install run and is kept")
		return exitOK
	}
	if err := os.RemoveAll(stateDir); err != nil {
		fmt.Fprintln(stderr, "cryo-agent msi-rollback: the state directory this install created could not be removed")
		return exitConfig
	}
	fmt.Fprintln(stdout, "rollback: the state directory this install created was removed")
	return exitOK
}

// msiCommit removes the creation marker, whichever run wrote it: removing a
// marker can only ever keep a directory, never lose one.
func msiCommit(stateDir string, stdout, stderr io.Writer) int {
	err := os.Remove(filepath.Join(stateDir, msiCreatedMarker))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stderr, "cryo-agent msi-commit: the install marker could not be removed")
		return exitConfig
	}
	fmt.Fprintln(stdout, "commit: install complete")
	return exitOK
}
