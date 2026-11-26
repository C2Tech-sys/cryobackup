package ui

import (
	"context"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-G: the screenshot set of the restore wizard and the restore status page (docs/screens/p22),
// every wizard step and every status, at 1360 and 390 px, dark and light, with the real fonts,
// from the real markup of fixtures mode (fixtures.SeedP22, fixtures.P22Files). Not a gate: it
// runs only when CRYO_SHOTS names a directory, and writes the PNG files and README.md there.

var staticRefRE = regexp.MustCompile(`(href|src)="/static/([^"?]+)(?:\?[^"]*)?"`)

// shotReady makes page what e.shot captures: only theme.js runs, and static files are relative.
func shotReady(page string) string {
	page = scriptTagRE.ReplaceAllStringFunc(page, func(tag string) string {
		if m := scriptSrcRE.FindStringSubmatch(tag); m != nil && m[1] == "theme.js" {
			return `<script src="static/js/theme.js"></script>`
		}
		return ""
	})
	return staticRefRE.ReplaceAllString(page, `$1="static/$2"`)
}

type p22Shot struct {
	slug, page, state, route string
	html                     string
}

func TestP22G_Screenshots(t *testing.T) {
	dir := os.Getenv("CRYO_SHOTS")
	if dir == "" {
		t.Skip("CRYO_SHOTS names no directory: the screenshots are for review, not a gate")
	}
	chromeOrSkip(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	e.login()
	e.be.Files = fixtures.P22Files()
	e.seedP22(t)
	plain := newEnv(t) // no seeded restores: DR-OFFICE-PC is free for a device restore, its backup running
	plain.login()
	plain.be.Files = fixtures.P22Files()

	forms, scans := fixtures.P22FormsFolder, fixtures.P22BigFolder
	// CRYO_SHOTS_ONLY=slug,slug re-shoots just those screens and leaves README.md as it is.
	only := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("CRYO_SHOTS_ONLY"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			only[s] = true
		}
	}
	var shots []p22Shot
	add := func(slug, page, state, route, html string) {
		if !strings.Contains(html, "</html>") {
			t.Fatalf("%s: not a page", slug)
		}
		if len(only) > 0 && !only[slug] {
			return
		}
		shots = append(shots, p22Shot{slug, page, state, route, html})
	}
	get := func(en *env, p string) string { _, b := en.do("GET", p, nil); return b }
	wiz := func(host string) string { return "/devices/" + e.dev[host] + "/restore" }
	fd := e.dev["FRONTDESK-01"]

	// Step 1 and 2: snapshot, browse and select.
	root := get(e, wiz("FRONTDESK-01"))
	add("wizard-select-root", "Restore wizard", "snapshot root, nothing selected", "/devices/{id}/restore", root)
	_, folder := e.click(t, fd, root, vv("dir", forms))
	_, folder = e.click(t, fd, folder, vv("path", forms+"/Intake", "path", forms+"/readme.txt", "dir", forms))
	add("wizard-select-folder", "Restore wizard", "folders, files, a long name, a link; two selected", "POST /ui/devices/{id}/restore", folder)
	_, big := e.click(t, fd, folder, vv("dir", scans))
	_, big = e.click(t, fd, big, vv("page", "500"))
	_, big = e.click(t, fd, big, vv("path", scans+"/scan-0502.pdf", "path", scans+"/scan-0504.pdf", "page", "500"))
	add("wizard-select-paged", "Restore wizard", "page 2 of a 2 000-entry folder, selection kept", "POST /ui/devices/{id}/restore", big)
	_, inside := e.click(t, fd, folder, vv("path", forms, "dir", forms+"/Intake"))
	add("wizard-select-inside", "Restore wizard", "inside a selected folder, nested items folded", "POST /ui/devices/{id}/restore", inside)
	over := vv("snapshot", fixtures.FixtureSnapshotID, "snapcur", fixtures.FixtureSnapshotID, "step", "select", "cur", scans, "action", "submit")
	for i := 1; i <= 53; i++ {
		over.Add("path", fmt.Sprintf("%s/scan-%04d.pdf", scans, i))
	}
	_, overBody := e.post("/ui/devices/"+fd+"/restore", over, false)
	add("wizard-select-over", "Restore wizard", "53 selected: over the limit, refused with the reason", "POST /ui/devices/{id}/restore", overBody)
	older := strings.Repeat("e2", 32)
	if _, _, err := e.st.AgentReplaceSnapshots(context.Background(), fd, []store.Snapshot{
		{DeviceID: fd, SnapshotID: fixtures.FixtureSnapshotID, TakenAt: e.now.Add(-26 * time.Hour), Hostname: "FRONTDESK-01", Paths: []string{`C:\Users`}},
		{DeviceID: fd, SnapshotID: older, TakenAt: e.now.Add(-50 * time.Hour), Hostname: "FRONTDESK-01", Paths: []string{`C:\Users`}}}); err != nil {
		t.Fatal(err)
	}
	b := browserForm(t, folder)
	b.Set("snapshot", older)
	b.Set("action", "snapshot")
	_, cleared := e.post("/ui/devices/"+fd+"/restore", b, false)
	add("wizard-select-cleared", "Restore wizard", "another snapshot chosen: selection cleared", "POST /ui/devices/{id}/restore", cleared)
	add("wizard-select-retired", "Restore wizard", "retired device: download only", "/devices/{id}/restore", get(e, wiz("OLD-RECEPTION-PC")))
	add("wizard-blocked-held", "Restore wizard", "customer-held key: blocked", "/devices/{id}/restore", get(e, wiz("LEGAL-PC-01")))
	add("wizard-blocked-purging", "Restore wizard", "device being purged: blocked", "/devices/{id}/restore", get(e, wiz("SPARE-PC-09")))
	nosnap := e.dev["SCAN-STATION-1"]
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM snapshots WHERE device_id = ?`, nosnap)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.be.Errors = map[string]error{adminpath.MethodSnapshots: adminpath.ErrUnavailable}
	add("wizard-select-nosnapshots", "Restore wizard", "no snapshot known, refresh failed (worker down)", "/devices/{id}/restore", get(e, "/devices/"+nosnap+"/restore"))
	e.be.Errors = nil
	e.svc.MasterLoaded = func() bool { return false }
	add("wizard-blocked-masterkey", "Restore wizard", "master key not loaded: blocked", "/devices/{id}/restore", get(e, wiz("FRONTDESK-H1")))
	e.svc.MasterLoaded = e.svc.Restores.Master.Loaded

	// Step 3: destination.
	dest := func(en *env, host string, pick ...string) string {
		t.Helper()
		v := vv("snapshot", fixtures.FixtureSnapshotID, "step", "select", "action", "submit", "cur", forms)
		for _, p := range pick {
			v.Add("path", p)
		}
		en.do("GET", "/devices/"+en.dev[host]+"/restore", nil) // fill an empty snapshot cache
		_, body := en.post("/ui/devices/"+en.dev[host]+"/restore", v, false)
		if !strings.Contains(body, `name="step" value="dest"`) {
			t.Fatalf("%s: no destination step", host)
		}
		return body
	}
	picks := []string{forms + "/Intake", forms + "/readme.txt", scans + "/scan-0502.pdf"}
	destAll := dest(e, "FRONTDESK-01", picks...)
	add("wizard-dest", "Restore wizard", "destination: every choice offered, original location default", "POST /ui/devices/{id}/restore", destAll)
	add("wizard-dest-retired", "Restore wizard", "destination: retired device, download only", "POST /ui/devices/{id}/restore", dest(e, "OLD-RECEPTION-PC", picks...))
	add("wizard-dest-oldagent", "Restore wizard", "destination: agent older than 0.4.0", "POST /ui/devices/{id}/restore", dest(e, fixtures.P22OldAgentHost, picks...))
	add("wizard-dest-active", "Restore wizard", "destination: a device restore already active", "POST /ui/devices/{id}/restore", dest(e, "OPS-ND-01", picks...))

	// Step 4: review.
	review := func(en *env, host, mode string) string {
		t.Helper()
		body := dest(en, host, picks...)
		_, body = en.click(t, en.dev[host], body, vv("mode", mode, "action", "submit"))
		if !strings.Contains(body, `name="step" value="review"`) {
			t.Fatalf("%s %s: no review step", host, mode)
		}
		return body
	}
	add("wizard-review-zip", "Restore wizard", "review: download as a zip", "POST /ui/devices/{id}/restore", review(e, "FRONTDESK-01", "zip"))
	add("wizard-review-original", "Restore wizard", "review: original location, keep existing; device online", "POST /ui/devices/{id}/restore", review(e, "FRONTDESK-01", "original"))
	add("wizard-review-side-offline", "Restore wizard", "review: a new folder; device offline", "POST /ui/devices/{id}/restore", review(plain, "LAB-RETINA-02", "side"))
	replace := review(plain, "DR-OFFICE-PC", "replace")
	add("wizard-review-replace", "Restore wizard", "review: replace changed files; backup running", "POST /ui/devices/{id}/restore", replace)
	_, mismatch := plain.click(t, plain.dev["DR-OFFICE-PC"], replace, vv("confirm_host", "dr-office-pc", "action", "submit"))
	add("wizard-review-replace-mismatch", "Restore wizard", "review: host name typed wrong (400)", "POST /ui/devices/{id}/restore", mismatch)
	plain.advance(6 * time.Minute)
	stale := review(plain, "FRONTDESK-01", "zip")
	_, stale403 := plain.click(t, plain.dev["FRONTDESK-01"], stale, vv("action", "submit"))
	add("wizard-review-stale", "Restore wizard", "review: authenticator check stale, code asked (403)", "POST /ui/devices/{id}/restore", stale403)
	forged := func(host, mode string) string {
		_, body := e.post("/ui/devices/"+e.dev[host]+"/restore", vv("snapshot", fixtures.FixtureSnapshotID, "step", "review", "mode", mode, "action", "submit", "path", forms+"/readme.txt"), false)
		return body
	}
	add("wizard-refused-oldagent", "Restore wizard", "refused: agent too old (409)", "POST /ui/devices/{id}/restore", forged(fixtures.P22OldAgentHost, "original"))
	add("wizard-refused-active", "Restore wizard", "refused: a restore already active (409)", "POST /ui/devices/{id}/restore", forged("OPS-ND-01", "side"))
	add("wizard-refused-retired", "Restore wizard", "refused: retired device, device mode (409)", "POST /ui/devices/{id}/restore", forged("OLD-RECEPTION-PC", "original"))

	// Every status.
	for _, r := range fixtures.P22Restores() {
		add("status-"+r.Key, "Restore status", r.Title, "/restores/{id}", get(e, "/restores/"+r.ID))
	}
	add("device-retired", "Device: Summary", "retired: Restore files offers a download only", "/devices/{id}", get(e, "/devices/"+e.dev["OLD-RECEPTION-PC"]))

	css := e.appCSS(t)
	var readme strings.Builder
	readme.WriteString(readmeHead)
	for _, s := range shots {
		files := []string{}
		for _, w := range []int{1360, 390} {
			for _, theme := range []string{"dark", "light"} {
				name := fmt.Sprintf("%s.%d.%s.png", s.slug, w, theme)
				html := s.html
				h := e.layout(t, layoutOpts{HTML: html, CSS: css, W: w, H: 900, Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `'); return {h: document.documentElement.scrollHeight};`})["h"]
				height := int(h) + 8
				if height < 700 {
					height = 700
				}
				if height > 4800 {
					height = 4800
				}
				img := e.shot(t, "/", theme, w, height, "", func(string) string { return shotReady(html) })
				f, err := os.Create(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := png.Encode(f, img); err != nil {
					t.Fatal(err)
				}
				f.Close()
				files = append(files, fmt.Sprintf("[%d %s](%s)", w, theme, url.PathEscape(name)))
			}
		}
		fmt.Fprintf(&readme, "| `%s` | %s | %s | `%s` | %s |\n", s.slug, s.page, s.state, s.route, strings.Join(files, ", "))
	}
	if len(only) == 0 {
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote %d screens to %s", len(shots), dir)
}

