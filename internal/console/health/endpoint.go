package health

import (
	"fmt"
	"math"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ADR 0013: the agent endpoint banners come from the console's self-probe of
// its public agent endpoint, never from devices being offline. A sleeping, shut
// down or disconnected fleet raises nothing at console level; each offline
// device is its own UNSEEN/STALE row.

// Banner ids of ADR 0013. C04 and C05 are the architecture §4.6 ids; the
// not-checked warning is a variant of C04 with an id that cannot collide with
// the §4.8 test-case numbers.
const (
	BannerEndpointFailing   = "C04"
	BannerCertExpiring      = "C05"
	BannerEndpointUnchecked = "C04b"
)

// EndpointGrace and EndpointMinFailures: C04 is raised once the probe has
// failed at least EndpointMinFailures runs in a row spanning at least
// EndpointGrace, so a single dropped packet or a restart never pages anyone.
// An expired certificate is a fact, not a blip: it skips the grace.
const (
	EndpointGrace       = 5 * time.Minute
	EndpointMinFailures = 2
)

// CertWarnDays and CertCritDays are the C05 thresholds of architecture §4.6
// for the agent listener certificate (warn under 21 days, critical under 7).
const (
	CertWarnDays = 21
	CertCritDays = 7
)

// EndpointStaleAfter is how many probe intervals may pass without a completed
// run before the probe itself is reported as not running (C04b).
const EndpointStaleAfter = 3

func stageText(st probe.Status) string {
	host := st.Host
	if host == "" {
		host = "the public agent address"
	}
	switch st.Last.Stage {
	case probe.StageDNS:
		return host + " does not resolve (DNS)"
	case probe.StageConnect:
		return "nothing accepts connections on " + host + " (connect)"
	case probe.StageTLS:
		switch st.Last.Class {
		case "hostname":
			return "the certificate does not match " + host + " (TLS)"
		case "untrusted":
			return "the certificate of " + host + " is not trusted (TLS)"
		case "pin_mismatch":
			return "the key of " + host + " does not match agent_probe.spki_pin, so pinned agents refuse it (TLS)"
		}
		return "the TLS handshake with " + host + " fails (TLS)"
	case probe.StageCertExpired:
		return "the agent listener certificate has expired"
	case probe.StageResponder:
		return host + " answers, but not as this console's agent API (wrong responder)"
	}
	return "the self-probe of " + host + " fails"
}

func minutes(d time.Duration) int {
	if d < 0 {
		return 0
	}
	return int(d.Minutes())
}

const uncheckedTail = ", so a broken agent listener, certificate or DNS name would go unnoticed."

// EndpointBanners turns a probe Status into the console banners of ADR 0013:
// C04 (critical: the probe fails, naming the stage), C05 (the agent listener
// certificate expires soon) and C04b (warning: the probe cannot run, runs
// without TLS, or has stopped completing).
func EndpointBanners(st probe.Status, now time.Time) []state.Banner {
	var out []state.Banner
	if !st.Runs {
		text := "Agent API not self-checked: public_url is not set" + uncheckedTail + " Set public_url to the address agents use."
		switch st.Limit {
		case probe.LimitCAFile:
			text = "Agent API not self-checked: agent_probe.ca_file cannot be read or holds no certificate" + uncheckedTail
		case probe.LimitSPKIPin:
			text = "Agent API not self-checked: agent_probe.spki_pin is not the base64 of a SHA-256 digest" + uncheckedTail
		}
		return append(out, state.Banner{ID: BannerEndpointUnchecked, Severity: state.SeverityAlarm, Text: text})
	}
	// One C04b at most: a probe that stopped completing outranks the plain-HTTP note.
	ref := st.Started
	if st.Known {
		ref = st.Last.At
	}
	if iv := st.Interval; iv > 0 && !ref.IsZero() && now.Sub(ref) > time.Duration(EndpointStaleAfter)*iv+probe.TotalTimeout {
		out = append(out, state.Banner{ID: BannerEndpointUnchecked, Severity: state.SeverityAlarm, Count: minutes(now.Sub(ref)),
			Text: fmt.Sprintf("Agent API self-check has not completed for %d min"+uncheckedTail, minutes(now.Sub(ref)))})
	} else if st.Limit == probe.LimitPlainHTTP {
		out = append(out, state.Banner{ID: BannerEndpointUnchecked, Severity: state.SeverityAlarm,
			Text: "Agent API checked without TLS: public_url is plain HTTP, so certificate problems go unnoticed. Use https for anything but a lab."})
	}
	if !st.Known {
		return out
	}
	expired := !st.Last.NotAfter.IsZero() && !now.Before(st.Last.NotAfter)
	failing := !st.Last.OK && (st.Last.Stage == probe.StageCertExpired ||
		(st.Failures >= EndpointMinFailures && now.Sub(st.FailingSince) >= EndpointGrace))
	if expired && st.Last.Stage != probe.StageCertExpired {
		// The certificate ran out after the last run: say so now, not at the next run.
		st.Last.OK, st.Last.Stage = false, probe.StageCertExpired
		failing = true
		if st.FailingSince.IsZero() {
			st.FailingSince = st.Last.NotAfter
		}
	}
	if failing {
		n := minutes(now.Sub(st.FailingSince))
		out = append(out, state.Banner{ID: BannerEndpointFailing, Severity: state.SeverityCritical, Count: n,
			Text: fmt.Sprintf("Agent API unreachable: %s. No agent can check in or report. Failing for %d min.", stageText(st), n)})
		return out
	}
	if !st.Last.NotAfter.IsZero() {
		left := st.Last.NotAfter.Sub(now)
		days := int(math.Floor(left.Hours() / 24))
		in := fmt.Sprintf("in %d days", days)
		switch days {
		case 0:
			in = "in less than a day"
		case 1:
			in = "in 1 day"
		}
		const day = 24 * time.Hour
		switch {
		case left < CertCritDays*day:
			out = append(out, state.Banner{ID: BannerCertExpiring, Subject: state.SubjectAgentListener, Severity: state.SeverityCritical, Count: days,
				Text: "Agent listener certificate expires " + in + ". Agents will stop checking in when it does."})
		case left < CertWarnDays*day:
			out = append(out, state.Banner{ID: BannerCertExpiring, Subject: state.SubjectAgentListener, Severity: state.SeverityAlarm, Count: days,
				Text: "Agent listener certificate expires " + in + "."})
		}
	}
	return out
}

// EndpointExamples is one banner of every ADR 0013 kind, for the /design page
// and the banner rendering tests: C04 at each failing stage (including a pin
// mismatch), C05 warn and critical, and the C04b not-checked warning.
func EndpointExamples(now time.Time) []state.Banner {
	base := probe.Status{Runs: true, Host: "agents.example.com:8443", Interval: probe.DefaultInterval, Started: now.Add(-time.Hour), Known: true}
	var out []state.Banner
	for _, s := range []struct {
		stage probe.Stage
		class string
	}{{probe.StageDNS, "nxdomain"}, {probe.StageConnect, "refused"}, {probe.StageTLS, "untrusted"}, {probe.StageTLS, "pin_mismatch"},
		{probe.StageCertExpired, "expired"}, {probe.StageResponder, "status_502"}} {
		st := base
		st.Last = probe.Result{At: now.Add(-time.Minute), Stage: s.stage, Class: s.class}
		st.Failures, st.FailingSince = 4, now.Add(-12*time.Minute)
		out = append(out, EndpointBanners(st, now)...)
	}
	for _, d := range []int{12, 5} {
		st := base
		st.Last = probe.Result{At: now.Add(-time.Minute), OK: true, NotAfter: now.Add(time.Duration(d)*24*time.Hour + time.Hour)}
		out = append(out, EndpointBanners(st, now)...)
	}
	return append(out, EndpointBanners(probe.Status{Limit: probe.LimitNoPublicURL}, now)...)
}
