package adminapi

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
)

// P21-T6 U2: the "Console link in e-mails" row of System health. Mail configured and admin_url unset
// is a warning that names the key; set is ok and names the link; mail not configured is not applicable
// (the pattern of a daily digest that is turned off). Fixtures mode has no row.

const (
	adminLinkDoc  = "https://console.example.net/"
	adminLinkHost = "console.example.net"
)

func adminURLRowFacts(m *mailLinkFacts) healthFacts {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ran, next := now.Add(-time.Minute), now.Add(time.Hour)
	f := fullEvidence(now, &ran, &next)
	f.MailLink = m
	return f
}

func TestAdminURLRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		facts  *mailLinkFacts
		status string
		detail []string // every one must be in the detail
		never  []string // none may be
		overal string
	}{
		{"mail set up, admin_url unset", &mailLinkFacts{Configured: true}, statusWarn,
			[]string{"e-mails carry no console link", "admin_url"}, []string{adminLinkHost}, statusWarn},
		{"mail set up, admin_url set", &mailLinkFacts{Configured: true, Link: adminLinkDoc}, statusOK,
			[]string{adminLinkHost, "admin_url"}, []string{"no console link"}, statusOK},
		{"mail not set up, admin_url unset", &mailLinkFacts{}, statusNotApplicable,
			[]string{"not set up", "no e-mail to carry a console link"}, []string{"admin_url is not set"}, statusOK},
		{"mail not set up, admin_url set", &mailLinkFacts{Link: adminLinkDoc}, statusNotApplicable,
			[]string{"not set up"}, []string{"admin_url is not set"}, statusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := adminURLRowFacts(tc.facts)
			rows := rowsOf(f)
			c, ok := rows["admin_url"]
			if !ok {
				t.Fatalf("no admin_url row in %+v", rows)
			}
			if c.Status != tc.status {
				t.Fatalf("status = %s %q, want %s", c.Status, c.Detail, tc.status)
			}
			if c.Label != "Console link in e-mails" {
				t.Errorf("label = %q", c.Label)
			}
			for _, w := range tc.detail {
				if !strings.Contains(c.Detail, w) {
					t.Errorf("detail %q lacks %q", c.Detail, w)
				}
			}
			for _, n := range tc.never {
				if strings.Contains(c.Detail, n) {
					t.Errorf("detail %q holds %q", c.Detail, n)
				}
			}
			if c.Status == statusWarn && c.CheckedAt == nil {
				t.Error("a warning with no checked_at")
			}
			// The warning is the worst this row says, and it is a warning: overall is warn, never failed.
			if got := overallOf(buildComponents(f)); got != tc.overal {
				t.Errorf("overall = %s, want %s", got, tc.overal)
			}
			// Nothing else on the page changed: every other row is what the control says.
			for id, o := range rows {
				if id == "admin_url" {
					continue
				}
				want := statusOK
				if slices.Contains(fullEvidenceNotInBuild, id) {
					want = statusNotInBuild
				}
				if o.Status != want {
					t.Errorf("%s = %s, want %s", id, o.Status, want)
				}
			}
		})
	}
}

// The row stands beside the Alert e-mail row on the page.
func TestAdminURLRowIsBesideAlertEmail(t *testing.T) {
	var ids []string
	for _, c := range buildComponents(adminURLRowFacts(&mailLinkFacts{Configured: true, Link: adminLinkDoc})) {
		ids = append(ids, c.ID)
	}
	for i, id := range ids {
		if id == "smtp" {
			if i+1 >= len(ids) || ids[i+1] != "admin_url" {
				t.Fatalf("the row after smtp is not admin_url: %v", ids)
			}
			return
		}
	}
	t.Fatalf("no smtp row: %v", ids)
}

// Fixtures mode has no configuration to be missing a key, and a console wired without an e-mail service
// has no e-mail to carry a link: no row in either, rather than a claim with no evidence.
func TestAdminURLRowIsAbsentInFixturesModeAndWithoutAMailService(t *testing.T) {
	for name, m := range map[string]*mailLinkFacts{"fixtures": {Configured: true, Fixtures: true}, "no mail service": nil} {
		rows := rowsOf(adminURLRowFacts(m))
		if c, ok := rows["admin_url"]; ok {
			t.Fatalf("%s shows the row: %+v", name, c)
		}
		if len(rows) != len(healthRowOrder)-1 {
			t.Fatalf("%s: %d rows, want %d", name, len(rows), len(healthRowOrder)-1)
		}
		if got := overallOf(buildComponents(adminURLRowFacts(m))); got != statusOK {
			t.Errorf("%s: overall = %s", name, got)
		}
	}
}

// The same through the API: MailService.AdminLink and .Configured are what the row reads.
func TestAdminURLRowOverTheAPI(t *testing.T) {
	e := newR3Env(t) // probes wired and healthy: C00 is not raised, so an ok row can read ok
	row := func() SystemComponentJSON { t.Helper(); return e.row("admin_url") }
	for _, c := range e.health().Components {
		if c.ID == "admin_url" {
			t.Fatalf("a console with no mail service shows the row: %+v", c)
		}
	}
	m := e.withMail(mail.NewFakeSender()) // Configured, AdminLink unset
	if c := row(); c.Status != statusWarn || !strings.Contains(c.Detail, "e-mails carry no console link") || !strings.Contains(c.Detail, "admin_url") {
		t.Fatalf("mail set up, admin_url unset: %+v", c)
	}
	m.AdminLink = adminLinkDoc
	if c := row(); c.Status != statusOK || !strings.Contains(c.Detail, adminLinkHost) {
		t.Fatalf("mail set up, admin_url set: %+v", c)
	}
	m.AdminLink, m.Configured = "", false
	if c := row(); c.Status != statusNotApplicable {
		t.Fatalf("mail not set up: %+v", c)
	}
	m.Fixtures, m.Configured = true, true
	for _, c := range e.health().Components {
		if c.ID == "admin_url" {
			t.Fatalf("fixtures mode shows the row: %+v", c)
		}
	}
}
