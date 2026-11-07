package state

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Thresholds of the probe-driven console conditions (architecture §4.6). They
// are the spec's proposed values, kept as named constants so a reader can find
// them; none is operator-specific.
const (
	// DefaultProberInterval is the health-prober period (§5.7) used when ProbeFacts.ProberInterval is zero.
	DefaultProberInterval = 60 * time.Second
	// ProbeStaleIntervals: a probe whose newest run is older than this many intervals is "not checked".
	ProbeStaleIntervals = 3

	// GatewayFailuresToAlarm raises C01 (about 3 min at the default interval).
	GatewayFailuresToAlarm = 3
	// StorageFailuresToAlarm raises C02.
	StorageFailuresToAlarm = 2
	// WorkerStoreFailuresToCritical raises C11 critical: the worker's own database reads failing on this
	// many storage probes in a row. The same rule as storage, so one transient failure does not page.
	WorkerStoreFailuresToCritical = StorageFailuresToAlarm
	// SMTPFailuresToAlarm raises C03 for failed sends or failed probes in a row.
	SMTPFailuresToAlarm = 3
	// SMTPPendingWithoutSend raises C03 when alerts are pending and nothing was delivered for this long.
	SMTPPendingWithoutSend = 24 * time.Hour

	// CertWarnDays and CertCritDays are the C05 thresholds (warn under 21 days, critical under 7).
	CertWarnDays = 21
	CertCritDays = 7

	// DefaultEvaluatorPeriod is the alarm-evaluator period (§5.7); C06 fires after EvaluatorStalledPeriods of them.
	DefaultEvaluatorPeriod  = 60 * time.Second
	EvaluatorStalledPeriods = 3

	// MaintenanceSchedulerStale raises C07 when the maintenance scheduler has not finished for this long.
	MaintenanceSchedulerStale = 30 * time.Minute

	// DefaultRefreshInterval is the page refresh interval (the design's fallback poll) used when
	// ViewFacts.RefreshInterval is zero; C10 fires after StaleViewIntervals of them.
	DefaultRefreshInterval = 30 * time.Second
	StaleViewIntervals     = 2

	// DBFreeDiskPct, DBWALLimit and DBSelfBackupMaxAge are the C11 thresholds.
	DBFreeDiskPct      = 10
	DBWALLimit         = 1 << 30
	DBSelfBackupMaxAge = 26 * time.Hour

	// DigestGrace raises C14 when no digest was sent by digest_local_time plus this.
	DigestGrace = 30 * time.Minute

	// QuotaWarnPct, QuotaCritPct and QuotaProjectionDays are the C15 thresholds.
	QuotaWarnPct        = 80
	QuotaCritPct        = 90
	QuotaProjectionDays = 14

	// FutureSkewAllowed: a recorded check time more than this ahead of now is garbled ("not checked").
	FutureSkewAllowed = 5 * time.Minute
	// IntegrityMaxAge: integrity_check runs at startup and weekly; older than a week and a day is a C11 cause.
	IntegrityMaxAge = 8 * 24 * time.Hour
	// DBMeasureMaxAge: the prober measures free disk and WAL each run (60 s); older than 15 min is a C11 cause.
	DBMeasureMaxAge = 15 * time.Minute
	// QuotaMeasureMaxAge: a target's usage against its soft quota comes from usage-sampler's provider-side
	// listing (M5), which is daily; two listings plus two hours of slack. Older usage is C15b. (The hourly
	// per-device restic stats are repository sizes, not what the provider bills against the quota.)
	QuotaMeasureMaxAge = 50 * time.Hour
	// ReleaseCheckMaxAge: release-check runs every 15 min; three periods without a run is C16b.
	ReleaseCheckMaxAge = 45 * time.Minute
)

// bannerCtx is what every banner rule needs to word its text.
type bannerCtx struct {
	now time.Time
	loc *time.Location
}

// clock renders t for banner text in the display zone: "3:04 PM" on today's
// local date, "Mon 2 Jan 3:04 PM" on any other (a bare clock time from
// yesterday would read as today).
func (c bannerCtx) clock(t time.Time) string {
	lt, ln := t.In(c.loc), c.now.In(c.loc)
	if ly, lm, ld := lt.Date(); ly == ln.Year() && lm == ln.Month() && ld == ln.Day() {
		return lt.Format("3:04 PM")
	}
	return lt.Format("Mon 2 Jan 3:04 PM")
}

func minutesOf(d time.Duration) int {
	if d < 0 {
		return 0
	}
	return int(d.Minutes())
}

// probeVerdict is what one ProbeRun says.
type probeVerdict int

const (
	probeOK        probeVerdict = iota
	probeFailing                // enough failures in a row: the condition holds
	probeUnchecked              // never completed, or stopped completing: "not checked"
)

