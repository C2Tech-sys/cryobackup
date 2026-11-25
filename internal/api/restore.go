package api

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// pathBearing marks a wire type that carries file-system paths on purpose.
// AssertPathFree does not look inside a value whose type implements it.
//
// The guard has exactly two exemptions (architecture addendum §13.3 "Wire
// protocol"): the pathBearing marker, which only RestoreSelection implements
// (architecture §8.4 item 4), and the field skip for restic.Snapshot.Paths,
// the configured backup roots of a snapshot. Adding a third is a decision for
// an ADR, not a convenience.
type pathBearing interface{ pathBearing() }

// DeviceRestoreMode is where a restore to a device lands, as the agent is told
// (RestoreSelection.Mode). It is a wire vocabulary of its own, not a column
// value: the database keeps the admin-side modes in restore_requests.mode
// (RestoreMode), and the console maps between the two when it answers the fetch.
type DeviceRestoreMode string

// Device restore modes. The console never sends a target path: for the side
// folder the agent computes <SystemDrive>\CryoBackup-Restore\<restore id>\ itself
// (ADR 0027, ruling C2), and the original location is where the snapshot says.
const (
	// DeviceRestoreOriginal puts the files back where they were.
	DeviceRestoreOriginal DeviceRestoreMode = "device_original"
	// DeviceRestoreSide puts them under the agent's side folder for this request.
	DeviceRestoreSide DeviceRestoreMode = "device_side"
)

// DeviceRestoreOverwrite is what the agent does with a file that already exists
// at the target (RestoreSelection.Overwrite): restic's --overwrite, in the two
// forms the pilot offers. The typed host-name confirmation that guards if_changed
// is a console concern and has no wire field. The agent never passes --delete.
type DeviceRestoreOverwrite string

// Device restore overwrite behaviours.
const (
	// DeviceOverwriteNever is --overwrite never: an existing file is left alone.
	DeviceOverwriteNever DeviceRestoreOverwrite = "never"
	// DeviceOverwriteIfChanged is --overwrite if-changed: replace what differs.
	DeviceOverwriteIfChanged DeviceRestoreOverwrite = "if_changed"
)

var (
	deviceRestoreModes      = []DeviceRestoreMode{DeviceRestoreOriginal, DeviceRestoreSide}
	deviceRestoreOverwrites = []DeviceRestoreOverwrite{DeviceOverwriteNever, DeviceOverwriteIfChanged}
)

// The limits of a restore selection. Each is a named constant with the reason
// for its value; a change is a decision for an ADR (docs/decisions), because the
// console, the agent and the wizard must agree on it.
const (
	// MaxRestorePaths is the number of items one restore may name. It is a
	// chosen cap, not one restic imposes. The number of restic runs does not
	// grow with it: the agent runs restic once per volume the selection touches
	// (ADR 0027, "How a request travels"), so at most 26 runs, each loading the
	// repository index once. What grows with it is per item: one include-file
	// line, one placement walk on the device, and one row the operator reads on
	// the wizard's review step before confirming. 100 keeps that review
	// readable. The wizard collapses a file into its selected folder, so an
	// operator who needs more selects a parent folder.
	MaxRestorePaths = 100
	// MaxRestorePathBytes is the length of one path, in bytes of UTF-8. It is a
	// chosen cap, not a Windows limit: an extended-length (\\?\) path reaches
	// 32 767 UTF-16 units, which is up to about 98 000 bytes of UTF-8, and NTFS
	// can hold such paths. 4096 bounds one include-file line and one row of the
	// wizard. The longest path in the spike was 378 characters (item 5), so few
	// real selections come near it; a deeper one is refused loudly, never cut,
	// and the operator restores a parent folder instead.
	MaxRestorePathBytes = 4096
	// MaxRestorePathsTotalBytes is the sum of all paths of one selection. The
	// wizard's basket travels as hidden form fields in a POST body capped at
	// 64 KiB (ADR 0027), so a selection the form accepted always fits; it also
	// bounds the response, the SYSTEM-only temp file and the include file the
	// agent writes.
	MaxRestorePathsTotalBytes = 64 << 10
)

