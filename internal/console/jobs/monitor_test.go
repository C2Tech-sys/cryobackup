package jobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

func pingRig(t *testing.T) (*store.Store, *time.Time, *httptest.Server, *atomic.Int64, *atomic.Value) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	clock := &now
	st, err := store.OpenMemory(context.Background(), func() time.Time { return *clock })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var hits atomic.Int64
	var method atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		method.Store(r.Method)
		if strings.HasSuffix(r.URL.Path, "/fail") {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return st, clock, srv, &hits, &method
}

func recordEvaluator(t *testing.T, st *store.Store, status string, at time.Time) {
	t.Helper()
	if err := st.RecordJobRun(context.Background(), NameAlarmEvaluator, store.JobRun{Status: status, StartedAt: at, FinishedAt: at}); err != nil {
		t.Fatal(err)
	}
}

// §4.8 C13 input: the alarm-evaluator loop records every run in job_state
// (C06 reads last_finished_at); a failed evaluation is an error row, never a
// silent skip, and the loop sends nothing (notifications are M4).
func TestC13AlarmEvaluatorLoopRecordsRuns(t *testing.T) {
	st, _, _, _, _ := pingRig(t)
	var calls atomic.Int64
	var fail atomic.Bool
	j := &AlarmEvaluator{Store: st, View: func(context.Context) (state.FleetView, error) {
		calls.Add(1)
		if fail.Load() {
			return state.FleetView{}, &ClassError{Class: ClassStore}
		}
		return state.FleetView{}, nil
	}}
	if got := j.Job(); got.Name != NameAlarmEvaluator || got.Every != time.Minute {
		t.Fatalf("job %+v", got)
	}
	r := NewRunner(st, "t", nil)
	r.Add(j.Job())
	ctx := context.Background()
	if ran, err := r.RunOnce(ctx, NameAlarmEvaluator); !ran || err != nil {
		t.Fatalf("ran %v err %v", ran, err)
	}
	row, _ := st.GetJobState(ctx, NameAlarmEvaluator)
	if row.LastStatus != "ok" || row.LastFinishedAt == nil || calls.Load() != 1 {
		t.Fatalf("row %+v calls %d", row, calls.Load())
	}
	okRow, err := st.GetJobState(ctx, NameAlarmEvaluatorOK)
	if err != nil || okRow.LastFinishedAt == nil {
		t.Fatalf("success row %+v %v", okRow, err)
	}
	fail.Store(true)
	_, _ = r.RunOnce(ctx, NameAlarmEvaluator)
	row, _ = st.GetJobState(ctx, NameAlarmEvaluator)
	if row.LastStatus != "error" || row.LastErrorClass != ClassStore {
		t.Fatalf("failed evaluation row %+v", row)
	}
	// A failed evaluation never stamps the success row (C06 reads it).
	if again, _ := st.GetJobState(ctx, NameAlarmEvaluatorOK); again.RunCount != okRow.RunCount {
		t.Fatalf("a failed evaluation stamped the success row: %d -> %d", okRow.RunCount, again.RunCount)
	}
	if ok, _ := EvaluatorHealthy(ctx, st, 0, st.Now()); ok {
		t.Fatal("a failing evaluator reads healthy")
	}
}
