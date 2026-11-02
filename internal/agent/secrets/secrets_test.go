package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
)

const value = "s3cret-value-0123456789"

func TestPlainProtectorRoundTrip(t *testing.T) {
	var p plainProtector
	blob, err := p.protect([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(blob, []byte("CRYO-PLAINTEXT-SECRET")) {
		t.Error("a plaintext file must announce itself")
	}
	got, err := p.unprotect(blob)
	if err != nil || string(got) != value {
		t.Fatalf("round trip: %q %v", got, err)
	}
	if _, err := p.unprotect([]byte("raw dpapi-looking bytes")); err == nil {
		t.Error("a file without the header was accepted as plaintext")
	}
}

func TestStoreBehaviour(t *testing.T) {
	var s *Store
	var err error
	if runtime.GOOS == "windows" {
		s, err = New(Options{Dir: t.TempDir(), AllowUnprotected: true})
	} else {
		s, err = New(Options{Dir: t.TempDir(), InsecurePlaintext: true, AllowUnprotected: true})
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(NameDeviceToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
	if s.Has(NameDeviceToken) {
		t.Error("Has on a missing secret")
	}
	for _, name := range []string{NameDeviceToken, NameGatewayCred, NameRepoKey} {
		if err := s.Put(name, []byte(value+name)); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
		got, err := s.Get(name)
		if err != nil || string(got) != value+name {
			t.Fatalf("get %s: %q %v", name, got, err)
		}
	}
	if err := s.Put(NameDeviceToken, []byte("rotated")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(NameDeviceToken); string(got) != "rotated" {
		t.Errorf("replace: %q", got)
	}
	if err := s.Put("other.txt", []byte(value)); !errors.Is(err, ErrInvalidName) {
		t.Errorf("an unknown name: %v", err)
	}
	if err := s.Put("../escape", []byte(value)); !errors.Is(err, ErrInvalidName) {
		t.Errorf("a path-like name: %v", err)
	}
	if err := s.Put(NameRepoKey, nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("an empty value: %v", err)
	}
	if err := s.Delete(NameRepoKey); err != nil || s.Has(NameRepoKey) {
		t.Errorf("delete: %v", err)
	}
	if err := s.Delete(NameRepoKey); err != nil {
		t.Errorf("deleting a missing secret: %v", err)
	}
}

// The file on disk never holds the secret in the clear on Windows (DPAPI).
func TestDPAPIRoundTripAndFileIsNotPlain(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DPAPI exists only on Windows; the Linux CI run covers the plaintext fallback instead")
	}
	dir := t.TempDir()
	s, err := New(Options{Dir: dir, AllowUnprotected: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Insecure() {
		t.Fatal("the Windows store must be DPAPI")
	}
	if err := s.Put(NameGatewayCred, []byte(value)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, NameGatewayCred))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(value)) {
		t.Fatal("the secret is on disk in the clear")
	}
	got, err := s.Get(NameGatewayCred)
	if err != nil || string(got) != value {
		t.Fatalf("DPAPI round trip: %q %v", got, err)
	}
	// A damaged file is an error that carries no content.
	if err := os.WriteFile(filepath.Join(dir, NameGatewayCred), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(NameGatewayCred); !errors.Is(err, ErrCorrupt) {
		t.Errorf("damaged file: %v", err)
	}
}

func TestPlaintextFallbackNeedsTheFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		if _, err := New(Options{Dir: t.TempDir(), InsecurePlaintext: true}); !errors.Is(err, ErrPlaintextRefused) {
			t.Fatalf("Windows must refuse the plaintext fallback, got %v", err)
		}
		return
	}
	if _, err := New(Options{Dir: t.TempDir()}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("without the flag: %v, want ErrUnsupported", err)
	}
	dir := t.TempDir()
	s, err := New(Options{Dir: dir, InsecurePlaintext: true, AllowUnprotected: true})
	if err != nil || !s.Insecure() {
		t.Fatalf("with the flag: %v", err)
	}
	if err := s.Put(NameDeviceToken, []byte(value)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, NameDeviceToken))
	if !bytes.HasPrefix(raw, []byte("CRYO-PLAINTEXT-SECRET")) {
		t.Error("the fallback file is not clearly marked")
	}
}

func TestACLNeedsElevation(t *testing.T) {
	dir := t.TempDir()
	err := RestrictToSystemAndAdmins(dir)
	if runtime.GOOS != "windows" {
		if !errors.Is(err, config.ErrACLNotApplied) {
			t.Fatalf("off Windows the helper must report ErrACLNotApplied, got %v", err)
		}
		t.Skip("SYSTEM + Administrators ACLs exist only on Windows")
	}
	if !Elevated() {
		if !errors.Is(err, config.ErrACLNotApplied) {
			t.Fatalf("an unelevated process must not touch the ACL (it would lock itself out): %v", err)
		}
		// Nothing changed: the directory is still usable by this process.
		if werr := os.WriteFile(filepath.Join(dir, "probe"), []byte("x"), 0o600); werr != nil {
			t.Fatalf("the directory became unusable: %v", werr)
		}
		t.Skip("the ACL itself is not exercised: this process is not elevated (P1-T8 checks it on a VM)")
	}
	if err != nil {
		t.Fatalf("elevated: %v", err)
	}
	sddl, err := config.ReadOwnerAndDACL(dir) // no-follow handle read (V7 N2)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"O:SY", "D:P", "(A;OICI;FA;;;SY)", "(A;OICI;FA;;;BA)"} {
		if !bytes.Contains([]byte(sddl), []byte(want)) {
			t.Errorf("descriptor %q lacks %q", sddl, want)
		}
	}
	if err := config.CheckStateSDDL(sddl); err != nil {
		t.Errorf("the applied descriptor does not pass the state check: %v", err)
	}
}

func TestLayoutEnsureToleratesNotElevated(t *testing.T) {
	l := config.Layout{Root: filepath.Join(t.TempDir(), "state")}
	res, err := l.Ensure(config.EnsureOptions{Restrict: RestrictToSystemAndAdmins})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Applied && !Elevated() {
		t.Error("reported the ACL as applied without elevation")
	}
	for _, d := range []string{l.ConfigDir(), l.SecretsDir(), l.PolicyDir(), l.JournalDir(), l.CacheDir(), l.TempDir(), l.LogsDir(), l.RunLogsDir()} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("layout directory missing: %s", filepath.Base(d))
		}
	}
	// A hook failure that is not "not applied" is an error (refuse in service mode).
	boom := errors.New("boom")
	if _, err := l.Ensure(config.EnsureOptions{Restrict: func(string) error { return boom }}); !errors.Is(err, boom) {
		t.Errorf("a real hook failure was swallowed: %v", err)
	}
}

