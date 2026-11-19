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

// P20-W2BF item 3: devices whose policy has digest_include off are left out of the digest's
// exception lists, and the view counts them: a customer all of whose devices are excluded is an
// excluded customer, a lone excluded device of an included customer an excluded device. Console
// banners are never excluded.
func TestC22DigestLeavesOutWhatThePolicyExcludes(t *testing.T) {
	w := healthyFleet(3, 2)
	for _, i := range []int{0, 1, 2} { // customer 1 entirely, and one device of customer 2
		w.Devices[i].Thresholds = DefaultThresholds()
		w.Devices[i].Thresholds.DigestExcluded = true
		w.Devices[i].LastBackup, w.Devices[i].ConsecutiveFailures = failedRun(hour, "network"), 1
	}
	w.Devices[3].LastBackup, w.Devices[3].ConsecutiveFailures = failedRun(hour, "network"), 1 // customer 2, included
	w.Devices[4].Complete, w.Devices[4].LoadErrorClass = false, "timeout"                     // a banner (C09)
	v := Evaluate(w, t0)
	d := DigestOf(v, t0)
	var hosts []string
	for _, l := range d.Lines {
		hosts = append(hosts, l.Hostname)
	}
	if len(d.Lines) != 2 || d.Lines[0].Hostname != w.Devices[4].Hostname && d.Lines[1].Hostname != w.Devices[4].Hostname {
		t.Fatalf("lines %v, want the included failing device and the UNKNOWN one", hosts)
	}
	for _, l := range d.Lines {
		if l.DeviceID == w.Devices[0].ID || l.DeviceID == w.Devices[1].ID || l.DeviceID == w.Devices[2].ID {
			t.Fatalf("an excluded device is listed: %v", hosts)
		}
	}
	if d.ExcludedCustomers != 1 || d.ExcludedDevices != 1 {
		t.Fatalf("excluded customers %d devices %d, want 1 and 1", d.ExcludedCustomers, d.ExcludedDevices)
	}
	if len(d.Banners) == 0 {
		t.Fatal("the console banners were dropped")
	}
	for _, ds := range v.Devices {
		want := ds.ID == w.Devices[0].ID || ds.ID == w.Devices[1].ID || ds.ID == w.Devices[2].ID
		if ds.DigestExcluded != want {
			t.Fatalf("device %s DigestExcluded = %v, want %v", ds.ID, ds.DigestExcluded, want)
		}
	}
}

// P20-W2BF item 5: deliveries that became final without being sent in the last 24 hours are a
// warning of their own (C21), also while another recipient's mail goes out and C03 stays quiet.
func TestC21UndeliveredMailIsAWarning(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C21_one_undelivered_message", mod: func(p *ProbeFacts, _ *World) { p.SMTP.Undelivered24h = 1 },
			want: []string{"C21/alarm"}, contains: "1 e-mail message could not be delivered in the last 24 hours: see the delivery log."},
		{name: "C21_three_undelivered_messages", mod: func(p *ProbeFacts, _ *World) { p.SMTP.Undelivered24h = 3 },
			want: []string{"C21/alarm"}, contains: "3 e-mail messages could not be delivered in the last 24 hours: see the delivery log."},
		{name: "C21_none_no_banner", mod: func(p *ProbeFacts, _ *World) { p.SMTP.Undelivered24h = 0 }},
	})
}

// P20-W2BF C4: DeviceState.CurrentBackup is the engine's own freshness verdict (a success since
// enrollment, not older than the stale threshold), whatever the primary state: an outdated agent
// with fresh backups has one; a stale, never-backed-up or long-paused device has not.
func TestDeviceStateCurrentBackup(t *testing.T) {
	w := healthyFleet(1, 4)
	w.Devices[0].ExpectedVersion = "9.0.0" // OUTDATED, backups current
	w.Devices[1].LastSuccess, w.Devices[1].LastBackup = ago(100*hour), successRun(100*hour)
	newlyEnrolled(&w.Devices[2], hour)
	w.Devices[3].PausedAt = ago(30 * day)
	w.Devices[3].LastSuccess, w.Devices[3].LastBackup = ago(30*day), successRun(30*day)
	want := map[string]bool{w.Devices[0].ID: true, w.Devices[1].ID: false, w.Devices[2].ID: false, w.Devices[3].ID: false}
	for _, d := range Evaluate(w, t0).Devices {
		if d.CurrentBackup != want[d.ID] {
			t.Errorf("%s (%s): CurrentBackup %v, want %v", d.ID, d.Primary, d.CurrentBackup, want[d.ID])
		}
	}
}
