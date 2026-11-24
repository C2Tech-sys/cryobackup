package cli

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/migrations"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P20-W3C: the mail section of doctor and its heartbeat line, against a fake relay.

// Markers that must never appear in doctor's output: the user name, the password and the relay's
// free-text reply.
const (
	mailUserMarker  = "USERMARK-4d1e"
	mailPassMarker  = "PASSMARK-8b2f"
	mailReplyMarker = "RELAYTEXT-6a9c"
	mailTestHost    = "mail.example.test"
)

// relayCfg scripts doctorRelay.
type relayCfg struct {
	silent   bool   // accept the connection and never greet
	user     string // the credentials the relay accepts
	pass     string
	noAuth   bool // offer no AUTH at all
	starttls bool
}

// doctorRelay is a small in-process SMTP relay for the doctor tests: greeting, EHLO, STARTTLS,
// AUTH PLAIN, QUIT. It records every command verb so a test can say that no message was sent.
type doctorRelay struct {
	ln   net.Listener
	cfg  relayCfg
	cert tls.Certificate
	pool *x509.CertPool
	mu   sync.Mutex
	cmds []string
}

func startDoctorRelay(t *testing.T, cfg relayCfg) *doctorRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &doctorRelay{ln: ln, cfg: cfg}
	r.cert, r.pool = relayCert(t)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(c)
		}
	}()
	return r
}

func relayCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: mailTestHost}, DNSNames: []string{mailTestHost},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func (r *doctorRelay) saw() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

func (r *doctorRelay) serve(conn net.Conn) {
	defer conn.Close()
	if r.cfg.silent {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	say := func(s string) { fmt.Fprint(conn, s+"\r\n") }
	br := bufio.NewReader(conn)
	tlsOn := false
	say("220 " + mailTestHost + " ESMTP " + mailReplyMarker)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		r.mu.Lock()
		r.cmds = append(r.cmds, verb)
		r.mu.Unlock()
		switch verb {
		case "EHLO", "HELO":
			lines := []string{mailTestHost + " " + mailReplyMarker}
			if r.cfg.starttls && !tlsOn {
				lines = append(lines, "STARTTLS")
			}
			if tlsOn && !r.cfg.noAuth {
				lines = append(lines, "AUTH PLAIN")
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				say("250" + sep + l)
			}
		case "STARTTLS":
			say("220 2.0.0 go ahead")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{r.cert}, MinVersion: tls.VersionTLS12})
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, tlsOn = tc, true
			br = bufio.NewReader(conn)
			say = func(s string) { fmt.Fprint(conn, s+"\r\n") }
		case "AUTH":
			f := strings.Fields(line)
			blob := ""
			if len(f) >= 3 {
				b, _ := base64.StdEncoding.DecodeString(f[2])
				blob = string(b)
			}
			parts := strings.Split(blob, "\x00")
			if len(parts) == 3 && parts[1] == r.cfg.user && parts[2] == r.cfg.pass {
				say("235 2.7.0 ok " + mailReplyMarker)
			} else {
				say("535 5.7.8 bad credentials " + mailReplyMarker)
			}
		case "QUIT":
			say("221 bye " + mailReplyMarker)
			return
		default:
			say("250 ok " + mailReplyMarker) // MAIL, RCPT, DATA: recorded; a test fails on them
		}
	}
}

// aimProbe points doctor's probe at the relay: the configured host name is kept (the certificate
// is verified against it), the dial goes to the relay's port and the test CA is trusted.
func aimProbe(t *testing.T, r *doctorRelay, tweak func(*mail.Settings)) {
	t.Helper()
	prev := probeMail
	t.Cleanup(func() { probeMail = prev })
	probeMail = func(ctx context.Context, set mail.Settings) error {
		set.Roots = r.pool
		set.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, r.ln.Addr().String())
		}
		if tweak != nil {
			tweak(&set)
		}
		return prev(ctx, set)
	}
}

