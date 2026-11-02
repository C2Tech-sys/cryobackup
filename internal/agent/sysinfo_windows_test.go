//go:build windows

package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// Profile enumeration, drive discovery and the fingerprint read the real
// machine; the assertions are the ones that hold on any Windows computer.
func TestDiscoverEnvironmentOnThisMachine(t *testing.T) {
	env := discoverEnvironment()
	if len(env.ProfileDirs) == 0 {
		t.Fatal("no user profile directories found in the registry's ProfileList")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range env.ProfileDirs {
		if strings.EqualFold(d, strings.TrimRight(home, `\`)) {
			found = true
		}
		if strings.HasSuffix(d, `\`) {
			t.Errorf("profile directory %q keeps a trailing separator", d)
		}
		if strings.Contains(strings.ToLower(d), `\systemprofile`) || strings.Contains(strings.ToLower(d), `\serviceprofiles`) {
			t.Errorf("a service profile was listed: %q", d)
		}
	}
	if !found {
		t.Errorf("the current user's profile (%q) is not among %v", home, env.ProfileDirs)
	}
	sys := strings.TrimSuffix(os.Getenv("SystemDrive"), ":")
	hasSys := false
	for _, d := range env.FixedDrives {
		if len(d) != 1 {
			t.Errorf("fixed drive %q is not a bare letter", d)
		}
		if strings.EqualFold(d, sys) {
			hasSys = true
		}
	}
	if !hasSys {
		t.Errorf("the system drive %q is not among the fixed drives %v", sys, env.FixedDrives)
	}
	// The result is what policy.Roots needs for the default selection.
	roots, err := policy.Roots(policy.Selection{Presets: []string{policy.PresetAllUserProfiles}}, env)
	if err != nil || len(roots) != len(env.ProfileDirs) {
		t.Errorf("roots = %v (%v)", roots, err)
	}
	if _, err := policy.Roots(policy.Selection{Presets: []string{policy.PresetFixedDrives}}, env); err != nil {
		t.Errorf("fixed drives preset: %v", err)
	}
}

func TestHostFactsAreUsableOnTheWire(t *testing.T) {
	h := collectHost()
	req := api.EnrollRequest{
		Nonce: strings.Repeat("ab", 16), Hostname: h.Hostname, OS: h.OS, OSVersion: h.OSVersion, Arch: h.Arch,
		DeviceClass: h.Class, HWFingerprint: h.HWFingerprint, AgentVersion: "t", ResticVersion: "0.19.1",
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("the enrollment request built from this machine does not validate: %v", err)
	}
	if h.OS != "windows" || !strings.HasPrefix(h.OSVersion, "10.") && !strings.HasPrefix(h.OSVersion, "6.") {
		t.Errorf("os = %q %q", h.OS, h.OSVersion)
	}
	t.Logf("fingerprint sources: %d SMBIOS parts (0 means the machine GUID fallback)", len(smbiosParts()))
	t.Logf("host: os %s %s, class %s, arch %s, fingerprint %s...", h.OS, h.OSVersion, h.Class, h.Arch, h.HWFingerprint[:12])
	if again := collectHost(); again.HWFingerprint != h.HWFingerprint {
		t.Error("the hardware fingerprint changes between calls")
	}
	if err := api.AssertPathFree(req); err != nil {
		t.Errorf("enrollment request is not path-free: %v", err)
	}
}
