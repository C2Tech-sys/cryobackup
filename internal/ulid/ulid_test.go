package ulid

import (
	"bytes"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

func frozen(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestNewIsValidAndCarriesTime(t *testing.T) {
	at := time.Date(2026, 10, 22, 13, 42, 10, 123_000_000, time.UTC)
	g := NewGenerator(frozen(at), bytes.NewReader(bytes.Repeat([]byte{0xAB}, 4096)))
	id := g.MustNew()
	if len(id) != Len || !Valid(id) {
		t.Fatalf("id %q not valid", id)
	}
	got, ok := Time(id)
	if !ok || !got.Equal(at) {
		t.Fatalf("Time(%q) = %v, %v; want %v", id, got, ok, at)
	}
}

// Known vector: timestamp 0 with all-zero entropy is 26 zeros; the largest
// timestamp with all-one entropy is "7ZZZ...Z".
func TestEncodeVectors(t *testing.T) {
	if got := encode(0, [10]byte{}); got != "00000000000000000000000000" {
		t.Fatalf("zero vector = %q", got)
	}
	var ones [10]byte
	for i := range ones {
		ones[i] = 0xff
	}
	if got := encode(maxMillis, ones); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("max vector = %q", got)
	}
	// 1 ms = ...1 in the last timestamp character.
	if got := encode(1, [10]byte{}); got != "00000000010000000000000000" {
		t.Fatalf("1ms vector = %q", got)
	}
}

func TestMonotonicWithinSameMillisecond(t *testing.T) {
	g := NewGenerator(frozen(time.UnixMilli(1_800_000_000_000)), bytes.NewReader(make([]byte, 64)))
	var ids []string
	for i := 0; i < 1000; i++ {
		id, err := g.New()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids generated in one millisecond are not sorted in generation order")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestClockGoingBackwardsStaysMonotonic(t *testing.T) {
	cur := time.UnixMilli(1_800_000_005_000)
	g := NewGenerator(func() time.Time { return cur }, nil)
	a := g.MustNew()
	cur = cur.Add(-3 * time.Second)
	b := g.MustNew()
	if !(a < b) {
		t.Fatalf("backwards clock broke order: %s then %s", a, b)
	}
}

func TestEntropyOverflowBumpsTimestamp(t *testing.T) {
	// Entropy all 0xff then fresh zeros: the increment overflows, the timestamp advances.
	src := bytes.NewReader(append(bytes.Repeat([]byte{0xff}, 10), make([]byte, 10)...))
	g := NewGenerator(frozen(time.UnixMilli(1_800_000_000_000)), src)
	a := g.MustNew()
	b := g.MustNew()
	if !(a < b) {
		t.Fatalf("overflow broke order: %s then %s", a, b)
	}
	ta, _ := Time(a)
	tb, _ := Time(b)
	if tb.Sub(ta) != time.Millisecond {
		t.Fatalf("expected the timestamp to advance by 1 ms, got %v", tb.Sub(ta))
	}
}

func TestEntropyFailureIsAnError(t *testing.T) {
	g := NewGenerator(frozen(time.UnixMilli(1_800_000_000_000)), bytes.NewReader(nil))
	if _, err := g.New(); err == nil {
		t.Fatal("expected an error when the entropy source is empty")
	}
}

func TestClockOutOfRange(t *testing.T) {
	g := NewGenerator(frozen(time.UnixMilli(-5)), nil)
	if _, err := g.New(); !errors.Is(err, ErrMillisOverflow) {
		t.Fatalf("negative clock: %v", err)
	}
}

func TestValid(t *testing.T) {
	good := "01J9Y4K3A0B1C2D3E4F5G6H7J8"
	cases := []struct {
		s    string
		want bool
	}{
		{good, true},
		{"01j9y4k3a0b1c2d3e4f5g6h7j8", true}, // lower case parses
		{"", false},
		{good[:25], false},
		{good + "0", false},
		{"81J9Y4K3A0B1C2D3E4F5G6H7J8", false}, // first char > 7 overflows 48 bits
		{"01J9Y4K3A0B1C2D3E4F5G6H7JI", false}, // I is not Crockford
		{"01J9Y4K3A0B1C2D3E4F5G6H7JU", false}, // U is not Crockford
		{"01J9Y4K3A0B1C2D3E4F5G6H7J-", false},
	}
	for _, c := range cases {
		if got := Valid(c.s); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.s, got, c.want)
		}
	}
	if _, ok := Time("not-a-ulid"); ok {
		t.Error("Time accepted garbage")
	}
}

func TestConcurrentUse(t *testing.T) {
	g := NewGenerator(frozen(time.UnixMilli(1_800_000_000_000)), nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				id := g.MustNew()
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate id %s", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 4000 {
		t.Fatalf("got %d distinct ids, want 4000", len(seen))
	}
}

func TestNilClockPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a nil clock")
		}
	}()
	NewGenerator(nil, nil)
}
