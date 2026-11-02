package restic

import (
	"sort"
	"strings"
)

// Rule is one row of the stderr classification table (ticket P1-T1 section 3).
// Rules are tried in order and the first row that matches wins, so the order
// is part of the contract. All matching is case-insensitive.
//
// A row matches when any of Any is a substring of the message, or any of Words
// occurs as a whole token, or (when All is not empty) every entry of All is a
// substring, or the whole message (surrounding white space trimmed) equals one
// of Exact. Words are used for short strings (HTTP status codes and "eof")
// that would otherwise match inside a file name or an identifier; a
// token is delimited by anything other than a letter, a digit or one of
// \ / . _ - ~ (so a directory called "500" does not look like an HTTP 500).
// Exact is for a message restic prints in one fixed form, where anything
// near it means something else.
type Rule struct {
	Class ErrorClass
	Any   []string
	Words []string
	All   []string
	Exact []string
}

// classifierRules is the precedence-ordered table. Keep it in step with the
// list in doc.go.
var classifierRules = []Rule{
	{Class: ClassWrongPassword, Any: []string{"wrong password", "no key found"}},
	{Class: ClassRepoMissing, Any: []string{"repository does not exist", "is there a repository at"}},
	{Class: ClassLock, Any: []string{"unable to create lock", "already locked", "failed to refresh lock"}, All: []string{"lock", "exclusive"}},
	{Class: ClassVSS, Any: []string{"vss", "shadow copy", "volume shadow"}},
	{Class: ClassTLS, Any: []string{"x509", "tls:", "certificate", "tls handshake"}},
	{Class: ClassAuth, Any: []string{"unauthorized", "forbidden", "authentication"}, Words: []string{"401", "403"}},
	{Class: ClassStorage, Any: []string{"internal server error", "bad gateway", "service unavailable", "gateway timeout", "insufficient storage"}, Words: []string{"500", "502", "503", "504", "507"}},
	{Class: ClassNetwork, Any: []string{"connection refused", "no such host", "i/o timeout", "connection reset", "network is unreachable", "dial tcp", "broken pipe", "proxy", "wsarecv", "wsasend", "host is down"}, Words: []string{"eof"}},
	{Class: ClassDisk, Any: []string{"no space left", "not enough space", "disk full", "disk quota"}},
	{Class: ClassMemory, Any: []string{"cannot allocate memory", "out of memory", "runtime: out of memory"}},
	{Class: ClassLocked, Any: []string{"being used by another process", "sharing violation", "lock violation"}},
	{Class: ClassPermission, Any: []string{"access is denied", "permission denied", "operation not permitted", "privilege"}},
	{Class: ClassMissingSource, Any: []string{"cannot find the file", "cannot find the path", "no such file or directory", "does not exist", "is not a directory"}},
	{Class: ClassParse, Any: []string{"unexpected end of json", "invalid character", "unmarshal", "cannot parse", "invalid json"}},
	{Class: ClassInternal, Any: []string{"panic", "runtime error", "goroutine", "nil pointer"}},
	unsupportedFileRule,
}

// --- unsupported_file ---------------------------------------------------------
//
// restic 0.19.1 has exactly one message of this kind, from internal/fs
// (nodeFillExtendedStat, node.go): errors.Errorf("unsupported file type %q",
// node.Type), reached only for the two node types it cannot archive, so the
// error text is one of
//
//	unsupported file type "irregular"   (a reparse point Go reports as ModeIrregular:
//	                                     app-execution aliases, some junctions)
//	unsupported file type ""            (a mode no known type matches)
//
// There is no "incomplete metadata for <path>:" wrapper for these two types,
// but the archiver's error hook puts the item path in front of any message that
// does not already contain it (internal/archiver/archiver.go, Archiver.error:
// fmt.Errorf("%v: %w", item, err)), so the JSON line restic prints is
//
//	{"message_type":"error","error":{"message":"<item>: unsupported file type \"irregular\""},"during":"archival","item":"<item>"}
//
// ClassifyItem strips that prefix (and every other path) before the table runs,
// so a keyword in the path cannot decide the class, and the row matches only the exact remaining
// text: "unsupported repository version 3" or a path that merely contains the
// words stays unknown. restic skips the file and exits 3 (a partial run).
var unsupportedFileRule = Rule{Class: ClassUnsupportedFile, Exact: []string{`unsupported file type "irregular"`, `unsupported file type ""`}}

// --- end unsupported_file -----------------------------------------------------

