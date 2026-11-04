// Package state is the product: the read-time health engine. Classify, Rollup
// and Evaluate are pure functions over persisted facts, policy thresholds and
// an injected server time (ADR 0002 D10, architecture section 4). Nothing here
// persists a health state, reads a clock, touches a database or the network,
// or logs; the package imports only the standard library. Every page, the
// status API, the digest and the alarm evaluator call these functions, so they
// can never disagree.
//
// # Why it fails loud
//
// A backup console that goes quietly green when it cannot see its agents is
// worse than none. The documented case is an 18-day silent outage behind a
// certificate problem on another product: nothing was red because nothing was
// reporting. Every "no data" path in this package is therefore an alarm
// condition: an incomplete load is UNKNOWN (critical), an empty poll after a
// non-empty one is banner C09 and C08, a timestamp that does not parse is never
// read as healthy, an unknown run outcome is a failure, a customer with no
// devices is never green, and FleetView.AllClear is true only on evidence.
//
// # Entry points
//
//	Classify(DeviceFacts, now) DeviceState            one device
//	Rollup(CustomerFacts, []DeviceState, now)         one customer (architecture 4.5)
//	Evaluate(World, now) FleetView                    devices, customers, banners, AllClear
//	CompareVersions, ExpectedVersion                  version tuples (A18, A19, A23)
//	DefaultThresholds                                 the 3.6 sla defaults
//
// Inputs are plain values; a zero Millis is NULL. Thresholds travel with each
// device (the merged policy sla plus schedule). An all-zero Thresholds means
// DefaultThresholds.
//
// # The precedence table (architecture 4.2)
//
// Every row is evaluated independently. The first applicable row, after
// suppression, is Primary; all applicable rows are kept as Flags in this order
// (Precedence returns it):
//
//	 1 RETIRED            retired_at set. Suppresses everything. Never alarms.
//	 2 PAUSED             paused_at set and paused_until absent or in the future.
//	 3 UNKNOWN            facts incomplete. Critical. No other row is evaluated.
//	 4 IDENTITY_CONFLICT  identity_conflict_at set. Critical.
//	 5 PENDING_FIRST      no success since (re)enrollment, inside the grace window,
//	                      no hard failure. No alarm.
//	 6 BLANK              no success since (re)enrollment and the grace is gone, or a
//	                      hard failure, or a garbled success time. Critical.
//	 7 STUCK              open backup run older than its limit. Critical.
//	 8 FAILED             newest finished backup run failed, enough in a row, nothing
//	                      since. Critical.
//	 9 PARTIAL            newest finished backup run is a partial over tolerance. Alarm.
//	10 UNSEEN             no heartbeat for more than unseen_after (strict). Critical.
//	11 STALE              last success older than stale_after (strict). Critical.
//	12 ESCROW_UNVERIFIED  escrowed key not proven recoverable. Alarm.
//	13 MAINT_FAILED       forget/prune/check failed, starved, or no forget in 2x cadence. Alarm.
//	14 MISSED             a scheduled run did not happen. Early warning, no alarm.
//	15 DEFERRED           the agent is deliberately not running. No alarm.
//	16 OUTDATED           agent version below expected. Alarm only if configured.
//	17 OK                 none of the above.
//
// DeviceState.Alarm is the highest severity over all flags, not just Primary.
//
// # Suppression (architecture 4.3)
//
// RETIRED suppresses everything for the device, even an incomplete load.
// PAUSED suppresses rows 6 to 11 and 14 to 16; it does not suppress rows 4, 12
// and 13, and the suppressed rows are not listed as flags at all. An expired
// pause is simply ignored, so a stale device alarms the instant the pause ends
// (B11); a pause expiry that does not parse is ignored too. A paused device
// whose facts are incomplete stays PAUSED but also carries UNKNOWN and alarms,
// because the identity, escrow and maintenance rows cannot be evaluated.
// PENDING_FIRST suppresses UNSEEN, STALE, MISSED, DEFERRED and OUTDATED while
// inside the grace, and also FAILED for transient failures (a hard failure class
// ends PENDING_FIRST at once, so BLANK and FAILED both show). Acknowledgements
// and snoozes never change a state: they set Ack and the acknowledged or
// snoozed attribute, and an expired snooze simply stops muting at read time.
// The customer-wide collapse of 4.3 is in "Customers and the fleet" below.
//
// # What Since means
//
// RETIRED retired_at; PAUSED paused_at; UNKNOWN zero (the facts carry no first
// failed load time); IDENTITY_CONFLICT identity_conflict_at; PENDING_FIRST
// enrolled_at; BLANK the first-backup deadline (the failure time when a hard
// failure broke out before it); STUCK run start plus its limit; FAILED the
// finish time of the newest failed run, which approximates the first failure of
// the streak because the facts do not carry the streak start; PARTIAL that run's
// finish time; UNSEEN last heartbeat plus unseen_after; STALE last success plus
// stale_after; ESCROW_UNVERIFIED key creation plus the verify grace (or, once
// EscrowRecheckEnforced is on, the last verification plus the recheck
// interval); MAINT_FAILED the failing run's finish;
// MISSED the expected run time; DEFERRED the deferred run's start; OUTDATED zero;
// OK the last success. A zero Since means the facts do not know.
//
// # Partial tolerance (architecture 3.4 and 4.4)
//
// A partial backup (restic exit 3) counts as a success, and the engine uses its
// finish time as the effective last success (B17), when all of these hold: the
// counts parsed; no volume that held a configured root fell back from VSS; every
// error class is permission, locked or unsupported_file (so missing_source is always over); failed
// files are attributed to a class; and FailedFiles is at most MaxFailedFiles or
// at most MaxFailedPct percent of total plus failed files. The state is then OK
// with the partial_under_tolerance attribute. Anything else, including counts
// that could not be parsed, is PARTIAL (B21). A VSS fallback on its own is only
// the vss_fallback attribute (B22); RunFacts.VSSFallbackOnRoot is the loader's
// signal that the fallback hit a configured root, which makes the run over
// tolerance. Until the loader fills it (M3), only the attribute is raised.
//
// # Time
//
// Server time decides every state. Millis values are parsed with Millis.Time,
// which accepts only 1e12 to 4102444800000 (2001 to 2100); zero, negative,
// seconds-epoch and far-future values are garbled, and the engine judges the
// parsed result: a garbled last success is BLANK, a garbled last heartbeat is
// UNSEEN, a garbled open-run start is STUCK. Detail.GarbledFields names them.
// The engine never reads the wall clock; a test greps this package's non-test
// sources to keep it that way (B48).
//
// # Customers and the fleet
//
// Rollup orders customers MISSING, UNKNOWN-containing, ALARM, WARN, OK,
// ONBOARDING, EMPTY (SortKey 0 to 6). Evaluate adds the console banners that
// need only facts: C08 (an enrolled fleet with no active device; critical) and
// C09 (partial data or an empty poll; critical). FleetView.AllClear is true only
// when there are no banners of any severity, no alarming device or customer,
// the load was Complete, and the fleet is non-empty or this is the first run.
//
// Collapse (4.3): a customer with at least 5 active (non-retired, non-paused)
// devices of which at least half are UNSEEN or DEFERRED for the gateway is one
// customer-level row (FleetView.Collapses, CustomerCollapse); its device rows
// stay. Fleet-wide, at least 5 reporting devices (active, facts known, not
// UNSEEN) of which at least half are DEFERRED for the gateway is one C01 (C19)
// and a fleet Collapse that covers the customer ones for notification. Devices
// being offline never raise a console banner, however many (ADR 0013, C12).
//
// C13 (clock suspect, warning): at least 5 reporting devices, and at least 20 %
// of them, report a clock offset of the same sign beyond 2 min. A reporting
// device here is non-retired, facts complete, with a parseable heartbeat no
// older than its unseen_after; the heartbeat handler stores an offset with every
// heartbeat, so such a device has a real offset. One skewed device is only its
// own clock_skew attribute, never its primary.
//
// # Probe banners (4.6)
//
// World.Probes carries one pointer per check. Nil means this build or caller
// does not run the check (it arrives with a later milestone, a unit test, a
// caller with no prober): nothing is raised, which is a property of the build,
// not of the data. A non-nil check at its zero value has never reported and is
// never read as healthy. Per condition:
//
//	C01  gateway probe, 3 failures in a row: critical. Never completed, or not for 3 prober intervals: C01b warning.
//	C02  storage probe, 2 failures in a row: critical. Never or stale: C02b warning.
//	C03  3 failed sends, a failed test send, 3 failed probes, or alerts pending with no delivery in 24 h:
//	     critical. Probe never or stale and no delivery in 24 h: C03b warning (a recent delivery is evidence).
//	C05  gateway certificate (the agent-listener half is health/endpoint.go, ADR 0013): under 21 d warning,
//	     under 7 d or expired critical. Judged only while the gateway probe passes; no notAfter recorded by a
//	     passing probe: C05b warning.
//	C06  alarm-evaluator not finished for 3 periods: critical. Never finished: C06b warning within 3 periods of
//	     ProbeFacts.StartedAt, critical after it or when the start is unknown.
//	C07  maintain RPC failed its newest probe, or the maintenance scheduler (when this build runs it) not
//	     finished for 30 min: critical. RPC never probed or stale, or the scheduler's first run pending within
//	     30 min of the start: C07b warning.
//	C10  snapshot (World.LoadedAt) older than 2 refresh intervals, unknown, or the stream disconnected: warning.
//	C11  one banner listing every cause: integrity failed or self-backup verify failed is critical; disk under
//	     10 % free, WAL over 1 GiB, self-backup older than 26 h or never written, and anything never measured
//	     (integrity never run, disk not measured) are warnings. WAL size zero is a real value.
//	C12  key not loaded (the zero value) or an active escrow wrapped under another key id: critical.
//	     MasterKeyFacts.EnrollmentAllowed is false without a loaded key (the 503 of C12 / C18).
//	C14  no digest on the local date of the newest digest_local_time + 30 min that has passed: critical.
//	     A disabled digest raises nothing; an unset or unparseable time is C14b warning.
//	C15  worst target with a soft quota: 80 % warning, 90 % and 100 % critical, growth (least squares over the
//	     samples, at least 2 spanning an hour) reaching 100 % within 14 d critical. A target without a quota
//	     raises nothing; a quota whose usage was never measured is C15b warning.
//	C16  a rejected manifest: warning. release-check never ran: C16b warning.
//
// A "b" id is the not-checked warning of its condition, the pattern ADR 0013 set
// with C04b. Banner text cites times in World.DisplayZone on a 12-hour clock and
// Banner.At carries the instant.
//
// # The ten-day strip and the digest (4.7)
//
// TenDayStrip draws one device's ten cells in the customer's zone (else the
// display zone, else UTC), bucketing each hour by its local date, so DST days of
// 23 and 25 hours are one cell. Days before the (re)enrollment day and from the
// pause day on are none; a day without runs is missed only when a run was
// expected (not pending, not without a schedule, not today). DigestDue and
// DigestKey give the digest one due time and one dedupe key per local date,
// across DST; DigestOf lists the exceptions the digest sends, MISSING customers
// and UNKNOWN devices first.
//
// # Scope of this version
//
// Implemented here: every row of 4.2, every rule of 4.3, 4.4, 4.5, every 4.6
// banner except C04, C04b and the agent-listener half of C05 (ADR 0013:
// internal/console/health), the ten-day strip and digest timing of 4.7, and
// test cases A01-A23, B01-B15, B17-B48, C01-C10, C12-C28, D01-D13 and E01-E05
// of 4.8. Elsewhere: B16 is a schema test; C11 is the agent-endpoint self-probe
// (internal/console/health); the enrollment handler's 503 (C18), the digest
// render and send with its notification_log dedupe (C22, C25, E05) and the
// one-notification-per-Collapse rule (C19, C20) are M4 and agentapi, built on
// MasterKeyFacts.EnrollmentAllowed, DigestOf, DigestDue, DigestKey and
// FleetView.Collapses, which are tested here.
//
// # Error-class vocabulary: a deliberate duplication
//
// internal/restic owns the error-class vocabulary of runs.error_class. This
// package may not import it, so it keeps its own copies of the strings it needs
// (the hard failure classes of row 5 and the tolerable partial classes of 3.4)
// and exposes them as HardFailureClasses and TolerableErrorClasses. The console
// should assert with a test that those sets agree with internal/restic's, so a
// change on one side cannot silently desynchronise the other.
package state
