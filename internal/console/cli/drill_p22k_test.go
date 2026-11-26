package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P22-K: serve in drill mode (ADR 0029, ruling C8). A drill copy is built over a real database
// whose config still names every outside party a production console talks to: a mail relay, a
// gateway, a heartbeat monitor and its own public agent endpoint. Each of them is a listener here
// that counts every connection; a drill copy must leave every count at zero.

// drillOutside counts the connections the outside parties of a drill config receive.
type drillOutside struct {
	smtp, gateway, ping, endpoint atomic.Int64
}

func (o *drillOutside) total() int64 {
	return o.smtp.Load() + o.gateway.Load() + o.ping.Load() + o.endpoint.Load()
}

// countingListener accepts and closes every connection, counting it.
func countingListener(t *testing.T, n *atomic.Int64) string {
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
			n.Add(1)
			_ = c.Close()
		}
	}()
	return l.Addr().String()
}

// drillConfig is a production config (init) pointed at counting outside parties; drill sets the key.
func drillConfig(t *testing.T, drill bool) (config.Config, *drillOutside) {
	t.Helper()
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	o := &drillOutside{}
	smtp := countingListener(t, &o.smtp)
	host, port, _ := net.SplitHostPort(smtp)
	p, _ := strconv.Atoi(port)
	cfg.SMTP = config.SMTP{Host: host, Port: p, Security: "plain", AllowPlaintext: true, From: "cryo@example.invalid"}
	gw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { o.gateway.Add(1); http.NotFound(w, r) }))
	t.Cleanup(gw.Close)
	cfg.GatewayURL = gw.URL
	ping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { o.ping.Add(1) }))
	t.Cleanup(ping.Close)
	cfg.HealthcheckPing.URL = ping.URL + "/ping"
	cfg.HealthcheckPing.IntervalS = config.HeartbeatMinIntervalS
	cfg.PublicURL = "https://" + countingListener(t, &o.endpoint)
	cfg.AgentListen, cfg.AdminListen = "127.0.0.1:"+strconv.Itoa(r3FreePort(t)), "127.0.0.1:0"
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	if drill {
		cfg.Drill = true
		cfg.TLS = config.TLS{} // a drill VM holds no agent certificate
	} else {
		cfg.InsecureHTTPLab = true
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg, o
}

func buildDrill(t *testing.T, cfg config.Config) (*App, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, &logs
}

// forbiddenInDrill are the jobs ADR 0029 names, plus every other job that sends mail, pings
// outside, writes to storage or changes the fleet.
var forbiddenInDrill = []string{
	jobs.NameHtpasswdSync, jobs.NameDevicePurge, jobs.NameHealthProber, jobs.NameAlarmEvaluator,
	jobs.NameDailyDigest, jobs.NameNotificationRetry, jobs.NameHealthcheckPing, jobs.NameAbandonedRunSweeper,
}

// TestDrillJobTableCoversEveryServeJob: every job a production serve registers (with mail and the
// heartbeat ping configured, so all of them register) has a decision in drillJobs, and the table
// names nothing serve does not schedule. A new job without a decision fails here.
func TestDrillJobTableCoversEveryServeJob(t *testing.T) {
	cfg, _ := drillConfig(t, false)
	app, _ := buildDrill(t, cfg)
	registered := map[string]bool{}
	for _, n := range app.Runner.Names() {
		registered[n] = true
	}
	table := map[string]bool{}
	for _, j := range DrillJobs() {
		if table[j.Name] {
			t.Errorf("job %s is listed twice", j.Name)
		}
		table[j.Name] = true
		if j.Why == "" {
			t.Errorf("job %s has no reason", j.Name)
		}
		if !registered[j.Name] {
			t.Errorf("drillJobs lists %s, which production serve does not schedule", j.Name)
		}
	}
	for n := range registered {
		if !table[n] {
			t.Errorf("production serve schedules %s, which drillJobs does not list: decide whether a drill copy runs it", n)
		}
	}
	for _, n := range forbiddenInDrill {
		if DrillRunsJob(n) {
			t.Errorf("drillJobs runs %s in a drill copy", n)
		}
	}
	if DrillRunsJob("a-job-nobody-listed") {
		t.Error("an unlisted job must not run in a drill copy")
	}
	// Production is unchanged: the probe-now and the endpoint probe are wired, mail is the relay.
	if app.drill || app.Admin.Prober == nil {
		t.Errorf("production wiring changed: drill=%v prober=%v", app.drill, app.Admin.Prober)
	}
	if _, fake := app.Mail.(*mail.FakeSender); fake || !app.TestMail.Configured {
		t.Errorf("production mail: fake sender %v, configured %v", fake, app.TestMail.Configured)
	}
}

