package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/ui"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M3-INT item D: a real serve App (not fixtures mode) hands the admin API the
// SSE hub, both event-stream caps from config.json, the job runner and the
// health-prober; fixtures mode, which runs no prober, leaves Prober nil (never
// a typed nil, which would answer the probe route through a nil pointer).
func TestServeWiresTheAdminAPIEventStreamsJobsAndProber(t *testing.T) { // M3-INT D
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen = true, "127.0.0.1:0", "127.0.0.1:0"
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	cfg.RateLimits.EventStreamsMax, cfg.RateLimits.EventStreamsPerAccount = 7, 3 // not the defaults, so a hard-coded value fails
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	if hub, ok := app.Admin.Events.(*ui.Hub); !ok || hub == nil || hub != app.Hub {
		t.Errorf("Admin.Events = %T %v, want the App's hub", app.Admin.Events, app.Admin.Events)
	}
	if app.Admin.MaxEventStreams != 7 || app.Admin.MaxEventStreamsPerPrincipal != 3 {
		t.Errorf("event stream caps = %d / %d, want rate_limits.event_streams_max 7 and event_streams_per_account 3",
			app.Admin.MaxEventStreams, app.Admin.MaxEventStreamsPerPrincipal)
	}
	if r, ok := app.Admin.Jobs.(*jobs.Runner); !ok || r == nil || r != app.Runner {
		t.Errorf("Admin.Jobs = %T, want the App's runner", app.Admin.Jobs)
	}
	if p, ok := app.Admin.Prober.(*jobs.HealthProber); !ok || p == nil || p != app.Prober {
		t.Errorf("Admin.Prober = %T, want the App's health-prober", app.Admin.Prober)
	}

	fcfg := config.Default()
	fcfg.Resolve()
	fcfg.Fixtures = true
	fcfg.AdminListen = "127.0.0.1:0"
	fapp, err := Build(context.Background(), fcfg, BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fapp.Close)
	if fapp.Admin.Prober != nil {
		t.Errorf("fixtures mode: Admin.Prober = %T, want nil (no prober runs)", fapp.Admin.Prober)
	}
	if fapp.Admin.Events == nil || fapp.Admin.Jobs == nil {
		t.Error("fixtures mode: the hub or the runner is not wired")
	}
}
