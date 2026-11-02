package maintain

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

const rcloneConfFixture = `# fixture rclone.conf
[gateway]
type = s3
endpoint = https://gateway.example.invalid:9443
access_key_id = GATEWAYKEY
secret_access_key = GATEWAYSECRET

[admin]
type = s3
provider = Wasabi
access_key_id = ADMINKEY
secret_access_key = ADMINSECRET
endpoint = https://s3.eu-central-2.example.com
no_check_bucket = true

[noendpoint]
type = s3
region = us-east-1
`

func writeRcloneConf(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(p, []byte(rcloneConfFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// M2-W1B: the storage host the outage WARN names comes from the rclone remote
// the worker actually uses, not from the database's default storage target.
func TestStorageHostComesFromTheRcloneRemoteInUse(t *testing.T) {
	conf := writeRcloneConf(t)
	for name, tc := range map[string]struct {
		cfg      Config
		explicit string
		want     string
	}{
		"default remote is admin":    {Config{RcloneConfig: conf}, "", "s3.eu-central-2.example.com"},
		"named remote":               {Config{RcloneConfig: conf, Remote: "admin"}, "", "s3.eu-central-2.example.com"},
		"another remote":             {Config{RcloneConfig: conf, Remote: "gateway"}, "", "gateway.example.invalid"},
		"explicit setting wins":      {Config{RcloneConfig: conf}, "override.example.invalid", "override.example.invalid"},
		"remote without an endpoint": {Config{RcloneConfig: conf, Remote: "noendpoint"}, "", ""},
		"unknown remote":             {Config{RcloneConfig: conf, Remote: "nope"}, "", ""},
		"unreadable config":          {Config{RcloneConfig: filepath.Join(t.TempDir(), "absent.conf")}, "", ""},
	} {
		if got := resolveStorageHost(tc.cfg, tc.explicit); got != tc.want {
			t.Errorf("%s: host = %q, want %q", name, got, tc.want)
		}
	}
}

func TestRcloneEndpointScanKeepsNoCredential(t *testing.T) {
	got := rcloneRemoteEndpoint(strings.NewReader(rcloneConfFixture), "admin")
	if got != "https://s3.eu-central-2.example.com" {
		t.Fatalf("endpoint = %q", got)
	}
	for _, secret := range []string{"ADMINKEY", "ADMINSECRET", "GATEWAYSECRET"} {
		if strings.Contains(got, secret) {
			t.Fatalf("the scan returned credential text: %q", got)
		}
	}
}

// M2-W1B: start-up errors reach stderr as a fixed class sentence, never as
// err.Error(), so an error that wrapped an OS error cannot print a path.
func TestClassTextIsPathFree(t *testing.T) {
	const secretPath = "/srv/clients/Alpha-Dental/master.key"
	for name, err := range map[string]error{
		"config read":      fmt.Errorf("%w: open %s: permission denied", errConfigRead, secretPath),
		"config json":      fmt.Errorf("%w: %s", errConfigJSON, secretPath),
		"checksum":         fmt.Errorf("%w: %s", errChecksumRead, secretPath),
		"restic binary":    fmt.Errorf("%w: %s", ErrResticBinary, secretPath),
		"restic version":   fmt.Errorf("%w: %s", ErrResticVersion, secretPath),
		"config":           cfgErr("ScratchDir %s is odd", secretPath),
		"config no names":  cfgErr("something at %s", secretPath),
		"unclassified":     fmt.Errorf("open %s: no such file", secretPath),
		"config with port": cfgErr("Bucket is required (%s)", secretPath),
	} {
		got := classText(err)
		if strings.Contains(got, secretPath) || strings.Contains(got, "Alpha-Dental") || strings.Contains(got, "/srv") {
			t.Errorf("%s: classText leaked the path: %q", name, got)
		}
		if !strings.HasPrefix(got, "maintain: ") {
			t.Errorf("%s: classText = %q", name, got)
		}
	}
	if got := classText(cfgErr("ScratchDir must be an absolute path")); !strings.Contains(got, "CRYO_MAINTAIN_SCRATCH_DIR") {
		t.Errorf("a ScratchDir error lost its hint: %q", got)
	}
}

// The binary prints the class text on a configuration error, not the error.
func TestRunPrintsClassTextForAConfigError(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"db_path":`+jsonStr(filepath.Join(dir, "c.db"))+`,"master_key_path":`+jsonStr(filepath.Join(dir, "master.key"))+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_MAINTAIN_BUCKET", "bucket")
	t.Setenv("CRYO_MAINTAIN_SCRATCH_DIR", "relative/scratch") // a configuration error: not absolute
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256", "")
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256_FILE", "")
	var out, errb bytes.Buffer
	keys := func(string, *store.Store) (KeyUnwrapper, error) { return &fakeKeys{pw: "x"}, nil }
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitUsage {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	want := "maintain: invalid configuration: CRYO_MAINTAIN_SCRATCH_DIR must be an absolute directory of its own (not a root or top-level directory, owned by the worker, no symlink in its path: use /run/cryo-maintain)\n"
	if errb.String() != want {
		t.Fatalf("stderr = %q, want %q", errb.String(), want)
	}
}
