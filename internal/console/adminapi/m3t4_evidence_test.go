package adminapi

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 delta, items 1 and 8: a system health row reaches ok only on evidence
// that is specific to that row.

func ms(t time.Time) state.Millis { return state.Millis(t.UnixMilli()) }

// fullEvidence is a console with every piece of evidence and no banner: every
// probe ran 20 s ago and passed, both listeners' certificates were read, the
// release check ran, the worker answered, the evaluator and a job ran.
func fullEvidence(now time.Time, ran, next *time.Time) healthFacts {
	ok := func() state.ProbeRun {
		return state.ProbeRun{LastRunAt: ms(now.Add(-20 * time.Second)), LastOKAt: ms(now.Add(-20 * time.Second))}
	}
	gw, st := ok(), ok()
	gw.CertNotAfter = ms(now.Add(90 * 24 * time.Hour))
	return healthFacts{Now: now, Complete: true, EndpointWired: true, WorkerPinged: true, WorkerReachable: true, MaintainKnown: true, MaintainReachable: true,
		EvaluatorRanAt: ran, MasterKeyWired: true, ReportingDevices: 9,
		Jobs: []store.JobState{{Name: "j", LastStatus: "ok", NextDueAt: next}},
		Probes: state.ProbeFacts{Wired: true, Gateway: &gw, Storage: &st, SMTP: &state.SMTPFacts{Probe: ok()},
			Release: &state.ReleaseFacts{CheckedAt: ms(now.Add(-time.Minute))}},
		Endpoint: probe.Status{Runs: true, Known: true, Interval: time.Minute, Last: probe.Result{At: now.Add(-20 * time.Second), OK: true, NotAfter: now.Add(60 * 24 * time.Hour)}},
	}
}

func rowsOf(f healthFacts) map[string]SystemComponentJSON {
	out := map[string]SystemComponentJSON{}
	for _, c := range buildComponents(f) {
		out[c.ID] = c
	}
	return out
}

func TestSystemHealthNeverSaysOKWithoutFacts(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	// A console that knows nothing: no row may read ok, except the ones a read in this very request proves
	// (the database answered; the facts load was complete).
	for id, c := range rowsOf(healthFacts{Now: now, Complete: true}) {
		switch id {
		case "database", "fleet_data":
			continue
		}
		if c.Status == statusOK {
			t.Errorf("with no facts, %s = %+v", id, c)
		}
	}
	// Probe facts that say "wired" and nothing else are not evidence either.
	for id, c := range rowsOf(healthFacts{Now: now, Complete: true, EndpointWired: true, Probes: state.ProbeFacts{Wired: true}}) {
		switch id {
		case "database", "fleet_data":
			continue
		}
		if c.Status == statusOK {
			t.Errorf("with empty probe facts, %s = %+v", id, c)
		}
	}
}

