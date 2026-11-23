package ui

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/web"
)

// P21-T2: the polish fixes to the login page, the retire dialog and the enrollment-token table.

// wideMetricsJS puts wideMetrics into a page that is screenshotted with its real fonts: the
// screenshots show the fault as the layout tests see it, not only as the design fonts render it.
const wideMetricsJS = `var s = document.createElement('style'); s.textContent = ` + "`" + wideMetrics + "`" + `; document.head.appendChild(s);`

// TestP21T2_Screenshots writes the retire dialog and the token table at 390 px (and the table at
// 1360 px), dark and light, with the real fonts and with the wide metrics, for the operator's
// before/after review. Not a gate: it runs only when CRYO_SHOTS names a directory.
func TestP21T2_Screenshots(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("CRYO_SHOTS")
	if dir == "" {
		t.Skip("CRYO_SHOTS names no directory: the screenshots are for review, not a gate")
	}
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	e.t6Token(summit, "a revoked token with quite a long label for the phone", true)
	e.t6Token(summit, "an active token with quite a long label as well", false)
	shots := []struct {
		name, path string
		widths     []int
	}{
		{"retire", "/devices/" + e.dev["FRONTDESK-01"] + "?retire=1", []int{390}},
		{"tokens", "/enroll", []int{390, 1360}},
	}
	for _, s := range shots {
		for _, w := range s.widths {
			for _, theme := range []string{"dark", "light"} {
				for _, v := range []struct{ name, js string }{{"served", ""}, {"wide", wideMetricsJS}} {
					h := 3600
					if w > 1000 {
						h = 1400
					}
					e.screenshot(t, s.path, theme, w, h, filepath.Join(dir, s.name+"_"+strconv.Itoa(w)+"_"+theme+"_"+v.name+".png"), v.js)
				}
			}
		}
	}
}

// veryWideMetrics stands in for a font far wider than the design's, wider than wideMetrics: the
// letter spacing alone makes every word about half as wide again on any machine, so a row that
// cannot wrap or shrink shows up whatever fonts the machine has. Layout must hold by structure.
const veryWideMetrics = `
html, body, body * { font-family: Verdana, "DejaVu Sans", sans-serif !important; letter-spacing: .3em !important; }`

// reachProbe measures, inside the page's main region, every visible control (buttons, the
// summaries that open the menus and dialogs, fields) against the viewport, and the page's
// sideways overflow. dialog counts the controls inside the open retire dialog, and popInside says
// whether the dialog's own box is inside the viewport.
const reachProbe = `
var out = {iw: innerWidth, overflow: document.documentElement.scrollWidth - innerWidth, controls: 0, outside: 0, dialog: 0, popInside: -1};
var pop = document.querySelector('#main details[open] > .retirepop');
if (pop) { var p = pop.getBoundingClientRect(); out.popInside = (p.left >= -0.5 && p.right <= innerWidth + 0.5) ? 1 : 0; }
document.querySelectorAll('#main .btn, #main button, #main summary, #main input:not([type=hidden]), #main select, #main textarea').forEach(function (b) {
  var r = b.getBoundingClientRect();
  if (r.width === 0 || r.height === 0 || b.closest('.tip') || !b.checkVisibility()) return;
  out.controls++;
  if (b.closest('details[open] > .retirepop')) out.dialog++;
  if (r.left < -0.5 || r.right > innerWidth + 0.5) out.outside++;
});
return out;`

