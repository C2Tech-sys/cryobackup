package health

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// A request that reads the fleet more than once (a page that also asks the
// admin API for its rows) must read one evaluation, not one per read: at a few
// thousand devices an evaluation is seconds, and two of them can disagree. A
// context made by WithMemo carries a Memo; Evaluator.Fleet then evaluates once
// per (evaluator, scope) under that context and hands every later read the same
// Fleet. Without a Memo in the context nothing changes: every call evaluates.
//
// The Fleet that comes back is shared by the readers of the request, so they
// must treat it as read-only (the handlers copy before they sort or filter).

type memoKey struct{}

type memoSlot struct {
	ev    *Evaluator
	scope string
}

type memoResult struct {
	once sync.Once
	f    Fleet
	err  error
}

// Memo is the per-request evaluation cache of WithMemo.
type Memo struct {
	mu    sync.Mutex
	slots map[memoSlot]*memoResult
}

// WithMemo returns ctx carrying a fresh Memo (ctx itself when it has one).
func WithMemo(ctx context.Context) context.Context {
	if ctx.Value(memoKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, memoKey{}, &Memo{slots: map[memoSlot]*memoResult{}})
}

// scopeKey identifies a scope: the same customers in any order are one scope.
func scopeKey(s store.Scope) string {
	if s.All {
		return "*"
	}
	ids := append([]string(nil), s.CustomerIDs...)
	sort.Strings(ids)
	return "=" + strings.Join(ids, "\x1f")
}

// memoized runs compute once per (e, scope) under ctx's Memo, or at once without one.
func (e *Evaluator) memoized(ctx context.Context, scope store.Scope, compute func() (Fleet, error)) (Fleet, error) {
	m, _ := ctx.Value(memoKey{}).(*Memo)
	if m == nil {
		return compute()
	}
	slot := memoSlot{ev: e, scope: scopeKey(scope)}
	m.mu.Lock()
	r := m.slots[slot]
	if r == nil {
		r = &memoResult{}
		m.slots[slot] = r
	}
	m.mu.Unlock()
	r.once.Do(func() { r.f, r.err = compute() })
	return r.f, r.err
}
