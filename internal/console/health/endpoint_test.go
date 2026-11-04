package health

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func healthy(now time.Time, certLeft time.Duration) probe.Status {
	return probe.Status{Runs: true, Host: "agents.example.com:8443", Interval: probe.DefaultInterval, Started: now.Add(-time.Hour), Known: true,
		LastOK: now.Add(-time.Minute), Last: probe.Result{At: now.Add(-time.Minute), OK: true, NotAfter: now.Add(certLeft)}}
}

func failing(now time.Time, stage probe.Stage, class string, n int, since time.Duration) probe.Status {
	st := healthy(now, 90*24*time.Hour)
	st.Last = probe.Result{At: now.Add(-time.Minute), Stage: stage, Class: class}
	st.Failures, st.FailingSince = n, now.Add(-since)
	return st
}

func only(t *testing.T, b []state.Banner, id string, sev state.Severity, contains ...string) state.Banner {
	t.Helper()
	if len(b) != 1 || b[0].ID != id || b[0].Severity != sev {
		t.Fatalf("want one %s at %v, got %+v", id, sev, b)
	}
	for _, c := range contains {
		if !strings.Contains(b[0].Text, c) {
			t.Fatalf("%s text %q lacks %q", id, b[0].Text, c)
		}
	}
	return b[0]
}

func TestEndpointStagesNameTheFailure(t *testing.T) {
	for _, c := range []struct {
		stage probe.Stage
		class string
		want  string
	}{
		{probe.StageDNS, "nxdomain", "agents.example.com:8443 does not resolve (DNS)"},
		{probe.StageConnect, "refused", "nothing accepts connections on agents.example.com:8443 (connect)"},
		{probe.StageTLS, "untrusted", "is not trusted (TLS)"},
		{probe.StageTLS, "hostname", "does not match agents.example.com:8443 (TLS)"},
		{probe.StageTLS, "handshake", "the TLS handshake with agents.example.com:8443 fails (TLS)"},
		{probe.StageTLS, "pin_mismatch", "does not match agent_probe.spki_pin, so pinned agents refuse it (TLS)"},
		{probe.StageCertExpired, "expired", "certificate has expired"},
		{probe.StageResponder, "status_502", "not as this console's agent API (wrong responder)"},
	} {
		b := only(t, EndpointBanners(failing(t0, c.stage, c.class, 3, 9*time.Minute), t0), "C04", state.SeverityCritical,
			"Agent API unreachable: ", c.want, "No agent can check in", "Failing for 9 min.")
		if b.Count != 9 {
			t.Fatalf("count %d", b.Count)
		}
	}
}

func TestEndpointGraceAndClear(t *testing.T) {
	// One failure, however old: no banner (a blip, or a restart).
	if b := EndpointBanners(failing(t0, probe.StageConnect, "timeout", 1, 30*time.Minute), t0); len(b) != 0 {
		t.Fatalf("one failure: %+v", b)
	}
	// Two failures inside the grace: no banner yet.
	if b := EndpointBanners(failing(t0, probe.StageConnect, "timeout", 2, EndpointGrace-time.Second), t0); len(b) != 0 {
		t.Fatalf("inside the grace: %+v", b)
	}
	// Two failures spanning the grace: critical.
	only(t, EndpointBanners(failing(t0, probe.StageConnect, "timeout", 2, EndpointGrace), t0), "C04", state.SeverityCritical)
	// An expired certificate skips the grace.
	only(t, EndpointBanners(failing(t0, probe.StageCertExpired, "expired", 1, 0), t0), "C04", state.SeverityCritical, "expired")
	// A certificate that ran out after the last passing run is reported now.
	st := healthy(t0, -time.Minute)
	only(t, EndpointBanners(st, t0), "C04", state.SeverityCritical, "certificate has expired")
	// A healthy probe clears it.
	if b := EndpointBanners(healthy(t0, 90*24*time.Hour), t0); len(b) != 0 {
		t.Fatalf("healthy: %+v", b)
	}
}

func TestEndpointCertificateExpiring(t *testing.T) {
	day := 24 * time.Hour
	if b := EndpointBanners(healthy(t0, 21*day), t0); len(b) != 0 {
		t.Fatalf("21 days: %+v", b)
	}
	only(t, EndpointBanners(healthy(t0, 21*day-time.Hour), t0), "C05", state.SeverityAlarm, "Agent listener certificate expires in 20 days.")
	only(t, EndpointBanners(healthy(t0, 12*day+time.Hour), t0), "C05", state.SeverityAlarm, "expires in 12 days.")
	only(t, EndpointBanners(healthy(t0, 7*day), t0), "C05", state.SeverityAlarm, "expires in 7 days.")
	only(t, EndpointBanners(healthy(t0, 7*day-time.Hour), t0), "C05", state.SeverityCritical, "expires in 6 days.", "Agents will stop checking in")
	only(t, EndpointBanners(healthy(t0, day+time.Hour), t0), "C05", state.SeverityCritical, "expires in 1 day.")
	only(t, EndpointBanners(healthy(t0, time.Hour), t0), "C05", state.SeverityCritical, "expires in less than a day.")
}

