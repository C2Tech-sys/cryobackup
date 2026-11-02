package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// ADR 0012: a zone `init` stored is the operator's choice and survives a
// different config.json; the migration's seed is nobody's choice and does not.

func buildFromInit(t *testing.T, tz, configZone string) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--timezone", tz)...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen = true, "127.0.0.1:0", "127.0.0.1:0"
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	cfg.DisplayTimezone = configZone // the operator edited config.json after init
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, logs.String()
}

func TestDatabaseInitialisedByInitKeepsItsZoneAndWarnsOnceWhenConfigDiffers(t *testing.T) {
	app, logs := buildFromInit(t, "Asia/Tokyo", "America/Chicago")
	if app.Display.Zone() != "Asia/Tokyo" {
		t.Fatalf("the zone init stored lost to config.json: %s", app.Display.Zone())
	}
	if n := strings.Count(logs, "the configured display settings differ from the ones saved"); n != 1 {
		t.Fatalf("%d warnings, want exactly one:\n%s", n, logs)
	}
	for _, want := range []string{"America_Chicago", "Asia_Tokyo", `"level":"WARN"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("warning misses %q:\n%s", want, logs)
		}
	}
	if notes := app.Display.ConfigNotes(); len(notes) != 1 || !strings.Contains(notes[0], "America/Chicago") || !strings.Contains(notes[0], "Asia/Tokyo") || !strings.Contains(notes[0], "wins") {
		t.Fatalf("settings-page note: %v", notes)
	}
}

func TestInitWithTheSameZoneAsConfigIsQuiet(t *testing.T) {
	app, logs := buildFromInit(t, "America/Denver", "America/Denver")
	if app.Display.Zone() != "America/Denver" || strings.Contains(logs, "config.json differs") || len(app.Display.ConfigNotes()) != 0 {
		t.Fatalf("zone %s, notes %v, logs:\n%s", app.Display.Zone(), app.Display.ConfigNotes(), logs)
	}
}

// init storing UTC is a choice too: it must not be mistaken for the migration's seed.
func TestInitStoringUTCStillWinsOverConfig(t *testing.T) {
	app, _ := buildFromInit(t, "UTC", "America/Chicago")
	if app.Display.Zone() != "UTC" {
		t.Fatalf("zone %s: the UTC init stored was treated as the seed", app.Display.Zone())
	}
}
