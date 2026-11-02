//go:build !windows

package logs

import (
	"os"
	"testing"
)

// listedSizes is the size of name in dir as os.ReadDir reports it.
func listedSizes(t *testing.T, dir, name string) map[string]int64 {
	t.Helper()
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
			return map[string]int64{"os.ReadDir": info.Size()}
		}
	}
	t.Fatalf("%s is not listed", name)
	return nil
}