// recorded parses a time the console recorded about its own checks. ok is
// false for NULL, for a garbled value, and for a time more than
// FutureSkewAllowed ahead of now: a check that claims to have run in the future
// proves nothing, so it is "not checked", never healthy.
func recorded(m Millis, now time.Time) (time.Time, bool) {
	t, ok := m.Time()
	if !ok || t.After(now.Add(FutureSkewAllowed)) {
		return time.Time{}, false
	}
	return t, true
}

// garbledWhy is the not-checked reason for a present but unusable timestamp.
const garbledWhy = " time is garbled or in the future"

// judgeProbe reads a ProbeRun. A failure streak at the threshold is the
// condition even when the prober has since stopped (the last evidence is a
// failure); otherwise a probe that never completed, whose newest run time is
// garbled or in the future, or that has not completed for ProbeStaleIntervals
// intervals is unchecked, never healthy, and so is a probe that cannot run
// (Unconfigured) or whose failure count is negative (garbled). why and n word
// the not-checked text.
func judgeProbe(p *ProbeRun, failAfter int, interval time.Duration, now time.Time) (v probeVerdict, why string, n int) {
	switch {
	case p.Unconfigured != "":
		return probeUnchecked, p.Unconfigured, 0
	case p.ConsecutiveFailures < 0:
		return probeUnchecked, "its failure count is garbled", 0
	}
	if p.ConsecutiveFailures >= failAfter {
		return probeFailing, "", 0
	}
	last, ok := recorded(p.LastRunAt, now)
	switch {
	case p.LastRunAt == 0:
		return probeUnchecked, "the health prober has not completed a probe of it yet", 0
	case !ok:
		return probeUnchecked, "its newest probe" + garbledWhy, 0
	}
	if age := now.Sub(last); age > time.Duration(ProbeStaleIntervals)*interval {
		n = minutesOf(age)
		return probeUnchecked, "no probe of it has completed for " + strconv.Itoa(n) + " min", n
	}
	return probeOK, "", 0
}

// notChecked is the "not checked" warning of a probe-driven condition.
func notChecked(id, subject, consequence, why string, n int) Banner {
	return Banner{ID: id, Severity: SeverityAlarm, Count: n, Text: subject + " not checked: " + why + ", so " + consequence + "."}
}

// lastOK is the " Last OK h:mm PM." tail of a failing-probe banner.
func (c bannerCtx) lastOK(m Millis, never string) (string, time.Time) {
	if t, ok := recorded(m, c.now); ok {
		return " Last OK " + c.clock(t) + ".", t
	}
	return " " + never, time.Time{}
}

// probeBanners derives every probe-driven §4.6 banner from w.Probes. A nil
// check raises nothing (doc.go "Probe banners"). The device-derived banners
// (C01 from mass gateway deferral, C13) are added by Evaluate.
func probeBanners(w World, now time.Time) []Banner {
	p := &w.Probes
	c := bannerCtx{now: now, loc: w.DisplayZone}
	if c.loc == nil {
		c.loc = time.UTC
	}
	iv := p.ProberInterval
	if iv <= 0 {
		iv = DefaultProberInterval
	}
	var out []Banner
	if !p.Wired {
		out = append(out, Banner{ID: "C00", Severity: SeverityAlarm,
			Text: "Console self-checks are not wired: gateway, storage, e-mail and job health are not being monitored."})
	}
	// Each check: present, its rule; absent while Wired, its not-checked warning.
	type check struct {
		present bool
		run     func() []Banner
		absent  Banner
	}
	checks := []check{
		{p.Gateway != nil, func() []Banner { return gatewayBanners(p.Gateway, iv, c) },
			unsupplied("C01b", "Gateway", "an unreachable gateway would go unnoticed")},
		{p.Storage != nil, func() []Banner { return storageBanners(p.Storage, iv, c) },
			unsupplied("C02b", "Storage", "storage that cannot be reached from the admin path would go unnoticed")},
		{p.SMTP != nil, func() []Banner { return smtpBanners(p.SMTP, iv, c) },
			unsupplied("C03b", "Alert e-mail", "alert e-mail that cannot be delivered would go unnoticed")},
		{p.Evaluator != nil, func() []Banner { return evaluatorBanners(p.Evaluator, p.StartedAt, c) },
			unsupplied("C06b", "Alerting", "stalled alerting would go unnoticed")},
		{p.Maintain != nil, func() []Banner { return maintainBanners(p.Maintain, p.StartedAt, iv, c) },
			unsupplied("C07b", "Maintenance worker", "a stopped maintenance worker would go unnoticed")},
		{p.Database != nil, func() []Banner { return databaseBanners(p.Database, c) },
			Banner{ID: "C11", Severity: SeverityAlarm, Count: 1, Text: "Console database needs attention: database health is not measured."}},
		{p.MasterKey != nil, func() []Banner { return masterKeyBanners(p.MasterKey) },
			unsupplied("C12b", "Master key", "an unavailable master key would go unnoticed")},
		{p.Digest != nil, func() []Banner { return digestBanners(p.Digest, c) },
			unsupplied("C14b", "Daily digest", "a digest that is never sent would go unnoticed")},
		{p.Quota != nil, func() []Banner { return quotaBanners(p.Quota, c) },
			unsupplied("C15b", "Storage quota", "a full quota would go unnoticed")},
		{p.Release != nil, func() []Banner { return releaseBanners(p.Release, c) },
			unsupplied("C16b", "Release manifests", "a rejected manifest would go unnoticed")},
		{p.LoginSync != nil, func() []Banner { return loginSyncBanners(p.LoginSync, c) },
			unsupplied("C17b", "Gateway login removal", "a retired login left in the gateway file would go unnoticed")},
	}
	for _, k := range checks {
		switch {
		case k.present:
			out = append(out, k.run()...)
		case p.Wired:
			out = append(out, k.absent)
		}
	}
	// View describes the page rendering this view; a caller with no page has none.
	if p.View != nil {
		out = append(out, viewBanners(p.View, w.LoadedAt, c)...)
	}
	return out
}