// Rules returns a copy of the classification table, in precedence order. The
// final implicit rule is ClassUnknown. Tests and documentation read this same
// table.
func Rules() []Rule {
	out := make([]Rule, len(classifierRules))
	for i, r := range classifierRules {
		out[i] = Rule{
			Class: r.Class,
			Any:   append([]string(nil), r.Any...),
			Words: append([]string(nil), r.Words...),
			All:   append([]string(nil), r.All...),
			Exact: append([]string(nil), r.Exact...),
		}
	}
	return out
}

// Classify maps a restic error message to a path-free class. It is pure,
// case-insensitive and applies the table from Rules in order; a message that
// matches nothing is ClassUnknown. The input is neither stored nor returned.
//
// The tables assume the English text that restic and the Go runtime write on
// Windows; a localized Windows renders system errors in the display language
// and falls through to ClassUnknown (doc.go, "Locale"). A keyword that appears
// inside a file name can mislead the classifier, which is why the class is
// advisory and the raw log stays on the device.
func Classify(message string) ErrorClass {
	msg := strings.ToLower(message)
	for _, r := range classifierRules {
		if r.matches(msg) {
			return r.Class
		}
	}
	return ClassUnknown
}

// ClassifyItem classifies the message of one restic error line whose "item"
// field is item, with every path taken out of the text first, so that the words
// of a file, directory or shadow-copy name never decide the class.
//
// What is guaranteed. Before the table runs, three things are removed:
//
//  1. the item, as a "<item>: " prefix (restic's archiver adds it to an error
//     that does not name the item) and, for an item of minItemCut bytes or more,
//     wherever else it occurs byte for byte (both strings come from one JSON
//     line, already unescaped, so they compare exactly);
//
//  2. every absolute-path token (withoutPaths): one that starts with a drive
//     letter and a separator ("C:\", "c:/"), with two backslashes ("\\?\",
//     "\\.\", "\\host\", which includes the shadow-copy form
//     \\?\GLOBALROOT\Device\HarddiskVolumeShadowCopyN\...), or with a slash and
//     a name, and that follows the start of the text or a space, a quote, a
//     bracket or one of "<=,;". The token runs to the next ": " (colon and
//     space, consumed with it), the next double quote, a line break or the end
//     of the text, so a path with spaces is removed whole.
//
//  3. every name restic prints without its directory (withoutNames): a Go %q
//     operand (a double-quoted string, backslash escapes honoured, up to the
//     closing quote) and the operand of "invalid child node name ", to the end
//     of the line. restic check and the restorer name bare file names, not
//     paths: "file %q blob %v not found in index", "dir node %q has no
//     subtree", "file %q has nil blob list" (internal/checker/checker.go) and
//     "invalid child node name %s" (internal/restorer/restorer.go). A quoted
//     URL (Get "https://...": dial tcp ...) goes too; the words after it still
//     decide. Only the Exact rows compare the text before this step: restic's
//     own `unsupported file type "irregular"` quotes a node type, not a name,
//     and an Exact row matches nothing but the whole message.
//
// The words that are left decide the class; a keyword in a path or a name
// cannot.
//
// What is not guaranteed. An unquoted relative path, or a path that does not
// follow one of those starts, is not removed (restic 0.19.1 names a file by its
// absolute path, by a %q-quoted name or after "invalid child node name "; see
// withoutPaths and withoutNames). A path that itself contains ": " is cut at
// that point. And the removal is blunt in one direction: a path followed by
// more words and no ": " takes the words with it, and a keyword restic itself
// puts in double quotes no longer counts, so the class can fall to a later row
// or to unknown, but a path or a name never decides it. Classify, which is used
// for exit_error and unparsable lines (text restic does not build from file
// names), removes nothing. Neither function stores, logs or returns its input.
//
// VSSFallbackVolume reads the per-volume fallback line's message and item on
// its own.
func ClassifyItem(message, item string) ErrorClass {
	pathFree := strings.ToLower(withoutPaths(withoutItem(message, item)))
	nameFree := withoutNames(pathFree)
	for _, r := range classifierRules {
		if r.matchesExact(pathFree) || r.matchesText(nameFree) {
			return r.Class
		}
	}
	return ClassUnknown
}

// minItemCut is the shortest item ClassifyItem removes from inside a message:
// one more than a drive root ("c:\").
const minItemCut = 4

func withoutItem(message, item string) string {
	if item == "" {
		return message
	}
	if p := item + ": "; strings.HasPrefix(message, p) {
		message = message[len(p):]
	}
	if len(item) >= minItemCut {
		message = strings.ReplaceAll(message, item, "")
	}
	return message
}

