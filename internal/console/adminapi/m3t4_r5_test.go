package adminapi

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 fix round 5: one rule, and its proof for every row.

var r5Now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func r5Ago(d time.Duration) state.Millis { return ms(r5Now.Add(-d)) }

func r5Row(name, status, class string, ago time.Duration) func(*healthFacts) {
	return func(f *healthFacts) {
		at := r5Now.Add(-ago)
		f.ProbeRows[name] = store.JobState{Name: name, LastStatus: status, LastErrorClass: class, LastFinishedAt: &at}
	}
}

// r5Passing is a console with every check in the build and every attempt passing just now.
func r5Passing() healthFacts {
	ran, next := r5Now.Add(-time.Minute), r5Now.Add(time.Hour)
	f := fullEvidence(r5Now, &ran, &next)
	g, st, sm := *f.Probes.Gateway, *f.Probes.Storage, *f.Probes.SMTP
	f.Probes.Gateway, f.Probes.Storage, f.Probes.SMTP = &g, &st, &sm
	f.Probes.Release = &state.ReleaseFacts{CheckedAt: r5Ago(time.Minute)}
	f.Probes.Quota = &state.QuotaFacts{WithQuota: 1, Targets: []state.TargetUsage{{SoftQuotaBytes: 10, MeasuredAt: r5Ago(time.Hour)}}}
	f.Probes.Digest = &state.DigestFacts{LocalTime: "07:00", LastSentAt: r5Ago(time.Hour)}
	f.Probes.Database = &state.DatabaseFacts{IntegrityCheckedAt: r5Ago(time.Hour), IntegrityOK: true, MeasuredAt: r5Ago(20 * time.Second), SelfBackup: &state.SelfBackupFacts{LastAt: r5Ago(time.Hour)}}
	f.ProbeRows = map[string]store.JobState{}
	return f
}

// r5Run is a prober run that passed this long ago, with a certificate a year out.
func r5Run(ago time.Duration) *state.ProbeRun {
	return &state.ProbeRun{LastRunAt: r5Ago(ago), LastOKAt: r5Ago(ago), CertNotAfter: ms(r5Now.Add(365 * 24 * time.Hour))}
}

// r5Failed is a prober run that failed 10 s ago, with a pass a minute before it.
func r5Failed(class string) *state.ProbeRun {
	return &state.ProbeRun{LastRunAt: r5Ago(10 * time.Second), LastOKAt: r5Ago(70 * time.Second), ConsecutiveFailures: 1, LastErrorClass: class, CertNotAfter: ms(r5Now.Add(365 * 24 * time.Hour))}
}

type r5Kind int

const (
	r5None r5Kind = iota
	r5FailedNewest
	r5SkippedFresh
	r5SkippedStale
	r5Stale
	r5PassedFresh
)

var r5Kinds = map[r5Kind]string{r5None: "no attempt", r5FailedNewest: "failed newest, fresh older pass", r5SkippedFresh: "skipped, fresh pass",
	r5SkippedStale: "skipped, stale pass", r5Stale: "stale", r5PassedFresh: "passed fresh"}

type r5Feed map[r5Kind][]func(*healthFacts)

func set(m ...func(*healthFacts)) []func(*healthFacts) { return m }

func r5ProbeFeed(run func(*healthFacts, *state.ProbeRun), row string) r5Feed {
	on := func(r *state.ProbeRun) func(*healthFacts) { return func(f *healthFacts) { run(f, r) } }
	return r5Feed{
		r5None:         set(on(&state.ProbeRun{}), on(&state.ProbeRun{Unconfigured: "not configured"}), on(nil)),
		r5FailedNewest: set(on(r5Failed("status_502"))),
		r5SkippedFresh: set(func(f *healthFacts) { run(f, r5Run(2*time.Minute)); r5Row(row, "skipped", "busy", 5*time.Second)(f) }),
		r5SkippedStale: set(func(f *healthFacts) { run(f, r5Run(10*time.Minute)); r5Row(row, "skipped", "busy", 5*time.Second)(f) }),
		r5Stale:        set(on(r5Run(time.Hour))),
		r5PassedFresh:  set(on(r5Run(10 * time.Second))),
	}
}