// unsupplied is the not-checked warning of a check the loader did not supply although it is Wired.
func unsupplied(id, subject, consequence string) Banner {
	return Banner{ID: id, Severity: SeverityAlarm,
		Text: subject + " not checked: the console does not supply this check, so " + consequence + "."}
}

// storageBanners is C02 from the storage probe through the maintain RPC.
func storageBanners(s *ProbeRun, iv time.Duration, c bannerCtx) []Banner {
	switch v, why, n := judgeProbe(s, StorageFailuresToAlarm, iv, c.now); v {
	case probeFailing:
		tail, at := c.lastOK(s.LastOKAt, "It has never answered a probe.")
		return []Banner{{ID: "C02", Severity: SeverityCritical, Count: s.ConsecutiveFailures, At: at,
			Text: "Storage unreachable from the admin path: retention, restore and usage are impaired." + tail}}
	case probeUnchecked:
		return []Banner{notChecked("C02b", "Storage", "storage that cannot be reached from the admin path would go unnoticed", why, n)}
	}
	return nil
}

// releaseBanners is C16; C16b when release-check never ran, its time is
// garbled or in the future, or it has not run for ReleaseCheckMaxAge.
func releaseBanners(r *ReleaseFacts, c bannerCtx) []Banner {
	if r.NotInBuild {
		return nil
	}
	if r.Rejected {
		return []Banner{{ID: "C16", Severity: SeverityAlarm, Text: "Release manifest rejected."}}
	}
	at, ok := recorded(r.CheckedAt, c.now)
	why := ""
	n := 0
	switch {
	case r.CheckedAt == 0:
		why = "release-check has not run"
	case !ok:
		why = "the newest release-check" + garbledWhy
	case c.now.Sub(at) > ReleaseCheckMaxAge:
		n = minutesOf(c.now.Sub(at))
		why = "release-check has not run for " + strconv.Itoa(n) + " min"
	default:
		return nil
	}
	return []Banner{{ID: "C16b", Severity: SeverityAlarm, Count: n,
		Text: "Release manifests not checked: " + why + ", so a rejected manifest would go unnoticed."}}
}

// gatewayBanners is C01 from the gateway probe and the gateway half of C05.
// The certificate is judged only while the probe passes: a failing probe is
// already C01, and an unchecked one is already C01b.
func gatewayBanners(g *ProbeRun, iv time.Duration, c bannerCtx) []Banner {
	switch v, why, n := judgeProbe(g, GatewayFailuresToAlarm, iv, c.now); v {
	case probeFailing:
		tail, at := c.lastOK(g.LastOKAt, "It has never answered a probe.")
		return []Banner{{ID: "C01", Severity: SeverityCritical, Count: g.ConsecutiveFailures, At: at,
			Text: "Gateway unreachable: no agent can back up." + tail}}
	case probeUnchecked:
		return []Banner{notChecked("C01b", "Gateway", "an unreachable gateway would go unnoticed", why, n)}
	}
	na, ok := g.CertNotAfter.Time()
	if !ok {
		return []Banner{{ID: "C05b", Subject: SubjectGateway, Severity: SeverityAlarm,
			Text: "Gateway certificate not checked: the gateway probe recorded no certificate expiry, so an expiring gateway certificate would go unnoticed."}}
	}
	left := na.Sub(c.now)
	if left <= 0 {
		return []Banner{{ID: "C05", Subject: SubjectGateway, Severity: SeverityCritical, At: na,
			Text: "Gateway certificate has expired: agents cannot back up through it."}}
	}
	days := int(math.Floor(left.Hours() / 24))
	in := "in " + strconv.Itoa(days) + " days"
	switch days {
	case 0:
		in = "in less than a day"
	case 1:
		in = "in 1 day"
	}
	const day = 24 * time.Hour
	switch {
	case left < CertCritDays*day:
		return []Banner{{ID: "C05", Subject: SubjectGateway, Severity: SeverityCritical, Count: days, At: na, Text: "Gateway certificate expires " + in + "."}}
	case left < CertWarnDays*day:
		return []Banner{{ID: "C05", Subject: SubjectGateway, Severity: SeverityAlarm, Count: days, At: na, Text: "Gateway certificate expires " + in + "."}}
	}
	return nil
}

