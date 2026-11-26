package ui

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-R2a: the screens the stale-running-restore fix adds to the P22 set (docs/screens/p22), shot
// the way TestP22G_Screenshots shoots the others: a running restore whose device has gone silent,
// the same restore after the sweep closed it as interrupted (ADR 0032), and after the device's late
// report. Not a gate: it runs only when CRYO_SHOTS names a directory, honours CRYO_SHOTS_ONLY, and
// leaves README.md alone (its rows are written by hand).
func TestR2A_Screenshots(t *testing.T) {
	dir := os.Getenv("CRYO_SHOTS")
	if dir == "" {
		t.Skip("CRYO_SHOTS names no directory: the screenshots are for review, not a gate")
	}
	chromeOrSkip(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	only := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("CRYO_SHOTS_ONLY"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			only[s] = true
		}
	}
	e := newEnv(t)
	e.login()
	e.be.Files = fixtures.P22Files()
	e.seedP22(t)
	ctx := context.Background()
	fx, _ := fixtures.P22RestoreByKey("device-running")
	get := func(p string) string { _, b := e.do("GET", p, nil); return b }

	pages := map[string]string{}
	e.advance(6 * time.Hour) // silent far longer than the report cadence, not yet past the silence limit
	e.login()
	pages["status-device-running-silent"] = get("/restores/" + fx.ID)
	e.advance(20 * time.Hour) // past the limit: the sweep closes it
	e.login()
	if _, err := e.st.SweepAbandonedRuns(ctx, store.SweepOptions{}); err != nil {
		t.Fatal(err)
	}
	pages["status-device-failed-silent"] = get("/restores/" + fx.ID)
	fin := e.now
	if _, err := e.st.AgentIngestRuns(ctx, e.dev[fx.Host], []store.RunInput{{ID: fx.ID, Kind: "restore", Trigger: "manual", StartedAt: e.now.Add(-26 * time.Hour),
		FinishedAt: &fin, Outcome: "success", Stats: []byte(`{"files_restored":1284,"files_skipped":37,"bytes_restored":2140000000}`)}}, e.now); err != nil {
		t.Fatal(err)
	}
	pages["status-device-failed-silent-late"] = get("/restores/" + fx.ID)

	css := e.appCSS(t)
	n := 0
	for slug, page := range pages {
		if len(only) > 0 && !only[slug] {
			continue
		}
		for _, w := range []int{1360, 390} {
			for _, theme := range []string{"dark", "light"} {
				h := e.layout(t, layoutOpts{HTML: page, CSS: css, W: w, H: 900, Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `'); return {h: document.documentElement.scrollHeight};`})["h"]
				height := min(max(int(h)+8, 700), 4800)
				img := e.shot(t, "/", theme, w, height, "", func(string) string { return shotReady(page) })
				f, err := os.Create(filepath.Join(dir, fmt.Sprintf("%s.%d.%s.png", slug, w, theme)))
				if err != nil {
					t.Fatal(err)
				}
				if err := png.Encode(f, img); err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
		}
		n++
	}
	t.Logf("wrote %d screens to %s", n, dir)
}