// r5Feeds drives every row into every state, through the facts a real console holds.
func r5Feeds() map[string]r5Feed {
	failedWith := func(st probe.Stage, class string, notAfter time.Time) func(*healthFacts) {
		return func(f *healthFacts) {
			f.Endpoint.Last = probe.Result{At: r5Now.Add(-10 * time.Second), OK: false, Stage: st, Class: class, NotAfter: notAfter}
			f.Endpoint.LastOK = r5Now.Add(-time.Minute)
		}
	}
	year := r5Now.Add(365 * 24 * time.Hour)
	withDB := func(mod func(*state.DatabaseFacts)) func(*healthFacts) {
		return func(f *healthFacts) { d := *f.Probes.Database; mod(&d); f.Probes.Database = &d }
	}
	return map[string]r5Feed{
		"gateway": r5ProbeFeed(func(f *healthFacts, r *state.ProbeRun) { f.Probes.Gateway = r }, jobs.ProbeRowGateway),
		"storage": r5ProbeFeed(func(f *healthFacts, r *state.ProbeRun) { f.Probes.Storage = r }, jobs.ProbeRowStorage),
		"smtp": func() r5Feed {
			feed := r5ProbeFeed(func(f *healthFacts, r *state.ProbeRun) {
				if r == nil {
					f.Probes.SMTP = nil
					return
				}
				f.Probes.SMTP = &state.SMTPFacts{Probe: *r}
			}, jobs.ProbeRowSMTP)
			feed[r5FailedNewest] = append(feed[r5FailedNewest],
				func(f *healthFacts) { f.Probes.SMTP.ConsecutiveSendFailures = 1 },
				func(f *healthFacts) { f.Probes.SMTP.LastTestFailed = true },
				func(f *healthFacts) { f.Probes.SMTP.PendingAlerts = 2 },
				func(f *healthFacts) { // F5: failed and stale, with a send a minute ago
					f.Probes.SMTP = &state.SMTPFacts{Probe: state.ProbeRun{LastRunAt: r5Ago(time.Hour), ConsecutiveFailures: 1}, LastSendOKAt: r5Ago(time.Minute)}
				})
			feed[r5PassedFresh] = append(feed[r5PassedFresh], func(f *healthFacts) { // a stale probe that did not fail, and a recent delivery
				f.Probes.SMTP = &state.SMTPFacts{Probe: *r5Run(time.Hour), LastSendOKAt: r5Ago(time.Hour)}
			})
			feed[r5Stale] = append(feed[r5Stale], func(f *healthFacts) { // a stale probe and a delivery too old to stand in
				f.Probes.SMTP = &state.SMTPFacts{Probe: *r5Run(time.Hour), LastSendOKAt: r5Ago(25 * time.Hour)}
			})
			return feed
		}(),
		"storage_quota": {
			r5None: set(func(f *healthFacts) { f.Probes.Quota = nil }, func(f *healthFacts) {
				f.Probes.Quota = &state.QuotaFacts{WithQuota: 1, Targets: []state.TargetUsage{{SoftQuotaBytes: 10}}}
			}, func(f *healthFacts) {
				f.Probes.Quota = &state.QuotaFacts{WithQuota: 2, Targets: []state.TargetUsage{{SoftQuotaBytes: 10, MeasuredAt: r5Ago(time.Hour)}}}
			}),
			r5Stale: set(func(f *healthFacts) {
				f.Probes.Quota = &state.QuotaFacts{WithQuota: 1, Targets: []state.TargetUsage{{SoftQuotaBytes: 10, MeasuredAt: r5Ago(state.QuotaMeasureMaxAge + time.Hour)}}}
			}),
			r5PassedFresh: set(func(*healthFacts) {}),
		},
		"digest": {
			r5None: set(func(f *healthFacts) { f.Probes.Digest = nil }, func(f *healthFacts) { f.Probes.Digest = &state.DigestFacts{LocalTime: "07:00"} }),
			r5Stale: set(func(f *healthFacts) {
				f.Probes.Digest = &state.DigestFacts{LocalTime: "07:00", LastSentAt: r5Ago(72 * time.Hour)}
			}),
			r5PassedFresh: set(func(*healthFacts) {}),
		},
		"certificates": {
			r5None: set(func(f *healthFacts) { f.Probes.Gateway = &state.ProbeRun{} }, func(f *healthFacts) { f.Endpoint.Known = false },
				func(f *healthFacts) { f.EndpointWired = false },
				func(f *healthFacts) { f.Probes.Gateway = r5Failed("refused") }, // no handshake: the expiry on file is not a reading of now
				failedWith(probe.StageConnect, "refused", time.Time{}), failedWith(probe.StageTLS, "handshake", time.Time{})),
			r5FailedNewest: set(
				func(f *healthFacts) { f.Probes.Gateway = r5Failed("untrusted") }, func(f *healthFacts) { f.Probes.Gateway = r5Failed("expired") },
				func(f *healthFacts) { f.Probes.Gateway.CertNotAfter = r5Ago(time.Second) }, // ran out since its newest passing run
				failedWith(probe.StageCertExpired, "expired", r5Now.Add(-time.Hour)), failedWith(probe.StageCertExpired, "expired", time.Time{}),
				failedWith(probe.StageTLS, "untrusted", year), failedWith(probe.StageTLS, "hostname", year), failedWith(probe.StageTLS, "pin_mismatch", year),
				func(f *healthFacts) { f.Endpoint.Last.NotAfter = r5Now.Add(-time.Second) }),
			r5SkippedFresh: set(func(f *healthFacts) {
				f.Probes.Gateway = r5Run(2 * time.Minute)
				r5Row(jobs.ProbeRowGateway, "skipped", "busy", 5*time.Second)(f)
			}),
			r5SkippedStale: set(func(f *healthFacts) {
				f.Probes.Gateway = r5Run(10 * time.Minute)
				r5Row(jobs.ProbeRowGateway, "skipped", "busy", 5*time.Second)(f)
			}),
			r5Stale:       set(func(f *healthFacts) { f.Probes.Gateway = r5Run(time.Hour) }, func(f *healthFacts) { f.Endpoint.Last.At = r5Now.Add(-time.Hour) }),
			r5PassedFresh: set(func(*healthFacts) {}, failedWith(probe.StageResponder, "status_502", year)), // the certificate was read and accepted: the 502 is agent_api's
		},
		"agent_api": {
			r5None:         set(func(f *healthFacts) { f.EndpointWired = false }, func(f *healthFacts) { f.Endpoint.Runs = false }, func(f *healthFacts) { f.Endpoint.Known = false }),
			r5FailedNewest: set(failedWith(probe.StageResponder, "status_502", year), failedWith(probe.StageConnect, "refused", time.Time{})),
			r5Stale:        set(func(f *healthFacts) { f.Endpoint.Last.At = r5Now.Add(-time.Hour) }),
			r5PassedFresh:  set(func(*healthFacts) {}),
		},
		"maintenance_worker": {
			r5None: set(func(f *healthFacts) {
				f.WorkerPinged, f.WorkerReachable, f.MaintainKnown, f.MaintainReachable = false, false, false, false
			},
				func(f *healthFacts) { f.Probes.Maintain = &state.MaintainFacts{} }),
			r5FailedNewest: set(func(f *healthFacts) { f.WorkerReachable = false },
				func(f *healthFacts) { f.WorkerPinged, f.WorkerReachable, f.MaintainReachable = false, false, false },
				func(f *healthFacts) { f.SchedulerLast.LastStatus = "error" }, func(f *healthFacts) { f.WorkerKeyLoaded = false }),
			r5SkippedFresh: set(func(f *healthFacts) { f.SchedulerLast.LastStatus = "skipped" }),
			r5SkippedStale: set(func(f *healthFacts) {
				f.SchedulerLast.LastStatus = "skipped"
				f.Probes.Maintain = &state.MaintainFacts{Scheduler: &state.JobFacts{LastFinishedAt: r5Ago(state.MaintenanceSchedulerStale + time.Minute)}}
			}),
			r5Stale: set(func(f *healthFacts) {
				f.Probes.Maintain = &state.MaintainFacts{Scheduler: &state.JobFacts{LastFinishedAt: r5Ago(state.MaintenanceSchedulerStale + time.Minute)}}
			}),
			r5PassedFresh: set(func(*healthFacts) {}),
		},
		"database": {
			r5None: set(func(f *healthFacts) { f.Probes.Database = nil }, withDB(func(d *state.DatabaseFacts) { d.IntegrityCheckedAt = 0 }), withDB(func(d *state.DatabaseFacts) { d.MeasuredAt = 0 })),
			r5FailedNewest: set(func(f *healthFacts) { f.SchemaErr = context.DeadlineExceeded }, withDB(func(d *state.DatabaseFacts) { d.IntegrityOK = false }),
				r5Row(jobs.ProbeRowIntegrity, "error", "timeout", 5*time.Second), r5Row(jobs.ProbeRowIntegrity, "error", "integrity_failed", 5*time.Second),
				r5Row(jobs.ProbeRowDatabase, "error", "disk_unmeasured", 5*time.Second), withDB(func(d *state.DatabaseFacts) { d.WorkerStoreFailures = 1 })),
			r5SkippedFresh: set(r5Row(jobs.ProbeRowIntegrity, "skipped", "x", 5*time.Second)),
			r5SkippedStale: set(func(f *healthFacts) {
				withDB(func(d *state.DatabaseFacts) { d.IntegrityCheckedAt = r5Ago(state.IntegrityMaxAge + time.Hour) })(f)
				r5Row(jobs.ProbeRowIntegrity, "skipped", "x", 5*time.Second)(f)
			}),
			r5Stale:       set(withDB(func(d *state.DatabaseFacts) { d.IntegrityCheckedAt = r5Ago(state.IntegrityMaxAge + time.Hour) }), withDB(func(d *state.DatabaseFacts) { d.MeasuredAt = r5Ago(state.DBMeasureMaxAge + time.Minute) })),
			r5PassedFresh: set(func(*healthFacts) {}, r5Row(jobs.ProbeRowIntegrity, "error", "timeout", 2*time.Hour)), // a failure older than the newest pass
		},
		"self_backup": {
			r5None: set(func(f *healthFacts) { f.Probes.Database = nil }, withDB(func(d *state.DatabaseFacts) { d.SelfBackup = &state.SelfBackupFacts{} })),
			r5FailedNewest: set(withDB(func(d *state.DatabaseFacts) {
				d.SelfBackup = &state.SelfBackupFacts{LastAt: r5Ago(time.Hour), VerifyFailed: true}
			})),
			r5Stale: set(withDB(func(d *state.DatabaseFacts) {
				d.SelfBackup = &state.SelfBackupFacts{LastAt: r5Ago(state.DBSelfBackupMaxAge + time.Hour)}
			})),
			r5PassedFresh: set(func(*healthFacts) {}),
		},
		"evaluator": {
			r5None:         set(func(f *healthFacts) { f.Probes.Evaluator = nil }, func(f *healthFacts) { f.Probes.Evaluator = &state.JobFacts{} }),
			r5FailedNewest: set(func(f *healthFacts) { f.EvaluatorLast.LastStatus = "error" }),
			r5SkippedFresh: set(func(f *healthFacts) { f.EvaluatorLast.LastStatus = "skipped" }),
			r5SkippedStale: set(func(f *healthFacts) {
				f.EvaluatorLast.LastStatus = "skipped"
				f.Probes.Evaluator = &state.JobFacts{LastFinishedAt: r5Ago(4 * time.Minute)}
			}),
			r5Stale:       set(func(f *healthFacts) { f.Probes.Evaluator = &state.JobFacts{LastFinishedAt: r5Ago(4 * time.Minute)} }),
			r5PassedFresh: set(func(*healthFacts) {}),
		},
		"master_key": {
			r5None: set(func(f *healthFacts) { f.MasterKeyWired = false }),
			r5FailedNewest: set(func(f *healthFacts) { f.MasterKeyLoaded = false }, func(f *healthFacts) { f.Probes.MasterKey = &state.MasterKeyFacts{} },
				func(f *healthFacts) {
					f.Probes.MasterKey = &state.MasterKeyFacts{Loaded: true, KeyID: "new", EscrowKeyIDs: []string{"new", "old"}}
				}),
			r5Stale:       set(func(f *healthFacts) { f.Probes.MasterKey = nil }), // the prober reports nil when its comparison is no longer current
			r5PassedFresh: set(func(*healthFacts) {}),
		},
		"jobs": {
			r5None: set(func(f *healthFacts) { f.Jobs = nil }, func(f *healthFacts) { f.FirstRunning = []string{"slow"} },
				func(f *healthFacts) {
					f.NeverRun, f.Probes.StartedAt = []string{"x"}, r5Ago(jobsFirstRunGrace-time.Second)
				}),
			r5FailedNewest: set(func(f *healthFacts) { f.Jobs[0].LastStatus = "error" },
				func(f *healthFacts) { f.NeverRun = []string{"x"} }, func(f *healthFacts) {
					f.NeverRun, f.Probes.StartedAt = []string{"x"}, r5Ago(jobsFirstRunGrace+time.Second)
				}),
			r5SkippedStale: set(func(f *healthFacts) { f.Jobs[0].LastStatus = "skipped" }),
			r5Stale: set(func(f *healthFacts) {
				fin, due := r5Now.Add(-time.Hour), r5Now.Add(-time.Hour+5*time.Minute)
				f.Jobs = []store.JobState{{Name: "j", LastStatus: "ok", LastFinishedAt: &fin, NextDueAt: &due}}
			}),
			r5PassedFresh: set(func(*healthFacts) {}),
		},
		"fleet_data": {
			r5FailedNewest: set(func(f *healthFacts) { f.Complete = false }),
			r5PassedFresh:  set(func(*healthFacts) {}),
		},
		"clock": {
			r5FailedNewest: set(func(f *healthFacts) { f.Banners = []state.Banner{{ID: "C13", Severity: state.SeverityAlarm}} }),
			r5PassedFresh:  set(func(*healthFacts) {}),
		},
		"release_manifest": {
			r5None: set(func(f *healthFacts) { f.Probes.Release = nil }, func(f *healthFacts) { f.Probes.Release = &state.ReleaseFacts{} }),
			r5FailedNewest: set(func(f *healthFacts) {
				f.Probes.Release = &state.ReleaseFacts{CheckedAt: r5Ago(time.Minute), Rejected: true}
			}),
			r5Stale: set(func(f *healthFacts) {
				f.Probes.Release = &state.ReleaseFacts{CheckedAt: r5Ago(state.ReleaseCheckMaxAge + time.Minute)}
			}),
			r5PassedFresh: set(func(*healthFacts) {}),
		},
	}
}

