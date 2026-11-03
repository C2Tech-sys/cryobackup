package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
)

// Enrollment retries (M2 lab finding F4).
//
// An RMM that pushes the MSI to a whole site starts every enrollment in the
// same second. The console admits one enrollment in flight per token and
// answers the others 429 with a Retry-After; an agent that takes the first
// answer then failed most of the site. Enroll therefore retries the answers
// that mean "try again later", for a bounded time:
//
//   - 429 (one in flight per token, or a rate limit), 503 (repository
//     initialisation failed, or the master key is unavailable: both carry
//     Retry-After), 502 and 504 (a proxy in front of the console), and a
//     transport failure or timeout (the response may have been lost after the
//     console stored the enrollment);
//   - never 400, 401, 403, 404, 409, 410, 422 or any other refusal a retry
//     cannot change, and never a TLS or pin failure (a retry gets the same
//     certificate);
//   - the wait is the server's Retry-After (capped at enrollRetryAfterCap) plus
//     up to half again as jitter, so a site's retries spread out instead of
//     arriving together; without Retry-After it is the uplink's bounded
//     exponential backoff (one second doubling to thirty, 20 % jitter);
//   - the total time is bounded by EnrollRetryBudget, measured on the device
//     clock: a retry that would pass it is not started, and the last error is
//     returned (wrapped, so its class survives);
//   - every attempt sends the same request, nonce included (it is on disk
//     before the first), so a retry can never create a second device: after a
//     lost response the console recognises the nonce and answers with the
//     device it already made;
//   - each retry is written to EnrollOptions.Log, path-free and token-free: the
//     uplink error's fixed vocabulary (status and wire code) and the wait.
//
// The classification lives here, not in the uplink: uplink.Refused answers a
// different question (a command-result post that must stop being retried), and
// its answer for 5xx differs from this one.

const (
	// EnrollRetryBudget bounds the time Enroll spends retrying.
	EnrollRetryBudget = 3 * time.Minute
	// EnrollTimeout is the context deadline callers give Enroll: the retry
	// budget plus room for the last attempt (the uplink bounds one call at 60 s)
	// and the local work around it.
	EnrollTimeout = EnrollRetryBudget + 2*time.Minute

	enrollWaitMin       = time.Second
	enrollWaitMax       = 30 * time.Second
	enrollRetryAfterCap = 30 * time.Second
	// enrollRetryAfterJitter is the fraction of Retry-After added at random.
	enrollRetryAfterJitter = 0.5
)

// enrollRetryClass reports whether err means "try again later". TLS failures,
// context errors and every refusal are final.
func enrollRetryClass(err error) bool {
	var ue *uplink.Error
	if !errors.As(err, &ue) || ue.TLS {
		return false
	}
	if ue.Network {
		return true
	}
	switch ue.Status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// enrollRetrier runs one enrollment call with retries. Its fields are the
// test seams: the clock and sleep are the agent's, so a fake clock makes the
// deadline deterministic and instant.
type enrollRetrier struct {
	budget time.Duration // 0: EnrollRetryBudget; negative: no retries
	clock  func() time.Time
	sleep  func(context.Context, time.Duration) error // nil: a timer
	rand   func() float64                             // nil: math/rand/v2
	log    io.Writer                                  // nil: silent
}

func (r enrollRetrier) logf(format string, a ...any) {
	if r.log != nil {
		_, _ = fmt.Fprintf(r.log, format+"\n", a...)
	}
}

func (r enrollRetrier) call(ctx context.Context, attempt func(context.Context) (api.EnrollResponse, error)) (api.EnrollResponse, error) {
	budget := r.budget
	if budget == 0 {
		budget = EnrollRetryBudget
	}
	rnd := r.rand
	if rnd == nil {
		rnd = rand.Float64
	}
	sleep := r.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	bo := &uplink.Backoff{Min: enrollWaitMin, Max: enrollWaitMax, Jitter: 0.2, Rand: rnd}
	start := r.clock()
	for n := 1; ; n++ {
		resp, err := attempt(ctx)
		if err == nil {
			if n > 1 {
				r.logf("enroll: attempt %d succeeded", n)
			}
			return resp, nil
		}
		if !enrollRetryClass(err) || budget < 0 || ctx.Err() != nil {
			return resp, err
		}
		wait := bo.Next()
		var ue *uplink.Error
		if errors.As(err, &ue) && ue.RetryAfter > 0 {
			ra := min(ue.RetryAfter, enrollRetryAfterCap)
			wait = ra + time.Duration(float64(ra)*enrollRetryAfterJitter*rnd())
		}
		elapsed := r.clock().Sub(start)
		if elapsed+wait > budget {
			r.logf("enroll: attempt %d failed (%s); not retrying: the %s retry budget is spent", n, err, budget)
			return resp, fmt.Errorf("%w (gave up after %d attempts over %s)", err, n, elapsed.Round(time.Second))
		}
		r.logf("enroll: attempt %d failed (%s); retrying in %s", n, err, wait.Round(100*time.Millisecond))
		if serr := sleep(ctx, wait); serr != nil {
			return resp, fmt.Errorf("%w (retry stopped after %d attempts: %v)", err, n, serr)
		}
	}
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
