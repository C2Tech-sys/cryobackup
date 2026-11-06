package ui

import (
	"encoding/json"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
)

// Rendered-layout tests (M3-T5 C). They replace the two M2-FIX-C tests that matched the TEXT of app.css
// (a rule can be present and still not take effect, or a different rule can do the job): here the
// real page is laid out by a real browser, and the test measures where things end up.
//
// Each test also runs the same measurement against a copy of app.css with the fix removed and requires
// that measurement to fail, so the test is known to see the fault it guards against.
//
// They need a Chrome or Chromium: CRYO_CHROME names it, otherwise the usual install paths and PATH
// names are tried. Without one the tests skip and say so.

func chromeBinary() string {
	if p := os.Getenv("CRYO_CHROME"); p != "" {
		return p
	}
	for _, p := range []string{`C:\Program Files\Google\Chrome\Application\chrome.exe`, `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, n := range []string{"google-chrome", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

var scriptTagRE = regexp.MustCompile(`(?s)<script\b.*?</script>`)

// layoutPage lays out the real markup of path with css as app.css, runs probe (JavaScript that returns
// a JSON-able value) in a 1360 px wide window and returns what it returned.
func (e *env) layoutPage(t *testing.T, path, css, probe string) map[string]float64 {
	t.Helper()
	chrome := chromeBinary()
	if chrome == "" {
		t.Skip("no Chrome or Chromium found (set CRYO_CHROME): the rendered-layout tests need a browser")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, tokens := e.do("GET", "/static/tokens.css", nil)
	for name, body := range map[string]string{"app.css": css, "tokens.css": tokens} {
		if err := os.WriteFile(filepath.Join(dir, "static", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	page := e.page(path, 200)
	page = scriptTagRE.ReplaceAllString(page, "")
	page = regexp.MustCompile(`(?:href|src)="/static/([^"?]+)(?:\?[^"]*)?"`).ReplaceAllStringFunc(page, func(m string) string {
		return strings.Replace(strings.Replace(m, `"/static/`, `"static/`, 1), "?", "?", 1)
	})
	page = regexp.MustCompile(`<link rel="stylesheet" href="static/fonts.css[^"]*">`).ReplaceAllString(page, "")
	script := `<script>
window.addEventListener('load', function () {
  var out; try { out = (function () {` + probe + `})(); } catch (e) { out = {error: String(e)}; }
  var pre = document.createElement('pre'); pre.id = '__r'; pre.textContent = JSON.stringify(out); document.body.appendChild(pre);
});
</script>`
	page = strings.Replace(page, "</body>", script+"</body>", 1)
	file := filepath.Join(dir, "page.html")
	if err := os.WriteFile(file, []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars", "--window-size=1360,900",
		"--user-data-dir="+filepath.Join(dir, "profile"), "--virtual-time-budget=4000", "--dump-dom", "file:///"+filepath.ToSlash(file)).Output()
	if err != nil {
		t.Skipf("the browser would not run here (%v)", err)
	}
	m := regexp.MustCompile(`(?s)<pre id="__r">(.*?)</pre>`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("the probe produced no result: %.300s", out)
	}
	var res map[string]float64
	if err := json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &res); err != nil {
		t.Fatalf("probe result %q: %v", m[1], err)
	}
	if _, bad := res["error"]; bad {
		t.Fatalf("probe failed: %s", m[1])
	}
	return res
}

// F8 (ADR 0015): the banner strip is in the flow. Scrolling moves it exactly as far as the page, and it
// never overlaps what follows it: nothing scrolls visibly underneath a translucent banner.
const stripProbe = `
var strip = document.querySelector('.banner-strip'); var next = strip.nextElementSibling;
if (!strip.querySelector('.banner')) return {error: 'no banners in the strip'};
document.documentElement.style.scrollBehavior = 'auto';
var top0 = strip.getBoundingClientRect().top, nextTop0 = next.getBoundingClientRect().top;
var max = document.documentElement.scrollHeight - innerHeight; if (max < 300) return {error: 'page too short to scroll: ' + max};
scrollTo(0, 300);
var top1 = strip.getBoundingClientRect().top;
var pos = getComputedStyle(strip).position;
return {moved: top0 - top1, overlap: strip.getBoundingClientRect().bottom - next.getBoundingClientRect().top, fixedOrSticky: (pos === 'sticky' || pos === 'fixed') ? 1 : 0};`

