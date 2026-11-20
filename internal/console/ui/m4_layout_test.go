package ui

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// P20-W3B: the new pages laid out by a real browser, at 390 px and desktop width, dark and light:
// nothing overflows the page sideways, every button sits inside the viewport, and an open popover
// (a recipient's Edit form, an alarm's Acknowledge form) stays inside the viewport too. The
// fixture world plus the longest recipient the store accepts is the worst case.

// popProbe opens the first details matching sel, then measures as layoutProbe does, plus how far
// the open popover reaches outside the viewport (0 when it fits).
func popProbe(sel string) string {
	open := ""
	if sel != "" {
		open = `var d = document.querySelector('` + sel + `'); if (!d) return {error: 'no ` + sel + `'}; d.open = true;`
	}
	return open + strings.Replace(layoutProbe, "return out;", `
out.popOutside = 0; out.pops = 0;
document.querySelectorAll('details[open] > .rowmenu-pop').forEach(function (p) {
  var r = p.getBoundingClientRect(); out.pops++;
  out.popOutside = Math.max(out.popOutside, -r.left, r.right - innerWidth);
});
return out;`, 1)
}

func TestP20W3B_LayoutAtBothWidthsAndThemes(t *testing.T) {
	chromeOrSkip(t)
	e := newM4Env(t)
	e.login()
	name, addr := longRecipient()
	e.mustPost("/ui/settings/email/recipients", url.Values{"name": {name}, "address": {addr}, "alerts": {"1"}}, 303)
	css := e.appCSS(t)
	pages := []struct{ name, path, open string }{
		{"email", "/settings/email", ""},
		{"email_edit_open", "/settings/email", "details.rcpt-menu"},
		{"email_preview", "/settings/email?preview=1", ""},
		{"device_acked", "/devices/" + e.dev["FRONTDESK-01"], ""},
		{"device_ack_open", "/devices/" + e.dev["RECEPTION-2"], "details.ackmenu"},
		{"customer_missing_open", "/customers/" + e.customerID("Blue Ridge Accounting"), "details.ackmenu"},
		{"design_mail", "/design/mail", ""},
	}
	for _, pg := range pages {
		for _, w := range []int{390, 1360} {
			for _, theme := range []string{"dark", "light"} {
				t.Run(fmt.Sprintf("%s_%d_%s", pg.name, w, theme), func(t *testing.T) {
					got := e.layout(t, layoutOpts{Path: pg.path, CSS: css, W: w, H: 900,
						Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `');` + popProbe(pg.open)})
					if msg, bad := got["error"]; bad {
						t.Fatalf("probe: %v", msg)
					}
					if got["overflow"] > 0.5 {
						t.Errorf("the page overflows sideways by %.0f px", got["overflow"])
					}
					if got["outside"] > 0 {
						t.Errorf("%.0f buttons sit outside the viewport", got["outside"])
					}
					if pg.open != "" && got["pops"] < 1 {
						t.Error("the popover did not open: this measures nothing")
					}
					if got["popOutside"] > 0.5 {
						t.Errorf("the open popover reaches %.0f px outside the viewport", got["popOutside"])
					}
				})
			}
		}
	}
	// The mutant: without the phone rules that put an open row popover in the flow, the Edit form of a
	// recipient runs off the side of a phone, and the same probe sees it.
	mutant := css
	for _, rule := range []string{"table.fleet.rcpts details.rowmenu, li.alarm details.rowmenu { position: static; }",
		"table.fleet.rcpts details.rowmenu[open] > .rowmenu-pop, li.alarm details.rowmenu[open] > .rowmenu-pop { position: static; width: auto; max-width: none; margin-top: 8px; box-shadow: none; }"} {
		m := strings.Replace(mutant, rule, "", 1)
		if m == mutant {
			t.Fatalf("the phone popover rule %q is not where the test expects it", rule)
		}
		mutant = m
	}
	bad := e.layout(t, layoutOpts{Path: "/settings/email", CSS: mutant, W: 390, H: 900, Probe: popProbe("details.rcpt-menu")})
	if bad["popOutside"] <= 0.5 && bad["outside"] == 0 {
		t.Errorf("the layout test cannot tell a stranded popover from one in the flow: %v", bad)
	}
}

