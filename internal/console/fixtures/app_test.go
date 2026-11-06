package fixtures_test

import (
	"context"
	"encoding/base32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
)

// M3-T4 delta, item 2: the real fixtures-mode App (cli.Build, the same one `cryo-console
// serve --fixtures` runs), driven over HTTP, shows every state the monitoring pages
// can show: every run outcome, a customer-wide silence, every console banner on
// request, a job table whose Run now can be pressed, and the MISSING customer.

type client struct {
	t    *testing.T
	base string
	cs   []*http.Cookie
	csrf string
}

func (c *client) do(method, path string, form url.Values) (*http.Response, string) {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", c.base)
	}
	for _, ck := range c.cs {
		req.AddCookie(ck)
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	for _, ck := range res.Cookies() {
		kept := c.cs[:0]
		for _, o := range c.cs {
			if o.Name != ck.Name {
				kept = append(kept, o)
			}
		}
		c.cs = append(kept, ck)
		if ck.Name == auth.CSRFCookie {
			c.csrf = ck.Value
		}
	}
	return res, string(b)
}

func (c *client) page(path string) string {
	c.t.Helper()
	res, body := c.do("GET", path, nil)
	if res.StatusCode != 200 {
		c.t.Fatalf("GET %s: %d %.200s", path, res.StatusCode, body)
	}
	return body
}

// login signs in as the fixtures admin: password, then the forced TOTP enrollment.
func (c *client) login(pw string) {
	c.t.Helper()
	res, body := c.do("POST", "/login", url.Values{"username": {"admin"}, "password": {pw}})
	if res.StatusCode != http.StatusSeeOther {
		c.t.Fatalf("login: %d %.200s", res.StatusCode, body)
	}
	_, body = c.do("GET", "/login/mfa", nil)
	m := regexp.MustCompile(`<pre class="code secret"[^>]*>([A-Z2-7 ]+)</pre>`).FindStringSubmatch(body)
	if m == nil {
		c.t.Fatalf("no enrollment key on the MFA page: %.200s", body)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(m[1], " ", ""))
	if err != nil {
		c.t.Fatal(err)
	}
	res, body = c.do("POST", "/login/mfa/enroll", url.Values{"code": {auth.TOTPCode(secret, time.Now())}, "csrf_token": {c.csrf}})
	if res.StatusCode != 200 || !strings.Contains(body, "Save your recovery codes") {
		c.t.Fatalf("enroll: %d %.200s", res.StatusCode, body)
	}
}