// TestDrillServeRegistersOnlyLocalJobs: the job registry of a drill copy holds exactly the jobs the
// table runs, none of the forbidden ones, and Run now refuses a forbidden job by name.
func TestDrillServeRegistersOnlyLocalJobs(t *testing.T) {
	cfg, _ := drillConfig(t, true)
	app, logs := buildDrill(t, cfg)
	got := map[string]bool{}
	for _, n := range app.Runner.Names() {
		got[n] = true
	}
	for _, j := range DrillJobs() {
		if got[j.Name] != j.On {
			t.Errorf("drill copy: job %s registered=%v, table says %v (%s)", j.Name, got[j.Name], j.On, j.Why)
		}
	}
	if len(got) == 0 {
		t.Fatal("a drill copy still expires sessions: its registry cannot be empty")
	}
	for _, n := range forbiddenInDrill {
		if got[n] {
			t.Errorf("forbidden job %s is registered in a drill copy", n)
		}
		if _, err := app.Runner.RunOnce(context.Background(), n); err == nil || !strings.Contains(err.Error(), "unknown job") {
			t.Errorf("Run now of %s in a drill copy = %v, want unknown job", n, err)
		}
	}
	if app.Admin.Prober != nil {
		t.Error("a drill copy offers probe-now, which would reach the production gateway and relay")
	}
	if _, fake := app.Mail.(*mail.FakeSender); !fake {
		t.Errorf("drill mail sender = %T, want the in-process fake", app.Mail)
	}
	if app.Dispatch == nil || app.Dispatch.Sender != app.Mail {
		t.Error("the dispatcher does not send through the drill copy's fake sender")
	}
	if app.TestMail.Configured || app.Admin.Mail.Configured || app.Admin.Mail.Digest != nil {
		t.Errorf("drill mail: test configured %v, service configured %v, digest %v", app.TestMail.Configured, app.Admin.Mail.Configured, app.Admin.Mail.Digest)
	}
	if !strings.Contains(logs.String(), `"class":"drill"`) {
		t.Errorf("Build did not log drill mode:\n%s", logs.String())
	}
}

// TestDrillServeOpensNoAgentPortAndReachesNoOneOutside runs a drill copy as serve does, signs in
// over the admin listener, walks the page routes, waits past the runner's first-run delay (5 s at
// most) and then checks: the agent port was never opened, no outside party saw a connection, no
// forbidden job left a job_state row, the gateway's login file was not written, and the jobs that
// do run have run.
func TestDrillServeOpensNoAgentPortAndReachesNoOneOutside(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the job runner's first-run delay")
	}
	cfg, outside := drillConfig(t, true)
	app, _ := buildDrill(t, cfg)
	started := time.Now()
	base := r3Serve(t, app)

	if a := app.Server.AgentAddr(); a != "" {
		t.Fatalf("drill copy bound the agent listener at %s", a)
	}
	if c, err := net.DialTimeout("tcp", cfg.AgentListen, time.Second); err == nil {
		_ = c.Close()
		t.Fatalf("something answers on the agent port %s of a drill copy", cfg.AgentListen)
	}

	b := r3SignIn(t, app, base)
	for _, path := range []string{"/", "/customers", "/devices", "/policies", "/policies/new", "/activity", "/runs",
		"/fleet/overdue", "/fleet/unseen", "/system", "/enroll", "/enroll/new", "/downloads", "/audit",
		"/settings", "/settings/accounts", "/settings/email", "/account"} {
		res, body := b.do("GET", path, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s in a drill copy: %d %.200s", path, res.StatusCode, body)
		}
	}

	if wait := 6*time.Second - time.Since(started); wait > 0 {
		time.Sleep(wait)
	}
	if n := outside.total(); n != 0 {
		t.Fatalf("a drill copy reached outside: smtp %d, gateway %d, heartbeat ping %d, agent endpoint %d",
			outside.smtp.Load(), outside.gateway.Load(), outside.ping.Load(), outside.endpoint.Load())
	}
	if st := app.Endpoint.Status(); !st.Started.IsZero() || st.Known {
		t.Errorf("the agent endpoint self-probe ran in a drill copy: %+v", st)
	}
	ctx := context.Background()
	for _, n := range forbiddenInDrill {
		js, err := app.Store.GetJobState(ctx, n)
		if err == nil && (js.RunCount > 0 || js.LastStatus != "") {
			t.Errorf("forbidden job %s ran in a drill copy: %+v", n, js)
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.GatewayAuthDir, gateway.FileName)); err == nil {
		t.Error("a drill copy wrote the gateway's login file")
	}
	ran := 0
	for _, j := range DrillJobs() {
		if !j.On {
			continue
		}
		if js, err := app.Store.GetJobState(ctx, j.Name); err == nil && js.RunCount > 0 {
			ran++
		}
	}
	if ran == 0 {
		t.Error("none of the drill copy's own jobs ran within the runner's first-run delay: the test proves nothing")
	}
}

