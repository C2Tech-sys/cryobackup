package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/agent/service"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// RunSummary is the newest backup run, path-free.
type RunSummary struct {
	ID             string
	Trigger        string
	StartedAt      time.Time
	FinishedAt     time.Time // zero while open
	Outcome        string    // empty while open
	ErrorClass     string
	DeferredReason string
	Attempt        int
	Delivered      bool
}

// Report is the device state "cryo-agent status" prints. It contains no path,
// no file name, no user-profile name and no secret (architecture §6.1
// "diagnostics", DH15): the local logs are deliberately not part of it.
type Report struct {
	AgentVersion string
	Enrolled     bool
	DeviceID     string
	Server       string
	Custody      string
	InsecureLab  bool

	ServiceState string // the Windows service state, or "unsupported"

	// StateVerified: every layout directory reads back as config.SDDLStateDir (owner
	// SYSTEM or Administrators, protected SYSTEM + Administrators DACL).
	StateVerified bool
	// InsecureState is non-empty when the agent last started with
	// --insecure-unprotected-state (the warning it logged; no path).
	InsecureState string
	// Untrusted: the untrusted-repair-required verdict exists
	// (config.ReadVerdict: HKLM\SOFTWARE\CryoBackup\Agent on Windows). An
	// elevated start found the state directory untrusted; the agent refuses it
	// until an elevated --repair-acl quarantines it. UntrustedSince and
	// UntrustedReason are what the verdict says (path-free).
	Untrusted       bool
	UntrustedSince  time.Time
	UntrustedReason string
	// VerdictErr is non-empty when the verdict could not be read: the agent
	// refuses such a state directory, and status says so.
	VerdictErr string
	// VerdictChecked names the state directory whose verdict was read,
	// without its path (there is one verdict per state directory):
	// config.VerdictLocation, e.g.
	// HKLM\SOFTWARE\CryoBackup\Agent\UntrustedRepairRequired-<id>.
	// DefaultStateDir: that directory is the default one (no --state-dir).
	VerdictChecked  string
	DefaultStateDir bool
	// Quarantines counts the quarantined state directories next to the state
	// directory (moved aside by --repair-acl, kept for review);
	// NewestQuarantine is the newest one's "<UTC stamp>-<8 hex>" (no path).
	Quarantines      int
	NewestQuarantine string

	LastHeartbeat    time.Time // zero = none yet
	ClockOffsetMS    int64
	HaveClockOffset  bool
	PolicyVersion    int
	PolicyETag       string
	Paused           bool
	HaveJournal      bool
	LastRun          *RunSummary
	LastSuccess      time.Time
	NextDue          time.Time // earliest start ignoring startup delay and jitter; zero = unknown
	NextDueReason    string
	QueueDepth       int
	OpenRuns         int
	LastErrorClass   string
	ReportedAtDevice time.Time

	// PlannedAt is the running scheduler's planned start (journal
	// KVPlannedStart), which does count the startup delay, the jitter and
	// the backoff; zero when the agent has no plan in the future (not
	// running, a backup in progress, nothing to schedule). It wins over
	// NextDue when set (L-STATUS-1).
	PlannedAt     time.Time
	PlannedReason string

	// TokenRefusedSince is when the console first refused the device token
	// (the first of three heartbeats answered 401; journal KVTokenRefusedSince);
	// zero = the agent does not believe the token is refused (ADR 0018).
	TokenRefusedSince time.Time
}

// readVerdict is config.ReadVerdict (replaced by this package's tests: the
// real one reads HKLM on Windows).
var readVerdict = config.ReadVerdict

