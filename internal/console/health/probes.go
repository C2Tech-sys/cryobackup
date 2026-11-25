package health

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The World's probe facts are assembled here, for serve (architecture §4.6):
// the health-prober supplies the probe-derived checks (Evaluator.Probes), the
// job_state rows the job heartbeats (alarm-evaluator C06, maintenance-scheduler
// C07), the page the view (C10), and the checks whose producer is a later
// milestone their NotInBuild marker, so no pointer is ever left nil and a fully
// wired console can read all clear (internal/state doc.go "Not in this build").
// The mail checks (C03, C14) come from the prober's delivery-log reads: a digest
// check it could not supply stays nil, which the engine reports as not checked.
// The self-backup (C11) comes from the maintenance worker's settings rows and
// its db-self-backup job row (selfBackupFacts).

// Check names of the NotInBuild markers (the facts type each one sits on).
// CheckSelfBackup is no longer marked (P22-F); the name stays so a guard can
// still look for a stray marker.
const (
	CheckQuota      = "quota"       // state.QuotaFacts
	CheckSelfBackup = "self-backup" // state.SelfBackupFacts
	CheckRelease    = "release"     // state.ReleaseFacts
)

// NotInBuild maps every check this build supplies with its NotInBuild marker to
// the job that will produce it (and its milestone, in internal/state doc.go).
// cli's TestNotInBuildMarksOnlyUnbuiltJobs fails if any of these jobs is
// registered while its check is still marked.
var NotInBuild = map[string]string{
	CheckQuota:   "usage-sampler", // M5, the provider-side listing
	CheckRelease: "release-check", // M11
	// The digest (M4) has its daily-digest job since P20-W2B: its facts come from the prober
	// (store.MailFacts) and carry no marker. The self-backup (db-self-backup, written by the
	// maintenance worker, ADR 0029) has been read from its settings rows since P22-F
	// (selfBackupFacts) and carries no marker either.
}

// SelfBackupJob is the maintenance worker's db-self-backup job row: its class
// when the newest run failed reaches C11's text (not_configured, no_slot, ...).
const SelfBackupJob = maintain.SelfBackupJob

// selfBackupReadTimeout bounds the one settings read of selfBackupFacts (this
// path takes no request context).
const selfBackupReadTimeout = 5 * time.Second

// selfBackupFacts is C11's self-backup picture: the worker's settings rows
// (store.SelfBackupFacts: the newest backup, the first, the newest verify
// verdict and the newest attempt) as plain values, and the class of the
// db-self-backup job row when its newest run ended in error. A settings read
// that fails is nil, which C11 reports as "self-backup not supplied", never as
// healthy; a job_state read that failed (jerr) only leaves the class out.
func (e *Evaluator) selfBackupFacts(jobs []store.JobState, jerr error) *state.SelfBackupFacts {
	if e.Store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), selfBackupReadTimeout)
	defer cancel()
	f, err := e.Store.SelfBackupFacts(ctx)
	if err != nil {
		return nil
	}
	out := f.SelfBackupFacts // LastAt, VerifyFailed; NotInBuild is false
	out.NotInBuild = false
	out.FirstBackupAt, out.VerifyAt = f.FirstBackupAt, f.VerifyAt
	out.VerifyAttemptAt, out.VerifyAttemptClass = f.VerifyAttemptAt, f.VerifyAttemptClass
	if jerr == nil {
		for _, j := range jobs {
			if j.Name == SelfBackupJob && j.LastStatus == "error" {
				out.JobClass = j.LastErrorClass
			}
		}
	}
	return &out
}

// SchedulerJob is the job_state row of the maintenance scheduler (written by
// `cryo-console maintain`, read here for C07).
const SchedulerJob = maintain.SchedulerJob

func millis(t *time.Time) state.Millis {
	if t == nil || t.IsZero() {
		return 0
	}
	return state.Millis(t.UnixMilli())
}

// jobFacts is a job's heartbeat from its job_state row. A missing row is a job
// that never finished (judged against ProbeFacts.StartedAt); a store error is
// nil, which the engine reports as "not supplied".
func jobFacts(jobs []store.JobState, jerr error, name string, period time.Duration) *state.JobFacts {
	if jerr != nil {
		return nil
	}
	j := &state.JobFacts{Period: period}
	for _, r := range jobs {
		if r.Name == name {
			j.LastFinishedAt = millis(r.LastFinishedAt)
		}
	}
	return j
}