// smtpBanners is C03: three failed sends or probes in a row, a failed test, or
// alerts pending with nothing delivered for a day. Recent successful delivery
// is evidence too, so an unchecked probe is only a warning when nothing has
// been delivered within the day.
func smtpBanners(s *SMTPFacts, iv time.Duration, c bannerCtx) []Banner {
	v, why, n := judgeProbe(&s.Probe, SMTPFailuresToAlarm, iv, c.now)
	sent, sentOK := recorded(s.LastSendOKAt, c.now)
	recent := sentOK && c.now.Sub(sent) <= SMTPPendingWithoutSend
	failing := s.ConsecutiveSendFailures >= SMTPFailuresToAlarm || s.LastTestFailed || v == probeFailing ||
		(s.PendingAlerts > 0 && !recent)
	if failing {
		b := Banner{ID: "C03", Severity: SeverityCritical, Count: s.ConsecutiveSendFailures,
			Text: "Alert e-mail cannot be delivered: alarms may go unnoticed."}
		if sentOK {
			b.Text += " Last send " + c.clock(sent) + "."
			b.At = sent
		} else {
			b.Text += " No alert e-mail has been delivered yet."
		}
		return []Banner{b}
	}
	if v == probeUnchecked && !recent {
		return []Banner{notChecked("C03b", "Alert e-mail", "alert e-mail that cannot be delivered would go unnoticed", why, n)}
	}
	return nil
}

// jobVerdict reads a job heartbeat: stalled once the newest finish is older than
// stalledAfter; a job that never finished is "not yet" within stalledAfter of the
// process start and stalled after it, or at once when the start is unknown. A
// finish time that is garbled or in the future proves nothing: garbled.
func jobVerdict(j *JobFacts, stalledAfter time.Duration, startedAt Millis, now time.Time) (stalled, notYet, garbled bool, last time.Time, hasLast bool) {
	if j.LastFinishedAt != 0 {
		last, ok := recorded(j.LastFinishedAt, now)
		if !ok {
			return false, false, true, time.Time{}, false
		}
		return now.Sub(last) > stalledAfter, false, false, last, true
	}
	if start, ok := recorded(startedAt, now); ok && now.Sub(start) <= stalledAfter {
		return false, true, false, time.Time{}, false
	}
	return true, false, false, time.Time{}, false
}

// evaluatorBanners is C06.
func evaluatorBanners(j *JobFacts, startedAt Millis, c bannerCtx) []Banner {
	period := j.Period
	if period <= 0 {
		period = DefaultEvaluatorPeriod
	}
	stalled, notYet, garbled, last, hasLast := jobVerdict(j, EvaluatorStalledPeriods*period, startedAt, c.now)
	const lead = "Alerting is stalled: this view and notifications may be stale."
	switch {
	case garbled:
		return []Banner{notChecked("C06b", "Alerting", "stalled alerting would go unnoticed", "the evaluator's newest finish"+garbledWhy, 0)}
	case stalled && hasLast:
		return []Banner{{ID: "C06", Severity: SeverityCritical, At: last, Count: minutesOf(c.now.Sub(last)),
			Text: lead + " Last evaluation " + c.clock(last) + "."}}
	case stalled:
		return []Banner{{ID: "C06", Severity: SeverityCritical, Text: lead + " No evaluation has finished since the console started."}}
	case notYet:
		return []Banner{{ID: "C06b", Severity: SeverityAlarm,
			Text: "Alerting has not run yet: the first evaluation has not finished, so alarms are not raised until it does."}}
	}
	return nil
}

// maintainBanners is C07: the RPC socket did not answer its newest probe, or the
// maintenance scheduler has not finished for 30 min. At most one C07 and, when
// there is no C07, one C07b.
func maintainBanners(m *MaintainFacts, startedAt Millis, iv time.Duration, c bannerCtx) []Banner {
	const lead = "Maintenance worker is not running: retention, restore browsing and usage sampling are off."
	var down *Banner
	var unchecked *Banner
	switch v, why, n := judgeProbe(&m.RPC, 1, iv, c.now); v {
	case probeFailing:
		down = &Banner{ID: "C07", Severity: SeverityCritical, Count: m.RPC.ConsecutiveFailures, Text: lead}
	case probeUnchecked:
		b := notChecked("C07b", "Maintenance worker", "a stopped maintenance worker would go unnoticed", why, n)
		unchecked = &b
	}
	if m.Scheduler == nil && unchecked == nil {
		b := notChecked("C07b", "Maintenance worker", "a stopped maintenance worker would go unnoticed",
			"the console does not supply the maintenance scheduler's heartbeat", 0)
		unchecked = &b
	}
	if m.Scheduler != nil {
		stalled, notYet, garbled, last, hasLast := jobVerdict(m.Scheduler, MaintenanceSchedulerStale, startedAt, c.now)
		switch {
		case garbled && unchecked == nil:
			b := notChecked("C07b", "Maintenance worker", "a stopped maintenance worker would go unnoticed",
				"the maintenance scheduler's newest finish"+garbledWhy, 0)
			unchecked = &b
		case stalled && down == nil:
			tail := " The maintenance scheduler has not finished a run since the console started."
			if hasLast {
				tail = " The maintenance scheduler last finished at " + c.clock(last) + "."
			}
			down = &Banner{ID: "C07", Severity: SeverityCritical, At: last, Text: lead + tail}
		case notYet && unchecked == nil:
			unchecked = &Banner{ID: "C07b", Severity: SeverityAlarm,
				Text: "Maintenance worker not checked: the maintenance scheduler has not finished its first run, so a stopped maintenance worker would go unnoticed."}
		}
	}
	if down != nil {
		return []Banner{*down}
	}
	if unchecked != nil {
		return []Banner{*unchecked}
	}
	return nil
}

