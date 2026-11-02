package api

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// PathFreeError is what AssertPathFree returns: the JSON path of the first
// value that looks like a file-system path, and a fixed reason. It never holds
// the value: the error text is safe to log (architecture §8.4).
type PathFreeError struct {
	// Path is the JSON path of the offender, for example
	// "runs[2].error_counts[0].ext" or "commands[0].params.where". A map key
	// that is not a plain identifier is shown as "*".
	Path string
	// Reason is one of the fixed phrases of this package.
	Reason string
}

// Reasons of a PathFreeError.
const (
	reasonPathLike = "path-like value"
	reasonRawJSON  = "unparseable raw JSON"
	reasonDepth    = "nesting too deep"
)

// Error implements error: "api: <reason> at <json path>".
func (e *PathFreeError) Error() string {
	p := e.Path
	if p == "" {
		p = "(root)"
	}
	return "api: " + e.Reason + " at " + p
}

// maxWalkDepth bounds the nesting AssertPathFree follows; deeper input fails
// closed.
const maxWalkDepth = 64

var (
	// A drive-letter path anywhere in a string: one ASCII letter that does not
	// follow a letter or digit, then ':' and a separator. "C:\x", "x C:/foo" and
	// "path:C:\foo" match; "https://" (the 's' follows a letter), "12:30",
	// "tag:value" and "urn:cryobackup:error:x" do not.
	driveRE = regexp.MustCompile(`(^|[^A-Za-z0-9])[A-Za-z]:[\\/]`)
	// A POSIX path of two or more segments that starts a string or follows any
	// character that is neither a word character nor '/': "/home/x/y",
	// "path:/home/x/y", "url=/etc/passwd". Not after a word character, so
	// "windows/amd64" and "2026/10/22" pass; not after '/', so the "//host/..."
	// of a URL is not read as a path.
	posixRE = regexp.MustCompile(`(^|[^\w/])/[^/\s]+/[^/\s]+`)
	// A forward-slash UNC path, "//server/share", at a string start or after a
	// space, quote, '=', '(' ',' or ';'.
	uncSlashRE = regexp.MustCompile(`(^|[\s"'=(,;])//[^/\s]+/[^/\s]+`)
	// A home-relative or relative prefix at a token start: "~/", "./", "../".
	relativeRE = regexp.MustCompile(`(^|[^\w~.])(~/|\.\.?/)`)
	// The URI schemes that name a file or a share, "file:" and "smb:" in any
	// case at a token start (so "profile:" is not one). The "\\?\" device prefix
	// is covered by the two-backslash rule in looksLikePath.
	schemeRE = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])(file|smb):`)
	// A %VAR% expansion: %APPDATA%, %ProgramFiles(x86)%.
	envRE = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_().]*%`)
	// A backslash followed by a word character: \Users, DOMAIN\user, a\b.
	backslashRE = regexp.MustCompile(`\\\w`)
)

// looksLikePath reports whether s contains a Windows drive path, a UNC or
// device prefix (two backslashes, which includes \\?\), a POSIX path of two or
// more segments, a "~/", "./" or "../" prefix, a file: or smb: URI, a %VAR%
// expansion or a backslash followed by a word character (architecture §8.4
// item 2). It is a heuristic net, not a parser: see AssertPathFree.
func looksLikePath(s string) bool {
	if s == "" {
		return false
	}
	return strings.Contains(s, `\\`) || driveRE.MatchString(s) || posixRE.MatchString(s) ||
		uncSlashRE.MatchString(s) || relativeRE.MatchString(s) || schemeRE.MatchString(s) ||
		envRE.MatchString(s) || backslashRE.MatchString(s)
}

var (
	pathBearingType = reflect.TypeOf((*pathBearing)(nil)).Elem()
	rawMessageType  = reflect.TypeOf(json.RawMessage(nil))
	snapshotType    = reflect.TypeOf(restic.Snapshot{})
)

