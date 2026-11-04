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
)

// bannerCtx is what every banner rule needs to word its text.
type bannerCtx struct {
	now time.Time
	loc *time.Location
}

// clock renders t for banner text: "3:04 PM" in the display zone within the
// last day, "Mon 2 Jan 3:04 PM" otherwise (a bare clock time days old would read
// as today).
func (c bannerCtx) clock(t time.Time) string {
	lt := t.In(c.loc)
	if d := c.now.Sub(t); d < 24*time.Hour && d > -24*time.Hour {
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

// judgeProbe reads a ProbeRun. A failure streak at the threshold is the
// condition even when the prober has since stopped (the last evidence is a
// failure); otherwise a probe that never completed or has not completed for
// ProbeStaleIntervals intervals is unchecked, never healthy.
func judgeProbe(p *ProbeRun, failAfter int, interval time.Duration, now time.Time) (v probeVerdict, age time.Duration, never bool) {
	if p.ConsecutiveFailures >= failAfter {
		return probeFailing, 0, false
	}
	last, ok := p.LastRunAt.Time()
	if !ok {
		return probeUnchecked, 0, true
	}
	if age = now.Sub(last); age > time.Duration(ProbeStaleIntervals)*interval {
		return probeUnchecked, age, false
	}
	return probeOK, age, false
}

// notChecked is the "not checked" warning of a probe-driven condition.
func notChecked(id, subject, consequence string, never bool, age time.Duration) Banner {
	why := "the health prober has not completed a probe of it yet"
	n := 0
	if !never {
		n = minutesOf(age)
		why = "no probe of it has completed for " + strconv.Itoa(n) + " min"
	}
	return Banner{ID: id, Severity: SeverityAlarm, Count: n, Text: subject + " not checked: " + why + ", so " + consequence + "."}
}

// lastOK is the " Last OK h:mm PM." tail of a failing-probe banner.
func (c bannerCtx) lastOK(m Millis, never string) (string, time.Time) {
	if t, ok := m.Time(); ok {
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
	switch v, age, never := judgeProbe(s, StorageFailuresToAlarm, iv, c.now); v {
	case probeFailing:
		tail, at := c.lastOK(s.LastOKAt, "It has never answered a probe.")
		return []Banner{{ID: "C02", Severity: SeverityCritical, Count: s.ConsecutiveFailures, At: at,
			Text: "Storage unreachable from the admin path: retention, restore and usage are impaired." + tail}}
	case probeUnchecked:
		return []Banner{notChecked("C02b", "Storage", "storage that cannot be reached from the admin path would go unnoticed", never, age)}
	}
	return nil
}

// releaseBanners is C16.
func releaseBanners(r *ReleaseFacts, c bannerCtx) []Banner {
	switch _, ok := r.CheckedAt.Time(); {
	case r.Rejected:
		return []Banner{{ID: "C16", Severity: SeverityAlarm, Text: "Release manifest rejected."}}
	case !ok:
		return []Banner{{ID: "C16b", Severity: SeverityAlarm,
			Text: "Release manifests not checked: release-check has not run, so a rejected manifest would go unnoticed."}}
	}
	return nil
}

// gatewayBanners is C01 from the gateway probe and the gateway half of C05.
// The certificate is judged only while the probe passes: a failing probe is
// already C01, and an unchecked one is already C01b.
func gatewayBanners(g *ProbeRun, iv time.Duration, c bannerCtx) []Banner {
	switch v, age, never := judgeProbe(g, GatewayFailuresToAlarm, iv, c.now); v {
	case probeFailing:
		tail, at := c.lastOK(g.LastOKAt, "It has never answered a probe.")
		return []Banner{{ID: "C01", Severity: SeverityCritical, Count: g.ConsecutiveFailures, At: at,
			Text: "Gateway unreachable: no agent can back up." + tail}}
	case probeUnchecked:
		return []Banner{notChecked("C01b", "Gateway", "an unreachable gateway would go unnoticed", never, age)}
	}
	na, ok := g.CertNotAfter.Time()
	if !ok {
		return []Banner{{ID: "C05b", Severity: SeverityAlarm,
			Text: "Gateway certificate not checked: the gateway probe recorded no certificate expiry, so an expiring gateway certificate would go unnoticed."}}
	}
	left := na.Sub(c.now)
	if left <= 0 {
		return []Banner{{ID: "C05", Severity: SeverityCritical, At: na,
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
		return []Banner{{ID: "C05", Severity: SeverityCritical, Count: days, At: na, Text: "Gateway certificate expires " + in + "."}}
	case left < CertWarnDays*day:
		return []Banner{{ID: "C05", Severity: SeverityAlarm, Count: days, At: na, Text: "Gateway certificate expires " + in + "."}}
	}
	return nil
}

// smtpBanners is C03: three failed sends or probes in a row, a failed test, or
// alerts pending with nothing delivered for a day. Recent successful delivery
// is evidence too, so an unchecked probe is only a warning when nothing has
// been delivered within the day.
func smtpBanners(s *SMTPFacts, iv time.Duration, c bannerCtx) []Banner {
	v, age, never := judgeProbe(&s.Probe, SMTPFailuresToAlarm, iv, c.now)
	sent, sentOK := s.LastSendOKAt.Time()
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
		return []Banner{notChecked("C03b", "Alert e-mail", "alert e-mail that cannot be delivered would go unnoticed", never, age)}
	}
	return nil
}

// jobVerdict reads a job heartbeat: stalled once the newest finish is older than
// stalledAfter; a job that never finished is "not yet" within stalledAfter of the
// process start and stalled after it, or at once when the start is unknown.
func jobVerdict(j *JobFacts, stalledAfter time.Duration, startedAt Millis, now time.Time) (stalled, notYet bool, last time.Time, hasLast bool) {
	if last, ok := j.LastFinishedAt.Time(); ok {
		return now.Sub(last) > stalledAfter, false, last, true
	}
	if start, ok := startedAt.Time(); ok && now.Sub(start) <= stalledAfter {
		return false, true, time.Time{}, false
	}
	return true, false, time.Time{}, false
}

// evaluatorBanners is C06.
func evaluatorBanners(j *JobFacts, startedAt Millis, c bannerCtx) []Banner {
	period := j.Period
	if period <= 0 {
		period = DefaultEvaluatorPeriod
	}
	stalled, notYet, last, hasLast := jobVerdict(j, EvaluatorStalledPeriods*period, startedAt, c.now)
	const lead = "Alerting is stalled: this view and notifications may be stale."
	switch {
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
	switch v, age, never := judgeProbe(&m.RPC, 1, iv, c.now); v {
	case probeFailing:
		down = &Banner{ID: "C07", Severity: SeverityCritical, Count: m.RPC.ConsecutiveFailures, Text: lead}
	case probeUnchecked:
		b := notChecked("C07b", "Maintenance worker", "a stopped maintenance worker would go unnoticed", never, age)
		unchecked = &b
	}
	if m.Scheduler != nil {
		stalled, notYet, last, hasLast := jobVerdict(m.Scheduler, MaintenanceSchedulerStale, startedAt, c.now)
		switch {
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
	if loadedAt.IsZero() {
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
	if _, ok := d.IntegrityCheckedAt.Time(); !ok {
		add(SeverityAlarm, "integrity check has never run")
	} else if !d.IntegrityOK {
		add(SeverityCritical, "integrity check failed")
	}
	if _, ok := d.MeasuredAt.Time(); !ok || d.TotalBytes <= 0 {
		add(SeverityAlarm, "free disk and WAL size not measured")
	} else {
		if pct := 100 * float64(d.FreeBytes) / float64(d.TotalBytes); pct < DBFreeDiskPct {
			add(SeverityAlarm, "disk below "+strconv.Itoa(DBFreeDiskPct)+"% free ("+strconv.Itoa(int(math.Floor(pct)))+"%)")
		}
		if d.WALBytes > DBWALLimit {
			add(SeverityAlarm, "WAL over 1 GiB")
		}
	}
	if sb := d.SelfBackup; sb != nil {
		last, ok := sb.LastAt.Time()
		switch {
		case !ok:
			add(SeverityAlarm, "no self-backup has been written")
		case c.now.Sub(last) > DBSelfBackupMaxAge:
			add(SeverityAlarm, "self-backup older than 26 h")
		}
		if sb.VerifyFailed {
			add(SeverityCritical, "self-backup verify failed")
		}
	}
	if len(causes) == 0 {
		return nil
	}
	return []Banner{{ID: "C11", Severity: sev, Count: len(causes),
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
	if d.Disabled {
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
	if sent, ok := d.LastSentAt.Time(); ok && !sent.Before(dayStart) {
		return nil
	}
	by := due.Add(DigestGrace)
	return []Banner{{ID: "C14", Severity: SeverityCritical, At: by.UTC(),
		Text: "Daily digest was not delivered. It was due by " + c.clock(by) + "."}}
}

// growthPerDay is the least-squares usage growth in bytes per day over the
// valid samples; ok is false with fewer than two valid samples or a span under
// an hour (too little to project from).
func growthPerDay(samples []UsageSample) (float64, bool) {
	type pt struct{ x, y float64 }
	var pts []pt
	for _, s := range samples {
		if t, ok := s.At.Time(); ok {
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
// measured is C15b.
func quotaBanners(q *QuotaFacts, c bannerCtx) []Banner {
	var worst *Banner
	worstPct := -1.0
	unmeasured := 0
	unmeasuredName := ""
	for i := range q.Targets {
		t := &q.Targets[i]
		if t.SoftQuotaBytes <= 0 {
			continue // no quota set: nothing to measure against
		}
		if _, ok := t.MeasuredAt.Time(); !ok {
			unmeasured++
			if unmeasuredName == "" {
				unmeasuredName = t.Name
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
			if g, ok := growthPerDay(t.Samples); ok && g > 0 {
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
	if unmeasured > 0 {
		subject := "Storage usage"
		if unmeasuredName != "" {
			subject += " of " + unmeasuredName
		}
		if unmeasured > 1 {
			subject += " and " + strconv.Itoa(unmeasured-1) + " more targets"
		}
		out = append(out, Banner{ID: "C15b", Severity: SeverityAlarm, Count: unmeasured,
			Text: subject + " not checked: usage has never been measured, so a full quota would go unnoticed."})
	}
	return out
}
