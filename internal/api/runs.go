package api

import (
	"regexp"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// RunStats is the statistics block of a run (architecture §6.2, §6.5
// runs.stats): restic's backup summary reduced to the numbers the console
// keeps, plus the two agent-side facts the state engine needs. All counts are
// non-negative. VSSFallbackVolumes are single upper-case drive letters whose
// snapshot fell back to a plain read (§13.3: the loader intersects them with
// the policy roots). LongRun marks a run cancelled by max_runtime (§6.2). Lease
// is how the fleet start lease went (ADR 0005); it lives in runs.stats rather
// than in a column because it is a per-run fact the console only shows and
// counts, never filters on, and runs.stats is already free-form JSON.
//
// The last six fields are the restore block (ADR 0027): a restore is a run of
// kind restore whose id is the restore request id, and these are its numbers.
// They are additive and omitted when empty, so a backup record encodes exactly
// as before and an old record still validates. The four counters are named after
// restic's own restore summary but are NOT copied from it: restic counts
// directories as restored files and reports a selection that matched nothing as
// an empty summary with exit 0, so the agent counts the files itself from the
// verbose status lines (spike P22-S1 item 4) and takes bytes_restored from the
// summary. RestoreResult is how the whole restore ended and RestoreErrors counts
// files (and, for nothing_matched, restic runs) by countable class; both are
// fixed words and numbers, never a path or a message (RestoreResultClass). Every
// count of the block is at most MaxRestoreCount. RunRecord.Validate ties the
// block to the run's kind and outcome (restoreCouplings).
type RunStats struct {
	FilesNew            int64        `json:"files_new"`
	FilesChanged        int64        `json:"files_changed"`
	FilesUnmodified     int64        `json:"files_unmodified"`
	DirsNew             int64        `json:"dirs_new"`
	DirsChanged         int64        `json:"dirs_changed"`
	DirsUnmodified      int64        `json:"dirs_unmodified"`
	TotalFilesProcessed int64        `json:"total_files_processed"`
	DataAdded           uint64       `json:"data_added"`
	DataAddedPacked     uint64       `json:"data_added_packed"`
	TotalBytesProcessed uint64       `json:"total_bytes_processed"`
	TotalDurationS      float64      `json:"total_duration_s"`
	FailedFiles         int64        `json:"failed_files"`
	VSSFallbackVolumes  []string     `json:"vss_fallback_volumes,omitempty"`
	LongRun             bool         `json:"long_run"`
	Lease               LeaseOutcome `json:"lease,omitempty"`

	FilesRestored int64               `json:"files_restored,omitempty"`
	FilesSkipped  int64               `json:"files_skipped,omitempty"`
	BytesRestored uint64              `json:"bytes_restored,omitempty"`
	BytesSkipped  uint64              `json:"bytes_skipped,omitempty"`
	RestoreResult RestoreResultClass  `json:"restore_result,omitempty"`
	RestoreErrors []RestoreClassCount `json:"restore_errors,omitempty"`
}

// maxVSSVolumes is the number of drive letters.
const maxVSSVolumes = 26

// maxErrorRows caps RunRecord.ErrorCounts: one row per (during, class, ext).
const maxErrorRows = 128

// Validate checks the statistics block.
func (s RunStats) Validate() error {
	var c collector
	c.notNegative("files_new", s.FilesNew)
	c.notNegative("files_changed", s.FilesChanged)
	c.notNegative("files_unmodified", s.FilesUnmodified)
	c.notNegative("dirs_new", s.DirsNew)
	c.notNegative("dirs_changed", s.DirsChanged)
	c.notNegative("dirs_unmodified", s.DirsUnmodified)
	c.notNegative("total_files_processed", s.TotalFilesProcessed)
	c.notNegative("failed_files", s.FailedFiles)
	c.finiteNonNegative("total_duration_s", s.TotalDurationS)
	if len(s.VSSFallbackVolumes) > maxVSSVolumes {
		c.add("vss_fallback_volumes", "has too many entries")
	}
	for i, v := range s.VSSFallbackVolumes {
		if len(v) != 1 || v[0] < 'A' || v[0] > 'Z' {
			c.add(idx("vss_fallback_volumes", i), "must be one upper-case drive letter")
		}
	}
	if s.Lease != "" && !s.Lease.Valid() {
		c.add("lease", msgUnknown)
	}
	c.restoreCount("files_restored", s.FilesRestored)
	c.restoreCount("files_skipped", s.FilesSkipped)
	if s.BytesRestored > MaxRestoreCount {
		c.add("bytes_restored", msgOverSafeInt)
	}
	if s.BytesSkipped > MaxRestoreCount {
		c.add("bytes_skipped", msgOverSafeInt)
	}
	if s.RestoreResult != "" && !s.RestoreResult.Valid() {
		c.add("restore_result", msgUnknown)
	}
	if len(s.RestoreErrors) > len(restoreResultClasses) {
		c.add("restore_errors", "has too many rows")
	}
	seen := map[RestoreResultClass]bool{}
	for i, e := range s.RestoreErrors {
		path := idx("restore_errors", i)
		switch {
		case !e.Class.Valid():
			c.add(path+".class", msgUnknown)
		case !e.Class.Countable():
			c.add(path+".class", "is not a countable class")
		case seen[e.Class]:
			c.add(path+".class", "is listed twice")
		}
		seen[e.Class] = true
		switch {
		case e.Count < 1:
			c.add(path+".count", "must be at least 1")
		case e.Count > MaxRestoreCount:
			c.add(path+".count", msgOverSafeInt)
		}
	}
	return c.err()
}

// MaxRestoreCount bounds every count of the restore block: files_restored,
// files_skipped, bytes_restored, bytes_skipped and each RestoreErrors count. It
// is 2^53-1, the largest integer a JSON number carries exactly to a reader that
// decodes numbers as IEEE-754 doubles (JavaScript, jq, most JSON tooling; RFC
// 8259 §6 names it as the interoperable range). A larger count would be
// rounded by any such reader of the API or of an export, so it is refused as
// corruption rather than passed on wrong. No real restore comes near it: 2^53
// bytes is 8 PiB.
const MaxRestoreCount = 1<<53 - 1

// msgOverSafeInt is the problem of a count above MaxRestoreCount.
const msgOverSafeInt = "is larger than 2^53-1, the largest JSON-safe integer"

// restoreCount records a problem unless 0 <= v <= MaxRestoreCount.
func (c *collector) restoreCount(path string, v int64) {
	switch {
	case v < 0:
		c.add(path, msgNegative)
	case v > MaxRestoreCount:
		c.add(path, msgOverSafeInt)
	}
}

// restoreCoupling is one rule that ties a run's restore block to its kind and
// its outcome. RunRecord.Validate applies every row of restoreCouplings; each
// row has a test of its own (restore_test.go, TestRestoreCouplings).
type restoreCoupling struct {
	id     string // stable name of the row, used by the tests
	reason string // why the rule exists
	check  func(r RunRecord, c *collector)
}

// restoreCouplings is the whole coupling table. An open record (no outcome
// yet) is judged by it too: the open row refuses every restore field on it, and
// the outcome rows leave an open record to that row so one mistake is one
// problem.
//
//	id                 rule                                                      reason
//	kind               restore block only on kind=restore                        a backup or maintenance record with restore numbers is a mixed-up record, and the console would count it as a restore
//	open               open restore: no counts, no result, no error rows         the numbers and the class say how a run ended; on a run that has not ended they are a guess the console would show as fact, and the agent writes the row open before restic starts, with nothing to report yet
//	success            success: no restore_result, no restore_errors,            restic exits 0 when nothing matched (spike item 4); a success that placed no file is the quiet green this product exists to prevent
//	                   files_restored+files_skipped > 0
//	nothing_matched    nothing_matched: outcome failed, all four counts 0        spike item 4 / ADR 0027: never delivered; a restored or skipped file or byte means something matched, which is partial
//	nothing_rows       nothing_matched: no error rows                            ADR 0027 reports the runs that matched nothing under nothing_matched only when another run restored files, which is partial; with the whole restore nothing_matched a row would claim files or runs that the class says do not exist
//	partial            partial: outcome partial                                  ADR 0027: restored with errors is delivered with error class partial, from the run's outcome
//	partial_rows       partial: at least one error row                           "with errors" that names no error is the quiet green: the operator is told RESTORED WITH ERRORS and can see none
//	outcome_partial    outcome partial: restore_result partial                   outcome partial with no class says a restore had errors and not which; a partial outcome with another class is refused by that class's own row
//	cancelled          cancelled: outcome cancelled                              the console closes the request from the outcome; a mismatch would show one state and count another
//	interrupted        interrupted: outcome interrupted                          a restore never resumes after a restart (ADR 0027); the outcome is what the console reads
//	failed             every other class: outcome failed                         the class names why it failed; on any other outcome it contradicts the outcome
var restoreCouplings = []restoreCoupling{
	{"kind", "the restore block belongs to a restore run only", func(r RunRecord, c *collector) {
		if r.Kind == restic.KindRestore {
			return
		}
		restoreFieldsSet(r.Stats, c, "is only allowed on a restore run")
	}},
	{"open", "a restore that has not ended has nothing to report yet", func(r RunRecord, c *collector) {
		if r.Kind == restic.KindRestore && r.Open() {
			restoreFieldsSet(r.Stats, c, "is not allowed on an open restore")
		}
	}},
	{"success", "a successful restore placed or kept at least one file and has nothing to report", func(r RunRecord, c *collector) {
		if r.Kind != restic.KindRestore || r.Outcome != restic.OutcomeSuccess {
			return
		}
		if r.Stats.RestoreResult != "" {
			c.add("stats.restore_result", "is not allowed on a successful restore")
		}
		if len(r.Stats.RestoreErrors) != 0 {
			c.add("stats.restore_errors", "is not allowed on a successful restore")
		}
		if r.Stats.FilesRestored <= 0 && r.Stats.FilesSkipped <= 0 {
			c.add("stats.files_restored", "a successful restore must restore or skip at least one file")
		}
	}},
	{"nothing_matched", "nothing matched ends the restore failed with nothing restored or skipped", func(r RunRecord, c *collector) {
		if r.Kind != restic.KindRestore || r.Stats.RestoreResult != RestoreResultNothingMatched {
			return
		}
		requireRestoreOutcome(r, c, restic.OutcomeFailed)
		s := r.Stats
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"files_restored", s.FilesRestored != 0}, {"files_skipped", s.FilesSkipped != 0},
			{"bytes_restored", s.BytesRestored != 0}, {"bytes_skipped", s.BytesSkipped != 0},
		} {
			if f.set {
				c.add("stats."+f.name, "must be 0 when nothing matched")
			}
		}
	}},
	{"nothing_rows", "a restore that matched nothing has no error rows to count", func(r RunRecord, c *collector) {
		if restoreClosedNotSuccess(r) && r.Stats.RestoreResult == RestoreResultNothingMatched && len(r.Stats.RestoreErrors) != 0 {
			c.add("stats.restore_errors", "is not allowed when nothing matched")
		}
	}},
	{"partial", "restored with errors is outcome partial", func(r RunRecord, c *collector) {
		if r.Kind == restic.KindRestore && r.Stats.RestoreResult == RestoreResultPartial {
			requireRestoreOutcome(r, c, restic.OutcomePartial)
		}
	}},
	{"partial_rows", "restored with errors names at least one error", func(r RunRecord, c *collector) {
		if restoreClosedNotSuccess(r) && r.Stats.RestoreResult == RestoreResultPartial && len(r.Stats.RestoreErrors) == 0 {
			c.add("stats.restore_errors", "is required when the result is partial")
		}
	}},
	{"outcome_partial", "a partial restore outcome says which class it is", func(r RunRecord, c *collector) {
		if r.Kind == restic.KindRestore && r.Outcome == restic.OutcomePartial && r.Stats.RestoreResult == "" {
			c.add("stats.restore_result", "is required (partial) when the outcome is partial")
		}
	}},
	{"cancelled", "a cancelled restore is outcome cancelled", func(r RunRecord, c *collector) {
		if r.Kind == restic.KindRestore && r.Stats.RestoreResult == RestoreResultCancelled {
			requireRestoreOutcome(r, c, restic.OutcomeCancelled)
		}
	}},
	{"interrupted", "an interrupted restore is outcome interrupted", func(r RunRecord, c *collector) {
		if r.Kind == restic.KindRestore && r.Stats.RestoreResult == RestoreResultInterrupted {
			requireRestoreOutcome(r, c, restic.OutcomeInterrupted)
		}
	}},
	{"failed", "every other class names why a restore failed", func(r RunRecord, c *collector) {
		if r.Kind != restic.KindRestore || !r.Stats.RestoreResult.Valid() {
			return
		}
		switch r.Stats.RestoreResult {
		case RestoreResultNothingMatched, RestoreResultPartial, RestoreResultCancelled, RestoreResultInterrupted:
			return
		}
		requireRestoreOutcome(r, c, restic.OutcomeFailed)
	}},
}

