package state

import (
	"strings"
	"testing"
	"time"
)

// The outbound heartbeat ping (ADR 0020 decision 12): three failed pings in a row raise a
// warning that names the class of the failure. No banner while no URL is configured (the feature
// is optional), none while the ping is withheld (the evaluator banners already say why), and a
// count that cannot be right is a "not checked" warning, never silence.
func TestM4HeartbeatPingBanner(t *testing.T) {
	c := bannerCtx{now: t0, loc: time.UTC}
	const failing = "The external heartbeat ping is failing (timeout): an outside monitor may report this console as down."
	cases := []struct {
		name string
		in   *HeartbeatFacts
		want []string // ID/severity
		text string
	}{
		{name: "not_supplied_is_nothing", in: nil},
		{name: "no_url_configured_is_nothing", in: &HeartbeatFacts{ConsecutiveFailures: 9, LastFailureClass: "timeout"}},
		{name: "healthy", in: &HeartbeatFacts{Configured: true, LastSuccessAt: ago(time.Minute)}},
		{name: "one_failure", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: 1, LastFailureClass: "timeout"}},
		{name: "two_failures", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: 2, LastFailureClass: "timeout"}},
		{name: "three_failures_warn", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: 3, LastFailureClass: "timeout"},
			want: []string{"C19/alarm"}, text: failing},
		{name: "many_failures_still_one_banner", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: 40, LastFailureClass: "timeout"},
			want: []string{"C19/alarm"}, text: failing},
		{name: "withheld_is_nothing", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: 5, LastFailureClass: "timeout", Withheld: true}},
		{name: "class_is_named", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: 3, LastFailureClass: "status_5xx"},
			want: []string{"C19/alarm"}, text: "(status_5xx)"},
		{name: "unknown_class_word_is_other", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: 3, LastFailureClass: "https://monitor.example.test/ping/secret"},
			want: []string{"C19/alarm"}, text: "(other)"},
		{name: "empty_class_is_other", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: 3},
			want: []string{"C19/alarm"}, text: "(other)"},
		{name: "negative_count_is_not_checked", in: &HeartbeatFacts{Configured: true, ConsecutiveFailures: -1},
			want: []string{"C19b/alarm"}, text: "failure count is garbled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			bs := heartbeatBanners(tc.in, c)
			for _, b := range bs {
				got = append(got, b.ID+"/"+b.Severity.String())
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("banners = %v, want %v", got, tc.want)
			}
			if tc.text != "" && !strings.Contains(bs[0].Text, tc.text) {
				t.Fatalf("text %q does not contain %q", bs[0].Text, tc.text)
			}
			for _, b := range bs {
				if strings.Contains(b.Text, "example.test") || strings.Contains(b.Text, "secret") {
					t.Fatalf("banner text carries the class input verbatim: %q", b.Text)
				}
			}
		})
	}
}

func TestM4HeartbeatPingBannerCountIsTheStreak(t *testing.T) {
	bs := heartbeatBanners(&HeartbeatFacts{Configured: true, ConsecutiveFailures: 7, LastFailureClass: "dns"}, bannerCtx{now: t0, loc: time.UTC})
	if len(bs) != 1 || bs[0].Count != 7 {
		t.Fatalf("banners %+v", bs)
	}
}
