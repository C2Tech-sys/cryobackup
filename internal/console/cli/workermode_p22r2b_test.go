package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P22-R2b B7: serve and maintain decide drill independently. The worker reports its mode in its
// ping; a console whose mode differs raises a critical banner (a fixed sentence) and logs an error
// once; doctor fails the worker line.

// TestWorkerModeBanner: both mismatches raise the banner, a match and an unreachable worker raise
// nothing, one ping answers for the TTL, and a standing mismatch logs once. Killed mutants: either
// direction of the comparison dropped, the error treated as a mismatch.
func TestWorkerModeBanner(t *testing.T) {
	for _, c := range []struct {
		name          string
		console, work bool
		err           error
		want          string // "" none
	}{
		{"production console, production worker", false, false, nil, ""},
		{"drill console, drill worker", true, true, nil, ""},
		{"drill console, production worker", true, false, nil, workerModeDrillConsole},
		{"production console, drill worker", false, true, nil, workerModeProdConsole},
		{"worker unreachable, production console", false, false, errors.New("down"), ""},
		{"worker unreachable, drill console", true, false, errors.New("down"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
			pings := 0
			var logs bytes.Buffer
			m := &workerMode{
				ping: func(context.Context) (adminpath.PingResult, error) {
					pings++
					return adminpath.PingResult{Drill: c.work}, c.err
				},
				drill: c.console, clock: func() time.Time { return now }, log: logx.New(&logs, logx.LevelDebug),
			}
			b := m.Banners(now)
			if c.want == "" {
				if len(b) != 0 {
					t.Fatalf("banners = %+v", b)
				}
			} else if len(b) != 1 || b[0].ID != health.BannerWorkerMode || b[0].Severity != state.SeverityCritical || b[0].Text != c.want {
				t.Fatalf("banners = %+v, want one critical with %q", b, c.want)
			}
			now = now.Add(10 * time.Second)
			m.Banners(now)
			if pings != 1 {
				t.Fatalf("%d pings inside the TTL, want 1", pings)
			}
			now = now.Add(workerModeTTL + time.Second)
			m.Banners(now)
			if pings != 2 {
				t.Fatalf("%d pings after the TTL, want 2", pings)
			}
			wantLogs := 0
			if c.want != "" {
				wantLogs = 1
			}
			if n := strings.Count(logs.String(), "disagree about drill mode"); n != wantLogs {
				t.Fatalf("%d error lines, want %d: %s", n, wantLogs, logs.String())
			}
			if strings.Contains(logs.String(), "/") || strings.Contains(c.want, "/") {
				t.Fatalf("a path in the log or the sentence: %s", logs.String())
			}
		})
	}
}

// TestDoctorFailsOnWorkerDrillMismatch: a worker answering on the socket whose drill mode differs
// from the config's is a FAIL line (exit 1), in both directions; a match is OK.
func TestDoctorFailsOnWorkerDrillMismatch(t *testing.T) {
	for _, c := range []struct {
		cfgDrill, workerDrill bool
		want                  string
	}{
		{false, false, "OK"}, {true, true, "OK"}, {true, false, "FAIL"}, {false, true, "FAIL"},
	} {
		cfg, _ := drillConfig(t, c.cfgDrill)
		sockDir, err := os.MkdirTemp("", "wm")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		sock := filepath.Join(sockDir, "w.sock")
		be := &fixtures.Backend{PingStatus: &adminpath.PingResult{Version: "test", MasterKeyLoaded: true, Drill: c.workerDrill}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- adminpath.Serve(ctx, sock, adminpath.Handler(be)) }()
		cl := adminpath.NewClient(sock, adminpath.ClientOptions{})
		var perr error
		for i := 0; i < 100; i++ {
			if _, perr = cl.Ping(ctx); perr == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cl.Close()
		if perr != nil {
			cancel()
			t.Fatalf("socket: %v", perr)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		raw := `{"data_dir": ` + strconv.Quote(cfg.DataDir) + `, "db_path": ` + strconv.Quote(cfg.DBPath) +
			`, "master_key_path": ` + strconv.Quote(cfg.MasterKeyPath) + `, "adminpath_socket": ` + strconv.Quote(sock) +
			`, "insecure_http_for_lab": true, "agent_listen": "127.0.0.1:0", "drill": ` + strconv.FormatBool(c.cfgDrill) + `}`
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, _ := run(t, "", "doctor", "--config", path)
		cancel()
		<-done
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, c.want) && strings.Contains(l, "maintenance worker") {
				line = l
			}
		}
		if line == "" {
			t.Errorf("config drill %v, worker drill %v: no %s maintenance worker line:\n%s", c.cfgDrill, c.workerDrill, c.want, out)
			continue
		}
		if c.want == "FAIL" && (code != ExitFailed || !strings.Contains(line, "drill mode differs")) {
			t.Errorf("config drill %v, worker drill %v: exit %d, line %q", c.cfgDrill, c.workerDrill, code, line)
		}
	}
}

// TestServeShowsTheWorkerModeBannerOnPages: a built serve wires the check to the pages. A drill
// console in front of a production worker, and a production console in front of a drill worker,
// show the banner (label and a fixed sentence) on the dashboard; a matching pair shows none.
// Killed mutant: fleet.Extra not wired in Build.
func TestServeShowsTheWorkerModeBannerOnPages(t *testing.T) {
	for _, c := range []struct {
		name                  string
		consoleDrill, wDrill  bool
		wantBanner, wantSense string
	}{
		{"production console, drill worker", false, true, "WORKER DRILL MODE MISMATCH", "in production mode but its maintenance worker is in drill mode"},
		{"drill console, production worker", true, false, "WORKER DRILL MODE MISMATCH", "DRILL COPY but its maintenance worker is not in drill mode"},
		{"production pair", false, false, "", ""},
		{"drill pair", true, true, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := drillConfig(t, c.consoleDrill)
			var logs bytes.Buffer
			be := &fixtures.Backend{PingStatus: &adminpath.PingResult{Version: "test", MasterKeyLoaded: true, Drill: c.wDrill}}
			app, err := Build(context.Background(), cfg, BuildOptions{Backend: be, Log: logx.New(&logs, logx.LevelDebug)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(app.Close)
			base := r3Serve(t, app)
			b := r3SignIn(t, app, base)
			res, body := b.do("GET", "/", nil)
			if res.StatusCode != 200 {
				t.Fatalf("GET /: %d %.200s", res.StatusCode, body)
			}
			has := strings.Contains(body, "WORKER DRILL MODE MISMATCH")
			if has != (c.wantBanner != "") {
				t.Fatalf("banner present = %v, want %v", has, c.wantBanner != "")
			}
			if has && !strings.Contains(body, c.wantSense) {
				t.Fatalf("the banner does not carry its sentence %q", c.wantSense)
			}
			if has != strings.Contains(logs.String(), "disagree about drill mode") {
				t.Fatalf("log line present = %v: %s", !has, logs.String())
			}
		})
	}
}
