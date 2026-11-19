package state

import "testing"

// ADR 0020 decision 10: SMTP configured and no recipient with the alerts switch is a warning of its
// own (C18): alerts that go to nobody must not look like a working set-up. With SMTP not configured
// C03b already says mail is not set up, so C18 stays away.
func TestC09SMTPNoAlertRecipient(t *testing.T) {
	const text = "Alert e-mail has no recipient: alarms are not mailed to anyone."
	runProbeCases(t, []probeCase{
		{name: "C09_smtp_no_alert_recipient_warns", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.NoAlertRecipient = true
		}, want: []string{"C18/alarm"}, contains: text},
		{name: "C09_smtp_no_alert_recipient_not_shown_when_smtp_is_not_configured", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.Probe, p.SMTP.LastSendOKAt, p.SMTP.NoAlertRecipient = ProbeRun{Unconfigured: "SMTP is not configured"}, 0, true
		}, want: []string{"C03b/alarm"}},
		{name: "C09_smtp_no_alert_recipient_beside_a_failing_relay", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.NoAlertRecipient, p.SMTP.ConsecutiveSendFailures = true, 3
		}, want: []string{"C03/critical", "C18/alarm"}},
		{name: "C09_smtp_no_alert_recipient_beside_an_unread_log", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.NoAlertRecipient, p.SMTP.NotRead = true, "the delivery log could not be read"
		}, want: []string{"C03b/alarm", "C18/alarm"}},
		{name: "C09_smtp_with_an_alert_recipient_no_warning", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.NoAlertRecipient = false
		}},
	})
}