// A secret is never written into a directory that does not read back as
// SYSTEM + Administrators only, unless the lab flag says so.
func TestPutRefusesAnUnprotectedDirectory(t *testing.T) {
	plain := runtime.GOOS != "windows"
	dir := t.TempDir() // inherits the user's temp ACL: not protected, on any OS and elevated or not
	s, err := New(Options{Dir: dir, InsecurePlaintext: plain})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Put(NameDeviceToken, []byte(value))
	if !errors.Is(err, ErrUnprotectedDir) {
		t.Fatalf("Put into an unprotected directory: %v, want ErrUnprotectedDir", err)
	}
	if strings.Contains(err.Error(), value) {
		t.Error("the refusal carries the secret")
	}
	if s.Has(NameDeviceToken) {
		t.Fatal("the secret was written although the directory is unprotected")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d files were left in the directory", len(entries))
	}
	// A missing directory is not protected either, and is not created.
	missing := filepath.Join(dir, "secrets")
	s2, _ := New(Options{Dir: missing, InsecurePlaintext: plain})
	if err := s2.Put(NameRepoKey, []byte(value)); !errors.Is(err, ErrUnprotectedDir) {
		t.Errorf("Put into a missing directory: %v", err)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("Put created an unprotected secrets directory")
	}

	// The check is what the directory reads back as: a directory that verifies is written to.
	orig := verifyDir
	t.Cleanup(func() { verifyDir = orig })
	verifyDir = func(string) error { return nil }
	if err := s.Put(NameDeviceToken, []byte(value)); err != nil {
		t.Fatalf("Put into a verified directory: %v", err)
	}
	verifyDir = orig

	// The lab flag writes, and says nothing about the directory.
	lab, _ := New(Options{Dir: t.TempDir(), InsecurePlaintext: plain, AllowUnprotected: true})
	if err := lab.Put(NameGatewayCred, []byte(value)); err != nil {
		t.Fatalf("Put with --insecure-unprotected-state: %v", err)
	}
}