// setConfig rewrites keys of the config file at cfgPath.
func setConfig(t *testing.T, cfgPath string, keys map[string]any) {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range keys {
		m[k] = v
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// smtpBlock is a STARTTLS block with the marked user name and a password file holding pass.
func smtpBlock(t *testing.T, pass string) map[string]any {
	t.Helper()
	pf := filepath.Join(t.TempDir(), "smtp-pass")
	if err := os.WriteFile(pf, []byte(pass), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"host": mailTestHost, "port": 587, "security": "starttls", "username": mailUserMarker,
		"password_file": pf, "from": "console@example.com"}
}

// addRecipients writes recipients through the store, then closes it.
func addRecipients(t *testing.T, db string, in ...store.RecipientInput) {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, r := range in {
		if _, err := st.CreateRecipient(context.Background(), store.ScopeAll(), r, store.AuditActor{Username: "test", Method: "api"}); err != nil {
			t.Fatal(err)
		}
	}
}

// mailLines returns doctor's lines of the mail section and the heartbeat line.
func mailLines(out string) string {
	var b strings.Builder
	for _, l := range strings.Split(out, "\n") {
		if len(l) > 12 && (strings.HasPrefix(l[12:], "mail") || strings.HasPrefix(l[12:], "heartbeat")) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// noLeak fails when doctor's output carries a marker.
func noLeak(t *testing.T, out string) {
	t.Helper()
	for _, m := range []string{mailUserMarker, mailPassMarker, mailReplyMarker} {
		if strings.Contains(out, m) {
			t.Fatalf("doctor printed %q:\n%s", m, out)
		}
	}
}

func noMessage(t *testing.T, r *doctorRelay) {
	t.Helper()
	for _, v := range r.saw() {
		if v == "MAIL" || v == "RCPT" || v == "DATA" {
			t.Fatalf("doctor's probe wrote %s: %v", v, r.saw())
		}
	}
}

// writeMailOutput keeps a copy of doctor's output for the report when DOCTOR_MAIL_OUT names a directory.
func writeMailOutput(t *testing.T, name, out string) {
	if dir := os.Getenv("DOCTOR_MAIL_OUT"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, name+".txt"), []byte(out), 0o644)
	}
}

func TestDoctorMailNotConfigured(t *testing.T) {
	cfgPath, _ := doctorRig(t)
	code, out, _ := run(t, "", "doctor", "--config", cfgPath)
	writeMailOutput(t, "not-configured", out)
	want := fmt.Sprintf("%-11s %-22s %s\n", "WARN", "mail", "not configured: alert e-mail and the daily digest are off")
	if !strings.Contains(out, want) {
		t.Fatalf("doctor without an smtp block:\n%s", out)
	}
	if strings.Contains(mailLines(out), "FAIL") || strings.Contains(out, "mail relay") || strings.Contains(out, "mail alerts") {
		t.Fatalf("the mail section without an smtp block fails, probes or warns about alerts:\n%s", out)
	}
	// The recipients are still counted (none), and the digest, on by default, reaches nobody.
	for _, w := range []string{dline("OK", "mail recipients", "0 in all"), doctorLine("WARN", "mail digest")} {
		if !strings.Contains(out, w) {
			t.Fatalf("doctor lacks %q:\n%s", w, out)
		}
	}
	if code != ExitOK && !strings.Contains(strings.Replace(out, mailLines(out), "", 1), "FAIL") {
		t.Fatalf("exit %d with no FAIL line:\n%s", code, out)
	}
}

// rigBaseline are the doctor lines that carry no mail and may read WARN or FAIL on doctorRig's console
// whatever the mail setup is: the rig has no TLS pair (config), no offline master-key copy to confirm,
// no swap check on some platforms and no maintenance worker listening. A healthy mail setup adds no
// WARN or FAIL line outside this set (P21-T6 U1: the old check read only the lines whose name begins
// with "mail", and the admin_url line's name escapes that filter).
var rigBaseline = map[string]bool{"config": true, "master key copy": true, "swap": true, "maintenance worker": true}

// levelLines are the lines of doctor's output that carry one of the given levels (WARN, FAIL, ...) and
// whose name is not in rigBaseline. The level is the first column, 11 wide; the name the next 22.
func levelLines(out string, levels ...string) []string {
	var got []string
	for _, l := range strings.Split(out, "\n") {
		if len(l) <= 12 || l[11] != ' ' || rigBaseline[strings.TrimSpace(l[12:min(len(l), 34)])] {
			continue
		}
		for _, lv := range levels {
			if strings.TrimSpace(l[:11]) == lv {
				got = append(got, l)
			}
		}
	}
	return got
}

// TestRigBaselineIsNotMail pins the baseline above to what the rig really prints without any mail
// setting: nothing in it may be a mail line, and every WARN or FAIL the rig prints outside "mail*" and
// "heartbeat" lines is in it. It keeps rigBaseline from growing to hide a mail line.
func TestRigBaselineIsNotMail(t *testing.T) {
	for name := range rigBaseline {
		if strings.HasPrefix(name, "mail") || strings.HasPrefix(name, "admin_url") || strings.HasPrefix(name, "heartbeat") {
			t.Errorf("rigBaseline holds a mail line: %q", name)
		}
	}
	cfgPath, _ := doctorRig(t)
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	for _, l := range strings.Split(out, "\n") {
		if len(l) <= 12 || l[11] != ' ' {
			continue
		}
		name := strings.TrimSpace(l[12:min(len(l), 34)])
		if lv := strings.TrimSpace(l[:11]); (lv == "WARN" || lv == "FAIL") && !rigBaseline[name] && !strings.HasPrefix(name, "mail") && !strings.HasPrefix(name, "heartbeat") && name != "admin_url" {
			t.Errorf("the bare rig prints %s %q, which is neither mail nor in rigBaseline:\n%s", lv, name, out)
		}
	}
}

func TestDoctorMailHealthy(t *testing.T) {
	cfgPath, db := doctorRig(t)
	r := startDoctorRelay(t, relayCfg{starttls: true, user: mailUserMarker, pass: mailPassMarker})
	aimProbe(t, r, nil)
	setConfig(t, cfgPath, map[string]any{"smtp": smtpBlock(t, mailPassMarker+"\n"), "admin_url": "https://console.example.net/"})
	addRecipients(t, db,
		store.RecipientInput{Name: "Ops", Address: "ops@example.test", Alerts: true, Recoveries: true, Digest: true},
		store.RecipientInput{Name: "Desk", Address: "desk@example.test", Digest: true})
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	writeMailOutput(t, "healthy", out)
	noLeak(t, out)
	noMessage(t, r)
	for _, w := range []string{
		dline("OK", "mail config", "relay mail.example.test:587, security starttls"),
		doctorLine("OK", "mail password file"),
		dline("OK", "mail relay", "mail.example.test:587: connected, greeted, STARTTLS, signed in, quit; no message sent"),
		dline("OK", "mail recipients", "2 in all: 1 with alerts, 1 with recoveries, 2 with the digest"),
		dline("OK", "mail digest", "on"),
		dline("OK", "admin_url", "e-mails link to https://console.example.net/ (admin_url)"),
	} {
		if !strings.Contains(out, w) {
			t.Fatalf("doctor lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(mailLines(out), "FAIL") || strings.Contains(mailLines(out), "WARN") {
		t.Fatalf("a healthy mail setup warns or fails:\n%s", mailLines(out))
	}
	// Every line of the output, not only the mail* ones: the admin_url line's name escapes that filter.
	if bad := levelLines(out, "WARN", "FAIL"); len(bad) != 0 {
		t.Fatalf("a healthy mail setup has WARN or FAIL lines:\n%s\nfull output:\n%s", strings.Join(bad, "\n"), out)
	}
	saw := strings.Join(r.saw(), " ")
	if !strings.Contains(saw, "STARTTLS") || !strings.Contains(saw, "AUTH") || !strings.Contains(saw, "QUIT") {
		t.Fatalf("relay saw %s", saw)
	}
}

// The counterpart of TestDoctorMailHealthy: the same setup with no admin_url has exactly one WARN, the
// admin_url line (beyond the rig's non-mail baseline), and nothing fails. Mail works; the messages carry no console link.
func TestDoctorMailHealthyWithoutAdminURLWarnsOnce(t *testing.T) {
	cfgPath, db := doctorRig(t)
	r := startDoctorRelay(t, relayCfg{starttls: true, user: mailUserMarker, pass: mailPassMarker})
	aimProbe(t, r, nil)
	setConfig(t, cfgPath, map[string]any{"smtp": smtpBlock(t, mailPassMarker+"\n")})
	addRecipients(t, db,
		store.RecipientInput{Name: "Ops", Address: "ops@example.test", Alerts: true, Recoveries: true, Digest: true},
		store.RecipientInput{Name: "Desk", Address: "desk@example.test", Digest: true})
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	writeMailOutput(t, "healthy-no-admin-url", out)
	noLeak(t, out)
	noMessage(t, r)
	want := dline("WARN", "admin_url", "admin_url is not set: e-mails carry no console link. Set admin_url to the address you type to open the console (not public_url, which is the agent API)")
	if got := levelLines(out, "WARN"); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Fatalf("want exactly one WARN, the admin_url line; got %d:\n%s\nfull output:\n%s", len(got), strings.Join(got, "\n"), out)
	}
	if bad := levelLines(out, "FAIL"); len(bad) != 0 {
		t.Fatalf("a missing admin_url fails doctor:\n%s", out)
	}
	if !strings.Contains(out, dline("OK", "mail relay", "mail.example.test:587: connected, greeted, STARTTLS, signed in, quit; no message sent")) {
		t.Fatalf("the relay is no longer probed:\n%s", out)
	}
}

func TestDoctorMailAuthFailure(t *testing.T) {
	cfgPath, _ := doctorRig(t)
	r := startDoctorRelay(t, relayCfg{starttls: true, user: mailUserMarker, pass: "the-right-one"})
	aimProbe(t, r, nil)
	setConfig(t, cfgPath, map[string]any{"smtp": smtpBlock(t, mailPassMarker)})
	code, out, _ := run(t, "", "doctor", "--config", cfgPath)
	writeMailOutput(t, "auth-failure", out)
	noLeak(t, out)
	noMessage(t, r)
	want := dline("FAIL", "mail relay", "mail.example.test:587: failed at auth: auth_rejected. "+mail.Hint(mail.ClassAuthRejected))
	if code != ExitFailed || !strings.Contains(out, want) {
		t.Fatalf("doctor with a rejected sign-in: %d\n%s", code, out)
	}
	// Nobody has the alerts switch while SMTP is configured.
	if !strings.Contains(out, doctorLine("WARN", "mail alerts")) {
		t.Fatalf("no alerts warning:\n%s", out)
	}
}

func TestDoctorMailOldDatabase(t *testing.T) {
	cfgPath, db := doctorRig(t)
	r := startDoctorRelay(t, relayCfg{starttls: true, user: mailUserMarker, pass: mailPassMarker})
	aimProbe(t, r, nil)
	setConfig(t, cfgPath, map[string]any{"smtp": smtpBlock(t, mailPassMarker)})
	ms, err := migrations.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(db + suffix)
	}
	olderDatabase(t, db, len(ms)-2)
	before := fileHash(t, db)
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	writeMailOutput(t, "old-database", out)
	if fileHash(t, db) != before {
		t.Fatal("doctor changed the older database")
	}
	want := doctorLine("NOT CHECKED", "mail recipients") + fmt.Sprintf("        the database is at schema version %d and this binary reads schema version %d: checked after the upgrade", len(ms)-2, len(ms))
	if !strings.Contains(out, want) || strings.Contains(out, "mail alerts") || strings.Contains(out, "mail digest") {
		t.Fatalf("doctor on an older database:\n%s", out)
	}
	if !strings.Contains(out, doctorLine("OK", "mail relay")) {
		t.Fatalf("the relay is still probed on an older database:\n%s", out)
	}
}

// A relay that accepts the connection and never greets ends the probe within its clocks (the
// greeting clock is shortened here; doctor's own bound is doctorProbeTimeout).
func TestDoctorMailSilentRelayEndsInTime(t *testing.T) {
	cfgPath, _ := doctorRig(t)
	r := startDoctorRelay(t, relayCfg{silent: true})
	aimProbe(t, r, func(s *mail.Settings) { s.GreetingTimeout = 300 * time.Millisecond })
	setConfig(t, cfgPath, map[string]any{"smtp": smtpBlock(t, mailPassMarker)})
	start := time.Now()
	code, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("doctor against a silent relay took %v", d)
	}
	want := dline("FAIL", "mail relay", "mail.example.test:587: failed at connect: timeout. "+mail.Hint(mail.ClassTimeout))
	if code != ExitFailed || !strings.Contains(out, want) {
		t.Fatalf("doctor against a silent relay: %d\n%s", code, out)
	}
	noLeak(t, out)
}

// The doctor probe's own bound is the context it hands the probe.
func TestDoctorProbeHasABound(t *testing.T) {
	prev := probeMail
	t.Cleanup(func() { probeMail = prev })
	var got time.Duration
	probeMail = func(ctx context.Context, _ mail.Settings) error {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("the probe has no deadline")
		}
		got = time.Until(dl)
		return nil
	}
	cfg := config.Default()
	cfg.SMTP = config.SMTP{Host: mailTestHost, Port: 587, Security: "starttls", From: "console@example.com"}
	checkMailRelay(cfg, func(string, string, string) {})
	if got <= 0 || got > doctorProbeTimeout {
		t.Fatalf("probe deadline in %v, want within %v", got, doctorProbeTimeout)
	}
}

