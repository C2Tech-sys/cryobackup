package health

import (
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

// Check names of the NotInBuild markers (the facts type each one sits on).
const (
	CheckQuota      = "quota"       // state.QuotaFacts
	CheckSelfBackup = "self-backup" // state.SelfBackupFacts
	CheckDigest     = "digest"      // state.DigestFacts
	CheckRelease    = "release"     // state.ReleaseFacts
)

// NotInBuild maps every check this build supplies with its NotInBuild marker to
// the job that will produce it (and its milestone, in internal/state doc.go).
// cli's TestNotInBuildMarksOnlyUnbuiltJobs fails if any of these jobs is
// registered while its check is still marked.
var NotInBuild = map[string]string{
	CheckQuota:      "usage-sampler",  // M5, the provider-side listing
	CheckSelfBackup: "db-self-backup", // M5
	CheckDigest:     "daily-digest",   // M4
	CheckRelease:    "release-check",  // M11
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
		d.SelfBackup = &state.SelfBackupFacts{NotInBuild: true}
		p.Database = &d
	}
	if p.Digest == nil {
		p.Digest = &state.DigestFacts{NotInBuild: true}
	}
	if p.Quota == nil {
		p.Quota = &state.QuotaFacts{NotInBuild: true}
	}
	if p.Release == nil {
		p.Release = &state.ReleaseFacts{NotInBuild: true}
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
			SMTP:      &state.SMTPFacts{Probe: ok()},
			Evaluator: &state.JobFacts{LastFinishedAt: at(30 * time.Second), Period: EvaluatorPeriod},
			Maintain:  &state.MaintainFacts{RPC: ok(), Scheduler: &state.JobFacts{LastFinishedAt: at(time.Minute), Period: maintain.DefaultTickEvery}},
			Database: &state.DatabaseFacts{IntegrityCheckedAt: at(time.Hour), IntegrityOK: true, MeasuredAt: at(20 * time.Second),
				FreeBytes: 80 << 30, TotalBytes: 100 << 30, WALBytes: 4 << 20},
			MasterKey: &state.MasterKeyFacts{Loaded: true, KeyID: "fixtures"},
		}
	}
}
