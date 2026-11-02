package journal

import (
	"context"
	"path/filepath"
	"testing"
)

// A refused command result leaves the unposted list for good and is told apart
// from an acknowledged one (M2-W3C).
func TestMarkCommandRefused(t *testing.T) {
	ctx := context.Background()
	j, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "j.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })

	for _, id := range []string{"a", "b", "c"} {
		if _, _, err := j.RecordCommand(ctx, id, "check_in", nil); err != nil {
			t.Fatal(err)
		}
		if err := j.FinishCommand(ctx, id, "succeeded", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.MarkCommandRefused(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := j.MarkCommandPosted(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	un, err := j.UnpostedCommands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 1 || un[0].ID != "c" {
		t.Fatalf("unposted = %+v, want only c", un)
	}
	for id, want := range map[string][2]bool{"a": {false, true}, "b": {true, false}, "c": {false, false}} {
		row, ok, err := j.Command(ctx, id)
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", id, ok, err)
		}
		if row.Posted != want[0] || row.Refused != want[1] {
			t.Errorf("%s: posted=%v refused=%v, want %v", id, row.Posted, row.Refused, want)
		}
	}
}
