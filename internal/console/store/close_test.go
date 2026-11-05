package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// M3-STORE2 1: Close is bounded behind work that does not end. Past the bound it
// answers ErrCloseAbandoned, so the process can exit; nothing committed is lost.

// wantAbandonedWithin closes s and checks the answer and the time it took.
func wantAbandonedWithin(t *testing.T, s *Store, d time.Duration) {
	t.Helper()
	got := make(chan error, 1)
	go func() { got <- s.Close() }()
	select {
	case err := <-got:
		if !errors.Is(err, ErrCloseAbandoned) {
			t.Fatalf("Close = %v, want ErrCloseAbandoned", err)
		}
	case <-time.After(d):
		t.Fatalf("Close still blocked after %v", d)
	}
	if err := s.Close(); !errors.Is(err, ErrCloseAbandoned) {
		t.Fatalf("the second Close = %v, want the same answer", err)
	}
}

func TestCloseIsBoundedBehindAStuckWrite(t *testing.T) { // M3-STORE2 1
	path := filepath.Join(t.TempDir(), "console.db")
	bg := context.Background()
	s, err := Open(bg, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	s.closeWait = 100 * time.Millisecond
	if err := s.RecordJobRun(bg, "committed", JobRun{Status: "ok", StartedAt: t0, FinishedAt: t0}); err != nil {
		t.Fatal(err)
	}
	entered, release, ended := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		ended <- s.Write(bg, func(tx *Tx) error {
			if _, err := tx.Exec(`INSERT INTO job_state (name, created_at, updated_at) VALUES ('uncommitted', 1, 1)`); err != nil {
				return err
			}
			close(entered)
			<-release
			return errors.New("the stuck work ends")
		})
	}()
	<-entered
	wantAbandonedWithin(t, s, 30*time.Second)
	close(release)
	if err := <-ended; err == nil {
		t.Fatal("the stuck Write committed")
	}

	// The next open has what was committed and nothing of the stuck transaction. The
	// abandoned close finishes in the background once the stuck work has ended, so
	// the file may still be held for a moment. On the old code the test never gets here.
	var again *Store
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if again, err = Open(bg, Options{Path: path, BusyTimeout: 100 * time.Millisecond}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the next open: %v", err)
		}
	}
	defer again.Close()
	if j, err := again.GetJobState(bg, "committed"); err != nil || j.RunCount != 1 {
		t.Fatalf("the committed row after an abandoned close: %+v %v", j, err)
	}
	if _, err := again.GetJobState(bg, "uncommitted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the stuck transaction's row after an abandoned close: %v, want not found", err)
	}
}

func TestCloseIsBoundedBehindAStuckRead(t *testing.T) { // M3-STORE2 1
	bg := context.Background()
	s, err := OpenMemory(bg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.closeWait = 100 * time.Millisecond
	entered, release, ended := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		ended <- s.Read(bg, func(tx *Tx) error {
			rows, err := tx.Query(`SELECT name FROM sqlite_master`)
			if err != nil {
				return err
			}
			defer rows.Close()
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	wantAbandonedWithin(t, s, 30*time.Second)
	close(release)
	<-ended
}

// With nothing in flight Close is clean, and a Write after it answers ErrClosed.
func TestCloseWithNothingInFlightIsClean(t *testing.T) { // M3-STORE2 1
	bg := context.Background()
	s, err := OpenMemory(bg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if err := s.Write(bg, func(*Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}
}
