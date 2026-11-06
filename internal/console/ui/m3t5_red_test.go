package ui

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T5 tests that read only the rendered page or the HTTP answer, so each one compiles and
// RUNS against the code before the ticket: that is how red-to-green is shown for them.

// D: critical banners are never behind warnings (components 9.2, ADR 0015). The strip printed three
// critical banners, then every warning, then "and 9 more critical conditions".
func TestM3T5D_CriticalBannersAreNeverBehindWarnings(t *testing.T) {
	e := newEnv(t)
	f := health.Fleet{View: state.FleetView{Banners: fixtures.AllBanners(fixtures.Now())}}
	out, err := e.svc.renderString("board", "banners", buildBanners(f, false))
	if err != nil {
		t.Fatal(err)
	}
	lastCrit, firstWarn := strings.LastIndex(out, "Critical: "), strings.Index(out, "Warning: ")
	if lastCrit < 0 || firstWarn < 0 {
		t.Fatalf("the all-banners fixture must hold both severities: crit at %d, warn at %d", lastCrit, firstWarn)
	}
	if lastCrit > firstWarn {
		t.Fatalf("a critical banner (offset %d) sits behind a warning (offset %d)", lastCrit, firstWarn)
	}
	// the count of the hidden critical conditions is on the page, before any warning
	m := regexp.MustCompile(`and (\d+) more critical conditions`).FindStringSubmatchIndex(out)
	if m == nil || m[0] > firstWarn {
		t.Fatalf("the \"and N more critical conditions\" button is missing or after the warnings: %v", m)
	}
}

// D: on a phone the strip must not fill the screen: at most three critical banners in full and the
// warnings folded into one line (past two), however many conditions are up.
func TestM3T5D_TheStripIsBoundedWithEveryBannerUp(t *testing.T) {
	e := newEnv(t)
	f := health.Fleet{View: state.FleetView{Banners: fixtures.AllBanners(fixtures.Now())}}
	out, _ := e.svc.renderString("board", "banners", buildBanners(f, false))
	// banners outside any <details> are the ones in full
	outside := regexp.MustCompile(`(?s)<details.*?</details>`).ReplaceAllString(out, "")
	if n := strings.Count(outside, `<li class="banner `); n > 3+warnInline {
		t.Fatalf("%d banners are shown in full, at most %d fit a phone", n, 3+warnInline)
	}
	if !strings.Contains(out, "WARNINGS") {
		t.Fatal("past two, the warnings fold into one line that counts them")
	}
	// one or two warnings stay in full, never folded out of sight
	two := health.Fleet{View: state.FleetView{Banners: []state.Banner{
		{ID: "C13", Severity: state.SeverityAlarm, Text: "Server clock may be wrong: device offsets agree."},
		{ID: "C16", Severity: state.SeverityAlarm, Text: "Release manifest rejected."}}}}
	o2, _ := e.svc.renderString("board", "banners", buildBanners(two, false))
	if strings.Contains(o2, "<details") || !strings.Contains(o2, "CLOCK SUSPECT") || !strings.Contains(o2, "RELEASE MANIFEST REJECTED") {
		t.Fatalf("two warnings must show in full: %s", o2)
	}
}

// C: the fixtures session cookies were issued already expired by the browser's clock (the console's
// clock is frozen in the past), so a person could never sign in through the real form.
func TestM3T5C_FixturesSessionCookiesAreNotExpired(t *testing.T) {
	e := newEnv(t)
	e.svc.Fixtures = true
	res, body := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {testPassword}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	check := func(when string, cookies []*http.Cookie, atLeast time.Duration) {
		seen := map[string]bool{}
		for _, c := range cookies {
			if c.Name != auth.SessionCookie && c.Name != auth.CSRFCookie {
				continue
			}
			seen[c.Name] = true
			if !c.Expires.After(time.Now().Add(atLeast)) {
				t.Errorf("%s: cookie %s expires %v, already gone by the browser's clock %v", when, c.Name, c.Expires, time.Now())
			}
		}
		if len(seen) != 2 {
			t.Errorf("%s: want both session cookies, got %v", when, seen)
		}
	}
	check("password step", res.Cookies(), 5*time.Minute) // the pending session lasts ten minutes
	e.login()                                            // the password step again, then the MFA step that issues the full session
	check("after MFA", e.sess, time.Hour)
}

// F: an agent version label is printed only through safeVersion: a file name sent as a label shows as
// "(withheld)" on every page that prints it, with the hostile fixture of M3-T4.
func TestM3T5F_AHostileAgentVersionNeverReachesAPage(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE devices SET agent_version = ? WHERE id = ?`, hostileTexts[4], id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE device_presence SET agent_version = ? WHERE device_id = ?`, hostileTexts[4], id) // what a heartbeat stores
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"board": "/", "devices": "/devices?size=100", "device summary": "/devices/" + id, "device settings": "/devices/" + id + "/settings",
		"customer": "/customers/" + e.customerID("Summit Eye Care")} {
		body := e.page(path, 200)
		if strings.Contains(body, hostileTexts[4]) {
			t.Errorf("%s prints the raw agent version label", name)
		}
	}
	for name, path := range map[string]string{"board": "/", "device summary": "/devices/" + id, "devices": "/devices?size=100"} {
		if !strings.Contains(e.page(path, 200), withheld) {
			t.Errorf("%s: the hostile label should read %q", name, withheld)
		}
	}
}

// G: the sidebar's Gateway line is read from the same evaluation as the page. It said "not probed"
// on every page, beside a Gateway "ok" row on System.
func TestM3T5G_TheSidebarGatewayLineIsNotAConstant(t *testing.T) {
	e := newEnv(t)
	e.login()
	clock := func() time.Time { return e.now }
	e.svc.Fleet.Probes = health.FixtureProbes(clock)
	for _, path := range []string{"/", "/system", "/runs"} {
		body := e.page(path, 200)
		if strings.Contains(body, "not probed") && strings.Contains(body, "Gateway: <b>not probed</b>") {
			t.Errorf("%s: the sidebar still prints a hardcoded %q", path, "Gateway: not probed")
		}
		if !strings.Contains(body, "Gateway: <b>ok</b>") {
			t.Errorf("%s: with a fresh passing gateway probe the sidebar must say ok", path)
		}
	}
}