func TestSystemHealthEachRowNeedsItsOwnEvidence(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ran, next := now.Add(-time.Minute), now.Add(time.Hour)
	for id, c := range rowsOf(fullEvidence(now, &ran, &next)) {
		if c.Status != statusOK {
			t.Fatalf("control: with every piece of evidence %s = %+v", id, c)
		}
	}
	long := func(d time.Duration) state.Millis { return ms(now.Add(-d)) }
	type mut = func(*healthFacts)
	for _, tc := range []struct {
		row  string
		want string
		why  string
		mut  mut
	}{
		// gateway
		{"gateway", statusNotChecked, "probe never ran", func(f *healthFacts) { f.Probes.Gateway = &state.ProbeRun{} }},
		{"gateway", statusNotChecked, "probe not supplied", func(f *healthFacts) { f.Probes.Gateway = nil }},
		{"gateway", statusNotChecked, "probe unconfigured", func(f *healthFacts) { f.Probes.Gateway = &state.ProbeRun{Unconfigured: "gateway_url is not set"} }},
		{"gateway", statusNotChecked, "probe stale", func(f *healthFacts) {
			f.Probes.Gateway = &state.ProbeRun{LastRunAt: long(time.Hour), LastOKAt: long(time.Hour)}
		}},
		{"gateway", statusWarn, "latest probe failing (before C01's streak and grace)", func(f *healthFacts) {
			f.Probes.Gateway = &state.ProbeRun{LastRunAt: long(10 * time.Second), LastOKAt: long(2 * time.Minute), ConsecutiveFailures: 1, LastErrorClass: "nxdomain"}
		}},
		{"gateway", statusNotChecked, "probe facts not wired", func(f *healthFacts) { f.Probes.Wired = false }},
		// storage
		{"storage", statusNotChecked, "probe never ran", func(f *healthFacts) { f.Probes.Storage = &state.ProbeRun{} }},
		{"storage", statusWarn, "latest probe failing", func(f *healthFacts) {
			f.Probes.Storage = &state.ProbeRun{LastRunAt: long(10 * time.Second), ConsecutiveFailures: 1, LastErrorClass: "storage_unreachable"}
		}},
		// alert e-mail
		{"smtp", statusNotChecked, "no probe", func(f *healthFacts) { f.Probes.SMTP = nil }},
		{"smtp", statusNotChecked, "unconfigured", func(f *healthFacts) {
			f.Probes.SMTP = &state.SMTPFacts{Probe: state.ProbeRun{Unconfigured: "SMTP is not configured"}}
		}},
		{"smtp", statusWarn, "probe failing", func(f *healthFacts) {
			f.Probes.SMTP = &state.SMTPFacts{Probe: state.ProbeRun{LastRunAt: long(10 * time.Second), ConsecutiveFailures: 2}}
		}},
		{"smtp", statusWarn, "the latest send failed", func(f *healthFacts) { f.Probes.SMTP.ConsecutiveSendFailures = 1 }},
		// certificates: an actual expiry from both listeners
		{"certificates", statusNotChecked, "the gateway never answered a probe (no certificate was read)", func(f *healthFacts) {
			f.Probes.Gateway = &state.ProbeRun{LastRunAt: long(10 * time.Second), ConsecutiveFailures: 3, LastErrorClass: "nxdomain"}
		}},
		{"certificates", statusNotChecked, "the gateway read no expiry", func(f *healthFacts) { f.Probes.Gateway.CertNotAfter = 0 }},
		{"certificates", statusNotChecked, "the gateway's latest probe failed", func(f *healthFacts) {
			f.Probes.Gateway.ConsecutiveFailures, f.Probes.Gateway.LastOKAt = 1, long(5*time.Minute)
		}},
		{"certificates", statusNotChecked, "the agent listener read no expiry (plain HTTP)", func(f *healthFacts) { f.Endpoint.Last.NotAfter = time.Time{} }},
		{"certificates", statusNotChecked, "the agent self-probe never ran", func(f *healthFacts) { f.Endpoint.Known = false }},
		{"certificates", statusNotChecked, "the agent self-probe is stale", func(f *healthFacts) { f.Endpoint.Last.At = now.Add(-time.Hour) }},
		// agent endpoint
		{"agent_api", statusNotChecked, "not wired", func(f *healthFacts) { f.EndpointWired = false }},
		{"agent_api", statusNotChecked, "the probe cannot run", func(f *healthFacts) { f.Endpoint = probe.Status{Limit: "no_public_url"} }},
		{"agent_api", statusNotChecked, "no run yet", func(f *healthFacts) { f.Endpoint.Known = false }},
		{"agent_api", statusNotChecked, "stale", func(f *healthFacts) { f.Endpoint.Last.At = now.Add(-time.Hour) }},
		{"agent_api", statusWarn, "the latest self-probe failing (NXDOMAIN), before the streak and grace raise C04", func(f *healthFacts) {
			f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: false, Class: "nxdomain"}
			f.Endpoint.Failures = 1
		}},
		// release manifest
		{"release_manifest", statusNotChecked, "release-check is not in the build", func(f *healthFacts) { f.Probes.Release = &state.ReleaseFacts{NotInBuild: true} }},
		{"release_manifest", statusNotChecked, "not in the build, whatever CheckedAt says", func(f *healthFacts) {
			f.Probes.Release = &state.ReleaseFacts{NotInBuild: true, CheckedAt: ms(now.Add(-time.Minute))}
		}},
		{"release_manifest", statusNotChecked, "no release facts", func(f *healthFacts) { f.Probes.Release = nil }},
		{"release_manifest", statusNotChecked, "release-check never ran", func(f *healthFacts) { f.Probes.Release = &state.ReleaseFacts{} }},
		// worker
		{"maintenance_worker", statusNotChecked, "never probed", func(f *healthFacts) {
			f.WorkerPinged, f.WorkerReachable, f.MaintainKnown, f.MaintainReachable = false, false, false, false
		}},
		{"maintenance_worker", statusWarn, "the periodic probe did not reach it", func(f *healthFacts) {
			f.WorkerPinged, f.WorkerReachable, f.MaintainReachable = false, false, false
		}},
		{"maintenance_worker", statusFailed, "the ping failed", func(f *healthFacts) { f.WorkerReachable = false }},
		// the rest
		{"evaluator", statusNotChecked, "never ran", func(f *healthFacts) { f.EvaluatorRanAt = nil }},
		{"master_key", statusNotChecked, "not reported", func(f *healthFacts) { f.MasterKeyWired = false }},
		{"clock", statusNotChecked, "too few devices", func(f *healthFacts) { f.ReportingDevices = 4 }},
		{"jobs", statusNotChecked, "no job ran", func(f *healthFacts) { f.Jobs = nil }},
		{"database", statusFailed, "no answer", func(f *healthFacts) { f.SchemaErr = context.DeadlineExceeded }},
		{"fleet_data", statusWarn, "incomplete load", func(f *healthFacts) { f.Complete = false }},
	} {
		f := fullEvidence(now, &ran, &next)
		g, st, sm := *f.Probes.Gateway, *f.Probes.Storage, *f.Probes.SMTP
		f.Probes.Gateway, f.Probes.Storage, f.Probes.SMTP = &g, &st, &sm // the mutation must not reach the control
		tc.mut(&f)
		got := rowsOf(f)[tc.row]
		if got.Status != tc.want {
			t.Errorf("%s (%s) = %s %q, want %s", tc.row, tc.why, got.Status, got.Detail, tc.want)
		}
		if tc.want != statusOK && got.Detail == "" {
			t.Errorf("%s (%s): %s without a stated reason", tc.row, tc.why, got.Status)
		}
	}
}