// Every way the password file can be unusable is a FAIL that names it, skips the probe, and never
// prints the path, the user name or the contents.
func TestDoctorMailPasswordFileBranches(t *testing.T) {
	type pfCase struct {
		name  string
		setup func(t *testing.T, dir string) string // returns the password_file value
		want  string
	}
	cases := []pfCase{
		{"unset", func(*testing.T, string) string { return "" }, "smtp.username is set but smtp.password_file is not"},
		{"missing", func(_ *testing.T, dir string) string { return filepath.Join(dir, "nope") }, "smtp.password_file does not exist"},
		{"a directory", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "d")
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			return p
		}, "smtp.password_file is not a regular file"},
		{"empty", func(t *testing.T, dir string) string { return writeSecret(t, dir, "", 0o600) }, "smtp.password_file is empty"},
		{"two lines", func(t *testing.T, dir string) string { return writeSecret(t, dir, mailPassMarker+"\nsecond\n", 0o600) },
			"smtp.password_file cannot be used: it must hold the password on one line"},
		{"a line end only", func(t *testing.T, dir string) string { return writeSecret(t, dir, "\r\n", 0o600) },
			"smtp.password_file cannot be used: it must hold the password on one line"},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, pfCase{"group-readable", func(t *testing.T, dir string) string { return writeSecret(t, dir, mailPassMarker, 0o640) },
			"smtp.password_file is open to group or others (mode 0640): the sender refuses it, so no mail is sent; chmod 600 it"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _ := doctorRig(t)
			r := startDoctorRelay(t, relayCfg{starttls: true, user: mailUserMarker, pass: mailPassMarker})
			aimProbe(t, r, nil)
			dir := t.TempDir()
			pf := tc.setup(t, dir)
			blk := smtpBlock(t, mailPassMarker)
			blk["password_file"] = pf
			setConfig(t, cfgPath, map[string]any{"smtp": blk})
			code, out, _ := run(t, "", "doctor", "--config", cfgPath)
			noLeak(t, out)
			if pf != "" && strings.Contains(out, pf) {
				t.Fatalf("doctor printed the password file's path:\n%s", out)
			}
			wantFail := fmt.Sprintf("%-11s %-22s %s\n", "FAIL", "mail password file", tc.want)
			wantSkip := doctorLine("NOT CHECKED", "mail relay")
			if code != ExitFailed || !strings.Contains(out, wantFail) || !strings.Contains(out, wantSkip) {
				t.Fatalf("password file %s: %d\n%s", tc.name, code, mailLines(out))
			}
			if len(r.saw()) != 0 {
				t.Fatalf("the relay was contacted with an unusable password file: %v", r.saw())
			}
		})
	}
}

