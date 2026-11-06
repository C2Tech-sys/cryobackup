package jobs

import (
	"context"
	"errors"
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

// M3-T4 r4 low 2: a ping before the first evaluation skips: nothing is sent, nothing fails.
func TestHealthcheckPingSkipsOnlyWhileAwaitingTheFirstEvaluation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		started time.Duration // before now; 0 is unknown
		row     string        // the evaluator's newest status
		rowAgo  time.Duration
		skip    bool
	}{
		{name: "fresh console, no row", started: 5 * time.Second, skip: true},
		{name: "start unknown, no row", skip: true},
		{name: "restarted, the newest evaluation is from before the start", started: 5 * time.Second, row: "ok", rowAgo: 10 * time.Minute, skip: true},
		{name: "restarted, the newest evaluation failed before the start", started: 5 * time.Second, row: "error", rowAgo: 10 * time.Minute, skip: true},
		{name: "no evaluation in the three periods it had", started: 3*time.Minute + time.Second},
		{name: "evaluated since the start, and it failed", started: time.Minute, row: "error", rowAgo: 30 * time.Second},
		{name: "evaluated long ago, start unknown", row: "ok", rowAgo: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, now, srv, hits, _ := pingRig(t)
			j := &HealthcheckPing{Store: st, URL: srv.URL + "/ping/abc"}
			if tc.started != 0 {
				j.Started = now.Add(-tc.started)
			}
			if tc.row != "" {
				recordEvaluator(t, st, tc.row, now.Add(-tc.rowAgo))
			}
			err := j.Run(ctx)
			var ce *ClassError
			switch {
			case tc.skip && !errors.Is(err, errSkipped):
				t.Errorf("err = %v, want the run skipped", err)
			case !tc.skip && (!errors.As(err, &ce) || ce.Class != ClassEvaluatorUnhealthy):
				t.Errorf("err = %v, want the ping withheld as a failure", err)
			}
			if hits.Load() != 0 {
				t.Errorf("%d pings went out", hits.Load())
			}
		})
	}
}

// A skipped run is recorded as skipped, with its next due time, and is not an error.
func TestARunThatSkipsIsRecordedAsSkipped(t *testing.T) {
	st, _, _, _, _ := pingRig(t)
	r := NewRunner(st, "t", nil)
	r.Add(Job{Name: "j", Every: time.Minute, Run: func(context.Context) error { return errSkipped }})
	ran, err := r.RunOnce(context.Background(), "j")
	if !ran || err != nil {
		t.Fatalf("ran %v err %v", ran, err)
	}
	row, err := st.GetJobState(context.Background(), "j")
	if err != nil || row.LastStatus != "skipped" || row.LastErrorClass != "" || row.NextDueAt == nil {
		t.Fatalf("row %+v err %v", row, err)
	}
}
