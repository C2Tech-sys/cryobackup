package ui

import (
	"context"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-I: the screenshot set of the device page's repository-key panel (docs/screens/p22), every
// panel state and the answers of Verify now, at 1360 and 390 px, dark and light. Not a gate: it
// runs only when CRYO_SHOTS names a directory. CRYO_SHOTS_ONLY=slug,slug re-shoots just those
// screens. The README section of these screens is rewritten in place; the rest of the file and the
// other screens are left as they are.

const escrowReadmeMark = "## Device page: the repository-key panel (P22-I)"

func TestP22I_Screenshots(t *testing.T) {
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
	e.seedEscrow(t)
	only := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("CRYO_SHOTS_ONLY"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			only[s] = true
		}
	}
	var shots []p22Shot
	add := func(slug, state, route, html string) {
		if !strings.Contains(html, "</html>") {
			t.Fatalf("%s: not a page", slug)
		}
		if len(only) > 0 && !only[slug] {
			return
		}
		shots = append(shots, p22Shot{slug, "Device: Summary", state, route, html})
	}
	devPage := func(key string) string { return e.page("/devices/"+e.escrowDev(t, key), 200) }

	add("escrow-verified", "key verified 2 days ago, Verify now offered", "/devices/{id}", devPage("verified"))
	add("escrow-stale", "last proof 10 days old: overdue, the weekly check has not run", "/devices/{id}", devPage("stale"))
	add("escrow-failed", "last check failed: wrong password", "/devices/{id}", devPage("failed"))
	add("escrow-failed-repo", "last check failed: repository missing", "/devices/{id}", devPage("failed-repo"))
	add("escrow-never", "never verified", "/devices/{id}", devPage("never"))
	add("escrow-held", "customer-held key: nothing to verify, no button", "/devices/{id}", devPage("held"))
	add("escrow-unconfirmed", "enrollment not confirmed: no repository yet, no button", "/devices/{id}", devPage("unconfirmed"))
	add("escrow-purging", "device being purged: key no longer checked, no button", "/devices/{id}", devPage("purging"))
	add("escrow-retired", "retired device: same panel, download-only note", "/devices/{id}", devPage("retired"))

	// A retired device whose key fails: no alarm of its own, so the fleet banner C22 counts it.
	rid := e.escrowDev(t, "retired")
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET last_verify_error_class = 'wrong_password', last_verify_attempt_at = ? WHERE device_id = ? AND retired_at IS NULL`, e.now.Add(-2*time.Hour).UnixMilli(), rid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	add("escrow-retired-failed", "retired device, last check failed: counted in fleet banner C22", "/devices/{id}", e.page("/devices/"+rid, 200))

	// Verify now: the verdicts (a 303 back to the page, the banner in the panel) and the refusals.
	verdict := func(id string) string {
		res, _ := e.verifyPost(id, false)
		if res.StatusCode != 303 {
			t.Fatalf("verify: %d", res.StatusCode)
		}
		return e.page(strings.SplitN(e.location(res), "#", 2)[0], 200)
	}
	sid := e.escrowDev(t, "stale")
	add("escrow-verdict-verified", "Verify now: the key opened the repository config", "POST /ui/devices/{id}/escrow/verify, then /devices/{id}", verdict(sid))
	e.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeWrongPassword, "")}
	add("escrow-verdict-failed", "Verify now: verification failed (wrong password)", "POST /ui/devices/{id}/escrow/verify, then /devices/{id}", verdict(e.escrowDev(t, "verified")))
	e.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeStorageUnreachable, "")}
	add("escrow-verdict-noverdict", "Verify now: no verdict, storage did not answer", "POST /ui/devices/{id}/escrow/verify, then /devices/{id}", verdict(e.escrowDev(t, "failed-repo")))
	e.be.Errors = map[string]error{adminpath.MethodPing: adminpath.ErrUnavailable}
	_, workerDown := e.verifyPost(e.escrowDev(t, "never"), false)
	add("escrow-refused-worker", "Verify now refused: the worker did not answer (503)", "POST /ui/devices/{id}/escrow/verify", workerDown)
	e.be.Errors = nil
	_, held := e.verifyPost(e.escrowDev(t, "held"), false)
	add("escrow-refused-held", "Verify now refused: customer-held key (409)", "POST /ui/devices/{id}/escrow/verify", held)

	css := e.appCSS(t)
	var rows strings.Builder
	for _, s := range shots {
		files := []string{}
		for _, w := range []int{1360, 390} {
			for _, theme := range []string{"dark", "light"} {
				name := fmt.Sprintf("%s.%d.%s.png", s.slug, w, theme)
				html := s.html
				h := e.layout(t, layoutOpts{HTML: html, CSS: css, W: w, H: 900, Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `'); return {h: document.documentElement.scrollHeight};`})["h"]
				height := min(max(int(h)+8, 700), 4800)
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
		fmt.Fprintf(&rows, "| `%s` | %s | `%s` | %s |\n", s.slug, s.state, s.route, strings.Join(files, ", "))
	}
	if len(only) == 0 {
		section := escrowReadmeMark + "\n\n" +
			"The Summary tab's repository-key panel and the answers of Verify now (ticket P22-I; ADR 0030; plan B.3), in **fixtures mode**:\n" +
			"the fictional fleet with `fixtures.SeedP22Escrow` (one device per key state), a frozen clock, no network. Same sizes, themes and file\n" +
			"names as above. Method: `CRYO_SHOTS=<dir> go test ./internal/console/ui/ -run TestP22I_Screenshots`\n" +
			"(internal/console/ui/devicepanel_escrow_p22i_shots_test.go).\n\n" +
			"| Slug | State | Route | Files |\n| --- | --- | --- | --- |\n" + rows.String()
		readme := filepath.Join(dir, "README.md")
		b, err := os.ReadFile(readme)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		old := string(b)
		if i := strings.Index(old, escrowReadmeMark); i >= 0 {
			old = strings.TrimRight(old[:i], "\n") + "\n"
		}
		if err := os.WriteFile(readme, []byte(strings.TrimRight(old, "\n")+"\n\n"+section), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote %d screens to %s", len(shots), dir)
}
