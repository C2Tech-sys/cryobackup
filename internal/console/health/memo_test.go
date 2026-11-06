package health

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 delta, item 6: under WithMemo a request evaluates the fleet once per scope.
func TestMemoEvaluatesOncePerScopeAndRequest(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 18, 42, 10, 0, time.UTC)
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	var evals atomic.Int32 // Extra runs once per evaluation
	e := &Evaluator{Store: st, Clock: func() time.Time { return now }, Extra: func(time.Time) []state.Banner { evals.Add(1); return nil }}

	read := func(c context.Context, scope store.Scope) Fleet {
		t.Helper()
		f, err := e.Fleet(c, scope)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// No memo: every read evaluates, as before.
	read(ctx, store.ScopeAll())
	read(ctx, store.ScopeAll())
	if evals.Load() != 2 {
		t.Fatalf("without a memo: %d evaluations for 2 reads", evals.Load())
	}
	// A memo: one evaluation however many reads, and the reads are the same view.
	evals.Store(0)
	mctx := WithMemo(ctx)
	a, b, c := read(mctx, store.ScopeAll()), read(mctx, store.ScopeAll()), read(WithMemo(mctx), store.ScopeAll())
	if evals.Load() != 1 {
		t.Fatalf("with a memo: %d evaluations for 3 reads", evals.Load())
	}
	if !a.Computed.Equal(b.Computed) || len(a.View.Devices) != len(c.View.Devices) || len(a.View.Devices) == 0 {
		t.Fatal("the reads of one request disagree")
	}
	// Another scope is another evaluation; the same customers in another order are the same scope.
	cs, _ := st.ListCustomers(ctx, store.ScopeAll())
	if len(cs) < 2 {
		t.Fatal("fixtures have fewer than two customers")
	}
	read(mctx, store.ScopeOf(cs[0].ID, cs[1].ID))
	read(mctx, store.ScopeOf(cs[1].ID, cs[0].ID))
	read(mctx, store.ScopeOf(cs[0].ID))
	if evals.Load() != 3 {
		t.Fatalf("scopes: %d evaluations, want 3 (all, two customers, one customer)", evals.Load())
	}
	// A new request starts empty.
	read(WithMemo(ctx), store.ScopeAll())
	if evals.Load() != 4 {
		t.Fatalf("a new request did not evaluate: %d", evals.Load())
	}
}