// RestoreSelection is the answer of GET /restores/{id} (architecture §5.1, DH4,
// §6.1 "restore executor", ADR 0027): what the agent restores. It is the single
// path-bearing type of the protocol, because a restore cannot be described
// without the paths. The restore command carries only the id of the restore
// request (RestoreParams); the agent fetches this with its device token.
//
// Paths are snapshot-absolute, in the form restic writes for a Windows volume:
// "/C/Users/x/Documents/a.docx", a slash, the drive letter, then the path below
// it, always with at least one name below the drive letter. They are copied from
// the console's own listing of the snapshot and are byte-exact: no case folding
// (restic matches case-sensitively, the drive letter included, spike item 3), no
// Unicode normalisation, no trimming. A folder selects everything below it.
//
// There is no target on the wire. The original location is the path itself; the
// side folder is computed by the agent from the request id. Mode, Overwrite and
// the paths are the whole instruction; verify, dry run, the relay and if-newer
// are out of the pilot (ADR 0027).
//
// Handling rules (§8.4 item 4): it travels only on this device-authenticated
// response, never in the command queue; the console stores it encrypted with a
// purge date; the agent writes it to a SYSTEM-only temp file and deletes it
// after the restore; nobody logs it. AssertPathFree skips this type by its
// pathBearing marker.
//
// String, GoString and LogValue print the id and the number of paths, and they
// cover direct logging only: a selection handed straight to fmt's %v, %+v, %#v,
// %s or %q, or to log/slog as an attribute value. They do not make a selection
// safe to log in general. A struct that holds one (in an unexported field, or
// any field under slog's JSON handler), encoding/json, a verb such as %d, and
// %v of a slice or map of such structs all print the paths. So callers must
// never log a selection or any struct, slice or map that holds one.
type RestoreSelection struct {
	ID         string                 `json:"id"`          // restore request id, 26-character ULID
	SnapshotID string                 `json:"snapshot_id"` // 64 lowercase hex
	Mode       DeviceRestoreMode      `json:"mode"`
	Overwrite  DeviceRestoreOverwrite `json:"overwrite"`
	Paths      []string               `json:"paths"`
	ExpiresAt  time.Time              `json:"expires_at"` // the console stops waiting; the agent drops an expired selection
}

func (RestoreSelection) pathBearing() {}

// String prints the id and the number of paths and nothing else, so a selection
// that is itself the operand of %v or %+v prints no path. It does not protect a
// selection nested in another value (see RestoreSelection).
func (s RestoreSelection) String() string {
	return fmt.Sprintf("restore selection %s (%d paths)", s.ID, len(s.Paths))
}

// GoString is String for %#v.
func (s RestoreSelection) GoString() string { return s.String() }

// LogValue keeps a selection logged through log/slog path-free.
func (s RestoreSelection) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", s.ID), slog.String("mode", string(s.Mode)), slog.Int("paths", len(s.Paths)))
}

// Validate checks the selection. The console calls it before it stores one, the
// agent before it acts on one. Problems are fixed phrases under the JSON path
// ("paths[3]"); a path is never repeated.
func (s RestoreSelection) Validate() error {
	var c collector
	c.ulid("id", s.ID)
	c.hexFixed("snapshot_id", s.SnapshotID, 64)
	modeOK := has(deviceRestoreModes, s.Mode)
	if !modeOK {
		c.add("mode", msgUnknown)
	}
	overwriteOK := has(deviceRestoreOverwrites, s.Overwrite)
	if !overwriteOK {
		c.add("overwrite", msgUnknown)
	}
	// A side folder is new and empty, so there is nothing to replace: an
	// if_changed there is a console bug, not an instruction.
	if modeOK && overwriteOK && s.Mode == DeviceRestoreSide && s.Overwrite != DeviceOverwriteNever {
		c.add("overwrite", "is only allowed for the original location")
	}
	switch n := len(s.Paths); {
	case n < 1:
		c.add("paths", "must hold at least one path")
	case n > MaxRestorePaths:
		c.add("paths", "has too many paths")
	}
	total := 0
	for i, p := range s.Paths {
		if i >= MaxRestorePaths {
			break // already reported; do not spend time on a flood
		}
		total += len(p)
		if msg := restorePathProblem(p); msg != "" {
			c.add(idx("paths", i), msg)
		}
	}
	if total > MaxRestorePathsTotalBytes {
		c.add("paths", "are too large together")
	}
	c.notBeforeMin("expires_at", s.ExpiresAt)
	return c.err()
}

// ValidateRestorePath checks one snapshot-absolute path with the rules of
// RestoreSelection.Validate. The wizard uses it for each basket entry. The error
// never holds the path.
func ValidateRestorePath(p string) error {
	var c collector
	if msg := restorePathProblem(p); msg != "" {
		c.add("", msg)
	}
	return c.err()
}

// nameForbidden are the characters Windows does not allow in a file name, which
// includes the backslash. None can be in a name restic read from an NTFS volume,
// so none is a real selection; and '*' and '?' are wildcards in the include file
// the agent writes, so one of them would select far more than the user chose
// (spike item 3).
const nameForbidden = `\:*?"<>|`

