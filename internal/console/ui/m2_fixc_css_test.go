package ui

import "testing"

// M3-T5 (C): the M2-FIX-C tests of F2 (form-row alignment) and F8 (the banner strip in the flow) used to
// match the text of app.css. They are rendered-layout tests now: m3t5_layout_test.go lays the real page
// out in a browser and measures it.

// appCSS is the stylesheet the console serves.
func (e *env) appCSS(t *testing.T) string {
	t.Helper()
	res, css := e.do("GET", "/static/app.css", nil)
	if res.StatusCode != 200 {
		t.Fatalf("app.css: %d", res.StatusCode)
	}
	return css
}
