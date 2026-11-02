package maintain

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// runnerWaitDelay is how long exec waits for a child (and for pipes that a
// grandchild such as rclone still holds open) after the child has been asked to
// stop, before it kills it and closes the pipes.
const runnerWaitDelay = 10 * time.Second

// runner is the one place in this package that starts a process. It executes
// the restic binary with a caller-built argv and a caller-built environment,
// reads both output streams concurrently so that neither can fill its pipe and
// stall the child, and returns only what the internal/restic decoders let
// through: the exit code, the stderr error classes and counts, and whatever the
// stdout consumer extracted. It never retains, logs or returns a raw output
// line, and it never touches a shell.
type runner struct {
	// program is the restic executable.
	program string
	// dir is the child's working directory (a scratch directory the worker owns).
	dir string
}

// procResult is the outcome of one process run. It carries no text from the
// child.
type procResult struct {
	// Exit is the process exit code; -1 when the process was killed by a
	// signal or never produced one.
	Exit int
	// TimedOut is set when the per-call timeout elapsed; the child was asked to
	// stop (interrupt, then kill after runnerWaitDelay).
	TimedOut bool
	// Canceled is set when the caller's context was cancelled.
	Canceled bool
	// ExitError is the decoded final exit_error message of stderr, if any.
	ExitError *restic.ExitError
	// Counts holds the classified stderr error events (restic.ErrorCounts).
	Counts restic.ErrorCounts
	// Stderr is restic.StderrClass(ExitError, Counts).
	Stderr restic.ErrorClass
	// StdoutErr is the error the stdout consumer returned, if any.
	StdoutErr error
}

// run starts the child and waits for it. environ must be non-empty (a nil
// slice would make exec inherit the parent's environment). onStdout, when
// non-nil, is called in its own goroutine with the stdout stream; whatever it
// leaves unread is discarded. run returns an error only when the process could
// not be started; the error text names the program path, so callers map it to a
// fixed code and never log it.
func (r *runner) run(ctx context.Context, timeout time.Duration, args, environ []string, onStdout func(io.Reader) error) (procResult, error) {
	if len(environ) == 0 {
		return procResult{Exit: -1}, errors.New("maintain: empty child environment")
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, r.program, args...)
	cmd.Env = environ
	cmd.Dir = r.dir
	cmd.WaitDelay = runnerWaitDelay
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill() // Windows cannot deliver os.Interrupt to a child
		}
		return cmd.Process.Signal(os.Interrupt) // restic exits 130 and removes its lock
	}

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	var res procResult
	res.Counts = make(restic.ErrorCounts)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if onStdout != nil {
			res.StdoutErr = onStdout(stdoutR)
		}
		_, _ = io.Copy(io.Discard, stdoutR)
	}()
	go func() {
		defer wg.Done()
		res.ExitError = decodeStderr(stderrR, res.Counts)
		_, _ = io.Copy(io.Discard, stderrR)
	}()

	if err := cmd.Start(); err != nil {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		wg.Wait()
		return procResult{Exit: -1}, err
	}
	waitErr := cmd.Wait()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	wg.Wait()

	res.Exit = -1
	if ps := cmd.ProcessState; ps != nil {
		res.Exit = ps.ExitCode()
	}
	var ee *exec.ExitError
	_ = errors.As(waitErr, &ee) // the code is already read from ProcessState
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.TimedOut = true
	case ctx.Err() != nil:
		res.Canceled = true
	}
	res.Stderr = restic.StderrClass(res.ExitError, res.Counts)
	return res, nil
}

// decodeStderr reads restic's stderr through the path-free decoder. Error
// events are counted by class, plain-text lines contribute their class (when it
// says something), and the final exit_error is returned. No line is kept.
func decodeStderr(r io.Reader, counts restic.ErrorCounts) *restic.ExitError {
	dec := restic.NewDecoder(r)
	var exit *restic.ExitError
	for {
		msg, err := dec.Next()
		if err != nil {
			return exit
		}
		switch m := msg.(type) {
		case restic.ErrorEvent:
			counts.Add(m)
		case restic.ExitError:
			c := m
			exit = &c
		case restic.Unparsed:
			if m.Class != restic.ClassUnknown && m.Class != "" {
				counts.Add(restic.ErrorEvent{Class: m.Class})
			}
		}
	}
}