// restorePathProblem returns a fixed phrase naming the first rule p breaks, or
// "" when p is acceptable. Every rule has a reason:
//
//   - valid UTF-8, at most MaxRestorePathBytes.
//   - absolute, "/X/...": the form restic writes for a drive. A relative path
//     would be matched against the wrong root.
//   - no control character, NUL included (so no CR or LF either): the agent
//     writes one path per line into the include file, and a line break would
//     smuggle in a second pattern.
//   - none of nameForbidden.
//   - a single ASCII letter as the first segment, and at least one name below
//     it: a drive root has no anchor folder (the folder the agent restores
//     into), and "everything on C:" is not a selection the pilot offers.
//   - no empty, "." or ".." segment: ".." would climb out of the snapshot
//     subtree the agent anchors to; "." and an empty segment ("//", a trailing
//     slash) are not what restic writes.
//   - no name with a leading or trailing space, judged on every segment with
//     unicode.IsSpace, the test strings.TrimSpace uses: restic trims each
//     include-file line, and a literal match of such a name is untested (spike
//     item 3), so it is refused until a test against the real binary proves a
//     form for it.
//   - no name that ends with '.', and no name Windows reserves for a device
//     (windowsReservedName): Win32 cannot create either (it strips the trailing
//     dot, and opens the device instead of a file), so no Windows snapshot holds
//     one legitimately, and a placement call without the \\?\ prefix would be
//     redirected to the device or to a different name.
//
// Case is never touched: "/c/x" and "/C/x" are different paths. '[' and '$'
// are allowed; the agent escapes them when it writes the include file.
//
// Look-alike dots (U+2024, U+FF0E, a dot with a combining mark) and invisible
// characters (U+200B, U+FEFF, U+202E, a soft hyphen) are accepted on purpose.
// None of them is '.' or a separator to restic, NTFS or the include file, so a
// name made of them is matched literally, byte for byte, and can only select
// that same name in the snapshot: harmless, and possibly a real file name.
func restorePathProblem(p string) string {
	switch {
	case p == "":
		return msgRequired
	case len(p) > MaxRestorePathBytes:
		return msgTooLong
	case !utf8.ValidString(p):
		return "must be valid UTF-8"
	case p[0] != '/':
		return "must be absolute: a slash, the drive letter, the path"
	}
	for _, r := range p {
		switch {
		case unicode.IsControl(r):
			return msgControl
		case strings.ContainsRune(nameForbidden, r):
			return "contains a character that is not allowed in a Windows name"
		}
	}
	segs := strings.Split(p[1:], "/")
	if d := segs[0]; len(d) != 1 || !(d[0] >= 'A' && d[0] <= 'Z' || d[0] >= 'a' && d[0] <= 'z') {
		return "must start with a drive letter, like /C/"
	}
	if len(segs) < 2 {
		return "must name something below the drive letter"
	}
	for _, seg := range segs {
		switch seg {
		case "":
			return "has an empty segment"
		case ".", "..":
			return "must not contain . or .. segments"
		}
		first, _ := utf8.DecodeRuneInString(seg)
		last, _ := utf8.DecodeLastRuneInString(seg)
		if unicode.IsSpace(first) || unicode.IsSpace(last) {
			return "has a name with leading or trailing white space"
		}
		if last == '.' {
			return "has a name that ends with a dot"
		}
		if windowsReservedName(seg) {
			return "has a name Windows reserves for a device"
		}
	}
	return ""
}

// windowsReservedName reports whether Win32 treats name as a device: CON, PRN,
// AUX, NUL, CONIN$, CONOUT$, COM0 to COM9 and LPT0 to LPT9, the last two also
// with the superscript digits ¹ ² ³, in any ASCII case, alone or with any
// extension ("con.txt", "NUL.tar.gz"), and with spaces before the first dot
// ("CON .txt"), which Win32 ignores. Only ASCII letters are folded, so a
// non-ASCII look-alike is accepted and matched literally.
func windowsReservedName(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = asciiUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) < 4 {
		return false
	}
	switch base[:3] {
	case "COM", "LPT":
		switch digit := base[3:]; {
		case len(digit) == 1 && digit[0] >= '0' && digit[0] <= '9':
			return true
		case digit == "\u00b9", digit == "\u00b2", digit == "\u00b3":
			return true
		}
	}
	return false
}

// asciiUpper upper-cases the ASCII letters of s and leaves every other byte as
// it is. strings.ToUpper is not used because it also maps non-ASCII letters
// (the dotless ı becomes I), which would widen the reserved list beyond the
// names Windows documents.
func asciiUpper(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if ch >= 'a' && ch <= 'z' {
			b[i] = ch - 'a' + 'A'
		}
	}
	return string(b)
}

