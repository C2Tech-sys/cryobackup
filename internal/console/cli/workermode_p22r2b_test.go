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
	yes, no := true, false
	for _, c := range []struct {
		name    string
		console bool
		work    *bool // nil: the worker does not report its mode (an older worker)
		err     error
		want    string // "" none
		wantSev state.Severity
	}{
		{"production console, production worker", false, &no, nil, "", 0},
		{"drill console, drill worker", true, &yes, nil, "", 0},
		{"drill console, production worker", true, &no, nil, workerModeDrillConsole, state.SeverityCritical},
		{"production console, drill worker", false, &yes, nil, workerModeProdConsole, state.SeverityCritical},
		{"worker unreachable, production console", false, &no, errors.New("down"), "", 0},
		{"worker unreachable, drill console", true, &no, errors.New("down"), "", 0},
		{"production console, worker mode unknown (L2)", false, nil, nil, workerModeUnknown, state.SeverityAlarm},
		{"drill console, worker mode unknown (L2)", true, nil, nil, workerModeDrillConsole, state.SeverityCritical},
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
			} else if wantID := map[bool]string{true: health.BannerWorkerModeUnknown, false: health.BannerWorkerMode}[c.want == workerModeUnknown]; len(b) != 1 || b[0].ID != wantID || b[0].Severity != c.wantSev || b[0].Text != c.want {
				t.Fatalf("banners = %+v, want one %v banner %s with %q", b, c.wantSev, wantID, c.want)
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
			if c.want != "" && c.want != workerModeUnknown {
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
	yes, no := true, false
	for _, c := range []struct {
		cfgDrill    bool
		workerDrill *bool // nil: the worker does not report its mode
		want        string
	}{
		{false, &no, "OK"}, {true, &yes, "OK"}, {true, &no, "FAIL"}, {false, &yes, "FAIL"},
		{true, nil, "FAIL"}, {false, nil, "WARN"},
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
		if c.want == "FAIL" && (code != ExitFailed || (c.workerDrill != nil && !strings.Contains(line, "drill mode differs")) || (c.workerDrill == nil && !strings.Contains(line, "does not report"))) {
			t.Errorf("config drill %v, worker drill %v: exit %d, line %q", c.cfgDrill, c.workerDrill, code, line)
		}
	}
}

// TestServeShowsTheWorkerModeBannerOnPages: a built serve wires the check to the pages. A drill
// console in front of a production worker, and a production console in front of a drill worker,
// show the banner (label and a fixed sentence) on the dashboard; a matching pair shows none.
// Killed mutant: fleet.Extra not wired in Build.
func TestServeShowsTheWorkerModeBannerOnPages(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name                  string
		consoleDrill          bool
		wDrill                *bool
		wantBanner, wantSense string
	}{
		{"production console, drill worker", false, &yes, "WORKER DRILL MODE MISMATCH", "in production mode but its maintenance worker is in drill mode"},
		{"drill console, production worker", true, &no, "WORKER DRILL MODE MISMATCH", "DRILL COPY but its maintenance worker is not in drill mode"},
		{"production console, worker mode unknown", false, nil, "WORKER MODE UNKNOWN", "Worker mode unknown (older worker)"},
		{"drill console, worker mode unknown", true, nil, "WORKER DRILL MODE MISMATCH", "DRILL COPY but its maintenance worker is not in drill mode"},
		{"production pair", false, &no, "", ""},
		{"drill pair", true, &yes, "", ""},
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
			if c.wantBanner == "" && (strings.Contains(body, "WORKER DRILL MODE MISMATCH") || strings.Contains(body, "WORKER MODE UNKNOWN")) {
				t.Fatal("a worker-mode banner on a matching pair")
			}
			has := strings.Contains(body, c.wantBanner) && c.wantBanner != ""
			if c.wantBanner != "" && !has {
				i := strings.Index(body, `id="banners"`)
				t.Logf("banners section: %.1500s", body[max(i, 0):])
			}
			if has != (c.wantBanner != "") {
				t.Fatalf("banner present = %v, want %v", has, c.wantBanner != "")
			}
			if has && !strings.Contains(body, c.wantSense) {
				t.Fatalf("the banner does not carry its sentence %q", c.wantSense)
			}
			if has != strings.Contains(logs.String(), "worker_drill_") {
				t.Fatalf("log line present = %v: %s", !has, logs.String())
			}
		})
	}
}
