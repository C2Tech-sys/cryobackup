package agentapi

import (
	"context"
	"sync"
	"time"
)

// defaultEnrollWait bounds how long an enrollment waits for another enrollment
// on the same token to finish (M2 lab finding F4). An RMM push of a site's
// machines on one multi-use token arrives in the same second; the enrollments
// are served one after another instead of refused. One enrollment through the
// admin path (repository init plus cat config) takes a few seconds, so 20 s lets
// a handful queue on one token while staying well inside the agent's 60 s
// request timeout and the listener's 60 s write timeout. Past it the request is
// refused with 429 and a Retry-After (enrollBusyRetry).
const defaultEnrollWait = 20 * time.Second

// enrollBusyRetry is the Retry-After given when the wait for the token ran out.
const enrollBusyRetry = 5 * time.Second

// tokenQueue serialises work per key: at most one holder per key, and later
// callers wait their turn (bounded, context-aware) instead of being refused.
// Keys are forgotten when nobody holds or waits for them, so the map does not
// grow with the number of tokens ever seen.
type tokenQueue struct {
	mu    sync.Mutex
	slots map[string]*queueSlot
}

type queueSlot struct {
	ch    chan struct{} // capacity 1: a value in it means "held"
	users int           // holder plus waiters; the slot is deleted at zero
}

func newTokenQueue() *tokenQueue { return &tokenQueue{slots: map[string]*queueSlot{}} }

// acquire takes key, waiting at most wait for the current holder to release it.
// It returns the release function and true, or false when the wait ran out or
// ctx ended first.
func (q *tokenQueue) acquire(ctx context.Context, key string, wait time.Duration) (release func(), ok bool) {
	q.mu.Lock()
	sl := q.slots[key]
	if sl == nil {
		sl = &queueSlot{ch: make(chan struct{}, 1)}
		q.slots[key] = sl
	}
	sl.users++
	q.mu.Unlock()

	leave := func() {
		q.mu.Lock()
		sl.users--
		if sl.users == 0 {
			delete(q.slots, key)
		}
		q.mu.Unlock()
	}
	granted := func() func() {
		var once sync.Once
		return func() {
			once.Do(func() {
				<-sl.ch
				leave()
			})
		}
	}

	select {
	case sl.ch <- struct{}{}:
		return granted(), true
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case sl.ch <- struct{}{}:
		return granted(), true
	case <-ctx.Done():
	case <-t.C:
	}
	leave()
	return nil, false
}

// held reports the number of keys currently tracked (tests).
func (q *tokenQueue) held() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.slots)
}
