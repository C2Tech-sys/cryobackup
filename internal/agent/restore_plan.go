package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/agent/restoreplace"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// restoreGroup is the part of a selection on one drive letter: one restic run
// (ADR 0027: one run per volume the selection touches). key is the drive
// segment exactly as the snapshot path has it ("C"); restic matches it
// case-sensitively, so "/c/x" and "/C/x" are two groups whose runs are judged
// separately. letter is the volume (key upper-cased).
//
// PATH-BEARING: rels and lines are the selection's paths. Never log a group.
type restoreGroup struct {
	key    string
	letter string
	rels   []restoreplace.Rel
	lines  []string
}

// groupSelection splits a validated selection's paths by drive segment, in a
// stable order. Each path is "/<key>/<name>/..." (api.RestoreSelection.Validate
// guarantees the shape).
func groupSelection(paths []string) []restoreGroup {
	by := map[string]*restoreGroup{}
	var keys []string
	for _, p := range paths {
		rest := strings.TrimPrefix(p, "/")
		key, below, _ := strings.Cut(rest, "/")
		g, ok := by[key]
		if !ok {
			g = &restoreGroup{key: key, letter: strings.ToUpper(key)}
			by[key] = g
			keys = append(keys, key)
		}
		g.rels = append(g.rels, restoreplace.Rel(strings.Split(below, "/")))
		g.lines = append(g.lines, includeLine("/"+below))
	}
	sort.Strings(keys)
	out := make([]restoreGroup, 0, len(keys))
	for _, k := range keys {
		out = append(out, *by[k])
	}
	return out
}

// includeLine escapes one include-file line (ADR 0027, "The include file";
// p22-spike.md:124-158): restic has no escape character, so every '[' is
// written "[[]" and then every '$' is written "[$]" (in that order: the
// second step adds brackets the first must not see). '*' and '?' cannot occur
// in an NTFS name and the wire contract refuses them. Nothing else is touched:
// no case folding, no normalisation, no trimming.
func includeLine(rel string) string {
	rel = strings.ReplaceAll(rel, "[", "[[]")
	return strings.ReplaceAll(rel, "$", "[$]")
}

// includeContent is the include file of a group: UTF-8 without BOM, one line
// per selected item, LF line ends.
func includeContent(lines []string) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// withIncludeFiles writes one include file per group into dir (the agent's
// temp folder under the state directory, SYSTEM and Administrators only), runs
// f with their paths, and deletes them on every way out of f, a panic
// included (ADR 0027: deleted on every exit). A file a killed process left
// behind is deleted when the agent starts (New empties the temp folder).
func withIncludeFiles(dir, id string, groups []restoreGroup, f func(files []string) error) (err error) {
	var files []string
	defer func() {
		for _, p := range files {
			if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
				err = errors.New("agent: an include file could not be deleted")
			}
		}
	}()
	for i, g := range groups {
		p := filepath.Join(dir, fmt.Sprintf("restore-%s-%d.include", id, i))
		files = append(files, p)
		if werr := os.WriteFile(p, includeContent(g.lines), 0o600); werr != nil {
			return errors.New("agent: an include file could not be written")
		}
	}
	return f(files)
}

// restoreOutcome is what a restore did, before it is turned into the run
// record (restoreVerdict). fatal is a run-level class (disk, guard_refused,
// auth, repo_unreachable, other, cancelled, interrupted) or "" when the
// restore ran to its end. runs and nothing count the restic runs and those
// that matched nothing. counts is the agent's own placement or side-folder
// count; it holds no path.
type restoreOutcome struct {
	fatal   api.RestoreResultClass
	runs    int
	nothing int
	counts  restoreplace.Counts
	exit    *int

	// The side folder (side mode): sideTop is <side root>\<restore id> once
	// the agent created it; sideWritten is set before restic's first real run
	// into it; sideUnsecured is true from that run until the group's reset,
	// owner change and check completed (foreman ruling, 2026-11-25: a side
	// restore that does not succeed removes its side folder, so no file keeps
	// the snapshot's owner and DACL).
	sideTop       string
	sideWritten   bool
	sideUnsecured bool
}

// removeSide reports whether a side restore that ended with outcome must
// remove its side folder before its run is closed: every outcome but success,
// except a partial one whose resets all completed (that folder is secured and
// is the operator's result).
func removeSide(outcome restic.Outcome, unsecured bool) bool {
	switch outcome {
	case restic.OutcomeSuccess:
		return false
	case restic.OutcomePartial:
		return unsecured
	}
	return true
}

// sideRemovedVerdict is the record of a side restore whose folder was removed
// (removeSide): nothing it wrote is left, so the four counts are 0; a
// cancelled or interrupted restore keeps its outcome; anything else ends
// failed, keeping its run-level class (other for a partial one). The rows
// stay: they say what went wrong.
func sideRemovedVerdict(outcome restic.Outcome, st api.RunStats) (restic.Outcome, api.RunStats) {
	st.FilesRestored, st.FilesSkipped, st.BytesRestored, st.BytesSkipped = 0, 0, 0, 0
	switch outcome {
	case restic.OutcomeCancelled, restic.OutcomeInterrupted, restic.OutcomeFailed:
		return outcome, st
	}
	st.RestoreResult = api.RestoreResultOther
	return restic.OutcomeFailed, st
}

