package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if _, err := Load(path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	want := Config{
		ServerURL: "https://console.example:8443", DeviceID: "01JB0000000000000000000001",
		GatewayURL: "https://gw.example:9443", Custody: "escrowed", UpdateRing: "stable",
		Proxy: "http://proxy.example:3128", SPKIPin: "pin", EnrollNonce: strings.Repeat("ab", 16),
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got != want {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if !got.Enrolled() {
		t.Error("a config with a device id is enrolled")
	}
	// No temporary files are left next to it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries after a save", len(entries))
	}
}

func TestLoadToleratesUnknownFieldsAndRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	newer := filepath.Join(dir, "newer.json")
	if err := os.WriteFile(newer, []byte(`{"server_url":"https://c.example","device_id":"X","from_the_future":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(newer); err != nil || c.DeviceID != "X" {
		t.Fatalf("unknown field: %+v %v", c, err)
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{not json C:\secret`), 0o600)
	_, err := Load(bad)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("garbage: %v (the error must not echo file content)", err)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Config
		ok   bool
	}{
		{"empty is fine (not enrolled yet)", Config{}, true},
		{"https console", Config{ServerURL: "https://c.example"}, true},
		{"http console needs the lab flag", Config{ServerURL: "http://c.example"}, false},
		{"http console with the lab flag", Config{ServerURL: "http://c.example", InsecureLab: true}, true},
		{"userinfo in a URL", Config{ServerURL: "https://u:p@c.example"}, false},
		{"gateway with a path", Config{GatewayURL: "https://g.example/x?y=1"}, false},
		{"bad proxy", Config{Proxy: "socks5://p.example"}, false},
		{"bad custody", Config{Custody: "mine"}, false},
		{"customer id", Config{CustomerID: "01JB0000000000000000000002"}, true},
		{"customer id that is not a ULID", Config{CustomerID: "unassigned"}, false},
	} {
		if err := tc.c.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if err := Save(filepath.Join(t.TempDir(), "a.json"), Config{ServerURL: "http://c.example"}); err == nil {
		t.Error("Save accepted an invalid config")
	}
}

func TestLayoutPaths(t *testing.T) {
	l := Layout{Root: filepath.Join("state", "CryoBackup")}
	want := map[string]string{
		l.ConfigFile():  filepath.Join("state", "CryoBackup", "config", "agent.json"),
		l.JournalFile(): filepath.Join("state", "CryoBackup", "journal", "journal.db"),
		l.CacheDir():    filepath.Join("state", "CryoBackup", "cache", "restic"),
		l.TempDir():     filepath.Join("state", "CryoBackup", "tmp"),
		l.AgentLog():    filepath.Join("state", "CryoBackup", "logs", "agent.log"),
		l.RunLog("ID1"): filepath.Join("state", "CryoBackup", "logs", "runs", "ID1.jsonl"),
		l.ExcludeFile(): filepath.Join("state", "CryoBackup", "policy", "excludes.txt"),
	}
	for got, w := range want {
		if got != w {
			t.Errorf("got %q want %q", got, w)
		}
	}
	if DefaultStateDir() == "" {
		t.Error("no default state dir")
	}
}

func TestEnsureRefusesAnEmptyRoot(t *testing.T) {
	if _, err := (Layout{}).Ensure(EnsureOptions{}); err == nil {
		t.Error("an empty state directory was accepted")
	}
}

