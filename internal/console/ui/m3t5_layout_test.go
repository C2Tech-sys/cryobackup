package ui

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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
// names are tried. Without one the tests skip and say so, or fail when CI is set.

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
var scriptSrcRE = regexp.MustCompile(`src="/static/js/([^"?]+)`)

// layoutOpts says what to lay out and how: the markup (HTML, or the real page at Path), the stylesheet,
// the window, and which of the console's own scripts run (none by default: the layout is then pure CSS).
type layoutOpts struct {
	Path    string // the real page to fetch (as the signed-in account) ...
	HTML    string // ... or markup already in hand
	CSS     string
	Probe   string
	W, H    int      // the window; 1360 x 900 by default
	Scripts []string // names under static/js to run (fresh.js, theme.js)
}

// chromeOrSkip returns the browser, or ends the test: a skip on a developer machine without one, a
// FAILURE when the CI variable is set (a skipped layout test would let a regression through unseen).
func chromeOrSkip(t *testing.T) string {
	t.Helper()
	if c := chromeBinary(); c != "" {
		return c
	}
	if os.Getenv("CI") != "" {
		t.Fatal("CI is set and no Chrome or Chromium was found (set CRYO_CHROME): the rendered-layout tests must run in CI, not skip")
	}
	t.Skip("no Chrome or Chromium found (set CRYO_CHROME): the rendered-layout tests need a browser")
	return ""
}

// layoutPage lays out the real markup of path with css as app.css, runs probe (JavaScript that returns
// a JSON-able value) in a 1360 px wide window and returns what it returned.
func (e *env) layoutPage(t *testing.T, path, css, probe string) map[string]float64 {
	t.Helper()
	return e.layout(t, layoutOpts{Path: path, CSS: css, Probe: probe})
}

