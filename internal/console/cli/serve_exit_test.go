package cli

import (
	"bytes"
	"errors"
	"testing"
)

// M3-HARDEN B (m16): serve's exit code is what its supervisor acts on. A failed listener
// (TestServeListenerFailureEndsRunWithAnError makes Run answer one) or a database not closed
// cleanly exits 1, so Restart=on-failure brings it back; a clean stop exits 0.
func TestServeExitCode(t *testing.T) {
	var stderr bytes.Buffer
	if got := serveExit(nil, IO{Stderr: &stderr}); got != ExitOK || stderr.Len() != 0 {
		t.Errorf("a clean stop: %d, %q", got, stderr.String())
	}
	if got := serveExit(errors.New("cli: a listener failed"), IO{Stderr: &stderr}); got != ExitFailed {
		t.Errorf("a failed listener exits %d, want %d", got, ExitFailed)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("a listener failed")) {
		t.Errorf("stderr: %q", stderr.String())
	}
}
