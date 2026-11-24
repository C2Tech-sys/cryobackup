package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P21-T6: the link in e-mails is admin_url (the address an operator types to open the console),
// never public_url (the agent API, which serves no console page). Unset: no link and no button.
// Two different hosts are used throughout so that a link built from the wrong key shows.

const (
	t6AdminHost  = "console.example.net"
	t6AgentHost  = "agents.example.test"
	t6AdminURL   = "https://" + t6AdminHost
	t6AdminLink  = t6AdminURL + "/"
	t6PublicURL  = "https://" + t6AgentHost + ":8443"
	t6SMTPFrom   = "console@example.test"
	t6RelayHost  = "relay.example.test"
	t6NoLinkWord = "Open the console:"
)

// t6Serve builds a serve-mode App (a SMTP relay configured, no message ever sent) with the given
// admin_url and public_url, and returns it with its log.
func t6Serve(t *testing.T, adminURL, publicURL string, smtp bool) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen = true, "127.0.0.1:0", "127.0.0.1:0"
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	cfg.PublicURL, cfg.AdminURL = publicURL, adminURL
	if smtp {
		cfg.SMTP = config.SMTP{Host: t6RelayHost, Port: 587, Security: "starttls", From: t6SMTPFrom}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, logs.String()
}

// t6Fixtures builds a fixtures-mode App (the FakeSender is the relay) with the given keys.
func t6Fixtures(t *testing.T, adminURL, publicURL string) *App {
	t.Helper()
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	cfg.PublicURL, cfg.AdminURL = publicURL, adminURL
	app, err := Build(context.Background(), cfg, BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

// mustLinkTo / mustNotLink check one rendered message, text and HTML parts both.
func t6MustLinkTo(t *testing.T, what, text, html string) {
	t.Helper()
	if !strings.Contains(text, t6NoLinkWord+" "+t6AdminLink) {
		t.Errorf("%s text part has no %q line to %s:\n%s", what, t6NoLinkWord, t6AdminLink, text)
	}
	if !strings.Contains(html, `href="`+t6AdminLink+`"`) || !strings.Contains(html, ">Open the console</a>") {
		t.Errorf("%s HTML part has no button to %s", what, t6AdminLink)
	}
	for part, body := range map[string]string{"text": text, "html": html} {
		if strings.Contains(body, t6AgentHost) {
			t.Errorf("%s %s part carries the agent host %s", what, part, t6AgentHost)
		}
	}
}

func t6MustNotLink(t *testing.T, what, text, html string) {
	t.Helper()
	for part, body := range map[string]string{"text": text, "html": html} {
		for _, bad := range []string{t6NoLinkWord, "http://", "https://" + t6AdminHost, t6AgentHost, ">Open the console</a>", `href=""`} {
			if strings.Contains(body, bad) {
				t.Errorf("%s %s part carries %q with no admin_url set", what, part, bad)
			}
		}
	}
	if strings.Contains(html, `<a href="http`) {
		t.Errorf("%s HTML part has a link with no admin_url set", what)
	}
}

// Every sender serve builds is given admin_url, never public_url.
func TestEverySenderIsGivenAdminURLAndNotPublicURL(t *testing.T) {
	app, logs := t6Serve(t, t6AdminURL, t6PublicURL, true)
	for name, got := range map[string]string{
		"alarm-evaluator notifier": app.Notifier.ConsoleURL,
		"notification-retry":       app.Retrier.ConsoleURL,
		"daily digest":             app.Digest.ConsoleURL,
		"test mail":                app.TestMail.ConsoleURL,
		"system health row":        app.Admin.Mail.AdminLink, // P21-T6 U2: the row beside Alert e-mail reads this
	} {
		if got != t6AdminLink {
			t.Errorf("%s ConsoleURL = %q, want %q", name, got, t6AdminLink)
		}
	}
	if strings.Contains(logs, "mail carries no console link") {
		t.Errorf("serve warned about a missing link although admin_url is set:\n%s", logs)
	}
}

// What each sender renders: the digest of the serve-mode App itself, and the alert and recovery
// notices and the digest over the fixture world with the options the notifier and the retrier hold.
func TestRenderedMailLinksToTheAdminHostAndNeverTheAgentHost(t *testing.T) {
	app, _ := t6Serve(t, t6AdminURL, t6PublicURL, true)
	fx := t6Fixtures(t, t6AdminURL, t6PublicURL)
	ctx := context.Background()

	pv, err := app.Digest.PreviewDigest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t6MustLinkTo(t, "digest (serve)", pv.Text, pv.HTML)

	fv, err := fx.Admin.Fleet.Alarms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eps, err := fx.Store.ListOpenEpisodes(ctx, store.ScopeAll())
	if err != nil || len(eps) == 0 {
		t.Fatalf("fixture episodes: %d, %v", len(eps), err)
	}
	al, err := notify.RenderAlert(fv.View, eps, notify.Options{ConsoleURL: app.Notifier.ConsoleURL})
	if err != nil {
		t.Fatal(err)
	}
	t6MustLinkTo(t, "alert", al.Text, al.HTML)
	rc, err := notify.RenderRecovery(fv.View, eps, notify.Options{ConsoleURL: app.Retrier.ConsoleURL})
	if err != nil {
		t.Fatal(err)
	}
	t6MustLinkTo(t, "recovery", rc.Text, rc.HTML)

	// The fixture console's own senders: the digest preview and the test message through the FakeSender.
	fp, err := fx.Admin.Mail.Digest.PreviewDigest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t6MustLinkTo(t, "digest (fixtures)", fp.Text, fp.HTML)
	if _, err := fx.TestMail.Send(ctx, "ops@example.test", nil); err != nil {
		t.Fatal(err)
	}
	sent := fx.Mail.(*mail.FakeSender).Sent()
	if len(sent) != 1 {
		t.Fatalf("fixtures sent %d messages", len(sent))
	}
	t6MustLinkTo(t, "test mail", sent[0].Text, sent[0].HTML)
}

// With admin_url unset (and public_url set) no sender has a link and no message has a button or a
// link-shaped line. The "Open the console to see the details." sentence of the existing no-link
// path stays: it is a sentence, not a link.
func TestUnsetAdminURLMeansNoLinkAndNoButtonAndNeverPublicURL(t *testing.T) {
	app, logs := t6Serve(t, "", t6PublicURL, true)
	for name, got := range map[string]string{
		"alarm-evaluator notifier": app.Notifier.ConsoleURL,
		"notification-retry":       app.Retrier.ConsoleURL,
		"daily digest":             app.Digest.ConsoleURL,
		"test mail":                app.TestMail.ConsoleURL,
		"system health row":        app.Admin.Mail.AdminLink,
	} {
		if got != "" {
			t.Errorf("%s ConsoleURL = %q with admin_url unset, want none", name, got)
		}
	}
	if n := strings.Count(logs, "admin url is not set; mail carries no console link"); n != 1 || !strings.Contains(logs, `"level":"WARN"`) {
		t.Errorf("serve must warn once that e-mails carry no console link, got %d:\n%s", n, logs)
	}
	if strings.Contains(logs, t6AgentHost) || strings.Contains(logs, t6AdminHost) {
		t.Errorf("the warning carries a host:\n%s", logs)
	}

	ctx := context.Background()
	fx := t6Fixtures(t, "", t6PublicURL)
	pv, err := app.Digest.PreviewDigest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t6MustNotLink(t, "digest (serve)", pv.Text, pv.HTML)
	fv, err := fx.Admin.Fleet.Alarms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eps, err := fx.Store.ListOpenEpisodes(ctx, store.ScopeAll())
	if err != nil || len(eps) == 0 {
		t.Fatalf("fixture episodes: %d, %v", len(eps), err)
	}
	al, err := notify.RenderAlert(fv.View, eps, notify.Options{ConsoleURL: app.Notifier.ConsoleURL})
	if err != nil {
		t.Fatal(err)
	}
	t6MustNotLink(t, "alert", al.Text, al.HTML)
	if !strings.Contains(al.Text, "Open the console to see the details.") || !strings.Contains(al.HTML, "Open the console to see the details.") {
		t.Errorf("the no-link sentence is missing from the alert")
	}
	rc, err := notify.RenderRecovery(fv.View, eps, notify.Options{ConsoleURL: app.Retrier.ConsoleURL})
	if err != nil {
		t.Fatal(err)
	}
	t6MustNotLink(t, "recovery", rc.Text, rc.HTML)
	fp, err := fx.Admin.Mail.Digest.PreviewDigest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t6MustNotLink(t, "digest (fixtures)", fp.Text, fp.HTML)
	if _, err := fx.TestMail.Send(ctx, "ops@example.test", nil); err != nil {
		t.Fatal(err)
	}
	sent := fx.Mail.(*mail.FakeSender).Sent()
	if len(sent) != 1 {
		t.Fatalf("fixtures sent %d messages", len(sent))
	}
	t6MustNotLink(t, "test mail", sent[0].Text, sent[0].HTML)
	if strings.Contains(sent[0].Text, "Open the console") || strings.Contains(sent[0].HTML, "Open the console") {
		t.Errorf("the test message has dangling console wording with no admin_url")
	}
}

// Mail not configured: nothing to warn about, whether or not admin_url is set.
func TestNoLinkWarningWithoutMail(t *testing.T) {
	_, logs := t6Serve(t, "", t6PublicURL, false)
	if strings.Contains(logs, "mail carries no console link") {
		t.Errorf("warned about the link although mail is off:\n%s", logs)
	}
}

// Fixture mode (serve --fixtures) keeps working with no config: no admin_url, no public_url, and the
// previews and the test message come out with no link.
func TestFixturesModeWorksWithNoAdminURL(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	if cfg.AdminURL != "" || cfg.PublicURL != "" {
		t.Fatalf("the defaults carry a URL: %q %q", cfg.AdminURL, cfg.PublicURL)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixtures config with no URL: %v", err)
	}
	app, err := Build(context.Background(), cfg, BuildOptions{FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	ctx := context.Background()
	p, err := app.Admin.Mail.Digest.PreviewDigest(ctx)
	if err != nil || p.Text == "" || p.HTML == "" {
		t.Fatalf("digest preview: %v", err)
	}
	if strings.Contains(p.Text, t6NoLinkWord) {
		t.Errorf("a link with no config:\n%s", p.Text)
	}
	if _, err := app.TestMail.Send(ctx, "ops@example.test", nil); err != nil {
		t.Fatal(err)
	}
	if n := len(app.Mail.(*mail.FakeSender).Sent()); n != 1 {
		t.Fatalf("fixtures sent %d messages", n)
	}
}

// The doctor line. Mail configured and admin_url unset: a WARN that says plainly that e-mails carry
// no console link, and it does not fail doctor. Set: OK with the link. Mail off: no such line.
func TestDoctorMailConsoleLinkLine(t *testing.T) {
	prev := probeMail
	t.Cleanup(func() { probeMail = prev })
	probeMail = func(context.Context, mail.Settings) error { return nil } // no relay in this test
	smtp := config.SMTP{Host: t6RelayHost, Port: 587, Security: "starttls", From: t6SMTPFrom}
	collect := func(cfg config.Config) (lines []string, failed bool) {
		checkMail(cfg, nil, "no database in this test", func(level, name, detail string) {
			lines = append(lines, dline(level, name, detail))
			failed = failed || level == "FAIL"
		})
		return lines, failed
	}
	has := func(lines []string, want string) bool {
		for _, l := range lines {
			if strings.HasPrefix(l, want) {
				return true
			}
		}
		return false
	}

	cfg := config.Default()
	cfg.SMTP, cfg.PublicURL = smtp, t6PublicURL
	lines, failed := collect(cfg)
	if failed {
		t.Fatalf("the missing link failed doctor:\n%s", strings.Join(lines, "\n"))
	}
	if !has(lines, dline("WARN", "admin_url", "admin_url is not set: e-mails carry no console link.")) {
		t.Fatalf("no WARN line for the missing link:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if strings.Contains(l, t6AgentHost) {
			t.Errorf("the line carries the agent host: %s", l)
		}
	}

	cfg.AdminURL = t6AdminURL
	lines, failed = collect(cfg)
	if failed || !has(lines, dline("OK", "admin_url", "e-mails link to "+t6AdminLink+" (admin_url)")) {
		t.Fatalf("no OK line with admin_url set:\n%s", strings.Join(lines, "\n"))
	}

	cfg.AdminURL = "console.example.net/login" // not valid: config says FAIL, this line cannot vouch
	lines, _ = collect(cfg)
	if !has(lines, dline("NOT CHECKED", "admin_url", "admin_url is not valid")) {
		t.Fatalf("no NOT CHECKED line with a bad admin_url:\n%s", strings.Join(lines, "\n"))
	}

	off := config.Default() // no smtp block: mail is off, the link is not the question
	lines, _ = collect(off)
	if strings.Contains(strings.Join(lines, "\n"), "admin_url") {
		t.Fatalf("a link line with mail off:\n%s", strings.Join(lines, "\n"))
	}
}

// The same line through the doctor command, from a config file: a bad admin_url fails the config
// check by name and not by value.
func TestDoctorCommandShowsTheLinkLineAndRefusesABadAdminURL(t *testing.T) {
	cfgPath, _ := doctorRig(t)
	prev := probeMail
	t.Cleanup(func() { probeMail = prev })
	probeMail = func(context.Context, mail.Settings) error { return nil }
	setConfig(t, cfgPath, map[string]any{"smtp": map[string]any{"host": t6RelayHost, "port": 587, "security": "starttls", "from": t6SMTPFrom}})
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if !strings.Contains(out, dline("WARN", "admin_url", "admin_url is not set: e-mails carry no console link.")) {
		t.Fatalf("doctor output:\n%s", out)
	}
	setConfig(t, cfgPath, map[string]any{"admin_url": t6AdminURL})
	_, out, _ = run(t, "", "doctor", "--config", cfgPath)
	if !strings.Contains(out, dline("OK", "admin_url", "e-mails link to "+t6AdminLink+" (admin_url)")) {
		t.Fatalf("doctor output with admin_url:\n%s", out)
	}
	setConfig(t, cfgPath, map[string]any{"admin_url": "https://console.example.net/SECRETPATH"})
	code, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if code == 0 || !strings.Contains(out, "FAIL        config") || !strings.Contains(out, "admin_url must be http(s)://host[:port]") || strings.Contains(out, "SECRETPATH") {
		t.Fatalf("a bad admin_url (exit %d):\n%s", code, out)
	}
}

// P21-T6 U2: the System health row beside Alert e-mail, through a real console and its API. Mail is set
// up in both runs; admin_url decides the row. A warning never turns the overall state red.
func TestSystemHealthAdminURLRowFollowsTheConfig(t *testing.T) {
	for _, tc := range []struct {
		name, adminURL, status, detail, never string
	}{
		{"unset", "", "warn", "e-mails carry no console link", t6AdminHost},
		{"set", t6AdminURL, "ok", "e-mails link to " + t6AdminLink, t6AgentHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := tc.adminURL
			app := r3RealApp(t, func(c *config.Config) { c.AdminURL, c.PublicURL = url, t6PublicURL })
			token := r3Token(t, app)
			base := r3Serve(t, app)
			row := func(h adminapi.SystemHealthResponse) adminapi.SystemComponentJSON {
				for _, c := range h.Components {
					if c.ID == "admin_url" {
						return c
					}
				}
				return adminapi.SystemComponentJSON{}
			}
			// The row is configuration, but C00 (self-checks not wired yet) holds an ok row at not_checked
			// until the first probe results are in: wait for the verdict this test is about.
			h, ok := r3WaitFor(t, base, token, func(h adminapi.SystemHealthResponse) bool { return row(h).Status == tc.status })
			c := row(h)
			if !ok {
				t.Fatalf("admin_url row = %+v, want %s:%s", c, tc.status, r3Rows(h))
			}
			if !strings.Contains(c.Detail, tc.detail) || strings.Contains(c.Detail, tc.never) {
				t.Errorf("detail = %q, want %q and never %q", c.Detail, tc.detail, tc.never)
			}
			if h.Overall == "failed" {
				t.Errorf("overall failed because of the link row:%s", r3Rows(h))
			}
		})
	}
}