// r5NoFact says why a row cannot be driven into a state. The facts it reads cannot express it, and
// the rule is proved for such a state on every row by TestR5TheRuleHoldsForEveryRow. Each entry is
// a statement about the code: the test fails on a state with neither a feed nor a reason here.
var r5NoFact = map[string]map[r5Kind]string{
	"storage_quota":    {r5FailedNewest: "QuotaFacts records measurements, not attempts: a listing that fails ages into stale", r5SkippedFresh: "same", r5SkippedStale: "same"},
	"digest":           {r5FailedNewest: "DigestFacts records sends, not attempts: C14 names a missed send", r5SkippedFresh: "same", r5SkippedStale: "same"},
	"agent_api":        {r5SkippedFresh: "a self-probe run cut short is not recorded, and it has no other non-result", r5SkippedStale: "same"},
	"self_backup":      {r5SkippedFresh: "SelfBackupFacts records backups, not attempts", r5SkippedStale: "same"},
	"master_key":       {r5SkippedFresh: "the comparison either happened or the prober reports nil", r5SkippedStale: "same"},
	"jobs":             {r5SkippedFresh: "job_state holds one status per job, so a skipped run has no earlier pass on record: it reads not_checked"},
	"fleet_data":       {r5None: "the load is this request's own", r5SkippedFresh: "same", r5SkippedStale: "same", r5Stale: "same, and C10 names a stale view"},
	"clock":            {r5None: "too few reporting devices is not_applicable: TestR4ClockRowAtTheEnginesBoundary", r5SkippedFresh: "the engine judges this request's load", r5SkippedStale: "same", r5Stale: "same"},
	"release_manifest": {r5SkippedFresh: "ReleaseFacts records checks, not attempts", r5SkippedStale: "same"},
}

