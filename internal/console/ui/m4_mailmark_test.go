package ui

import (
	"bytes"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/server"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P20-MD (ADR 0022): the previews of the e-mail messages show the mark from the console's own route.

func TestP20MD_MailMarkRouteServesTheEmbeddedMark(t *testing.T) {
	e := newM4Env(t)
	res, body := e.do("GET", mailMarkPath, nil) // no session: a sandboxed frame sends none
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("GET %s: %d %q %q", mailMarkPath, res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("X-Content-Type-Options"))
	}
	if res.Header.Get("Cross-Origin-Resource-Policy") != "cross-origin" || !strings.HasPrefix(res.Header.Get("Cache-Control"), "public") {
		t.Fatalf("GET %s: CORP %q, Cache-Control %q: a sandboxed frame (opaque origin) needs cross-origin", mailMarkPath,
			res.Header.Get("Cross-Origin-Resource-Policy"), res.Header.Get("Cache-Control"))
	}
	if !bytes.Equal([]byte(body), notify.MarkPNG()) {
		t.Fatal("the route does not serve the embedded mark")
	}
	if res, _ := e.do("POST", mailMarkPath, nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST %s: %d", mailMarkPath, res.StatusCode)
	}
	if !slices.Contains(Patterns(true), patMailMark) || !slices.Contains(Patterns(false), patMailMark) {
		t.Fatal("the mark route is not listed in Patterns")
	}
}

var srcdocRE = regexp.MustCompile(`<iframe [^>]*sandbox=""[^>]*srcdoc="([^"]*)"`)

// frames returns the decoded srcdoc of every sandboxed frame of a page.
func frames(t *testing.T, page string) []string {
	t.Helper()
	var out []string
	for _, m := range srcdocRE.FindAllStringSubmatch(page, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

func checkPreviewFrame(t *testing.T, where, doc string) {
	t.Helper()
	if strings.Count(doc, `src="`+mailMarkPath+`"`) != 1 || strings.Contains(doc, "cid:") {
		t.Errorf("%s: the frame does not show the mark from %s (or still links cid:)", where, mailMarkPath)
	}
	if strings.Contains(doc, "<style") {
		t.Errorf("%s: the frame carries a <style> block the console's CSP refuses", where)
	}
	for _, want := range []string{`bgcolor="#0B1220"`, `CRYO<span`, `<meta name="color-scheme" content="dark">`} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s: the frame lacks %q: not the new look", where, want)
		}
	}
}

func TestP20MD_DigestPreviewShowsTheMarkFromTheRoute(t *testing.T) {
	e := newM4Env(t)
	e.login()
	pv := e.page("/settings/email?preview=1", 200)
	fs := frames(t, pv)
	if len(fs) != 1 {
		t.Fatalf("%d sandboxed frames on the preview page, want 1", len(fs))
	}
	checkPreviewFrame(t, "settings preview", fs[0])
	if strings.Contains(pv, "allow-scripts") || strings.Contains(pv, "allow-same-origin") {
		t.Error("the preview frame is not fully sandboxed")
	}
}

func TestP20MD_DesignMailSamplesShowTheNewLook(t *testing.T) {
	e := newM4Env(t)
	page := e.page("/design/mail", 200)
	fs := frames(t, page)
	if len(fs) != 3 {
		t.Fatalf("%d sandboxed frames on /design/mail, want 3 (digest, alert, recovery)", len(fs))
	}
	for i, f := range fs {
		checkPreviewFrame(t, "design frame "+string(rune('1'+i)), f)
	}
	for _, want := range []string{"Alert: the critical devices of the fixture fleet", "Recovery notice: the same alarms, ended", "new alarm", "resolved at"} {
		if !strings.Contains(page, want) {
			t.Errorf("/design/mail lacks %q", want)
		}
	}
	if strings.Contains(page, "allow-scripts") || strings.Contains(page, "allow-same-origin") {
		t.Error("a sample frame is not fully sandboxed")
	}
}

// FIX BATCH 1 L1 and L5: through the real admin handler chain of the server (request id, access
// log, recover, security headers), the mark keeps its Cross-Origin-Resource-Policy: cross-origin
// while the listener's other headers stay, and the access log names the route (a pattern with a dot
// in its last segment is logged as "invalid").
func TestP20MD_MailMarkThroughTheRealServerChain(t *testing.T) {
	e := newM4Env(t)
	var logs bytes.Buffer
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	srv, err := server.New(cfg, server.Options{RegisterAdmin: e.svc.Register, AgentDisabled: true, Log: logx.New(&logs, logx.LevelInfo)})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", mailMarkPath, nil))
	h := w.Header()
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), notify.MarkPNG()) {
		t.Fatalf("GET %s through the server: %d", mailMarkPath, w.Code)
	}
	if h.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Fatalf("CORP %q after the security-headers middleware, want cross-origin", h.Get("Cross-Origin-Resource-Policy"))
	}
	if h.Get("Content-Security-Policy") != server.ContentSecurityPolicy || h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("the listener's other security headers changed: %v", h)
	}
	// Another response keeps the listener's same-origin.
	w = httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", "/static/app.css", nil))
	if w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("/static CORP %q: the relaxation leaked", w.Header().Get("Cross-Origin-Resource-Policy"))
	}
	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, `"status":200`) && strings.Contains(l, "mail") {
			line = l
		}
	}
	if !strings.Contains(line, `"route":"GET__mail_mark"`) {
		t.Fatalf("the access log does not name the mark route:\n%s", logs.String())
	}
}
