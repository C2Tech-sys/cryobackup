//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// IsService reports whether the process was started by the service control
// manager (console mode, "cryo-agent run", is the false case).
func IsService() (bool, error) { return svc.IsWindowsService() }

// Run hands the process to the service control manager and runs h until the
// service is stopped.
func Run(h Host) error {
	return svc.Run(Name, &handler{host: h})
}

// handler implements svc.Handler for Host.
type handler struct {
	host Host
}

const acceptedControls = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown |
	svc.AcceptPowerEvent | svc.AcceptSessionChange

// Execute is svc.Handler.Execute. It posts the work to a goroutine, reports
// Running, and on Stop, Shutdown or PreShutdown cancels the agent's context
// (with the service-stop cause) while reporting StopPending with a checkpoint
// every two seconds, so the SCM does not give up before the supervisor has
// cancelled restic. Power and session notifications are accepted and handed to
// OnEvent, which must return at once: this loop is also what receives Stop.
func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending, WaitHint: 30000}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { done <- h.host.Run(ctx) }()
	s <- svc.Status{State: svc.Running, Accepts: acceptedControls}

	for {
		select {
		case err := <-done:
			// The agent ended by itself: report it as a failed stop.
			s <- svc.Status{State: svc.StopPending}
			if err != nil && !errors.Is(err, context.Canceled) {
				return false, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				kind := map[svc.Cmd]string{svc.Stop: "stop", svc.Shutdown: "shutdown", svc.PreShutdown: "preshutdown"}[c.Cmd]
				if h.host.OnStop != nil {
					h.host.OnStop(kind)
				}
				cancel(h.host.StopCause)
				budget := StopBudgetFor(kind)
				timeout := time.After(budget)
				tick := time.NewTicker(2 * time.Second)
				defer tick.Stop()
				var check uint32 = 1
				s <- svc.Status{State: svc.StopPending, CheckPoint: check, WaitHint: uint32(5 * time.Second / time.Millisecond)}
				for {
					select {
					case <-done:
						return false, 0
					case <-timeout:
						return false, 0
					case <-tick.C:
						check++
						s <- svc.Status{State: svc.StopPending, CheckPoint: check, WaitHint: uint32(5 * time.Second / time.Millisecond)}
					}
				}
			case svc.PowerEvent:
				if h.host.OnEvent != nil {
					h.host.OnEvent(Event{Power: true, Code: c.EventType})
				}
			case svc.SessionChange:
				if h.host.OnEvent != nil {
					h.host.OnEvent(Event{Power: false, Code: c.EventType})
				}
			}
		}
	}
}

// InstallOptions configures Install.
type InstallOptions struct {
	// Exe is the path of cryo-agent.exe; empty means the running executable.
	Exe string
	// Args are the service's arguments, normally "run" and --state-dir.
	Args []string
}

// Install registers the service (LocalSystem, delayed automatic start,
// recovery = restart, description) and the Event Log source. It needs an
// elevated process. An existing service is updated, not duplicated.
func Install(o InstallOptions) error {
	if err := installService(o); err != nil {
		return err
	}
	return ensureEventSource()
}

// installService and ensureEventSource are Install's two steps: the service
// manager part and the Event Log source. Tests replace them to check without
// elevation that Install registers the source.
var (
	installService    = registerService
	ensureEventSource = EnsureEventSource
)

// registerService creates or updates the service in the service manager.
func registerService(o InstallOptions) error {
	exe := o.Exe
	if exe == "" {
		var err error
		if exe, err = exePath(); err != nil {
			return err
		}
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: connect to the service manager (elevation needed): %w", err)
	}
	defer m.Disconnect()

	cfg := mgr.Config{
		DisplayName:      DisplayName,
		Description:      Description,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: "", // LocalSystem
	}
	s, err := m.OpenService(Name)
	if err == nil {
		defer s.Close()
		cur, qerr := s.Config()
		if qerr != nil {
			return qerr
		}
		cfg.BinaryPathName = binaryPath(exe, o.Args)
		cfg.ServiceType, cfg.LoadOrderGroup, cfg.TagId, cfg.Dependencies = cur.ServiceType, cur.LoadOrderGroup, cur.TagId, cur.Dependencies
		if err := s.UpdateConfig(cfg); err != nil {
			return fmt.Errorf("service: update: %w", err)
		}
	} else {
		s, err = m.CreateService(Name, exe, cfg, o.Args...)
		if err != nil {
			return fmt.Errorf("service: create: %w", err)
		}
		defer s.Close()
	}
	if err := applyRecovery(s); err != nil {
		return err
	}
	return nil
}

