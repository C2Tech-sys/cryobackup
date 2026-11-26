package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// P22-R2b K3: the device page's "Verify now" posts with htmx, which swaps no 4xx or 5xx. An answer that is
// not the panel (the CSRF or origin gate's 403, a 5xx, a dropped connection) used to leave the
// administrator looking at an unchanged page. escrow.js shows it in the panel's own banner.

func TestP22R2bDevicePageLoadsEscrowJSAndHasItsBanner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	for _, key := range []string{"verified", "stale", "failed", "never", "held", "retired"} {
		body := e.page("/devices/"+e.escrowDev(t, key), 200)
		if !regexp.MustCompile(`src="/static/js/escrow\.js[^"]*"`).MatchString(body) {
			t.Errorf("%s: the device page does not load escrow.js", key)
		}
		if strings.Count(body, `id="escrow-fail"`) != 1 || !strings.Contains(body, `id="escrow-fail" class="banner crit dense" role="alert" hidden`) {
			t.Errorf("%s: no single hidden, alert escrow-fail banner in the panel", key)
		}
		// Inside the panel, so a swap replaces it with a fresh hidden one.
		panel := body[strings.Index(body, `id="escrow-panel"`):]
		if i := strings.Index(panel, `id="escrow-fail"`); i < 0 || i > strings.Index(panel, "</section>") {
			t.Errorf("%s: the banner is outside the panel", key)
		}
	}
	// CSP is script-src 'self': the script is a static file of ours, never inline.
	_, js := e.do("GET", "/static/js/escrow.js", nil)
	for _, want := range []string{"htmx:responseError", "htmx:sendError", "htmx:timeout", "#escrow-panel", "#escrow-fail"} {
		if !strings.Contains(js, want) {
			t.Errorf("escrow.js lacks %q", want)
		}
	}
	for _, bad := range []string{"fetch(", "XMLHttpRequest", "eval(", "localStorage", "sendBeacon", "http://", "https://"} {
		if strings.Contains(js, bad) {
			t.Errorf("escrow.js contains %q", bad)
		}
	}
}

const k3Driver = `<script>
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
    var x = this; calls.push({url: x.url, hx: x.headers['HX-Request']});
    setTimeout(function () {
      if (answer.status === 0) { if (x.onerror) { x.onerror(); } return; }
      x.readyState = 4; x.status = answer.status; x.response = x.responseText = answer.body; x.responseURL = x.url;
      if (x.onload) { x.onload(); }
    }, 10);
  };
  window.XMLHttpRequest = Fake;
  window.addEventListener('load', function () {
    var panel = document.getElementById('escrow-panel');
    var btn = panel.querySelector('button.btn');
    btn.click();
    var tries = 0;
    (function look() {
      var fail = document.getElementById('escrow-fail');
      var shown = !!fail && !fail.hidden;
      if (shown || ++tries > 60) {
        parent.postMessage(JSON.stringify({calls: calls.length, hx: calls.length ? calls[0].hx : '', failShown: shown,
          failText: shown ? fail.textContent : '', inPanel: !!fail && !!fail.closest('#escrow-panel'),
          swapped: document.getElementById('escrow-panel') !== panel, buttonEnabled: !btn.disabled}), '*');
        return;
      }
      setTimeout(look, 50);
    })();
  });
})();
</script>`

type k3Seen struct {
	Calls         int    `json:"calls"`
	HX            string `json:"hx"`
	FailShown     bool   `json:"failShown"`
	FailText      string `json:"failText"`
	InPanel       bool   `json:"inPanel"`
	Swapped       bool   `json:"swapped"`
	ButtonEnabled bool   `json:"buttonEnabled"`
}

// k3Browse loads the console's own device page in the browser with the real htmx and escrow.js, the
// Verify now post answered with the given status (0: a connection error), and clicks the real button.
func k3Browse(t *testing.T, e *env, page string, status int, body string) k3Seen {
	t.Helper()
	chrome := chromeOrSkip(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "static", "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []string{"htmx.min.js", "escrow.js", "theme.js"}
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
	for _, js := range []string{"htmx.min.js", "escrow.js"} {
		if !kept[js] {
			t.Fatalf("the device page does not load %s", js)
		}
	}
	page = g1StaticRE.ReplaceAllString(page, `$1="static/$2"`)
	page = regexp.MustCompile(`<link rel="stylesheet" href="static/fonts.css[^"]*">`).ReplaceAllString(page, "")
	ans, _ := json.Marshal(map[string]any{"status": status, "body": body})
	page = strings.Replace(page, "</body>", fmt.Sprintf(k3Driver, ans)+"</body>", 1)
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
	var s k3Seen
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("the page answered %q", out)
	}
	return s
}

// TestP22R2bBrowserShowsAnErrorBeforeTheHandler: a 403, a 500 and a dropped connection before the handler
// leave a visible error in #escrow-panel, the button usable again, and the panel unswapped. Killed
// mutant: escrow.js not loaded, or its handlers removed (nothing shows).
func TestP22R2bBrowserShowsAnErrorBeforeTheHandler(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	chromeOrSkip(t)
	page := e.page("/devices/"+e.escrowDev(t, "stale"), 200)
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"403 from the CSRF or origin gate", 403, "Forbidden", "The console refused the request."},
		{"500", 500, "oops", "status 500"},
		{"413", 413, "too large", "status 413"},
		{"connection error", 0, "", "The console could not be reached."},
	} {
		seen := k3Browse(t, e, page, c.status, c.body)
		if seen.Calls != 1 || seen.HX != "true" || !seen.FailShown || !seen.InPanel || seen.Swapped || !strings.Contains(seen.FailText, c.want) || !strings.Contains(seen.FailText, "Nothing was checked") {
			t.Errorf("%s: %+v", c.name, seen)
		}
		if !seen.ButtonEnabled {
			t.Errorf("%s: the button stays disabled after the error", c.name)
		}
	}
}
