package uplink

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// M1-FIX3 item 6: Options.StartLeaseTimeout is StartLease's own bound (zero
// means api.StartLeaseTimeout), so a test can make it longer than its own
// deadlines and let only the caller's context end a hung call.
func TestStartLeaseTimeoutOption(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hang)

	if c := clientFor(t, srv, nil); c.leaseTO != api.StartLeaseTimeout {
		t.Fatalf("default bound = %v, want api.StartLeaseTimeout", c.leaseTO)
	}

	short := clientFor(t, srv, func(o *Options) { o.StartLeaseTimeout = 100 * time.Millisecond })
	began := time.Now()
	_, err := short.StartLease(context.Background(), api.StartLeaseRequest{RunID: deviceULID})
	var ue *Error
	if !errors.As(err, &ue) || !ue.Network || time.Since(began) > 5*time.Second {
		t.Fatalf("a 100 ms bound: %v after %v", err, time.Since(began))
	}

	// A bound longer than the caller's deadline: the caller's context ends it.
	long := clientFor(t, srv, func(o *Options) { o.StartLeaseTimeout = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	began = time.Now()
	if _, err := long.StartLease(ctx, api.StartLeaseRequest{RunID: deviceULID}); err == nil || time.Since(began) > 5*time.Second {
		t.Fatalf("a cancelled caller: %v after %v", err, time.Since(began))
	}
}