func TestEndpointCannotRunIsAWarning(t *testing.T) {
	only(t, EndpointBanners(probe.Status{Limit: probe.LimitNoPublicURL}, t0), "C04b", state.SeverityAlarm, "public_url is not set")
	only(t, EndpointBanners(probe.Status{Limit: probe.LimitCAFile}, t0), "C04b", state.SeverityAlarm, "agent_probe.ca_file")
	only(t, EndpointBanners(probe.Status{Limit: probe.LimitSPKIPin}, t0), "C04b", state.SeverityAlarm, "agent_probe.spki_pin")
	st := healthy(t0, 0)
	st.Limit, st.Last.NotAfter = probe.LimitPlainHTTP, time.Time{}
	only(t, EndpointBanners(st, t0), "C04b", state.SeverityAlarm, "plain HTTP")
	// Started but never completed, or stopped completing: say so.
	st = probe.Status{Runs: true, Interval: probe.DefaultInterval, Started: t0.Add(-20 * time.Minute)}
	only(t, EndpointBanners(st, t0), "C04b", state.SeverityAlarm, "has not completed for 20 min")
	st = healthy(t0.Add(-15*time.Minute), 90*24*time.Hour)
	only(t, EndpointBanners(st, t0), "C04b", state.SeverityAlarm, "has not completed for 16 min")
	// Just started, first run in flight: nothing yet.
	if b := EndpointBanners(probe.Status{Runs: true, Interval: probe.DefaultInterval, Started: t0.Add(-5 * time.Second)}, t0); len(b) != 0 {
		t.Fatalf("first run in flight: %+v", b)
	}
}

// The evaluator wires the probe: a healthy probe raises nothing, not even when
// every agent went silent at the same instant (the frozen fixture fleet hours
// on: a scheduled shutdown, a console restart, a site outage); a failing probe
// is C04.
func TestFleetUsesTheProbe(t *testing.T) {
	ctx := context.Background()
	e, c := newEval(t)
	st := healthy(fixtures.Now(), 90*24*time.Hour)
	e.Endpoint = func() probe.Status { return st }
	// C00 (self-checks not wired) stays until store.LoadWorld supplies probe facts (M3-T2).
	f, _ := e.Fleet(ctx, store.ScopeAll())
	if ids(f.View.Banners) != "C00" {
		t.Fatalf("healthy probe: %s", ids(f.View.Banners))
	}
	for _, quiet := range []time.Duration{20 * time.Minute, 40 * time.Minute, 14 * time.Hour} {
		c.t = fixtures.Now().Add(quiet)
		st = healthy(c.t, 90*24*time.Hour)
		if f, _ = e.Fleet(ctx, store.ScopeAll()); ids(f.View.Banners) != "C00" {
			t.Fatalf("every agent offline for %v, probe healthy: %s", quiet, ids(f.View.Banners))
		}
	}
	st = failing(c.t, probe.StageTLS, "untrusted", 3, 10*time.Minute)
	f, _ = e.Fleet(ctx, store.ScopeAll())
	if ids(f.View.Banners) != "C04,C00" || f.View.Banners[0].Severity != state.SeverityCritical {
		t.Fatalf("failing probe: %s", ids(f.View.Banners))
	}
	if f.View.AllClear() {
		t.Fatal("AllClear with C04")
	}
	st = probe.Status{Limit: probe.LimitNoPublicURL}
	c.t = fixtures.Now()
	if f, _ = e.Fleet(ctx, store.ScopeAll()); ids(f.View.Banners) != "C00,C04b" {
		t.Fatalf("no public_url: %s", ids(f.View.Banners))
	}
}

func TestEndpointExamplesCoverEveryKind(t *testing.T) {
	got := map[string]int{}
	for _, b := range EndpointExamples(t0) {
		got[b.ID+"/"+b.Severity.String()]++
		if b.Text == "" {
			t.Fatalf("empty text: %+v", b)
		}
	}
	want := map[string]int{"C04/" + state.SeverityCritical.String(): 6, "C05/" + state.SeverityAlarm.String(): 1, "C05/" + state.SeverityCritical.String(): 1,
		"C04b/" + state.SeverityAlarm.String(): 1}
	if len(got) != len(want) {
		t.Errorf("kinds %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: %d, want %d (all %v)", k, got[k], n, got)
		}
	}
}
