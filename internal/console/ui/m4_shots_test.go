package ui

import (
	"context"
	"html"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// P20-W3B: screenshots of the new pages for the operator's review, light and dark, at 390 px and
// at desktop width. Not a gate: it runs only when CRYO_SHOTS names a directory to write into, and
// writes PNG files there. The page is the real markup of fixtures mode, with the real fonts.

func (e *env) screenshot(t *testing.T, path, theme string, w, h int, out string, extraJS string) {
	t.Helper()
	chrome := chromeOrSkip(t)
	dir := t.TempDir()
	for _, d := range []string{"static", filepath.Join("static", "js"), filepath.Join("static", "fonts", "ibmplexsans"), filepath.Join("static", "fonts", "martianmono"), filepath.Join("static", "fonts", "orbitron")} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"app.css", "tokens.css", "fonts.css", "js/theme.js", "fonts/ibmplexsans/IBMPlexSans-var.woff2", "fonts/martianmono/MartianMono-var.woff2", "fonts/orbitron/Orbitron-var.woff2"} {
		_, body := e.do("GET", "/static/"+f, nil)
		body = regexp.MustCompile(`/static/(fonts/[^)?"]+)(\?v=[0-9a-f]+)?`).ReplaceAllString(body, "$1")
		if err := os.WriteFile(filepath.Join(dir, "static", filepath.FromSlash(f)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	page := e.page(path, 200)
	page = scriptTagRE.ReplaceAllStringFunc(page, func(tag string) string {
		if m := scriptSrcRE.FindStringSubmatch(tag); m != nil && m[1] == "theme.js" {
			return `<script src="static/js/theme.js"></script>`
		}
		return ""
	})
	page = regexp.MustCompile(`(href|src)="/static/([^"?]+)(?:\?[^"]*)?"`).ReplaceAllString(page, `$1="static/$2"`)
	page = strings.Replace(page, "</body>", `<script>document.documentElement.setAttribute('data-theme','`+theme+`');`+extraJS+`</script></body>`, 1)
	wrapper := `<!doctype html><html><body style="margin:0;background:#000"><iframe style="border:0;display:block;width:` + strconv.Itoa(w) + `px;height:` + strconv.Itoa(h) +
		`px" srcdoc="` + html.EscapeString(page) + `"></iframe></body></html>`
	file := filepath.Join(dir, "page.html")
	if err := os.WriteFile(file, []byte(wrapper), 0o644); err != nil {
		t.Fatal(err)
	}
	ww := w
	if ww < 600 {
		ww = 600
	}
	raw := filepath.Join(dir, "shot.png")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars", "--window-size="+strconv.Itoa(ww)+","+strconv.Itoa(h),
		"--user-data-dir="+filepath.Join(dir, "profile"), "--virtual-time-budget=6000", "--screenshot="+raw, "file:///"+filepath.ToSlash(file)).Run(); err != nil {
		t.Skipf("the browser would not take a screenshot here (%v)", err)
	}
	f, err := os.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	crop := image.NewRGBA(image.Rect(0, 0, w, img.Bounds().Dy()))
	draw.Draw(crop, crop.Bounds(), img, image.Point{}, draw.Src)
	o, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err := png.Encode(o, crop); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}

func TestP20W3B_Screenshots(t *testing.T) {
	dir := os.Getenv("CRYO_SHOTS")
	if dir == "" {
		t.Skip("CRYO_SHOTS names no directory: the screenshots are for review, not a gate")
	}
	e := newM4Env(t)
	e.login()
	shots := []struct {
		name, path, js string
		h              int
	}{
		{"settings_email", "/settings/email", "", 0},
		{"settings_email_preview", "/settings/email?preview=1", "", 0},
		{"device_acked", "/devices/" + e.dev["FRONTDESK-01"], "", 0},
		{"device_ack_open", "/devices/" + e.dev["RECEPTION-2"], `document.querySelector('details.ackmenu').open=true;`, 0},
		{"customer_missing", "/customers/" + e.customerID("Blue Ridge Accounting"), "", 0},
	}
	for _, s := range shots {
		for _, theme := range []string{"light", "dark"} {
			for _, w := range []int{390, 1360} {
				h := 3600
				if w > 1000 {
					h = 2400
				}
				e.screenshot(t, s.path, theme, w, h, filepath.Join(dir, s.name+"_"+theme+"_"+strconv.Itoa(w)+".png"), s.js)
			}
		}
	}
}
