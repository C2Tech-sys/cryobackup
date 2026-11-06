package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// TestM3T4WriteSnapshots renders every M3-T4 page of the fixtures console to a
// standalone HTML file (real templates, real fixtures, the real stylesheet
// referenced from the source tree), for a headless browser to photograph. It
// does nothing unless M3T4_SNAPSHOT_DIR is set.
func TestM3T4WriteSnapshots(t *testing.T) {
	dir := os.Getenv("M3T4_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("M3T4_SNAPSHOT_DIR is not set")
	}
	static, _ := filepath.Abs(filepath.Join("..", "..", "..", "web", "static"))
	static = "file:///" + strings.TrimPrefix(filepath.ToSlash(static), "/")
	link := regexp.MustCompile(`(href|src)="/static/([^"?]+)(\?v=[0-9a-f]+)?"`)
	script := regexp.MustCompile(`<script src="[^"]*"[^>]*></script>`)
	write := func(name, body string) {
		body = link.ReplaceAllString(body, `$1="`+static+`/$2"`)
		body = script.ReplaceAllStringFunc(body, func(s string) string {
			if strings.Contains(s, "js/theme.js") {
				return s
			}
			return ""
		})
		body = strings.Replace(body, "</body>", `<script>document.title='OVF '+document.documentElement.scrollWidth+' '+document.documentElement.clientWidth;</script></body>`, 1)
		for _, theme := range []string{"dark", "light"} {
			b := strings.Replace(body, `<html lang="en">`, `<html lang="en" data-theme="`+theme+`">`, 1)
			if err := os.WriteFile(filepath.Join(dir, name+"-"+theme+".html"), []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	e, _ := newM3Env(t)
	e.m3Seed()
	e.login()
	pages := e.m3Pages()
	delete(pages, "runs fragment")
	for name, path := range pages {
		write(strings.ReplaceAll(name, " ", "-"), e.page(path, 200))
	}
	// a run with every kind of error class, and the failed run of the hostile fixture
	write("run-detail-failed", e.page("/runs/"+ulid(11), 200))
	write("run-detail-partial", e.page("/runs/"+ulid(2), 200))
	write("runs-clauses", e.page("/runs?status=failed&status=partial&kind=backup&customer="+e.customerID("Summit Eye Care")+"&from=2026-10-18&to=2026-10-20&to=", 200))
	// the engine's customer-wide silence
	e2, _ := newM3Env(t)
	e2.login()
	e2.m3Silence()
	write("unseen-silence", e2.page("/fleet/unseen", 200))
	write("overdue-silence", e2.page("/fleet/overdue", 200))
	// every banner at once
	e3, _ := newM3Env(t)
	e3.svc.Fleet.Extra = func(now time.Time) []state.Banner { return allBanners(now) }
	e3.login()
	write("system-banners", e3.page("/system", 200))
	write("runs-banners", e3.page("/runs", 200))
	write("activity-banners", e3.page("/activity", 200))
}
