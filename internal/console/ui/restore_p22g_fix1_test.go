package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
)

// P22-G fix batch 1 (blind check): every wizard refusal is visible with JavaScript on (G1), the
// 64 KiB cap is enforced before the gate parses the form (G2), a create with an invalid posted
// path is refused (G3).

const g1Docs = "/C/Users/Public/Documents"

type g1Case struct {
	name, host string
	form       url.Values
	status     int
	text       string
}

func g1Form(extra ...string) url.Values {
	v := vv("snapshot", fixtures.FixtureSnapshotID, "cur", g1Docs)
	for i := 0; i+1 < len(extra); i += 2 {
		v.Add(extra[i], extra[i+1])
	}
	return v
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func isBoxAlone(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), `<form id="restore-box"`) && !strings.Contains(body, "</html>")
}

// The same post is answered with its real 4xx and the banner to a plain form post, and with 200
// and the box (banner inside) to an htmx post, because htmx swaps no 4xx.
func TestP22G_G1_EveryRefusalIsABoxForHTMXAndKeepsItsStatusForABrowserPost(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedP22(t)
	e.be.Files = fixtures.P22Files()
	doc := g1Docs + "/Quarterly-Report.txt"
	over := g1Form("step", "select", "action", "submit")
	for i := 1; i <= 51; i++ {
		over.Add("path", fmt.Sprintf("%s/scan-%04d.pdf", fixtures.P22BigFolder, i))
	}
	cases := []g1Case{
		{"nothing selected", "FRONTDESK-01", g1Form("step", "select", "action", "submit"), http.StatusUnprocessableEntity, "Choose a file first"},
		{"over the limit", "FRONTDESK-01", over, http.StatusUnprocessableEntity, "51 items are selected; one restore takes at most 50."},
		{"wrong host name", "DR-OFFICE-PC", g1Form("step", "review", "mode", "replace", "confirm_host", "dr-office-pc", "action", "submit", "path", doc), http.StatusBadRequest, "The host name does not match"},
		{"invalid path", "FRONTDESK-01", g1Form("step", "review", "mode", "zip", "action", "submit", "path", doc, "path", "relative/../x"), http.StatusBadRequest, "Some selected items are not valid."},
		{"agent too old", fixtures.P22OldAgentHost, g1Form("step", "review", "mode", "side", "action", "submit", "path", doc), http.StatusConflict, "The agent on this device is too old"},
		{"restore active", "OPS-ND-01", g1Form("step", "review", "mode", "original", "action", "submit", "path", doc), http.StatusConflict, "A restore is already active for this device"},
		{"retired, device mode", "OLD-RECEPTION-PC", g1Form("step", "review", "mode", "original", "action", "submit", "path", doc), http.StatusConflict, "This device is retired: download only"},
		{"purging", "SPARE-PC-09", g1Form("step", "review", "mode", "zip", "action", "submit", "path", doc), http.StatusConflict, "This device is being purged"},
		{"customer-held", "LEGAL-PC-01", g1Form("step", "review", "mode", "zip", "action", "submit", "path", doc), http.StatusConflict, "Customer-held key"},
	}
	for _, c := range cases {
		id := e.dev[c.host]
		e.do("GET", "/devices/"+id+"/restore", nil) // an empty snapshot cache is filled from the repository
		path := "/ui/devices/" + id + "/restore"
		before := e.restoreCount(id)
		res, body := e.post(path, cloneValues(c.form), false)
		if res.StatusCode != c.status || !strings.Contains(body, c.text) || !strings.Contains(body, "</html>") {
			t.Errorf("%s, browser post: %d, banner %v, full page %v (want %d)", c.name, res.StatusCode, strings.Contains(body, c.text), strings.Contains(body, "</html>"), c.status)
		}
		res, box := e.post(path, cloneValues(c.form), true)
		switch {
		case res.StatusCode != http.StatusOK:
			t.Errorf("%s, htmx post: %d, want 200 so htmx swaps it", c.name, res.StatusCode)
		case !isBoxAlone(box):
			t.Errorf("%s, htmx post: the answer is not the box alone", c.name)
		case !strings.Contains(box, c.text):
			t.Errorf("%s, htmx post: the box lacks %q", c.name, c.text)
		}
		if e.restoreCount(id) != before {
			t.Errorf("%s: a refusal wrote a restore", c.name)
		}
	}
}

