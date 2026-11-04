package ui

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
)

// M3-T3: the admin event stream listens to the hub through Watch.

func recvSignal(t *testing.T, ch <-chan adminapi.Signal) adminapi.Signal {
	t.Helper()
	select {
	case sg := <-ch:
		return sg
	case <-time.After(5 * time.Second):
		t.Fatal("no signal")
	}
	return adminapi.Signal{}
}

func TestWatchReceivesDeviceChangesAndRunProgress(t *testing.T) {
	h := NewHub(time.Hour, nil)
	ch, stop := h.Watch()
	h.DeviceChanged("dev-1")
	if sg := recvSignal(t, ch); sg.Kind != adminapi.SignalChange || sg.DeviceID != "dev-1" {
		t.Fatalf("change: %+v", sg)
	}
	h.RunProgress("dev-2", "run-9")
	if sg := recvSignal(t, ch); sg.Kind != adminapi.SignalProgress || sg.DeviceID != "dev-2" || sg.RunID != "run-9" {
		t.Fatalf("progress: %+v", sg)
	}
	stop()
	h.DeviceChanged("dev-3")
	select {
	case sg := <-ch:
		t.Fatalf("a stopped watcher still got %+v", sg)
	case <-time.After(50 * time.Millisecond):
	}
	if h.Subscribers() != 0 {
		t.Fatal("a watcher is not an SSE subscriber")
	}
}

func TestWatchWithASlowReaderNeverBlocksTheHubAndOthersStillHear(t *testing.T) {
	h := NewHub(time.Hour, nil)
	_, stopSlow := h.Watch() // never read
	defer stopSlow()
	fast, stopFast := h.Watch()
	defer stopFast()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			h.DeviceChanged("d")
		}
		close(done)
	}()
	var got int
	deadline := time.After(10 * time.Second)
	for got < 32 { // the fast watcher's buffer fills before it drains; it must still hear
		select {
		case <-fast:
			got++
		case <-deadline:
			t.Fatalf("heard only %d signals", got)
		}
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DeviceChanged blocked on a watcher that does not read")
	}
}

func TestWatchHearsTheRefreshTick(t *testing.T) {
	h := NewHub(123*time.Second, nil)
	var mu sync.Mutex
	tick := map[time.Duration]chan time.Time{}
	h.Tick = func(d time.Duration) (<-chan time.Time, func()) {
		mu.Lock()
		defer mu.Unlock()
		c := make(chan time.Time, 1)
		tick[d] = c
		return c, func() {}
	}
	ch, stop := h.Watch()
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		c := tick[123*time.Second]
		mu.Unlock()
		if c != nil {
			c <- time.Now()
			break
		}
		select {
		case <-deadline:
			t.Fatal("the hub never started its refresh ticker")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if sg := recvSignal(t, ch); sg.Kind != adminapi.SignalRefresh {
		t.Fatalf("refresh tick: %+v", sg)
	}
}
