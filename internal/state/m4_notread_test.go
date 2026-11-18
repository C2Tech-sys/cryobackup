package state

import "testing"

// ADR 0020: the loader of the delivery-log facts says when it could not read them, and the two
// mail banners turn that into their "not checked" warning instead of reading empty facts as
// "nothing is wrong" (C03b, C14b).
func TestC09C25MailFactsNotRead(t *testing.T) {
	runProbeCases(t, []probeCase{
		{name: "C09_smtp_send_log_not_read_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.NotRead = "the delivery log could not be read"
		}, want: []string{"C03b/alarm"}, contains: "Alert e-mail not checked: the delivery log could not be read, so alert e-mail that cannot be delivered would go unnoticed."},
		{name: "C09_smtp_send_log_not_read_but_probe_failing_still_alarms", mod: func(p *ProbeFacts, _ *World) {
			p.SMTP.NotRead, p.SMTP.Probe.ConsecutiveFailures = "the delivery log could not be read", 3
		}, want: []string{"C03/critical"}},
		{name: "C25_digest_facts_not_read_is_not_checked", mod: func(p *ProbeFacts, _ *World) {
			*p.Digest = DigestFacts{LocalTime: "07:00", NotRead: "the digest settings could not be read"}
		}, want: []string{"C14b/alarm"}, contains: "Daily digest not checked: the digest settings could not be read, so a digest that is never sent would go unnoticed."},
		{name: "C25_digest_disabled_beats_not_read", mod: func(p *ProbeFacts, _ *World) {
			*p.Digest = DigestFacts{Disabled: true, NotRead: "the digest settings could not be read"}
		}},
		{name: "C25_digest_not_in_this_build_beats_not_read", mod: func(p *ProbeFacts, _ *World) {
			*p.Digest = DigestFacts{NotInBuild: true, NotRead: "the digest settings could not be read"}
		}},
	})
}