// The two refusals that need their own session state: a stale check (403) and a wrong code (401).
func TestP22G_G1_StaleAndWrongCodeRefusalsAreBoxesForHTMX(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	body := e.walkToReview(t, id, g1Docs, []string{g1Docs + "/Quarterly-Report.txt"}, "zip")
	e.advance(6 * time.Minute)
	path := "/ui/devices/" + id + "/restore"
	for _, tc := range []struct {
		name   string
		extra  url.Values
		status int
		text   string
	}{
		{"stale", vv("action", "submit"), http.StatusForbidden, "A recent MFA check is required."},
		{"wrong code", vv("action", "submit", "code", "000000"), http.StatusUnauthorized, ""},
	} {
		form := func() url.Values {
			v := browserForm(t, body)
			for k, vals := range tc.extra {
				v[k] = vals
			}
			return v
		}
		res, b := e.post(path, form(), false)
		if res.StatusCode != tc.status || !strings.Contains(b, `name="code"`) {
			t.Errorf("%s, browser post: %d, want %d", tc.name, res.StatusCode, tc.status)
		}
		res, box := e.post(path, form(), true)
		if res.StatusCode != http.StatusOK || !isBoxAlone(box) || !strings.Contains(box, `name="code"`) || !strings.Contains(box, `class="banner`) {
			t.Errorf("%s, htmx post: %d, box alone %v", tc.name, res.StatusCode, isBoxAlone(box))
		}
		if tc.text != "" && !strings.Contains(box, tc.text) {
			t.Errorf("%s, htmx post: no %q", tc.name, tc.text)
		}
	}
	if e.restoreCount(id) != 0 {
		t.Fatal("a refusal wrote a restore")
	}
}

// G2: the cap is enforced before the gate: even a request with no session at all is answered 413
// when its body is over 64 KiB, and a normal 50-item basket goes through.
func TestP22G_G2_TheBodyCapIsEnforcedBeforeTheGate(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // no login: the gate would answer a sign-in redirect or 401
	dev := e.dev["DR-OFFICE-PC"]
	big := url.Values{"junk": {strings.Repeat("a", 64<<10+1)}}
	if res, _ := e.do("POST", "/ui/devices/"+dev+"/restore", big, "Origin", e.srv.URL); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("64 KiB + 1 without a session: %d, want 413", res.StatusCode)
	}
	small := url.Values{"junk": {strings.Repeat("a", 1000)}}
	if res, _ := e.do("POST", "/ui/devices/"+dev+"/restore", small, "Origin", e.srv.URL); res.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatal("a small body was refused as too large")
	}
	// Signed in: over the cap is 413, a 50-item basket of long paths is fine.
	e.login()
	if res, _ := e.post("/ui/devices/"+dev+"/restore", vv("junk", strings.Repeat("a", 64<<10+1)), false); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("64 KiB + 1 signed in: %d", res.StatusCode)
	}
	v := vv("snapshot", fixtures.FixtureSnapshotID, "snapcur", fixtures.FixtureSnapshotID, "step", "select", "cur", g1Docs, "action", "submit")
	for i := 1; i <= 50; i++ {
		v.Add("path", fmt.Sprintf("%s/%s-%02d.txt", g1Docs, strings.Repeat("long-name-", 20), i))
	}
	e.do("GET", "/devices/"+dev+"/restore", nil)
	if res, body := e.post("/ui/devices/"+dev+"/restore", v, false); res.StatusCode != http.StatusOK || !strings.Contains(body, `name="step" value="dest"`) {
		t.Fatalf("a 50-item basket: %d", res.StatusCode)
	}
}