// RestoreResultClass is how a restore ended or what went wrong in it, as a
// fixed word. It never carries a path or a message: restic's error lines hold
// absolute paths, so the agent classifies them on the device and sends only the
// class and a count (spike item 4; architecture §8.4).
//
// Two kinds share the vocabulary. Run-level classes say how the whole run ended
// and travel in RunStats.RestoreResult next to the run's outcome; countable
// classes (Countable) are tallied in RunStats.RestoreErrors: what a failed file
// hit (target_locked, access_denied, other) and how many restic runs matched
// nothing (nothing_matched).
//
// A restore runs restic once per volume the selection touches (ADR 0027) and
// the agent judges nothing matched per run: every run matched nothing is
// nothing_matched and the restore ends failed; some runs restored files and
// some matched nothing (or had file errors) is partial, with RestoreErrors
// holding the count per class (nothing_matched counts restic runs). No run
// restoring anything and some having file errors ends failed with the dominant
// class.
//
// Pairing with the run outcome: RunRecord.Validate enforces it, one row of
// restoreCouplings per rule (runs.go). nothing_matched is outcome failed with
// all four counts 0 (restic exits 0 with an empty summary when no include
// matched anything, spike item 4; the restore must still end FAILED, never
// delivered); partial is outcome partial; cancelled is cancelled; interrupted
// is interrupted; every other class is failed; a successful restore carries no
// class and no error rows, and restored or skipped at least one file. The
// vocabulary has no "ok" word: success is the absence of a class.
//
// None of these values is in the runs.error_class CHECK list of the database
// except auth, guard_refused and disk; the classes ride in runs.stats (JSON), which
// needs no migration. RunRecord.ErrorClass stays a restic.ErrorClass.
type RestoreResultClass string

// Restore result classes.
const (
	// RestoreResultNothingMatched: the selection (or, as a count, one restic run of it) matched nothing in the snapshot.
	RestoreResultNothingMatched RestoreResultClass = "nothing_matched"
	// RestoreResultTargetLocked: a target file was held open by another process.
	RestoreResultTargetLocked RestoreResultClass = "target_locked"
	// RestoreResultAccessDenied: Windows refused access to a target.
	RestoreResultAccessDenied RestoreResultClass = "access_denied"
	// RestoreResultPartial: restored with errors (some files restored, some not).
	RestoreResultPartial RestoreResultClass = "partial"
	// RestoreResultInterrupted: the agent stopped mid-restore; it never resumes.
	RestoreResultInterrupted RestoreResultClass = "interrupted"
	// RestoreResultGuardRefused: the agent's target guard refused to write.
	RestoreResultGuardRefused RestoreResultClass = "guard_refused"
	// RestoreResultAuth: the gateway refused the device's login.
	RestoreResultAuth RestoreResultClass = "auth"
	// RestoreResultRepoUnreachable: the repository could not be reached.
	RestoreResultRepoUnreachable RestoreResultClass = "repo_unreachable"
	// RestoreResultCancelled: the operator cancelled the run.
	RestoreResultCancelled RestoreResultClass = "cancelled"
	// RestoreResultDisk: a destination volume had too little free space for the
	// selection, so the agent refused before writing anything (ADR 0027, "Size
	// check"). Run-level only.
	RestoreResultDisk RestoreResultClass = "disk"
	// RestoreResultOther: a per-file error the agent could not classify. It is
	// counted so that no failure vanishes into a smaller number.
	RestoreResultOther RestoreResultClass = "other"
)

var restoreResultClasses = []RestoreResultClass{
	RestoreResultNothingMatched, RestoreResultTargetLocked, RestoreResultAccessDenied, RestoreResultPartial,
	RestoreResultInterrupted, RestoreResultGuardRefused, RestoreResultAuth, RestoreResultRepoUnreachable,
	RestoreResultCancelled, RestoreResultDisk, RestoreResultOther,
}

// Valid reports whether c is in the vocabulary.
func (c RestoreResultClass) Valid() bool { return has(restoreResultClasses, c) }

// Countable reports whether c may be tallied in RunStats.RestoreErrors.
func (c RestoreResultClass) Countable() bool {
	switch c {
	case RestoreResultNothingMatched, RestoreResultTargetLocked, RestoreResultAccessDenied, RestoreResultOther:
		return true
	}
	return false
}

// RestoreClassCount is how many files (or, for nothing_matched, restic runs)
// ended in one countable class. Count is at most MaxRestoreCount.
type RestoreClassCount struct {
	Class RestoreResultClass `json:"class"`
	Count int64              `json:"count"`
}
