package maintain

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// M2-W1B: systemd credential isolation, configuration directory mode and the
// optional certificate sync.

func unitValue(lines []string, key string) []string {
	var out []string
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

// Each unit hides the credentials and state it must not be able to read, so a
// compromise of one process cannot read another's secret even as the same user.
func TestSystemdUnitsIsolateCredentials(t *testing.T) {
	want := map[string]string{
		"rclone-gateway.service": "-/etc/cryobackup/rclone-admin.conf -/etc/cryobackup/master.key -/etc/cryobackup/config.json -/var/lib/cryobackup",
		"cryo-console.service":   "-/etc/cryobackup/rclone-admin.conf -/etc/cryobackup/rclone-gateway.conf",
		"cryo-maintain.service":  "-/etc/cryobackup/rclone-gateway.conf",
	}
	for unit, ip := range want {
		lines := unitLines(readDeploy(t, "systemd", unit))
		got := unitValue(lines, "InaccessiblePaths")
		if len(got) != 1 || got[0] != ip {
			t.Errorf("%s: InaccessiblePaths = %q, want exactly %q", unit, got, ip)
			continue
		}
		// A unit must not hide what it needs: its own rclone file, config, key.
		hidden := strings.Fields(ip)
		needs := append(unitValue(lines, "ExecStart"), unitValue(lines, "Environment")...)
		for _, h := range hidden {
			p := strings.TrimPrefix(h, "-")
			for _, n := range needs {
				if strings.Contains(n, p+" ") || strings.HasSuffix(n, p) || strings.Contains(n, "="+p) {
					t.Errorf("%s hides %s but its own settings use it: %s", unit, p, n)
				}
			}
		}
	}
	// The gateway reads the htpasswd under cryobackup-auth, a different directory
	// from the hidden /var/lib/cryobackup.
	gw := readDeploy(t, "systemd", "rclone-gateway.service")
	if !strings.Contains(gw, "/var/lib/cryobackup-auth/htpasswd") {
		t.Error("the gateway no longer reads /var/lib/cryobackup-auth/htpasswd")
	}
}

// systemd warns when a unit's ConfigurationDirectory is created 0755 while the
// README creates it 0750: the units say 0750.
func TestSystemdConfigurationDirectoryMode(t *testing.T) {
	for _, unit := range []string{"cryo-console.service", "cryo-maintain.service", "rclone-gateway.service"} {
		lines := unitLines(readDeploy(t, "systemd", unit))
		if len(unitValue(lines, "ConfigurationDirectory")) == 0 {
			continue
		}
		if m := unitValue(lines, "ConfigurationDirectoryMode"); len(m) != 1 || m[0] != "0750" {
			t.Errorf("%s declares ConfigurationDirectory but ConfigurationDirectoryMode = %q, want 0750", unit, m)
		}
	}
	for _, unit := range []string{"cryo-console.service", "cryo-maintain.service"} {
		if len(unitValue(unitLines(readDeploy(t, "systemd", unit)), "ConfigurationDirectory")) == 0 {
			t.Errorf("%s no longer declares ConfigurationDirectory: the mode test above checks nothing", unit)
		}
	}
}

func TestSystemdReadmeDocumentsTheHardening(t *testing.T) {
	readme := readDeploy(t, "systemd", "README.md")
	for _, want := range []string{"InaccessiblePaths", "ConfigurationDirectoryMode=0750", "cryobackup-certsync", "certsync.env", "0440"} {
		if !strings.Contains(readme, want) {
			t.Errorf("deploy/systemd/README.md lacks %q", want)
		}
	}
	// Wasabi refuses rclone's CreateBucket probe on a regional endpoint: both remotes.
	if n := strings.Count(readme, "no_check_bucket = true"); n < 2 {
		t.Errorf("deploy/systemd/README.md names no_check_bucket = true %d time(s), want both remotes", n)
	}
	for _, remote := range []string{"[store]", "[admin]"} {
		if !strings.Contains(readme, remote) {
			t.Errorf("deploy/systemd/README.md does not name the %s remote beside no_check_bucket", remote)
		}
	}
}

func TestCertsyncUnitsAreOptionalAndGeneric(t *testing.T) {
	svc := unitLines(readDeploy(t, "systemd", "certsync", "cryobackup-certsync.service"))
	if v := unitValue(svc, "Type"); len(v) != 1 || v[0] != "oneshot" {
		t.Errorf("certsync service Type = %q, want oneshot", v)
	}
	if v := unitValue(svc, "EnvironmentFile"); len(v) != 1 || v[0] != "/etc/cryobackup/certsync.env" {
		t.Errorf("certsync service EnvironmentFile = %q", v)
	}
	if v := unitValue(svc, "ExecStart"); len(v) != 1 || !strings.HasSuffix(v[0], "/cryobackup-certsync") {
		t.Errorf("certsync service ExecStart = %q", v)
	}
	timer := unitLines(readDeploy(t, "systemd", "certsync", "cryobackup-certsync.timer"))
	if v := unitValue(timer, "Persistent"); len(v) != 1 || v[0] != "true" {
		t.Errorf("certsync timer Persistent = %q, want true (a host that was off still syncs)", v)
	}
	if len(unitValue(timer, "OnCalendar")) == 0 || len(unitValue(timer, "RandomizedDelaySec")) == 0 {
		t.Error("certsync timer lacks OnCalendar or RandomizedDelaySec")
	}
	// Nothing operator-specific: no host name outside the .invalid placeholders.
	host := regexp.MustCompile(`[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.(com|net|org|io|dev|us|biz)\b`)
	for _, f := range []string{"cryobackup-certsync", "cryobackup-certsync.service", "cryobackup-certsync.timer", "certsync.env.example"} {
		if m := host.FindString(readDeploy(t, "systemd", "certsync", f)); m != "" {
			t.Errorf("certsync/%s names a host: %s", f, m)
		}
	}
}

// ---- the script, run for real ---------------------------------------------------

type certsyncRig struct {
	t       *testing.T
	sh      string
	src     string // <src>/live/<host>/{fullchain,privkey}.pem
	dest    string
	ctlLog  string
	stub    string
	agent   string
	gateway string
	env     []string
}

func slash(p string) string { return filepath.ToSlash(p) }

func newCertsyncRig(t *testing.T) *certsyncRig {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl on PATH")
	}
	dir := t.TempDir()
	r := &certsyncRig{t: t, sh: sh, src: filepath.Join(dir, "acme"), dest: filepath.Join(dir, "tls"),
		ctlLog: filepath.Join(dir, "systemctl.log"), stub: filepath.Join(dir, "systemctl-stub"),
		agent: "agent.example.invalid", gateway: "gateway.example.invalid"}
	if err := os.MkdirAll(r.dest, 0o750); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\necho \"$@\" >> '" + slash(r.ctlLog) + "'\n"
	if err := os.WriteFile(r.stub, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	r.env = []string{
		"CERTSYNC_AGENT_HOST=" + r.agent, "CERTSYNC_GATEWAY_HOST=" + r.gateway,
		"CERTSYNC_CERT_PATTERN=" + slash(r.src) + "/live/%s/fullchain.pem",
		"CERTSYNC_KEY_PATTERN=" + slash(r.src) + "/live/%s/privkey.pem",
		"CERTSYNC_DEST_DIR=" + slash(r.dest), "CERTSYNC_OWNER=", // no chown: the test does not run as root
		"CERTSYNC_SYSTEMCTL=" + slash(r.stub),
	}
	r.issue(r.agent, 90*24*time.Hour)
	r.issue(r.gateway, 90*24*time.Hour)
	return r
}

// issue writes a fresh self-signed certificate and key for host into the fake ACME tree.
func (r *certsyncRig) issue(host string, valid time.Duration) {
	r.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		r.t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(valid)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		r.t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		r.t.Fatal(err)
	}
	dir := filepath.Join(r.src, "live", host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Fatal(err)
	}
	write := func(name, typ string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	write("fullchain.pem", "CERTIFICATE", der)
	write("privkey.pem", "PRIVATE KEY", kb)
}

func (r *certsyncRig) script() string {
	return deployPath(r.t, "systemd", "certsync", "cryobackup-certsync")
}

// run executes the script with extra environment entries and returns its exit code and output.
func (r *certsyncRig) run(extra ...string) (int, string) {
	r.t.Helper()
	// A hung script (for example openssl waiting for a passphrase) must fail the test, not stall it.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.sh, slash(r.script()))
	cmd.Env = append(append(os.Environ(), r.env...), extra...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		r.t.Fatalf("the script did not finish (a prompt?):\n%s", out.String())
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatalf("running the script: %v", err)
	}
	return code, out.String()
}

func (r *certsyncRig) restarts() []string {
	b, _ := os.ReadFile(r.ctlLog)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (r *certsyncRig) read(path ...string) string {
	b, err := os.ReadFile(filepath.Join(path...))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *certsyncRig) noLeftovers() {
	r.t.Helper()
	ents, err := os.ReadDir(r.dest)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			r.t.Errorf("a temporary file was left in the destination: %s", e.Name())
		}
	}
}