// applyRecovery sets the restart-on-failure actions and the pre-shutdown
// budget of Wanted.
func applyRecovery(s *mgr.Service) error {
	w := Wanted()
	acts := make([]mgr.RecoveryAction, 0, len(w.Restarts))
	for _, d := range w.Restarts {
		acts = append(acts, mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: d})
	}
	if err := s.SetRecoveryActions(acts, uint32(w.ResetPeriod/time.Second)); err != nil {
		return fmt.Errorf("service: recovery actions: %w", err)
	}
	if err := setPreshutdownTimeout(s.Handle, uint32(w.PreShutdown/time.Millisecond)); err != nil {
		return fmt.Errorf("service: pre-shutdown timeout: %w", err)
	}
	return nil
}

// EnsureConfig is called by the service at every start (it runs as
// LocalSystem, which may reconfigure itself): it reads the service's start
// type, recovery actions and pre-shutdown budget, and when any of them
// differs from Wanted (a rolled-back MSI upgrade restores the service without
// them; an administrator may have changed them) re-applies them. It returns
// the settings it repaired (Drift's phrases) so the caller can say so loudly;
// the changes take effect from the next start. Nothing else is touched.
func EnsureConfig() ([]string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("service: connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return nil, fmt.Errorf("service: open: %w", err)
	}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return nil, fmt.Errorf("service: query config: %w", err)
	}
	cur := Settings{AutoStart: cfg.StartType == mgr.StartAutomatic, DelayedAutoStart: cfg.DelayedAutoStart}
	acts, err := s.RecoveryActions()
	if err != nil {
		return nil, fmt.Errorf("service: query recovery actions: %w", err)
	}
	for _, a := range acts {
		if a.Type != mgr.ServiceRestart {
			cur.Restarts = append(cur.Restarts, -1) // not a restart: never equal to Wanted
			continue
		}
		cur.Restarts = append(cur.Restarts, a.Delay)
	}
	reset, err := s.ResetPeriod()
	if err != nil {
		return nil, fmt.Errorf("service: query reset period: %w", err)
	}
	cur.ResetPeriod = time.Duration(reset) * time.Second
	pre, err := preshutdownTimeout(s.Handle)
	if err != nil {
		return nil, fmt.Errorf("service: query pre-shutdown timeout: %w", err)
	}
	cur.PreShutdown = time.Duration(pre) * time.Millisecond

	drift := Drift(cur)
	if len(drift) == 0 {
		return nil, nil
	}
	if !cfg.DelayedAutoStart || cfg.StartType != mgr.StartAutomatic {
		cfg.StartType, cfg.DelayedAutoStart = mgr.StartAutomatic, true
		if err := s.UpdateConfig(cfg); err != nil {
			return drift, fmt.Errorf("service: update: %w", err)
		}
	}
	if err := applyRecovery(s); err != nil {
		return drift, err
	}
	return drift, nil
}

func preshutdownTimeout(h windows.Handle) (uint32, error) {
	var info struct{ PreshutdownTimeout uint32 }
	var needed uint32
	err := windows.QueryServiceConfig2(h, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &needed)
	return info.PreshutdownTimeout, err
}

