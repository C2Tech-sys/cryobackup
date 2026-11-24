package ui

import (
	"math"
	"strings"
	"testing"
)

// P21-T3 (ADR 0023), measured with the house fonts (set D):
//   - the display roles' capitals sit centred in their boxes (Josefin Sans's capitals sit above the
//     middle of a line-height: 1 box; the roles trade line height for top padding);
//   - the big numbers do not jitter: the role is the body face with tabular figures, so "111" and
//     "000" are equally wide, which the display face's digits are not;
//   - no element draws the data face's contextual ligatures.

const p21t3Probe = `
function vc(el) { // capitals' centre minus the box's centre, px (negative: the capitals sit high)
  var cs = getComputedStyle(el), ctx = document.createElement('canvas').getContext('2d');
  ctx.font = cs.fontWeight + ' ' + cs.fontSize + ' ' + cs.fontFamily;
  var capH = ctx.measureText('H').actualBoundingBoxAscent;
  var p = document.createElement('span'); p.style.cssText = 'display:inline-block;width:0;height:0;vertical-align:baseline';
  el.appendChild(p); var base = p.getBoundingClientRect().bottom; p.remove();
  var r = el.getBoundingClientRect();
  return (base - capH / 2) - (r.top + r.bottom) / 2;
}
function width(cls, text) { var s = document.createElement('span'); s.className = cls; s.textContent = text; s.style.position = 'absolute';
  document.body.appendChild(s); var w = s.getBoundingClientRect().width; s.remove(); return w; }
var out = {iw: innerWidth, display: document.fonts.check('15px "Josefin Sans"') ? 1 : 0, roles: 0, worst: 0, lig: 0, mono: 0};
document.querySelectorAll('.t-hero-wordmark, .t-wordmark, .t-page-title').forEach(function (el) {
  if (!el.getClientRects().length) return; out.roles++; out.worst = Math.max(out.worst, Math.abs(vc(el))); });
out.num_diff = Math.abs(width('t-big-number', '111111') - width('t-big-number', '000000'));
out.disp_diff = Math.abs(width('t-wordmark', '111111') - width('t-wordmark', '000000'));
out.num_sans = getComputedStyle(document.body.appendChild(Object.assign(document.createElement('span'), {className: 't-big-number'}))).fontFamily.indexOf('Open Sans') === 1 ? 1 : 0;
document.querySelectorAll('body *').forEach(function (el) { var cs = getComputedStyle(el);
  if (cs.fontFamily.indexOf('JetBrains Mono') !== 1) return; out.mono++; if (cs.fontVariantLigatures !== 'no-contextual') out.lig++; });
return out;`

func TestP21T3_DisplayCapsCentredBigNumbersTabularNoLigatures(t *testing.T) {
	t.Parallel()
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	out := newEnv(t) // signed out: the sign-in page and its hero wordmark
	base, fonts := e.appCSS(t), e.houseFontsCSS(t)
	for _, c := range []struct {
		name string
		e    *env
		path string
		w    int
	}{{"fleet", e, "/", 1280}, {"fleet", e, "/", 390}, {"design", e, "/design", 1280}, {"login", out, "/login", 1280}} {
		got := c.e.layout(t, layoutOpts{Path: c.path, CSS: base + fonts, W: c.w, H: 900, Probe: p21t3Probe})
		if got["display"] != 1 {
			t.Fatalf("%s %d: the display face did not load: the check would measure a fallback", c.name, c.w)
		}
		if got["roles"] < 1 || got["worst"] > 1 {
			t.Errorf("%s %d px: a display role's capitals sit %.2f px off its box's centre (%v roles measured)", c.name, c.w, got["worst"], got["roles"])
		}
		if got["num_sans"] != 1 || got["num_diff"] > 0.1 {
			t.Errorf("%s %d px: big numbers in the body face %v, 111111 and 000000 differ by %.2f px: digits would jitter", c.name, c.w, got["num_sans"], got["num_diff"])
		}
		if got["disp_diff"] < 5 {
			t.Errorf("%s %d px: the display face's 111111 and 000000 differ by only %.2f px: the tabular check cannot tell", c.name, c.w, got["disp_diff"])
		}
		if got["mono"] < 1 || got["lig"] != 0 {
			t.Errorf("%s %d px: %v of %v data-face elements allow contextual ligatures", c.name, c.w, got["lig"], got["mono"])
		}
	}
	// Mutants: without the role padding the capitals sit high again; without the rule the shorthands
	// turn the ligatures back on.
	unpadded := base + fonts + ` .t-hero-wordmark, .t-wordmark, .t-page-title { padding-top: 0 !important; }`
	if got := e.layout(t, layoutOpts{Path: "/", CSS: unpadded, W: 1280, H: 900, Probe: p21t3Probe}); got["worst"] <= 1 {
		t.Errorf("without the padding the capitals still measure centred (%.2f px): the check cannot fail", got["worst"])
	}
	const rule = "*, *::before, *::after { font-variant-ligatures: no-contextual !important; }"
	if !strings.Contains(base, rule) {
		t.Fatal("the ligature rule is not where the test expects it")
	}
	if got := e.layout(t, layoutOpts{Path: "/", CSS: strings.Replace(base, rule, "", 1) + fonts, W: 1280, H: 900, Probe: p21t3Probe}); got["lig"] == 0 || math.IsNaN(got["lig"]) {
		t.Errorf("without the rule no element allows ligatures: the check cannot fail")
	}
}
