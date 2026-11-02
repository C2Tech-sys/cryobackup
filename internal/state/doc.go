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
// The customer-wide collapse of 4.3 is a notification concern for M3.
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
// stale_after; ESCROW_UNVERIFIED key creation plus the verify grace (or the last
// verification plus the recheck interval); MAINT_FAILED the failing run's finish;
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
// when there are no banners, no alarming device or customer, the load was
// Complete, and the fleet is non-empty or this is the first run.
//
// # Scope of this version (v0)
//
// Implemented here: every row of 4.2, the rules of 4.3 that concern one device,
// 4.4, 4.5, banners C08 and C09, and test cases A01-A23, B01-B15, B17-B48,
// C01-C05, C21, C23 and D01-D13 of 4.8 (B16 is a schema test). Arriving later:
// the probe-driven banners C01-C07 and C10-C16 and cases C06-C20, C22 and
// C24-C28 (M3/M4; World.Probes already exists so signatures do not change), the
// customer-wide collapse (C19, C20), and the ten-day strip with cases E01-E05
// (M3; there is no strip_test.go in this version). The test rows for the probe
// cases exist as skipped placeholders so their ids stay visible.
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
