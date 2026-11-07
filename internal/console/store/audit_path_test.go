package store

import (
	"encoding/json"
	"testing"
)

// M3-HARDEN B: an absolute POSIX path in a pause reason or a view name is refused like a
// Windows one; ordinary prose with a slash is not.
func TestAuditPathCheckSeesPosixPathsAndLeavesProseAlone(t *testing.T) {
	js := func(s string) string {
		b, err := json.Marshal(map[string]string{"reason": s})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, bad := range []string{"/home/bob/x.txt", "copied /home/bob/x.txt today", "(/srv/data/chart.pdf)", "path=/var/lib/x", `C:\Users\bob`, `\\srv\share`, "c:/users/bob"} {
		if !looksLikePath(js(bad)) {
			t.Errorf("%q is not seen as a path", bad)
		}
	}
	for _, ok := range []string{"and/or", "on/off site", "gateway/storage down", "1/2 done", "24/7 machine", "10.0.0.0/24", "/24", "w/o power", "see https://example.invalid/a/b", "patching", "a/b/c"} {
		if looksLikePath(js(ok)) {
			t.Errorf("%q is refused as a path", ok)
		}
	}
}