func (e *env) allBannersOn(t *testing.T) {
	t.Helper()
	fixtures.SetAllBanners(true)
	t.Cleanup(func() { fixtures.SetAllBanners(false) })
	e.svc.Fleet.Extra = fixtures.ExtraBanners
}

func TestM3T5C_F8_TheBannerStripScrollsWithThePage(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.allBannersOn(t)
	css := e.appCSS(t)
	got := e.layoutPage(t, "/", css, stripProbe)
	if got["moved"] != 300 {
		t.Errorf("the strip moved %v px when the page scrolled 300: it is not in the flow", got["moved"])
	}
	if got["fixedOrSticky"] != 0 || got["overlap"] > 0.5 {
		t.Errorf("the strip is %v and overlaps the content below by %v px", map[float64]string{0: "in the flow", 1: "sticky or fixed"}[got["fixedOrSticky"]], got["overlap"])
	}
	// sensitivity: the sticky strip M2-FIX-C removed must be seen by the same measurement
	mutant := strings.Replace(css, ".banner-strip { position: static; }", ".banner-strip { position: sticky; top: 56px; z-index: 5; }", 1)
	if mutant == css {
		t.Fatal("the mutation did not apply: the rule is not where the test expects it")
	}
	bad := e.layoutPage(t, "/", mutant, stripProbe)
	if bad["moved"] == 300 && bad["fixedOrSticky"] == 0 {
		t.Error("the layout test cannot tell a sticky strip from one in the flow")
	}
}

// F2: the fields of one form row start on the same line, whatever the help text under their
// neighbour. The probe gives the second field a long help text and compares where the two controls begin.
const frowProbe = `
var row = document.querySelector('.frow'); var fields = row.querySelectorAll(':scope > .field');
if (fields.length < 2) return {error: 'the row has ' + fields.length + ' fields'};
var help = document.createElement('span'); help.className = 'help'; help.textContent = 'A long explanation under the second field. '.repeat(14); fields[1].appendChild(help);
var a = fields[0].querySelector('.input').getBoundingClientRect(), b = fields[1].querySelector('.input').getBoundingClientRect();
return {first: a.top, second: b.top, heightA: fields[0].getBoundingClientRect().height, heightB: fields[1].getBoundingClientRect().height};`

func TestM3T5C_F2_FormRowFieldsStartOnTheSameLine(t *testing.T) {
	e := newEnv(t)
	e.login()
	css := e.appCSS(t)
	path := "/devices/" + e.dev["FRONTDESK-01"] + "/settings"
	got := e.layoutPage(t, path, css, frowProbe)
	if d := got["first"] - got["second"]; d > 0.5 || d < -0.5 {
		t.Errorf("the controls of one row begin %v px apart (%v and %v)", d, got["first"], got["second"])
	}
	if got["heightB"] <= got["heightA"] {
		t.Fatalf("the probe did not make the fields differ in height: %v vs %v", got["heightA"], got["heightB"])
	}
	mutant := strings.Replace(css, ".frow > .field { align-content: start; min-width: 0; }", ".frow > .field { min-width: 0; }", 1)
	mutant = strings.Replace(mutant, "gap: 14px; align-items: start; }", "gap: 14px; align-items: stretch; }", 1)
	if mutant == css {
		t.Fatal("the mutation did not apply: the rules are not where the test expects them")
	}
	bad := e.layoutPage(t, path, mutant, frowProbe)
	if d := bad["first"] - bad["second"]; d < 0.5 && d > -0.5 {
		t.Error("the layout test cannot tell stretched fields from aligned ones")
	}
}
