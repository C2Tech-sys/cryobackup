package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAgentAckCommandRecordsALateResultForADeliveredExpiredCommand(t *testing.T) {
	w := seed(t)
	q := must[QueueResult](t)(w.s.QueueCommandAudited(w.ctx, ScopeAll(), w.devA.ID, CommandInput{Type: "backup_now", IdempotencyKey: "l1", TTL: time.Minute}, wActor))
	if got := must[[]Command](t)(w.s.AgentNextCommands(w.ctx, w.devA.ID, 5)); len(got) != 1 {
		t.Fatalf("delivery: %d", len(got))
	}
	w.clk.Advance(2 * time.Minute)
	if n := must[int64](t)(w.s.ExpireCommands(w.ctx)); n != 1 {
		t.Fatalf("ExpireCommands = %d", n)
	}
	c, err := w.s.AgentAckCommand(w.ctx, w.devA.ID, q.Command.ID, CommandAck{Status: "succeeded", Result: []byte(`{"started":true}`)})
	if err != nil {
		t.Fatalf("a late result must be accepted: %v", err)
	}
	if c.Status != "expired" || c.CompletedAt == nil || !strings.Contains(string(c.Result), `"late_status":"succeeded"`) || !strings.Contains(string(c.Result), `"started":true`) {
		t.Fatalf("late result: %s %s", c.Status, c.Result)
	}
	// Repeating it, or sending another outcome, changes nothing.
	again := must[Command](t)(w.s.AgentAckCommand(w.ctx, w.devA.ID, q.Command.ID, CommandAck{Status: "failed"}))
	if string(again.Result) != string(c.Result) || again.Status != "expired" {
		t.Fatalf("second late result changed the record: %s", again.Result)
	}
	// Another device's late result is still a not-found.
	if _, err := w.s.AgentAckCommand(w.ctx, w.devB.ID, q.Command.ID, CommandAck{Status: "succeeded"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other device: %v", err)
	}
}

func TestAgentAckCommandStillRefusesWhatWasNeverDelivered(t *testing.T) {
	w := seed(t)
	q := must[QueueResult](t)(w.s.QueueCommandAudited(w.ctx, ScopeAll(), w.devA.ID, CommandInput{Type: "check_in", IdempotencyKey: "l2", TTL: time.Minute}, wActor))
	w.clk.Advance(2 * time.Minute)
	must[int64](t)(w.s.ExpireCommands(w.ctx))
	if _, err := w.s.AgentAckCommand(w.ctx, w.devA.ID, q.Command.ID, CommandAck{Status: "succeeded"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a result for a command that expired undelivered: %v", err)
	}
}
