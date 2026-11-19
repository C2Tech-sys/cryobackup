package state

import "testing"

// P20-W2B item 4: the heartbeat facts ride on ProbeFacts and probeBanners raises C19 from them. The
// feature is optional: no facts, or no URL configured, raises nothing (no "not checked" warning).
func TestC19HeartbeatPingThroughTheEngine(t *testing.T) {
	const text = "The external heartbeat ping is failing (status_5xx): an outside monitor may report this console as down."
	runProbeCases(t, []probeCase{
		{name: "C19_heartbeat_not_supplied_raises_nothing", mod: func(p *ProbeFacts, _ *World) { p.Heartbeat = nil }},
		{name: "C19_heartbeat_not_configured_raises_nothing", mod: func(p *ProbeFacts, _ *World) {
			p.Heartbeat = &HeartbeatFacts{ConsecutiveFailures: 9, LastFailureClass: "timeout"}
		}},
		{name: "C19_heartbeat_three_failures_warn", mod: func(p *ProbeFacts, _ *World) {
			p.Heartbeat = &HeartbeatFacts{Configured: true, ConsecutiveFailures: 3, LastFailureClass: "status_5xx"}
		}, want: []string{"C19/alarm"}, contains: text},
		{name: "C19_heartbeat_two_failures_quiet", mod: func(p *ProbeFacts, _ *World) {
			p.Heartbeat = &HeartbeatFacts{Configured: true, ConsecutiveFailures: 2, LastFailureClass: "status_5xx"}
		}},
		{name: "C19_heartbeat_withheld_quiet", mod: func(p *ProbeFacts, _ *World) {
			p.Heartbeat = &HeartbeatFacts{Configured: true, ConsecutiveFailures: 5, LastFailureClass: "timeout", Withheld: true}
		}},
	})
}
