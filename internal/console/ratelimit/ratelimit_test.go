package ratelimit

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestBucketHeartbeatShape(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	b := NewBucket(1, 20*time.Second, 1, c.now)
	if ok, _ := b.Allow("dev"); !ok {
		t.Fatal("first must pass")
	}
	ok, wait := b.Allow("dev")
	if ok || wait < 10*time.Second || wait > 21*time.Second {
		t.Fatalf("second too soon: %v %v", ok, wait)
	}
	if ok, _ := b.Allow("other"); !ok {
		t.Fatal("keys are independent")
	}
	c.advance(21 * time.Second)
	if ok, _ := b.Allow("dev"); !ok {
		t.Fatal("must pass after the interval")
	}
}

func TestBucketPerMinuteBurst(t *testing.T) {
	c := &clock{t: time.Now()}
	b := NewBucket(6, time.Minute, 6, c.now)
	for i := 0; i < 6; i++ {
		if ok, _ := b.Allow("d"); !ok {
			t.Fatalf("request %d must pass", i)
		}
	}
	if ok, wait := b.Allow("d"); ok || wait < time.Second {
		t.Fatalf("seventh must be limited: %v %v", ok, wait)
	}
	c.advance(10 * time.Second)
	if ok, _ := b.Allow("d"); !ok {
		t.Fatal("one token per 10 s")
	}
}

func TestBucketPrunesIdleKeys(t *testing.T) {
	c := &clock{t: time.Now()}
	b := NewBucket(1, time.Minute, 1, c.now)
	for i := 0; i < 50; i++ {
		b.Allow(string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	c.advance(time.Hour)
	for i := 0; i < pruneEvery; i++ {
		b.Allow("hot")
	}
	if b.Len() != 1 {
		t.Fatalf("idle keys not pruned: %d", b.Len())
	}
}

func TestBlockerExponentialBackoff(t *testing.T) {
	c := &clock{t: time.Now()}
	b := NewBlocker(3, time.Minute, 15*time.Minute, c.now)
	for i := 0; i < 3; i++ {
		if b.Fail("ip") {
			t.Fatal("must not block within the threshold")
		}
	}
	if !b.Fail("ip") {
		t.Fatal("fourth failure must start a block")
	}
	if blocked, d := b.Blocked("ip"); !blocked || d != time.Minute {
		t.Fatalf("first block 1 min: %v %v", blocked, d)
	}
	if b.Fail("ip") {
		t.Fatal("failures during a block do not start another (one audit row per block)")
	}
	c.advance(61 * time.Second)
	if blocked, _ := b.Blocked("ip"); blocked {
		t.Fatal("block must end")
	}
	for i := 0; i < 3; i++ {
		b.Fail("ip")
	}
	if !b.Fail("ip") {
		t.Fatal("second block")
	}
	if _, d := b.Blocked("ip"); d != 2*time.Minute {
		t.Fatalf("second block doubles: %v", d)
	}
	for lvl := 0; lvl < 6; lvl++ {
		c.advance(time.Hour)
		for i := 0; i < 4; i++ {
			b.Fail("ip")
		}
	}
	if _, d := b.Blocked("ip"); d != 15*time.Minute {
		t.Fatalf("capped at 15 min: %v", d)
	}
	b.Reset("ip")
	if blocked, _ := b.Blocked("ip"); blocked {
		t.Fatal("reset clears")
	}
}

func TestBlockerWindowForgets(t *testing.T) {
	c := &clock{t: time.Now()}
	b := NewBlocker(2, time.Minute, time.Hour, c.now)
	b.Fail("x")
	b.Fail("x")
	c.advance(2 * time.Minute)
	if b.Fail("x") {
		t.Fatal("old failures fall out of the window")
	}
}

func TestFlight(t *testing.T) {
	f := NewFlight()
	rel, ok := f.TryAcquire("k")
	if !ok {
		t.Fatal("first acquire")
	}
	if _, ok := f.TryAcquire("k"); ok {
		t.Fatal("second must fail")
	}
	rel()
	rel() // idempotent
	if _, ok := f.TryAcquire("k"); !ok {
		t.Fatal("after release")
	}
}