// Status reads agent.json and the journal of the state directory (read-only,
// without touching a running service) and builds the Report as of now.
func Status(ctx context.Context, stateDir string, now time.Time) (Report, error) {
	if stateDir == "" {
		stateDir = config.DefaultStateDir()
	}
	layout := config.Layout{Root: stateDir}
	r := Report{AgentVersion: api.VersionLabel(buildinfo.Version), ServiceState: service.State(), ReportedAtDevice: now}
	// Read before agent.json: after a quarantine the device is not enrolled.
	r.VerdictChecked = config.VerdictLocation(layout.Root)
	r.DefaultStateDir = config.VerdictID(layout.Root) == config.VerdictID(config.DefaultStateDir())
	switch v, present, err := readVerdict(layout.Root); {
	case err != nil:
		r.VerdictErr = err.Error()
	case present:
		r.Untrusted, r.UntrustedSince, r.UntrustedReason = true, v.Since, v.Reason
	}
	if q := layout.Quarantines(); len(q) > 0 {
		r.Quarantines, r.NewestQuarantine = len(q), q[len(q)-1]
	}

	cfg, err := config.Load(layout.ConfigFile())
	switch {
	case errors.Is(err, config.ErrNotFound):
		return r, nil
	case err != nil:
		return r, err
	}
	r.Enrolled, r.DeviceID, r.Server, r.Custody, r.InsecureLab = cfg.Enrolled(), cfg.DeviceID, cfg.ServerURL, cfg.Custody, cfg.InsecureLab
	r.StateVerified = true
	for _, d := range layout.All() {
		r.StateVerified = r.StateVerified && config.VerifyDir(d) == nil
	}

	if _, err := os.Stat(layout.JournalFile()); err != nil {
		return r, nil
	}
	jr, err := journal.Open(ctx, journal.Options{Path: layout.JournalFile(), ReadOnly: true})
	if err != nil {
		return r, fmt.Errorf("diag: open journal: %w", err)
	}
	defer jr.Close()
	r.HaveJournal = true

	if v, ok, _ := jr.KVGet(ctx, journal.KVLastHeartbeat); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			r.LastHeartbeat = t
		}
	}
	if v, ok, _ := jr.KVGet(ctx, journal.KVClockOffsetMS); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			r.ClockOffsetMS, r.HaveClockOffset = n, true
		}
	}
	r.LastErrorClass, _, _ = jr.KVGet(ctx, journal.KVLastErrorClass)
	r.InsecureState, _, _ = jr.KVGet(ctx, journal.KVInsecureState)
	if v, ok, _ := jr.KVGet(ctx, journal.KVTokenRefusedSince); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			r.TokenRefusedSince = t
		}
	}
	r.PolicyETag, _, _ = jr.KVGet(ctx, journal.KVPolicyETag)
	r.QueueDepth, _ = jr.OutboxDepth(ctx)
	r.OpenRuns, _ = jr.OpenRunCount(ctx)

	var pol policy.Policy
	havePolicy := false
	if raw, ok, _ := jr.KVGet(ctx, journal.KVPolicyJSON); ok {
		var resp api.PolicyResponse
		if json.Unmarshal([]byte(raw), &resp) == nil {
			pol, havePolicy = resp.Policy, true
			r.PolicyVersion, r.Paused = resp.Version, resp.Paused
		}
	}

	rows, err := jr.RecentRuns(ctx, restic.KindBackup, 100)
	if err != nil {
		return r, err
	}
	if len(rows) > 0 {
		last := rows[0]
		s := &RunSummary{
			ID: last.ID, Trigger: string(last.Trigger), StartedAt: last.StartedAt, Outcome: string(last.Outcome),
			ErrorClass: string(last.ErrorClass), DeferredReason: string(last.DeferredReason), Attempt: last.Attempt,
			Delivered: last.DeliveredAt != nil,
		}
		if last.FinishedAt != nil {
			s.FinishedAt = *last.FinishedAt
		}
		r.LastRun = s
	}
	if havePolicy {
		f := scheduler.FactsFromRuns(rows, pol)
		r.LastSuccess = f.LastSuccess
		r.NextDue, r.NextDueReason = nextDue(now, f, pol, cfg.DeviceID)
		if raw, ok, _ := jr.KVGet(ctx, journal.KVPlannedStart); ok {
			var ps journal.PlannedStart
			if json.Unmarshal([]byte(raw), &ps) == nil && ps.At.After(now) {
				r.PlannedAt, r.PlannedReason = ps.At, ps.Reason
			}
		}
	}
	return r, nil
}

