package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// hostFacts are the device facts sent at enrollment and in heartbeats.
type hostFacts struct {
	Hostname      string
	OS            string
	OSVersion     string
	Arch          api.Arch
	Class         api.DeviceClass
	HWFingerprint string
}

// power is the battery state for the heartbeat; nil fields are "not known" (a
// desktop has no battery).
type power struct {
	OnAC       *bool
	BatteryPct *int
}

func archOf() api.Arch {
	switch runtime.GOARCH {
	case "arm64":
		return api.ArchARM64
	case "386":
		return api.ArchX86
	}
	return api.ArchAMD64
}

// collectHost gathers the facts of this machine.
func collectHost() hostFacts {
	h := hostFacts{OS: runtime.GOOS, Arch: archOf(), Class: api.DeviceClassUnknown}
	h.Hostname = cleanHostname(osHostname())
	h.OSVersion, h.Class, h.HWFingerprint = platformFacts()
	if h.OSVersion == "" {
		h.OSVersion = runtime.GOOS
	}
	if h.HWFingerprint == "" {
		sum := sha256.Sum256([]byte("cryobackup-hw-v1\nhost:" + h.Hostname))
		h.HWFingerprint = hex.EncodeToString(sum[:])
	}
	return h
}

func osHostname() string {
	n, err := os.Hostname()
	if err != nil {
		return ""
	}
	return n
}

// cleanHostname keeps the name a valid wire hostname (no separators, at most
// 253 bytes); a failed lookup becomes "unknown-host".
func cleanHostname(n string) string {
	n = strings.TrimSpace(n)
	n = strings.NewReplacer("/", "-", `\`, "-", " ", "-").Replace(n)
	if n == "" {
		return "unknown-host"
	}
	if len(n) > 253 {
		n = n[:253]
	}
	return n
}

func hexHash(parts ...string) string {
	h := sha256.New()
	h.Write([]byte("cryobackup-hw-v1"))
	for _, p := range parts {
		h.Write([]byte{'\n'})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