// withRetired adds the retired devices to the prober's measurement of the login file.
// What cannot be read is NotRead (C17b), never "nothing is retired".
func (e *Evaluator) withRetired(ctx context.Context, f state.LoginFileFacts) state.LoginFileFacts {
	total, garbled, err := e.Store.RetiredDeviceCounts(ctx)
	if err != nil {
		f.NotRead = "the retired devices could not be read"
		return f
	}
	f.Retired, f.RetiredGarbled = total, garbled
	return f
}

// ProbeFacts assembles the World's probe facts. Without a probe source
// (Evaluator.Probes nil: unit tests, or a caller that does not run the prober)
// it returns the zero value, which is not wired (C00). page says whether a
// page or API response will show the view: the page path always sets View
// (C10), a page-less caller (the alarm evaluator) never does.
func (e *Evaluator) ProbeFacts(jobs []store.JobState, jerr error, page bool) state.ProbeFacts {
	if e.Probes == nil {
		return state.ProbeFacts{}
	}
	p := e.Probes()
	if p.Evaluator == nil {
		p.Evaluator = jobFacts(jobs, jerr, EvaluatorOKJob, EvaluatorPeriod) // the newest successful evaluation
	}
	if p.Maintain == nil {
		p.Maintain = &state.MaintainFacts{}
	}
	if p.Maintain.Scheduler == nil {
		p.Maintain.Scheduler = jobFacts(jobs, jerr, SchedulerJob, maintain.DefaultTickEvery)
	}
	if p.Database != nil && p.Database.SelfBackup == nil {
		d := *p.Database
		d.SelfBackup = e.selfBackupFacts(jobs, jerr) // nil when unreadable: a C11 cause
		p.Database = &d
	}
	if p.Quota == nil {
		p.Quota = &state.QuotaFacts{NotInBuild: true}
	}
	if p.Release == nil {
		p.Release = &state.ReleaseFacts{NotInBuild: true}
	}
	if p.LoginFile != nil {
		f := e.withRetired(context.Background(), *p.LoginFile) // two counts; this path takes no request context
		p.LoginFile = &f
	}
	if page && p.View == nil {
		v := &state.ViewFacts{RefreshInterval: state.DefaultRefreshInterval}
		if e.Refresh != nil {
			if r := e.Refresh(); r > 0 {
				v.RefreshInterval = r
			}
		}
		p.View = v
	}
	return p
}

// FixtureProbes is fixtures mode's probe source: every check wired and
// passing at the frozen clock, the gateway certificate a year out, so the
// fixture board reads exactly the banners its fleet earns (the /design page
// shows every probe banner from internal/console/fixtures).
func FixtureProbes(clock func() time.Time) func() state.ProbeFacts {
	return func() state.ProbeFacts {
		now := clock().UTC()
		at := func(d time.Duration) state.Millis { return state.Millis(now.Add(-d).UnixMilli()) }
		ok := func() state.ProbeRun {
			return state.ProbeRun{LastRunAt: at(20 * time.Second), LastOKAt: at(20 * time.Second)}
		}
		gw := ok()
		gw.CertNotAfter = state.Millis(now.Add(365 * 24 * time.Hour).UnixMilli())
		st := ok()
		return state.ProbeFacts{
			Wired: true, StartedAt: at(24 * time.Hour), ProberInterval: state.DefaultProberInterval,
			Gateway: &gw, Storage: &st,
			SMTP: &state.SMTPFacts{Probe: ok()},
			// The day's digest went out an hour ago: the newest due date's digest whatever the clock.
			Digest:    &state.DigestFacts{LocalTime: "07:00", LastSentAt: at(time.Hour)},
			Evaluator: &state.JobFacts{LastFinishedAt: at(30 * time.Second), Period: EvaluatorPeriod},
			Maintain:  &state.MaintainFacts{RPC: ok(), Scheduler: &state.JobFacts{LastFinishedAt: at(time.Minute), Period: maintain.DefaultTickEvery}},
			Database: &state.DatabaseFacts{IntegrityCheckedAt: at(time.Hour), IntegrityOK: true, MeasuredAt: at(20 * time.Second),
				FreeBytes: 80 << 30, TotalBytes: 100 << 30, WALBytes: 4 << 20,
				// The self-backup wrote two hours ago and was verified two days ago (ADR 0029).
				SelfBackup: &state.SelfBackupFacts{LastAt: at(2 * time.Hour), FirstBackupAt: at(90 * 24 * time.Hour),
					VerifyAt: at(48 * time.Hour), VerifyAttemptAt: at(48 * time.Hour)}},
			MasterKey: &state.MasterKeyFacts{Loaded: true, KeyID: "fixtures"},
			LoginFile: &state.LoginFileFacts{MeasuredAt: at(20 * time.Second)},
		}
	}
}