// nextDue is the earliest time a backup could start by the schedule alone: it
// ignores the startup delay and the per-slot jitter, which only the running
// agent knows.
func nextDue(now time.Time, f scheduler.Facts, pol policy.Policy, deviceID string) (time.Time, string) {
	if f.Retry.Pending && f.Retry.NotBefore.After(now) {
		return f.Retry.NotBefore, scheduler.ReasonRetryWait
	}
	if f.Retry.Pending {
		return now, scheduler.ReasonRetry
	}
	dec := policy.Due(policy.DueInput{
		Now: now.In(time.Local), LastSuccess: f.LastSuccess, LastAttemptStart: f.LastAttemptStart,
		Schedule: pol.Schedule, DeviceID: deviceID,
	})
	if dec.Due {
		return now, dec.Reason
	}
	return dec.NotBefore, dec.Reason
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// renderTokenRefused prints the block for a refused device token, in plain
// words, with what to do. No URL, no path.
func (r Report) renderTokenRefused(w io.Writer) {
	_, _ = fmt.Fprintf(w, "TOKEN REFUSED since %s (%s)\n", r.TokenRefusedSince.UTC().Format("2006-01-02 15:04 MST"), refusedFor(r.ReportedAtDevice.Sub(r.TokenRefusedSince)))
	for _, l := range []string{
		"  The console answers 401 to this device's token: the device was retired or",
		"  revoked, the console was restored from before this enrollment, or this",
		"  agent points at the wrong console. Backups keep running on the last policy",
		"  and run records queue locally. The agent asks the console once an hour.",
		"  To fix: re-enroll this device with a new enrollment token, or uninstall.",
	} {
		_, _ = fmt.Fprintln(w, l)
	}
}

// refusedFor words a duration in days, hours and minutes ("3 h 12 min"); under
// a minute, or a clock that ran backwards, is "under 1 min".
func refusedFor(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "under 1 min"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 24*60:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	return fmt.Sprintf("%d d %d h", m/(24*60), m%(24*60)/60)
}

// Render writes the report as "key: value" lines.
func (r Report) Render(w io.Writer) {
	p := func(k, v string) { _, _ = fmt.Fprintf(w, "%-18s %s\n", k+":", v) }
	p("agent", r.AgentVersion)
	// The verdict's registry location only when there is one (or it cannot
	// be read): with no verdict the location only looked like one (L-STATUS-1).
	switch {
	case r.Untrusted || r.VerdictErr != "":
		which := "the --state-dir given"
		if r.DefaultStateDir {
			which = "the default state directory"
		}
		p("state verdict", r.VerdictChecked+" ("+which+")")
	default:
		p("state verdict", "none")
	}
	switch {
	case r.VerdictErr != "":
		p("state trust", "UNTRUSTED (the "+config.VerdictSource+" cannot be read: "+r.VerdictErr+"); the agent refuses the state directory until it can be read")
	case r.Untrusted:
		v := config.Verdict{Since: r.UntrustedSince, Reason: r.UntrustedReason}
		p("state trust", "UNTRUSTED ("+v.Describe()+"); repair required: the agent refuses the state directory; run --repair-acl (install, enroll or run) from an elevated prompt, then enroll again")
	}
	if r.Quarantines > 0 {
		p("quarantine", fmt.Sprintf("%d quarantined state director%s next to the state directory (newest %s); review, then delete", r.Quarantines, plural(r.Quarantines, "y", "ies"), r.NewestQuarantine))
	}
	if !r.Enrolled {
		p("enrolled", "no (run \"cryo-agent enroll\")")
		p("service", r.ServiceState)
		return
	}
	p("enrolled", "yes")
	p("device id", r.DeviceID)
	p("console", r.Server)
	if r.Custody != "" {
		p("key custody", r.Custody)
	}
	if r.InsecureLab {
		p("mode", "INSECURE LAB (http allowed, TLS not verified)")
	}
	if !r.TokenRefusedSince.IsZero() {
		r.renderTokenRefused(w)
	}
	if r.StateVerified {
		p("state acl", "verified (owner SYSTEM or Administrators, SYSTEM + Administrators only)")
	} else {
		p("state acl", "NOT VERIFIED (not restricted to SYSTEM + Administrators, or not readable by this process)")
	}
	if r.InsecureState != "" {
		p("insecure state", "the agent last started with --insecure-unprotected-state (lab only)")
	}
	p("service", r.ServiceState)
	if !r.HaveJournal {
		p("journal", "none yet")
		return
	}
	if r.LastHeartbeat.IsZero() {
		p("last heartbeat", "never")
	} else {
		p("last heartbeat", fmt.Sprintf("%s (%s ago)", r.LastHeartbeat.UTC().Format(time.RFC3339), r.ReportedAtDevice.Sub(r.LastHeartbeat).Round(time.Second)))
	}
	if r.HaveClockOffset {
		p("clock offset", fmt.Sprintf("%d ms (device minus console)", r.ClockOffsetMS))
	}
	pol := fmt.Sprintf("version %d", r.PolicyVersion)
	if r.PolicyVersion == 0 {
		pol = "none yet"
	}
	if r.Paused {
		pol += ", paused"
	}
	p("policy", pol)
	if r.PolicyETag != "" {
		p("policy etag", r.PolicyETag)
	}
	if r.LastRun == nil {
		p("last run", "none yet")
	} else {
		l := r.LastRun
		state := l.Outcome
		if state == "" {
			state = "running"
		}
		if l.ErrorClass != "" {
			state += " (" + l.ErrorClass + ")"
		}
		if l.DeferredReason != "" {
			state += " (deferred: " + l.DeferredReason + ")"
		}
		p("last run", fmt.Sprintf("%s, started %s, attempt %d, trigger %s", state, l.StartedAt.UTC().Format(time.RFC3339), l.Attempt, l.Trigger))
	}
	if r.LastSuccess.IsZero() {
		p("last success", "never")
	} else {
		p("last success", r.LastSuccess.UTC().Format(time.RFC3339))
	}
	switch {
	case !r.PlannedAt.IsZero():
		p("next due", fmt.Sprintf("%s (%s, as the running agent plans it)", r.PlannedAt.UTC().Format(time.RFC3339), r.PlannedReason))
	case r.NextDue.IsZero():
		p("next due", "unknown")
	case !r.NextDue.After(r.ReportedAtDevice):
		p("next due", "now ("+r.NextDueReason+")")
	default:
		p("next due", fmt.Sprintf("%s (%s)", r.NextDue.UTC().Format(time.RFC3339), r.NextDueReason))
	}
	p("upload queue", strconv.Itoa(r.QueueDepth)+" undelivered, "+strconv.Itoa(r.OpenRuns)+" open")
	if r.LastErrorClass != "" {
		p("last error class", r.LastErrorClass)
	}
}