func TestCertsyncInstallsAtomicallyAndRestartsOnlyOnChange(t *testing.T) {
	r := newCertsyncRig(t)

	// First run: all four files installed, both units restarted.
	if code, out := r.run(); code != 0 {
		t.Fatalf("first run exit %d:\n%s", code, out)
	}
	for _, p := range []struct{ dst, host, name string }{
		{"agent.crt", r.agent, "fullchain.pem"}, {"agent.key", r.agent, "privkey.pem"},
		{"gateway.crt", r.gateway, "fullchain.pem"}, {"gateway.key", r.gateway, "privkey.pem"},
	} {
		if r.read(r.dest, p.dst) != r.read(r.src, "live", p.host, p.name) {
			t.Errorf("%s differs from its source", p.dst)
		}
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{"agent.crt", "agent.key", "gateway.crt", "gateway.key"} {
			if fi, err := os.Stat(filepath.Join(r.dest, f)); err != nil || fi.Mode().Perm() != 0o440 {
				t.Errorf("%s mode = %v, want 0440 (%v)", f, fi, err)
			}
		}
	}
	if got := r.restarts(); len(got) != 2 || !contains(got, "restart cryo-console.service") || !contains(got, "restart rclone-gateway.service") {
		t.Fatalf("restarts after the first run = %q", got)
	}
	r.noLeftovers()

	// Second run, nothing changed: no restart, no rewrite.
	if code, out := r.run(); code != 0 {
		t.Fatalf("idempotent run exit %d:\n%s", code, out)
	}
	if got := r.restarts(); len(got) != 2 {
		t.Fatalf("an unchanged run restarted something: %q", got)
	}

	// Only the agent's certificate renews: only its unit restarts.
	gwBefore := r.read(r.dest, "gateway.crt")
	r.issue(r.agent, 90*24*time.Hour)
	if code, out := r.run(); code != 0 {
		t.Fatalf("renewal run exit %d:\n%s", code, out)
	}
	got := r.restarts()
	if len(got) != 3 || got[2] != "restart cryo-console.service" {
		t.Fatalf("restarts after an agent renewal = %q, want one more: cryo-console", got)
	}
	if r.read(r.dest, "agent.crt") != r.read(r.src, "live", r.agent, "fullchain.pem") || r.read(r.dest, "gateway.crt") != gwBefore {
		t.Error("the renewal did not land on the agent pair only")
	}
	r.noLeftovers()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestCertsyncRefusesWhatItCannotTrust(t *testing.T) {
	r := newCertsyncRig(t)
	if code, out := r.run(); code != 0 {
		t.Fatalf("setup run exit %d:\n%s", code, out)
	}
	before := r.read(r.dest, "agent.crt") + r.read(r.dest, "agent.key")
	n := len(r.restarts())

	t.Run("a key that does not belong to the certificate", func(t *testing.T) {
		r.issue(r.agent, 90*24*time.Hour)
		other := r.read(r.src, "live", r.gateway, "privkey.pem") // a different key
		if err := os.WriteFile(filepath.Join(r.src, "live", r.agent, "privkey.pem"), []byte(other), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := r.run()
		if code == 0 || !strings.Contains(out, "does not match") {
			t.Fatalf("a mismatched pair exit %d:\n%s", code, out)
		}
		if r.read(r.dest, "agent.crt")+r.read(r.dest, "agent.key") != before || len(r.restarts()) != n {
			t.Error("a mismatched pair was installed or restarted a unit")
		}
	})

	t.Run("a missing source is an error and the other pair is still served", func(t *testing.T) {
		r.issue(r.agent, 90*24*time.Hour)
		r.issue(r.gateway, 90*24*time.Hour)
		if err := os.Remove(filepath.Join(r.src, "live", r.agent, "fullchain.pem")); err != nil {
			t.Fatal(err)
		}
		code, out := r.run()
		if code != 1 || !strings.Contains(out, r.agent) {
			t.Fatalf("a missing certificate exit %d, want 1 naming the host:\n%s", code, out)
		}
		if r.read(r.dest, "gateway.crt") != r.read(r.src, "live", r.gateway, "fullchain.pem") {
			t.Error("the gateway pair was not synced after the agent pair failed")
		}
	})

	t.Run("a host name that could escape the pattern", func(t *testing.T) {
		code, out := r.run("CERTSYNC_AGENT_HOST=../../etc")
		if code != 2 {
			t.Fatalf("a bad host name exit %d, want 2:\n%s", code, out)
		}
	})

	t.Run("no host configured at all", func(t *testing.T) {
		if code, _ := r.run("CERTSYNC_AGENT_HOST=", "CERTSYNC_GATEWAY_HOST="); code != 2 {
			t.Fatalf("no hosts exit %d, want 2", code)
		}
	})
}

func TestCertsyncDryRunAndExpiry(t *testing.T) {
	r := newCertsyncRig(t)
	if code, out := r.run("CERTSYNC_DRY_RUN=1"); code != 0 || !strings.Contains(out, "dry run") {
		t.Fatalf("dry run exit %d:\n%s", code, out)
	}
	if ents, _ := os.ReadDir(r.dest); len(ents) != 0 || len(r.restarts()) != 0 {
		t.Fatalf("a dry run changed something: %v, restarts %q", ents, r.restarts())
	}

	// A certificate the ACME client has left to run down is a loud failure, not a quiet one,
	// and it is still installed (it is the newest there is).
	r.issue(r.agent, 48*time.Hour)
	code, out := r.run()
	if code != 1 || !strings.Contains(out, "expires") {
		t.Fatalf("a near-expiry certificate exit %d, want 1 saying it expires:\n%s", code, out)
	}
	if r.read(r.dest, "agent.crt") != r.read(r.src, "live", r.agent, "fullchain.pem") {
		t.Error("the near-expiry certificate was not installed")
	}
	if code, out := r.run("CERTSYNC_WARN_DAYS=1"); code != 0 {
		t.Fatalf("with a 1-day threshold the same certificate exit %d:\n%s", code, out)
	}
}

// A step that fails while the pair is being prepared (chmod, chown) stops that
// pair: nothing is renamed into place, no unit restarts, the run exits non-zero
// and the work directory is gone. (set -e does not apply inside a function
// called as `f || rc=1`, so every step is checked by hand.)
func TestCertsyncStopsOnAFailedStep(t *testing.T) {
	failingChown := func(r *certsyncRig) string {
		p := filepath.Join(filepath.Dir(r.stub), "chown-stub")
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'chown: refused' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return slash(p)
	}
	for name, tc := range map[string]func(r *certsyncRig) []string{
		"chmod rejects the mode": func(r *certsyncRig) []string { return []string{"CERTSYNC_MODE=bogus"} },
		"chown fails": func(r *certsyncRig) []string {
			return []string{"CERTSYNC_OWNER=root:cryobackup", "CERTSYNC_CHOWN=" + failingChown(r)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newCertsyncRig(t)
			code, out := r.run(tc(r)...)
			if code == 0 {
				t.Fatalf("a failed step exited 0:\n%s", out)
			}
			if strings.Contains(out, "pair installed") || strings.Contains(out, ".service restarted") {
				t.Errorf("the run reported an install or restart after a failed step:\n%s", out)
			}
			if ents, _ := os.ReadDir(r.dest); len(ents) != 0 {
				t.Errorf("files reached the destination (or were left behind) after a failed step: %v", ents)
			}
			if got := r.restarts(); len(got) != 0 {
				t.Errorf("a unit restarted after a failed step: %q", got)
			}
		})
	}
}

// shim writes an executable named name into a fresh directory and returns the
// PATH entry that puts it ahead of the real one.
func shim(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	env := "PATH=" + slash(dir) + string(os.PathListSeparator) + os.Getenv("PATH")
	// Some Windows shells (the GitHub windows runner's) resolve core utilities
	// without honouring a PATH entry ahead of them; the script targets Linux,
	// where the Linux CI job runs these tests for real. Skip rather than test a
	// shim that is silently bypassed.
	if sh, err := exec.LookPath("sh"); err == nil {
		c := exec.Command(sh, "-c", "command -v "+name)
		c.Env = append(os.Environ(), env)
		if out, err := c.Output(); err != nil || !strings.Contains(slash(strings.TrimSpace(string(out))), slash(dir)) {
			t.Skipf("this sh does not resolve %s through a PATH shim (got %q); covered by the Linux test job", name, strings.TrimSpace(string(out)))
		}
	}
	return env
}

// Unparseable material must never be installed. A failed openssl parse piped
// into `openssl sha256` still yields the empty-input digest, so two garbage files
// compare equal unless each is parsed on its own first.
func TestCertsyncRejectsUnparseableMaterial(t *testing.T) {
	t.Run("sources that do not parse", func(t *testing.T) {
		r := newCertsyncRig(t)
		for _, host := range []string{r.agent, r.gateway} {
			for _, f := range []string{"fullchain.pem", "privkey.pem"} {
				if err := os.WriteFile(filepath.Join(r.src, "live", host, f), []byte("not a pem\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		code, out := r.run()
		if code == 0 || strings.Contains(out, "pair installed") {
			t.Fatalf("garbage sources: exit %d\n%s", code, out)
		}
		if ents, _ := os.ReadDir(r.dest); len(ents) != 0 || len(r.restarts()) != 0 {
			t.Errorf("garbage sources reached the destination or restarted a unit: %v %q", ents, r.restarts())
		}
	})
	t.Run("sources that parse but copies that do not", func(t *testing.T) {
		r := newCertsyncRig(t)
		// cat is only used to copy the pair into the work directory: here the copies come out as garbage.
		code, out := r.run(shim(t, "cat", "echo 'not a pem'\n"))
		if code == 0 || strings.Contains(out, "pair installed") || strings.Contains(out, ".service restarted") {
			t.Fatalf("garbage copies: exit %d\n%s", code, out)
		}
		if ents, _ := os.ReadDir(r.dest); len(ents) != 0 || len(r.restarts()) != 0 {
			t.Errorf("garbage copies reached the destination or restarted a unit: %v %q", ents, r.restarts())
		}
	})
}

// A passphrase-protected ACME key must fail at once, not make openssl prompt on
// the terminal: nothing installed, no restart, exit 1, a clear ERROR.
func TestCertsyncEncryptedKeyFailsWithoutPrompting(t *testing.T) {
	r := newCertsyncRig(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// A certificate for this key, then the key written passphrase-encrypted (legacy PEM, AES-256).
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: r.agent}, DNSNames: []string{r.agent},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := x509.EncryptPEMBlock(rand.Reader, "EC PRIVATE KEY", kder, []byte("correct horse"), x509.PEMCipherAES256) //nolint:staticcheck // legacy PEM encryption is what an encrypted ACME key looks like
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.src, "live", r.agent)
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	code, out := r.run("CERTSYNC_GATEWAY_HOST=") // only the encrypted pair
	if d := time.Since(began); d > 30*time.Second {
		t.Errorf("the run took %v: it waited for something", d)
	}
	if code != 1 || !strings.Contains(out, "encrypted or unreadable") {
		t.Fatalf("encrypted key exit %d, want 1 saying the key is encrypted or unreadable:\n%s", code, out)
	}
	if strings.Contains(out, "pair installed") || strings.Contains(out, ".service restarted") {
		t.Errorf("an encrypted key was installed or restarted a unit:\n%s", out)
	}
	if ents, _ := os.ReadDir(r.dest); len(ents) != 0 || len(r.restarts()) != 0 {
		t.Errorf("an encrypted key reached the destination or restarted a unit: %v %q", ents, r.restarts())
	}
}

// If the certificate cannot be moved after the key was, the previous key is put
// back: a restart for any other reason must never load a mismatched pair.
func TestCertsyncRollsBackTheKeyWhenTheCertificateMoveFails(t *testing.T) {
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Skip("no mv on PATH")
	}
	failCertMove := func(t *testing.T) string {
		return shim(t, "mv", "for a; do last=$a; done\ncase $last in *.crt) echo 'mv: forced failure' >&2; exit 1;; esac\nexec '"+slash(realMV)+"' \"$@\"\n")
	}

	t.Run("a previous pair is restored", func(t *testing.T) {
		r := newCertsyncRig(t)
		if code, out := r.run(); code != 0 {
			t.Fatalf("setup run exit %d:\n%s", code, out)
		}
		oldKey, oldCert, n := r.read(r.dest, "agent.key"), r.read(r.dest, "agent.crt"), len(r.restarts())
		r.issue(r.agent, 90*24*time.Hour) // a renewal, so the agent pair changes
		code, out := r.run(failCertMove(t))
		if code == 0 || !strings.Contains(out, "ERROR") {
			t.Fatalf("a failed certificate move exit %d:\n%s", code, out)
		}
		if r.read(r.dest, "agent.key") != oldKey || r.read(r.dest, "agent.crt") != oldCert {
			t.Error("the destination does not hold the previous, matching pair after the failed move")
		}
		if len(r.restarts()) != n {
			t.Errorf("a unit restarted after the failed move: %q", r.restarts())
		}
		r.noLeftovers()
	})

	t.Run("no previous pair: nothing is left behind", func(t *testing.T) {
		r := newCertsyncRig(t)
		code, out := r.run(failCertMove(t))
		if code == 0 {
			t.Fatalf("exit 0 after a failed certificate move:\n%s", out)
		}
		if ents, _ := os.ReadDir(r.dest); len(ents) != 0 || len(r.restarts()) != 0 {
			t.Errorf("a lone new key was left in the destination, or a unit restarted: %v %q", ents, r.restarts())
		}
	})
}
