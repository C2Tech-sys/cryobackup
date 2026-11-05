package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
)

// M3-STORE1 cleanup: both on-demand probes skipped means nothing was probed, and
// a tick that did not run after Stop leaves no "ok" in the job's row.

// Every way the gateway probe and the storage probe can both be skipped: ran is
// false, with the error of its own, not a cancellation.
func TestC08ProbeNowWithBothProbesSkippedDidNotRun(t *testing.T) { // M3-STORE1 cleanup 4
	for name, storage := range map[string]error{
		"worker unreachable": adminpath.ErrUnavailable,
		"worker busy":        adminpath.NewError(adminpath.CodeBusy, ""),
		"worker store":       adminpath.NewError(adminpath.CodeUnavailable, maintain.ProbeRecordClass),
	} {
		t.Run(name, func(t *testing.T) {
			r := newProberRig(t, false)
			r.hp.Gateway.URL = "" // not configured
			r.be.Errors = map[string]error{adminpath.MethodCatConfig: storage}
			ran, err := r.hp.ProbeNow(context.Background())
			if ran || !errors.Is(err, ErrNothingProbed) {
				t.Fatalf("ProbeNow = (%v, %v), want ran false and ErrNothingProbed", ran, err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("%v reads as a cancellation", err)
			}
		})
	}
}

// One probe skipped and the other real is still a run: its checks speak for themselves.
func TestC08ProbeNowWithOneProbeSkippedRan(t *testing.T) { // M3-STORE1 cleanup 4
	t.Run("gateway not configured, storage probed", func(t *testing.T) {
		r := newProberRig(t, false)
		r.hp.Gateway.URL = ""
		if ran, err := r.hp.ProbeNow(context.Background()); !ran || err != nil {
			t.Fatalf("ProbeNow = (%v, %v), want (true, nil)", ran, err)
		}
	})
	t.Run("gateway probed, worker unreachable", func(t *testing.T) {
		r := newProberRig(t, false)
		r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.ErrUnavailable}
		if ran, err := r.hp.ProbeNow(context.Background()); !ran || err != nil {
			t.Fatalf("ProbeNow = (%v, %v), want (true, nil)", ran, err)
		}
	})
}

// A tick the runner calls after Stop did not run: RunOnce says so, and the job's
// row keeps what it had. An "ok" there would say a tick passed that never happened.
func TestC08RunnerRecordsNothingForATickAfterStop(t *testing.T) { // M3-STORE1 cleanup 5
	t.Run("a job that has run", func(t *testing.T) {
		r := newProberRig(t, false)
		runner := NewRunner(r.st, "console-a", nil)
		runner.Add(r.hp.Job())
		if ran, err := runner.RunOnce(context.Background(), NameHealthProber); !ran || err != nil {
			t.Fatalf("the tick before Stop: ran %v err %v", ran, err)
		}
		before := r.row(t, NameHealthProber)
		if before.RunCount != 1 || before.LastStatus != "ok" {
			t.Fatalf("row before Stop: %+v", before)
		}
		r.hp.Stop()
		r.clk.Advance(time.Minute)
		if ran, err := runner.RunOnce(context.Background(), NameHealthProber); ran || err != nil {
			t.Fatalf("the tick after Stop: ran %v err %v, want (false, nil)", ran, err)
		}
		after := r.row(t, NameHealthProber)
		if after.RunCount != 1 || !after.LastFinishedAt.Equal(*before.LastFinishedAt) {
			t.Fatalf("row after a tick that did not run: %+v, was %+v", after, before)
		}
		if after.LeaseHolder != "" {
			t.Fatalf("the runner kept the job lease: %+v", after)
		}
	})
	t.Run("a job that never ran", func(t *testing.T) {
		r := newProberRig(t, false)
		runner := NewRunner(r.st, "console-a", nil)
		runner.Add(r.hp.Job())
		r.hp.Stop()
		if ran, err := runner.RunOnce(context.Background(), NameHealthProber); ran || err != nil {
			t.Fatalf("the tick after Stop: ran %v err %v, want (false, nil)", ran, err)
		}
		if j := r.row(t, NameHealthProber); j.RunCount != 0 || j.LastStatus != "" {
			t.Fatalf("row after a tick that did not run: %+v, want no run recorded", j)
		}
	})
}