// G3: a create that carries an invalid path is refused, not trimmed; the stale banner title ends
// with a period.
func TestP22G_G3_AnInvalidPathRefusesTheCreate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	e.do("GET", "/devices/"+id+"/restore", nil)
	ok := g1Docs + "/Quarterly-Report.txt"
	for _, bad := range []string{"relative/x", g1Docs + "/../x", "/", "/C/x\x00y"} {
		v := g1Form("step", "review", "mode", "zip", "action", "submit", "path", ok, "path", bad)
		res, body := e.post("/ui/devices/"+id+"/restore", v, false)
		if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Some selected items are not valid.") {
			t.Errorf("%q: %d", bad, res.StatusCode)
		}
		if e.restoreCount(id) != 0 {
			t.Fatalf("%q created a restore", bad)
		}
	}
	// The same post without the bad path creates the restore.
	if res, _ := e.post("/ui/devices/"+id+"/restore", g1Form("step", "review", "mode", "zip", "action", "submit", "path", ok), false); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid create: %d", res.StatusCode)
	}
	e.advance(6 * time.Minute)
	_, body := e.post("/ui/devices/"+id+"/restore", g1Form("step", "review", "mode", "zip", "action", "submit", "path", ok), false)
	if !strings.Contains(body, "A recent MFA check is required.") || strings.Contains(body, "A recent MFA check is required</b>") {
		t.Fatal("the stale banner title lacks its period")
	}
}

// ---- the headless-browser proof (G1) ----

var g1StaticRE = regexp.MustCompile(`(href|src)="/static/([^"?]+)(?:\?[^"]*)?"`)

// g1Driver replaces XMLHttpRequest with one that answers the wizard post with the status and body
// the console really gave (captured by the test), then runs the real htmx and the real restore.js
// on the real review page, clicks the real button and reports what the operator would see.
const g1Driver = `<script>
(function () {
  var answer = %s, calls = [];
  function Fake() { this.headers = {}; this.readyState = 0; this.status = 0; this.upload = {addEventListener: function () {}}; this.timeout = 0; }
  Fake.prototype.open = function (m, u) { this.method = m; this.url = u; this.readyState = 1; };
  Fake.prototype.setRequestHeader = function (k, v) { this.headers[k] = v; };
  Fake.prototype.overrideMimeType = function () {};
  Fake.prototype.addEventListener = function (t, f) { this['on' + t] = f; };
  Fake.prototype.abort = function () {};
  Fake.prototype.getResponseHeader = function (n) { return n.toLowerCase() === 'content-type' ? 'text/html; charset=utf-8' : null; };
  Fake.prototype.getAllResponseHeaders = function () { return 'content-type: text/html; charset=utf-8\r\n'; };
  Fake.prototype.send = function (body) {
    var x = this; calls.push({hx: x.headers['HX-Request']});
    setTimeout(function () {
      x.readyState = 4; x.status = answer.status; x.response = x.responseText = answer.body; x.responseURL = x.url;
      if (x.onload) { x.onload(); }
    }, 10);
  };
  window.XMLHttpRequest = Fake;
  window.addEventListener('load', function () {
    var box = document.getElementById('restore-box'), host = document.getElementById('confirm-host');
    if (host) { host.value = answer.typed; }
    document.querySelector('button.primary[value="submit"]').click();
    var tries = 0;
    (function look() {
      var fail = document.getElementById('restore-fail');
      var shown = !!fail && !fail.hidden;
      var swapped = document.getElementById('restore-box') !== box;
      var banner = document.querySelector('#restore-box .banner:not(#restore-fail)');
      if (shown || (swapped && banner) || ++tries > 60) {
        parent.postMessage(JSON.stringify({calls: calls.length, hx: calls.length ? calls[0].hx : '', failShown: shown, failText: shown ? fail.textContent : '',
          bannerText: banner ? banner.textContent : '', swapped: swapped,
          stillReview: !!document.querySelector('#restore-box input[name="step"][value="review"]')}), '*');
        return;
      }
      setTimeout(look, 50);
    })();
  });
})();
</script>`

type g1Seen struct {
	Calls       int    `json:"calls"`
	HX          string `json:"hx"`
	FailShown   bool   `json:"failShown"`
	FailText    string `json:"failText"`
	BannerText  string `json:"bannerText"`
	Swapped     bool   `json:"swapped"`
	StillReview bool   `json:"stillReview"`
}

