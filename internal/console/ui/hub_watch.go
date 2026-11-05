package ui

import "github.com/c2tech-sys/cryobackup/internal/console/hubsig"

// Watch lets the admin API's event stream (adminapi.EventSource, M3-T3; the Signal type lives in internal/console/hubsig so ui need not import adminapi for it) listen
// to the hub's topics without the hub rendering anything for it: every
// DeviceChanged, RunProgress and refresh tick is passed on as a Signal. The
// channel is buffered and a reader that falls behind loses signals (the stream
// re-evaluates the whole fleet on the next one, so a lost signal costs a delay,
// never a wrong answer); a slow reader never blocks the hub. The returned
// function stops watching.
func (h *Hub) Watch() (<-chan hubsig.Signal, func()) {
	ch := make(chan hubsig.Signal, 32)
	h.mu.Lock()
	if h.watchers == nil {
		h.watchers = map[chan hubsig.Signal]struct{}{}
	}
	h.watchers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.watchers, ch)
		h.mu.Unlock()
	}
}

// Generation counts the hub's signals: it grows by one before every signal is
// passed to the watchers, including a signal a slow watcher loses. The admin
// event stream keys its shared fleet evaluation to it, so an evaluation made
// before a change is never served after it.
func (h *Hub) Generation() uint64 { return h.gen.Load() }

// notify passes a signal to every watcher, without blocking. The generation is
// bumped first, so a watcher that reads Generation after receiving sg sees a
// value that already counts it.
func (h *Hub) notify(sg hubsig.Signal) {
	h.gen.Add(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.watchers {
		select {
		case ch <- sg:
		default:
		}
	}
}
