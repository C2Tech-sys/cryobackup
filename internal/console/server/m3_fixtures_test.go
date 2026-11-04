package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
)

// M3-T3: every new admin route answers in fixtures mode, over a real listener and
// the full middleware chain, with the real job runner and the real SSE hub wired
// into the admin API. The two assignments below are the wiring cli.Build needs
// (adminapi.Service.Jobs and .Events); the test makes them itself so it passes
// with or without them in Build, and the SSE assertions prove the hub half works
// end to end.
func TestM3RoutesAnswerInFixturesModeOverARealListener(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	app.Admin.Jobs, app.Admin.Events = app.Runner, app.Hub
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
	base := "http://" + app.Server.AdminAddr()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {"fixtures-pass-1"}})
	if err != nil || res.StatusCode != 303 {
		t.Fatalf("login: %v %v", err, res)
	}
	res.Body.Close()
	cookies := uiEnroll(t, client, base, res.Cookies())
	csrf := ""
	for _, c := range cookies {
		if c.Name == auth.CSRFCookie {
			csrf = c.Value
		}
	}
	do := func(method, path string, body string) (int, http.Header, string) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		r, _ := http.NewRequest(method, base+path, rd)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		if method != "GET" {
			r.Header.Set("Origin", base)
			r.Header.Set(auth.CSRFHeader, csrf)
			r.Header.Set("Content-Type", "application/json")
		}
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header, string(b)
	}
	id := func(re, body string) string {
		m := regexp.MustCompile(re).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no match for %s in %.300s", re, body)
		}
		return m[1]
	}

	_, _, devs := do("GET", "/api/v1/devices?limit=500", "")
	frontdesk := id(`"id":"([0-9A-Z]{26})"[^{}]*"hostname":"FRONTDESK-01"`, devs)
	_, _, custs := do("GET", "/api/v1/customers", "")
	summit := id(`"id":"([0-9A-Z]{26})","name":"Summit Eye Care"`, custs)
	_, _, runs := do("GET", "/api/v1/runs?limit=1", "")
	run := id(`"id":"([0-9A-Z]{26})"`, runs)
	for _, p := range []string{
		"/api/v1/fleet/summary", "/api/v1/fleet/banners", "/api/v1/fleet/overdue", "/api/v1/fleet/unseen", "/api/v1/fleet/ten-day", "/api/v1/alarms",
		"/api/v1/customers/" + summit + "/health", "/api/v1/devices/" + frontdesk + "/state", "/api/v1/devices/" + frontdesk + "/heartbeats",
		"/api/v1/devices/" + frontdesk + "/ten-day", "/api/v1/runs", "/api/v1/runs/" + run, "/api/v1/views", "/api/v1/status",
		"/api/v1/system/health", "/api/v1/system/jobs", "/metrics",
	} {
		code, _, body := do("GET", p, "")
		if code != 200 {
			t.Errorf("GET %s = %d %.200s", p, code, body)
		}
	}
	if _, h, body := do("GET", "/metrics", ""); h.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || !strings.Contains(body, "cryobackup_fleet_all_clear 0") {
		t.Errorf("/metrics: %s %.200s", h.Get("Content-Type"), body)
	}

	// The stream, through the real hub: connect, read the first state, pause a device, read the next one.
	r, _ := http.NewRequest("GET", base+"/api/v1/events?topics=fleet", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	sres, err := client.Do(r.WithContext(sctx))
	if err != nil || sres.StatusCode != 200 || sres.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %v %v", err, sres)
	}
	defer sres.Body.Close()
	events := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(sres.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		name := ""
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && name == "fleet.state":
				events <- strings.TrimPrefix(line, "data: ")
			}
		}
		close(events)
	}()
	var first struct {
		Devices struct{ Paused int } `json:"devices"`
	}
	waitState := func() string {
		select {
		case d, ok := <-events:
			if !ok {
				t.Fatal("the event stream closed")
			}
			return d
		case <-time.After(15 * time.Second):
			t.Fatal("no fleet.state event")
		}
		return ""
	}
	if err := json.Unmarshal([]byte(waitState()), &first); err != nil {
		t.Fatal(err)
	}
	if code, _, body := do("POST", "/api/v1/devices/"+frontdesk+"/pause", `{"reason":"fixtures sweep","duration_hours":1}`); code != 200 {
		t.Fatalf("pause: %d %s", code, body)
	}
	var next struct {
		Devices struct{ Paused int } `json:"devices"`
	}
	for i := 0; i < 5 && next.Devices.Paused <= first.Devices.Paused; i++ { // the hub coalesces for up to two seconds
		if err := json.Unmarshal([]byte(waitState()), &next); err != nil {
			t.Fatal(err)
		}
	}
	if next.Devices.Paused != first.Devices.Paused+1 {
		t.Errorf("the stream did not carry the pause: %d paused before, %d after", first.Devices.Paused, next.Devices.Paused)
	}
	if code, _, body := do("POST", "/api/v1/devices/"+frontdesk+"/resume", ""); code != 200 {
		t.Fatalf("resume: %d %s", code, body)
	}
	// Run-now on an unregistered job is a clean 404; the fixtures runner registers none.
	if code, _, _ := do("POST", "/api/v1/system/jobs/htpasswd-sync/run", ""); code != 404 {
		t.Errorf("run-now of an unregistered job = %d, want 404", code)
	}
}
