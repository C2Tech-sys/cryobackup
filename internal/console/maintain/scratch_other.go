//go:build !linux

package maintain

import "os"

// onMemoryFS cannot tell outside Linux; the production worker runs on Linux.
func onMemoryFS(string) (inMemory, known bool) { return false, false }

// ownedByMe cannot read a POSIX owner outside Linux: the check is skipped.
func ownedByMe(os.FileInfo) (mine, known bool) { return false, false }
