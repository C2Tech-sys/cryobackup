package restic

import (
	"strings"
	"testing"
	"time"
)

// metadataDroppingFlags are the restic 0.19.1 options under which a backup
// would not carry the NTFS metadata of the files (matrix r52): stdin backups
// store a stream without the file's security descriptor, attributes, creation
// time or extended attributes, and the xattr filters exist on restore only (a
// backup must never be steered by them). restic 0.19.1 has no backup flag that
// switches the Windows metadata off (backup.go, Backup); this list keeps the
// rendering honest if one is ever added to BackupOptions.
var metadataDroppingFlags = []string{
	"--stdin", "--stdin-from-command", "--stdin-filename",
	"--exclude-xattr", "--include-xattr",
}

// TestBackupKeepsNTFSMetadata renders a backup with every option set and
// checks that no argument disables or filters the Windows metadata restic
// stores by default.
func TestBackupKeepsNTFSMetadata(t *testing.T) {
	cmd, err := Backup(BackupOptions{
		Common:            Common{RetryLock: 2 * time.Minute, LimitUpload: 100, LimitDownload: 200},
		Host:              "dev1",
		Tags:              StandardTags("01JB0000000000000000000C01", "01JB0000000000000000000P01", 3, "01JB0000000000000000000R01"),
		VSS:               &VSSOptions{Timeout: 120 * time.Second, ExcludeAllMountPoints: true, ExcludeVolumes: []string{`d:\`}},
		ExcludeCloudFiles: true,
		IExcludeFile:      `C:\ProgramData\CryoBackup\policy\excludes.txt`,
		IExcludes:         []string{"*.tmp"},
		ExcludeCaches:     true,
		ExcludeLargerThan: 1 << 30,
		ExcludeIfPresent:  []string{".nobackup"},
		ReadConcurrency:   4,
		PackSizeMiB:       16,
		Compression:       "auto",
		SkipIfUnchanged:   true,
		Force:             true,
		Roots:             []string{`C:\fixture\tree`},
	})
	if err != nil {
		t.Fatal(err)
	}
	roots := len(cmd.Args)
	for i, a := range cmd.Args {
		if a == "--" {
			roots = i
			break
		}
	}
	for _, a := range cmd.Args[:roots] {
		for _, bad := range metadataDroppingFlags {
			if a == bad || strings.HasPrefix(a, bad+"=") {
				t.Errorf("the backup command carries %s, which drops or filters NTFS metadata:\n%s", a, strings.Join(cmd.Args, " "))
			}
		}
	}
	if !contains(cmd.Args, "--use-fs-snapshot") {
		t.Error("the VSS option was not rendered; the test does not cover the snapshot path")
	}
}
