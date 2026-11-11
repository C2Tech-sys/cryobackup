package uplink

import (
	"errors"
	"math/rand/v2"
	"time"
)

// Backoff bounds (architecture §6.1 "uplink", as changed by ADR 0018 decision
// 2): one minute doubling to five, reset on success.
//
// The cap was thirty minutes; after a console outage that spread the fleet's
// return over half an hour (lab finding L9). Five minutes bounds the last
// agent's return, and a 10 000-agent fleet spread over those minutes is about
// 35 heartbeats a second at the worst instant. A console that wants a real
// herd slowed down uses Retry-After, which Delay still honours exactly (up to
// an hour, whatever the cap): the cap bounds the agent's own guess, never the
// console's instruction.
const (
	BackoffMin = time.Minute
	BackoffMax = 5 * time.Minute
)

// Backoff is a bounded exponential backoff with jitter. Each loop of the agent
// (heartbeat, outbox, command poll, policy pull) owns one. It is not safe for
// concurrent use.
type Backoff struct {
	Min, Max time.Duration
	// Jitter is the fraction of the delay that is randomised, in [0, 1];
	// 0.2 spreads a delay d over [0.8d, 1.2d] (never above Max).
	Jitter float64
	// Rand returns a number in [0, 1); nil means math/rand/v2.
	Rand func() float64

	failures int
}

// NewBackoff returns the standard 1 min to 5 min backoff with 20 % jitter.
func NewBackoff() *Backoff {
	return &Backoff{Min: BackoffMin, Max: BackoffMax, Jitter: 0.2}
}

// Next records a failure and returns how long to wait before the next try.
func (b *Backoff) Next() time.Duration {
	d := b.Min
	for i := 0; i < b.failures && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	b.failures++
	if b.Jitter > 0 {
		r := rand.Float64
		if b.Rand != nil {
			r = b.Rand
		}
		d = time.Duration(float64(d) * (1 - b.Jitter + 2*b.Jitter*r()))
		if d > b.Max {
			d = b.Max
		}
	}
	return d
}

// DelayFor is the pure form of Next for a given number of prior failures; it
// does not touch the failure counter (the outbox keeps its own per-row
// attempt counts).
func (b *Backoff) DelayFor(failures int) time.Duration {
	c := *b
	c.failures = failures
	return c.Next()
}

// Reset is called after a success.
func (b *Backoff) Reset() { b.failures = 0 }

// Failures is the number of consecutive failures recorded.
func (b *Backoff) Failures() int { return b.failures }

// Delay returns how long to wait after err: the server's Retry-After when it
// sent one (honoured exactly, up to an hour), otherwise the backoff's next
// step. The backoff is advanced either way, so a run of 429s that carry no
// header still backs off.
func (b *Backoff) Delay(err error) time.Duration {
	next := b.Next()
	var ue *Error
	if errors.As(err, &ue) && ue.RetryAfter > 0 {
		return ue.RetryAfter
	}
	return next
}
