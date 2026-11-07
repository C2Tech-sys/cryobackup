package ui

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-HARDEN B: a pause "until" in the hour the clocks skip has no instant. It was stored an hour
// early with no word; it is refused, and nothing changes.
func TestPauseUntilInTheSkippedHourIsRefused(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	r := e.postForm("/ui/devices/"+id+"/pause", url.Values{"when": {"until"}, "until": {"2027-03-14T02:30"}, "reason": {"patching"}}) // America/Chicago, spring forward
	if !strings.Contains(e.location(r), "cmd=err_pause_gap") {
		t.Fatalf("went to %q", e.location(r))
	}
	d, err := e.st.GetDevice(context.Background(), store.ScopeAll(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.PausedUntil != nil {
		t.Fatalf("parked until %v", d.PausedUntil)
	}
	mustContain(t, "the notice", e.page(e.location(r), 200), "clocks skip it")

	// The fall-back hour exists, twice: it is accepted.
	r = e.postForm("/ui/devices/"+id+"/pause", url.Values{"when": {"until"}, "until": {"2026-11-07T14:00"}, "reason": {"patching"}})
	if strings.Contains(e.location(r), "err_pause_gap") {
		t.Fatalf("an ordinary time was refused: %q", e.location(r))
	}
}

// M3-HARDEN B: the account page offers the narrow scopes and names them in the table.
func TestAccountPageOffersAndNamesTheNarrowScopes(t *testing.T) {
	e := newEnv(t)
	e.login()
	page := e.page("/account", 200)
	mustContain(t, "scope options", page, `value="status:read"`, `value="metrics:read"`)
	if res, body := e.post("/ui/account/tokens", url.Values{"name": {"uptime"}, "scope": {"status:read"}, "days": {"7"}}, false); res.StatusCode >= 400 {
		t.Fatalf("token create answered %d: %s", res.StatusCode, body)
	}
	mustContain(t, "token row", e.page("/account", 200), "Status endpoint only")
}

// M3-HARDEN B: S36 at both widths: nothing runs off the page, and Run now stays in the viewport.
func TestSystemHealthLayoutAtBothWidths(t *testing.T) {
	chromeOrSkip(t)
	e := newEnv(t)
	e.login()
	css := e.appCSS(t)
	for _, w := range []int{1360, 390} {
		t.Run(fmt.Sprintf("%d", w), func(t *testing.T) {
			got := e.layout(t, layoutOpts{Path: "/system", CSS: css, W: w, H: 900, Probe: layoutProbe})
			if got["overflow"] > 0.5 {
				t.Errorf("the page overflows sideways by %.0f px", got["overflow"])
			}
			if got["outside"] > 0 {
				t.Errorf("%.0f action buttons sit outside the viewport", got["outside"])
			}
		})
	}
}
