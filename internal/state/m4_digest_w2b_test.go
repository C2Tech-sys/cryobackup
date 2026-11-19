package state

import (
	"strings"
	"testing"
	"time"
)

// P20-W2B: the digest's dedupe key carries the recipient rule (ADR 0020 decision 8), so one
// recipient can be retried without mailing the others twice: "digest:<local-date>:<rule id>".
func TestE05DigestKeyCarriesTheRule(t *testing.T) {
	chi := zone(t, "America/Chicago")
	at := time.Date(2026, 3, 9, 4, 30, 0, 0, time.UTC) // 23:30 CDT on Mar 8
	if got := DigestKey(at, chi, "01RULE"); got != "digest:2026-03-08:01RULE" {
		t.Errorf("key = %s", got)
	}
	if got := DigestKey(at, nil, "01RULE"); got != "digest:2026-03-09:01RULE" {
		t.Errorf("nil zone key = %s, want the UTC date", got)
	}
}

// P20-W2B item 3: C14 with mail not set up is the "not set up" warning, not a red alarm about a job
// that cannot run; enabled with nobody to mail is C20. A disabled digest stays silent.
func TestC25DigestMailNotSetUpAndNoRecipient(t *testing.T) {
	late := t0.Add(-48 * time.Hour) // the last digest went out two days ago: C14 would be due
	runProbeCases(t, []probeCase{
		{name: "C25_digest_mail_not_set_up_is_a_warning", mod: func(p *ProbeFacts, _ *World) {
			p.Digest.LastSentAt, p.Digest.MailNotSetUp = Millis(late.UnixMilli()), true
		}, want: []string{"C14b/alarm"}, contains: "Daily digest not sent: e-mail is not set up"},
		{name: "C25_digest_no_recipient_warns", mod: func(p *ProbeFacts, _ *World) {
			p.Digest.LastSentAt, p.Digest.NoRecipient = Millis(late.UnixMilli()), true
		}, want: []string{"C20/alarm"}, contains: "The daily digest has no recipient"},
		{name: "C25_digest_disabled_with_mail_not_set_up_is_silent", mod: func(p *ProbeFacts, _ *World) {
			p.Digest.LastSentAt, p.Digest.MailNotSetUp, p.Digest.Disabled = Millis(late.UnixMilli()), true, true
		}},
		{name: "C25_digest_overdue_with_mail_and_a_recipient_is_critical", mod: func(p *ProbeFacts, _ *World) {
			p.Digest.LastSentAt = Millis(late.UnixMilli())
		}, want: []string{"C14/critical"}},
	})
}

// P20-W2B: what an operator who never configured mail sees after the upgrade (no smtp block, the
// digest on by default, never sent): two warnings, the e-mail one and the digest's "not set up",
// and no red C14.
func TestC25NeverConfiguredMailAfterTheUpgrade(t *testing.T) {
	w := healthyFleet(2, 3)
	w.Probes = healthyProbes()
	w.Probes.SMTP = &SMTPFacts{Probe: ProbeRun{Unconfigured: "SMTP is not configured"}}
	w.Probes.Digest = &DigestFacts{LocalTime: "07:00", MailNotSetUp: true}
	var got []string
	for _, b := range Evaluate(w, t0).Banners {
		got = append(got, b.ID+"/"+b.Severity.String()+": "+b.Text)
	}
	want := []string{
		"C03b/alarm: Alert e-mail not checked: SMTP is not configured, so alert e-mail that cannot be delivered would go unnoticed.",
		"C14b/alarm: Daily digest not sent: e-mail is not set up. Configure the smtp block of config.json, or turn the digest off.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("banners:\n%s", strings.Join(got, "\n"))
	}
}
