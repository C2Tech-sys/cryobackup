package ui

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
)

// TestBusyNoticeIsPlainWords (M1-HIPAA2 follow-up 3 and last batch item 4):
// when every restore stream is in use the admin API sends the browser back to
// the restore page with ?notice=busy&at=<unix>, and the page says so in plain
// words while still offering the download. The live partial keeps the notice
// on refresh (htmx names the page in HX-Current-URL) until a minute has
// passed; an unknown notice, a missing or stale at, or another page's URL
// shows nothing.
func TestBusyNoticeIsPlainWords(t *testing.T) {
	e := newEnv(t)
	e.login()
	res, _ := e.post("/ui/devices/"+e.dev["DR-OFFICE-PC"]+"/restore", url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "action": {"submit"}, "path": {"/C/Users/Public/Desktop/Welcome.txt"}}, false)
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, "/restores/") {
		t.Fatalf("create: %q", loc)
	}
	id := strings.TrimPrefix(loc, "/restores/")
	const msg = "Other restores are already downloading. Try again in a moment."
	page := loc + "?notice=busy&at=" + strconv.FormatInt(e.now.Unix(), 10)

	_, body := e.do("GET", page, nil)
	mustContain(t, "busy notice", body, msg, "Download zip")
	if strings.Contains(body, "temporarily unavailable") {
		t.Fatal("the busy notice must not be the generic unavailable text")
	}

	// The live partial, refreshed from that page, keeps it...
	partial := "/ui/partials/restore/" + id
	_, body = e.do("GET", partial, nil, "HX-Request", "true", "HX-Current-URL", "http://console.example"+page)
	mustContain(t, "busy notice in the live partial", body, msg)
	// ...until a minute has passed.
	e.now = e.now.Add(busyNoticeTTL + time.Second)
	if _, body = e.do("GET", partial, nil, "HX-Request", "true", "HX-Current-URL", "http://console.example"+page); strings.Contains(body, msg) {
		t.Fatal("the busy notice outlived its minute")
	}
	e.login() // the session aged with the clock
	at := strconv.FormatInt(e.now.Unix(), 10)
	for _, other := range []string{
		loc + "?notice=%3Cb%3Ex&at=" + at,                           // unknown notice
		loc + "?notice=busy",                                        // no at
		loc + "?notice=busy&at=not-a-number",                        // bad at
		"/restores/01J9ZZ0000000000000000ORPH?notice=busy&at=" + at, // another restore's page
	} {
		_, body = e.do("GET", partial, nil, "HX-Request", "true", "HX-Current-URL", "http://console.example"+other)
		if strings.Contains(body, msg) || strings.Contains(body, "<b>x") {
			t.Errorf("%s: shows the busy notice", other)
		}
	}
	if _, body = e.do("GET", partial, nil); strings.Contains(body, msg) {
		t.Error("a partial with no page URL shows the busy notice")
	}
}