// viewBanners is C10: the snapshot is older than two refresh intervals, its age
// is unknown, or the live stream is down.
func viewBanners(v *ViewFacts, loadedAt time.Time, c bannerCtx) []Banner {
	iv := v.RefreshInterval
	if iv <= 0 {
		iv = DefaultRefreshInterval
	}
	if loadedAt.IsZero() || loadedAt.After(c.now.Add(FutureSkewAllowed)) {
		return []Banner{{ID: "C10", Severity: SeverityAlarm, Text: "Live updates disconnected: the age of this data is unknown."}}
	}
	if v.StreamDisconnected || c.now.Sub(loadedAt) > time.Duration(StaleViewIntervals)*iv {
		return []Banner{{ID: "C10", Severity: SeverityAlarm, At: loadedAt.UTC(),
			Text: "Live updates disconnected: data as of " + c.clock(loadedAt) + "."}}
	}
	return nil
}

// databaseBanners is C11, one banner listing every cause; critical when the
// database or its only backup is proven bad, a warning otherwise. Inputs that
// were never measured are causes in their own right.
func databaseBanners(d *DatabaseFacts, c bannerCtx) []Banner {
	var causes []string
	sev := SeverityNone
	add := func(s Severity, cause string) {
		causes = append(causes, cause)
		if s > sev {
			sev = s
		}
	}
	integrityAt, integrityOK := recorded(d.IntegrityCheckedAt, c.now)
	switch {
	case d.IntegrityCheckedAt == 0:
		add(SeverityAlarm, "integrity check has never run")
	case !integrityOK:
		add(SeverityAlarm, "integrity check"+garbledWhy)
	case !d.IntegrityOK:
		add(SeverityCritical, "integrity check failed")
	case c.now.Sub(integrityAt) > IntegrityMaxAge:
		add(SeverityAlarm, "integrity check not run for "+strconv.Itoa(int(c.now.Sub(integrityAt)/(24*time.Hour)))+" days")
	}
	measuredAt, measuredOK := recorded(d.MeasuredAt, c.now)
	switch {
	case d.MeasuredAt == 0 || d.TotalBytes <= 0:
		add(SeverityAlarm, "free disk and WAL size not measured")
	case !measuredOK:
		add(SeverityAlarm, "disk measurement"+garbledWhy)
	case c.now.Sub(measuredAt) > DBMeasureMaxAge:
		add(SeverityAlarm, "free disk and WAL size not measured for "+strconv.Itoa(minutesOf(c.now.Sub(measuredAt)))+" min")
	case d.FreeBytes < 0 || d.FreeBytes > d.TotalBytes || d.WALBytes < 0:
		add(SeverityAlarm, "disk measurement is garbled")
	default:
		if pct := 100 * float64(d.FreeBytes) / float64(d.TotalBytes); pct < DBFreeDiskPct {
			add(SeverityAlarm, "disk below "+strconv.Itoa(DBFreeDiskPct)+"% free ("+strconv.Itoa(int(math.Floor(pct)))+"%)")
		}
		if d.WALBytes > DBWALLimit {
			add(SeverityAlarm, "WAL over 1 GiB")
		}
	}
	switch sb := d.SelfBackup; {
	case sb == nil:
		add(SeverityAlarm, "self-backup not supplied")
	case sb.NotInBuild:
		// db-self-backup is a later milestone (doc.go "Not in this build").
	default:
		last, ok := recorded(sb.LastAt, c.now)
		switch {
		case sb.LastAt == 0:
			add(SeverityAlarm, "no self-backup has been written")
		case !ok:
			add(SeverityAlarm, "self-backup"+garbledWhy)
		case c.now.Sub(last) > DBSelfBackupMaxAge:
			add(SeverityAlarm, "self-backup older than 26 h")
		}
		if sb.VerifyFailed {
			add(SeverityCritical, "self-backup verify failed")
		}
	}
	since, sinceOK := recorded(d.WorkerStoreFailingSince, c.now)
	var at time.Time // the one clock time in the text, so a page can re-render it
	switch n := d.WorkerStoreFailures; {
	case n >= WorkerStoreFailuresToCritical:
		cause := "the maintenance worker cannot read or write the console database"
		if sinceOK {
			cause += " (since " + c.clock(since) + ")"
			at = since
		}
		add(SeverityCritical, cause)
	case n < 0, n == 0 && d.WorkerStoreFailingSince != 0, n > 0 && !sinceOK:
		add(SeverityAlarm, "the maintenance worker's database check is garbled")
	}
	if len(causes) == 0 {
		return nil
	}
	return []Banner{{ID: "C11", Severity: sev, Count: len(causes), At: at,
		Text: "Console database needs attention: " + strings.Join(causes, "; ") + "."}}
}

