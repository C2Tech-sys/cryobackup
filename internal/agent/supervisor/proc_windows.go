//go:build windows

package supervisor

import (
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job-object completion-port messages (winnt.h). x/sys/windows does not name them.
const (
	jobMsgActiveProcessZero  = 4  // JOB_OBJECT_MSG_ACTIVE_PROCESS_ZERO
	jobMsgProcessMemoryLimit = 9  // JOB_OBJECT_MSG_PROCESS_MEMORY_LIMIT
	jobMsgJobMemoryLimit     = 10 // JOB_OBJECT_MSG_JOB_MEMORY_LIMIT
)

// statusNoMemory (STATUS_NO_MEMORY) is the exit code a child gets when the
// supervisor ends it after it hit the memory cap.
const statusNoMemory = 0xC0000017

// jobAssociateCompletionPort is JOBOBJECT_ASSOCIATE_COMPLETION_PORT.
type jobAssociateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

// control manages one child's lifetime on Windows: a job object with
// KILL_ON_JOB_CLOSE (so a crashed agent cannot leave restic running), an
// optional process memory cap watched through a completion port, a graceful
// interrupt and a hard stop.
type control struct {
	pid uint32
	job windows.Handle // 0 when the job object could not be set up

	// Set only with a memory cap.
	port     windows.Handle
	stopping atomic.Bool // set by closePort before the reader is stopped
	memHit   atomic.Bool
	memOnce  sync.Once
	memCh    chan struct{} // closed on the first memory-limit message
	zeroCh   chan struct{} // closed when the job has no active process left
	zeroOnce sync.Once
	portDone chan struct{}
}

// prepare sets the creation flags: a new process group (so CTRL_BREAK_EVENT can
// address the child alone) and, in service mode, no console window.
func prepare(cmd *exec.Cmd, spec Spec) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP)
	if spec.NoWindow {
		flags |= windows.CREATE_NO_WINDOW
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
}

// newControl assigns the started child to a new job object. It always returns
// a usable control; a non-nil error says the job could not be set up and the
// child is supervised by interrupt and terminate only.
//
// With a memory cap the job is also associated with a completion port before
// the child is assigned, so the memory-limit message cannot be missed.
// Windows does not kill a process at JOB_OBJECT_LIMIT_PROCESS_MEMORY: its
// next commit fails. restic is a Go program; the runtime prints
// "runtime: VirtualAlloc of <n> bytes failed with errno=1455" and "fatal error:
// out of memory" and then, measured with the Go 1.26 runtime under a 256 MiB
// job cap, does not exit: the process stays alive with its pipes open until
// the job is terminated. The supervisor therefore watches the port and ends
// the child itself (Run, memoryKillGrace).
func newControl(cmd *exec.Cmd, spec Spec) (*control, error) {
	c := &control{pid: uint32(cmd.Process.Pid)}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return c, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if spec.MemoryLimitMiB > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.ProcessMemoryLimit = uintptr(spec.MemoryLimitMiB) << 20
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return c, err
	}
	if spec.MemoryLimitMiB > 0 {
		// A port that cannot be set up leaves the cap in force; only the
		// memory classification and the kill of a stuck child are lost.
		c.watchMemory(job)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, c.pid)
	if err != nil {
		c.closePort()
		_ = windows.CloseHandle(job)
		return c, err
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		c.closePort()
		_ = windows.CloseHandle(job)
		return c, err
	}
	c.job = job
	return c, nil
}

// watchMemory associates job with a new completion port and starts the reader
// of its messages.
func (c *control) watchMemory(job windows.Handle) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return
	}
	assoc := jobAssociateCompletionPort{CompletionKey: 1, CompletionPort: port}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc))); err != nil {
		_ = windows.CloseHandle(port)
		return
	}
	c.port = port
	c.initWatch()
	go c.readPort(port, portWait)
}

// initWatch makes the channels the port reader closes.
func (c *control) initWatch() {
	c.memCh, c.zeroCh, c.portDone = make(chan struct{}), make(chan struct{}), make(chan struct{})
}

// portQuitKey is the completion key of the packet closePort posts to stop the
// reader. The job's own messages carry key 1 (watchMemory).
const portQuitKey = 2

// portPollMillis bounds one wait of the reader, so that it also notices
// c.stopping when the quit packet could not be posted.
const portPollMillis = 1000

// portWait is GetQueuedCompletionStatus; tests replace the reader's wait to
// drive the message order and to check that the port is never waited on after
// it is closed.
type portWaitFunc func(port windows.Handle, msg *uint32, key *uintptr, ov **windows.Overlapped, millis uint32) error

var portWait portWaitFunc = windows.GetQueuedCompletionStatus

