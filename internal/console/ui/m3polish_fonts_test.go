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
// display face at the same URL, and Orbitron uses font-display: block.
func TestFontsAreVersionedAndPreloaded(t *testing.T) {
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
	if len(fontURLs) != 3 {
		t.Fatalf("want 3 font urls in fonts.css, got %d", len(fontURLs))
	}
	var orbitron string
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
		if strings.Contains(u, "Orbitron") {
			orbitron = u
		}
	}
	if orbitron == "" {
		t.Fatal("no Orbitron url in fonts.css")
	}
	// The preload names exactly the URL the stylesheet will request, with crossorigin
	// (a font preload without it is fetched twice), on the bare and the shell layout.
	want := `<link rel="preload" href="` + orbitron + `" as="font" type="font/woff2" crossorigin>`
	if !strings.Contains(login, want) {
		t.Errorf("login page: preload missing or at another url\nwant %s", want)
	}
	e.loginAs("admin", testPassword)
	_, board := e.do("GET", "/", nil)
	if !strings.Contains(board, want) {
		t.Errorf("shell: preload missing or at another url")
	}
	// Orbitron blocks (a wordmark in the fallback face is a different logo); the text faces swap.
	face := regexp.MustCompile(`(?s)font-family: 'Orbitron';.*?\}`).FindString(css)
	if !strings.Contains(face, "font-display: block;") {
		t.Errorf("Orbitron @font-face: want font-display: block, got\n%s", face)
	}
	if strings.Count(css, "font-display: swap;") != 2 {
		t.Errorf("the two text faces must keep font-display: swap")
	}
}