// The scenario of the delta report: gateway and agent host do not resolve, a few
// minutes in. No row speaks of health, certificates and the manifest are not
// checked, and the gateway and agent rows are warnings before C01 and C04 exist.
func TestSystemHealthWithTheGatewayAndAgentHostDownAfterTwelveMinutes(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ran, next := now.Add(-time.Minute), now.Add(time.Hour)
	f := fullEvidence(now, &ran, &next)
	f.Probes.Gateway = &state.ProbeRun{LastRunAt: ms(now.Add(-10 * time.Second)), ConsecutiveFailures: 12, LastErrorClass: "nxdomain"}
	f.Probes.Release = &state.ReleaseFacts{NotInBuild: true}
	f.Endpoint = probe.Status{Runs: true, Known: true, Interval: time.Minute, Failures: 12, FailingSince: now.Add(-12 * time.Minute),
		Last: probe.Result{At: now.Add(-10 * time.Second), OK: false, Class: "nxdomain"}}
	rows := rowsOf(f)
	for id, want := range map[string]string{"gateway": statusWarn, "agent_api": statusWarn, "certificates": statusNotChecked, "release_manifest": statusNotChecked} {
		if rows[id].Status != want {
			t.Errorf("%s = %+v, want %s", id, rows[id], want)
		}
	}
}

// Item 8: the prober's rows and the evaluator's marker row are not jobs, in the
// health row and in the table alike.
func TestSystemJobsRowAndTableAgreeOnWhatAJobIs(t *testing.T) {
	for name, want := range map[string]bool{"alarm-evaluator": true, "session-expire": true, health.EvaluatorOKJob: false, "health-prober.probes": false, "health-prober.gateway": false} {
		if isJob(name) != want {
			t.Errorf("isJob(%q) = %v, want %v", name, !want, want)
		}
	}
	e := newEnv(t)
	owner := e.login("admin", pw)
	ctx := context.Background()
	now := e.clk.Now()
	good := now.Add(-time.Minute)
	for _, n := range []string{"session-expire", health.EvaluatorOKJob, "health-prober.gateway"} {
		if err := e.st.Write(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO job_state (name, last_status, last_started_at, last_finished_at, run_count, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)`,
				n, map[bool]string{true: "error", false: "ok"}[n == health.EvaluatorOKJob], good.UnixMilli(), good.UnixMilli(), now.UnixMilli(), now.UnixMilli())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The marker row says "error"; it must not make "Background jobs" a warning while every real job is ok.
	h := decode[SystemHealthResponse](t, mustGet(e, "/api/v1/system/health", owner))
	for _, c := range h.Components {
		if c.ID == "jobs" && c.Status == statusWarn {
			t.Errorf("the jobs row = %+v although every job row is ok (the marker row is not a job)", c)
		}
	}
	jl := decode[JobListResponse](t, mustGet(e, "/api/v1/system/jobs", owner))
	for _, j := range jl.Jobs {
		if !isJob(j.Name) {
			t.Errorf("the job table lists %q, which is not a job", j.Name)
		}
	}
}
