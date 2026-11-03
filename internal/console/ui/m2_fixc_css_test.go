package ui

import (
	"regexp"
	"testing"
)

// Layout fixes of M2-FIX-C cannot be proven without a browser; these tests pin
// the CSS rules that carry each fix so a later edit cannot drop one silently.
// The visual check is in the ticket report (screenshots, or the manual check).

func (e *env) appCSS(t *testing.T) string {
	t.Helper()
	res, css := e.do("GET", "/static/app.css", nil)
	if res.StatusCode != 200 {
		t.Fatalf("app.css: %d", res.StatusCode)
	}
	return css
}

func mustMatchCSS(t *testing.T, name, css, pattern string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(css) {
		t.Errorf("%s: app.css lacks the rule /%s/", name, pattern)
	}
}

// F8: the banner strip takes space in the flow; nothing may scroll underneath it.
func TestFixCBannerStripIsInTheFlow(t *testing.T) {
	css := newEnv(t).appCSS(t)
	mustMatchCSS(t, "F8 in flow", css, `\.banner-strip \{ position: static; \}`)
	if regexp.MustCompile(`\.banner-strip \{[^}]*position: sticky`).MatchString(css) {
		t.Error("F8: the banner strip must not be sticky (content scrolls under translucent banners)")
	}
}

// F2: fields of one form row start on the same line.
func TestFixCFormRowFieldsAlignAtTheTop(t *testing.T) {
	css := newEnv(t).appCSS(t)
	mustMatchCSS(t, "F2 row", css, `\.frow \{[^}]*align-items: start`)
	mustMatchCSS(t, "F2 field", css, `\.frow > \.field \{[^}]*align-content: start`)
}