const readmeHead = `# P22 screenshot set: the restore wizard and the restore status page

Every step of the restore wizard and every status of a restore (ticket P22-G; ADR 0027, ADR 0028, ADR 0030; plan B.1), rendered
in **fixtures mode** from the real markup: the fictional fleet, the restore world of ` + "`fixtures.SeedP22`" + ` and the fake snapshot of
` + "`fixtures.P22Files`" + ` (a folder of 2 000 entries, nested folders, a long name, a link), a frozen clock of Tue 20 Oct 2026 1:42 PM
America/Chicago, no network. Nothing here is a real name, host, path or credential.

Each screen exists at 1360 px (desktop) and 390 px (mobile) in dark and light themes, with the house fonts. File name:
` + "`<slug>.<width>.<theme>.png`" + `. Each capture is as tall as its page (at most 4 800 px).

Method: ` + "`CRYO_SHOTS=<dir> go test ./internal/console/ui/ -run TestP22G_Screenshots`" + ` (internal/console/ui/restore_p22g_shots_test.go)
drives the console's handlers through every step with plain form posts, as a browser with JavaScript off does, and captures each
page with the package's headless Chrome. A wizard step reached by a POST is shown at its route ` + "`POST /ui/devices/{id}/restore`" + `;
no path ever appears in a URL.

| Slug | Page | State | Route | Files |
| --- | --- | --- | --- | --- |
`