func TestTheRealFixturesAppShowsEveryM3State(t *testing.T) {
	t.Cleanup(func() { fixtures.SetAllBanners(false) })
	ctx := context.Background()
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	app, err := cli.Build(ctx, cfg, cli.BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	srv := httptest.NewServer(app.Server.AdminRouter)
	t.Cleanup(srv.Close)
	c := &client{t: t, base: srv.URL}
	c.login("fixtures-pass-1")

	// every run outcome
	runs := c.page("/runs?size=200")
	for _, label := range []string{"SUCCESS", "PARTIAL", "FAILED", "CANCELLED", "SKIPPED", "DEFERRED", "INTERRUPTED", "ABANDONED", "RUNNING"} {
		if !strings.Contains(runs, `<i aria-hidden="true"></i>`+label+`</span>`) {
			t.Errorf("fixtures mode has no %s run", label)
		}
	}
	// a customer whose devices went silent together: one row, and its link lists exactly the devices it counts
	un := c.page("/fleet/unseen")
	count := regexp.MustCompile(`Show the (\d+) devices`).FindStringSubmatch(un)
	if !strings.Contains(un, "CUSTOMER-WIDE SILENCE") || !strings.Contains(un, "Harbor Family Practice") || count == nil || count[1] < "5" {
		t.Fatalf("no silent customer on the not-seen page")
	}
	href := regexp.MustCompile(`href="(/fleet/unseen\?show=[^"#]+)#silence-`).FindStringSubmatch(un)
	if href == nil {
		t.Fatal("no link that expands the silence row")
	}
	if n := strings.Count(c.page(strings.ReplaceAll(href[1], "&amp;", "&")), `class="dev silence-dev"`); strconv.Itoa(n) != count[1] {
		t.Errorf("the silence row says %s devices and lists %d", count[1], n)
	}
	// the MISSING customer, and the grid says so
	act := c.page("/activity")
	if !strings.Contains(act, "MISSING: no devices") || !strings.Contains(act, "Blue Ridge Accounting") {
		t.Error("the MISSING customer is not an alarm on the activity grid")
	}
	// the job table: fictional jobs, one that failed, one that never ran, Run now works with one audit row
	sys := c.page("/system")
	for _, name := range fixtures.JobNames {
		if !strings.Contains(sys, `id="job-`+name+`"`) {
			t.Errorf("job %s is not in the table", name)
		}
	}
	if !strings.Contains(sys, "NEVER RUN") || !strings.Contains(sys, ">ERROR<") || strings.Contains(sys, "the job runner is not running") {
		t.Error("the job table does not show a failed job, a job that never ran, and a running runner")
	}
	form := regexp.MustCompile(`action="(/ui/system/jobs/alarm-evaluator/run)" class="stack"><input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(sys)
	if form == nil {
		t.Fatal("Run now is not offered in fixtures mode")
	}
	res, _ := c.do("POST", form[1], url.Values{"csrf_token": {form[2]}})
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/system?flash=") {
		t.Fatalf("Run now: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// the health rows: no row is OK on evidence the fixtures console does not have
	row := func(body, id string) string {
		m := regexp.MustCompile(`(?s)<tr id="comp-` + id + `" class="comp">.*?<i aria-hidden="true"></i>([A-Z ]+)</span>`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no health row %s", id)
		}
		return m[1]
	}
	for _, id := range []string{"release_manifest", "certificates"} {
		if got := row(sys, id); got == "OK" {
			t.Errorf("fixtures mode reads %s OK with no evidence", id)
		}
	}
	// every banner, on request, on every page
	if strings.Contains(sys, "RELEASE MANIFEST NOT CHECKED") && !fixtures.AllBannersOn() {
		t.Log("a not-checked banner is raised by the fixtures fleet itself")
	}
	c.page("/fixtures?banners=all")
	all := c.page("/system")
	for _, b := range fixtures.AllBanners(fixtures.Now()) {
		if label := bannerLabel(b.ID); !strings.Contains(all, label) {
			t.Errorf("banner %s (%s) is not shown with every banner on", b.ID, label)
		}
	}
	if !strings.Contains(c.page("/runs"), "GATEWAY NOT CHECKED") {
		t.Error("the banners are not on the runs page")
	}
}

// bannerLabel is the strip's label of a banner id (ui.BannerLabel, spelled out so this
// test does not import the UI).
func bannerLabel(id string) string {
	labels := map[string]string{"C00": "SELF-CHECKS NOT WIRED", "C01": "GATEWAY UNREACHABLE", "C02": "STORAGE PROBE FAILED", "C03": "EMAIL NOT DELIVERING", "C04": "AGENT API UNREACHABLE",
		"C05": "CERTIFICATE EXPIRING", "C06": "ALERTING STALLED", "C07": "MAINTENANCE WORKER DOWN", "C08": "NO ACTIVE DEVICES", "C09": "PARTIAL DATA", "C10": "LIVE UPDATES DISCONNECTED",
		"C11": "DATABASE NEEDS ATTENTION", "C12": "MASTER KEY UNAVAILABLE", "C13": "CLOCK SUSPECT", "C14": "DIGEST NOT SENT", "C15": "STORAGE QUOTA", "C16": "RELEASE MANIFEST REJECTED",
		"C01b": "GATEWAY NOT CHECKED", "C02b": "STORAGE NOT CHECKED", "C03b": "EMAIL NOT CHECKED", "C04b": "AGENT API NOT CHECKED", "C05b": "CERTIFICATE NOT CHECKED", "C06b": "ALERTING NOT CHECKED",
		"C07b": "MAINTENANCE WORKER NOT CHECKED", "C12b": "MASTER KEY NOT CHECKED", "C14b": "DIGEST NOT CHECKED", "C15b": "STORAGE QUOTA NOT CHECKED", "C16b": "RELEASE MANIFEST NOT CHECKED"}
	return labels[id]
}
