package ui

import (
	"fmt"
	"strings"
	"testing"
)

// P20-CI: TestADR19_LayoutAtBothWidthsAndThemes/has_devices_390_* failed on ubuntu-latest only ("the page
// overflows sideways by 25 px"). Linux Chrome falls back to DejaVu Sans, wider than the Windows fonts, and the
// widest state chip of the customer page's "Needs attention" table ("NEVER BACKED UP · no successful run",
// white-space: nowrap) made the table's narrowest width more than a phone has; the card's grid items, sized to
// their content, carried that out to the page.

// spacedMetrics widens every text run on any machine, whatever fonts it has: the mutant check below uses it, so
// it does not depend on Verdana or DejaVu Sans being installed.
const spacedMetrics = `
html, body, body * { letter-spacing: .15em !important; }`

// hasDevicesProbe is layoutProbe plus how far the "Needs attention" table scrolls inside its region (0 when the
// chips wrap and nothing is hidden) and a checksum of every element's box (the desktop layout must not move).
var hasDevicesProbe = strings.Replace(layoutProbe, "return out;", `
var tw = document.querySelector('#customer-overview .table-wrap');
out.attnScroll = tw ? tw.scrollWidth - tw.clientWidth : -1;
var h = 0;
document.querySelectorAll('body *').forEach(function (el) {
  var r = el.getBoundingClientRect();
  [r.left, r.top, r.width, r.height].forEach(function (v) { h = (h * 31 + Math.round(v * 10)) % 1000000007; });
});
out.boxes = h;
return out;`, 1)

func TestP20CI_HasDevicesHoldsAt390WithWideFontMetrics(t *testing.T) {
	t.Parallel()
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	css := e.appCSS(t)
	path := "/customers/" + e.customerID("Summit Eye Care")
	opts := func(c string, w int, theme string) layoutOpts {
		return layoutOpts{Path: path, CSS: c, W: w, H: 900, Scripts: []string{"purge.js"},
			Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `');` + hasDevicesProbe}
	}
	for _, m := range []struct{ name, css string }{{"wide", wideMetrics}, {"spaced", spacedMetrics}} {
		for _, theme := range []string{"dark", "light"} {
			t.Run(fmt.Sprintf("%s_390_%s", m.name, theme), func(t *testing.T) {
				got := e.layout(t, opts(css+m.css, 390, theme))
				if got["attnScroll"] < 0 {
					t.Fatal("no \"Needs attention\" table on the page: this test measures nothing")
				}
				if got["overflow"] > 0.5 {
					t.Errorf("the page overflows sideways by %.0f px", got["overflow"])
				}
				if got["outside"] > 0 {
					t.Errorf("%.0f buttons sit outside the viewport", got["outside"])
				}
				// The chips wrap: nothing of the table is hidden behind a sideways scroll.
				if got["attnScroll"] > 0.5 {
					t.Errorf("the \"Needs attention\" table scrolls sideways by %.0f px inside its region", got["attnScroll"])
				}
			})
		}
	}
	i := strings.Index(css, "/* ===== P20-CI:")
	if i < 0 {
		t.Fatal("the P20-CI block is not where the test expects it")
	}
	without := css[:i]
	// The mutant: without this ticket's block the same wide metrics overflow the phone, and the probe sees it.
	if bad := e.layout(t, opts(without+spacedMetrics, 390, "dark")); bad["overflow"] <= 0.5 {
		t.Errorf("the layout test cannot tell a page that overflows a phone from one that fits: %v", bad)
	}
	// The grid items may shrink, so the page holds even where the chips cannot wrap enough: with the wrap rule
	// alone removed, the table scrolls inside its region and the page still does not overflow.
	noWrap := strings.Replace(css, "#customer-overview table.fleet td.state-cell .st { height: auto; min-height: 24px; white-space: normal; }", "", 1)
	if noWrap == css {
		t.Fatal("the chip wrap rule is not where the test expects it")
	}
	if got := e.layout(t, opts(noWrap+spacedMetrics, 390, "dark")); got["overflow"] > 0.5 || got["attnScroll"] <= 0.5 {
		t.Errorf("without the chip wrap the table should scroll inside its region (%.0f px) and the page not overflow (%.0f px)", got["attnScroll"], got["overflow"])
	}
	// No visual change at desktop width: every box is where it was without the block, with the design's metrics
	// and with the wide ones.
	for _, m := range []string{"", wideMetrics} {
		before := e.layout(t, opts(without+m, 1360, "dark"))
		after := e.layout(t, opts(css+m, 1360, "dark"))
		if before["boxes"] != after["boxes"] || after["overflow"] > 0.5 {
			t.Errorf("at 1360 px the layout moved (box checksum %v without the block, %v with it; overflow %.0f)", before["boxes"], after["boxes"], after["overflow"])
		}
	}
}
