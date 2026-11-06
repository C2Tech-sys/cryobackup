package health

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T5 (C): the banner the evaluator words itself (C06, an evaluator that stopped, before the probes are
// wired) cites its time on a 12-hour clock in the display zone, not as 24-hour UTC.
func stalledEvaluator(t *testing.T) (*Evaluator, context.Context) {
	t.Helper()
	ctx := context.Background()
	e, _ := newEval(t)
	e.MasterLoaded = func() bool { return true }
	if _, err := e.Store.AcquireJobLease(ctx, EvaluatorJob, "t", time.Second); err != nil {
		t.Fatal(err)
	}
	st := fixtures.Now().Add(-10 * time.Minute)
	if err := e.Store.RecordJobRun(ctx, EvaluatorJob, store.JobRun{Status: "ok", StartedAt: st, FinishedAt: st}); err != nil {
		t.Fatal(err)
	}
	return e, ctx
}

func c06Text(t *testing.T, e *Evaluator, ctx context.Context) string {
	t.Helper()
	f, err := e.Fleet(ctx, store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range f.View.Banners {
		if b.ID == "C06" {
			return b.Text
		}
	}
	t.Fatal("no C06")
	return ""
}

func TestM3T5C_C06TimeIsTwelveHour(t *testing.T) {
	e, ctx := stalledEvaluator(t)
	// no zone given: UTC, but on a 12-hour clock (it said "Last evaluation 18:32.")
	if got := c06Text(t, e, ctx); !strings.HasSuffix(got, "Last evaluation 6:32 PM.") {
		t.Fatalf("C06: %q", got)
	}
}

func TestM3T5C_C06TimeIsInTheDisplayZone(t *testing.T) {
	e, ctx := stalledEvaluator(t)
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no zone database")
	}
	e.Zone = func(context.Context) *time.Location { return chicago }
	if got := c06Text(t, e, ctx); !strings.HasSuffix(got, "Last evaluation 1:32 PM.") {
		t.Fatalf("C06: %q", got)
	}
}
