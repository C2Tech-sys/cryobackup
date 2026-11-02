//go:build windows

package logs

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// listedSizes is the size of name in dir as directory listings report it:
// Go's ReadDir and cmd's dir (what an operator, Explorer or an RMM file
// check sees). On NTFS both come from the directory entry, not the open file.
func listedSizes(t *testing.T, dir, name string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == name {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			out["os.ReadDir"] = info.Size()
		}
	}
	listing, err := exec.Command("cmd", "/c", "dir", "/-C", dir).Output()
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	for _, line := range strings.Split(string(listing), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[len(f)-1] == name {
			if n, err := strconv.ParseInt(f[len(f)-2], 10, 64); err == nil {
				out["dir"] = n
			}
		}
	}
	if len(out) != 2 {
		t.Fatalf("%s is not listed by both (%v)", name, out)
	}
	return out
}
