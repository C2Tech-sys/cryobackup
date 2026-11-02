package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// FIX6: opening a source right after registering it can fail for a moment
// (Access is denied on the elevated CI runner); the open is retried, bounded,
// and the last error comes back when every attempt fails.
func TestOpenWithRetry(t *testing.T) {
	denied := errors.New("Access is denied.")
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }

	calls := 0
	el, err := openWithRetry(func() (*EventLog, error) {
		calls++
		if calls < 3 {
			return nil, denied
		}
		return &EventLog{}, nil
	}, freshOpenAttempts, freshOpenWait, sleep)
	if err != nil || el == nil || calls != 3 {
		t.Fatalf("open failing twice then working: el=%v err=%v calls=%d, want a log after 3 calls", el, err, calls)
	}
	if len(slept) != 2 || slept[0] != freshOpenWait {
		t.Fatalf("slept %v, want two waits of %v", slept, freshOpenWait)
	}

	calls, slept = 0, nil
	last := errors.New("still denied")
	_, err = openWithRetry(func() (*EventLog, error) {
		calls++
		if calls == freshOpenAttempts {
			return nil, last
		}
		return nil, denied
	}, freshOpenAttempts, freshOpenWait, sleep)
	if !errors.Is(err, last) || calls != freshOpenAttempts {
		t.Fatalf("open always failing: err=%v calls=%d, want the last error after %d calls", err, calls, freshOpenAttempts)
	}
	if total := time.Duration(len(slept)) * freshOpenWait; total > 2*time.Second {
		t.Fatalf("the retry waits %v in all; it must stay short", total)
	}
}

// L-EVT-1: the ids the agent writes are exactly the ids the message table
// carries (deploy/windows/eventlog/messages.mc); cmd/cryo-agent checks that
// the compiled table in cryo-agent.exe renders each of them.
func TestEventIDsMatchTheMessageTable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "windows", "eventlog", "messages.mc"))
	if err != nil {
		t.Fatal(err)
	}
	var mc []uint32
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "MessageId="); ok {
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				t.Fatalf("messages.mc: %q is not a decimal id", line)
			}
			mc = append(mc, uint32(n))
		}
	}
	if fmt.Sprint(mc) != fmt.Sprint(EventIDs) {
		t.Fatalf("messages.mc ids %v, EventIDs %v: keep them the same (and rerun build-messages.ps1)", mc, EventIDs)
	}
}