// withoutPaths replaces every absolute-path token of s by one space.
//
// The shapes it is built for, all read from restic 0.19.1 (tag v0.19.1):
//
//   - os.PathError, "<op> <path>: <error>" (Go's PathError.Error), for every
//     file operation. Under --use-fs-snapshot (the default on Windows) <path>
//     is the shadow-copy path, not the item: LocalVss.snapshotPath
//     (internal/fs/fs_local_vss.go) joins snapshot.GetSnapshotDeviceObject(),
//     \\?\GLOBALROOT\Device\HarddiskVolumeShadowCopyN, to the path below the
//     volume, and fixpath (internal/fs/file_windows.go) puts \\?\ in front of
//     any other absolute path. Archiver.error (internal/archiver/archiver.go)
//     therefore does not find the item in the text and prefixes it:
//     "<item>: open \\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy3\Data\proxy\x:
//     Access is denied."
//   - fmt.Errorf wrappers that put a path first or last: "incomplete metadata
//     for <path>: <error>", "readdirnames <path> failed: <error>", "path is not
//     a directory: <path>", and, with %q (backslashes doubled, closing quote),
//     "failed to restore timestamp of \"<path>\": <error>" (internal/fs/node.go,
//     nodeRestoreTimestamps) and "directory \"<path>\" changed type, refusing to
//     archive" (internal/archiver/archiver.go).
//   - the restorer's errors name the absolute restore target (Restorer makes
//     the destination absolute with filepath.Abs, internal/restorer/restorer.go)
//     and the archiver works on absolute targets (Archiver.FS.Abs), so neither
//     prints a relative path. What restic does print without a directory is a
//     bare name (check's "file %q ...", the restorer's "invalid child node
//     name %s"); withoutNames, not this function, removes those.
//
// Windows file names cannot contain a double quote or a line break, so those
// end a token for certain; they can contain spaces but not a colon, so ": " ends
// one unless the name is an alternate data stream.
func withoutPaths(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if !pathStart(s, i) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i + 1
		for j < len(s) && s[j] != '"' && s[j] != '\n' && s[j] != '\r' && !strings.HasPrefix(s[j:], ": ") {
			j++
		}
		if j < len(s) && s[j] == ':' {
			j += 2
		}
		b.WriteByte(' ')
		i = j
	}
	return b.String()
}

// pathStart reports whether an absolute-path token begins at s[i]: a drive
// letter, a colon and a separator; two backslashes; or a slash followed by a
// name character (so a lone "/" and the "i/o" or "HTTP/1.1" inside a word are
// not paths). It must follow the start of the text or a character that opens a
// path in a message ("open <path>", "\"<path>", "[<path>", "(<path>", "=<path>").
func pathStart(s string, i int) bool {
	if i > 0 && strings.IndexByte(" \t\r\n\"'[(<=,;", s[i-1]) < 0 {
		return false
	}
	switch c := s[i]; {
	case c == '\\':
		return i+1 < len(s) && s[i+1] == '\\'
	case c == '/':
		return i+1 < len(s) && strings.IndexByte(" \t\r\n:\"", s[i+1]) < 0
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return i+2 < len(s) && s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/')
	}
	return false
}

// bareNameLeads are restic's fixed texts after which the rest of the line is a
// bare node name printed with %s (internal/restorer/restorer.go, "invalid
// child node name %s"). The name is the one restic refused, so it can hold
// anything but a line break. Lower case: withoutNames sees lower-cased text.
var bareNameLeads = []string{"invalid child node name "}

// withoutNames replaces by one space every name that restic prints without its
// directory: each Go %q operand (see quoteStart) and the rest of the line after
// a bareNameLeads text. A %q operand runs to the closing double quote, skipping
// backslash escapes (%q escapes a quote and a line break inside the name); a
// quote that is never closed takes the rest of the line.
func withoutNames(s string) string {
	for _, lead := range bareNameLeads {
		for from := 0; from < len(s); {
			i := strings.Index(s[from:], lead)
			if i < 0 {
				break
			}
			start := from + i + len(lead)
			end := start
			for end < len(s) && s[end] != '\n' && s[end] != '\r' {
				end++
			}
			s = s[:start] + " " + s[end:]
			from = start + 1
		}
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '"' || !quoteStart(s, i) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i + 1
		for j < len(s) && s[j] != '"' && s[j] != '\n' && s[j] != '\r' {
			if s[j] == '\\' && j+1 < len(s) {
				j++
			}
			j++
		}
		if j < len(s) && s[j] == '"' {
			j++
		}
		b.WriteByte(' ')
		i = j
	}
	return b.String()
}

