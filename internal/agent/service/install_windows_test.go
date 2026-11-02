//go:build windows

package service

import (
	"errors"
	"testing"
)

// Install registers the Event Log source after the service (FIX19/FIX20: the
// source must exist for Event Log 1007), and not when the service step fails.
// The service manager step is replaced, so this runs unelevated (M1-FIX1 D15).
func TestInstallEnsuresTheEventSource(t *testing.T) {
	oldInstall, oldEnsure := installService, ensureEventSource
	t.Cleanup(func() { installService, ensureEventSource = oldInstall, oldEnsure })

	var steps []string
	svcErr, srcErr := error(nil), error(nil)
	installService = func(o InstallOptions) error {
		steps = append(steps, "service")
		if len(o.Args) != 1 || o.Args[0] != "run" {
			t.Errorf("options not passed through: %+v", o)
		}
		return svcErr
	}
	ensureEventSource = func() error { steps = append(steps, "source"); return srcErr }

	if err := Install(InstallOptions{Args: []string{"run"}}); err != nil {
		t.Fatalf("Install = %v", err)
	}
	if len(steps) != 2 || steps[0] != "service" || steps[1] != "source" {
		t.Fatalf("steps = %v, want [service source]", steps)
	}

	steps, srcErr = nil, errors.New("source refused")
	if err := Install(InstallOptions{Args: []string{"run"}}); !errors.Is(err, srcErr) {
		t.Fatalf("Install with a failing source = %v, want that failure", err)
	}

	steps, svcErr, srcErr = nil, errors.New("scm refused"), nil
	if err := Install(InstallOptions{Args: []string{"run"}}); !errors.Is(err, svcErr) {
		t.Fatalf("Install with a failing service step = %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps after a failed service step = %v, want the source left alone", steps)
	}
}