// AssertPathFree checks that no string anywhere in v looks like a file-system
// path: a drive-letter path (C:\ or C:/, anywhere in the string), a UNC or
// device prefix (two backslashes, \\?\), a POSIX path of two or more
// '/'-separated segments after any non-word character or at the start, a "~/",
// "./" or "../" prefix, a file: or smb: URI, a %VAR% expansion, or a backslash
// followed by a word character. It walks the exported fields, slices and maps
// of v by their JSON names, looks inside json.RawMessage values (command
// Params and Result, the release Manifest) as JSON, and also checks map keys.
// On the first offender it returns a *PathFreeError holding the JSON path,
// never the value; it returns nil when v is clean.
//
// Two things are exempt: any value whose type implements the pathBearing marker
// (RestoreSelection, the single path-bearing type of the protocol) and the Paths
// field of a restic.Snapshot (the backup roots of a snapshot, architecture §5.1;
// SnapshotsRequest.Validate caps their number). Everything else in a snapshot
// (host name, tags, program version) is checked.
//
// The console calls it on every body it ingests and the agent on every body it
// sends (architecture §8.4 item 8). The strings that are meant to be paths
// elsewhere, such as the selection of a policy, are never in an ingested type.
//
// It is a safety net, not the control: the control is that the wire types have
// no field that can hold a path (doc.go, "The path-free rule"). Known limit: a
// relative path with forward slashes and no leading '/' or "./"
// ("Users/x/a.docx") cannot be told from "windows/amd64" and stays
// undetectable; a bare file name ("a.docx") is not path-like either.
func AssertPathFree(v any) error {
	if v == nil {
		return nil
	}
	return walkValue(reflect.ValueOf(v), "", 0)
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

// safeKey returns key for use in a path when it is a plain identifier, "*" otherwise.
func safeKey(key string) string {
	if key == "" || len(key) > 64 {
		return "*"
	}
	for i := 0; i < len(key); i++ {
		ch := key[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_', ch == '-':
		default:
			return "*"
		}
	}
	return key
}

func walkValue(rv reflect.Value, path string, depth int) error {
	if depth > maxWalkDepth {
		return &PathFreeError{Path: path, Reason: reasonDepth}
	}
	switch rv.Kind() {
	case reflect.Invalid:
		return nil
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return walkValue(rv.Elem(), path, depth)
	}
	t := rv.Type()
	if t.Implements(pathBearingType) || reflect.PointerTo(t).Implements(pathBearingType) {
		return nil
	}
	switch rv.Kind() {
	case reflect.String:
		if looksLikePath(rv.String()) {
			return &PathFreeError{Path: path, Reason: reasonPathLike}
		}
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			if t == snapshotType && f.Name == "Paths" {
				continue
			}
			name, skip := jsonName(f)
			if skip {
				continue
			}
			child := joinPath(path, name)
			if f.Anonymous && f.Tag.Get("json") == "" && f.Type.Kind() == reflect.Struct {
				child = path
			}
			if err := walkValue(rv.Field(i), child, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if t == rawMessageType {
			return walkRaw(rv.Bytes(), path)
		}
		if t.Elem().Kind() == reflect.Uint8 {
			return nil // []byte marshals to base64: not text
		}
		fallthrough
	case reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if err := walkValue(rv.Index(i), path+"["+strconv.Itoa(i)+"]", depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil
		}
		keys := rv.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			child := joinPath(path, safeKey(k.String()))
			if looksLikePath(k.String()) {
				return &PathFreeError{Path: joinPath(path, "*"), Reason: reasonPathLike}
			}
			if err := walkValue(rv.MapIndex(k), child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonName returns the JSON object key of a struct field and whether it is
// excluded from JSON.
func jsonName(f reflect.StructField) (name string, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	name, _, _ = strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, false
}

// walkRaw checks a raw JSON document. Invalid JSON fails closed.
func walkRaw(raw []byte, path string) error {
	if len(raw) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return &PathFreeError{Path: path, Reason: reasonRawJSON}
	}
	return walkGeneric(doc, path, 0)
}

func walkGeneric(v any, path string, depth int) error {
	if depth > maxWalkDepth {
		return &PathFreeError{Path: path, Reason: reasonDepth}
	}
	switch x := v.(type) {
	case string:
		if looksLikePath(x) {
			return &PathFreeError{Path: path, Reason: reasonPathLike}
		}
	case []any:
		for i, e := range x {
			if err := walkGeneric(e, path+"["+strconv.Itoa(i)+"]", depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if looksLikePath(k) {
				return &PathFreeError{Path: joinPath(path, "*"), Reason: reasonPathLike}
			}
			if err := walkGeneric(x[k], joinPath(path, safeKey(k)), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
