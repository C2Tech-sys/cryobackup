package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4: what fixtures mode needs so that the monitoring pages show every state
// it can show with no real console behind it: every console banner on request,
// a run of every outcome, a customer whose devices went silent together, and a
// job table whose "Run now" can be pressed.

// ---- every banner, on request ----

var allBanners atomic.Bool

// SetAllBanners turns the "show every console banner" view on or off. It is a
// process-wide switch of fixtures mode (the fleet is fictional, so a banner shown
// here claims nothing about a real console): ui exposes it at /fixtures and wires
// ExtraBanners into the evaluator, so every page and the system health rows see
// the same banners.
func SetAllBanners(on bool) { allBanners.Store(on) }

// AllBannersOn reports whether the switch is on.
func AllBannersOn() bool { return allBanners.Load() }

// ExtraBanners is the evaluator's Extra hook: nothing while the switch is off,
// every banner of AllBanners while it is on.
func ExtraBanners(now time.Time) []state.Banner {
	if !allBanners.Load() {
		return nil
	}
	return AllBanners(now)
}

// AllBanners is every condition the console can raise: C01 to C16 (C11 at its
// critical severity), C00 (the self-checks are not wired) and every "b"
// not-checked warning. Timestamps are relative to now.
func AllBanners(now time.Time) []state.Banner {
	out := Banners(now)
	for i := range out {
		if out[i].ID == "C11" {
			out[i].Severity = state.SeverityCritical
			out[i].Text = "Console database needs attention: the maintenance worker cannot read its database."
		}
	}
	warn := state.SeverityAlarm
	out = append(out, state.Banner{ID: "C00", Severity: warn, Text: "Console self-checks are not wired: gateway, storage, e-mail and job health are not being monitored."})
	for _, b := range []struct{ id, what string }{{"C01b", "Gateway"}, {"C02b", "Storage"}, {"C03b", "Alert e-mail"}, {"C04b", "Agent API"}, {"C05b", "Certificate"},
		{"C06b", "Alerting"}, {"C07b", "Maintenance worker"}, {"C12b", "Master key"}, {"C14b", "Digest"}, {"C15b", "Storage quota"}, {"C16b", "Release manifest"}} {
		out = append(out, state.Banner{ID: b.id, Severity: warn, Text: b.what + " has not been checked: a problem here would go unnoticed."})
	}
	return out
}

// ---- the healthy console, on request ----

var healthyView atomic.Bool

// SetHealthyView turns the "show a healthy console" view of the System page on or off. Like
// SetAllBanners it is a process-wide switch of fixtures mode, exposed at /fixtures. The fictional
// fleet raises real conditions and has more than five devices, so overall OK and a not_applicable
// row can be seen no other way. ui reads it only in fixtures mode.
func SetHealthyView(on bool) { healthyView.Store(on) }

// HealthyViewOn reports whether the switch is on.
func HealthyViewOn() bool { return healthyView.Load() }

// ---- jobs ----

// JobNames are the fictional background jobs of fixtures mode. The pages' job runner for them is
// ui's (fixtures cannot import jobs: jobs' own tests import fixtures).
var JobNames = []string{"alarm-evaluator", "command-expiry", "heartbeat-ring-trim", "htpasswd-sync", "restore-purge", "session-expire"}

// The jobs SeedM3 puts in each of the job table's other states. JobLeftover is not in JobNames: it is a
// row a job left behind when it stopped being registered.
const (
	JobLeftover = "abandoned-nightly-export"
	JobSkipped  = "heartbeat-ring-trim"
	JobRunning  = "command-expiry"
	JobOverdue  = "htpasswd-sync"
)

// ---- data ----

