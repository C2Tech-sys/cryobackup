package service

import (
	"errors"
	"testing"
)

// FIX19: an Event Log source that already exists is success, decided on the
// registry key and not on error text.
func TestEnsureSource(t *testing.T) {
	boom := errors.New("access is denied")
	existsErr := errors.New("registry unreadable")
	tests := []struct {
		name        string
		exists      []bool // successive answers of exists
		existsErr   error
		installErr  error
		wantInstall int
		wantErr     error
	}{
		{name: "absent: installed", exists: []bool{false}, wantInstall: 1},
		{name: "already registered: no install call", exists: []bool{true}, wantInstall: 0},
		// install reports "already exists" in words the code never inspects;
		// the second look at the key is what turns it into success.
		{name: "install lost a race, key now present", exists: []bool{false, true}, installErr: errors.New("registry key already exists"), wantInstall: 1},
		{name: "install fails and the key is still absent", exists: []bool{false, false}, installErr: boom, wantInstall: 1, wantErr: boom},
		{name: "exists unreadable, install works", existsErr: existsErr, wantInstall: 1},
		{name: "exists unreadable, install fails", existsErr: existsErr, installErr: boom, wantInstall: 1, wantErr: boom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls, installs := 0, 0
			err := ensureSource(
				func() (bool, error) {
					if tc.existsErr != nil {
						return false, tc.existsErr
					}
					i := calls
					calls++
					if i >= len(tc.exists) {
						i = len(tc.exists) - 1
					}
					return tc.exists[i], nil
				},
				func() error { installs++; return tc.installErr },
			)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
			if installs != tc.wantInstall {
				t.Errorf("install ran %d times, want %d", installs, tc.wantInstall)
			}
		})
	}
}

// Twice is the point of FIX19: the second ensure must not run install at all
// once the first one registered the source.
func TestEnsureSourceTwice(t *testing.T) {
	registered, installs := false, 0
	exists := func() (bool, error) { return registered, nil }
	install := func() error {
		installs++
		if registered {
			return errors.New("registry key already exists")
		}
		registered = true
		return nil
	}
	for i := 0; i < 3; i++ {
		if err := ensureSource(exists, install); err != nil {
			t.Fatalf("ensure #%d: %v", i+1, err)
		}
	}
	if installs != 1 {
		t.Errorf("install ran %d times for three ensures, want 1", installs)
	}
}