// quoteStart reports whether the double quote at s[i] opens a %q operand: it is
// at the start of the text or follows a character that is not a letter, a
// digit, a backslash, a quote or an apostrophe (so the '"' of "invalid
// character '"' after object key" is not taken for one).
func quoteStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	switch c := s[i-1]; {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return false
	case c == '\\', c == '\'', c == '"':
		return false
	}
	return true
}

func (r Rule) matches(msg string) bool {
	return r.matchesExact(msg) || r.matchesText(msg)
}

// matchesExact reports whether the whole message, surrounding white space
// trimmed, is one of the row's Exact texts.
func (r Rule) matchesExact(msg string) bool {
	whole := strings.TrimSpace(msg)
	for _, e := range r.Exact {
		if whole == e {
			return true
		}
	}
	return false
}

// matchesText applies the row's Any, Words and All entries.
func (r Rule) matchesText(msg string) bool {
	for _, n := range r.Any {
		if strings.Contains(msg, n) {
			return true
		}
	}
	for _, w := range r.Words {
		if containsToken(msg, w) {
			return true
		}
	}
	if len(r.All) > 0 {
		all := true
		for _, n := range r.All {
			if !strings.Contains(msg, n) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// containsToken reports whether tok occurs in s delimited on both sides by
// something that is not part of a word or a path component.
func containsToken(s, tok string) bool {
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], tok)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(tok)
		before := i == 0 || !tokenChar(s[i-1])
		after := end == len(s) || !tokenChar(s[end]) || trailingStop(s, end)
		if before && after {
			return true
		}
		from = i + 1
	}
	return false
}

// tokenChar reports whether c glues a token to its neighbour. A '.' directly
// before a token (".500", a version number or an extension) glues; after a
// token it is handled by trailingStop.
func tokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '\\', '/', '.', '_', '-', '~':
		return true
	}
	return false
}

// trailingStop reports whether the '.' at s[end] ends a sentence ("got 503.")
// rather than starting an extension ("503.txt").
func trailingStop(s string, end int) bool {
	if end >= len(s) || s[end] != '.' {
		return false
	}
	return end+1 == len(s) || s[end+1] == ' ' || s[end+1] == '\n' || s[end+1] == '\r'
}

// allowedExt is the extension allow-list (ticket section 3). Only an extension
// in this list may leave the device; everything else is reported as "".
var allowedExt = map[string]struct{}{}

func init() {
	for _, e := range strings.Fields(`docx xlsx pptx doc xls ppt pdf txt csv rtf odt ods
		jpg jpeg png gif heic tif tiff mp4 mov avi mp3 wav zip 7z rar pst ost msg eml
		html htm xml json log db sqlite bak qbw qbb dwg psd ai`) {
		allowedExt[e] = struct{}{}
	}
}

// Ext returns the lower-cased extension of the last component of item (split on
// both '\' and '/' so it behaves identically on every platform) when that
// extension is in the allow-list, and "" otherwise. It is the only part of a
// path that may be reported (architecture §8.4).
func Ext(item string) string {
	if i := strings.LastIndexAny(item, `\/`); i >= 0 {
		item = item[i+1:]
	}
	item = strings.TrimRight(item, " ")
	dot := strings.LastIndexByte(item, '.')
	if dot <= 0 || dot == len(item)-1 {
		return ""
	}
	ext := strings.ToLower(item[dot+1:])
	if _, ok := allowedExt[ext]; !ok {
		return ""
	}
	return ext
}

// ErrorKey is the grouping key of ErrorCounts: where in the run the error
// happened (restic's "during"), what class it is, and the allow-listed file
// extension, if any.
type ErrorKey struct {
	During string
	Class  ErrorClass
	Ext    string
}

// ErrorCounts accumulates the errors of one run, path-free. Create it with
// make(ErrorCounts).
type ErrorCounts map[ErrorKey]int

// Add counts one error event.
func (c ErrorCounts) Add(e ErrorEvent) {
	c[ErrorKey{During: e.During, Class: e.Class, Ext: e.Ext}]++
}

// ByClass sums the counts per class: the {class: n} shape of runs.error_counts.
func (c ErrorCounts) ByClass() map[ErrorClass]int {
	out := make(map[ErrorClass]int)
	for k, n := range c {
		out[k.Class] += n
	}
	return out
}

