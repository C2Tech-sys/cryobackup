package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// M1-FIX3 item 2: boot step 2b. A service start re-applies the service's own
// SCM configuration through ensureServiceConfig and says so in agent.log (and
// Event Log 1009); a console start does not touch it. The step runs before the
// enrollment check, so an unenrolled device (this test's) repairs it too.
func TestBootRepairsServiceConfigInServiceModeOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		service  bool
		repaired []string
		err      error
		calls    int
		want     string
	}{
		{"service mode, drifted", true, []string{"delayed automatic start", "restart on failure"}, nil, 1,
			"had lost delayed automatic start, restart on failure"},
		{"service mode, check failed", true, nil, errors.New("access denied by the fake"), 1,
			"could not be checked: access denied by the fake"},
		{"service mode, nothing drifted", true, nil, nil, 1, ""},
		{"console mode", false, []string{"delayed automatic start"}, nil, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acl := stubElevatedACL(t)
			l := verifiedTree(t, acl)
			calls := 0
			old := ensureServiceConfig
			t.Cleanup(func() { ensureServiceConfig = old })
			ensureServiceConfig = func() ([]string, error) {
				calls++
				return tc.repaired, tc.err
			}
			var logBuf bytes.Buffer
			o := Options{StateDir: l.Root, ServiceMode: tc.service,
				Logger: slog.New(slog.NewTextHandler(&logBuf, nil))}
			if !tc.service {
				o.AllowUnprotectedState = true
			}
			a, err := New(context.Background(), o)
			if a != nil {
				a.Close()
			}
			if !errors.Is(err, ErrNotEnrolled) {
				t.Fatalf("New: %v, want ErrNotEnrolled (the state is verified and empty)", err)
			}
			if calls != tc.calls {
				t.Fatalf("ensureServiceConfig called %d times, want %d", calls, tc.calls)
			}
			got := logBuf.String()
			if tc.want == "" {
				if strings.Contains(got, "service's configuration") {
					t.Errorf("agent.log reports a service configuration change that did not happen: %s", got)
				}
				return
			}
			if !strings.Contains(got, "level=WARN") || !strings.Contains(got, tc.want) {
				t.Errorf("agent.log = %s, want a warning containing %q", got, tc.want)
			}
		})
	}
}