// g1Browse loads page (the console's own review page) in the browser, with the post answered as given.
func g1Browse(t *testing.T, e *env, page string, status int, body, typed string) g1Seen {
	t.Helper()
	chrome := chromeOrSkip(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "static", "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []string{"htmx.min.js", "restore.js", "theme.js"}
	for _, f := range []string{"app.css", "tokens.css"} {
		_, b := e.do("GET", "/static/"+f, nil)
		if err := os.WriteFile(filepath.Join(dir, "static", f), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, js := range want {
		_, b := e.do("GET", "/static/js/"+js, nil)
		if len(b) == 0 {
			t.Fatalf("static/js/%s is not served", js)
		}
		if err := os.WriteFile(filepath.Join(dir, "static", "js", js), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kept := map[string]bool{}
	page = scriptTagRE.ReplaceAllStringFunc(page, func(tag string) string {
		if m := scriptSrcRE.FindStringSubmatch(tag); m != nil && containsStr(want, m[1]) {
			kept[m[1]] = true
			return `<script src="static/js/` + m[1] + `" defer></script>`
		}
		return ""
	})
	for _, js := range []string{"htmx.min.js", "restore.js"} {
		if !kept[js] {
			t.Fatalf("the restore page does not load %s", js)
		}
	}
	page = g1StaticRE.ReplaceAllString(page, `$1="static/$2"`)
	page = regexp.MustCompile(`<link rel="stylesheet" href="static/fonts.css[^"]*">`).ReplaceAllString(page, "")
	ans, _ := json.Marshal(map[string]any{"status": status, "body": body, "typed": typed})
	page = strings.Replace(page, "</body>", fmt.Sprintf(g1Driver, ans)+"</body>", 1)
	if err := os.WriteFile(filepath.Join(dir, "page.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := sharedLayoutBrowser(chrome)
	var out string
	if err == nil {
		out, err = b.run(dir, 1100, 900, 2*time.Minute)
	}
	if err != nil || out == "" {
		browserWouldNotRun(t, fmt.Errorf("%v (answer %q)", err, out))
	}
	var s g1Seen
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("the page answered %q", out)
	}
	return s
}

func TestP22G_G1_BrowserShowsTheRefusalWithJavaScriptOn(t *testing.T) {
	e := newEnv(t)
	e.login()
	chromeOrSkip(t)
	id := e.dev["DR-OFFICE-PC"]
	review := e.walkToReview(t, id, "/C/Users/Public/Desktop", []string{"/C/Users/Public/Desktop/Welcome.txt"}, "replace")

	// The wrong host name: the console's real answer to the htmx post, swapped by the real htmx.
	form := browserForm(t, review)
	form.Set("confirm_host", "dr-office-pc")
	form.Set("action", "submit")
	res, box := e.post("/ui/devices/"+id+"/restore", form, true)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the console's htmx answer: %d", res.StatusCode)
	}
	seen := g1Browse(t, e, review, res.StatusCode, box, "dr-office-pc")
	if seen.Calls != 1 || seen.HX != "true" || !seen.Swapped || !strings.Contains(seen.BannerText, "The host name does not match") || !seen.StillReview {
		t.Fatalf("wrong host name with JavaScript on: %+v", seen)
	}

	// An answer that is not the box (the body cap's plain 413, a 500): htmx swaps nothing, so
	// restore.js says so in the box.
	seen = g1Browse(t, e, review, http.StatusRequestEntityTooLarge, "The request is too large.", "x")
	if seen.Swapped || !seen.FailShown || !strings.Contains(seen.FailText, "The selection is too large.") {
		t.Fatalf("413 with JavaScript on: %+v", seen)
	}
	seen = g1Browse(t, e, review, http.StatusInternalServerError, "oops", "x")
	if seen.Swapped || !seen.FailShown || !strings.Contains(seen.FailText, "status 500") {
		t.Fatalf("500 with JavaScript on: %+v", seen)
	}
}
