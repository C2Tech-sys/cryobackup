package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorSwap covers the ADR 0008 swap line: OK with no swap or only
// encrypted/in-memory swap, WARN for a partition, a swap file, an unknown
// device-mapper device or an unreadable table, and never FAIL or a path.
func TestDoctorSwap(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys", "block")
	mkdm := func(dm, name, uuid string) {
		d := filepath.Join(sys, dm, "dm")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(d, "name"), []byte(name+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(d, "uuid"), []byte(uuid+"\n"), 0o644)
	}
	mkdm("dm-1", "cryptswap", "CRYPT-PLAIN-cryptswap")
	mkdm("dm-2", "vg-swap", "LVM-abcdef")
	const head = "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"
	cases := []struct {
		name, table, level, detail string
	}{
		{"no swap", head, "OK", "no active swap"},
		{"empty file", "", "OK", "no active swap"},
		{"dm-crypt by dm-N", head + "/dev/dm-1  partition 4194300 0 -2\n", "OK", "all encrypted"},
		{"dm-crypt by mapper name", head + "/dev/mapper/cryptswap  partition 4194300 0 -2\n", "OK", "all encrypted"},
		{"zram", head + "/dev/zram0  partition 1048572 0 100\n", "OK", "in memory"},
		{"plain partition", head + "/dev/sda2  partition 4194300 0 -2\n", "WARN", "1 not known to be encrypted"},
		{"swap file", head + "/swap\\040file  file 2097148 0 -3\n", "WARN", "1 not known to be encrypted"},
		{"lvm, not crypt", head + "/dev/mapper/vg-swap  partition 4194300 0 -2\n", "WARN", "1 not known to be encrypted"},
		{"mixed", head + "/dev/dm-1  partition 1 0 -2\n/dev/sdb1  partition 1 0 -3\n", "WARN", "2 active swap area(s), 1 not known"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, "swaps-"+strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.WriteFile(p, []byte(tc.table), 0o644); err != nil {
				t.Fatal(err)
			}
			var level, name, detail string
			calls := 0
			checkSwap(p, sys, func(l, n, d string) { level, name, detail = l, n, d; calls++ })
			if calls != 1 || name != "swap" || level != tc.level || !strings.Contains(detail, tc.detail) {
				t.Fatalf("got %d report(s): %s %s %q; want one %s containing %q", calls, level, name, detail, tc.level, tc.detail)
			}
			for _, leak := range []string{dir, "/dev/", "sda", "cryptswap", "swap file"} {
				if strings.Contains(detail, leak) {
					t.Fatalf("doctor must not print device names or paths: %q", detail)
				}
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		var level string
		checkSwap(filepath.Join(dir, "missing"), sys, func(l, _, _ string) { level = l })
		if level != "WARN" {
			t.Fatalf("an unreadable swap table is a WARN, never OK or FAIL: %s", level)
		}
	})
}

// TestDoctorPrintsTheSwapLine runs the real command: the swap line is there and
// is never a FAIL.
func TestDoctorPrintsTheSwapLine(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0")...); code != 0 {
		t.Fatal(e)
	}
	_, out, _ := run(t, "", "doctor", "--config", filepath.Join(dir, "config.json"))
	if !strings.Contains(out, "OK    swap") && !strings.Contains(out, "WARN  swap") {
		t.Fatalf("doctor lacks the swap line:\n%s", out)
	}
	if strings.Contains(out, "FAIL  swap") {
		t.Fatalf("the swap line never fails:\n%s", out)
	}
}
