package maintain

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeInfo is an Lstat answer of the fake filesystem below.
type fakeInfo struct {
	name string
	mode fs.FileMode
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

// linuxENOTDIR is ENOTDIR as Linux reports it: it is syscall.ENOTDIR and is
// not "does not exist". (On Windows, Go also matches its "path not found" as
// ENOTDIR and ErrNotExist at once, so the real errno cannot model Linux here.)
type linuxENOTDIR struct{}

func (linuxENOTDIR) Error() string        { return "not a directory" }
func (linuxENOTDIR) Is(target error) bool { return target == syscall.ENOTDIR }

// fakeLstat answers from a map of cleaned paths: a FileMode, or ENOTDIR for
// any path below a regular file (as Linux does), else ENOENT.
func fakeLstat(entries map[string]fs.FileMode) func(string) (os.FileInfo, error) {
	return func(p string) (os.FileInfo, error) {
		p = filepath.Clean(p)
		if m, ok := entries[p]; ok {
			return fakeInfo{name: filepath.Base(p), mode: m}, nil
		}
		for q := filepath.Dir(p); q != filepath.Dir(q); q = filepath.Dir(q) {
			if m, ok := entries[q]; ok && m.IsRegular() {
				return nil, &os.PathError{Op: "lstat", Path: p, Err: linuxENOTDIR{}}
			}
		}
		return nil, &os.PathError{Op: "lstat", Path: p, Err: syscall.ENOENT}
	}
}

// TestScratchPathChecksEveryComponent (M1-HIPAA2 last batch, items 1-2): the
// pre-creation check maps Linux's ENOTDIR (a path below a regular file) to the
// refusal, and refuses a symlink anywhere in the existing part of the path -
// the directory itself, the directory it would be created under, or any
// ancestor (/var/run -> /run) - while a path of real directories passes.
func TestScratchPathChecksEveryComponent(t *testing.T) {
	root := rootOf()
	j := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	dir, link, file := fs.ModeDir|0o755, fs.ModeSymlink|0o777, fs.FileMode(0o644)
	fsys := map[string]fs.FileMode{
		filepath.Clean(root):          dir,
		j("run"):                      dir,
		j("run", "cryo-maintain"):     dir,
		j("var"):                      dir,
		j("var", "run"):               link, // -> /run on most distributions
		j("var", "run", "cryo"):       dir,  // what /run/cryo looks like through the link
		j("srv"):                      dir,
		j("srv", "a-file"):            file,
		j("srv", "real"):              dir,
		j("srv", "real", "linked"):    link,
		j("srv", "real", "deep"):      dir,
		j("srv", "real", "deep", "x"): dir,
	}
	mine := func(os.FileInfo) (bool, bool) { return true, true }
	for _, tc := range []struct {
		path string
		want error
	}{
		{j("run", "cryo-maintain"), nil},                        // exists, real all the way
		{j("run", "new-scratch"), nil},                          // created under /run
		{j("srv", "real", "deep", "x", "scratch"), nil},         // created under a real tree
		{j("srv", "a-file", "scratch"), errScratchNotOurs},      // ENOTDIR on Linux
		{j("var", "run", "cryo-maintain"), errScratchSymlink},   // created under the symlink itself
		{j("var", "run", "cryo", "scratch"), errScratchSymlink}, // a symlinked ancestor higher up
		{j("var", "run", "cryo"), errScratchSymlink},            // an existing directory below a symlink
		{j("srv", "real", "linked"), errScratchSymlink},         // the directory itself is a symlink
	} {
		err := checkScratchPathWith(tc.path, mine, fakeLstat(fsys))
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v", tc.path, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: err = %v, want %v", tc.path, err, tc.want)
		}
	}
	if !errors.Is(errScratchSymlink, errScratchNotOurs) {
		t.Error("errScratchSymlink must wrap errScratchNotOurs")
	}
}

// TestScratchRefusalsSayWhatToDo: the start-up message for a symlinked path
// names /var/run and what to use instead; the other refusal names ownership.
func TestScratchRefusalsSayWhatToDo(t *testing.T) {
	err := scratchErr(errScratchSymlink)
	if !errors.Is(err, errConfig) || !strings.Contains(err.Error(), "/var/run") || !strings.Contains(err.Error(), "use the real directory, such as /run/cryo-maintain") {
		t.Fatalf("symlink refusal: %v", err)
	}
	if err := scratchErr(errScratchNotOurs); !errors.Is(err, errConfig) || !strings.Contains(err.Error(), "owned by the worker's user") {
		t.Fatalf("ownership refusal: %v", err)
	}
	if err := scratchErr(os.ErrPermission); !errors.Is(err, errConfig) || strings.Contains(err.Error(), "symlink") {
		t.Fatalf("other failure: %v", err)
	}
}
