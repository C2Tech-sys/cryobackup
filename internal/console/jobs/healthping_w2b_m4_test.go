package jobs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P20-W2B item 5: a 2xx whose body then stalls or breaks is a failed ping of class timeout, not a
// success: the monitor may never have recorded the request as complete.
func TestM4HeartbeatPingBodyThatStallsOrBreaksIsATimeout(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		build func(*testing.T, *hbRig) *HeartbeatPing
	}{
		{"body stalls after the status line", func(t *testing.T, r *hbRig) *HeartbeatPing {
			srv, _ := hbServer(t, func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("o"))
				w.(http.Flusher).Flush()
				<-req.Context().Done()
			})
			j := r.job(secretURL(srv.URL))
			j.Timeout = 150 * time.Millisecond
			return j
		}},
		{"body read fails", func(t *testing.T, r *hbRig) *HeartbeatPing {
			j := r.job(secretURL("https://monitor.example.test"))
			j.Transport = rtFunc(func(req *http.Request) (*http.Response, error) {
				body := readerFunc(func([]byte) (int, error) { return 0, errors.New("reset " + req.URL.String()) })
				return &http.Response{StatusCode: 200, Body: struct {
					readerFunc
					closer
				}{body, closer{}}, Header: http.Header{}, Request: req}, nil
			})
			return j
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHBRig(t)
			rig.evaluator(t, "ok", 0)
			j := tc.build(t, rig)
			err := j.Run(ctx)
			if got := classOfErr(err); got != pingTimeout {
				t.Fatalf("class %q (err %v), want timeout", got, err)
			}
			if f := j.Facts(); f.ConsecutiveFailures != 1 || f.LastFailureClass != pingTimeout || f.LastSuccessAt != 0 {
				t.Fatalf("facts %+v", f)
			}
			assertNoSecret(t, "the error", err)
		})
	}
}

type closer struct{}

func (closer) Close() error { return nil }

// Ported from the deleted HealthcheckPing (M3-T4 r4 low 2): a ping before the first evaluation
// skips (nothing sent, nothing failed) only while the evaluator still has its three periods on
// this console. Unlike the old job, an unknown start time is no grace: a missing evaluation is a
// failure at once (NewHeartbeatPing's doc).
func TestM4HeartbeatPingSkipsOnlyWhileAwaitingTheFirstEvaluation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		started time.Duration // before now; 0 is unknown
		row     string        // the evaluator's newest status
		rowAgo  time.Duration
		skip    bool
	}{
		{name: "fresh console, no row", started: 5 * time.Second, skip: true},
		{name: "start unknown, no row"},
		{name: "restarted, the newest evaluation is from before the start", started: 5 * time.Second, row: "ok", rowAgo: 10 * time.Minute, skip: true},
		{name: "restarted, the newest evaluation failed before the start", started: 5 * time.Second, row: "error", rowAgo: 10 * time.Minute, skip: true},
		{name: "no evaluation in the three periods it had", started: 3*time.Minute + time.Second},
		{name: "evaluated since the start, and it failed", started: time.Minute, row: "error", rowAgo: 30 * time.Second},
		{name: "evaluated long ago, start unknown", row: "ok", rowAgo: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHBRig(t)
			srv, hits := hbServer(t, func(http.ResponseWriter, *http.Request) {})
			var started time.Time
			if tc.started != 0 {
				started = rig.now.Add(-tc.started)
			}
			j := NewHeartbeatPing(rig.st, secretURL(srv.URL), "", 0, started)
			if tc.row != "" {
				rig.evaluator(t, tc.row, tc.rowAgo)
			}
			err := j.Run(ctx)
			switch {
			case tc.skip && !errors.Is(err, errSkipped):
				t.Errorf("err = %v, want the run skipped", err)
			case !tc.skip && classOfErr(err) != ClassEvaluatorUnhealthy:
				t.Errorf("err = %v, want the ping withheld as a failure", err)
			}
			if hits.Load() != 0 {
				t.Errorf("%d pings went out", hits.Load())
			}
		})
	}
}

// Ported from the deleted HealthcheckPing: through the runner, a withheld ping is an error row
// that says evaluator_unhealthy (the System page shows why), and the log carries no URL.
func TestM4HeartbeatPingWithheldThroughTheRunner(t *testing.T) {
	rig := newHBRig(t)
	srv, hits := hbServer(t, func(http.ResponseWriter, *http.Request) {})
	var logs bytes.Buffer
	r := NewRunner(rig.st, "t", logx.New(&logs, logx.LevelDebug))
	if !RegisterHeartbeatPing(r, rig.job(secretURL(srv.URL))) {
		t.Fatal("not registered")
	}
	if ran, err := r.RunOnce(context.Background(), NameHealthcheckPing); !ran || err == nil || hits.Load() != 0 {
		t.Fatalf("ran %v err %v hits %d", ran, err, hits.Load())
	}
	row, err := rig.st.GetJobState(context.Background(), NameHealthcheckPing)
	if err != nil || row.LastStatus != "error" || row.LastErrorClass != ClassEvaluatorUnhealthy {
		t.Fatalf("row %+v %v", row, err)
	}
	if leaks(logs.String()) || strings.Contains(logs.String(), srv.URL) {
		t.Fatalf("logs carry the URL:\n%s", logs.String())
	}
}

// P20-W2B item 4: the prober carries the heartbeat job's facts into ProbeFacts (banner C19); without
// a source they are nil, which raises nothing.
func TestM4HealthProberCarriesTheHeartbeatFacts(t *testing.T) {
	rig := newHBRig(t)
	h := &HealthProber{Store: rig.st, Clock: func() time.Time { return *rig.now }}
	if p := h.Facts(); p.Heartbeat != nil {
		t.Fatalf("no source, facts %+v", p.Heartbeat)
	}
	j := NewHeartbeatPing(rig.st, "https://monitor.example.test"+hbSecretPath, "", 0, rig.now.Add(-time.Hour))
	j.Transport = rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })
	rig.evaluator(t, "ok", 0)
	_ = j.Run(context.Background())
	h.Heartbeat = j.Facts
	p := h.Facts()
	if p.Heartbeat == nil || !p.Heartbeat.Configured || p.Heartbeat.ConsecutiveFailures != 1 || p.Heartbeat.LastFailureClass != "other" {
		t.Fatalf("facts %+v", p.Heartbeat)
	}
}
