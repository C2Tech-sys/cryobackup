package jobs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
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

// §5.7 healthcheck-ping: the outbound ping goes out only while the
// alarm-evaluator is healthy (its newest run finished ok within three periods);
// otherwise it is withheld and the run records evaluator_unhealthy, so the
// external monitor alerts exactly when alerting stops.
func TestC13HealthcheckPingOnlyWhileEvaluatorHealthy(t *testing.T) {
	st, now, srv, hits, method := pingRig(t)
	ctx := context.Background()
	j := &HealthcheckPing{Store: st, URL: srv.URL + "/ping/abc", Method: "POST"}
	var ce *ClassError

	// Never ran: withheld.
	if err := j.Run(ctx); !errors.As(err, &ce) || ce.Class != ClassEvaluatorUnhealthy || hits.Load() != 0 {
		t.Fatalf("no evaluator row: err %v hits %d", err, hits.Load())
	}
	// Ran ok 30 s ago: sent, with the configured method.
	recordEvaluator(t, st, "ok", now.Add(-30*time.Second))
	if err := j.Run(ctx); err != nil || hits.Load() != 1 || method.Load() != "POST" {
		t.Fatalf("healthy evaluator: err %v hits %d method %v", err, hits.Load(), method.Load())
	}
	// Ran, but failed: withheld.
	recordEvaluator(t, st, "error", now.Add(-10*time.Second))
	if err := j.Run(ctx); !errors.As(err, &ce) || ce.Class != ClassEvaluatorUnhealthy || hits.Load() != 1 {
		t.Fatalf("failing evaluator: err %v hits %d", err, hits.Load())
	}
	// Finished ok, but four periods ago: stalled, withheld.
	recordEvaluator(t, st, "ok", now.Add(-4*time.Minute))
	if err := j.Run(ctx); !errors.As(err, &ce) || ce.Class != ClassEvaluatorUnhealthy || hits.Load() != 1 {
		t.Fatalf("stalled evaluator: err %v hits %d", err, hits.Load())
	}
	// A monitor answering 503 is the run's error class; the URL never appears in it.
	recordEvaluator(t, st, "ok", *now)
	bad := &HealthcheckPing{Store: st, URL: srv.URL + "/ping/secret-token/fail"}
	err := bad.Run(ctx)
	if !errors.As(err, &ce) || ce.Class != "status_503" || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("503: %v", err)
	}
	if method.Load() != "GET" {
		t.Fatalf("default method %v", method.Load())
	}
}

// Through the runner: the job_state row says why, and the log never carries the URL.
func TestHealthcheckPingThroughTheRunnerIsTokenFree(t *testing.T) {
	st, _, srv, _, _ := pingRig(t)
	var logs bytes.Buffer
	r := NewRunner(st, "t", logx.New(&logs, logx.LevelDebug))
	r.Add((&HealthcheckPing{Store: st, URL: srv.URL + "/ping/secret-token"}).Job())
	if ran, err := r.RunOnce(context.Background(), NameHealthcheckPing); !ran || err == nil {
		t.Fatalf("ran %v err %v", ran, err)
	}
	row, err := st.GetJobState(context.Background(), NameHealthcheckPing)
	if err != nil || row.LastStatus != "error" || row.LastErrorClass != ClassEvaluatorUnhealthy {
		t.Fatalf("row %+v %v", row, err)
	}
	if strings.Contains(logs.String(), "secret-token") || strings.Contains(logs.String(), srv.URL) {
		t.Fatalf("logs carry the URL:\n%s", logs.String())
	}
	if (&HealthcheckPing{}).Job().Every != DefaultHealthcheckPingEvery {
		t.Fatal("default period")
	}
}
