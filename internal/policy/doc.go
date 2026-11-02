// Package policy is the pure, standard-library-only core of CryoBackup's
// policy: the nine-section policy document, its defaults, decoding, merge-patch
// and validation, the backup-window and throttle mini-languages, due-ness
// computation, and the translation of a selection into backup roots and a
// restic exclude file. The agent and the console share it; neither depends on
// the other through it. It does no I/O, reads no clock (callers pass the time
// in), logs nothing and imports only the standard library.
//
// Sources: architecture §3.6 (document shape and defaults), §6.1 (scheduler),
// §6.3 (what the backup builder needs), §7.3 step 3 (why keep-within is
// mandatory), matrix rows r22-r34 and r47-r62; engine §1.5 (exclusion syntax);
// docs/recon-notes/sweep_acronis_datto_axcient_veeam_urbackup.md §5.5 A, C, E
// (the mini-languages).
//
// # The document
//
// Policy has nine sections, each a JSON object with the snake_case field names
// of architecture §3.6: schedule, conditions, selection, retention, throttle,
// sla, maintenance, agent_permissions, notification_overrides. The policies
// table keeps one column per section; Encode and Decode convert between that
// form and Policy, and DecodeDocument reads the single-document form.
//
// Fields are plain values with no omitempty, so the encoded form is complete;
// the exceptions are retention.preset and retention.rules (a union: exactly one
// is present) and the tri-state notification_overrides.digest_include. A zero
// number or string means none, unlimited, off or inherit, as each field's
// comment says. Default(class) returns the proposed defaults (laptop: battery
// floor 30%, otherwise none); they are proposals for the init wizard and the
// pilot to tune, and Default(...).Validate() is always nil.
//
// Decoding is tolerant on read and strict on write:
//
//   - Decode ignores unknown sections and fields; a missing section, or a field
//     missing from a present section, takes its default. The two unions are
//     the exception: naming selection roots or presets drops the default
//     preset, and naming a retention preset or rules drops the default
//     retention (a preset without keep_within takes the preset's own).
//   - DecodeStrict and DecodeDocument(b, true) require all nine sections,
//     reject unknown sections and fields, reject trailing data and default
//     nothing; a field left out is its zero value and Validate then reports it.
//
// Canonical is the deterministic encoding (keys sorted at every level, no
// whitespace) and ETag is the quoted SHA-256 of it. MergePatch is RFC 7396;
// Patch applies a patch to a Policy through Encode, MergePatch, DecodeStrict
// and Validate, so it can neither add unknown keys nor leave the policy
// invalid. Because arrays and scalars replace and null deletes, switching
// retention from preset to rules is {"retention":{"preset":null,"rules":{...}}}.
//
// # Validation
//
// Validate returns nil or a *ValidationError holding every problem as a
// {Path, Message} pair sorted by path (use errors.As). The ranges are the
// bounds the agent and console can honour, not promises; see the checks in
// validate.go. Notable rules: retention.keep_within is required in both
// retention forms and must be a restic duration greater than zero
// (1y2m3d4h, each part optional); sla.unseen_after_hours must be at least
// stale_after_hours because a device must be stale before it can be unseen;
// selection.path_flags keys must be listed roots.
//
// # The window mini-language
//
// ParseWindow reads the backup-window language of matrix r26. A window gates
// the start of a run and never stops one that is running.
//
//	window  := item (';' item)*
//	item    := days '/' hours (',' hours)*
//	days    := '*' | dayspec (',' dayspec)*
//	dayspec := day | day '-' day          inclusive; Fri-Mon wraps over Sunday
//	day     := 1..7 (Mon=1) | Mon|Tue|Wed|Thu|Fri|Sat|Sun    case-insensitive
//	hours   := time '-' time
//	time    := H | H:MM                   0 <= H <= 24, MM 00-59; 24 only as 24 or 24:00
//
// Whitespace around tokens is ignored. A range whose end is at or before its
// start wraps past midnight: 20-6 is 20:00 to 06:00 the next day, and Sun/20-6
// spills into Monday. A range cannot start at 24:00 (it would be empty or
// belong to the next day). "*/0-24" and "1-7/0-24" are always open.
//
//	Mon-Fri/8-18;Sat,Sun/0-24             weekdays 08:00-18:00, weekends all day
//	Mon-Fri/8:00-9:00, 19:30-20:30        two slots a day
//	*/20-6                                every night
//
// Errors are *ParseError values carrying the byte offset of the first problem,
// for example "at 6: expected hour". A Window is a comparable value with
// minute resolution; String is the canonical form and ParseWindow(w.String())
// equals w. Time zones: a Window has none. Contains and NextOpen read the wall
// clock of the time they are given, so callers pass device-local time (Go's
// time with the Windows zone), and a 23- or 25-hour DST day is handled by the
// zone: on 2026-03-08 in America/Chicago the window Sun/1-3 is open for 60
// minutes, on 2026-11-01 for 180. NextOpen advances in real minutes and reads
// the wall clock at each step, never adding 24 hours.
//
// # The throttle mini-language
//
// Rates are KiB/s, the only unit restic takes. ParseRate accepts "0" or
// "unlimited" (0), "<n>" and "<n>k" (KiB/s) and "<n>m" (MiB/s). Percent rates
// ("50%") are not supported and return an error saying so.
// ParseThrottleSpec reads the compact form of the recon notes:
//
//	spec := rate (';' rate '@' window)*
//	60;10@Mon-Fri/8-18;5@Sat,Sun/0-24
//
// 60 KiB/s by default, 10 on weekdays 08:00-18:00, 5 at weekends; one rate
// applies to both directions and each window is a single window item. On
// overlap the later window in the list wins ("most specific" is the one the
// operator lists last). Effective(now) returns the limits in force and
// NextBoundary(from) the next minute at which they change, which is when the
// agent restarts the restic child because restic fixes its limits for the life
// of the process.
//
// # Due-ness
//
// Due is pure: it reads a DueInput and returns a Decision. There is no stored
// next-run; everything is recomputed from the last success (matrix r22).
//
// Interval mode. interval = interval_minutes; offset = StableOffset(deviceID,
// min(interval/4, 1h)); next = LastSuccess + interval + offset. Slots start at
// LastSuccess + k*interval and SlotStart is the latest slot not after Now.
//
//   - No LastSuccess: due now, reason "never" (SlotStart = Now).
//   - Now < next: not due, reason "not_yet", NotBefore = next.
//   - catchup = true: due once Now >= next (inclusive). However many intervals
//     were missed, one run: a laptop asleep for a week gets one run, not seven.
//     Reason "catchup" when Now - next >= interval, otherwise "interval".
//   - catchup = false: due only if no attempt has started in the current slot
//     (LastAttemptStart < SlotStart). Past slots are skipped, never run late; a
//     slot already attempted gives "slot_attempted" with NotBefore = the next
//     slot's start plus the offset.
//   - A LastSuccess in the future (the clock went backwards) is not waited for:
//     due, reason "clock_skew".
//
// Fixed mode. Candidates are every fixed_times entry, "HH:MM" local, each day.
// SlotStart is the latest candidate not after Now. Due when LastAttemptStart <
// SlotStart (reason "fixed"); otherwise "slot_attempted" with NotBefore = the
// next candidate. A fixed schedule with no usable time falls back to interval
// mode rather than going silent. Because SlotStart is by definition the latest
// candidate, catchup does not change fixed-mode results: a missed slot is
// always folded into the latest one.
//
// Window. A due run whose time is outside the schedule's window is delayed, not
// cancelled: Due = false, reason "window", NotBefore = Window.NextOpen(Now)
// (zero if it never opens). An unparsable or empty window never blocks a
// backup (Validate rejects it earlier).
//
// Offset and jitter. StableOffset is a fixed per-device delay, an avalanche-
// mixed FNV-1a hash of the device id mapped evenly onto [0, max); it is part
// of next, so a fleet's due times spread and stay spread. Jitter(deviceID,
// slot, max) is a deterministic per-slot delay in [0, max) that the caller
// applies after it first sees Due for that slot, with max = jitter_minutes.
// Due itself does not add jitter, so NotBefore and "exactly at next" stay
// exact.
//
// Worked example, interval 1440 min, catchup on, offset o for this device
// (0 <= o < 1h), LastSuccess Monday 03:00:
//
//	Now Monday 15:00        not due, "not_yet", NotBefore Tuesday 03:00 + o
//	Now Tuesday 03:00 + o   due, "interval"   (inclusive)
//	Now next Monday 09:00   due, "catchup"    (seven intervals missed, one run)
//	... and the window is Mon-Fri/8-18 on a Saturday: not due, "window",
//	    NotBefore Monday 08:00; at that instant Due says "catchup".
//
// With catchup off and the same Now next Monday 09:00: due, reason "interval",
// SlotStart = Monday 03:00 + 7 days; once the agent has started that attempt
// (LastAttemptStart >= SlotStart) the answer is "slot_attempted" until the
// following slot.
//
// DST: slots are absolute durations, so a daily interval across spring forward
// falls one local hour later; the window and fixed times are wall-clock in the
// device zone, so 08:00 stays 08:00. On 2026-03-08 in America/Chicago the
// fixed time 03:30 is 2.5 real hours after 00:00 (one hour is skipped), a closed window opening at
// 08:00 CDT is 11 real hours after 20:00 CST the evening before.
//
// What Due does not do: retry backoff after a failure, run-on-wake, the
// startup delay, the run-time limit and the start conditions (battery,
// metered, idle, Wi-Fi, VPN) belong to the agent (architecture §6.1, §6.6);
// conditions are M8.
//
// # Selection, roots and the exclude file
//
// Roots expands selection.roots and selection.presets, using facts the agent
// discovers (Environment), into the sorted, deduplicated list of absolute
// roots, and fails loudly rather than returning a partial list. Presets:
// all_user_profiles (each profile directory), documents_desktop (Documents and
// Desktop of each profile), fixed_drives (each X:\).
//
// ExcludeFile renders the restic --iexclude-file content (case-insensitive, as
// NTFS is). Layout, in order, UTF-8 with LF line endings and "#" comments:
//
//  1. mandatory excludes (MandatoryExcludes): CryoBackup's own folder under
//     ProgramData (state and the restic cache), the per-profile default restic
//     cache, pagefile.sys, hiberfil.sys, swapfile.sys, DumpStack.log.tmp,
//     $Recycle.Bin, System Volume Information;
//  2. the published baseline (Baseline, matrix r50): temp folders, Recycle Bin,
//     paging/hibernation/swap files, System Volume Information, each profile's
//     WindowsApps folder (app-execution aliases, reparse points restic cannot
//     archive), Chrome, Edge and Firefox caches, Prefetch and thumbnail caches;
//  3. each exclusion preset selected by an "@name" entry of selection.excludes
//     (ExclusionPresets, matrix r54): browser_caches, vm_images (*.vhd *.vhdx
//     *.vmdk *.iso), installers (*.msi and *.exe under Downloads), dev_caches
//     (node_modules, __pycache__ and package caches; deliberately not .git);
//  4. the remaining selection.excludes entries verbatim; "!pattern" re-includes;
//  5. only when step 4 contains a re-include: the mandatory excludes again.
//
// "@name" is how a preset is selected without adding a field to the §3.6
// document. exclude_larger_than, exclude_cloud_files, includes, path_flags and
// the VSS options are read by the agent straight from Selection and turned into
// flags (architecture §6.3); no file is generated for them. The Windows
// FilesNotToBackup registry key (r50, M2) is the agent's to read; it appends
// what it finds through Selection.Excludes.
//
// Baseline and preset entries that start with %PROFILE% expand to one line per
// profile directory and %SYSTEMDRIVE% to the system drive, so temp and cache
// folders are anchored to real locations instead of matching a folder called
// Temp anywhere. (An unanchored multi-component pattern also matches the
// ancestors of the backup root; the live test's own temp tree lives under
// AppData\Local\Temp and was wholly excluded by one before the baseline was
// anchored.)
//
// # restic's pattern semantics on Windows, settled against the binary
//
// internal/policy's live test (CRYO_RESTIC set to restic 0.19.1; Windows only)
// runs the generated file through restic backup --dry-run --json -vv
// --iexclude-file against a temp tree. What restic 0.19.1 does, observed:
//
//   - Separator: both backslash and forward slash are honoured in patterns, in
//     absolute (drive-qualified) and in relative multi-component patterns. The
//     engine sheet left the Windows form open; this package emits backslash,
//     the native form, and the probe subtest logs that forward slash gives an
//     identical result. A backslash is a separator on Windows, not an escape.
//   - Case: --iexclude-file matches case-insensitively.
//   - Dollar: restic expands $VAR in exclude files, so $Recycle.Bin must be
//     written $$Recycle.Bin. A control run shows the single-dollar form
//     excludes nothing. ExcludeFile doubles every '$', in custom patterns too,
//     so "~$*" (Office lock files) means a literal dollar.
//   - Order: the last matching line wins; "!pattern" re-includes. That is why
//     the mandatory block is repeated after any custom re-include. A control
//     run proves "pagefile.sys" then "!pagefile.sys" keeps the file.
//   - Components: a single name or glob without a separator matches at any
//     depth; a "**" component matches any number of directories; single "*"
//     does not cross a separator; a '[' in a path is neutralised as "[[]".
//   - Not verified: patterns under --use-fs-snapshot (VSS needs elevation and a
//     real volume; engine §1.1 and the lab plan cover it).
//
// # Retention
//
// Retention names a preset or explicit keep counts (-1 = unlimited, 0 = unused)
// plus a mandatory keep_within and optional keep_tag entries. Resolve turns it
// into Rules, the restic forget flags; the section's keep_within wins over the
// preset's own. Presets (proposals, matrix r62):
//
//	30_days     keep-within 30d
//	90_days     keep-within 90d
//	1_year_gfs  keep-within 14d, 30 daily, 13 weekly, 12 monthly
//	7_years     keep-within 14d, 30 daily, 13 weekly, 24 monthly, 7 yearly
//
// keep-within is always passed to forget so that a flood of snapshots cannot
// push real history out of the keep windows (architecture §7.3 step 3).
//
// # Not here
//
// The retention calendar preview (M9), the conditions evaluator and retry
// backoff (M8 and the agent), the FilesNotToBackup registry reader (M2) and any
// restic invocation (internal/restic) are elsewhere; this package only supplies
// the values they need.
package policy
