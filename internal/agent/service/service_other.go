//go:build !windows

package service

// IsService is always false off Windows.
func IsService() (bool, error) { return false, nil }

// Run is not available off Windows.
func Run(Host) error { return ErrUnsupported }

// InstallOptions configures Install (Windows only).
type InstallOptions struct {
	Exe  string
	Args []string
}

// Install is not available off Windows.
func Install(InstallOptions) error { return ErrUnsupported }

// EnsureConfig has no service to check off Windows: nothing to repair.
func EnsureConfig() ([]string, error) { return nil, nil }

// Uninstall is not available off Windows.
func Uninstall() error { return ErrUnsupported }

// State reports that there is no service host off Windows.
func State() string { return "unsupported" }

// EventLog is a no-op off Windows.
type EventLog struct{}

// OpenEventLog returns a no-op log.
func OpenEventLog() (*EventLog, error) { return &EventLog{}, nil }

// EnsureEventSource does nothing off Windows.
func EnsureEventSource() error { return nil }

// Log does nothing.
func (*EventLog) Log(level string, id uint32, msg string) {}

// Write does nothing and never fails.
func (*EventLog) Write(level string, id uint32, msg string) error { return nil }

// Close does nothing.
func (*EventLog) Close() {}
