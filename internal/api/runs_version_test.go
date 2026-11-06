package api

import "testing"

// M3-T4 delta, item 4: a run's agent_version must look like a version, so a
// file name or a sentence is refused at intake instead of stored and shown.
func TestRunAgentVersionMustBeVersionShaped(t *testing.T) {
	for _, ok := range []string{"1.4.2", "dev", "1", "v1.5.0-rc1", "v1.2.0-3-gabc1234-dirty", "abc1234", "1.5.0+build7", "1.5.0-rc.2", "t", "test-1.0", "old"} {
		if !VersionShape(ok) {
			t.Errorf("%q is refused", ok)
		}
		r := okRun()
		r.AgentVersion = ok
		if err := r.Validate(); err != nil {
			t.Errorf("a run with agent_version %q is refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"payroll.xlsx", "Patient_Chart.docx", "secret-payroll.xlsx", "John Smith MRI.pdf", `C:\Users\x`, "a/b", "1.4.2 beta", "..", "v1..2", "1.2.3.4.5"} {
		if VersionShape(bad) {
			t.Errorf("%q passes as a version", bad)
		}
		r := okRun()
		r.AgentVersion = bad
		if r.Validate() == nil {
			t.Errorf("a run with agent_version %q is accepted", bad)
		}
	}
}