// ADDED (P20-CI): the every-banner world holds C18 to C21 now, and the strip still holds at 390 px and
// desktop width: in the flow (it scrolls with the page), never over the content, never wider than the
// window, with the M4 warnings folded into the one summary line.
func TestP20W3B_TheStripHoldsWithTheM4BannersAt390AndDesktop(t *testing.T) {
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	e.allBannersOn(t)
	body := e.page("/", 200)
	for _, label := range []string{"NO ALERT RECIPIENT", "HEARTBEAT PING FAILING", "HEARTBEAT NOT CHECKED", "NO DIGEST RECIPIENT", "MAIL UNDELIVERED"} {
		if !strings.Contains(body, label) {
			t.Errorf("the strip lacks %s", label)
		}
	}
	css := e.appCSS(t)
	probe := strings.Replace(stripProbe, "return {moved:", "return {overflow: document.documentElement.scrollWidth - innerWidth, moved:", 1)
	for _, w := range []int{390, 1360} {
		got := e.layout(t, layoutOpts{Path: "/", CSS: css, W: w, H: 844, Probe: probe})
		if got["moved"] != 300 || got["fixedOrSticky"] != 0 || got["overlap"] > 0.5 {
			t.Errorf("%d px: the strip moved %v px for 300, is %v sticky, overlaps by %v px", w, got["moved"], got["fixedOrSticky"], got["overlap"])
		}
		if got["overflow"] > 0.5 {
			t.Errorf("%d px: the page overflows sideways by %.0f px with every banner up", w, got["overflow"])
		}
	}
}

// F4: /settings at 390 px with wide font metrics: the digest rows' "Edit in e-mail settings" end inside
// the table's visible box, and nothing of the table needs a sideways scroll. The mutant without the
// F4 rules is seen by the same probe.
func TestP20W3B_F4_SettingsEditLinksFitAPhone(t *testing.T) {
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	css := e.appCSS(t) + wideMetrics
	probe := `var tw = document.querySelector('.settingsform ~ section .table-wrap') || document.querySelectorAll('.table-wrap')[0];
var box = tw.getBoundingClientRect(), out = {iw: innerWidth, links: 0, past: 0, scroll: tw.scrollWidth - tw.clientWidth, overflow: document.documentElement.scrollWidth - innerWidth};
document.querySelectorAll('a.editlink').forEach(function (a) { var r = a.getBoundingClientRect(); out.links++; out.past = Math.max(out.past, r.right - box.right); });
return out;`
	got := e.layout(t, layoutOpts{Path: "/settings", CSS: css, W: 390, H: 900, Probe: probe})
	if got["links"] != 2 {
		t.Fatalf("%v edit links on /settings, want the two digest rows'", got["links"])
	}
	if got["past"] > 0.5 || got["scroll"] > 0.5 || got["overflow"] > 0.5 {
		t.Errorf("at 390 px the edit links end %.0f px past the table, which scrolls %.0f px (page overflow %.0f)", got["past"], got["scroll"], got["overflow"])
	}
	mutant := strings.Replace(css, "table.fleet.compact td .btn.editlink { white-space: normal;", "table.fleet.compact td .btn.editlink { white-space: nowrap;", 1)
	mutant = strings.Replace(mutant, g1Rule, "", 1)
	if strings.Contains(mutant, "editlink { white-space: normal;") || strings.Contains(mutant, "table.fleet.settable td.wrap") {
		t.Fatal("the F4 rules are not where the test expects them")
	}
	if bad := e.layout(t, layoutOpts{Path: "/settings", CSS: mutant, W: 390, H: 900, Probe: probe}); bad["past"] <= 0.5 && bad["scroll"] <= 0.5 {
		t.Errorf("the probe cannot tell links that run past the table from ones that wrap: %v", bad)
	}
}

