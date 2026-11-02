//go:build !windows

package supervisor

import (
	"os"
	"os/exec"
)

// control manages one child's lifetime off Windows (CI and development): there
// is no job object, so the interrupt is SIGINT and the hard stop is Kill, and
// there is no memory cap (Spec.MemoryLimitMiB is ignored).
type control struct {
	proc *os.Process
}

func prepare(cmd *exec.Cmd, spec Spec) {}

func newControl(cmd *exec.Cmd, spec Spec) (*control, error) {
	return &control{proc: cmd.Process}, nil
}

func (c *control) interrupt() error { return c.proc.Signal(os.Interrupt) }

func (c *control) terminate() error { return c.proc.Kill() }

func (c *control) terminateMemory() error { return c.proc.Kill() }

// memoryLimit never fires: there is no cap to hit.
func (c *control) memoryLimit() <-chan struct{} { return nil }

func (c *control) memoryLimitHit() bool { return false }

func (c *control) close() {}

// PhysicalMemoryMiB is not implemented off Windows; the agent's memory cap is
// a Windows job-object limit.
func PhysicalMemoryMiB() (int64, bool) { return 0, false }