// SeedM3 adds the M3 monitoring data to a database LoadFixtures filled: one run
// of every outcome (cancelled, skipped, interrupted and abandoned are the ones
// LoadFixtures has none of), a retried run, job results, and the silence of
// Harbor Family Practice: all six of its devices have not been heard from for ten
// days, and the engine collapses five of them into one customer-wide row. The sixth,
// NEW-EXAM-6, is PENDING_FIRST: inside its first-backup grace the engine suppresses
// UNSEEN, so it is not in that set. Call it once, right after LoadFixtures; a second
// call fails on the run ids.
func SeedM3(ctx context.Context, st *store.Store, now time.Time) error {
	now = now.UTC()
	devs := map[string]string{}
	l, err := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 500}})
	if err != nil {
		return err
	}
	for _, d := range l.Items {
		devs[d.Hostname] = d.ID
	}
	h := time.Hour
	n := 0
	seed := func(host, outcome, kind string, started, took time.Duration, mod func(*store.RunInput)) error {
		n++
		id, ok := devs[host]
		if !ok {
			return errors.New("fixtures: no device " + host)
		}
		in := store.RunInput{ID: runID(n), Kind: kind, Trigger: "schedule", StartedAt: now.Add(-started), Outcome: outcome,
			Stats: json.RawMessage(`{"total_files_processed":1200,"failed_files":0,"files_new":14}`)}
		if outcome != "" {
			fin := now.Add(-started + took)
			in.FinishedAt = &fin
		}
		if mod != nil {
			mod(&in)
		}
		res, err := st.AgentIngestRuns(ctx, id, []store.RunInput{in}, now)
		if err != nil {
			return err
		}
		if res[0].Status != store.IngestCreated {
			return errors.New("fixtures: run not created: " + res[0].Status + " " + res[0].Reason)
		}
		return nil
	}
	for _, r := range []struct {
		host, outcome, kind string
		started, took       time.Duration
		mod                 func(*store.RunInput)
	}{
		{"LT-OPTOM-04", "cancelled", "backup", 17 * h, 3 * time.Minute, nil},
		{"OPS-ND-01", "skipped", "backup", 16 * h, time.Second, nil},
		{"LEGAL-PC-01", "interrupted", "backup", 14 * h, 5 * time.Minute, nil},
		{"LEGAL-PC-02", "abandoned", "backup", 13 * h, 6 * time.Minute, func(r *store.RunInput) { r.ErrorClass = "network" }},
		{"FRONTDESK-01", "failed", "backup", 9 * h, 2 * time.Minute, func(r *store.RunInput) {
			r.ErrorClass, r.ErrorCounts, r.Attempt = "vss", map[string]int{"vss": 3, "disk": 1}, 2
		}},
		{"RECEPTION-2", "partial", "backup", 11 * h, 9 * time.Minute, func(r *store.RunInput) {
			r.ErrorClass, r.ErrorCounts = "permission", map[string]int{"permission": 23, "locked": 4}
			r.Stats = json.RawMessage(`{"total_files_processed":2000,"failed_files":27}`)
		}},
		{"PRINT-SRV-01", "success", "check", 8 * h, 4 * time.Minute, nil},
	} {
		if err := seed(r.host, r.outcome, r.kind, r.started, r.took, r.mod); err != nil {
			return err
		}
	}
	// job results: most ok, one that failed last time, one that has never run (no row)
	for i, name := range JobNames[:len(JobNames)-1] {
		fin := now.Add(-time.Duration(2+i) * time.Minute)
		next := fin.Add(15 * time.Minute)
		rec := store.JobRun{Status: "ok", StartedAt: fin.Add(-800 * time.Millisecond), FinishedAt: fin, NextDueAt: &next}
		switch name {
		case "restore-purge":
			rec.Status, rec.ErrorClass = "error", "storage"
		case JobSkipped:
			rec.Status = "skipped"
		case JobOverdue:
			long := now.Add(-2 * h)
			due := long.Add(15 * time.Minute)
			rec.StartedAt, rec.FinishedAt, rec.NextDueAt = long.Add(-800*time.Millisecond), long, &due
		}
		if err := st.RecordJobRun(ctx, name, rec); err != nil {
			return err
		}
	}
	// A run in progress: the lease is held, for longer than this fictional console will ever run.
	if _, err := st.AcquireJobLease(ctx, JobRunning, "fixtures-lab", 10*365*24*h); err != nil {
		return err
	}
	// A row left behind by a job that is no longer registered.
	gone, goneDue := now.Add(-30*24*h), now.Add(-30*24*h+24*h)
	if err := st.RecordJobRun(ctx, JobLeftover, store.JobRun{Status: "ok", StartedAt: gone.Add(-time.Second), FinishedAt: gone, NextDueAt: &goneDue}); err != nil {
		return err
	}
	// Harbor Family Practice: silent together
	old := now.Add(-10 * 24 * h).UnixMilli()
	return st.Write(ctx, func(tx *store.Tx) error {
		for _, host := range []string{"NEW-EXAM-6", "LT-BILLING-1", "WAITING-RM-PC", "FRONTDESK-H1", "EXAM-H2", "BILLING-H3"} {
			if _, err := tx.Exec(`UPDATE device_presence SET received_at = ? WHERE device_id = ?`, old, devs[host]); err != nil {
				return err
			}
		}
		return nil
	})
}

// runID makes the n-th seeded run's 26-character id (Crockford base 32).
func runID(n int) string {
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := []byte("01J9Y4K3F0X0B1C2D3E4F5G6H7")
	for i := len(b) - 1; n > 0 && i > len(b)-4; i-- {
		b[i] = alpha[n%32]
		n /= 32
	}
	return string(b)
}
