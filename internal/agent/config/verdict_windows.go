//go:build windows

package config

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// Where the verdict lives on Windows (see Verdict).
const (
	// VerdictKey is the key under HKEY_LOCAL_MACHINE.
	VerdictKey = `SOFTWARE\CryoBackup\Agent`
	// VerdictValuePrefix starts every verdict's REG_SZ value name; the state
	// directory's VerdictID follows it after a hyphen.
	VerdictValuePrefix = "UntrustedRepairRequired"
	// VerdictSource names the store in messages and "status".
	VerdictSource = "HKLM verdict"
)

// VerdictValueName is the REG_SZ value of root's verdict:
// UntrustedRepairRequired-<VerdictID(root)>.
func VerdictValueName(root string) string { return VerdictValuePrefix + "-" + VerdictID(root) }

// VerdictLocation names where root's verdict is kept, without root's path
// (for "status"): HKLM\SOFTWARE\CryoBackup\Agent\UntrustedRepairRequired-<id>.
func VerdictLocation(root string) string { return `HKLM\` + VerdictKey + `\` + VerdictValueName(root) }

// regVerdict is a verdict store in the registry.
type regVerdict struct {
	root registry.Key
	path string
}

// verdictStore is HKLM\SOFTWARE\CryoBackup\Agent. This package's tests
// replace it with a throwaway key under HKCU to exercise the read and write
// mechanics without elevation.
var verdictStore = regVerdict{root: registry.LOCAL_MACHINE, path: VerdictKey}

// ReadVerdict reads root's verdict; it needs no privilege
// (registry.QUERY_VALUE). An absent key or value is no verdict; a value of
// another type, or one that does not parse, still stands; any other failure
// is an error (the caller refuses).
func ReadVerdict(root string) (Verdict, bool, error) {
	return verdictStore.read(VerdictValueName(root))
}

// WriteVerdict records root's verdict unless one already exists (the first
// one and its stamp stand). Only an elevated process can: HKLM refuses an
// unprivileged writer (registry.CreateKey with registry.SET_VALUE).
func WriteVerdict(root string, v Verdict) error {
	return verdictStore.write(VerdictValueName(root), v)
}

// ClearVerdict deletes root's verdict value (elevated only); an absent key or
// value is not an error. The key and other directories' verdicts stay.
func ClearVerdict(root string) error { return verdictStore.clear(VerdictValueName(root)) }

func (s regVerdict) read(name string) (Verdict, bool, error) {
	k, err := registry.OpenKey(s.root, s.path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return Verdict{}, false, nil
	}
	if err != nil {
		return Verdict{}, false, fmt.Errorf("config: open the untrusted verdict key: %w", err)
	}
	defer k.Close()
	val, typ, err := k.GetStringValue(name)
	switch {
	case errors.Is(err, registry.ErrNotExist):
		return Verdict{}, false, nil
	case errors.Is(err, registry.ErrUnexpectedType):
		return Verdict{Reason: fmt.Sprintf("the stored verdict is not a REG_SZ (type %d)", typ)}, true, nil
	case err != nil:
		return Verdict{}, false, fmt.Errorf("config: read the untrusted verdict: %w", err)
	}
	return parseVerdict(val), true, nil
}

func (s regVerdict) write(name string, v Verdict) error {
	if _, present, err := s.read(name); err != nil {
		return err
	} else if present {
		return nil
	}
	k, _, err := registry.CreateKey(s.root, s.path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("config: open the untrusted verdict key for writing (elevated only): %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(name, formatVerdict(v)); err != nil {
		return fmt.Errorf("config: write the untrusted verdict: %w", err)
	}
	return nil
}

func (s regVerdict) clear(name string) error {
	k, err := registry.OpenKey(s.root, s.path, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: open the untrusted verdict key for writing (elevated only): %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("config: clear the untrusted verdict: %w", err)
	}
	return nil
}
