package logx

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Free text is OPAQUE BY DEFAULT (M1-FIX4, foreman ruling after three rounds
// of leaks through pattern lists). A string value, or a message, survives the
// handler unchanged only if EVERY white-space-separated token of it is one of
// the shapes freeTokenOK accepts; otherwise the whole value becomes one
// "<path:hmac8>" marker. There is no list of file extensions, no TLD list, no
// URL or quote parsing to get wrong: a client's file name, a path, a URL, a
// quoted operand, a backend string, a non-ASCII look-alike dot all hold a
// character outside the accepted shapes, so the value goes.
//
// The accepted token shapes:
//
//   - ASCII letters only, optionally starting with one "(" or "[" and ending
//     in one of ": , ; ) ]" ("dial", "denied:", "(timeout)");
//   - ASCII digits only, or a hex code 0x..., optionally ending in one of
//     ": , ;" ("3", "0x80070005");
//   - a version: v?N.N[.N[.N]] or goN.N[.N] ("0.19.1", "go1.27");
//   - a duration or size: N[.N] with a unit of ns us µs ms s m h B KB KiB MB
//     MiB GB GiB % ("1.2s", "38.5%");
//   - the punctuation tokens - : = -> |.
//
// Over-redaction is accepted ("dial tcp 10.0.0.9:443: i/o timeout" goes as a
// whole); a leak is not. What an operator needs to diagnose an outage comes
// from TYPED fields the code itself fills (Host, Class, Code, ID, Count, ...),
// never from free text.

var (
	freeWordRE    = regexp.MustCompile(`^[(\[]?[A-Za-z]+[:,;)\]]?$`)
	freeNumberRE  = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-fA-F]+)[:,;]?$`)
	freeVersionRE = regexp.MustCompile(`^(?:v?[0-9]+(?:\.[0-9]+){1,3}|go[0-9]+(?:\.[0-9]+){1,2})$`)
	freeSizeRE    = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h|B|KB|KiB|MB|MiB|GB|GiB|%)$`)
	freePunct     = map[string]bool{"-": true, ":": true, "=": true, "->": true, "|": true}
)

// freeTokenOK reports whether one white-space-free token of free text is one
// of the accepted shapes.
func freeTokenOK(tok string) bool {
	return freePunct[tok] || freeWordRE.MatchString(tok) || freeNumberRE.MatchString(tok) ||
		freeVersionRE.MatchString(tok) || freeSizeRE.MatchString(tok)
}

// freeTextOK reports whether every token of s is accepted.
func freeTextOK(s string) bool {
	for _, tok := range strings.Fields(s) {
		if !freeTokenOK(tok) {
			return false
		}
	}
	return true
}

// redactor holds the per-process random key that makes "<path:hmac8>" stable
// inside one process (the same value hashes to the same marker, so two log
// lines can be correlated) and meaningless outside it.
type redactor struct{ key []byte }

func newRedactor() *redactor {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("logx: crypto/rand failed: " + err.Error()) // cannot happen on a supported platform
	}
	return &redactor{key: k}
}

func (r *redactor) mark(s string) string {
	m := hmac.New(sha256.New, r.key)
	m.Write([]byte(s))
	return "<path:" + hex.EncodeToString(m.Sum(nil)[:4]) + ">"
}

// String is the free-text path: s unchanged when every token is accepted,
// else one marker for the whole value.
func (r *redactor) String(s string) string {
	if freeTextOK(s) {
		return s
	}
	return r.mark(s)
}

// ErrorText renders err without ever printing a path. A chain that contains a
// *fs.PathError, *os.LinkError or *exec.Error is reduced to the operation and a
// fixed class; any other error text is free text (String).
func (r *redactor) ErrorText(err error) string {
	if err == nil {
		return ""
	}
	if s, ok := PathFreeError(err); ok {
		return s
	}
	return r.String(err.Error())
}

// PathFreeError reports the path-free form of err when its chain holds an
// error type whose Error() prints a path: "op:<op> class:<class>".
func PathFreeError(err error) (string, bool) {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return "op:" + safeToken(pe.Op) + " class:" + errClass(pe.Err), true
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return "op:" + safeToken(le.Op) + " class:" + errClass(le.Err), true
	}
	var xe *exec.Error
	if errors.As(err, &xe) {
		return "op:exec class:" + errClass(xe.Err), true
	}
	return "", false
}

func errClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not_exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, fs.ErrExist):
		return "exist"
	case errors.Is(err, exec.ErrNotFound):
		return "not_found"
	case errors.Is(err, fs.ErrClosed):
		return "closed"
	}
	return "io"
}

// tokenDotRE finds a dot followed by a letter or digit: a typed token may hold
// one only as a version ("0.19.1"); "x.docx" or "123456.7z" is refused.
var tokenDotRE = regexp.MustCompile(`\.[A-Za-z0-9]`)

// safeToken reduces s to a short allow-listed token for the typed
// constructors (ID, Class, Kind, Str): [A-Za-z0-9_.-] (no colon: "b2:bucket:x"
// is a backend string), at most 64 bytes, and
// a dot followed by a letter or digit only in a version; anything else
// becomes "invalid".
func safeToken(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 64 {
		return "invalid"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-':
		default:
			return "invalid"
		}
	}
	if tokenDotRE.MatchString(s) && !freeVersionRE.MatchString(s) {
		return "invalid"
	}
	return s
}