// restoreClosedNotSuccess reports whether r is a restore that has ended other than in
// success: the records the error-row rules judge. An open restore belongs to the
// open row and a successful one to the success row, which report the block
// themselves, so one mistake is one problem.
func restoreClosedNotSuccess(r RunRecord) bool {
	return r.Kind == restic.KindRestore && !r.Open() && r.Outcome != restic.OutcomeSuccess
}

// restoreFieldsSet records a problem, with msg, for every restore field of s
// that is set: the four counts, the result and the error rows.
func restoreFieldsSet(s RunStats, c *collector, msg string) {
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"files_restored", s.FilesRestored != 0}, {"files_skipped", s.FilesSkipped != 0},
		{"bytes_restored", s.BytesRestored != 0}, {"bytes_skipped", s.BytesSkipped != 0},
		{"restore_result", s.RestoreResult != ""}, {"restore_errors", len(s.RestoreErrors) != 0},
	} {
		if f.set {
			c.add("stats."+f.name, msg)
		}
	}
}

// requireRestoreOutcome records a problem when the record's outcome is not
// want. A successful record is left to the success row and an open record to
// the open row, which report the class themselves, so one mistake is one
// problem.
func requireRestoreOutcome(r RunRecord, c *collector, want restic.Outcome) {
	if r.Outcome != want && r.Outcome != restic.OutcomeSuccess && !r.Open() {
		c.add("stats.restore_result", "requires outcome "+string(want))
	}
}

