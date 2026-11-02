package server_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
)

// TestServeFixturesOverARealListener is what `cryo-console serve --fixtures
// --admin-addr 127.0.0.1:0` does, through the same Build, Listen and Run: it
// starts with no network and no config file, binds no agent listener, and serves
// the login page, the board, the device panel, the restore page and /design.
func TestServeFixturesOverARealListener(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixtures mode needs no certificate and no database path: %v", err)
	}
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
	}
	if app.Server.AgentAddr() != "" {
		t.Fatal("fixtures mode binds no agent listener")
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
	base := "http://" + app.Server.AdminAddr()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, cookies []*http.Cookie) (int, string) {
		r, _ := http.NewRequest("GET", base+path, nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		res, err := noRedirect.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	if code, body := get("/login", nil); code != 200 || !strings.Contains(body, "Fixtures mode") || !strings.Contains(body, "Sign in") {
		t.Fatalf("/login: %d", code)
	}
	if code, body := get("/design", nil); code != 200 || !strings.Contains(body, "Built-in checks") || !strings.Contains(body, "NEVER BACKED UP") {
		t.Fatalf("/design: %d", code)
	}
	if code, _ := get("/", nil); code != 303 {
		t.Fatalf("the board needs a session even in fixtures mode: %d", code)
	}
	res, err := noRedirect.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {"fixtures-pass-1"}})
	if err != nil || res.StatusCode != 303 {
		t.Fatalf("login: %v %v", err, res)
	}
	res.Body.Close()
	cookies := uiEnroll(t, noRedirect, base, res.Cookies()) // M2-W1A: the MFA step (forced enrollment)

	code, board := get("/", cookies)
	if code != 200 {
		t.Fatalf("/: %d", code)
	}
	for _, want := range []string{"Summit Eye Care", "FRONTDESK-01", "MISSING", `id="freshness"`, `<span class="word" aria-live="polite">Live</span>`, `data-at="1:42:10 PM"`, `id="kpis"`} {
		if !strings.Contains(board, want) {
			t.Errorf("/ lacks %q", want)
		}
	}
	id := regexp.MustCompile(`href="/devices/([0-9A-Z]{26})">FRONTDESK-01<`).FindStringSubmatch(board)
	if id == nil {
		t.Fatal("no link to FRONTDESK-01 on the board")
	}
	if code, body := get("/devices/"+id[1], cookies); code != 200 || !strings.Contains(body, "Last ten runs") || !strings.Contains(body, "FAILED") {
		t.Fatalf("device panel: %d", code)
	}
	if code, body := get("/devices/"+id[1]+"/restore", cookies); code != 200 || !strings.Contains(body, "Snapshot root") {
		t.Fatalf("restore page: %d", code)
	}
	// M2-W1A: the account screens render in fixtures mode.
	if code, body := get("/account", cookies); code != 200 || !strings.Contains(body, "Signed-in sessions") || !strings.Contains(body, "API tokens") {
		t.Fatalf("/account: %d", code)
	}
	if code, body := get("/settings/accounts", cookies); code != 200 || !strings.Contains(body, "Accounts and roles") || !strings.Contains(body, "+ Add an account") {
		t.Fatalf("/settings/accounts: %d", code)
	}
	if code, body := get("/healthz", nil); code != 200 || !strings.Contains(body, `"fixtures_mode":true`) {
		t.Fatalf("healthz: %d %s", code, body)
	}
	if code, _ := get("/static/app.css", nil); code != 200 {
		t.Fatalf("static: %d", code)
	}
}