// B: the open retire dialog at 390 px. Every control of the page, the dialog's own included, and
// the dialog's box are inside the viewport, in both themes, with the fallback font, the wide
// metrics and a font wider still; with the first two the page does not scroll sideways at all.
// (With the widest stand-in a status chip in the Facts list, below the dialog, still runs past the
// edge: chips are one line by design, and no control is in it.) The mutant (the two B rules taken
// out) proves the probe sees the fault: the device tabs, a row that cannot shrink on a phone,
// widened the page head, and the action bar and the open dialog with it.
func TestP21T2_B_RetireDialogAt390KeepsEveryControlInView(t *testing.T) {
	t.Parallel()
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	path := "/devices/" + e.dev["FRONTDESK-01"] + "?retire=1"
	css := e.appCSS(t)
	for _, m := range []struct{ name, css string }{{"fallback", ""}, {"wide", wideMetrics}, {"verywide", veryWideMetrics}} {
		for _, theme := range []string{"dark", "light"} {
			t.Run(m.name+"_"+theme, func(t *testing.T) {
				got := e.layout(t, layoutOpts{Path: path, CSS: css + m.css, W: 390, H: 844,
					Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `');` + reachProbe})
				if got["dialog"] < 4 { // reason, replacement, final backup, confirm, Retire device
					t.Fatalf("the open retire dialog was not measured (%v controls in it)", got["dialog"])
				}
				if got["popInside"] != 1 {
					t.Error("the open retire dialog runs past the edge of the 390 px viewport")
				}
				if m.name != "verywide" && got["overflow"] > 0.5 {
					t.Errorf("the page scrolls sideways by %.0f px", got["overflow"])
				}
				if got["outside"] > 0 {
					t.Errorf("%.0f of %.0f controls sit outside the 390 px viewport", got["outside"], got["controls"])
				}
			})
		}
	}
	// Fix batch 1 R3: the .page-head rule has its own case. A host name is one unbreakable word (a DNS
	// label may be 63 characters), so on its own it is wider than the phone; without the rule the
	// head's grid track takes that width and the action bar and the open dialog run off the screen with
	// it. (The word itself still runs past the edge: that is the host name, not a control.) The tab
	// rule cannot hide this case: it is the title row that is wide here, not the tabs.
	longHost := func(t *testing.T, css string) map[string]float64 {
		return e.layout(t, layoutOpts{Path: path, CSS: css, W: 390, H: 844,
			Probe: `var h = document.getElementById('dev-h'); if (h) h.textContent = 'WORKSTATION-' + 'X'.repeat(48);` + reachProbe})
	}
	t.Run("long_host_name", func(t *testing.T) {
		got := longHost(t, css)
		if got["dialog"] < 4 {
			t.Fatalf("the open retire dialog was not measured (%v controls in it)", got["dialog"])
		}
		if got["popInside"] != 1 || got["outside"] > 0 {
			t.Errorf("a 60-character host name pushes %.0f of %.0f controls (dialog inside the viewport: %v) off the 390 px screen", got["outside"], got["controls"], got["popInside"] == 1)
		}
	})
	noRule := strings.Replace(css, ".page-head { grid-template-columns: minmax(0, 1fr); }", "", 1)
	if noRule == css {
		t.Fatal("the mutation did not apply: the .page-head rule is not in app.css")
	}
	if bad := longHost(t, noRule); bad["outside"] == 0 && bad["popInside"] == 1 {
		t.Error("the long host name case cannot tell a page head whose grid track is bounded from one that is not")
	}
	mutant := css
	for _, rule := range []string{".page-head { grid-template-columns: minmax(0, 1fr); }", ".tabs:has(.tab-more) { flex-wrap: wrap; }"} {
		if !strings.Contains(mutant, rule) {
			t.Fatalf("the mutation did not apply: %q is not in app.css", rule)
		}
		mutant = strings.Replace(mutant, rule, "", 1)
	}
	bad := e.layout(t, layoutOpts{Path: path, CSS: mutant + veryWideMetrics, W: 390, H: 844, Probe: reachProbe})
	if bad["outside"] == 0 && bad["popInside"] == 1 {
		t.Error("the layout test cannot tell a page head that runs off a phone from one that fits")
	}
}

// dtProbe counts the token table's date-time values and those laid out on more than one line (an
// inline box broken across lines has one client rect per line), and measures the controls as reachProbe.
const dtProbe = `
var out = {iw: innerWidth, overflow: document.documentElement.scrollWidth - innerWidth, n: 0, broken: 0, controls: 0, outside: 0};
document.querySelectorAll('table.tokens .dt').forEach(function (d) {
  if (!d.checkVisibility()) return;
  out.n++;
  if (d.getClientRects().length > 1) out.broken++;
});
document.querySelectorAll('#main .btn').forEach(function (b) {
  var r = b.getBoundingClientRect();
  if (r.width === 0 || r.height === 0 || b.closest('.tip') || !b.checkVisibility()) return;
  out.controls++;
  if (r.left < -0.5 || r.right > innerWidth + 0.5) out.outside++;
});
return out;`