// RunRecord is one run as the agent reports it (architecture §5.1 /runs, §6.2,
// §6.5). The agent writes the row before restic starts and may send it open
// (no FinishedAt, no Outcome) and again, closed, later; the id is the
// idempotency key. Times are the device's clock; the console converts them to
// server time with the clock offset (DH5).
//
// Couplings enforced by Validate (they are the schema's CHECKs): Outcome set if
// and only if FinishedAt set; deferred outcome if and only if DeferredReason
// set; SnapshotID only on a backup; no time before MinTimestamp; FinishedAt not
// before StartedAt; and the restore block of Stats only on a restore, paired
// with the outcome row by row (restoreCouplings). ErrorCounts is the path-free
// projection of DH15, never a message or a file name.
//
// AgentVersion is a display label and Validate does not judge it: a record the
// agent's Validate refuses is journaled as rejected with no retry
// (internal/agent/loops.go), and a backup record must never be lost over a label.
// The console keeps it only when VersionShape accepts it and stores UnknownVersion
// otherwise (agentapi.handleRuns). AssertPathFree still covers it, like every string.
type RunRecord struct {
	ID             string                   `json:"id"` // 26-character ULID, generated by the agent
	Kind           restic.Kind              `json:"kind"`
	Trigger        Trigger                  `json:"trigger"`
	StartedAt      time.Time                `json:"started_at"`
	FinishedAt     *time.Time               `json:"finished_at,omitempty"`
	Outcome        restic.Outcome           `json:"outcome,omitempty"` // empty = open
	ExitCode       *int                     `json:"exit_code,omitempty"`
	SnapshotID     string                   `json:"snapshot_id,omitempty"` // 64 lowercase hex, backups only
	Stats          RunStats                 `json:"stats"`
	ErrorCounts    []restic.ErrorProjection `json:"error_counts,omitempty"`
	ErrorClass     restic.ErrorClass        `json:"error_class,omitempty"`
	Attempt        int                      `json:"attempt"` // 1 for the first try
	DeferredReason restic.DeferredReason    `json:"deferred_reason,omitempty"`
	PolicyVersion  int                      `json:"policy_version"` // 0 = none yet
	AgentVersion   string                   `json:"agent_version"`
	Initial        bool                     `json:"initial"` // the first backup of this device
}