func r5Want(k r5Kind, status string) bool {
	switch k {
	case r5None, r5SkippedStale, r5Stale:
		return status == statusNotChecked
	case r5FailedNewest:
		return status == statusWarn || status == statusFailed
	}
	return status == statusOK
}

// The property: every row, fed every state through its real facts, obeys the rule, and the POST
// check of the same id says the same. A row or a state with no case fails.
func TestR5EveryRowObeysTheOneRule(t *testing.T) {
	for id, c := range rowsOf(r5Passing()) {
		if c.Status != statusOK {
			t.Fatalf("control: with every attempt passing, %s = %+v", id, c)
		}
	}
	feeds := r5Feeds()
	for _, row := range healthRowOrder {
		if _, ok := rowAttempts[row.id]; !ok {
			t.Errorf("row %s has no attempts", row.id)
		}
		feed, ok := feeds[row.id]
		if !ok {
			t.Errorf("row %s has no case", row.id)
			continue
		}
		for k, name := range r5Kinds {
			muts := feed[k]
			if len(muts) == 0 {
				if r5NoFact[row.id][k] == "" {
					t.Errorf("row %s has no case for %q", row.id, name)
				}
				continue
			}
			for i, mut := range muts {
				f := r5Passing()
				mut(&f)
				got := rowsOf(f)[row.id]
				if !r5Want(k, got.Status) {
					t.Errorf("%s, %s, case %d: %s %q", row.id, name, i, got.Status, got.Detail)
				}
				if got.Status != statusOK && got.Detail == "" && len(got.Banners) == 0 {
					t.Errorf("%s, %s, case %d: %s with no reason", row.id, name, i, got.Status)
				}
				if slices.Contains([]string{"gateway", "storage", "certificates"}, row.id) {
					for _, c := range probeChecks(f) {
						if c.ID == row.id && (c.Status != got.Status || c.Detail != got.Detail) {
							t.Errorf("%s, %s, case %d: POST says %s %q, the row says %s %q", row.id, name, i, c.Status, c.Detail, got.Status, got.Detail)
						}
					}
				}
			}
		}
		for k := range r5NoFact[row.id] {
			if len(feed[k]) != 0 {
				t.Errorf("row %s has both a case and a no-fact reason for %q", row.id, r5Kinds[k])
			}
		}
	}
	for id := range feeds {
		if _, ok := rowAttempts[id]; !ok {
			t.Errorf("a case for %s, which is not a row", id)
		}
	}
}

