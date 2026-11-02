// Package ratelimit holds the in-memory limiters the console listeners use
// (architecture §5.5): token buckets keyed by device id or source IP, a blocker
// for repeated failed authentications with exponential back-off, and a
// one-in-flight guard. All of them take an injected clock, and none logs or
// stores anything outside the process.
package ratelimit

import (
	"sync"
	"time"
)

const (
	pruneEvery = 1024
	idleAfter  = 15 * time.Minute
)

type bucketEntry struct {
	tokens float64
	last   time.Time
}

// Bucket is a keyed token bucket.
type Bucket struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	now     func() time.Time
	entries map[string]*bucketEntry
	ops     int
}

// NewBucket allows n events per period per key with the given burst (at least
// 1). A nil clock means time.Now.
func NewBucket(n int, period time.Duration, burst int, now func() time.Time) *Bucket {
	if now == nil {
		now = time.Now
	}
	if burst < 1 {
		burst = 1
	}
	return &Bucket{rate: float64(n) / period.Seconds(), burst: float64(burst), now: now, entries: map[string]*bucketEntry{}}
}

// Allow takes one token for key. When none is left it returns false and how
// long until one is available (the Retry-After value).
func (b *Bucket) Allow(key string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.maybePrune(now)
	e, ok := b.entries[key]
	if !ok {
		b.entries[key] = &bucketEntry{tokens: b.burst - 1, last: now}
		return true, 0
	}
	if dt := now.Sub(e.last).Seconds(); dt > 0 {
		e.tokens += dt * b.rate
		if e.tokens > b.burst {
			e.tokens = b.burst
		}
		e.last = now
	}
	if e.tokens >= 1 {
		e.tokens--
		return true, 0
	}
	wait := time.Duration((1 - e.tokens) / b.rate * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

func (b *Bucket) maybePrune(now time.Time) {
	b.ops++
	if b.ops%pruneEvery != 0 {
		return
	}
	for k, e := range b.entries {
		if now.Sub(e.last) > idleAfter {
			delete(b.entries, k)
		}
	}
}

// Len returns the number of tracked keys (tests).
func (b *Bucket) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

type blockEntry struct {
	fails      []time.Time
	blockedTil time.Time
	level      int
	last       time.Time
}

// Blocker counts failures per key and, past a threshold inside a window, blocks
// the key for a time that doubles with every repeat up to a ceiling.
type Blocker struct {
	mu        sync.Mutex
	threshold int
	window    time.Duration
	first     time.Duration
	max       time.Duration
	now       func() time.Time
	entries   map[string]*blockEntry
	ops       int
}

// NewBlocker blocks a key that fails more than threshold times within window,
// first for one minute, then twice as long each time, never beyond max.
func NewBlocker(threshold int, window, max time.Duration, now func() time.Time) *Blocker {
	if now == nil {
		now = time.Now
	}
	return &Blocker{threshold: threshold, window: window, first: time.Minute, max: max, now: now, entries: map[string]*blockEntry{}}
}

// Blocked reports whether key is blocked now and for how long still.
func (b *Blocker) Blocked(key string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[key]
	if !ok {
		return false, 0
	}
	if now := b.now(); now.Before(e.blockedTil) {
		return true, e.blockedTil.Sub(now)
	}
	return false, 0
}

// Fail records a failure and reports whether this failure started a block (one
// audit row per block, not per request).
func (b *Blocker) Fail(key string) (justBlocked bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.ops++
	if b.ops%pruneEvery == 0 {
		for k, e := range b.entries {
			if now.Sub(e.last) > b.max+b.window && now.After(e.blockedTil) {
				delete(b.entries, k)
			}
		}
	}
	e, ok := b.entries[key]
	if !ok {
		e = &blockEntry{}
		b.entries[key] = e
	}
	e.last = now
	if now.Before(e.blockedTil) {
		return false
	}
	kept := e.fails[:0]
	for _, t := range e.fails {
		if now.Sub(t) < b.window {
			kept = append(kept, t)
		}
	}
	e.fails = append(kept, now)
	if len(e.fails) <= b.threshold {
		return false
	}
	d := b.first << e.level
	if d > b.max || d <= 0 {
		d = b.max
	}
	e.level++
	e.blockedTil = now.Add(d)
	e.fails = e.fails[:0]
	return true
}

// Reset forgets key (after a successful authentication).
func (b *Blocker) Reset(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, key)
}

// Flight allows at most one holder per key.
type Flight struct {
	mu   sync.Mutex
	held map[string]struct{}
}

// NewFlight returns an empty guard.
func NewFlight() *Flight { return &Flight{held: map[string]struct{}{}} }

// TryAcquire takes key; it returns the release function and true, or false when
// the key is already held.
func (f *Flight) TryAcquire(key string) (release func(), ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.held[key]; taken {
		return nil, false
	}
	f.held[key] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.held, key)
			f.mu.Unlock()
		})
	}, true
}