// Open reports whether the record is an open run (no outcome yet).
func (r RunRecord) Open() bool { return r.Outcome == "" }

// Validate checks the record and its couplings.
func (r RunRecord) Validate() error {
	var c collector
	c.ulid("id", r.ID)
	if !r.Kind.Recorded() {
		c.add("kind", msgUnknown)
	}
	if !has(triggers, r.Trigger) {
		c.add("trigger", msgUnknown)
	}
	c.notBeforeMin("started_at", r.StartedAt)
	if r.FinishedAt != nil {
		c.notBeforeMin("finished_at", *r.FinishedAt)
		if !r.FinishedAt.Before(MinTimestamp) && r.FinishedAt.Before(r.StartedAt) {
			c.add("finished_at", "is before started_at")
		}
	}
	if r.Outcome != "" && !has(outcomes, r.Outcome) {
		c.add("outcome", msgUnknown)
	}
	if (r.Outcome == "") != (r.FinishedAt == nil) {
		if r.Outcome == "" {
			c.add("outcome", "is required when finished_at is set")
		} else {
			c.add("finished_at", "is required when outcome is set")
		}
	}
	if r.SnapshotID != "" {
		c.hexFixed("snapshot_id", r.SnapshotID, 64)
		if r.Kind != restic.KindBackup {
			c.add("snapshot_id", "is only allowed on a backup")
		}
	}
	c.sub("stats", r.Stats.Validate())
	for _, rc := range restoreCouplings {
		rc.check(r, &c)
	}
	if len(r.ErrorCounts) > maxErrorRows {
		c.add("error_counts", "has too many rows")
	}
	for i, e := range r.ErrorCounts {
		validateProjection(&c, idx("error_counts", i), e)
	}
	if r.ErrorClass != "" && !r.ErrorClass.Valid() {
		c.add("error_class", msgUnknown)
	}
	if r.Attempt < 1 {
		c.add("attempt", "must be at least 1")
	}
	switch {
	case r.DeferredReason != "" && !has(deferredReasons, r.DeferredReason):
		c.add("deferred_reason", msgUnknown)
	case r.Outcome == restic.OutcomeDeferred && r.DeferredReason == "":
		c.add("deferred_reason", "is required when outcome is deferred")
	case r.DeferredReason != "" && r.Outcome != restic.OutcomeDeferred:
		c.add("deferred_reason", "is only allowed when outcome is deferred")
	}
	c.notNegative("policy_version", int64(r.PolicyVersion))
	return c.err()
}