// C: in the token table a date-time never breaks inside itself, at desktop widths and on a phone,
// with the fallback font and with the wide metrics; at 390 px the table stays usable (no sideways
// scroll, every action in view). The mutant (the C rule taken out) proves the probe sees "1:42 / PM".
func TestP21T2_C_TokenTableTimesNeverBreakInside(t *testing.T) {
	t.Parallel()
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	e.t6Token(summit, "a revoked token with quite a long label for the phone", true)
	e.t6Token(summit, "an active token with quite a long label as well", false)
	css := e.appCSS(t)
	for _, m := range []struct{ name, css string }{{"fallback", ""}, {"wide", wideMetrics}, {"verywide", veryWideMetrics}} {
		for _, w := range []int{1360, 1100, 390} {
			for _, theme := range []string{"dark", "light"} {
				t.Run(fmt.Sprintf("%s_%d_%s", m.name, w, theme), func(t *testing.T) {
					got := e.layout(t, layoutOpts{Path: "/enroll", CSS: css + m.css, W: w, H: 900,
						Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `');` + dtProbe})
					if got["n"] < 20 { // ten tokens: an expiry and a creation time each, and "Never"
						t.Fatalf("only %.0f date-time values measured", got["n"])
					}
					if got["broken"] > 0 {
						t.Errorf("%.0f of %.0f date-time values break inside themselves", got["broken"], got["n"])
					}
					if w == 390 && got["overflow"] > 0.5 {
						t.Errorf("the page scrolls sideways by %.0f px", got["overflow"])
					}
					if got["outside"] > 0 {
						t.Errorf("%.0f of %.0f action buttons sit outside the viewport", got["outside"], got["controls"])
					}
				})
			}
		}
	}
	mutant := strings.Replace(css, "table.fleet.tokens .dt { white-space: nowrap; overflow-wrap: normal; }", "", 1)
	if mutant == css {
		t.Fatal("the mutation did not apply: the C rule is not where the test expects it")
	}
	if bad := e.layout(t, layoutOpts{Path: "/enroll", CSS: mutant + wideMetrics, W: 1360, H: 900, Probe: dtProbe}); bad["broken"] == 0 {
		t.Error("the layout test cannot tell a date-time broken across lines from one kept whole")
	}
}

// A (and fix batch 1 R2): the sign-in page and the account page say the configured session limits
// (session.idle_h and session.absolute_h, which cli hands the auth Manager as Idle and Absolute),
// worded by sessionSpanText, and neither template holds a number of hours or days for them. The
// rule: under 48 hours as hours, from 48 hours whole days as days and any other count as hours,
// singular for 1 only.
func TestP21T2_A_LoginPageSaysTheConfiguredIdleLimit(t *testing.T) {
	t.Parallel()
	e, acct := newEnv(t), newEnv(t) // acct is signed in: a signed-in browser is sent away from /login
	acct.login()
	for _, c := range []struct {
		idle, absolute time.Duration
		login, account string
	}{
		{time.Hour, 48 * time.Hour, "Sessions end after 1 hour idle and 2 days in total.", "Sessions end after 1 hour idle or 2 days."},
		{36 * time.Hour, 168 * time.Hour, "Sessions end after 36 hours idle and 7 days in total.", "Sessions end after 36 hours idle or 7 days."},
		{12 * time.Hour, 50 * time.Hour, "Sessions end after 12 hours idle and 50 hours in total.", "Sessions end after 12 hours idle or 50 hours."},
		{24 * time.Hour, 24 * time.Hour, "Sessions end after 24 hours idle and 24 hours in total.", "Sessions end after 24 hours idle or 24 hours."},
		{6 * time.Hour, 24 * 90 * time.Hour, "Sessions end after 6 hours idle and 90 days in total.", "Sessions end after 6 hours idle or 90 days."},
	} {
		e.svc.Auth.Idle, e.svc.Auth.Absolute = c.idle, c.absolute
		acct.svc.Auth.Idle, acct.svc.Auth.Absolute = c.idle, c.absolute
		name := c.idle.String() + "/" + c.absolute.String()
		res, body := e.do("GET", "/login", nil)
		if res.StatusCode != 200 {
			t.Fatalf("%s: /login answered %d", name, res.StatusCode)
		}
		mustContain(t, "login "+name, body, c.login)
		res, body = acct.do("GET", "/account", nil)
		if res.StatusCode != 200 {
			t.Fatalf("%s: /account answered %d", name, res.StatusCode)
		}
		mustContain(t, "account "+name, body, c.account)
	}
	literal := regexp.MustCompile(`\d+\s+(hours?|days?|minutes?)`)
	for _, f := range []string{"templates/pages/login.html", "templates/pages/account.html"} {
		src, err := fs.ReadFile(web.Templates, f)
		if err != nil {
			t.Fatal(err)
		}
		if m := literal.Find(src); m != nil {
			t.Errorf("%s still holds a literal %q for a configured session limit", f, m)
		}
		if !strings.Contains(string(src), "{{.Idle}} idle") || !strings.Contains(string(src), "{{.Absolute}}") {
			t.Errorf("%s does not say both configured session limits", f)
		}
	}
	for d, want := range map[time.Duration]string{time.Hour: "1 hour", 2 * time.Hour: "2 hours", 36 * time.Hour: "36 hours", 47 * time.Hour: "47 hours", 48 * time.Hour: "2 days",
		50 * time.Hour: "50 hours", 72 * time.Hour: "3 days", 168 * time.Hour: "7 days", 24 * time.Hour: "24 hours", 90 * time.Minute: "90 minutes", time.Minute: "1 minute", 30 * time.Second: "1 minute"} {
		if got := sessionSpanText(d); got != want {
			t.Errorf("sessionSpanText(%v) = %q, want %q", d, got, want)
		}
	}
}