// Dominant returns the most frequent class. Ties go to a hard class first
// (ErrorClass.Hard), then to the alphabetically smaller name. An empty set is
// ClassUnknown.
func (c ErrorCounts) Dominant() ErrorClass {
	best, bestN := ClassUnknown, 0
	for class, n := range c.ByClass() {
		if n <= 0 {
			continue
		}
		switch {
		case n > bestN:
		case n == bestN && class.Hard() && !best.Hard():
		case n == bestN && class.Hard() == best.Hard() && class < best:
		default:
			continue
		}
		best, bestN = class, n
	}
	return best
}

// ErrorProjection is one row of the wire shape DH15 {during, class, ext, count}
// that the agent uploads: no message, no path.
type ErrorProjection struct {
	During string     `json:"during"`
	Class  ErrorClass `json:"class"`
	Ext    string     `json:"ext"`
	Count  int        `json:"count"`
}

// Projection returns the counts as rows sorted by during, class, ext.
func (c ErrorCounts) Projection() []ErrorProjection {
	out := make([]ErrorProjection, 0, len(c))
	for k, n := range c {
		if n <= 0 {
			continue
		}
		out = append(out, ErrorProjection{During: k.During, Class: k.Class, Ext: k.Ext, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.During != b.During {
			return a.During < b.During
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Ext < b.Ext
	})
	return out
}

// StderrClass picks the Evidence.Stderr class for a finished run: the class of
// the final exit_error message when it says something (not ClassUnknown),
// otherwise the dominant class of the counted error events. exit may be nil.
func StderrClass(exit *ExitError, counts ErrorCounts) ErrorClass {
	if exit != nil && exit.Class != ClassUnknown && exit.Class != "" {
		return exit.Class
	}
	return counts.Dominant()
}

// ---- VSS per-volume fallback (matrix r211) ---------------------------------
//
// This block is the VSS fallback rule and nothing else; it does not touch the
// classifier table above.
//
// restic 0.19.1 with --use-fs-snapshot creates one snapshot per volume the
// first time it reads a path on that volume (internal/fs/fs_local_vss.go,
// LocalVss.snapshotPath). When NewVssSnapshot fails it reports
//
//	errors.Errorf("failed to create snapshot for [%s]: %s", vssVolume, err)
//
// through the backup's error callback with item = vssVolume, the lower-cased
// volume name plus a separator ("c:\"), remembers the volume as failed and
// reads it live from then on. With --json that is one stderr line:
//
//	{"message_type":"error","error":{"message":"failed to create snapshot for [c:\\]: VSS error: ..."},"during":"archival","item":"c:\\"}
//
// That callback is not the archiver's, so restic does not mark the backup
// incomplete for it: a fallback on its own ends with exit 0 and a snapshot
// (cmd/restic/cmd_backup.go, the VSS errorHandler beside NewLocalVss). Only
// files that then fail to read live (a locked file) make it exit 3. A volume
// excluded by vss.exclude-volumes, a UNC path and a mount point that is not in
// the snapshot are read live without any message and cannot be seen here.

// vssFallbackPrefix opens restic's message for a volume whose snapshot failed.
const vssFallbackPrefix = "failed to create snapshot for ["

// VSSFallbackVolume reports whether an error message and its item (both from
// one restic "error" line) say that a volume's VSS snapshot failed and restic
// fell back to reading it live. volume is the upper-case drive letter alone
// ("C"), the form the wire carries (api.RunStats.VSSFallbackVolumes), or ""
// when the volume is not a drive letter (a GUID volume path): the fallback
// still happened and must still be reported. Nothing deeper than the letter is
// returned, so the result is path-free.
func VSSFallbackVolume(message, item string) (volume string, ok bool) {
	if !strings.HasPrefix(strings.ToLower(message), vssFallbackPrefix) {
		return "", false
	}
	if l, ok := driveLetter(item); ok {
		return l, true
	}
	// No usable item: the bracket in the message names the same volume.
	rest := message[len(vssFallbackPrefix):]
	if end := strings.IndexByte(rest, ']'); end >= 0 {
		if l, ok := driveLetter(rest[:end]); ok {
			return l, true
		}
	}
	return "", true
}

// driveLetter returns the upper-case letter of "c:", "c:\" or "c:/" and false
// for anything else (a longer path, a GUID volume, a UNC share).
func driveLetter(s string) (string, bool) {
	if len(s) < 2 || len(s) > 3 || s[1] != ':' {
		return "", false
	}
	if len(s) == 3 && s[2] != '\\' && s[2] != '/' {
		return "", false
	}
	c := s[0]
	switch {
	case c >= 'a' && c <= 'z':
		c -= 'a' - 'A'
	case c >= 'A' && c <= 'Z':
	default:
		return "", false
	}
	return string(c), true
}
