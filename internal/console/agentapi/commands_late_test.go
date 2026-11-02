package agentapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/commands"
	"github.com/c2tech-sys/cryobackup/internal/api"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// pass is one pass of the deployed agent's commandsLoop (internal/agent/loops.go):
// first re-post every unposted result, and give up the pass on the first failure
// (the real loop then backs off and never reaches the poll); then long poll; then
// execute each command and post its result, leaving it unposted when the post fails.
func (a *fakeAgent) pass(wait time.Duration) error {
	ctx := context.Background()
	rows, err := a.jr.UnpostedCommands(ctx)
	if err != nil {
		a.t.Fatal(err)
	}
	for _, row := range rows {
		if err := a.up.CommandResult(ctx, row.ID, commands.ResultFromRow(row)); err != nil {
			return err
		}
		_ = a.jr.MarkCommandPosted(ctx, row.ID)
	}
	resp, err := a.up.PollCommands(ctx, wait)
	if err != nil {
		return err
	}
	for _, c := range resp.Commands {
		res, err := a.exec.Execute(ctx, c)
		if err != nil {
			a.t.Fatal(err)
		}
		if err := a.up.CommandResult(ctx, c.ID, res); err != nil {
			continue
		}
		_ = a.jr.MarkCommandPosted(ctx, c.ID)
	}
	return nil
}

// pollRaw polls and returns how many commands it was handed, executing none.
func (a *fakeAgent) pollRaw(wait time.Duration) (int, error) {
	resp, err := a.up.PollCommands(context.Background(), wait)
	return len(resp.Commands), err
}

// M2-W2C finding 1: the agent in production keeps a result unposted until it gets
// a 2xx and does not poll while one is unposted. A result that reaches the
// console after the command expired must therefore be answered 2xx, or the agent
// never takes another command.
func TestLateResultAfterExpiryDoesNotWedgeTheAgent(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	agent := h.newFakeAgent(string(e.DeviceToken))

	first := h.queue(e.DeviceID, "backup_now", "late-1", time.Minute, "")
	agent.cutPost.Store(true) // the connection drops after the agent started the backup
	if err := agent.pass(0); err != nil {
		t.Fatal(err)
	}
	if b, _ := agent.counts(); b != 1 {
		t.Fatalf("the agent started %d backups", b)
	}
	if rows, _ := agent.jr.UnpostedCommands(context.Background()); len(rows) != 1 {
		t.Fatalf("the result should be waiting to be re-posted (%d)", len(rows))
	}
	// The deadline passes and the 60 s job records the expiry; the console has never heard back.
	h.clk.Advance(2 * time.Minute)
	if n, err := h.st.ExpireCommands(context.Background()); err != nil || n != 1 {
		t.Fatalf("ExpireCommands = %d, %v", n, err)
	}
	if got := h.command(e.DeviceID, first.Command.ID); got.Status != "expired" {
		t.Fatalf("status %s", got.Status)
	}
	// The network is back and the operator asks again.
	agent.cutPost.Store(false)
	second := h.queue(e.DeviceID, "backup_now", "late-2", 0, "")
	if err := agent.pass(0); err != nil {
		t.Fatalf("the late result was refused and the agent's loop is stuck: %v", err)
	}
	if b, _ := agent.counts(); b != 2 {
		t.Fatalf("the new Back up now did not run after the late result (%d backups)", b)
	}
	if rows, _ := agent.jr.UnpostedCommands(context.Background()); len(rows) != 0 {
		t.Fatalf("%d results still unposted", len(rows))
	}
	// The expired command keeps its status; the late outcome is recorded for the panel.
	got := h.command(e.DeviceID, first.Command.ID)
	if got.Status != "expired" || !strings.Contains(string(got.Result), `"late_status":"succeeded"`) || got.CompletedAt == nil {
		t.Fatalf("late result: status %s result %s", got.Status, got.Result)
	}
	if st := h.command(e.DeviceID, second.Command.ID).Status; st != "succeeded" {
		t.Fatalf("the new command is %s", st)
	}
	// A repeat of the late post changes nothing and is still 2xx.
	if err := agent.up.CommandResult(context.Background(), first.Command.ID, api.CommandResultRequest{Status: api.CommandSucceeded}); err != nil {
		t.Fatalf("repeat of a late result: %v", err)
	}
}

// Every result the console cannot apply is answered 2xx, so no result can wedge the agent.
func TestEveryUnappliableResultIsAnswered2xx(t *testing.T) {
	h := newHarness(t)
	e := h.enrolled()
	agent := h.newFakeAgent(string(e.DeviceToken))
	ctx := context.Background()
	// An id the console does not know.
	if err := agent.up.CommandResult(ctx, h.mustID(), api.CommandResultRequest{Status: api.CommandSucceeded}); err != nil {
		t.Fatalf("unknown id: %v", err)
	}
	// A command that expired before it was ever delivered.
	res := h.queue(e.DeviceID, "check_in", "nd-1", time.Minute, "")
	h.clk.Advance(2 * time.Minute)
	if _, err := h.st.ExpireCommands(ctx); err != nil {
		t.Fatal(err)
	}
	if err := agent.up.CommandResult(ctx, res.Command.ID, api.CommandResultRequest{Status: api.CommandSucceeded}); err != nil {
		t.Fatalf("expired before delivery: %v", err)
	}
	if got := h.command(e.DeviceID, res.Command.ID); got.Status != "expired" || len(got.Result) != 0 {
		t.Fatalf("an undelivered expired command took a result: %s %s", got.Status, got.Result)
	}
	// A late "acked" is not an outcome.
	del := h.queue(e.DeviceID, "backup_now", "nd-2", time.Minute, "")
	if n, err := agent.pollRaw(0); err != nil || n != 1 {
		t.Fatalf("delivery: %d %v", n, err)
	}
	h.clk.Advance(2 * time.Minute)
	_, _ = h.st.ExpireCommands(ctx)
	if err := agent.up.CommandResult(ctx, del.Command.ID, api.CommandResultRequest{Status: api.CommandAcked}); err != nil {
		t.Fatalf("late ack: %v", err)
	}
	if got := h.command(e.DeviceID, del.Command.ID); len(got.Result) != 0 {
		t.Fatalf("a late ack recorded an outcome: %s", got.Result)
	}
}