// eventSourceKey is the registry key of the Event Log source.
const eventSourceKey = `SYSTEM\CurrentControlSet\Services\EventLog\Application\` + EventSource

// sourceRegistered reports whether the Event Log source's registry key exists
// (a read, so it needs no elevation).
func sourceRegistered() (bool, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, eventSourceKey, registry.QUERY_VALUE)
	if err == nil {
		k.Close()
		return true, nil
	}
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// EnsureEventSource registers the Event Log source "CryoBackup" if it is not
// registered yet (elevated), with the running cryo-agent.exe as its
// EventMessageFile: the executable carries a message table with the text "%1"
// for every id in EventIDs (L-EVT-1). A source that already exists is
// success, so Install is idempotent (FIX19); one that names another message
// file (EventCreate.exe, from an older agent) is pointed at this executable.
// "install" and "enroll" call it before a --repair-acl quarantine so that
// Event Log 1007 has somewhere to go (FIX20).
func EnsureEventSource() error {
	exe, err := exePath()
	if err != nil {
		return fmt.Errorf("service: event log source: %w", err)
	}
	err = ensureSource(sourceRegistered, func() error {
		return eventlog.Install(EventSource, exe, false, eventlog.Error|eventlog.Warning|eventlog.Info)
	})
	if err == nil {
		err = ensureMessageFile(exe)
	}
	if err != nil {
		return fmt.Errorf("service: event log source: %w", err)
	}
	return nil
}

// registeredMessageFile is the source's EventMessageFile with environment
// variables expanded (a read: no elevation needed).
func registeredMessageFile() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, eventSourceKey, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("EventMessageFile")
	if err != nil {
		return "", err
	}
	return registry.ExpandString(v)
}

// ensureMessageFile points the registered source's EventMessageFile at exe
// unless it already names it (case-insensitive, after expansion); only a
// change opens the key for writing.
func ensureMessageFile(exe string) error {
	if cur, err := registeredMessageFile(); err == nil && strings.EqualFold(filepath.Clean(cur), filepath.Clean(exe)) {
		return nil
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, eventSourceKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("set the message file: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue("EventMessageFile", exe); err != nil {
		return fmt.Errorf("set the message file: %w", err)
	}
	return nil
}

// Uninstall stops (best effort) and deletes the service and the Event Log
// source. A service that does not exist is not an error.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service: connect to the service manager (elevation needed): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(Name)
	if err != nil {
		return nil
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if st, err := s.Query(); err != nil || st.State == svc.Stopped {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("service: delete: %w", err)
	}
	_ = eventlog.Remove(EventSource)
	return nil
}

// State returns the service state as text ("running", "stopped", "not installed",
// ...) using only query rights, so an unelevated "status" works.
func State() string {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "unknown"
	}
	defer windows.CloseServiceHandle(scm)
	name, _ := windows.UTF16PtrFromString(Name)
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return "not installed"
		}
		return "unknown"
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return "unknown"
	}
	switch st.CurrentState {
	case windows.SERVICE_RUNNING:
		return "running"
	case windows.SERVICE_STOPPED:
		return "stopped"
	case windows.SERVICE_START_PENDING:
		return "starting"
	case windows.SERVICE_STOP_PENDING:
		return "stopping"
	case windows.SERVICE_PAUSED, windows.SERVICE_PAUSE_PENDING, windows.SERVICE_CONTINUE_PENDING:
		return "paused"
	}
	return "unknown"
}

func setPreshutdownTimeout(h windows.Handle, ms uint32) error {
	info := struct{ PreshutdownTimeout uint32 }{ms}
	return windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info)))
}

// exePath is the running executable, absolute.
func exePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// binaryPath renders the service command line the way mgr.CreateService does.
func binaryPath(exe string, args []string) string {
	b := syscall.EscapeArg(exe)
	for _, a := range args {
		b += " " + syscall.EscapeArg(a)
	}
	return b
}

// EventLog mirrors Info, Warning and Error lines to the Windows Event Log
// source "CryoBackup" so an RMM can watch it (architecture §6.5). Messages are
// fixed phrases and classes: the Event Log is machine-wide and not ACL'd like
// the state directory, so no path ever goes there.
type EventLog struct{ l *eventlog.Log }

// OpenEventLog opens the source; it fails if Install has not registered it.
func OpenEventLog() (*EventLog, error) {
	l, err := eventlog.Open(EventSource)
	if err != nil {
		return nil, err
	}
	return &EventLog{l: l}, nil
}

// Log writes one line and drops a write failure (the running agent has its own
// log). level is "info", "warning" or "error".
func (e *EventLog) Log(level string, id uint32, msg string) {
	_ = e.Write(level, id, msg)
}

// Write is Log that returns the failure, for the commands that must not lose a
// security audit line silently ("install" and "enroll" print it to stderr).
func (e *EventLog) Write(level string, id uint32, msg string) error {
	if e == nil || e.l == nil {
		return nil
	}
	switch level {
	case "error":
		return e.l.Error(id, msg)
	case "warning":
		return e.l.Warning(id, msg)
	default:
		return e.l.Info(id, msg)
	}
}

// Close closes the source.
func (e *EventLog) Close() {
	if e != nil && e.l != nil {
		_ = e.l.Close()
	}
}
