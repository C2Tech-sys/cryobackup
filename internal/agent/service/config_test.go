package service

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// M1-T4: what a rolled-back MSI upgrade leaves (automatic start, no delayed
// start, no recovery actions, the default pre-shutdown budget) is drift in
// every setting; the configuration Install applies is none.
func TestDrift(t *testing.T) {
	if d := Drift(Wanted()); len(d) != 0 {
		t.Errorf("Wanted drifts from itself: %v", d)
	}
	rolledBack := Settings{AutoStart: true, PreShutdown: 10 * time.Second}
	want := []string{"delayed automatic start", "restart on failure after 1, 5 and 15 minutes", "the 120 s pre-shutdown budget"}
	if d := Drift(rolledBack); !reflect.DeepEqual(d, want) {
		t.Errorf("rolled-back service: drift %v, want %v", d, want)
	}
	for name, mut := range map[string]func(*Settings){
		"manual start":       func(s *Settings) { s.AutoStart = false },
		"not delayed":        func(s *Settings) { s.DelayedAutoStart = false },
		"two restarts":       func(s *Settings) { s.Restarts = s.Restarts[:2] },
		"a different delay":  func(s *Settings) { s.Restarts = []time.Duration{time.Minute, time.Minute, 15 * time.Minute} },
		"a non-restart":      func(s *Settings) { s.Restarts = []time.Duration{time.Minute, 5 * time.Minute, -1} },
		"other reset period": func(s *Settings) { s.ResetPeriod = time.Hour },
		"short pre-shutdown": func(s *Settings) { s.PreShutdown = 3 * time.Minute },
	} {
		s := Wanted()
		s.Restarts = append([]time.Duration(nil), s.Restarts...)
		mut(&s)
		if d := Drift(s); len(d) != 1 {
			t.Errorf("%s: drift %v, want exactly one setting", name, d)
		}
	}
}

func TestDriftReport(t *testing.T) {
	if m := DriftReport(nil, nil); m != "" {
		t.Errorf("nothing repaired: %q, want silence", m)
	}
	m := DriftReport([]string{"delayed automatic start"}, nil)
	if !strings.Contains(m, "had lost delayed automatic start") || !strings.Contains(m, "re-applied") {
		t.Errorf("repaired: %q", m)
	}
	m = DriftReport([]string{"delayed automatic start"}, errors.New("access is denied"))
	if !strings.Contains(m, "could not be repaired: access is denied") || !strings.Contains(m, "cryo-agent install") {
		t.Errorf("repair failed: %q", m)
	}
	if m := DriftReport(nil, errors.New("boom")); !strings.Contains(m, "could not be checked: boom") {
		t.Errorf("check failed: %q", m)
	}
}
