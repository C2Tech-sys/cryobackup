package api

import (
	"strings"
	"testing"
)

// M3-T4 r3 M2: agent_version is a display label. VersionShape decides whether it
// is stored and shown; it never decides whether the run itself is accepted,
// because internal/agent/loops.go journals a record that fails Validate as
// rejected, with no retry, and that run's history is then lost for good.
func TestAVersionLabelNeverFailsTheRun(t *testing.T) {
	for _, ok := range []string{"1.4.2", "dev", "1", "v1.5.0-rc1", "v1.2.0-3-gabc1234-dirty", "abc1234", "1.5.0+build7", "1.5.0-rc.2", "t", "test-1.0", "old", "unknown"} {
		if !VersionShape(ok) {
			t.Errorf("%q is not version-shaped", ok)
		}
		r := okRun()
		r.AgentVersion = ok
		if err := r.Validate(); err != nil {
			t.Errorf("a run with agent_version %q is refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"payroll.xlsx", "Patient_Chart.docx", "secret-payroll.xlsx", "John Smith MRI.pdf", "a/b", "1.4.2 beta", "..", "v1..2", "1.2.3.4.5",
		"v1.0.0-rc1-3-gaf518e2-dirty", strings.Repeat("af518e2c", 5), "release_1.2", "release/1.2", strings.Repeat("v1", 4000), "1.4.2\nx", "1.4.2\x7f"} {
		if VersionShape(bad) {
			t.Errorf("%.30q passes as a version", bad)
		}
		r := okRun()
		r.AgentVersion = bad
		if err := r.Validate(); err != nil {
			t.Errorf("a run with agent_version %.30q is refused, and the agent would lose it: %v", bad, err)
		}
	}
}

// A bare word is version-shaped: the shape keeps sentences and names with an
// extension out, it cannot tell "payroll" from "dev".
func TestABareWordStillPassesAsAVersion(t *testing.T) {
	if !VersionShape("payroll") {
		t.Fatal("the display rule's stated limit changed: update the comments that describe it")
	}
}
