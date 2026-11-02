//go:build !windows

package agent

import (
	"os"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// platformFacts is the Linux stand-in: the machine id when there is one, and
// no battery knowledge (CI and development only).
func platformFacts() (osVersion string, class api.DeviceClass, fingerprint string) {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			fingerprint = hexHash("machine-id", id)
		}
	}
	return "", api.DeviceClassUnknown, fingerprint
}

// discoverEnvironment returns no facts off Windows: presets cannot expand and
// explicit roots are used as configured (policy.Roots reports a selection that
// needs facts the environment did not supply).
func discoverEnvironment() policy.Environment { return policy.Environment{} }

// readPower reports nothing off Windows.
func readPower() power { return power{} }
