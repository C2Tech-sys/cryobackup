package jobs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T6 round 3: the deployed file is the only witness that a login has left it, and
// runs of the job cannot overlap.

func loginRig(t *testing.T) (*HtpasswdSync, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	now := fixtures.Now()
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	mk, err := escrow.NewMasterKey(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	l, err := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{Search: "FRONTDESK-01"})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("fixture device: %d, %v", len(l.Items), err)
	}
	return &HtpasswdSync{Store: st, Master: mk, Dir: t.TempDir()}, st, l.Items[0].ID
}

func retireForTest(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.RetireDeviceAudited(context.Background(), store.ScopeAll(), id, store.RetireRequest{Reason: "test"}, store.AuditActor{Username: "t", Method: "api"}); err != nil {
		t.Fatal(err)
	}
}

func TestLoginFileCheckReadsTheFileItself(t *testing.T) {
	j, st, id := loginRig(t)
	ctx := context.Background()

	// No file yet: not read, and that is not "absent".
	if c := j.Check(ctx); c.ReadClass != LoginFileMissing || c.Has(id) {
		t.Fatalf("no file: %+v", c)
	}
	if f := j.Facts(); f.ReadFailed != LoginFileMissing || f.MeasuredAt == 0 {
		t.Fatalf("facts: %+v", f)
	}
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if c := j.Check(ctx); c.ReadClass != "" || len(c.InFile) != 0 {
		t.Fatalf("a clean file with no retired login in it: %+v", c)
	}

	// Retired, and nothing has synced: the check reads the file and finds the login. It changes nothing.
	path := filepath.Join(j.Dir, gateway.FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	retireForTest(t, st, id)
	c := j.Check(ctx)
	if c.ReadClass != "" || !c.Has(id) {
		t.Fatalf("the login is in the file: %+v", c)
	}
	if f := j.Facts(); f.InFile != 1 || f.ReadFailed != "" {
		t.Fatalf("facts: %+v", f)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the check changed the file")
	}
	// Usernames only.
	for _, u := range c.InFile {
		if strings.Contains(u, "$") {
			t.Errorf("a hash in the answer: %q", u)
		}
	}

	// Only a clean read clears: a file that cannot be read carries the login forward.
	if err := os.Rename(path, path+".aside"); err != nil {
		t.Fatal(err)
	}
	c = j.Check(ctx)
	if c.ReadClass != LoginFileMissing || !c.Has(id) {
		t.Fatalf("an unreadable file must not clear the login: %+v", c)
	}
	if f := j.Facts(); f.InFile != 1 || f.ReadFailed != LoginFileMissing {
		t.Fatalf("facts: %+v", f)
	}
	if err := os.WriteFile(path, []byte("not a login file"), 0o640); err != nil {
		t.Fatal(err)
	}
	if c = j.Check(ctx); c.ReadClass != LoginFileGarbled || !c.Has(id) {
		t.Fatalf("a garbled file: %+v", c)
	}
	if err := os.Rename(path+".aside", path); err != nil {
		// Windows will not rename over an existing file with a plain Rename in every case.
		if rerr := os.Remove(path); rerr != nil {
			t.Fatal(err, rerr)
		}
		if err := os.Rename(path+".aside", path); err != nil {
			t.Fatal(err)
		}
	}

	// A run removes it, and the same measurement says so at the end of the run.
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if f := j.Facts(); f.InFile != 0 || f.ReadFailed != "" {
		t.Fatalf("after the run: %+v", f)
	}
}

// The store cannot be read: not read, never "nothing retired".
func TestLoginFileCheckAStoreThatCannotAnswerIsNotRead(t *testing.T) {
	j, st, _ := loginRig(t)
	ctx := context.Background()
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE devices RENAME COLUMN retired_at TO retired_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c := j.Check(ctx); c.ReadClass != LoginFileNoStore {
		t.Fatalf("%+v", c)
	}
}

// A run waits its turn, and a check waits for a run in flight, within its own bound.
func TestLoginSyncRunsDoNotOverlapAndACheckWaitsForThem(t *testing.T) {
	j, _, _ := loginRig(t)
	ctx := context.Background()
	if err := j.take(ctx); err != nil { // a run in flight
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() { close(started); finished <- j.Run(ctx) }()
	<-started
	select {
	case err := <-finished:
		t.Fatalf("a second run did not wait for the first: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(j.Dir, gateway.FileName)); err == nil {
		t.Fatal("the second run wrote the file while the first was in flight")
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if c := j.Check(short); c.ReadClass != LoginFileBusy {
		t.Fatalf("a check that could not wait: %+v", c)
	}
	if f := j.Facts(); f.MeasuredAt != 0 {
		t.Fatalf("a check that read nothing recorded a measurement: %+v", f)
	}
	j.give()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

// A run reads the logins inside its turn: a retire that commits while another run
// holds the slot is honoured by the run that was waiting.
func TestALoginSyncRunReadsTheLoginsInsideItsTurn(t *testing.T) {
	j, st, id := loginRig(t)
	ctx := context.Background()
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.take(ctx); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- j.Run(ctx) }() // queued before the retire
	time.Sleep(50 * time.Millisecond)
	retireForTest(t, st, id)
	j.give()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if c := j.Check(ctx); c.ReadClass != "" || c.Has(id) {
		t.Fatalf("the queued run wrote a login read before its turn: %+v", c)
	}
}
