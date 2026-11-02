package maintain

import (
	"regexp"
	"strings"
	"testing"
)

// unitLines returns the non-comment lines of a systemd unit.
func unitLines(src string) []string {
	var out []string
	for _, l := range strings.Split(src, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// TestSystemdUnitsKeepNoDataAtRest (ADR 0008, M1-HIPAA2 item 5): no unit may
// write a core dump, and the maintenance worker's scratch space is its own
// runtime directory under /run (a tmpfs), with no persistent state directory.
func TestSystemdUnitsKeepNoDataAtRest(t *testing.T) {
	for _, u := range []string{"cryo-console.service", "cryo-maintain.service", "rclone-gateway.service"} {
		if !hasLine(unitLines(readDeploy(t, "systemd", u)), "LimitCORE=0") {
			t.Errorf("%s lacks LimitCORE=0", u)
		}
	}
	src := readDeploy(t, "systemd", "cryo-maintain.service")
	mt := unitLines(src)
	for _, bad := range []string{"CRYO_MAINTAIN_STATE_DIR", "cryobackup-maintain"} {
		if strings.Contains(strings.Join(mt, "\n"), bad) {
			t.Errorf("cryo-maintain.service still uses %s", bad)
		}
	}
	if !hasLine(mt, "Environment=CRYO_MAINTAIN_SCRATCH_DIR=/run/cryo-maintain") {
		t.Error("cryo-maintain.service does not point CRYO_MAINTAIN_SCRATCH_DIR at /run/cryo-maintain")
	}
	rd := regexp.MustCompile(`(?m)^RuntimeDirectory=(.*)$`).FindStringSubmatch(src)
	if rd == nil || !strings.Contains(" "+rd[1]+" ", " cryo-maintain ") || !strings.Contains(" "+rd[1]+" ", " cryobackup ") {
		t.Errorf("cryo-maintain.service RuntimeDirectory = %v, want cryobackup (the socket) and cryo-maintain (the scratch tmpfs)", rd)
	}
	readme := readDeploy(t, "systemd", "README.md")
	for _, want := range []string{"LimitCORE=0", "/run/cryo-maintain", "CRYO_MAINTAIN_MAX_STREAMS"} {
		if !strings.Contains(readme, want) {
			t.Errorf("deploy/systemd/README.md lacks %q", want)
		}
	}
}

// TestImageHasNoStateDirectory (M1-HIPAA2 item 6): since ADR 0008 nothing uses
// /state, so the image does not create it.
func TestImageHasNoStateDirectory(t *testing.T) {
	for _, name := range []string{"console.Dockerfile", "gateway.Dockerfile"} {
		for _, l := range strings.Split(readDeploy(t, "compose", name), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			if regexp.MustCompile(`(^|\s)/state(\s|$)`).MatchString(l) {
				t.Errorf("%s still creates /state: %q", name, strings.TrimSpace(l))
			}
		}
	}
}