// restoreVerdict turns an outcome into the run's outcome and the restore block
// of its stats, row by row of the coupling table (api restoreCouplings, ADR
// 0027 "Nothing matched is judged per restic run", "The outcome"):
//
//	cancelled / interrupted   outcome of the same name, counts kept
//	other run-level class     failed with that class, counts kept
//	every run matched nothing failed nothing_matched, all four counts 0, no rows
//	items, no error, no miss  success, no class, no rows
//	items and errors/misses   partial (class partial) with a row per class;
//	                          nothing_matched counts the runs that matched nothing
//	no item, errors           failed with the dominant counted class, rows kept
//	no item, no error         failed other (something matched, nothing could be placed)
func restoreVerdict(o restoreOutcome) (restic.Outcome, api.RunStats) {
	c := o.counts
	st := api.RunStats{
		FilesRestored: c.Restored, FilesSkipped: c.Skipped,
		BytesRestored: c.BytesRestored, BytesSkipped: c.BytesSkipped,
	}
	rows := restoreRows(c)
	if o.nothing > 0 && o.nothing < o.runs {
		rows = append(rows, api.RestoreClassCount{Class: api.RestoreResultNothingMatched, Count: int64(o.nothing)})
		sortRows(rows)
	}
	items := c.Restored + c.Skipped
	switch {
	case o.fatal == api.RestoreResultCancelled:
		st.RestoreResult, st.RestoreErrors = o.fatal, rows
		return restic.OutcomeCancelled, st
	case o.fatal == api.RestoreResultInterrupted:
		st.RestoreResult, st.RestoreErrors = o.fatal, rows
		return restic.OutcomeInterrupted, st
	case o.fatal != "":
		st.RestoreResult, st.RestoreErrors = o.fatal, rows
		return restic.OutcomeFailed, st
	case o.runs > 0 && o.nothing == o.runs:
		return restic.OutcomeFailed, api.RunStats{RestoreResult: api.RestoreResultNothingMatched}
	case items > 0 && len(rows) == 0:
		return restic.OutcomeSuccess, st
	case items > 0:
		st.RestoreResult, st.RestoreErrors = api.RestoreResultPartial, rows
		return restic.OutcomePartial, st
	case c.ErrorTotal() > 0:
		st.RestoreResult, st.RestoreErrors = dominantClass(c), rows
		return restic.OutcomeFailed, st
	}
	st.RestoreResult, st.RestoreErrors = api.RestoreResultOther, rows
	return restic.OutcomeFailed, st
}

// restoreRows turns the counted classes into wire rows, capped at the wire's
// maximum, in a stable order.
func restoreRows(c restoreplace.Counts) []api.RestoreClassCount {
	var rows []api.RestoreClassCount
	for _, cl := range c.Classes() {
		n := c.Errors[cl]
		if n > api.MaxRestoreCount {
			n = api.MaxRestoreCount
		}
		rows = append(rows, api.RestoreClassCount{Class: api.RestoreResultClass(cl), Count: n})
	}
	return rows
}

func sortRows(rows []api.RestoreClassCount) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Class < rows[j].Class })
}

// dominantClass is the counted class with the most items (ties: the
// alphabetically first, so the choice is stable).
func dominantClass(c restoreplace.Counts) api.RestoreResultClass {
	best, bestN := api.RestoreResultOther, int64(0)
	for _, cl := range c.Classes() {
		if n := c.Errors[cl]; n > bestN {
			best, bestN = api.RestoreResultClass(cl), n
		}
	}
	return best
}

// resticRestoreErrors counts restic's own per-item errors of a restore run by
// class (ADR 0027: errors are classified on the device; restic's error lines
// carry absolute paths and the decoder already dropped them): a file in use
// is target_locked, a refused access access_denied, anything else other.
func resticRestoreErrors(counts restic.ErrorCounts, into *restoreplace.Counts) {
	for k, n := range counts {
		if k.During != "restore" || n <= 0 {
			continue
		}
		switch k.Class {
		case restic.ClassLocked:
			into.Add(restoreplace.ClassTargetLocked, int64(n))
		case restic.ClassPermission:
			into.Add(restoreplace.ClassAccessDenied, int64(n))
		default:
			into.Add(restoreplace.ClassOther, int64(n))
		}
	}
}

// fatalOfRestic is the run-level class of a restic run that ended without a
// summary: the gateway refused the login (auth, read from restic's own error
// output because the gateway logs nothing for a refused request), the
// repository could not be reached (repo_unreachable), or anything else.
func fatalOfRestic(class restic.ErrorClass) api.RestoreResultClass {
	switch class {
	case restic.ClassAuth:
		return api.RestoreResultAuth
	case restic.ClassNetwork, restic.ClassTLS, restic.ClassStorage, restic.ClassRepoMissing, restic.ClassLock:
		return api.RestoreResultRepoUnreachable
	}
	return api.RestoreResultOther
}

// matchedNothing reports a restic run whose include file matched nothing:
// exit 0 and a summary with every count 0 (restic leaves zero fields out,
// p22-spike.md:176-182).
func matchedNothing(exit int, s *restic.RestoreSummary) bool {
	return exit == 0 && s != nil && s.TotalFiles == 0 && s.FilesRestored == 0 && s.FilesSkipped == 0 &&
		s.TotalBytes == 0 && s.BytesRestored == 0 && s.BytesSkipped == 0
}
