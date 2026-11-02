package ui_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// cli.Build hands the UI and the admin API one DisplayPrefs (M2-W3A), following
// the configured zone until an operator saves one, so a settings change reaches both.
func TestBuildSharesOneDisplayPrefsBetweenTheUIAndTheAPI(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	cfg.DisplayTimezone = "America/Denver"
	cfg.RefreshIntervalS = 45
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := cli.Build(context.Background(), cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.Display == nil || app.Display != app.Admin.Display || app.Display != app.UI.Display {
		t.Fatal("the UI and the API do not share one DisplayPrefs")
	}
	if app.Display.Zone() != "America/Denver" || app.Display.RefreshS() != 45 {
		t.Fatalf("fixtures prefs: zone %q refresh %d, want the configured ones", app.Display.Zone(), app.Display.RefreshS())
	}
	// Nothing was chosen by an operator: the migration's seed stays in the table, config.json supplies the value, and there is nothing to warn about.
	if n := app.Display.ConfigNotes(); len(n) != 0 || app.Display.Note() != "" {
		t.Fatalf("notes on a fresh fixtures database: %v %q", n, app.Display.Note())
	}
	// A change through the store-level path updates what both read.
	ch, err := app.Store.SetSettingsAudited(context.Background(), store.ScopeAll(),
		[]store.SettingInput{{Key: store.SettingDisplayTimezone, Value: json.RawMessage(`"Asia/Tokyo"`)}}, store.AuditActor{Method: "web"})
	if err != nil {
		t.Fatal(err)
	}
	app.Display.Apply(ch)
	if app.Admin.Display.Zone() != "Asia/Tokyo" || app.UI.Display.Loc().String() != "Asia/Tokyo" {
		t.Fatal("the change did not reach both services")
	}
}
