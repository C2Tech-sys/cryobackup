package server_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M2-W2C: a command queued through the admin API or the UI reaches an agent that
// is sitting in its 30 s command long poll at once, because cli.Build wires the
// agent API's Notifier into both. The app is the real one (cli.Build, both
// listeners); the agent is a plain HTTP caller holding the poll open.
func TestQueuedCommandWakesAnOpenAgentLongPoll(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	out := runCLI(t, "init", "--config", cfgPath, "--data-dir", filepath.Join(dir, "data"), "--non-interactive", "--master-key-copy-confirmed",
		"--endpoint", "s3.example.invalid", "--bucket", "wake-bucket", "--prefix", "p", "--gateway-url", "https://gateway.example.invalid:9443",
		"--owner", "owner", "--timezone", "America/Chicago", "--agent-listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:0")
	pwm := regexp.MustCompile(`Password \(shown once[^)]*\): (\S+)`).FindStringSubmatch(out)
	if pwm == nil {
		t.Fatalf("init printed no owner password:\n%s", out)
	}
	tokOut := runCLI(t, "token", "--config", cfgPath, "--create-customer", "Wake Customer", "--uses", "5", "--expires", "2d")
	tokm := regexp.MustCompile(`never stored\):\n(\S+)`).FindStringSubmatch(tokOut)
	if tokm == nil {
		t.Fatalf("token printed no secret:\n%s", tokOut)
	}
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen = true, "127.0.0.1:0", "127.0.0.1:0"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
	var logs syncBuf
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{Clock: clk.Now, Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("shutdown timed out")
		}
	}()
	agent := agentCall{t: t, base: "http://" + app.Server.AgentAddr()}
	admin := &adminCall{t: t, base: "http://" + app.Server.AdminAddr()}

	var er api.EnrollResponse
	req := api.EnrollRequest{Nonce: strings.Repeat("e5", 16), Hostname: "WAKE-PC-01", OS: "windows", OSVersion: "Windows 11 Pro", Arch: api.ArchAMD64, DeviceClass: api.DeviceClassDesktop,
		HWFingerprint: strings.Repeat("ab", 16), AgentVersion: "1.4.2", ResticVersion: "0.19.1"}
	if code := agent.do("POST", api.RouteEnroll, tokm[1], req, &er); code != 200 {
		t.Fatalf("enroll: %d\nlogs:\n%s", code, logs.String())
	}
	devToken := string(er.DeviceToken)
	admin.login("owner", pwm[1])

	// holdPoll starts the agent's 30 s long poll and waits until the console holds it
	// (a second poller is refused with 409 while one is in flight).
	type polled struct {
		code int
		cmds api.CommandsResponse
		took time.Duration
	}
	holdPoll := func() chan polled {
		ch := make(chan polled, 1)
		start := time.Now()
		go func() {
			var p polled
			// A probe below may hold the single flight for a moment; the agent's loop retries a 409 too.
			for i := 0; i < 50; i++ {
				if p.code = agent.do("GET", api.RouteCommands+"?wait=30", devToken, nil, &p.cmds); p.code != http.StatusConflict {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			p.took = time.Since(start)
			ch <- p
		}()
		deadline := time.Now().Add(10 * time.Second)
		for pc := agent.do("GET", api.RouteCommands+"?wait=0", devToken, nil, nil); pc != http.StatusConflict; pc = agent.do("GET", api.RouteCommands+"?wait=0", devToken, nil, nil) {
			if time.Now().After(deadline) {
				t.Fatalf("the long poll never started (last probe %d)", pc)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return ch
	}
	expect := func(ch chan polled, typ string) api.Command {
		t.Helper()
		select {
		case p := <-ch:
			if p.code != 200 || len(p.cmds.Commands) != 1 || string(p.cmds.Commands[0].Type) != typ {
				t.Fatalf("poll answered %d %+v", p.code, p.cmds)
			}
			if p.took > 2*time.Second {
				t.Fatalf("the open poll was handed the command after %v; a wake-up takes milliseconds and the poll would otherwise wait 30 s", p.took)
			}
			return p.cmds.Commands[0]
		case <-time.After(10 * time.Second):
			t.Fatal("the queued command did not wake the open long poll")
		}
		return api.Command{}
	}

	// 1. Through the admin API.
	ch := holdPoll()
	code, b, _ := admin.do("POST", "/api/v1/devices/"+er.DeviceID+"/commands", adminapi.QueueCommandRequest{Type: "backup_now", IdempotencyKey: "wake-api"})
	if code != 201 {
		t.Fatalf("queue over the API: %d %s", code, b)
	}
	first := expect(ch, "backup_now")
	// The agent reports it done, so the next request is a new command rather than the one in flight.
	if code := agent.do("POST", api.RouteCommandResult(first.ID), devToken, api.CommandResultRequest{Status: api.CommandSucceeded}, nil); code != 204 {
		t.Fatalf("result: %d", code)
	}

	// 2. Through the UI route (same session, form post with the CSRF field).
	ch = holdPoll()
	form := url.Values{"key": {"wake-ui"}, "csrf_token": {admin.csrf}}
	r, _ := http.NewRequest("POST", admin.base+"/ui/devices/"+er.DeviceID+"/backup-now", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", admin.base)
	for _, c := range admin.cookies {
		r.AddCookie(c)
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "cmd=queued") {
		t.Fatalf("queue over the UI: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if second := expect(ch, "backup_now"); second.ID == first.ID {
		t.Fatal("the UI request returned the command that was already finished")
	}

	// 3. Retiring a device through the identity resolution drops its gateway login at once:
	// the htpasswd-sync job is signalled instead of waiting for its five-minute run.
	htpasswd := filepath.Join(cfg.GatewayAuthDir, "htpasswd")
	has := func() bool {
		b, err := os.ReadFile(htpasswd)
		return err == nil && strings.Contains(string(b), er.DeviceID+":$2y$")
	}
	for deadline := time.Now().Add(15 * time.Second); !has(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the enrolled device never got a gateway login")
		}
	}
	// The job's own first run lands at a random moment in the first 5 s and then every 5 minutes;
	// wait it out so only the signal can explain a prompt update.
	time.Sleep(6 * time.Second)
	if err := app.Store.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET identity_conflict_at = ? WHERE id = ?`, clk.Now().UnixMilli(), er.DeviceID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	code, b, _ = admin.do("POST", "/api/v1/devices/"+er.DeviceID+"/identity/resolve", adminapi.IdentityResolveRequest{Action: "retire", Reason: "clone of a restored image"})
	if code != 200 {
		t.Fatalf("retire: %d %s", code, b)
	}
	for deadline := time.Now().Add(10 * time.Second); has(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the retired device's gateway login is still in the htpasswd file")
		}
	}
}
