//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The CI and lab stand-in for the HKLM verdict: a file next to the root,
// mode 0600, honoured only when root owns it.
func TestVerdictFileStandIn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "CryoBackup")
	at := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)

	if _, present, err := ReadVerdict(root); err != nil || present {
		t.Fatalf("no file: present=%v %v", present, err)
	}
	if err := WriteVerdict(root, Verdict{Since: at, Reason: "the state root: owner 1000"}); err != nil {
		t.Fatalf("WriteVerdict: %v", err)
	}
	st, err := os.Lstat(verdictFile(root))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("verdict file: %v %v, want mode 0600", st, err)
	}
	if os.Geteuid() != 0 {
		// Written by an ordinary account: a planted verdict is not honoured.
		if _, present, err := ReadVerdict(root); err != nil || present {
			t.Errorf("a verdict file owned by uid %d was honoured", os.Geteuid())
		}
	}

	old := verdictOwnerOK
	t.Cleanup(func() { verdictOwnerOK = old })
	me := uint32(os.Getuid())
	verdictOwnerOK = func(uid uint32) bool { return uid == 0 || uid == me } // tests only: this process's own uid

	v, present, err := ReadVerdict(root)
	if err != nil || !present || !v.Since.Equal(at) || v.Reason != "the state root: owner 1000" {
		t.Fatalf("ReadVerdict: %+v %v %v", v, present, err)
	}
	if err := WriteVerdict(root, Verdict{Since: at.Add(time.Hour), Reason: "later"}); err != nil {
		t.Fatalf("a second WriteVerdict: %v", err)
	}
	if v, _, _ := ReadVerdict(root); v.Reason == "later" {
		t.Error("a second write replaced the verdict")
	}
	if err := ClearVerdict(root); err != nil {
		t.Fatalf("ClearVerdict: %v", err)
	}
	if _, present, _ := ReadVerdict(root); present {
		t.Error("the verdict survived ClearVerdict")
	}
	if err := ClearVerdict(root); err != nil {
		t.Errorf("clearing a missing verdict: %v", err)
	}

	// A link at the path is not a verdict, and is not written through.
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, verdictFile(root)); err != nil {
		t.Skipf("cannot create a symbolic link: %v", err)
	}
	if _, present, err := ReadVerdict(root); err != nil || present {
		t.Errorf("a symbolic link was honoured as a verdict: %v %v", present, err)
	}
	if err := WriteVerdict(root, Verdict{Since: at, Reason: "y"}); err != nil {
		t.Fatalf("WriteVerdict over a link: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "x" {
		t.Errorf("WriteVerdict wrote through a link: %q", b)
	}
}
