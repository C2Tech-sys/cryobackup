package api

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Problem is one validation failure: Path is the JSON path of the offending
// field ("runs[3].snapshot_id") and Message says what is wrong in a fixed
// phrase. A message never repeats the offending value: a hostname, a path
// fragment or a token must not travel from a request into a log through an
// error text (architecture §8.4).
type Problem struct {
	Path    string
	Message string
}

// ValidationError is what every Validate method returns for a bad value: every
// problem found, sorted by path then message, like internal/policy's
// ValidationError. Use errors.As to reach it. The console answers 400
// invalid_request and may log Problems, which are path-free by construction.
type ValidationError struct {
	Problems []Problem
}

// Error lists the problems, one per line, as "path: message".
func (e *ValidationError) Error() string {
	lines := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		lines[i] = p.Path + ": " + p.Message
	}
	return strings.Join(lines, "\n")
}

// MinTimestamp is the earliest time the database accepts for a run or a
// snapshot (schema CHECK started_at >= 1000000000000 ms, 2001-09-09T01:46:40Z).
// Anything before it is a device whose clock was never set, not a backup.
var MinTimestamp = time.UnixMilli(1_000_000_000_000).UTC()

// Messages used by more than one rule. All are fixed phrases.
const (
	msgRequired   = "is required"
	msgUnknown    = "is not a known value"
	msgNegative   = "must not be negative"
	msgTooLong    = "is too long"
	msgControl    = "must not contain control characters"
	msgNotULID    = "must be a 26-character ULID"
	msgBeforeMin  = "is before 2001-09-09 (clock not set)"
	msgNotFinite  = "must be a finite number"
	msgNotJSONObj = "must be a JSON object"
)

// collector accumulates problems.
type collector struct{ probs []Problem }

func (c *collector) add(path, msg string) {
	c.probs = append(c.probs, Problem{Path: path, Message: msg})
}

// sub merges the problems of a nested Validate under prefix.
func (c *collector) sub(prefix string, err error) {
	if err == nil {
		return
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		c.add(prefix, "is not valid")
		return
	}
	for _, p := range ve.Problems {
		switch {
		case p.Path == "":
			c.add(prefix, p.Message)
		case strings.HasPrefix(p.Path, "["):
			c.add(prefix+p.Path, p.Message)
		default:
			c.add(prefix+"."+p.Path, p.Message)
		}
	}
}

// err returns nil when nothing was collected, else a sorted *ValidationError.
// It returns a plain nil error, never a typed nil.
func (c *collector) err() error {
	if len(c.probs) == 0 {
		return nil
	}
	sort.SliceStable(c.probs, func(i, j int) bool {
		if c.probs[i].Path != c.probs[j].Path {
			return c.probs[i].Path < c.probs[j].Path
		}
		return c.probs[i].Message < c.probs[j].Message
	})
	return &ValidationError{Problems: c.probs}
}

func idx(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// ulid checks the canonical 26-character ULID: Crockford base32, upper case, the
// first character at most 7 (128 bits).
func (c *collector) ulid(path, s string) {
	if !isULID(s) {
		c.add(path, msgNotULID)
	}
}

func isULID(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= '0' && ch <= '9':
		case ch >= 'A' && ch <= 'Z' && ch != 'I' && ch != 'L' && ch != 'O' && ch != 'U':
		default:
			return false
		}
	}
	return true
}

// isLowerHex reports whether s is lower-case hex of min..max characters.
func isLowerHex(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// hexFixed requires exactly n lower-case hex characters.
func (c *collector) hexFixed(path, s string, n int) {
	if !isLowerHex(s, n, n) {
		c.add(path, "must be "+strconv.Itoa(n)+" lowercase hex characters")
	}
}

// hexRange requires min..max lower-case hex characters.
func (c *collector) hexRange(path, s string, min, max int) {
	if !isLowerHex(s, min, max) {
		c.add(path, "must be "+strconv.Itoa(min)+" to "+strconv.Itoa(max)+" lowercase hex characters")
	}
}

// text requires a printable string of at most max bytes; required says whether
// the empty string is a problem.
func (c *collector) text(path, s string, max int, required bool) {
	switch {
	case s == "":
		if required {
			c.add(path, msgRequired)
		}
	case len(s) > max:
		c.add(path, msgTooLong)
	case !utf8.ValidString(s) || hasControl(s):
		c.add(path, msgControl)
	}
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// notNegative records a problem when v < 0.
func (c *collector) notNegative(path string, v int64) {
	if v < 0 {
		c.add(path, msgNegative)
	}
}

// finiteNonNegative records a problem unless v is a finite number >= 0.
func (c *collector) finiteNonNegative(path string, v float64) {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		c.add(path, msgNotFinite)
	case v < 0:
		c.add(path, msgNegative)
	}
}

// notBeforeMin records a problem when t is zero or earlier than MinTimestamp.
func (c *collector) notBeforeMin(path string, t time.Time) {
	switch {
	case t.IsZero():
		c.add(path, msgRequired)
	case t.Before(MinTimestamp):
		c.add(path, msgBeforeMin)
	}
}

// rawObject checks an optional raw JSON parameter blob: absent, or a JSON object
// of at most max bytes.
func (c *collector) rawObject(path string, raw json.RawMessage, max int) {
	if len(raw) == 0 {
		return
	}
	if len(raw) > max {
		c.add(path, msgTooLong)
		return
	}
	if !json.Valid(raw) || firstByte(raw) != '{' {
		c.add(path, msgNotJSONObj)
	}
}

func firstByte(raw []byte) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return b
	}
	return 0
}

// hostname checks a host name: 1..253 bytes, no path separators, no control
// characters (the ticket's rule: a host name that contains / or \ is a path).
func (c *collector) hostname(path, s string) {
	switch {
	case s == "":
		c.add(path, msgRequired)
	case len(s) > 253:
		c.add(path, msgTooLong)
	case strings.ContainsAny(s, `/\`):
		c.add(path, "must not contain path separators")
	case !utf8.ValidString(s) || hasControl(s):
		c.add(path, msgControl)
	}
}

// maxText is the length cap of short free-form identity strings (versions, OS
// names).
const maxText = 128