func writeSecret(t *testing.T, dir, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "smtp-pass")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // WriteFile's mode is subject to the umask
		t.Fatal(err)
	}
	return p
}

// The smtp block is validated by the functions serve and every send use: config.Validate's smtp
// problems, and mail.Settings.Validate for what only the sender checks (here a sender address
// that holds an "@" but does not parse). A failure skips the probe.
func TestDoctorMailConfigProblems(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"plain without allow_plaintext", func(b map[string]any) { b["security"] = "plain"; b["username"] = "" },
			"smtp.security plain sends mail unencrypted"},
		{"no port", func(b map[string]any) { b["port"] = 0 }, "smtp.port must be between 1 and 65535"},
		{"a sender that does not parse", func(b map[string]any) { b["from"] = "not an address @" },
			"config_invalid: " + mail.Hint(mail.ClassConfigInvalid)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _ := doctorRig(t)
			r := startDoctorRelay(t, relayCfg{starttls: true, user: mailUserMarker, pass: mailPassMarker})
			aimProbe(t, r, nil)
			blk := smtpBlock(t, mailPassMarker)
			tc.edit(blk)
			setConfig(t, cfgPath, map[string]any{"smtp": blk})
			code, out, _ := run(t, "", "doctor", "--config", cfgPath)
			noLeak(t, out)
			if code != ExitFailed || !strings.Contains(out, doctorLine("FAIL", "mail config")) || !strings.Contains(out, tc.want) ||
				!strings.Contains(out, doctorLine("NOT CHECKED", "mail relay")) {
				t.Fatalf("%s: %d\n%s", tc.name, code, out)
			}
			if len(r.saw()) != 0 {
				t.Fatalf("the relay was contacted with invalid settings: %v", r.saw())
			}
		})
	}
}