// TestDoctorSaysDrill: doctor prints the drill line either way, WARN when on.
func TestDoctorSaysDrill(t *testing.T) {
	for _, drill := range []bool{false, true} {
		cfg, _ := drillConfig(t, drill)
		path := filepath.Join(t.TempDir(), "config.json")
		raw := `{"data_dir": ` + strconv.Quote(cfg.DataDir) + `, "db_path": ` + strconv.Quote(cfg.DBPath) +
			`, "master_key_path": ` + strconv.Quote(cfg.MasterKeyPath) + `, "insecure_http_for_lab": true, "agent_listen": "127.0.0.1:0", "drill": ` + strconv.FormatBool(drill) + `}`
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		_, out, _ := run(t, "", "doctor", "--config", path)
		var line string
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[1] == "drill" {
				line = l
			}
		}
		switch {
		case line == "":
			t.Errorf("drill=%v: doctor printed no drill line:\n%s", drill, out)
		case drill && (!strings.HasPrefix(line, "WARN") || !strings.Contains(line, "drill copy")):
			t.Errorf("drill on: %q", line)
		case !drill && (!strings.HasPrefix(line, "OK") || !strings.Contains(line, "off")):
			t.Errorf("drill off: %q", line)
		}
	}
}

// drillBannerText is the DRILL COPY strip's sentence (FIX BATCH 1, K1).
const drillBannerText = "DRILL COPY: this console is a restored copy. It does not talk to agents, send mail or change storage."

// drillPageRoutes are the console's page routes without a path parameter (every one renders
// base.html).
var drillPageRoutes = []string{"/", "/customers", "/devices", "/policies", "/policies/new", "/activity", "/runs",
	"/fleet/overdue", "/fleet/unseen", "/system", "/enroll", "/enroll/new", "/downloads", "/audit",
	"/settings", "/settings/accounts", "/settings/email", "/account"}

// TestDrillCopyBannerOnEveryPage (FIX BATCH 1, K1): in a drill copy the DRILL COPY strip is on the
// three bare pages (sign-in, password reset, MFA) and on every page route, as an alert with no
// close button; in a production console it is on none of them.
func TestDrillCopyBannerOnEveryPage(t *testing.T) {
	for _, drill := range []bool{true, false} {
		name := "production"
		if drill {
			name = "drill"
		}
		t.Run(name, func(t *testing.T) {
			cfg, _ := drillConfig(t, drill)
			app, _ := buildDrill(t, cfg)
			base := r3Serve(t, app)
			check := func(what, body string) {
				t.Helper()
				has := strings.Contains(body, drillBannerText)
				if has != drill {
					t.Errorf("%s: DRILL COPY strip present=%v, want %v", what, has, drill)
					return
				}
				if drill {
					// The strip is one element: from its opening tag to the end of its sentence's block.
					i := strings.Index(body, `<div class="banner crit drill-copy"`)
					j := strings.Index(body[max(i, 0):], "</b></div></div>")
					if i < 0 || j < 0 || strings.Count(body, `id="drill-copy"`) != 1 {
						t.Errorf("%s: no single DRILL COPY strip element", what)
						return
					}
					strip := body[i : i+j]
					if !strings.Contains(strip, `role="alert"`) || !strings.Contains(strip, drillBannerText) {
						t.Errorf("%s: the strip is not an alert carrying the sentence: %.300s", what, strip)
					}
					if strings.Contains(strip, "<button") || strings.Contains(strip, "dismiss") {
						t.Errorf("%s: the strip can be dismissed: %.300s", what, strip)
					}
				}
			}
			anon := &r3Browser{t: t, base: base}
			_, body := anon.do("GET", "/login", nil)
			check("GET /login", body)
			_, body = anon.do("GET", "/login/reset?token=not-a-token", nil)
			check("GET /login/reset", body)

			b := r3SignIn(t, app, base) // the owner enrolls TOTP on the way in
			for _, path := range drillPageRoutes {
				res, body := b.do("GET", path, nil)
				if res.StatusCode != http.StatusOK {
					t.Errorf("GET %s: %d %.200s", path, res.StatusCode, body)
					continue
				}
				check("GET "+path, body)
			}
			// The MFA page: a second sign-in of the same owner lands on its verify step.
			again := &r3Browser{t: t, base: base}
			res, body := again.do("POST", "/login", url.Values{"username": {"r3-owner"}, "password": {"r3-owner-password-1"}})
			if res.StatusCode != http.StatusSeeOther {
				t.Fatalf("second sign-in: %d %.200s", res.StatusCode, body)
			}
			res, body = again.do("GET", "/login/mfa", nil)
			if res.StatusCode != http.StatusOK || !strings.Contains(body, "Verify it is you") {
				t.Fatalf("GET /login/mfa: %d %.200s", res.StatusCode, body)
			}
			check("GET /login/mfa", body)
		})
	}
}

