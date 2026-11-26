package maintain

import "testing"

// P22-R2b B7: the worker's ping carries its drill mode, so the console can compare it with its own
// (cli workermode.go) and doctor can fail a mismatch. Killed mutant: Drill left out of the ping.
func TestPingReportsDrillMode(t *testing.T) {
	e := newTenv(t, nil, nil)
	pr, err := e.w.Ping(e.ctx)
	if err != nil || pr.Drill {
		t.Fatalf("production worker ping = %+v, %v", pr, err)
	}
	e.w.enterDrill()
	leaveDrill(t, e.w)
	if pr, err = e.w.Ping(e.ctx); err != nil || !pr.Drill {
		t.Fatalf("drill worker ping = %+v, %v", pr, err)
	}
}
