package ui

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// The login wordmark flashed in the fallback face on a revalidating reload: the
// font files were referenced from the static fonts.css without a content hash,
// so they sat on the one-hour cache and arrived three hops after the HTML.
// Now fonts.css is served with every font URL versioned, the pages preload the
// display face at the same URL, and the display face uses font-display: block.
// ADR 0023 (set D): eight files; the display face's latin subset is the one preloaded.
func TestFontsAreVersionedAndPreloaded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, login := e.do("GET", "/login", nil)
	cssURL := regexp.MustCompile(`/static/fonts\.css\?v=[0-9a-f]{8}`).FindString(login)
	if cssURL == "" {
		t.Fatal("fonts.css is linked without a content hash")
	}
	res, err := http.Get(e.srv.URL + cssURL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	css := string(b)
	if !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("fonts.css cache: %q", res.Header.Get("Cache-Control"))
	}
	fontURLs := regexp.MustCompile(`url\('(/static/fonts/[^']+)'\)`).FindAllStringSubmatch(css, -1)
	// Josefin Sans and Open Sans: latin and latin-ext each; JetBrains Mono: 400, 500, 600, 700.
	if len(fontURLs) != 8 {
		t.Fatalf("want 8 font urls in fonts.css, got %d", len(fontURLs))
	}
	var display string
	for _, m := range fontURLs {
		u := m[1]
		if !regexp.MustCompile(`\.woff2\?v=[0-9a-f]{8}$`).MatchString(u) {
			t.Errorf("font url not versioned: %s", u)
			continue
		}
		res, err := http.Get(e.srv.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %q", u, res.StatusCode, res.Header.Get("Cache-Control"))
		}
		if strings.Contains(u, "/JosefinSans-latin-var.woff2?") {
			display = u
		}
	}
	if display == "" {
		t.Fatal("no Josefin Sans latin url in fonts.css")
	}
	// The preload names exactly the URL the stylesheet will request, with crossorigin
	// (a font preload without it is fetched twice), on the bare and the shell layout.
	want := `<link rel="preload" href="` + display + `" as="font" type="font/woff2" crossorigin>`
	if !strings.Contains(login, want) {
		t.Errorf("login page: preload missing or at another url\nwant %s", want)
	}
	e.loginAs("admin", testPassword)
	_, board := e.do("GET", "/", nil)
	if !strings.Contains(board, want) {
		t.Errorf("shell: preload missing or at another url")
	}
	// The display face blocks (a wordmark in the fallback face is a different logo), in both its
	// subsets; the text faces swap.
	faces := regexp.MustCompile(`(?s)font-family: 'Josefin Sans';.*?\}`).FindAllString(css, -1)
	if len(faces) != 2 {
		t.Fatalf("want the two Josefin Sans @font-face rules, got %d", len(faces))
	}
	for _, face := range faces {
		if !strings.Contains(face, "font-display: block;") {
			t.Errorf("Josefin Sans @font-face: want font-display: block, got\n%s", face)
		}
	}
	if strings.Count(css, "font-display: swap;") != 6 {
		t.Errorf("the six text faces (Open Sans x2, JetBrains Mono x4) must keep font-display: swap")
	}
}
