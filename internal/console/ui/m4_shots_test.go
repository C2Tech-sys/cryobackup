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
	crop := e.shot(t, path, theme, w, h, extraJS, nil)
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

// shot captures the real markup of path, w px wide, in theme, after mutate (nil: as served).
//
// F7 (P20-W3B): Chrome runs the digest preview frame (sandbox="", an opaque origin) as an
// out-of-process frame under site isolation, and the headless --screenshot capture does not
// reliably composite such a frame: the preview came out blank in the review captures, and a page
// holding only that frame captured blank every time, while the same frame without the sandbox, or
// the same capture with --disable-site-isolation-trials, showed the digest. The flag changes only
// how this test browser lays out its processes; the page and its sandbox are exactly as served.
func (e *env) shot(t *testing.T, path, theme string, w, h int, extraJS string, mutate func(string) string) *image.RGBA {
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
	if mutate != nil {
		page = mutate(page)
	}
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
	args := append([]string{"--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars", "--disable-site-isolation-trials", "--window-size=" + strconv.Itoa(ww) + "," + strconv.Itoa(h),
		"--user-data-dir=" + filepath.Join(dir, "profile"), "--virtual-time-budget=6000", "--screenshot=" + raw}, quietBrowserFlags...)
	if err := exec.CommandContext(ctx, chrome, append(args, "file:///"+filepath.ToSlash(file))...).Run(); err != nil {
		browserWouldNotRun(t, err) // P21-T2 F1: a found browser that fails is a failure, not a skip
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
	return crop
}

// F7: the digest preview frame paints the digest, in both themes of the page. The same capture with
// the frame's document emptied is the control: the two must differ over a large area. (The frame's
// DOM cannot be read from the page: its origin is opaque by design, so pixels are the evidence.)
func TestP20W3B_F7_ThePreviewFramePaintsTheDigest(t *testing.T) {
	t.Parallel()
	chromeOrSkip(t)
	e := newM4Env(t)
	e.login()
	empty := func(p string) string { return regexp.MustCompile(`srcdoc="[^"]*"`).ReplaceAllString(p, `srcdoc=""`) }
	for _, theme := range []string{"light", "dark"} {
		full := e.shot(t, "/settings/email?preview=1", theme, 1360, 2400, "", nil)
		blank := e.shot(t, "/settings/email?preview=1", theme, 1360, 2400, "", empty)
		if full.Bounds() != blank.Bounds() {
			t.Fatalf("%s: the two captures differ in size", theme)
		}
		diff := 0
		for y := full.Bounds().Min.Y; y < full.Bounds().Max.Y; y++ {
			for x := full.Bounds().Min.X; x < full.Bounds().Max.X; x++ {
				if full.RGBAAt(x, y) != blank.RGBAAt(x, y) {
					diff++
				}
			}
		}
		if diff < 50000 {
			t.Errorf("%s: the preview frame painted %d pixels of the digest: it is blank", theme, diff)
		}
		t.Logf("%s: %d pixels differ between the preview and an empty frame", theme, diff)
	}
}

func TestP20W3B_Screenshots(t *testing.T) {
	t.Parallel()
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
		{"settings_general", "/settings", "", 0},
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
