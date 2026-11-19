package jobs

import (
	"context"
	"testing"
	"time"
)

// M3-T4 r4 M2: MaxJitter bounds the timer the runner really uses, for the periods it really runs.
func TestMaxJitterBoundsTheRunnersTimer(t *testing.T) {
	if got := MaxJitter(time.Hour); got != 6*time.Minute {
		t.Fatalf("MaxJitter(1 h) = %v", got)
	}
	for _, every := range []time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute, time.Hour} {
		for i := 0; i < 20000; i++ {
			if d := jittered(every) - every; d > MaxJitter(every) || d < -MaxJitter(every) {
				t.Fatalf("every %v: the timer moved by %v, past MaxJitter %v", every, d, MaxJitter(every))
			}
		}
	}
}

// A skipped run is recorded as skipped, with its next due time, and is not an error.
func TestASkipDoesNotWipeOutAFailure(t *testing.T) {
	st, _, _, _, _ := pingRig(t)
	r := NewRunner(st, "t", nil)
	outcome := error(&ClassError{Class: "status_500"})
	r.Add(Job{Name: "j", Every: time.Minute, Run: func(context.Context) error { return outcome }})
	ctx := context.Background()
	if _, err := r.RunOnce(ctx, "j"); err == nil {
		t.Fatal("the first run should fail")
	}
	outcome = errSkipped
	if ran, err := r.RunOnce(ctx, "j"); !ran || err != nil {
		t.Fatalf("skip: ran %v err %v", ran, err)
	}
	row, err := st.GetJobState(ctx, "j")
	if err != nil || row.LastStatus != "error" || row.LastErrorClass != "status_500" {
		t.Fatalf("a skip after a failure: row %+v err %v", row, err)
	}
	outcome = nil
	if _, err := r.RunOnce(ctx, "j"); err != nil {
		t.Fatal(err)
	}
	if row, err = st.GetJobState(ctx, "j"); err != nil || row.LastStatus != "ok" {
		t.Fatalf("a pass clears it: row %+v err %v", row, err)
	}
}

func TestARunThatSkipsIsRecordedAsSkipped(t *testing.T) {
	st, _, _, _, _ := pingRig(t)
	r := NewRunner(st, "t", nil)
	r.Add(Job{Name: "j", Every: time.Minute, Run: func(context.Context) error { return errSkipped }})
	ran, err := r.RunOnce(context.Background(), "j")
	if !ran || err != nil {
		t.Fatalf("ran %v err %v", ran, err)
	}
	row, err := st.GetJobState(context.Background(), "j")
	if err != nil || row.LastStatus != "skipped" || row.LastErrorClass != "awaiting_first_evaluation" || row.NextDueAt == nil {
		t.Fatalf("row %+v err %v", row, err)
	}
}
