//go:build !linux && !windows

package maintain

// freeDiskBytes cannot tell on this platform (the production worker runs on
// Linux): the self-backup logs that the free-disk check was skipped.
func freeDiskBytes(string) (uint64, bool, error) { return 0, false, nil }
