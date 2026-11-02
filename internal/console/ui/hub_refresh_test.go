package ui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// A refresh interval saved in the settings reaches the running stream: on the
// next ping the hub restarts its refresh ticker at the new period.
func TestHubFollowsAChangedRefreshInterval(t *testing.T) {
	ft := &fakeTicks{ch: map[time.Duration]chan time.Time{}, ready: make(chan struct{})}
	h := NewHub(30*time.Second, nil)
	h.Tick = ft.tick
	var secs atomic.Int32
	secs.Store(30)
	h.RefreshNow = func() time.Duration { return time.Duration(secs.Load()) * time.Second }
	h.render = func(ctx context.Context, sub *Sub) ([]Event, error) {
		return []Event{{Name: "fleet.state", Data: "<div id=\"board\">x</div>"}, {Name: "freshness", Data: "{}"}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	defer func() { cancel(); <-done }()
	<-ft.ready

	a, cancelA := h.Subscribe(&Sub{Scope: store.ScopeAll(), Role: auth.RoleReadOnly})
	defer cancelA()

	// Unchanged interval: the ping does not restart anything.
	ft.send(t, 15*time.Second)
	recv(t, a, "ping")
	ft.mu.Lock()
	n := len(ft.ch)
	ft.mu.Unlock()
	if n != 3 {
		t.Fatalf("tickers after an unchanged ping: %d, want 3", n)
	}

	// The owner saves 90 s; the next ping restarts the refresh ticker.
	secs.Store(90)
	ft.send(t, 15*time.Second)
	recv(t, a, "ping")
	deadline := time.Now().Add(3 * time.Second)
	for {
		ft.mu.Lock()
		_, ok := ft.ch[90*time.Second]
		ft.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no ticker at the new interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ft.send(t, 90*time.Second)
	recv(t, a, "fleet.state")
	recv(t, a, "freshness")
}
