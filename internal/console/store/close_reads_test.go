package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// M3-HARDEN A5a: a Close that answers nil has released the database file. database/sql
// closes an in-use connection only when it is released, so a Read outliving rdb.Close
// kept the handle with Close reporting success (a CI run failed at TempDir cleanup on
// "console.db in use"). Close now waits for it, within its bound, and says if it could not.
func TestCloseWaitsForAReadInProgress(t *testing.T) {
	bg := context.Background()
	open := func(t *testing.T) *Store {
		t.Helper()
		s, err := Open(bg, Options{Path: filepath.Join(t.TempDir(), "console.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	read := func(s *Store) (entered, release chan struct{}, ended chan error) {
		entered, release, ended = make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			ended <- s.Read(bg, func(tx *Tx) error {
				var one int
				if err := tx.QueryRow(`SELECT 1`).Scan(&one); err != nil {
					return err
				}
				close(entered)
				<-release
				return nil
			})
		}()
		return entered, release, ended
	}

	t.Run("a read that does not end is reported", func(t *testing.T) {
		s := open(t)
		s.closeWait = 100 * time.Millisecond
		entered, release, ended := read(s)
		<-entered
		if err := s.Close(); !errors.Is(err, ErrCloseAbandoned) {
			t.Fatalf("Close = %v with a read in progress, want ErrCloseAbandoned", err)
		}
		close(release)
		if err := <-ended; err != nil {
			t.Fatalf("the read that was in progress: %v", err)
		}
	})

	t.Run("a read that ends in time is waited for", func(t *testing.T) {
		s := open(t)
		entered, release, ended := read(s)
		<-entered
		got := make(chan error, 1)
		go func() { got <- s.Close() }()
		select {
		case err := <-got:
			t.Fatalf("Close returned %v with a read in progress", err)
		case <-time.After(200 * time.Millisecond):
		}
		if err := s.Read(bg, func(*Tx) error { return nil }); !errors.Is(err, ErrClosed) {
			t.Fatalf("a new Read during Close = %v, want ErrClosed", err)
		}
		close(release)
		if err := <-got; err != nil {
			t.Fatalf("Close = %v", err)
		}
		if err := <-ended; err != nil {
			t.Fatalf("the read: %v", err)
		}
	})
}
