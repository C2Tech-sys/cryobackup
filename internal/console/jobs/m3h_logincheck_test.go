package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-HARDEN: a Check that meets a run in flight reads nothing, and carries the newest clean
// read's list and its time, so the caller can answer from it. A failed read carries them too, and
// never moves the time: only a clean read can.
func TestLoginFileCheckCarriesTheNewestCleanRead(t *testing.T) {
	clean := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenMemory(context.Background(), func() time.Time { return clean })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	j := &HtpasswdSync{Store: st}
	j.Dir = t.TempDir() // no file yet: a failed read, and no clean read to carry
	if c := j.Check(context.Background()); c.ReadClass != LoginFileMissing || !c.CleanAt.IsZero() {
		t.Fatalf("no clean read yet: %+v", c)
	}
	j.last.Store(&LoginFileCheck{At: clean, CleanAt: clean, InFile: []string{"device-1"}})

	if c := j.Check(context.Background()); c.ReadClass != LoginFileMissing || !c.CleanAt.Equal(clean) || !c.Has("device-1") {
		t.Fatalf("a failed read after a clean one: %+v", c)
	}

	if err := j.take(context.Background()); err != nil { // a run holds the slot
		t.Fatal(err)
	}
	defer j.give()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c := j.Check(ctx)
	if c.ReadClass != LoginFileBusy || !c.CleanAt.Equal(clean) || !c.Has("device-1") {
		t.Fatalf("busy: %+v", c)
	}

	for _, tc := range []struct {
		name  string
		since time.Time
		want  bool
	}{
		{"retired before the read", clean.Add(-time.Second), true},
		{"retired in the same millisecond", clean, false}, // the store's resolution cannot order them
		{"retired after the read", clean.Add(time.Second), false},
	} {
		if got := c.ReadSince(tc.since); got != tc.want {
			t.Errorf("%s: ReadSince = %v", tc.name, got)
		}
	}
	if (LoginFileCheck{}).ReadSince(time.Time{}) {
		t.Error("no clean read at all counts as one")
	}
}