// masterKeyBanners is C12: the key is not loaded, or active escrows were
// wrapped under a different key generation.
func masterKeyBanners(m *MasterKeyFacts) []Banner {
	if !m.Loaded {
		return []Banner{{ID: "C12", Severity: SeverityCritical,
			Text: "Master key unavailable: enrollment, escrow reveal and server-side retention are disabled."}}
	}
	mismatched := 0
	for _, id := range m.EscrowKeyIDs {
		if id != m.KeyID {
			mismatched++
		}
	}
	if mismatched > 0 {
		return []Banner{{ID: "C12", Severity: SeverityCritical, Count: mismatched,
			Text: "Master key unavailable: the loaded master key is not the one some escrowed repository keys were wrapped with, so escrow reveal and server-side retention fail for those devices."}}
	}
	return nil
}

// parseLocalTime reads "15:04" (24-hour, as digest_local_time is stored).
func parseLocalTime(s string) (hh, mm int, ok bool) {
	h, m, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found {
		return 0, 0, false
	}
	hh, e1 := strconv.Atoi(h)
	mm, e2 := strconv.Atoi(m)
	if e1 != nil || e2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		return 0, 0, false
	}
	return hh, mm, true
}

// localTimeOn is hh:mm on the local date y-mo-d in loc. A time that a DST
// change skips (02:30 on a spring-forward day) becomes the first instant after
// the gap at the same distance past it (03:30), never an hour early; a time the
// change repeats (01:30 on a fall-back day) is its first occurrence.
func localTimeOn(y int, mo time.Month, d, hh, mm int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, hh, mm, 0, 0, loc)
	if t.Hour() == hh && t.Minute() == mm {
		return t
	}
	_, offBefore := t.Add(-12 * time.Hour).Zone()
	if later := time.Date(y, mo, d, hh, mm, 0, 0, time.UTC).Add(-time.Duration(offBefore) * time.Second); later.After(t) {
		return later
	}
	return t
}

// digestDueAtOrBefore is the newest instant at or before ref at which the
// digest was due: hh:mm on a local calendar day of loc.
func digestDueAtOrBefore(hh, mm int, loc *time.Location, ref time.Time) time.Time {
	y, mo, d := ref.In(loc).Date()
	due := localTimeOn(y, mo, d, hh, mm, loc)
	if due.After(ref) {
		due = localTimeOn(y, mo, d-1, hh, mm, loc)
	}
	return due
}

// DigestDue returns the newest time at or before now at which the daily digest
// was due, for localTime ("15:04") in loc (nil means UTC); ok is false when
// localTime does not parse. The digest job sends for that due time's local date
// (DigestKey), so triggering it twice on one local day, DST days included,
// names the same digest and sends once (§4.7, §4.8 E05).
func DigestDue(localTime string, loc *time.Location, now time.Time) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	hh, mm, ok := parseLocalTime(localTime)
	if !ok {
		return time.Time{}, false
	}
	return digestDueAtOrBefore(hh, mm, loc, now).UTC(), true
}

// DigestKey is the notification_log dedupe key of the digest for the local
// calendar date of t in loc (nil means UTC): "digest:2006-01-02" (§4.7).
func DigestKey(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return "digest:" + t.In(loc).Format("2006-01-02")
}

// digestBanners is C14: no digest on the local date of the newest due time
// whose grace has passed. A digest sent earlier that same local date (a manual
// "send now") counts, because the dedupe key is the local date.
func digestBanners(d *DigestFacts, c bannerCtx) []Banner {
	if d.NotInBuild || d.Disabled {
		return nil
	}
	hh, mm, ok := parseLocalTime(d.LocalTime)
	if !ok {
		return []Banner{{ID: "C14b", Severity: SeverityAlarm,
			Text: "Daily digest not checked: digest_local_time is not set, so a digest that is never sent would go unnoticed."}}
	}
	due := digestDueAtOrBefore(hh, mm, c.loc, c.now.Add(-DigestGrace))
	ld := due.In(c.loc)
	y, mo, day := ld.Date()
	dayStart := time.Date(y, mo, day, 0, 0, 0, 0, c.loc)
	sent, sentOK := recorded(d.LastSentAt, c.now)
	if d.LastSentAt != 0 && !sentOK {
		return []Banner{{ID: "C14b", Severity: SeverityAlarm,
			Text: "Daily digest not checked: the newest digest send" + garbledWhy + ", so a digest that is never sent would go unnoticed."}}
	}
	if sentOK && !sent.Before(dayStart) {
		return nil
	}
	by := due.Add(DigestGrace)
	return []Banner{{ID: "C14", Severity: SeverityCritical, At: by.UTC(),
		Text: "Daily digest was not delivered. It was due by " + c.clock(by) + "."}}
}

