package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
)

// M3-T4 fix round 3: mutants N17, N18 and N29 of the second review survived, because
// no test drove a real App for them. These do.

// ---- N17, N18: the fixtures switch does not exist on a real console ----

func TestR3ARealConsoleHasNoFixturesRouteAndNoBannerSwitch(t *testing.T) { // M3-T4 r3 N17 N18
	t.Cleanup(func() { fixtures.SetAllBanners(false) })
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	srv := httptest.NewServer(app.Server.AdminRouter)
	t.Cleanup(srv.Close)

	// N17, signed out: no route, so no redirect to the login page either.
	anon := &r3Browser{t: t, base: srv.URL}
	if res, _ := anon.do("GET", "/fixtures?banners=all", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("signed out, GET /fixtures on a real console = %d, want 404", res.StatusCode)
	}
	// N17, signed in: the same, and a read-only GET changes nothing.
	c := r3SignIn(t, app, srv.URL)
	if res, _ := c.do("GET", "/fixtures?banners=all", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("signed in, GET /fixtures on a real console = %d, want 404", res.StatusCode)
	}
	if fixtures.AllBannersOn() {
		t.Fatal("a GET turned every alarm on on a real console")
	}

	// N18: even with the switch thrown, a real console's evaluator never reads it.
	if app.UI.Fleet.Extra != nil {
		t.Error("the fixtures banner hook is wired into a real console's evaluator")
	}
	fixtures.SetAllBanners(true)
	var h adminapi.SystemHealthResponse
	res, body := c.do("GET", "/api/v1/system/health", nil)
	if res.StatusCode != 200 {
		t.Fatalf("health: %d %.300s", res.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		t.Fatal(err)
	}
	for _, b := range h.Banners {
		switch b.ID {
		case "C00", "C16", "C13", "C08": // only the fictional set raises these on this console: it is wired, has run no check of them, and has no fleet
			t.Errorf("the fixtures banner %s shows on a real console: %s", b.ID, b.Text)
		}
	}
	page := func() string { _, b := c.do("GET", "/system", nil); return b }()
	if strings.Contains(page, "SELF-CHECKS NOT WIRED") || strings.Contains(page, "RELEASE MANIFEST REJECTED") {
		t.Error("the fixtures banners show on a real console's system page")
	}
}

// ---- N29: serve's shutdown reaches the pages' private admin API ----

func TestR3ServeShutdownCancelsARunStartedFromTheSystemPage(t *testing.T) { // M3-T4 r3 N29
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	app, err := Build(context.Background(), cfg, BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	entered := make(chan struct{})
	var cancelled atomic.Bool
	// On a runner of its own that is never started, the way TestServeShutdownWaitsForAManualJobRun does.
	manual := jobs.NewRunner(app.Store, "r3-manual", nil)
	app.UI.Jobs = manual
	manual.Add(jobs.Job{Name: "r3-slow", Every: time.Hour, Lease: time.Hour, Run: func(ctx context.Context) error {
		close(entered)
		select {
		case <-ctx.Done(): // only UI.BeginShutdown cancels this: Admin's gate is a different Service's
			cancelled.Store(true)
		case <-time.After(45 * time.Second):
		}
		return nil
	}})
	if err := app.Server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	returned := make(chan error, 1)
	ended := make(chan struct{})
	go func() { defer close(ended); returned <- app.Run(ctx) }()
	t.Cleanup(func() { stop(); <-ended }) // before app.Close: cleanups are LIFO
	base := "http://" + app.Server.AdminAddr()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		conn, err := net.DialTimeout("tcp", app.Server.AdminAddr(), time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the admin listener never came up")
		}
	}
	c := &r3Browser{t: t, base: base}
	c.login("admin", "fixtures-pass-1")
	res, body := c.do("POST", "/ui/system/jobs/r3-slow/run", url.Values{"csrf_token": {c.csrf}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("Run now from the page: %d %.300s", res.StatusCode, body)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never started")
	}
	stop()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("App.Run = %v", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("App.Run did not return")
	}
	if !cancelled.Load() {
		t.Fatal("App.Run returned without cancelling the run started from the System page: a.UI.BeginShutdown did not reach it")
	}
}
