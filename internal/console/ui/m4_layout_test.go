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