// growthPerDay is the least-squares usage growth in bytes per day over the
// valid samples; ok is false with fewer than two valid samples or a span under
// an hour (too little to project from).
func growthPerDay(samples []UsageSample, now time.Time) (float64, bool) {
	type pt struct{ x, y float64 }
	var pts []pt
	for _, s := range samples {
		if t, ok := recorded(s.At, now); ok {
			pts = append(pts, pt{float64(t.UnixMilli()) / float64(24*time.Hour/time.Millisecond), float64(s.Bytes)})
		}
	}
	if len(pts) < 2 {
		return 0, false
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].x < pts[j].x })
	if (pts[len(pts)-1].x-pts[0].x)*24 < 1 {
		return 0, false
	}
	var mx, my float64
	for _, p := range pts {
		mx += p.x
		my += p.y
	}
	mx /= float64(len(pts))
	my /= float64(len(pts))
	var num, den float64
	for _, p := range pts {
		num += (p.x - mx) * (p.y - my)
		den += (p.x - mx) * (p.x - mx)
	}
	if den == 0 {
		return 0, false
	}
	return num / den, true
}

// quotaBanners is C15 for the worst storage target with a soft quota: 80 %
// warns, 90 % and 100 % are critical, and a growth projection that reaches
// 100 % within 14 days is critical. A target with a quota whose usage was never
// measured, whose measurement time is garbled or in the future, or whose usage
// is older than QuotaMeasureMaxAge is C15b.
func quotaBanners(q *QuotaFacts, c bannerCtx) []Banner {
	if q.NotInBuild {
		return nil
	}
	var worst *Banner
	worstPct := -1.0
	unmeasured := 0
	unmeasuredName, unmeasuredWhy := "", ""
	withQuota := 0
	for i := range q.Targets {
		t := &q.Targets[i]
		if t.SoftQuotaBytes <= 0 {
			continue // no quota set: nothing to measure against
		}
		withQuota++
		at, ok := recorded(t.MeasuredAt, c.now)
		why := ""
		switch {
		case t.MeasuredAt == 0:
			why = "usage has never been measured"
		case !ok:
			why = "the newest usage measurement" + garbledWhy
		case t.UsedBytes < 0:
			why = "the newest usage figure is garbled"
		case c.now.Sub(at) > QuotaMeasureMaxAge:
			why = "usage has not been measured for " + strconv.Itoa(int(c.now.Sub(at)/time.Hour)) + " h"
		}
		if why != "" {
			unmeasured++
			if unmeasured == 1 {
				unmeasuredName, unmeasuredWhy = t.Name, why
			}
			continue
		}
		pct := 100 * float64(t.UsedBytes) / float64(t.SoftQuotaBytes)
		sev := SeverityNone
		switch {
		case pct >= QuotaCritPct:
			sev = SeverityCritical
		case pct >= QuotaWarnPct:
			sev = SeverityAlarm
		}
		projection := ""
		if pct < 100 {
			if g, ok := growthPerDay(t.Samples, c.now); ok && g > 0 {
				days := float64(t.SoftQuotaBytes-t.UsedBytes) / g
				if days < QuotaProjectionDays {
					sev = SeverityCritical
					switch n := int(math.Floor(days)); n {
					case 0:
						projection = " Projected full in less than a day."
					case 1:
						projection = " Projected full in about 1 day."
					default:
						projection = " Projected full in about " + strconv.Itoa(n) + " days."
					}
				}
			}
		}
		if sev == SeverityNone {
			continue
		}
		if worst != nil && (sev < worst.Severity || (sev == worst.Severity && pct <= worstPct)) {
			continue
		}
		shown := int(math.Floor(pct))
		on := ""
		if t.Name != "" {
			on = " on " + t.Name
		}
		worst = &Banner{ID: "C15", Severity: sev, Count: shown,
			Text: "Storage at " + strconv.Itoa(shown) + "% of quota" + on + "." + projection}
		worstPct = pct
	}
	var out []Banner
	if worst != nil {
		out = append(out, *worst)
	}
	if missing := q.WithQuota - withQuota; missing > 0 && unmeasured == 0 {
		// Targets with a quota whose usage the loader did not supply: never "within quota".
		return append(out, Banner{ID: "C15b", Severity: SeverityAlarm, Count: missing,
			Text: "Storage usage not checked: " + strconv.Itoa(missing) + " of " + strconv.Itoa(q.WithQuota) +
				" targets with a quota have no usage figure, so a full quota would go unnoticed."})
	} else if missing > 0 {
		unmeasured += missing
	}
	if unmeasured > 0 {
		subject := "Storage usage"
		if unmeasuredName != "" {
			subject += " of " + unmeasuredName
		}
		if unmeasured > 1 {
			subject += " and " + strconv.Itoa(unmeasured-1) + " more targets"
		}
		out = append(out, Banner{ID: "C15b", Severity: SeverityAlarm, Count: unmeasured,
			Text: subject + " not checked: " + unmeasuredWhy + ", so a full quota would go unnoticed."})
	}
	return out
}