// The rule itself, for every row: whatever a row supplies, only these outcomes exist.
func TestR5TheRuleHoldsForEveryRow(t *testing.T) {
	pass, old := r5Now.Add(-time.Minute), r5Now.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		a    attempt
		want []string
	}{
		{"no attempt", attempt{Why: "x"}, []string{statusNotChecked}},
		{"failed, fresh older pass", attempt{Result: failed, At: pass, LastPass: pass, MaxAge: time.Hour, Why: "x"}, []string{statusWarn}},
		{"failed, severe", attempt{Result: failed, At: pass, LastPass: pass, MaxAge: time.Hour, Severe: true, Why: "x"}, []string{statusFailed}},
		{"failed, stale", attempt{Result: failed, At: old, LastPass: old, MaxAge: time.Minute, Why: "x"}, []string{statusWarn}},
		{"skipped, fresh pass", attempt{Result: skipped, At: pass, LastPass: pass, MaxAge: 2 * time.Minute, Why: "x"}, []string{statusOK}},
		{"skipped, stale pass", attempt{Result: skipped, At: pass, LastPass: old, MaxAge: 2 * time.Minute, Why: "x"}, []string{statusNotChecked}},
		{"skipped, no pass", attempt{Result: skipped, At: pass, Why: "x"}, []string{statusNotChecked}},
		{"stale", attempt{Result: passed, At: old, MaxAge: time.Minute}, []string{statusNotChecked}},
		{"passed fresh", attempt{Result: passed, At: pass, MaxAge: time.Hour}, []string{statusOK}},
		{"passed, this request's own read", attempt{Result: passed, At: r5Now}, []string{statusOK}},
		{"passed, before the request", attempt{Result: passed, At: pass, MaxAge: time.Hour, NotBefore: r5Now}, []string{statusNotChecked}},
		{"failed, before the request", attempt{Result: failed, At: pass, NotBefore: r5Now, Severe: true}, []string{statusNotChecked}},
		{"skipped during the request, pass before it", attempt{Result: skipped, At: r5Now, LastPass: pass, MaxAge: time.Hour, NotBefore: r5Now}, []string{statusNotChecked}},
		{"not in this build", attempt{Outside: statusNotInBuild, Why: "x"}, []string{statusNotInBuild}},
	} {
		status, _ := tc.a.judge(r5Now)
		if !slices.Contains(tc.want, status) {
			t.Errorf("%s: %s, want %v", tc.name, status, tc.want)
		}
		// A row of this one attempt: a banner can only make it worse, and an ok row needs no banner.
		for _, banner := range []string{statusNotChecked, statusWarn, statusFailed} {
			got, _, _ := judgeRow(r5Now, []attempt{tc.a}, banner, false)
			if statusRank[got] < statusRank[banner] || got == statusOK {
				t.Errorf("%s under a %s banner: %s", tc.name, banner, got)
			}
			if statusRank[status] > statusRank[banner] && got != status {
				t.Errorf("%s under a weaker %s banner: softened to %s", tc.name, banner, got)
			}
		}
		if got, _, _ := judgeRow(r5Now, []attempt{{Result: passed, At: pass}, tc.a}, "", false); got != status && statusRank[status] > 0 {
			t.Errorf("%s beside a passing attempt: %s, want %s", tc.name, got, status)
		}
		if got, _, _ := judgeRow(r5Now, []attempt{tc.a}, "", true); got == statusOK {
			t.Errorf("%s with C00 up reads ok", tc.name)
		}
	}
	if got, _, _ := judgeRow(r5Now, nil, "", false); got != statusNotChecked {
		t.Errorf("a row with no attempts: %s", got)
	}
}