// readPort reads the job's completion port until closePort asks it to stop.
// It is the only goroutine that waits on the port, and closePort closes the
// handle only after this function has returned, so the port is never waited
// on after CloseHandle (a closed handle value can be reused by another object).
func (c *control) readPort(port windows.Handle, wait portWaitFunc) {
	defer close(c.portDone)
	for {
		var msg uint32
		var key uintptr
		var ov *windows.Overlapped
		err := wait(port, &msg, &key, &ov, portPollMillis)
		if c.stopping.Load() {
			return
		}
		if err != nil {
			if errors.Is(err, windows.WAIT_TIMEOUT) {
				continue
			}
			return // the port failed: only the memory classification is lost
		}
		if key == portQuitKey {
			return
		}
		c.deliver(msg)
	}
}

// deliver records one job message.
func (c *control) deliver(msg uint32) {
	switch msg {
	case jobMsgProcessMemoryLimit, jobMsgJobMemoryLimit:
		c.memHit.Store(true)
		c.memOnce.Do(func() { close(c.memCh) })
	case jobMsgActiveProcessZero:
		c.zeroOnce.Do(func() { close(c.zeroCh) })
	}
}

// memoryLimit is closed when the child hit the memory cap; nil (never ready)
// without a cap or a port.
func (c *control) memoryLimit() <-chan struct{} { return c.memCh }

// activeProcessZeroWait bounds how long memoryLimitHit waits for "no active
// process" after the child has exited.
const activeProcessZeroWait = 2 * time.Second

// memoryLimitHit reports, after the child has exited, whether it hit the cap.
// The child's exit and the port's messages race: Wait can return before the
// reader has taken the memory-limit message off the port. The port delivers in
// order, so once "no active process" (posted after the last process left the
// job) has been read every earlier memory message has been too; the wait for
// it is bounded.
//
// The wait also ends the moment the reader has returned (portDone): it is the
// only goroutine that takes messages off the port, so after it is gone nothing
// more can arrive and the answer is final. That is the case when the job could
// not be set up after the port was created (newControl closes the port again)
// and when the port itself failed; neither makes a run wait the bound.
func (c *control) memoryLimitHit() bool {
	if c.zeroCh == nil {
		return false
	}
	select {
	case <-c.zeroCh:
	case <-c.portDone:
	case <-time.After(activeProcessZeroWait):
	}
	return c.memHit.Load()
}

// interrupt sends CTRL_BREAK_EVENT to the child's process group. UNCERTAIN
// (L-CANCEL-1): it reaches the child only when agent and child share a console
// (console mode); a service has none and the call fails, so the caller falls
// back to terminate.
func (c *control) interrupt() error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, c.pid)
}

// terminate ends the child through the job object (exit code 130), or through
// the process handle when there is no job.
func (c *control) terminate() error { return c.kill(130) }

// terminateMemory ends a child that hit the memory cap (exit STATUS_NO_MEMORY).
func (c *control) terminateMemory() error { return c.kill(statusNoMemory) }

func (c *control) kill(code uint32) error {
	if c.job != 0 {
		return windows.TerminateJobObject(c.job, code)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, c.pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(proc)
	return windows.TerminateProcess(proc, code)
}

// close releases the job; KILL_ON_JOB_CLOSE ends any survivor. The port reader
// is stopped and waited for.
func (c *control) close() {
	if c.job != 0 {
		_ = windows.CloseHandle(c.job)
		c.job = 0
	}
	c.closePort()
}

// closePort stops the reader and only then closes the port: it asks the reader
// to stop (c.stopping, and a quit packet so it wakes at once), waits for it to
// return, and closes the handle last. If the quit packet cannot be posted the
// reader still sees c.stopping within portPollMillis.
func (c *control) closePort() {
	if c.port == 0 {
		return
	}
	c.stopping.Store(true)
	_ = portPost(c.port, 0, portQuitKey, nil)
	<-c.portDone
	_ = portClose(c.port)
	c.port = 0
}

// portPost and portClose are PostQueuedCompletionStatus and CloseHandle; tests
// replace them to observe the order of the calls.
var (
	portPost  = windows.PostQueuedCompletionStatus
	portClose = windows.CloseHandle
)

// PhysicalMemoryMiB returns the machine's installed physical memory in MiB
// (GlobalMemoryStatusEx, ullTotalPhys), or false when it cannot be read.
func PhysicalMemoryMiB() (int64, bool) {
	// MEMORYSTATUSEX; x/sys/windows has no wrapper for GlobalMemoryStatusEx.
	var ms struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys            uint64
		AvailPhys            uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 || ms.TotalPhys == 0 {
		return 0, false
	}
	return int64(ms.TotalPhys >> 20), true
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