// LoginSyncStatus is what JudgeLoginSync decides about a retired device's gateway login.
type LoginSyncStatus string

const (
	// LoginRemoved: the sync ran after the retire and passed, so the login is gone from the file.
	LoginRemoved LoginSyncStatus = "removed"
	// LoginPending: the login may still be in the file, so the gateway may still accept it.
	LoginPending LoginSyncStatus = "pending"
	// LoginUnchecked: the facts cannot say. This is never read as removed.
	LoginUnchecked LoginSyncStatus = "unchecked"
)

// Reason classes of a login that is not known to be removed. A failed run reports the job's own
// error class instead.
const (
	// LoginNoSyncSinceRetire: the job has not started a run since the retire.
	LoginNoSyncSinceRetire = "no_sync_since_retire"
	// LoginSyncFailed: the newest run failed and named no class.
	LoginSyncFailed = "sync_failed"
	// LoginSyncSkipped: the newest run says nothing about the file.
	LoginSyncSkipped = "sync_skipped"
	// LoginNotChecked: the facts could not be read or are garbled.
	LoginNotChecked = "not_checked"
)

// LoginSyncVerdict is the judgement with its reason class and, when unchecked, why in words.
type LoginSyncVerdict struct {
	Status LoginSyncStatus
	Class  string
	Why    string
}

// JudgeLoginSync decides from the facts alone. It is the one rule behind banner C17, the retire
// answer and the device page, so they cannot disagree. A run that started in the retire's own
// millisecond counts as after it: the retire signals the job after its commit, so a run that read
// the logins just before that commit is followed at once by the signalled one.
func JudgeLoginSync(f LoginSyncFacts, now time.Time) LoginSyncVerdict {
	unchecked := func(why string) LoginSyncVerdict {
		return LoginSyncVerdict{Status: LoginUnchecked, Class: LoginNotChecked, Why: why}
	}
	if f.NotRead != "" {
		return unchecked(f.NotRead)
	}
	if f.NewestRetireAt == 0 {
		return LoginSyncVerdict{Status: LoginRemoved}
	}
	retired, ok := recorded(f.NewestRetireAt, now)
	if !ok {
		return unchecked("the newest retire" + garbledWhy)
	}
	if f.LastStartedAt == 0 {
		return unchecked("the login sync has no recorded run")
	}
	started, ok := recorded(f.LastStartedAt, now)
	if !ok {
		return unchecked("the newest login sync" + garbledWhy)
	}
	if started.Before(retired) {
		return LoginSyncVerdict{Status: LoginPending, Class: LoginNoSyncSinceRetire}
	}
	switch f.LastStatus {
	case "ok":
		return LoginSyncVerdict{Status: LoginRemoved}
	case "error":
		if f.LastErrorClass != "" {
			return LoginSyncVerdict{Status: LoginPending, Class: f.LastErrorClass}
		}
		return LoginSyncVerdict{Status: LoginPending, Class: LoginSyncFailed}
	case "skipped":
		return LoginSyncVerdict{Status: LoginPending, Class: LoginSyncSkipped}
	}
	return unchecked("the newest login sync has a status this console does not know")
}

// loginSyncBanners is C17: a retired device's gateway login may still be in the deployed file. It
// clears by itself when a sync passes. C17b: the facts cannot say, which is never read as removed.
func loginSyncBanners(f *LoginSyncFacts, c bannerCtx) []Banner {
	v := JudgeLoginSync(*f, c.now)
	switch v.Status {
	case LoginPending:
		why := "the newest login sync failed: " + v.Class
		if v.Class == LoginNoSyncSinceRetire {
			why = "no login sync has run since the retire"
		}
		b := Banner{ID: "C17", Severity: SeverityCritical,
			Text: "The gateway login of a retired device is not removed: the gateway may still accept it until the sync succeeds (" + why + ")."}
		if t, ok := recorded(f.NewestRetireAt, c.now); ok {
			b.At = t
		}
		return []Banner{b}
	case LoginUnchecked:
		return []Banner{notChecked("C17b", "Gateway login removal", "a retired login left in the gateway file would go unnoticed", v.Why, 0)}
	}
	return nil
}
