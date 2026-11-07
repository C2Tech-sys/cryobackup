package ui

import (
	"fmt"
	"testing"
)

// wideMetrics stands in for a browser whose fonts run wider than the ones this was
// designed with: Linux Chrome falls back to DejaVu Sans, which is about as wide as
// Verdana. The letter spacing makes the fault reproduce on any machine, whatever fonts
// it has, and with fonts.css left out of the rig the fallback is what gets used anyway.
const wideMetrics = `
html, body, body * { font-family: Verdana, "DejaVu Sans", sans-serif !important; letter-spacing: .05em !important; }`

// M3-HARDEN (CI red on ubuntu-latest, table_1360_*): the token table's actions stay
// reachable at 1360 whatever the font metrics. The table scrolls inside its own box,
// so the page did not overflow, but the action buttons sat past the right edge.
func TestTokenTableActionsStayInViewWithWideFontMetrics(t *testing.T) {
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	summit := e.customerID("Summit Eye Care")
	e.t6Token(summit, "a revoked token with quite a long label for the phone", true)
	e.t6Token(summit, "an active token with quite a long label as well", false)
	css := e.appCSS(t) + wideMetrics
	for _, w := range []int{1360, 1100} {
		for _, theme := range []string{"dark", "light"} {
			t.Run(fmt.Sprintf("%d_%s", w, theme), func(t *testing.T) {
				got := e.layout(t, layoutOpts{Path: "/enroll", CSS: css, W: w, H: 900,
					Probe: `document.documentElement.setAttribute('data-theme', '` + theme + `');` + layoutProbe})
				if got["buttons"] == 0 {
					t.Fatal("no action button measured")
				}
				if got["overflow"] > 0.5 {
					t.Errorf("the page overflows sideways by %.0f px", got["overflow"])
				}
				if got["outside"] > 0 {
					t.Errorf("%.0f action buttons sit outside the viewport", got["outside"])
				}
			})
		}
	}
}
