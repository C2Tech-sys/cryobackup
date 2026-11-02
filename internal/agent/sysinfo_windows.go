//go:build windows

package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemPowerStatus = kernel32.NewProc("GetSystemPowerStatus")
	procGetFirmwareTable     = kernel32.NewProc("GetSystemFirmwareTable")
)

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// batteryPresent: BatteryFlag 128 is "no system battery", 255 "unknown".
func batteryPresent(s systemPowerStatus) bool { return s.BatteryFlag != 128 && s.BatteryFlag != 255 }

func powerStatus() (systemPowerStatus, bool) {
	var s systemPowerStatus
	r, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s)))
	return s, r != 0
}

// readPower reports AC and battery percent; a machine without a battery
// reports nothing (the fields stay "not known").
func readPower() power {
	var p power
	s, ok := powerStatus()
	if !ok {
		return p
	}
	if s.ACLineStatus == 0 || s.ACLineStatus == 1 {
		ac := s.ACLineStatus == 1
		p.OnAC = &ac
	}
	if batteryPresent(s) && s.BatteryLifePercent <= 100 {
		pct := int(s.BatteryLifePercent)
		p.BatteryPct = &pct
	}
	return p
}

// platformFacts reads the OS version (RtlGetVersion), the device class (a
// battery makes it a laptop) and the hardware fingerprint (SMBIOS system UUID
// and board identifiers, falling back to the machine GUID).
func platformFacts() (osVersion string, class api.DeviceClass, fingerprint string) {
	class = api.DeviceClassDesktop
	if v := windows.RtlGetVersion(); v != nil {
		osVersion = fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	}
	if s, ok := powerStatus(); ok {
		if batteryPresent(s) {
			class = api.DeviceClassLaptop
		}
	} else {
		class = api.DeviceClassUnknown
	}
	if parts := smbiosParts(); len(parts) > 0 {
		fingerprint = hexHash(append([]string{"smbios"}, parts...)...)
	} else if g := machineGUID(); g != "" {
		fingerprint = hexHash("machine-guid", g)
	}
	return osVersion, class, fingerprint
}

func machineGUID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// smbiosParts returns stable identifiers from the SMBIOS tables: the system
// UUID (unless it is all zeros or ones) and the system and baseboard serial
// numbers and product names.
func smbiosParts() []string {
	const rsmb = 0x52534D42 // 'RSMB'
	n, _, _ := procGetFirmwareTable.Call(rsmb, 0, 0, 0)
	if n == 0 || n > 4<<20 {
		return nil
	}
	buf := make([]byte, n)
	r, _, _ := procGetFirmwareTable.Call(rsmb, 0, uintptr(unsafe.Pointer(&buf[0])), n)
	if r == 0 || r > n || r < 8 {
		return nil
	}
	data := buf[8:r] // RawSMBIOSData header: 4 version bytes and a 32-bit length
	var parts []string
	for off := 0; off+4 <= len(data); {
		typ, length := data[off], int(data[off+1])
		if length < 4 || off+length > len(data) {
			break
		}
		formatted := data[off : off+length]
		end := off + length
		for end+1 < len(data) && !(data[end] == 0 && data[end+1] == 0) {
			end++
		}
		strs := splitStrings(data[off+length : min(end+1, len(data))])
		str := func(i int) string {
			if i >= len(formatted) {
				return ""
			}
			idx := int(formatted[i])
			if idx == 0 || idx > len(strs) {
				return ""
			}
			return strings.TrimSpace(strs[idx-1])
		}
		switch typ {
		case 1: // System Information
			if length >= 25 {
				uuid := formatted[8:24]
				if !allEqual(uuid, 0x00) && !allEqual(uuid, 0xFF) {
					parts = append(parts, fmt.Sprintf("uuid:%x", uuid))
				}
				parts = append(parts, "sys-serial:"+str(7), "sys-product:"+str(5))
			}
		case 2: // Baseboard
			if length >= 8 {
				parts = append(parts, "board-serial:"+str(7), "board-product:"+str(5))
			}
		case 127:
			sort.Strings(parts)
			return parts
		}
		off = end + 2
	}
	sort.Strings(parts)
	return parts
}

func splitStrings(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	return out
}

func allEqual(b []byte, v byte) bool {
	for _, c := range b {
		if c != v {
			return false
		}
	}
	return true
}

// discoverEnvironment reads the facts only the agent can know: the user profile
// directories (registry ProfileList: local, domain and Entra ID accounts, not
// the service profiles), the fixed drives (GetLogicalDrives and GetDriveType),
// the system drive and ProgramData.
func discoverEnvironment() policy.Environment {
	env := policy.Environment{
		SystemDrive: os.Getenv("SystemDrive"),
		ProgramData: os.Getenv("ProgramData"),
	}
	env.ProfileDirs = profileDirs()
	env.FixedDrives = fixedDrives()
	return env
}

func profileDirs() []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`,
		registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return nil
	}
	defer k.Close()
	sids, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, sid := range sids {
		if !strings.HasPrefix(sid, "S-1-5-21-") && !strings.HasPrefix(sid, "S-1-12-1-") {
			continue // service and built-in profiles (SYSTEM, LocalService, NetworkService)
		}
		sk, err := registry.OpenKey(k, sid, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		raw, _, err := sk.GetStringValue("ProfileImagePath")
		_ = sk.Close()
		if err != nil || raw == "" {
			continue
		}
		path, err := registry.ExpandString(raw)
		if err != nil {
			path = raw
		}
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			dirs = append(dirs, strings.TrimRight(path, `\`))
		}
	}
	sort.Strings(dirs)
	return dirs
}

func fixedDrives() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		root, err := syscall.UTF16PtrFromString(letter + `:\`)
		if err != nil {
			continue
		}
		if windows.GetDriveType(root) == windows.DRIVE_FIXED {
			out = append(out, letter)
		}
	}
	return out
}
