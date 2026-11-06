package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M3-T4 fix round 3: what only a real App can show. These drive cli.Build's App
// over HTTP: the same wiring `cryo-console serve` runs.

// r3Browser is a signed-in browser over HTTP.
type r3Browser struct {
	t    *testing.T
	base string
	cs   []*http.Cookie
	csrf string
}

func (c *r3Browser) do(method, path string, form url.Values) (*http.Response, string) {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", c.base)
	}
	for _, ck := range c.cs {
		req.AddCookie(ck)
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	for _, ck := range res.Cookies() {
		kept := c.cs[:0]
		for _, o := range c.cs {
			if o.Name != ck.Name {
				kept = append(kept, o)
			}
		}
		c.cs = append(kept, ck)
		if ck.Name == auth.CSRFCookie {
			c.csrf = ck.Value
		}
	}
	return res, string(b)
}

// login signs in with the password, then the forced TOTP enrollment.
func (c *r3Browser) login(user, pw string) {
	c.t.Helper()
	res, body := c.do("POST", "/login", url.Values{"username": {user}, "password": {pw}})
	if res.StatusCode != http.StatusSeeOther {
		c.t.Fatalf("login: %d %.300s", res.StatusCode, body)
	}
	_, body = c.do("GET", "/login/mfa", nil)
	m := regexp.MustCompile(`<pre class="code secret"[^>]*>([A-Z2-7 ]+)</pre>`).FindStringSubmatch(body)
	if m == nil {
		c.t.Fatalf("no enrollment key on the MFA page: %.300s", body)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(m[1], " ", ""))
	if err != nil {
		c.t.Fatal(err)
	}
	res, body = c.do("POST", "/login/mfa/enroll", url.Values{"code": {auth.TOTPCode(secret, time.Now())}, "csrf_token": {c.csrf}})
	if res.StatusCode != 200 || !strings.Contains(body, "Save your recovery codes") {
		c.t.Fatalf("enroll: %d %.300s", res.StatusCode, body)
	}
}

// r3SignIn creates an owner on a real App and signs it in.
func r3SignIn(t *testing.T, app *App, base string) *r3Browser {
	t.Helper()
	const user, pw = "r3-owner", "r3-owner-password-1"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.CreateAdmin(context.Background(), store.Admin{Username: user, Email: user + "@example.invalid", Role: "owner", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	c := &r3Browser{t: t, base: base}
	c.login(user, pw)
	return c
}

// ---- M1: a healthy console reads overall ok ----

// r3Cert writes a self-signed certificate for 127.0.0.1 that is its own CA, a year out.
func r3Cert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "r3"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "r3.crt"), filepath.Join(dir, "r3.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// r3SMTP is a relay that answers what the SMTP probe asks: the greeting, EHLO, QUIT.
func r3SMTP(t *testing.T) (host string, port int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.WriteString(c, "220 r3 ESMTP\r\n")
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					switch strings.ToUpper(strings.Fields(line + " x")[0]) {
					case "EHLO", "HELO":
						_, _ = io.WriteString(c, "250 r3\r\n")
					case "QUIT":
						_, _ = io.WriteString(c, "221 bye\r\n")
						return
					default:
						_, _ = io.WriteString(c, "250 ok\r\n")
					}
				}
			}()
		}
	}()
	a := l.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func r3FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// r3RealApp is cli.Build with real probes against real listeners: a TLS gateway, an
