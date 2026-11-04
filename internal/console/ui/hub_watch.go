package ui

import "github.com/c2tech-sys/cryobackup/internal/console/adminapi"

// Watch lets the admin API's event stream (adminapi.EventSource, M3-T3) listen
// to the hub's topics without the hub rendering anything for it: every
// DeviceChanged, RunProgress and refresh tick is passed on as a Signal. The
// channel is buffered and a reader that falls behind loses signals (the stream
// re-evaluates the whole fleet on the next one, so a lost signal costs a delay,
// never a wrong answer); a slow reader never blocks the hub. The returned
// function stops watching.
func (h *Hub) Watch() (<-chan adminapi.Signal, func()) {
	ch := make(chan adminapi.Signal, 32)
	h.mu.Lock()
	if h.watchers == nil {
		h.watchers = map[chan adminapi.Signal]struct{}{}
	}
	h.watchers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.watchers, ch)
		h.mu.Unlock()
	}
}

// notify passes a signal to every watcher, without blocking.
func (h *Hub) notify(sg adminapi.Signal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.watchers {
		select {
		case ch <- sg:
		default:
		}
	}
}

var _ adminapi.EventSource = (*Hub)(nil)
