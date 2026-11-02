package config

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Verdict is the persisted "untrusted - repair required" verdict (ticket
// P1-FIX10). An elevated start that finds the state directory untrusted
// records it before it refuses; while it exists every start refuses the state
// directory, elevated or not, lab flag or not, and only a successful
// --repair-acl (quarantine and a verified fresh root) clears it.
//
// It is kept where an unprivileged account cannot write, so it can neither be
// planted (to force a refusal and a re-enrolment) nor removed:
//
//   - Windows: the REG_SZ value UntrustedRepairRequired-<VerdictID(root)> of
//     HKLM\SOFTWARE\CryoBackup\Agent ("<RFC 3339 UTC stamp>|<reason>"),
//     written and cleared by elevated starts only (registry.SET_VALUE; HKLM
//     refuses an unprivileged writer) and read without privilege
//     (registry.QUERY_VALUE). There is one verdict per state directory, so a
//     refused lab --state-dir never blocks the service's default one. An
//     absent key or value is no verdict.
//   - elsewhere (the CI and lab stand-in only): the file
//     <name>.untrusted-verdict next to the state root, mode 0600, honoured
//     only when it is a regular file owned by uid 0 (the current uid in this
//     package's tests).
//
// A verdict that exists but does not parse still stands (fail closed); a
// verdict that cannot be read at all is an error, and the caller refuses.
type Verdict struct {
	// Since is when the verdict was recorded (UTC); zero when the stored stamp
	// does not parse.
	Since time.Time
	// Reason is why (path-free: the failing layout directory relative to the
	// root, an owner's SID, the difference found).
	Reason string
}

// maxVerdictReason bounds the stored reason (bytes).
const maxVerdictReason = 1024

// VerdictID names a state directory's verdict without naming its path: the
// first 16 hex digits of the SHA-256 of the absolute, cleaned, lower-cased
// path (Windows paths are case-insensitive, so C:\ProgramData\CryoBackup and
// c:\programdata\cryobackup\. are the same directory and the same verdict).
func VerdictID(root string) string {
	p := root
	if abs, err := filepath.Abs(root); err == nil {
		p = abs
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(p))))
	return hex.EncodeToString(sum[:])[:16]
}

// Describe renders the verdict for messages and "status":
// "<VerdictSource> since <stamp>: <reason>".
func (v Verdict) Describe() string {
	stamp := "an unknown time"
	if !v.Since.IsZero() {
		stamp = v.Since.UTC().Format(time.RFC3339)
	}
	reason := v.Reason
	if reason == "" {
		reason = "no reason recorded"
	}
	return VerdictSource + " since " + stamp + ": " + reason
}

// formatVerdict is the stored form, "<RFC 3339 UTC stamp>|<reason>", on one
// line and bounded.
func formatVerdict(v Verdict) string {
	reason := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return ' '
		}
		return r
	}, v.Reason)
	if len(reason) > maxVerdictReason {
		reason = strings.ToValidUTF8(reason[:maxVerdictReason], "")
	}
	return v.Since.UTC().Format(time.RFC3339) + "|" + reason
}

// parseVerdict reads the stored form back. Whatever is stored is a verdict: a
// value that does not parse keeps standing with what could be read.
func parseVerdict(s string) Verdict {
	stamp, reason, ok := strings.Cut(strings.TrimSpace(s), "|")
	if !ok {
		reason = "unparseable stored verdict"
	}
	var v Verdict
	if t, err := time.Parse(time.RFC3339, stamp); err == nil {
		v.Since = t.UTC()
	}
	if !utf8.ValidString(reason) {
		reason = strings.ToValidUTF8(reason, "?")
	}
	v.Reason = strings.TrimSpace(reason)
	return v
}