// The structure: only the rule names a status of health. A row that decides for itself fails here.
func TestR5OnlyTheRuleNamesAStatus(t *testing.T) {
	allowed := map[string]bool{"judge": true, "judgeRow": true, "worstOf": true, "overallOf": true,
		"storageTargetProbe": true} // a probe that could not run answers not_checked before there is an attempt to judge
	named := map[string]bool{"statusOK": true, "statusWarn": true, "statusFailed": true, "statusNotChecked": true}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "m3_system.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || allowed[fn.Name.Name] {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && named[id.Name] {
				t.Errorf("%s: %s names %s: rows supply attempts, and only judge decides", fset.Position(id.Pos()), fn.Name.Name, id.Name)
			}
			return true
		})
	}
}

// F1: both halves of the certificates row use C05's own thresholds, whichever banner wins.
func TestR5TheCertificatesRowAgreesWithC05AtEveryThreshold(t *testing.T) { // r5 F1, C05
	statusOf := func(bs []state.Banner, c05Subject string) string {
		for _, b := range bs {
			if b.ID == "C05" && b.Subject == c05Subject {
				if b.Severity >= state.SeverityCritical {
					return statusFailed
				}
				return statusWarn
			}
		}
		return statusOK
	}
	const day = 24 * time.Hour
	for _, left := range []time.Duration{time.Hour, 3 * day, 7*day - time.Minute, 7*day + time.Minute, 21*day - time.Minute, 21*day + time.Minute, 90 * day} {
		notAfter := r5Now.Add(left)
		// The agent half against the real EndpointBanners, on a healthy probe, where C05 is not preempted.
		st := probe.Status{Runs: true, Known: true, Interval: time.Minute, Started: r5Now.Add(-time.Hour), LastOK: r5Now.Add(-10 * time.Second),
			Last: probe.Result{At: r5Now.Add(-10 * time.Second), OK: true, NotAfter: notAfter}}
		want := statusOf(health.EndpointBanners(st, r5Now), string(state.SubjectAgentListener))
		f := r5Passing()
		f.Endpoint = st
		if got := rowsOf(f)["certificates"].Status; got != want {
			t.Errorf("agent listener, %v left: row %s, C05 says %s", left, got, want)
		}
		// The same under C04: a responder failing past the grace, where EndpointBanners returns before C05.
		f = r5Passing()
		f.Endpoint = st
		f.Endpoint.Last.OK, f.Endpoint.Last.Stage, f.Endpoint.Last.Class = false, probe.StageResponder, "status_502"
		f.Endpoint.Failures, f.Endpoint.FailingSince = 9, r5Now.Add(-time.Hour)
		if got := rowsOf(f)["certificates"].Status; got != want {
			t.Errorf("agent listener under C04, %v left: row %s, C05 says %s", left, got, want)
		}
		// The gateway half against the real engine.
		run := *r5Run(10 * time.Second)
		run.CertNotAfter = ms(notAfter)
		w := state.World{LoadComplete: true, LoadedAt: r5Now, Probes: state.ProbeFacts{Wired: true, Gateway: &run}}
		wantGW := statusOf(state.Evaluate(w, r5Now).Banners, string(state.SubjectGateway))
		f = r5Passing()
		f.Probes.Gateway = &run
		if got := rowsOf(f)["certificates"].Status; got != wantGW {
			t.Errorf("gateway, %v left: row %s, C05 says %s", left, got, wantGW)
		}
		if want != wantGW {
			t.Errorf("%v left: the agent half says %s and the gateway half %s", left, want, wantGW)
		}
		// The gateway answering 502, which the row reads the same way.
		failing := run
		failing.LastOKAt, failing.ConsecutiveFailures, failing.LastErrorClass = r5Ago(time.Hour), 9, "status_502"
		f = r5Passing()
		f.Probes.Gateway = &failing
		if got := rowsOf(f)["certificates"].Status; got != wantGW {
			t.Errorf("gateway answering 502, %v left: row %s, C05 says %s", left, got, wantGW)
		}
	}
}
