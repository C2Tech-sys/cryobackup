package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Where checkSwap looks on Linux. Inside a container /proc/swaps lists the
// host's swap areas, and /sys/block is the host's too.
const (
	procSwapsPath = "/proc/swaps"
	sysBlockPath  = "/sys/block"
)

// checkSwap is the doctor line of ADR 0008: a host with swap must encrypt it or
// run without it, because swapped-out memory of the console, the gateway or the
// maintenance worker can hold restore bytes, repository keys and the master
// key. It reports OK with no active swap, OK when every swap area is known to be
// encrypted (a dm-crypt device) or in memory (zram), and WARN otherwise or when
// it cannot tell. It never fails: doctor cannot prove encryption a layer below
// (a swap file on an encrypted filesystem, say), and says so. No device name or
// path is printed.
func checkSwap(procSwaps, sysBlock string, report func(level, name, detail string)) {
	const name = "swap"
	data, err := os.ReadFile(procSwaps)
	if err != nil {
		if runtime.GOOS != "linux" {
			report("WARN", name, "cannot be checked on this platform: the host must run without swap or with encrypted swap (ADR 0008)")
			return
		}
		report("WARN", name, "the swap table could not be read: confirm the host runs without swap or with encrypted swap (ADR 0008)")
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	total, unknown := 0, 0
	for i, line := range lines {
		f := strings.Fields(line)
		if i == 0 || len(f) == 0 { // the header line
			continue
		}
		total++
		if !swapProtected(unescapeSwapName(f[0]), sysBlock) {
			unknown++
		}
	}
	switch {
	case total == 0:
		report("OK", name, "no active swap")
	case unknown == 0:
		report("OK", name, fmt.Sprintf("%d active swap area(s), all encrypted (dm-crypt) or in memory (zram)", total))
	default:
		report("WARN", name, fmt.Sprintf("%d active swap area(s), %d not known to be encrypted: turn swap off or encrypt it (ADR 0008: swapped memory can hold restore data and keys)", total, unknown))
	}
}

// unescapeSwapName undoes the octal escapes the kernel writes for whitespace
// in /proc/swaps (\040 for a space).
func unescapeSwapName(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// swapProtected reports whether a swap area is known not to land on disk in
// clear text: a zram device, or a device-mapper device whose dm uuid is a
// dm-crypt one ("CRYPT-..."). Anything else - a partition, a swap file, a
// device it cannot resolve - is not known to be.
func swapProtected(dev, sysBlock string) bool {
	base := filepath.Base(dev)
	if strings.HasPrefix(base, "zram") {
		return true
	}
	if strings.HasPrefix(dev, "/dev/mapper/") {
		if r, err := filepath.EvalSymlinks(dev); err == nil {
			base = filepath.Base(r)
		} else if dm := dmByName(base, sysBlock); dm != "" {
			base = dm
		}
	}
	if !strings.HasPrefix(base, "dm-") {
		return false
	}
	uuid, err := os.ReadFile(filepath.Join(sysBlock, base, "dm", "uuid"))
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(uuid)), "CRYPT-")
}

// dmByName finds the dm-N block device whose dm/name is name.
func dmByName(name, sysBlock string) string {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "dm-") {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(sysBlock, e.Name(), "dm", "name")); err == nil && strings.TrimSpace(string(b)) == name {
			return e.Name()
		}
	}
	return ""
}