// validateProjection checks one {during, class, ext, count} row (DH15): the
// phase is a short word, the class is in the vocabulary, the extension is one
// restic.Ext would have let through (its allow-list), the count is positive.
func validateProjection(c *collector, path string, e restic.ErrorProjection) {
	if len(e.During) > 32 || !isWord(e.During) {
		c.add(path+".during", "must be a short lower-case word")
	}
	if !e.Class.Valid() {
		c.add(path+".class", msgUnknown)
	}
	if e.Ext != "" && restic.Ext("x."+e.Ext) != e.Ext {
		c.add(path+".ext", "is not an allowed extension")
	}
	if e.Count < 1 {
		c.add(path+".count", "must be at least 1")
	}
}

// isWord reports whether s is empty or made of [a-z0-9_-].
func isWord(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '_', ch == '-':
		default:
			return false
		}
	}
	return true
}

// RunsRequest is the body of POST /runs (architecture §5.1): 1 to
// MaxRunsPerRequest records, at most MaxRunsBody bytes, RunsPerMinute calls per minute.
type RunsRequest struct {
	Runs []RunRecord `json:"runs"`
}

// Validate checks the batch and every record in it; problems of record i are
// reported under "runs[i]".
func (r RunsRequest) Validate() error {
	var c collector
	if n := len(r.Runs); n < 1 {
		c.add("runs", "must hold at least one run")
	} else if n > MaxRunsPerRequest {
		c.add("runs", "has too many runs")
	}
	for i, run := range r.Runs {
		c.sub(idx("runs", i), run.Validate())
	}
	return c.err()
}

// RunResult is the console's verdict on one record. Reason is empty unless the
// status is rejected, then it is one of the Reason* codes.
type RunResult struct {
	ID     string    `json:"id"`
	Status RunStatus `json:"status"`
	Reason string    `json:"reason,omitempty"`
}

// RunsResponse is the 200 body of POST /runs: one result per record, in order.
// The agent marks a run delivered only after created, updated or duplicate
// (architecture §6.2); a rejected run would be rejected again if resent unchanged.
type RunsResponse struct {
	Results    []RunResult `json:"results"`
	ServerTime time.Time   `json:"server_time"`
}

// versionShapeRE is what the agent's build stamp can look like (internal/buildinfo,
// set by the Makefile from CRYO_MSI_VERSION or `git describe --tags --always
// --dirty`, or "dev"): one word, then dotted numbers, then up to three
// "-word" or "+word" parts, each optionally followed by ".digits". Whatever sits
// after a dot must be a number, so a name with an extension ("payroll.xlsx",
// "Chart.docx") and anything with a space, slash or colon is not a version. A bare
// word ("test", a commit hash) is: this check keeps file names and sentences out,
// it cannot tell a word from a version.
var versionShapeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,31}(?:\.[0-9]{1,5}){0,3}(?:[-+][A-Za-z0-9]{1,16}(?:\.[0-9]{1,5})?){0,3}$`)

// VersionShape reports whether s looks like a build version (see versionShapeRE).
// It is how the console decides a version string is safe to keep and to show. It
// never decides whether a run is accepted: see RunRecord.
func VersionShape(s string) bool { return len(s) <= 64 && versionShapeRE.MatchString(s) }

// UnknownVersion is what the console stores for a run whose agent_version is not
// version-shaped. It is itself version-shaped, so the pages show it as it is.
const UnknownVersion = "unknown"

// VersionLabel is s when it is version-shaped and UnknownVersion otherwise: what an
// agent reports and what a page may show. A label decides nothing else.
func VersionLabel(s string) string {
	if VersionShape(s) {
		return s
	}
	return UnknownVersion
}
