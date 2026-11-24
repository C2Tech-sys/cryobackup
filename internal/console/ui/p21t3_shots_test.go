package ui

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestP21T3_Screenshots writes the screens ADR 0023 changed the faces of (fleet, device detail,
// login, the retire dialog at 390 px, /design), dark and light, with the real fonts, for the
// operator's review. Not a gate: it runs only when CRYO_SHOTS names a directory.
func TestP21T3_Screenshots(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("CRYO_SHOTS")
	if dir == "" {
		t.Skip("CRYO_SHOTS names no directory: the screenshots are for review, not a gate")
	}
	signedOut := newEnv(t)
	e := newEnv(t)
	e.login()
	shots := []struct {
		name, path string
		env        *env
		widths     []int
	}{
		{"fleet", "/", e, []int{1280, 390}},
		{"device", "/devices/" + e.dev["FRONTDESK-01"], e, []int{1280, 390}},
		{"login", "/login", signedOut, []int{1280, 390}},
		{"retire", "/devices/" + e.dev["FRONTDESK-01"] + "?retire=1", e, []int{390}},
		{"design", "/design", e, []int{1280, 390}},
	}
	for _, s := range shots {
		for _, w := range s.widths {
			for _, theme := range []string{"dark", "light"} {
				h := 3600
				if s.name == "design" {
					h = 12000
				}
				s.env.screenshot(t, s.path, theme, w, h, filepath.Join(dir, "t3_"+s.name+"_"+strconv.Itoa(w)+"_"+theme+".png"), "")
			}
		}
	}
}