// TestDoctorDrillContactsNoOutsideParty (FIX BATCH 2, K3): doctor against a drill config whose smtp
// block, gateway, heartbeat URL and public URL all name counting listeners opens no connection to
// any of them and says so ("not checked: drill copy") for the mail relay; the same config without
// the drill key still runs the live relay probe (one connection), so the zero is the drill key's.
func TestDoctorDrillContactsNoOutsideParty(t *testing.T) {
	for _, drill := range []bool{true, false} {
		t.Run("drill="+strconv.FormatBool(drill), func(t *testing.T) {
			cfg, o := drillConfig(t, drill)
			path := filepath.Join(t.TempDir(), "config.json")
			raw := `{"data_dir": ` + strconv.Quote(cfg.DataDir) + `, "db_path": ` + strconv.Quote(cfg.DBPath) +
				`, "master_key_path": ` + strconv.Quote(cfg.MasterKeyPath) + `, "insecure_http_for_lab": true, "agent_listen": "127.0.0.1:0"` +
				`, "public_url": ` + strconv.Quote(cfg.PublicURL) + `, "gateway_url": ` + strconv.Quote(cfg.GatewayURL) +
				`, "healthcheck_ping": {"url": ` + strconv.Quote(cfg.HealthcheckPing.URL) + `, "interval_s": ` + strconv.Itoa(cfg.HealthcheckPing.IntervalS) + `}` +
				`, "smtp": {"host": ` + strconv.Quote(cfg.SMTP.Host) + `, "port": ` + strconv.Itoa(cfg.SMTP.Port) +
				`, "security": "plain", "allow_plaintext": true, "from": "cryo@example.invalid"}` +
				`, "drill": ` + strconv.FormatBool(drill) + `}`
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, out, _ := run(t, "", "doctor", "--config", path)
			var relay string
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, "mail relay") {
					relay = l
				}
			}
			if relay == "" {
				t.Fatalf("doctor printed no mail relay line:\n%s", out)
			}
			if drill {
				if n := o.total(); n != 0 {
					t.Errorf("doctor in drill mode opened %d connection(s) to outside parties (smtp %d, gateway %d, ping %d, endpoint %d):\n%s",
						n, o.smtp.Load(), o.gateway.Load(), o.ping.Load(), o.endpoint.Load(), out)
				}
				if !strings.HasPrefix(relay, "NOT CHECKED") || !strings.Contains(relay, "not checked: drill copy") {
					t.Errorf("drill relay line = %q, want NOT CHECKED ... not checked: drill copy", relay)
				}
				return
			}
			if n := o.smtp.Load(); n != 1 {
				t.Errorf("non-drill doctor opened %d relay connection(s), want the one live probe:\n%s", n, out)
			}
			if strings.Contains(relay, "drill copy") {
				t.Errorf("non-drill relay line mentions the drill: %q", relay)
			}
		})
	}
}

// TestDrillBannerFollowsTheLastBuild (FIX BATCH 2, K5; P22-I: the switch is now ui.Service.Drill):
// the strip belongs to the console that was built, not to the process. A drill console and a
// production console built one after the other in the same process, with no cleanup between them,
// each answer for themselves, in either order: a process-wide switch would give both the last
// build's answer.
func TestDrillBannerFollowsTheLastBuild(t *testing.T) {
	has := func(app *App) bool {
		t.Helper()
		b := &r3Browser{t: t, base: r3Serve(t, app)}
		_, body := b.do("GET", "/login", nil)
		return strings.Contains(body, drillBannerText)
	}
	build := func(drill bool) *App {
		t.Helper()
		cfg, _ := drillConfig(t, drill)
		app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&bytes.Buffer{}, logx.LevelDebug)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Close)
		return app
	}
	first := build(true)
	if !has(first) {
		t.Fatal("the drill console shows no DRILL COPY strip: the test proves nothing")
	}
	second := build(false)
	if has(second) {
		t.Fatal("a production console built after a drill console shows the DRILL COPY strip")
	}
	if !has(first) {
		t.Fatal("building a production console took the strip off the drill console that was built before it")
	}
	third := build(true) // and back: each console keeps its own answer
	if !has(third) || has(second) || !has(first) {
		t.Fatalf("after a third build: drill-first=%v production-second=%v drill-third=%v, want true false true", has(first), has(second), has(third))
	}
}
