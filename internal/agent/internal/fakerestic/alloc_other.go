//go:build !windows

package fakerestic

// commitUntilRefused has no cap to run into off Windows (the memory cap is a
// job-object limit); it reports that nothing was refused.
func commitUntilRefused(limit uintptr) bool { return false }
