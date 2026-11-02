//go:build windows

package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// TEST ONLY: useTestVerdictKey points the verdict store at a throwaway key
// under HKEY_CURRENT_USER (HKCU\Software\CryoBackup-test-<random>), which an
// unelevated test process may write, so the registry read and write mechanics
// are exercised without elevation. The production store is always
// HKLM\SOFTWARE\CryoBackup\Agent; the key is deleted after the test.
func useTestVerdictKey(t *testing.T) regVerdict {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	s := regVerdict{root: registry.CURRENT_USER, path: `Software\CryoBackup-test-` + hex.EncodeToString(b[:])}
	old := verdictStore
	verdictStore = s
	t.Cleanup(func() {
		verdictStore = old
		if err := registry.DeleteKey(s.root, s.path); err != nil && !errors.Is(err, registry.ErrNotExist) {
			t.Errorf("remove the test key HKCU\\%s: %v", s.path, err)
		}
	})
	return s
}

func TestVerdictRegistryMechanicsHKCU(t *testing.T) {
	s := useTestVerdictKey(t)
	root := filepath.Join(t.TempDir(), "CryoBackup")
	at := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)

	if _, present, err := ReadVerdict(root); err != nil || present {
		t.Fatalf("no key: present=%v %v, want no verdict", present, err)
	}
	if err := ClearVerdict(root); err != nil {
		t.Fatalf("clearing without a key: %v", err)
	}
	if err := WriteVerdict(root, Verdict{Since: at, Reason: "the state root: owner S-1-5-21-1"}); err != nil {
		t.Fatalf("WriteVerdict: %v", err)
	}
	name := VerdictValueName(root)
	if !strings.HasPrefix(name, "UntrustedRepairRequired-") || len(name) != len("UntrustedRepairRequired-")+16 {
		t.Fatalf("value name %q, want UntrustedRepairRequired-<16 hex>", name)
	}
	k, err := registry.OpenKey(s.root, s.path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	raw, typ, err := k.GetStringValue(name)
	k.Close()
	if err != nil || typ != registry.SZ || raw != "2026-10-22T15:30:00Z|the state root: owner S-1-5-21-1" {
		t.Fatalf("stored %s = %q (type %d, %v), want REG_SZ <utc stamp>|<reason>", name, raw, typ, err)
	}
	v, present, err := ReadVerdict(root)
	if err != nil || !present || !v.Since.Equal(at) || v.Reason != "the state root: owner S-1-5-21-1" {
		t.Fatalf("ReadVerdict: %+v %v %v", v, present, err)
	}
	// The first verdict and its stamp stand.
	if err := WriteVerdict(root, Verdict{Since: at.Add(time.Hour), Reason: "later"}); err != nil {
		t.Fatalf("a second WriteVerdict: %v", err)
	}
	if v, _, _ := ReadVerdict(root); !v.Since.Equal(at) || v.Reason == "later" {
		t.Errorf("a second write replaced the verdict: %+v", v)
	}
	if err := ClearVerdict(root); err != nil {
		t.Fatalf("ClearVerdict: %v", err)
	}
	if _, present, err := ReadVerdict(root); err != nil || present {
		t.Fatalf("after ClearVerdict: present=%v %v", present, err)
	}
	if err := ClearVerdict(root); err != nil {
		t.Errorf("clearing an absent value: %v", err)
	}

	// Whatever is stored stands: another type, or a string that does not parse.
	k, _, err = registry.CreateKey(s.root, s.path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetDWordValue(name, 1); err != nil {
		t.Fatal(err)
	}
	if v, present, err := ReadVerdict(root); err != nil || !present || !strings.Contains(v.Reason, "not a REG_SZ") {
		t.Errorf("a DWORD verdict: %+v %v %v, want it standing", v, present, err)
	}
	if err := k.SetStringValue(name, "garbage"); err != nil {
		t.Fatal(err)
	}
	if v, present, err := ReadVerdict(root); err != nil || !present || !v.Since.IsZero() {
		t.Errorf("an unparseable verdict: %+v %v %v, want it standing", v, present, err)
	}
}

// One verdict per state directory: a refused lab --state-dir does not block
// the default one, and clearing one leaves the other standing.
func TestVerdictPerStateDirectory(t *testing.T) {
	useTestVerdictKey(t)
	base := t.TempDir()
	lab, service := filepath.Join(base, "lab", "state"), filepath.Join(base, "CryoBackup")
	at := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)

	if err := WriteVerdict(lab, Verdict{Since: at, Reason: "lab"}); err != nil {
		t.Fatal(err)
	}
	if _, present, err := ReadVerdict(service); err != nil || present {
		t.Fatalf("the lab directory's verdict blocks the service directory: %v %v", present, err)
	}
	if err := WriteVerdict(service, Verdict{Since: at.Add(time.Hour), Reason: "service"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearVerdict(lab); err != nil {
		t.Fatal(err)
	}
	if _, present, _ := ReadVerdict(lab); present {
		t.Error("the lab verdict survived its clear")
	}
	if v, present, err := ReadVerdict(service); err != nil || !present || v.Reason != "service" {
		t.Errorf("clearing the lab verdict touched the service one: %+v %v %v", v, present, err)
	}
	// The same directory spelled differently is the same verdict.
	if v, present, _ := ReadVerdict(strings.ToUpper(service) + `\.`); !present || v.Reason != "service" {
		t.Errorf("%s spelled differently has no verdict", filepath.Base(service))
	}
	if !strings.HasSuffix(VerdictLocation(service), `HKLM\SOFTWARE\CryoBackup\Agent\UntrustedRepairRequired-`+VerdictID(service)) ||
		strings.Contains(VerdictLocation(service), base) {
		t.Errorf("VerdictLocation = %q (want the value name, never the path)", VerdictLocation(service))
	}
}

// Reading the real HKLM verdict needs no privilege; an unelevated process
// cannot write one (so no unprivileged account can plant a verdict).
func TestVerdictHKLMUnprivileged(t *testing.T) {
	if verdictStore.root != registry.LOCAL_MACHINE || verdictStore.path != `SOFTWARE\CryoBackup\Agent` {
		t.Fatalf("the production store is %v\\%s", verdictStore.root, verdictStore.path)
	}
	root := filepath.Join(t.TempDir(), "CryoBackup") // a directory no real verdict names
	_, present, err := ReadVerdict(root)
	if err != nil {
		t.Fatalf("reading HKLM\\%s needs no privilege: %v", VerdictKey, err)
	}
	if canRestrict() {
		t.Skip("elevated: the unprivileged write refusal is checked by an unelevated process")
	}
	if present {
		t.Skip("a verdict already exists for this directory; it is not touched")
	}
	if err := WriteVerdict(root, Verdict{Since: time.Now(), Reason: "planted by an unprivileged test"}); err == nil {
		_ = ClearVerdict(root)
		t.Fatal("an unelevated process wrote the HKLM verdict")
	}
	if _, present, err := ReadVerdict(root); err != nil || present {
		t.Errorf("after the refused write: present=%v %v", present, err)
	}
}

// Elevated only: the real HKLM value, written, read back and cleared, for a
// throwaway state directory (no real directory's verdict is touched).
func TestVerdictHKLMElevated(t *testing.T) {
	if !canRestrict() {
		t.Skip("this process is not elevated: writing HKLM\\SOFTWARE\\CryoBackup\\Agent needs an elevated process (P1-T8 runs it elevated on a VM)")
	}
	root := filepath.Join(t.TempDir(), "CryoBackup")
	at := time.Now().UTC().Truncate(time.Second)
	if err := WriteVerdict(root, Verdict{Since: at, Reason: "elevated test"}); err != nil {
		t.Fatalf("WriteVerdict elevated: %v", err)
	}
	v, present, err := ReadVerdict(root)
	if cerr := ClearVerdict(root); cerr != nil {
		t.Errorf("ClearVerdict elevated: %v", cerr)
	}
	if err != nil || !present || !v.Since.Equal(at) || v.Reason != "elevated test" {
		t.Errorf("read back: %+v %v %v", v, present, err)
	}
	if _, present, err := ReadVerdict(root); err != nil || present {
		t.Errorf("after ClearVerdict: present=%v %v", present, err)
	}
}
