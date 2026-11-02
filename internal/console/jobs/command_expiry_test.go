package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func TestCommandExpiryJobExpiresDueCommandsUnderTheLease(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l, err := e.st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{Search: "DR-OFFICE-PC"})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("fixture device: %v", err)
	}
	dev := l.Items[0].ID
	short, _, err := e.st.EnqueueCommand(ctx, store.ScopeAll(), dev, store.CommandInput{Type: "check_in", IdempotencyKey: "short", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	long, _, err := e.st.EnqueueCommand(ctx, store.ScopeAll(), dev, store.CommandInput{Type: "check_in", IdempotencyKey: "long", TTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	status := func(id string) string {
		cmds, err := e.st.ListCommands(ctx, store.ScopeAll(), dev, store.Page{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cmds.Items {
			if c.ID == id {
				return c.Status
			}
		}
		t.Fatalf("command %s not listed", id)
		return ""
	}

	job := (&CommandExpiry{Store: e.st}).Job()
	if job.Name != NameCommandExpiry || NameCommandExpiry != "command-expiry" || job.Every != time.Minute { // the architecture jobs table: command-expiry every 60 s
		t.Fatalf("job = %+v", job)
	}
	r := NewRunner(e.st, "holder-a", nil)
	r.Add(job)
	// Nothing is due yet.
	if ran, err := r.RunOnce(ctx, NameCommandExpiry); !ran || err != nil {
		t.Fatalf("first run: %v %v", ran, err)
	}
	if status(short.ID) != "queued" || status(long.ID) != "queued" {
		t.Fatal("a command expired before its time")
	}
	// Past the short TTL: the short command expires, the long one does not.
	e.now = e.now.Add(2 * time.Minute)
	if got, _ := e.st.AcquireJobLease(ctx, NameCommandExpiry, "holder-b", time.Minute); !got {
		t.Fatal("holder-b lease")
	}
	if ran, err := r.RunOnce(ctx, NameCommandExpiry); ran || err != nil || status(short.ID) != "queued" {
		t.Fatalf("a runner without the lease must not expire anything: ran=%v err=%v status=%s", ran, err, status(short.ID))
	}
	e.now = e.now.Add(2 * time.Minute) // holder-b's lease lapses
	if ran, err := r.RunOnce(ctx, NameCommandExpiry); !ran || err != nil {
		t.Fatalf("run with the lease: %v %v", ran, err)
	}
	if status(short.ID) != "expired" || status(long.ID) != "queued" {
		t.Fatalf("short=%s long=%s, want expired and queued", status(short.ID), status(long.ID))
	}
	st, err := e.st.GetJobState(ctx, NameCommandExpiry)
	if err != nil || st.LastStatus != "ok" || st.RunCount != 2 {
		t.Fatalf("job_state: %+v %v", st, err)
	}
}