// The leak test across every outcome of the probe: whatever the relay answers, doctor prints
// neither the user name nor the password nor the relay's text.
func TestDoctorMailNeverPrintsSecretsOrRelayText(t *testing.T) {
	for _, rc := range []relayCfg{
		{starttls: true, user: mailUserMarker, pass: mailPassMarker},  // healthy
		{starttls: true, user: mailUserMarker, pass: "other"},         // auth rejected
		{starttls: true, user: mailUserMarker, noAuth: true},          // auth unsupported
		{starttls: false, user: mailUserMarker, pass: mailPassMarker}, // starttls missing
	} {
		cfgPath, _ := doctorRig(t)
		r := startDoctorRelay(t, rc)
		aimProbe(t, r, nil)
		setConfig(t, cfgPath, map[string]any{"smtp": smtpBlock(t, mailPassMarker)})
		_, out, errOut := run(t, "", "doctor", "--config", cfgPath)
		noLeak(t, out)
		noLeak(t, errOut)
		noMessage(t, r)
	}
}

// Without an smtp block nothing in the mail section fails doctor: a FAIL there (here the
// recipients cannot be read from a closed store) prints as WARN.
func TestDoctorMailAbsentNeverFails(t *testing.T) {
	_, db := doctorRig(t)
	st, _, err := store.OpenNoMigrate(context.Background(), store.Options{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	var lines []string
	checkMail(config.Default(), st, "", func(level, name, detail string) { lines = append(lines, level+" "+name+" "+detail) })
	got := strings.Join(lines, "\n")
	if strings.Contains(got, "FAIL") || !strings.Contains(got, "WARN mail recipients cannot be read") {
		t.Fatalf("absent smtp block:\n%s", got)
	}
	// With the block present the same read failure is a FAIL.
	cfg := config.Default()
	cfg.SMTP = config.SMTP{Host: mailTestHost}
	lines = nil
	checkMailRecipients(true, st, "", func(level, name, detail string) { lines = append(lines, level+" "+name) })
	if !strings.Contains(strings.Join(lines, "\n"), "FAIL mail recipients") {
		t.Fatalf("present smtp block: %v", lines)
	}
}

// The mail section changes nothing in the database: main file and WAL hash the same before and after.
func TestDoctorMailDoesNotChangeTheDatabase(t *testing.T) {
	cfgPath, db := doctorRig(t)
	addRecipients(t, db, store.RecipientInput{Name: "Ops", Address: "ops@example.test", Alerts: true})
	r := startDoctorRelay(t, relayCfg{starttls: true, user: mailUserMarker, pass: mailPassMarker})
	aimProbe(t, r, nil)
	setConfig(t, cfgPath, map[string]any{"smtp": smtpBlock(t, mailPassMarker)})
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	st, pending, err := store.OpenNoMigrate(context.Background(), store.Options{Path: db})
	if err != nil || len(pending) != 0 {
		t.Fatalf("open: %v %v", err, pending)
	}
	defer st.Close()
	hash := func() string {
		h := fileHash(t, db)
		if _, err := os.Stat(db + "-wal"); err == nil {
			h += "/" + fileHash(t, db+"-wal")
		}
		return h
	}
	before := hash()
	var out strings.Builder
	checkMail(cfg, st, "", func(level, name, detail string) { fmt.Fprintf(&out, "%s %s %s\n", level, name, detail) })
	if hash() != before {
		t.Fatalf("the mail section changed the database:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "OK mail recipients 1 in all: 1 with alerts") {
		t.Fatalf("mail section:\n%s", out.String())
	}
}

// The heartbeat line: off, or on with scheme and host only (never the path or the token), the
// method and interval, and doctor does not ping it.
func TestDoctorHeartbeatLine(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	cfgPath, _ := doctorRig(t)
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if !strings.Contains(out, dline("OK", "heartbeat ping", "off: healthcheck_ping.url is empty")) {
		t.Fatalf("no heartbeat line:\n%s", out)
	}
	pingURL := srv.URL + "/ping/TOKENMARK-31c7?k=TOKENMARK-31c7"
	setConfig(t, cfgPath, map[string]any{"healthcheck_ping": map[string]any{"url": pingURL, "method": "POST", "interval_s": 600}})
	_, out, _ = run(t, "", "doctor", "--config", cfgPath)
	want := dline("NOT CHECKED", "heartbeat ping", "configured: http://127.0.0.1, POST every 600 s; doctor does not ping it")
	if !strings.Contains(out, want) || strings.Contains(out, "TOKENMARK") || strings.Contains(out, srv.URL) {
		t.Fatalf("heartbeat line:\n%s", out)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("doctor pinged the monitor %d times", n)
	}
}

// ADDED after the fifth blind check of P20-W2BF: an unreadable -wal is named as the cause, and an
// open failure with no visible cause blames nothing. The open failure is injected (openNoMigrate)
// and so is the unreadable -wal (openForReading): P20-W3D 5, the fault held on every OS only
// then (a directory at the -shm name does not fail the open on Linux).
func TestDoctorNamesAnUnreadableWAL(t *testing.T) {
	cfgPath, db := doctorRig(t)
	injectOpenError(t, "store: open: unable to open database file: out of memory (14)")
	if err := os.WriteFile(db+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := openForReading
	t.Cleanup(func() { openForReading = prev })
	openForReading = func(path string) error {
		if path == db+"-wal" {
			return os.ErrPermission
		}
		return prev(path)
	}
	code, out, _ := run(t, "", "doctor", "--config", cfgPath)
	want := fmt.Sprintf("%-11s %-22s %s\n", "FAIL", "database", "database -wal file is not readable by this user")
	if code != ExitFailed || !strings.Contains(out, want) || strings.Contains(out, "directory") || strings.Contains(out, "-shm index") {
		t.Fatalf("doctor on an unreadable -wal: %d\n%s", code, out)
	}
}

func TestDoctorOpenFailureCauses(t *testing.T) {
	unable := errors.New("store: open: unable to open database file: out of memory (14)")
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, db string)
		want   string
		forbid string
	}{
		{"no visible cause", func(t *testing.T, db string) {
			if err := os.WriteFile(db+"-shm", nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "database could not be opened read-only", "directory"},
		{"no -shm: SQLite had to create it", func(t *testing.T, db string) {
			_ = os.Remove(db + "-shm")
			// P20-W3D: the directory is blamed only once doctor has tested it; here it cannot write in it.
			prev := dirWritable
			t.Cleanup(func() { dirWritable = prev })
			dirWritable = func(string) bool { return false }
		},
			"must create its -shm index file next to it, and this account cannot (the directory is not writable for it)", "not readable"},
		{"a -shm this account cannot read", func(t *testing.T, db string) {
			if err := os.WriteFile(db+"-shm", nil, 0o600); err != nil {
				t.Fatal(err)
			}
			prev := openForReading
			t.Cleanup(func() { openForReading = prev })
			openForReading = func(path string) error {
				if path == db+"-shm" {
					return os.ErrPermission
				}
				return prev(path)
			}
		}, "database -shm file is not readable by this user", "directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, db := doctorRig(t)
			tc.setup(t, db)
			prev := openNoMigrate
			t.Cleanup(func() { openNoMigrate = prev })
			openNoMigrate = func(context.Context, store.Options) (*store.Store, []string, error) { return nil, nil, unable }
			code, out, _ := run(t, "", "doctor", "--config", cfgPath)
			if code != ExitFailed || !strings.Contains(out, doctorLine("FAIL", "database")) || !strings.Contains(out, tc.want) ||
				strings.Contains(out, tc.forbid) || strings.Contains(out, "unable to open database file") {
				t.Fatalf("%s: %d\n%s", tc.name, code, out)
			}
			if !strings.Contains(out, dline("NOT CHECKED", "mail recipients", dbNotOpened)) {
				t.Fatalf("the mail reads are not NOT CHECKED after a failed open:\n%s", out)
			}
		})
	}
}

// dline is one whole doctor line without its line end: status, check name and detail, padded as doctor pads them.
func dline(level, name, detail string) string {
	return fmt.Sprintf("%-11s %-22s %s", level, name, detail)
}
