package uplink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

func TestStartLease(t *testing.T) {
	var mu sync.Mutex
	var bodies []api.StartLeaseRequest
	answer := atomic.Int32{}
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != api.RouteStartLease || r.Header.Get(api.HeaderAuthorization) != api.BearerPrefix+devToken {
			http.Error(w, "wrong request", 400)
			return
		}
		var req api.StartLeaseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		bodies = append(bodies, req)
		mu.Unlock()
		switch answer.Load() {
		case 0:
			until := now.Add(api.StartLeaseTTL)
			writeJSON(w, 200, api.StartLeaseResponse{Granted: true, LeaseUntil: &until, ServerTime: now})
		case 1:
			writeJSON(w, 200, api.StartLeaseResponse{Granted: false, RetryAfterS: 420, ServerTime: now})
		case 2:
			problem(w, api.CodeRateLimited, "30")
		case 3:
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		}
	}))
	defer srv.Close()
	defer close(hang)
	c := clientFor(t, srv, nil)
	ctx := context.Background()

	got, err := c.StartLease(ctx, api.StartLeaseRequest{RunID: deviceULID})
	if err != nil || !got.Granted || got.LeaseUntil == nil || !got.LeaseUntil.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("granted: %+v %v", got, err)
	}
	answer.Store(1)
	if got, err = c.StartLease(ctx, api.StartLeaseRequest{RunID: deviceULID}); err != nil || got.Granted || got.RetryAfterS != 420 {
		t.Fatalf("refused: %+v %v", got, err)
	}
	answer.Store(2)
	_, err = c.StartLease(ctx, api.StartLeaseRequest{RunID: deviceULID})
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != 429 || ue.RetryAfter != 30*time.Second || strings.Contains(err.Error(), "PHI-sentinel") {
		t.Fatalf("429: %v", err)
	}
	// A hung console: the caller's deadline ends the call as a network error
	// (without one, api.StartLeaseTimeout does).
	answer.Store(3)
	short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	startAt := time.Now()
	_, err = c.StartLease(short, api.StartLeaseRequest{RunID: deviceULID})
	if !errors.As(err, &ue) || !ue.Network || time.Since(startAt) > 5*time.Second {
		t.Fatalf("hung console: %v after %v", err, time.Since(startAt))
	}
	// A body that is not path-free never leaves.
	if _, err := c.StartLease(ctx, api.StartLeaseRequest{RunID: `C:\Users\x`}); !errors.Is(err, ErrPathLeak) {
		t.Fatalf("path in the body: %v", err)
	}
	// No token, no request.
	nc := clientFor(t, srv, func(o *Options) { o.Token = func() string { return "" } })
	if _, err := nc.StartLease(ctx, api.StartLeaseRequest{RunID: deviceULID}); !errors.Is(err, ErrNoToken) {
		t.Fatalf("no token: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 4 || bodies[0].RunID != deviceULID {
		t.Fatalf("bodies = %+v", bodies)
	}
}
