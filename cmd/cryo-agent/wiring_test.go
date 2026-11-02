package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/agent"
	"github.com/c2tech-sys/cryobackup/internal/agent/service"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
)

// The cmd layer's wiring (M1-FIX1 D12): what each subcommand hands to the
// service host, and what it prints when the Event Log cannot be reached. The
// seams in main.go stand in for the service manager, the state preparation and
// the console.

// swap replaces *p with v for the rest of the test.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// "run" under the service manager hands the host the agent's event relay as
// OnEvent (power and session events reach the agent only through it) and the
// service-stop cause.
func TestRunAsServiceWiresTheEventRelay(t *testing.T) {
	var host service.Host
	called := false
	swap(t, &isService, func() (bool, error) { return true, nil })
	swap(t, &openEventLog, func() (*service.EventLog, error) { return nil, errors.New("test: no event log") })
	swap(t, &runService, func(h service.Host) error { host, called = h, true; return nil })

	if code, _, errs := do("run", "--state-dir", filepath.Join(t.TempDir(), "state")); code != exitOK {
		t.Fatalf("run as a service: %d %q", code, errs)
	}
	if !called {
		t.Fatal("the service host was never run")
	}
	if host.OnEvent == nil {
		t.Fatal("Host.OnEvent is nil: power and session events would never reach the agent")
	}
	var relay agent.EventRelay
	if got, want := reflect.ValueOf(host.OnEvent).Pointer(), reflect.ValueOf(relay.Handle).Pointer(); got != want {
		t.Error("Host.OnEvent is not EventRelay.Handle")
	}
	host.OnEvent(service.Event{Power: true, Code: service.PBTAPMResumeAutomatic}) // no agent yet: dropped, no panic
	if !errors.Is(host.StopCause, supervisor.ErrServiceStop) {
		t.Errorf("Host.StopCause = %v, want supervisor.ErrServiceStop", host.StopCause)
	}
	// Host.Run is not called: it would prepare a real state directory in
	// service mode (strict ACLs, and on an elevated test run the HKLM verdict).
	if host.Run == nil {
		t.Fatal("Host.Run is nil")
	}
}

// "install" prints the Event Log failure that PrepareInstallState reports,
// whether or not the state preparation itself fails.
func TestInstallPrintsTheEventLogFailure(t *testing.T) {
	const line = "event log: the CryoBackup source could not be registered or opened (test)"
	for _, prepErr := range []error{nil, errors.New("test: state refused")} {
		swap(t, &prepareInstallState, func(string, bool) (agent.StateReport, error) {
			return agent.StateReport{EventLogErr: line}, prepErr
		})
		swap(t, &installService, func(service.InstallOptions) error { return nil })
		code, _, errs := do("install", "--state-dir", filepath.Join(t.TempDir(), "state"), "--repair-acl")
		if !strings.Contains(errs, line) {
			t.Errorf("install (prepare error %v): stderr %q does not carry the Event Log failure", prepErr, errs)
		}
		if want := map[bool]int{true: exitOK, false: exitConfig}[prepErr == nil]; code != want {
			t.Errorf("install (prepare error %v): exit %d, want %d", prepErr, code, want)
		}
	}
}

// "enroll" prints the Event Log failure that Enroll reports, whether or not the
// enrollment itself fails.
func TestEnrollPrintsTheEventLogFailure(t *testing.T) {
	const line = "event log: the CryoBackup source could not be registered or opened (test)"
	for _, enrollErr := range []error{nil, errors.New("test: enrollment refused")} {
		swap(t, &enrollDevice, func(context.Context, agent.EnrollOptions) (agent.EnrollResult, error) {
			return agent.EnrollResult{EventLogErr: line, DeviceID: "dev"}, enrollErr
		})
		code, _, errs := do("enroll", "--state-dir", filepath.Join(t.TempDir(), "state"), "--server", "https://console.example.invalid", "--token", "t", "--repair-acl")
		if !strings.Contains(errs, line) {
			t.Errorf("enroll (error %v): stderr %q does not carry the Event Log failure", enrollErr, errs)
		}
		if want := map[bool]int{true: exitOK, false: exitConfig}[enrollErr == nil]; code != want {
			t.Errorf("enroll (error %v): exit %d, want %d", enrollErr, code, want)
		}
	}
}
