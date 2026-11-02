package store

import "testing"

// M1-FIX3 item 9: each guard of the lease release on the closed-run upload,
// on its own. In TestStartLeaseReleasedByTheClosedRunUpload both guards stop
// the same record, so either could be removed without a failure.

// The IngestRejected guard: the device's own closed record that the console
// rejects (here: an outcome it does not know) releases nothing. The device
// scope cannot help here: it is the lease holder's own record.
func TestStartLeaseNotReleasedByARejectedRecord(t *testing.T) {
	w := seed(t)
	w.setCap(1)
	w.heartbeat(w.devA.ID)
	_ = w.lease(w.devA.ID, runID1)

	bad := runInput(runID1, w.clk.Now())
	bad.Outcome = "exploded"
	res := must[[]IngestResult](t)(w.s.AgentIngestRuns(w.ctx, w.devA.ID, []RunInput{bad}, w.clk.Now()))
	if res[0].Status != IngestRejected {
		t.Fatalf("closed record with an unknown outcome = %+v, want rejected", res[0])
	}
	if n := leaseCount(t, w.s); n != 1 {
		t.Fatalf("a rejected record released the lease (%d left)", n)
	}
	if d := w.lease(w.devB.ID, runID2); d.Granted {
		t.Fatal("device B was granted the only slot after a rejected record of device A")
	}
}

// The device scope: a closed record of another device that the console
// accepts, carrying the run id of device A's lease (device A has not sent its
// open record yet, so the id is free in runs), releases nothing of device A.
// The IngestRejected guard cannot help here: the record is accepted.
func TestStartLeaseNotReleasedByAnotherDevicesRecord(t *testing.T) {
	w := seed(t)
	w.setCap(1)
	w.heartbeat(w.devA.ID)
	w.heartbeat(w.devB.ID)
	_ = w.lease(w.devA.ID, runID1)

	res := must[[]IngestResult](t)(w.s.AgentIngestRuns(w.ctx, w.devB.ID, []RunInput{runInput(runID1, w.clk.Now())}, w.clk.Now()))
	if res[0].Status == IngestRejected {
		t.Fatalf("device B's record = %+v: the test needs it accepted", res[0])
	}
	if n := leaseCount(t, w.s); n != 1 {
		t.Fatalf("device B's closed record released device A's lease (%d left)", n)
	}
	if d := w.lease(w.devB.ID, runID2); d.Granted {
		t.Fatal("device B was granted the only slot by closing a run id it does not hold")
	}
}