// F4: on a phone the delivery log hides the Attempt column, so a row past its first attempt says
// "attempt N" in its stacked card; a first attempt says nothing.
func TestP20W3B_F4_PhoneDeliveryRowsShowTheirAttempt(t *testing.T) {
	chromeOrSkip(t)
	e := newM4Env(t)
	e.login()
	probe := `var out = {iw: innerWidth, shown: 0, hidden: 0, text: 0};
document.querySelectorAll('table.dlog td.att').forEach(function (td) {
  var vis = td.checkVisibility(); if (vis) { out.shown++; if (/^attempt \d+$/.test(td.innerText.trim())) out.text++; } else out.hidden++; });
return out;`
	got := e.layout(t, layoutOpts{Path: "/settings/email", CSS: e.appCSS(t), W: 390, H: 900, Probe: probe})
	// the fixture log's first page holds two rows past their first attempt (attempt 2 and attempt 4)
	if got["shown"] != 2 || got["text"] != 2 || got["hidden"] < 20 {
		t.Errorf("at 390 px: %v attempt cells shown (%v reading \"attempt N\"), %v hidden; want the 2 later attempts shown", got["shown"], got["text"], got["hidden"])
	}
}

// g1Rule is the phone rule of the /settings table (G1).
const g1Rule = "@media (max-width: 640px) { table.fleet.settable td.wrap { min-width: 0; overflow-wrap: break-word; } table.fleet.settable td.val { overflow-wrap: normal; word-break: keep-all; } }"

// G1: at 390 px, with the house fonts and with wide-font metrics, no /settings value breaks inside a word
// or a number: each value cell occupies no more lines than it has space-separated words. And nothing
// overflows sideways, neither the page nor the table's own box. The F4 rule that broke values ("40/0/da/ys")
// is the mutant.
func TestP20W3B_G1_SettingsValuesNeverBreakInsideAWord(t *testing.T) {
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	base := e.appCSS(t)
	if !strings.Contains(base, g1Rule) {
		t.Fatal("the G1 rule is not where the test expects it")
	}
	probe := `var tw = document.querySelector('table.settable').closest('.table-wrap');
var out = {iw: innerWidth, rows: 0, broken: 0, scroll: tw.scrollWidth - tw.clientWidth, overflow: document.documentElement.scrollWidth - innerWidth};
document.querySelectorAll('table.settable td.val').forEach(function (td) {
  out.rows++;
  var words = td.textContent.trim().split(/\s+/).length, r = document.createRange(); r.selectNodeContents(td);
  var tops = {}; Array.prototype.forEach.call(r.getClientRects(), function (q) { if (q.width > 0) tops[Math.round(q.top)] = 1; });
  if (Object.keys(tops).length > words) out.broken++;
});
return out;`
	for _, m := range []struct{ name, css string }{{"house", ""}, {"wide", wideMetrics}} {
		got := e.layout(t, layoutOpts{Path: "/settings", CSS: base + m.css, W: 390, H: 900, Probe: probe})
		if got["rows"] < 4 {
			t.Fatalf("%s: %v value cells: the probe measures nothing", m.name, got["rows"])
		}
		if got["broken"] > 0 || got["scroll"] > 0.5 || got["overflow"] > 0.5 {
			t.Errorf("%s fonts at 390 px: %v values break inside a word; the table scrolls %.0f px; the page overflows %.0f px", m.name, got["broken"], got["scroll"], got["overflow"])
		}
	}
	old := strings.Replace(base, g1Rule, "@media (max-width: 640px) { table.fleet.settable td.wrap { min-width: 0; } table.fleet.settable td { white-space: normal; overflow-wrap: anywhere; } }", 1)
	if bad := e.layout(t, layoutOpts{Path: "/settings", CSS: old, W: 390, H: 900, Probe: probe}); bad["broken"] == 0 {
		t.Errorf("the probe does not see the values the F4 rule broke: %v", bad)
	}
}