func (e *env) layout(t *testing.T, o layoutOpts) map[string]float64 {
	t.Helper()
	chrome := chromeOrSkip(t)
	if !containsStr(o.Scripts, "theme.js") { // it marks the page script-enabled (html.js): the phone drawer CSS depends on it
		o.Scripts = append(o.Scripts, "theme.js")
	}
	if o.W == 0 {
		o.W, o.H = 1360, 900
	}
	dir := t.TempDir()
	for _, d := range []string{"static", filepath.Join("static", "js")} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, tokens := e.do("GET", "/static/tokens.css", nil)
	for name, body := range map[string]string{"app.css": o.CSS, "tokens.css": tokens} {
		if err := os.WriteFile(filepath.Join(dir, "static", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, js := range o.Scripts {
		_, body := e.do("GET", "/static/js/"+js, nil)
		if err := os.WriteFile(filepath.Join(dir, "static", "js", js), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	page := o.HTML
	if page == "" {
		page = e.page(o.Path, 200)
	}
	page = scriptTagRE.ReplaceAllStringFunc(page, func(tag string) string {
		if m := scriptSrcRE.FindStringSubmatch(tag); m != nil && containsStr(o.Scripts, m[1]) {
			return `<script src="static/js/` + m[1] + `" defer></script>`
		}
		return ""
	})
	page = regexp.MustCompile(`(href|src)="/static/([^"?]+)(?:\?[^"]*)?"`).ReplaceAllString(page, `$1="static/$2"`)
	page = regexp.MustCompile(`<link rel="stylesheet" href="static/fonts.css[^"]*">`).ReplaceAllString(page, "")
	script := `<script>
window.addEventListener('load', function () {
  var out; try { out = (function () {` + o.Probe + `})(); } catch (e) { out = {error: String(e)}; }
  var pre = document.createElement('pre'); pre.id = '__r'; pre.textContent = JSON.stringify(out); document.body.appendChild(pre);
});
</script>`
	page = strings.Replace(page, "</body>", script+"</body>", 1)
	// Headless Chrome will not open a window narrower than about 500 px (and reports a few px less than asked
	// at any width), so the page is laid out in an iframe of exactly W x H: the iframe's viewport IS W x H, so
	// the media queries, position: fixed and innerWidth/innerHeight are those of a W x H screen. srcdoc keeps it
	// same-origin with the wrapper, which hands the probe's answer out.
	wrapper := `<!doctype html><html><body style="margin:0;background:#888"><iframe id="f" style="border:0;display:block;width:` + strconv.Itoa(o.W) + `px;height:` + strconv.Itoa(o.H) +
		`px" srcdoc="` + html.EscapeString(page) + `"></iframe><script>
window.addEventListener('load', function () { setTimeout(function () {
  var r = document.getElementById('f').contentDocument.getElementById('__r'), pre = document.createElement('pre'); pre.id = '__r';
  pre.textContent = r ? r.textContent : '{"error":"the probe did not run"}'; document.body.appendChild(pre); }, 800); });
</script></body></html>`
	file := filepath.Join(dir, "page.html")
	if err := os.WriteFile(file, []byte(wrapper), 0o644); err != nil {
		t.Fatal(err)
	}
	if keep := os.Getenv("CRYO_LAYOUT_KEEP"); keep != "" { // to look at what the browser was given: the page and its static/ files
		_ = os.CopyFS(filepath.Join(keep, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())+"_"+strconv.Itoa(o.W)), os.DirFS(dir))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // a hung browser must not burn the package's limit
	defer cancel()
	out, err := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars", "--window-size="+strconv.Itoa(o.W+300)+","+strconv.Itoa(o.H+300),
		"--user-data-dir="+filepath.Join(dir, "profile"), "--virtual-time-budget=6000", "--dump-dom", "file:///"+filepath.ToSlash(file)).Output()
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI is set and the browser would not run (%v)", err)
		}
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
	if w := res["iw"]; w != 0 && int(w) != o.W {
		t.Fatalf("the window is %v px wide, not the %d asked for: the measurement would not be at the real viewport", w, o.W)
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

// ---- M2: while a filter is active every matching group is open, so the rows on screen are the count ----

const visibleRowsProbe = `
function visible() { return Array.prototype.filter.call(document.querySelectorAll('tr.dev'), function (r) { return r.getClientRects().length > 0 && getComputedStyle(r).display !== 'none'; }).length; }
var bar = document.querySelector('.chipbar'); var m = bar ? /Showing (\d+) of/.exec(bar.textContent) : null;
var out = {iw: innerWidth, shown: m ? Number(m[1]) : -1, onLoad: visible(), total: document.querySelectorAll('tr.dev').length};
var chev = document.querySelector('button.chev'); if (chev) chev.click();
var b = document.getElementById('board'); var n = b.cloneNode(true); b.replaceWith(n); document.dispatchEvent(new Event('htmx:afterSwap'));
out.afterUpdate = visible(); return out;`

func TestM3T5M2_VisibleRowsEqualTheCountInARealBrowser(t *testing.T) {
	e := newEnv(t)
	e.login()
	css := e.appCSS(t)
	for _, path := range []string{"/?age=parked", "/?cust=ok", "/?age=under24", "/?age=mid", "/?state=FAILED", "/?cust=crit", "/?cust=warn"} {
		got := e.layout(t, layoutOpts{Path: path, CSS: css, Probe: visibleRowsProbe, Scripts: []string{"fresh.js", "theme.js"}})
		if got["shown"] < 0 {
			t.Errorf("%s: no \"Showing N of M\" line", path)
			continue
		}
		if got["onLoad"] != got["shown"] || got["afterUpdate"] != got["shown"] {
			t.Errorf("%s: the page says %v devices, %v rows are visible on load and %v after a live update", path, got["shown"], got["onLoad"], got["afterUpdate"])
		}
	}
	// without a filter the healthy customers still start collapsed (the default is untouched)
	all := e.layout(t, layoutOpts{Path: "/", CSS: css, Probe: visibleRowsProbe, Scripts: []string{"fresh.js", "theme.js"}})
	if all["onLoad"] >= all["total"] {
		t.Errorf("the unfiltered board shows %v of %v rows: healthy customers no longer start collapsed", all["onLoad"], all["total"])
	}
}

// ---- M3: on a phone the saved-views popover, and its Save button, are reachable ----

const viewPopProbe = `
var d = document.querySelector('details.viewmenu'); d.open = true; window.scrollTo(0, 400);
var pop = document.querySelector('.viewpop'); var btn = pop.querySelector('.vsave button[type=submit]');
function inView(r) { return r.top >= -0.5 && r.bottom <= innerHeight + 0.5 && r.left >= -0.5 && r.right <= innerWidth + 0.5; }
var r0 = pop.getBoundingClientRect();
var out = {iw: innerWidth, ih: innerHeight, popInside: inView(r0) ? 1 : 0, popTop: r0.top, popBottom: r0.bottom, pos: getComputedStyle(pop).position === 'absolute' ? 1 : 0};
pop.scrollTop = pop.scrollHeight;
var b = btn.getBoundingClientRect(), p = pop.getBoundingClientRect();
out.btnTop = b.top; out.btnBottom = b.bottom;
out.btnReachable = (inView(b) && b.top >= p.top - 0.5 && b.bottom <= p.bottom + 0.5) ? 1 : 0;
return out;`

func TestM3T5M3_ThePhoneViewsPopoverKeepsItsSaveButtonInTheViewport(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.allBannersOn(t) // the worst case: three critical banners and the folded warnings above the control
	css := e.appCSS(t)
	opt := layoutOpts{Path: "/", CSS: css, Probe: viewPopProbe, W: 390, H: 844}
	got := e.layout(t, opt)
	if got["popInside"] != 1 || got["btnReachable"] != 1 {
		t.Errorf("at 390x844 the popover spans %v-%v and the Save button %v-%v (viewport %v): not reachable", got["popTop"], got["popBottom"], got["btnTop"], got["btnBottom"], got["ih"])
	}
	// the same popover at desktop width stays where it was (under its button, inside the page)
	if d := e.layout(t, layoutOpts{Path: "/", CSS: css, Probe: viewPopProbe}); d["pos"] != 1 {
		t.Error("on a desktop the popover is no longer a dropdown under its button")
	}
	// sensitivity: the rule that stranded the button (fixed, no top, no scroll) is seen by the same probe
	i := strings.Index(css, "/* M3-T5 delta M3: phone views popover */")
	if i < 0 {
		t.Fatal("the phone popover rule is not where the test expects it")
	}
	j := strings.Index(css[i:], "\n}\n") + i + 3
	mutant := css[:i] + "@media (max-width: 639px) { .viewpop { position: fixed; left: 16px; right: 16px; top: auto; width: auto; max-width: none; } }\n" + css[j:]
	opt.CSS = mutant
	if bad := e.layout(t, opt); bad["popInside"] == 1 && bad["btnReachable"] == 1 {
		t.Error("the layout test cannot tell the stranded popover from the fixed one")
	}
}

// ---- L4: rulings 3, 4 and 5 as rendered layout ----

func (e *env) revisionsPage(t *testing.T) string {
	t.Helper()
	e.signInAs("ad-layout", "admin", nil)
	tpl := e.newTemplate("Layout policy", map[string]string{"sla": `{"stale_after_hours":24,"unseen_after_hours":100}`})
	return "/policies/" + tpl.ID
}

const revsProbe = `
var t = document.querySelector('table.revs'); if (!t) return {error: 'no revisions table'};
var rows = t.querySelectorAll('tbody tr'); var tds = rows[0].querySelectorAll('td');
var tops = Array.prototype.map.call(tds, function (x) { return x.getBoundingClientRect().top; });
var lefts = Array.prototype.map.call(tds, function (x) { return x.getBoundingClientRect().left; });
var lab = getComputedStyle(tds[1], '::before').content;
return {iw: innerWidth, sw: document.documentElement.scrollWidth, headH: t.querySelector('thead').getBoundingClientRect().height,
  stacked: (tops[1] > tops[0] + 1 && tops[2] > tops[1] + 1) ? 1 : 0, sameLeft: Math.abs(lefts[0] - lefts[1]) < 1 ? 1 : 0,
  sideBySide: (Math.abs(tops[0] - tops[1]) < 2 && lefts[1] > lefts[0]) ? 1 : 0, labelled: /Changed by/.test(lab) ? 1 : 0, cardW: rows[0].getBoundingClientRect().width};`

func TestM3T5L4_Ruling3_TheRevisionsTableIsACardListOnAPhone(t *testing.T) {
	e := newEnv(t)
	path := e.revisionsPage(t)
	css := e.appCSS(t)
	phone := e.layout(t, layoutOpts{Path: path, CSS: css, Probe: revsProbe, W: 390, H: 844})
	if phone["headH"] > 1 || phone["stacked"] != 1 || phone["sameLeft"] != 1 || phone["labelled"] != 1 || phone["sw"] > phone["iw"] {
		t.Errorf("at 390 px the revisions are not a card list: %v", phone)
	}
	wide := e.layout(t, layoutOpts{Path: path, CSS: css, Probe: revsProbe})
	if wide["sideBySide"] != 1 || wide["headH"] < 10 || wide["labelled"] != 0 {
		t.Errorf("at 1360 px the revisions are still a table: %v", wide)
	}
	i, j := strings.Index(css, "/* Ruling 3:"), strings.Index(css, "/* E: device history rows")
	if i < 0 || j < i {
		t.Fatal("the card-list rules are not where the test expects them")
	}
	mutant := css[:i] + css[j:]
	if bad := e.layout(t, layoutOpts{Path: path, CSS: mutant, Probe: revsProbe, W: 390, H: 844}); bad["stacked"] == 1 {
		t.Error("the layout test cannot tell the card list from the table")
	}
}

const tabsProbe = `
var tabs = document.querySelector('nav.tabs'); var vis = function (el) { return el.getClientRects().length > 0 && getComputedStyle(el).display !== 'none'; };
var direct = Array.prototype.filter.call(tabs.querySelectorAll(':scope > a'), vis).length;
var more = tabs.querySelector('.tab-more'); var out = {iw: innerWidth, direct: direct, moreVisible: vis(more) ? 1 : 0, sw: document.documentElement.scrollWidth};
more.querySelector('summary').click();
var pop = more.querySelector('.tab-more-pop'); var links = Array.prototype.filter.call(pop.querySelectorAll('a'), vis).length; var r = pop.getBoundingClientRect();
out.l = r.left; out.r = r.right; out.t = r.top; out.b = r.bottom; out.ih = innerHeight;
out.popLinks = out.moreVisible ? links : 0; out.popInside = (!out.moreVisible || (r.left >= 0 && r.right <= innerWidth && r.top >= 0 && r.bottom <= innerHeight)) ? 1 : 0;
return out;`

func TestM3T5L4_Ruling4_PhoneDeviceTabsHaveAMoreTab(t *testing.T) {
	e := newEnv(t)
	e.login()
	css := e.appCSS(t)
	path := "/devices/" + e.dev["FRONTDESK-01"]
	phone := e.layout(t, layoutOpts{Path: path, CSS: css, Probe: tabsProbe, W: 390, H: 844})
	if phone["direct"] != 3 || phone["moreVisible"] != 1 || phone["popLinks"] < 3 || phone["popInside"] != 1 || phone["sw"] > phone["iw"] {
		t.Errorf("at 390 px: want 3 tabs, a More tab holding the rest inside the viewport: %v", phone)
	}
	wide := e.layout(t, layoutOpts{Path: path, CSS: css, Probe: tabsProbe})
	if wide["direct"] < 5 || wide["moreVisible"] != 0 {
		t.Errorf("at 1360 px every tab is in the row and no More tab shows: %v", wide)
	}
	mutant := strings.Replace(css, ".tabs .tab-ov { display: none; }", ".tabs .tab-ov { }", 1)
	if mutant == css {
		t.Fatal("the mutation did not apply")
	}
	if bad := e.layout(t, layoutOpts{Path: path, CSS: mutant, Probe: tabsProbe, W: 390, H: 844}); bad["direct"] == 3 {
		t.Error("the layout test cannot tell the overflow from the visible tabs")
	}
}

const blastProbe = `
var dl = document.querySelector('dl.blast'); if (!dl) return {error: 'no review list'};
var dts = dl.querySelectorAll('dt'), dds = dl.querySelectorAll('dd'); var ok = 1;
for (var i = 0; i < dts.length; i++) { var a = dts[i].getBoundingClientRect(), b = dds[i].getBoundingClientRect(); if (b.top < a.bottom - 0.5 || Math.abs(a.left - b.left) > 1) ok = 0; }
return {iw: innerWidth, n: dts.length, labelsAbove: ok};`

func TestM3T5L4_Ruling5_ReviewLabelsSitAboveTheirValues(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	north := e.customerID("Northwind Dental")
	tpl := e.newTemplate("Strict", map[string]string{"sla": `{"stale_after_hours":24,"unseen_after_hours":100}`})
	res, review := e.post("/ui/customers/"+north+"/policy/impact", url.Values{"policy_id": {tpl.ID}}, false)
	if res.StatusCode != 200 {
		t.Fatalf("review: %d", res.StatusCode)
	}
	css := e.appCSS(t)
	for _, w := range []int{1360, 390} {
		h := 900
		if w == 390 {
			h = 844
		}
		got := e.layout(t, layoutOpts{HTML: review, CSS: css, Probe: blastProbe, W: w, H: h})
		if got["n"] < 2 || got["labelsAbove"] != 1 {
			t.Errorf("at %d px the review labels are not above their values: %v", w, got)
		}
	}
	mutant := strings.Replace(css, ".review .blast { grid-template-columns: 1fr; gap: 2px 0; }", "", 1)
	if mutant == css {
		t.Fatal("the mutation did not apply")
	}
	if bad := e.layout(t, layoutOpts{HTML: review, CSS: mutant, Probe: blastProbe}); bad["labelsAbove"] == 1 {
		t.Error("the layout test cannot tell labels beside their values from labels above")
	}
}
