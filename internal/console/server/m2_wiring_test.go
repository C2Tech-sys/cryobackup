package server_test

import (
	"context"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
)

// The admin API reads a device's schedule window in the operator's display zone
// (M2-W1C next_due), so the app must hand it the same zone the UI uses.
func TestAdminAPIGetsTheDisplayZone(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	cfg.DisplayTimezone = "America/Denver"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.Admin.Loc == nil || app.Admin.Loc.String() != "America/Denver" {
		t.Fatalf("adminapi.Service.Loc = %v, want America/Denver", app.Admin.Loc)
	}
}
