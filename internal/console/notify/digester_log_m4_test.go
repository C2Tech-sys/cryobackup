package notify

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P20-W3B: the log line of a digest send-now is readable. logx keeps free text only when every
// token is a plain word, and "send-now" is not one, so the whole message used to go out as an
// opaque "<path:...>" marker: an operator reading the log could not tell what had happened.
func TestDigestSendNowLogLineIsReadable(t *testing.T) {
	w, g := digestWorld(t, "07:00")
	var buf bytes.Buffer
	g.Log = logx.New(&buf, logx.LevelInfo)
	w.digestRecipient("a@example.test")
	w.at("2026-11-18 05:00")
	if _, err := g.SendDigestNow(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	// The second call finds today's row and returns before sending: the other log line.
	if _, err := g.SendDigestNow(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "<path:") {
		t.Fatalf("a digest log line was redacted as a path: %s", out)
	}
	if n := strings.Count(out, `"msg":"digest send now"`); n != 2 {
		t.Fatalf("want two readable send-now lines, got %d: %s", n, out)
	}
}
