package health

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// C05 covers two certificates (architecture §4.6): the gateway's, raised by
// internal/state, and the agent listener's, raised here from the ADR 0013
// self-probe. They share the id, so the merge keys on (id, subject).
func TestC15GatewayAndListenerCertificatesBothShow(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ms := func(t time.Time) state.Millis { return state.Millis(t.UnixMilli()) }
	gw := state.Evaluate(state.World{LoadComplete: true, LoadedAt: now, Probes: state.ProbeFacts{Gateway: &state.ProbeRun{
		LastRunAt: ms(now.Add(-30 * time.Second)), LastOKAt: ms(now.Add(-30 * time.Second)),
		CertNotAfter: ms(now.Add(20*24*time.Hour + time.Hour)),
	}}}, now).Banners
	listener := EndpointBanners(probe.Status{Runs: true, Host: "agents.example.com:8443", Interval: probe.DefaultInterval,
		Started: now.Add(-time.Hour), Known: true,
		Last: probe.Result{At: now.Add(-time.Minute), OK: true, NotAfter: now.Add(3*24*time.Hour + time.Hour)}}, now)

	merged := SortBanners(dedupe(append(append([]state.Banner(nil), gw...), listener...)))
	var got []state.Banner
	for _, b := range merged {
		if b.ID == BannerCertExpiring {
			got = append(got, b)
		}
	}
	if len(got) != 2 {
		t.Fatalf("C05 banners = %+v, want the gateway and the agent listener", merged)
	}
	// Critical before warn: the listener at 3 days, then the gateway at 20 days.
	if got[0].Subject != state.SubjectAgentListener || got[0].Severity != state.SeverityCritical || got[0].Count != 3 {
		t.Errorf("first C05 = %+v, want the agent listener, critical, 3 days", got[0])
	}
	if got[1].Subject != state.SubjectGateway || got[1].Severity != state.SeverityAlarm || got[1].Count != 20 ||
		got[1].Text != "Gateway certificate expires in 20 days." {
		t.Errorf("second C05 = %+v, want the gateway, warn, 20 days", got[1])
	}

	// The same (id, subject) twice is still one banner.
	if d := dedupe([]state.Banner{got[1], got[1], got[0]}); len(d) != 2 {
		t.Errorf("dedupe kept %d of a duplicated gateway C05 and a listener C05, want 2", len(d))
	}
}
