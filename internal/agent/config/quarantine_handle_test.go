package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A taken quarantine name is ErrQuarantineExists (and is retried) whatever else
// is happening to the tree. On Windows an open handle inside it (a scanner, the
// indexer) makes a directory rename fail with "Access is denied", which used to
// be returned instead, so the retry with a fresh name never happened.
func TestQuarantineRootTakenNamesWinOverAnOpenHandle(t *testing.T) {
	suffixes := []string{"00000001", "00000002", "00000003", "00000004", "00000005"}
	n := stubSuffixes(t, suffixes...)
	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	plant(t, l.Root)
	for _, s := range suffixes {
		if err := os.Mkdir(l.QuarantinePath(stamp, s), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(l.ConfigFile()) // held for the whole call, like a scanner
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = QuarantineRoot(l.Root, stamp)
	if !errors.Is(err, ErrQuarantineExists) || *n != QuarantineAttempts {
		t.Fatalf("with an open handle and five taken names: %v after %d names, want ErrQuarantineExists after %d", err, *n, QuarantineAttempts)
	}
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		t.Errorf("the root was changed: %v", err)
	}
}