func TestCheckStateSDDL(t *testing.T) {
	const sy, ba = "(A;OICI;FA;;;SY)", "(A;OICI;FA;;;BA)"
	user := "S-1-5-21-1000000000-2000000000-3000000000-1001" // a made-up user SID
	for _, tc := range []struct {
		name, sddl string
		ok         bool
	}{
		{"the expected descriptor", SDDLStateDir, true},
		{"as read back (AI flag, no group)", "O:SYD:PAI" + sy + ba, true},
		{"owner Administrators", "O:BAG:SYD:P" + sy + ba, true},
		{"ACEs in the other order", "O:SYD:P" + ba + sy, true},
		{"owner a user (a squatter)", "O:" + user + "G:" + user + "D:P" + sy + ba, false},
		{"not protected (inherits from ProgramData)", "O:SYD:AI" + sy + ba, false},
		{"inherited ACEs", "O:SYD:(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)", false},
		{"an extra ACE for users", "O:SYD:P" + sy + ba + "(A;OICI;0x1200a9;;;BU)", false},
		{"an extra ACE for the squatter", "O:BAD:P" + sy + ba + "(A;OICI;FA;;;" + user + ")", false},
		{"only SYSTEM", "O:SYD:P" + sy, false},
		{"read-only for Administrators", "O:SYD:P" + sy + "(A;OICI;FR;;;BA)", false},
		{"no owner", "D:P" + sy + ba, false},
		{"no DACL", "O:SY", false},
		{"an empty DACL", "O:SYD:P", false},
		{"garbage", "not an sddl", false},
	} {
		err := CheckStateSDDL(tc.sddl)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %q -> %v", tc.name, tc.sddl, err)
		}
		if err != nil && !errors.Is(err, ErrStateUntrusted) {
			t.Errorf("%s: %v is not ErrStateUntrusted", tc.name, err)
		}
	}
}

func TestEnsureCreatesAndRestrictsEveryDirectory(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "parent", "CryoBackup")}
	var restricted []string
	res, err := l.Ensure(EnsureOptions{
		Restrict: func(p string) error { restricted = append(restricted, p); return nil },
		elevated: func() bool { return false }, // create plainly; the hook still runs and decides
	})
	if err != nil || !res.Applied || !res.Created {
		t.Fatalf("Ensure: %+v %v", res, err)
	}
	want := append([]string{l.Root}, l.Children()...)
	if strings.Join(restricted, "|") != strings.Join(want, "|") {
		t.Errorf("restricted %d directories, want root + %d children:\n%v", len(restricted), len(l.Children()), restricted)
	}
	for _, d := range []string{l.ConfigDir(), l.SecretsDir(), l.PolicyDir(), l.JournalDir(), l.CacheDir(), l.TempDir(), l.LogsDir(), l.RunLogsDir()} {
		found := false
		for _, r := range restricted {
			found = found || r == d
		}
		if !found {
			t.Errorf("%s was not restricted", l.rel(d))
		}
	}
	// "Not applied" (not elevated) is reported, not an error; any other hook error is returned.
	res, err = l.Ensure(EnsureOptions{Restrict: func(string) error { return ErrACLNotApplied }})
	if err != nil || res.Applied || res.Created {
		t.Errorf("not applied: %+v %v", res, err)
	}
	boom := errors.New("boom")
	if _, err := l.Ensure(EnsureOptions{Restrict: func(string) error { return boom }}); !errors.Is(err, boom) {
		t.Errorf("a real hook failure was swallowed: %v", err)
	}
}

// link makes a directory link at link pointing to target: a junction on
// Windows (no privilege needed), a symbolic link elsewhere.
func link(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction here (%v: %s)", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
}

func TestEnsureRefusesReparsePoints(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "attacker-owned")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	noHook := EnsureOptions{Restrict: func(string) error { return nil }}

	t.Run("the root", func(t *testing.T) {
		root := filepath.Join(base, "rootlink")
		link(t, target, root)
		if _, err := (Layout{Root: root}).Ensure(noHook); !errors.Is(err, ErrReparsePoint) {
			t.Fatalf("a reparse-point root: %v, want ErrReparsePoint", err)
		}
	})
	t.Run("a child", func(t *testing.T) {
		l := Layout{Root: filepath.Join(base, "state")}
		if err := os.Mkdir(l.Root, 0o700); err != nil {
			t.Fatal(err)
		}
		link(t, target, l.SecretsDir())
		_, err := l.Ensure(noHook)
		if !errors.Is(err, ErrReparsePoint) {
			t.Fatalf("a reparse-point child: %v, want ErrReparsePoint", err)
		}
		if strings.Contains(err.Error(), base) {
			t.Errorf("the error names the path: %v", err)
		}
	})
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("the link target was written to: %d entries", len(entries))
	}
}
