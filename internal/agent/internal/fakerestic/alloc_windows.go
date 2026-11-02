//go:build windows

package fakerestic

import "golang.org/x/sys/windows"

// commitUntilRefused commits memory with VirtualAlloc, outside the Go heap,
// until Windows refuses a commit (a job-object memory cap was reached) or
// limit bytes are committed. It reports whether a commit was refused. The Go
// runtime is not involved, so the process survives the refusal and the caller
// decides what happens next.
func commitUntilRefused(limit uintptr) bool {
	const chunk = 16 << 20
	for total := uintptr(0); total < limit; total += chunk {
		if _, err := windows.VirtualAlloc(0, chunk, windows.MEM_RESERVE|windows.MEM_COMMIT, windows.PAGE_READWRITE); err != nil {
			return true
		}
	}
	return false
}
