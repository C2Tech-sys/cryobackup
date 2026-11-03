package ui

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// M2-FIX-C (F13): fresh.js must not announce "Live updates disconnected" for a
// stream error caused by leaving the page or for a blip that reconnects in
// seconds, but must still announce a real outage and clear it on recovery.
// testdata/fresh_sim.js drives the real fresh.js with a fake DOM, clock and
// EventSource. It needs node; without node the test is skipped and the same
// scenarios are checked by hand (stop the console for 60 s: NO DATA pill, banner
// and "as of" appear; start it: they clear by themselves; reload during normal
// running: no banner flash).
func TestFixCFreshStreamGrace(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; run internal/console/ui/testdata/fresh_sim.js by hand")
	}
	js, err := filepath.Abs("../../../web/static/js/fresh.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "testdata/fresh_sim.js", js).CombinedOutput()
	if err != nil {
		t.Fatalf("fresh.js simulation failed: %v\n%s", err, out)
	}
	t.Logf("\n%s", out)
}
