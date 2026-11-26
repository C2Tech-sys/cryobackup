package restores

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-R2a, A3 and A5: a browse that arrives while the restore.browse row of the same key is being
// written waits for that write and shares its result, so no listing is ever shown on a row that
// was not written; and the once-per-window memory belongs to its Service, judged and pruned by
// that Service's clock, so one console database never holds another's entries.

// waitForWaiter blocks until n browses wait on the claim of k (or fails after ten seconds).
func waitForWaiter(t *testing.T, s *Service, k browseKey, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.browseMu.Lock()
		c := s.browseSeen[k]
		got := 0
		if c != nil {
			got = c.waiters
		}
		s.browseMu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no browse is waiting on the first write")
}

// slowWrite makes the next browse write block until release is closed, then answer fail
// (nil: the real write).
func slowWrite(e *env, fail error) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.svc.browseAppend = func(ctx context.Context, ev store.AuditEvent) (store.AuditEvent, error) {
		once.Do(func() { close(entered) })
		<-release
		if fail != nil {
			return store.AuditEvent{}, fail
		}
		return e.st.AppendAudit(ctx, ev)
	}
	return entered, release
}

func TestR2A_ConcurrentBrowseSharesAFailedWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	who := Actor{AdminID: "01JADMINDDDDDDDDDDDDDDDDDD", Username: "dana"}
	k := browseKey{admin: who.AdminID, device: e.device.ID, snapshot: fixtures.FixtureSnapshotID}
	entered, release := slowWrite(e, errors.New("disk I/O error"))

	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		first <- e.svc.AuditBrowse(ctx, who, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID)
	}()
	<-entered // the first browse is writing its row
	go func() {
		second <- e.svc.AuditBrowse(ctx, who, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID)
	}()
	waitForWaiter(t, e.svc, k, 1)
	select {
	case err := <-second:
		t.Fatalf("the second browse did not wait for the first write: %v", err)
	default:
	}
	close(release) // the first write fails

	if err := <-first; !errors.Is(err, store.ErrAudit) {
		t.Fatalf("first browse: %v", err)
	}
	if err := <-second; !errors.Is(err, store.ErrAudit) {
		t.Fatalf("second browse was let through on a row that was never written: %v", err)
	}
	if n := len(browseRows(t, e)); n != 0 {
		t.Fatalf("%d rows after a failed write", n)
	}
	e.svc.browseMu.Lock()
	_, kept := e.svc.browseSeen[k]
	e.svc.browseMu.Unlock()
	if kept {
		t.Fatal("a failed write is remembered as seen")
	}

	// The next browse writes its row.
	e.svc.browseAppend = nil
	if err := e.svc.AuditBrowse(ctx, who, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID); err != nil {
		t.Fatal(err)
	}
	if n := len(browseRows(t, e)); n != 1 {
		t.Fatalf("%d rows after the retry", n)
	}
}

func TestR2A_ConcurrentBrowseSharesASuccessfulWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	who := Actor{AdminID: "01JADMINEEEEEEEEEEEEEEEEEE", Username: "eve"}
	k := browseKey{admin: who.AdminID, device: e.device.ID, snapshot: fixtures.FixtureSnapshotID}
	entered, release := slowWrite(e, nil)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		first <- e.svc.AuditBrowse(ctx, who, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID)
	}()
	<-entered
	go func() {
		second <- e.svc.AuditBrowse(ctx, who, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID)
	}()
	waitForWaiter(t, e.svc, k, 1)
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if n := len(browseRows(t, e)); n != 1 {
		t.Fatalf("%d rows for two browses at once, want one", n)
	}
}

// K5: two Services on two databases. A's entry never sits in B's memory, B's browse is judged by
// B's clock alone, and A's stale entry is pruned by A once its window has passed.
func TestR2A_BrowseMemoryIsPerServiceAndPruned(t *testing.T) {
	a, b := newEnv(t), newEnv(t)
	ctx := context.Background()
	who := Actor{AdminID: "01JADMINFFFFFFFFFFFFFFFFFF", Username: "fay"}
	if err := a.svc.AuditBrowse(ctx, who, store.ScopeAll(), a.device.ID, fixtures.FixtureSnapshotID); err != nil {
		t.Fatal(err)
	}
	aAt := a.now
	a.now = a.now.Add(BrowseAuditWindow + time.Minute)
	b.now = b.now.Add(BrowseAuditWindow + time.Minute)
	if err := b.svc.AuditBrowse(ctx, who, store.ScopeAll(), b.device.ID, fixtures.FixtureSnapshotID); err != nil {
		t.Fatal(err)
	}
	b.svc.browseMu.Lock()
	for k, c := range b.svc.browseSeen {
		if c.at.Equal(aAt) {
			t.Errorf("B's memory holds an entry of A: %+v", k)
		}
		if !c.at.Equal(b.now) {
			t.Errorf("B's memory holds an entry not from B's browse: %+v at %v", k, c.at)
		}
	}
	if n := len(b.svc.browseSeen); n != 1 {
		t.Errorf("B's memory holds %d entries, want 1", n)
	}
	b.svc.browseMu.Unlock()
	if n := len(browseRows(t, b)); n != 1 {
		t.Fatalf("B wrote %d rows", n)
	}

	// A browses again past its window: its stale entry is pruned and a new row is written.
	if err := a.svc.AuditBrowse(ctx, who, store.ScopeAll(), a.device.ID, fixtures.FixtureSnapshotID); err != nil {
		t.Fatal(err)
	}
	a.svc.browseMu.Lock()
	for k, c := range a.svc.browseSeen {
		if c.at.Equal(aAt) {
			t.Errorf("A's stale entry survived its window: %+v", k)
		}
	}
	a.svc.browseMu.Unlock()
	if n := len(browseRows(t, a)); n != 2 {
		t.Fatalf("A wrote %d rows, want 2", n)
	}
}