// SMTP relay, and the App's own TLS agent listener, which the self-probe reaches as an
// agent would. Only the worker is fake (fixtures.Backend answers Ping and CatConfig),
// and mod can change the configuration before Build.
func r3RealApp(t *testing.T, mod func(*config.Config)) *App {
	t.Helper()
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--timezone", "America/Chicago")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := r3Cert(t, dir)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // the probe login's repository does not exist yet: healthy (probe.CheckGateway)
	}))
	gw.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	gw.StartTLS()
	t.Cleanup(gw.Close)
	smtpHost, smtpPort := r3SMTP(t)
	port := strconv.Itoa(r3FreePort(t))
	cfg.AgentListen, cfg.AdminListen = "127.0.0.1:"+port, "127.0.0.1:0"
	cfg.TLS = config.TLS{Cert: certPath, Key: keyPath}
	cfg.PublicURL = "https://127.0.0.1:" + port
	cfg.AgentProbe.CAFile = certPath
	cfg.GatewayURL = gw.URL
	cfg.HealthProber.GatewayCAFile = certPath
	cfg.SMTP = config.SMTP{Host: smtpHost, Port: smtpPort, Security: "plain", From: "cryo@example.invalid"}
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	if mod != nil {
		mod(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

// r3Serve runs the App until the test ends and answers its admin address.
func r3Serve(t *testing.T, app *App) string {
	t.Helper()
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() { defer close(ended); _ = app.Run(ctx) }()
	t.Cleanup(func() { stop(); <-ended })
	return "http://" + app.Server.AdminAddr()
}

// r3MaintainPass is what one real `cryo-console maintain` pass writes to job_state
// (maintain.Worker.RunOnce and recordJob): the lease on maintenance-scheduler, then a run
// with status ok and the next tick as its due time, then the lease freed. The fake
// worker has no scheduler of its own, so the test writes it the same way. C07 and the
// maintenance_worker row read this row.
func r3MaintainPass(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if got, err := st.AcquireJobLease(ctx, maintain.SchedulerJob, "r3-maintain", time.Minute); err != nil || !got {
		t.Fatalf("scheduler lease: %v %v", got, err)
	}
	start := time.Now().UTC()
	end := start.Add(time.Second)
	next := end.Add(maintain.DefaultTickEvery)
	if err := st.RecordJobRun(ctx, maintain.SchedulerJob, store.JobRun{Status: "ok", StartedAt: start, FinishedAt: end, NextDueAt: &next}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseJobLease(ctx, maintain.SchedulerJob, "r3-maintain"); err != nil {
		t.Fatal(err)
	}
}

func r3Token(t *testing.T, app *App) string {
	t.Helper()
	bg := context.Background()
	admin, err := app.Store.GetAdminByUsername(bg, "owner")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := app.Auth.CreateAPIToken(bg, &auth.Principal{Admin: admin}, "r3", auth.ScopeFull, 7, "api", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func r3Health(t *testing.T, base, token string) adminapi.SystemHealthResponse {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/v1/system/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var h adminapi.SystemHealthResponse
	if err := json.NewDecoder(res.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func r3Rows(h adminapi.SystemHealthResponse) string {
	var b strings.Builder
	for _, c := range h.Components {
		b.WriteString("\n  " + c.ID + " = " + c.Status + " [" + strings.Join(c.Banners, ",") + "] " + c.Detail)
	}
	for _, x := range h.Banners {
		b.WriteString("\n  banner " + x.ID + ": " + x.Text)
	}
	return b.String()
}

// r3WaitFor polls the health answer until ok says it is what the test waits for.
func r3WaitFor(t *testing.T, base, token string, ok func(adminapi.SystemHealthResponse) bool) (adminapi.SystemHealthResponse, bool) {
	t.Helper()
	var h adminapi.SystemHealthResponse
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		h = r3Health(t, base, token)
		if ok(h) {
			return h, true
		}
	}
	return h, false
}

// The honest control of system health: real probes, a fake worker, and only facts
// this build can produce. Overall reaches ok, with the checks this build does not run
// and the clock check a small fleet cannot run said on their rows.
func TestR3AHealthyConsoleReadsOverallOK(t *testing.T) { // M3-T4 r3 M1
	app := r3RealApp(t, nil)
	r3MaintainPass(t, app.Store)
	token := r3Token(t, app)
	base := r3Serve(t, app)
	h, ok := r3WaitFor(t, base, token, func(h adminapi.SystemHealthResponse) bool { return h.Overall == "ok" })
	if !ok {
		t.Fatalf("a healthy console never read overall ok: %s%s", h.Overall, r3Rows(h))
	}
	want := map[string]string{"release_manifest": "not_in_build", "storage_quota": "not_in_build", "digest": "not_in_build", "self_backup": "not_in_build", "clock": "not_applicable", "fleet_data": "not_applicable"}
	for _, c := range h.Components {
		w, special := want[c.ID]
		if !special {
			w = "ok"
		}
		if c.Status != w {
			t.Errorf("%s = %s %q, want %s", c.ID, c.Status, c.Detail, w)
		}
		if special && c.Detail == "" {
			t.Errorf("%s is %s without saying why", c.ID, c.Status)
		}
		if c.ID == "clock" && !strings.Contains(c.Detail, "needs at least 5 devices") {
			t.Errorf("clock detail = %q", c.Detail)
		}
	}
	if len(h.Banners) != 0 {
		t.Errorf("banners on a healthy console: %s", r3Rows(h))
	}
	// The same on S36.
	c := r3SignIn(t, app, base)
	_, page := c.do("GET", "/system", nil)
	if !regexp.MustCompile(`id="sys-h" class="sys-title">Overall</h2><span class="st ok"><i aria-hidden="true"></i>OK<`).MatchString(page) {
		t.Errorf("S36 does not read overall OK: %.400s", page)
	}
	for _, s := range []string{"NOT IN THIS BUILD", "NOT APPLICABLE", "Needs at least 5 devices", "6 cannot be checked here", "No devices enrolled yet: nothing is being backed up"} {
		if !strings.Contains(page, s) {
			t.Errorf("S36 lacks %q", s)
		}
	}
}

// The fail-loud half: something that IS in this build and is not checked still blocks overall.
func TestR3NotCheckedForSomethingInThisBuildStillBlocksOverall(t *testing.T) { // M3-T4 r3 M1
	app := r3RealApp(t, func(c *config.Config) { c.SMTP = config.SMTP{Security: "starttls"} })
	r3MaintainPass(t, app.Store)
	token := r3Token(t, app)
	base := r3Serve(t, app)
	smtpRow := func(h adminapi.SystemHealthResponse) adminapi.SystemComponentJSON {
		for _, c := range h.Components {
			if c.ID == "smtp" {
				return c
			}
		}
		return adminapi.SystemComponentJSON{}
	}
	// Wait until everything else is ok: no probe is still on its way, and only the unconfigured relay is left.
	h, ok := r3WaitFor(t, base, token, func(h adminapi.SystemHealthResponse) bool {
		for _, c := range h.Components {
			if c.ID != "smtp" && c.Status != "ok" && c.Status != "not_in_build" && c.Status != "not_applicable" {
				return false
			}
		}
		return true
	})
	if !ok {
		t.Fatalf("the rest never settled: %s", r3Rows(h))
	}
	if c := smtpRow(h); c.Status != "not_checked" || len(c.Banners) != 1 || c.Banners[0] != "C03b" {
		t.Errorf("smtp with no relay configured = %+v, want not_checked under C03b", c)
	}
	if h.Overall != "not_checked" {
		t.Errorf("overall = %s with a row of this build not checked: %s", h.Overall, r3Rows(h))
	}
}
