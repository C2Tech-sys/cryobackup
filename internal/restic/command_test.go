package restic

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const snapID = "3ed466cdb2f1a8bb7a1d5c5d2d5b1d2f0f0a1b2c3d4e5f60718293a4b5c6d7e8"

func argsOf(t *testing.T, c Command, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return c.Args
}

func wantArgs(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args differ\n got: %q\nwant: %q", got, want)
	}
}

func TestBackupGolden(t *testing.T) {
	t.Run("fully populated", func(t *testing.T) {
		c, err := Backup(BackupOptions{
			Common: Common{
				RetryLock: 90 * time.Second, LimitUpload: 512, LimitDownload: 2048,
				Extra: append(AdminOptions("rclone", false), "-vv"),
			},
			Host: "dev-123",
			Tags: StandardTags("cust-1", "pol-9", 3, "run-77"),
			VSS: &VSSOptions{
				Timeout: 120 * time.Second, ExcludeAllMountPoints: true,
				ExcludeVolumes: []string{`D:\`, `E:\`},
			},
			ExcludeCloudFiles: true,
			IExcludeFile:      `C:\ProgramData\CryoBackup\policy\excludes.txt`,
			IExcludes:         []string{"*.tmp", "~$*"},
			ExcludeCaches:     true,
			ExcludeLargerThan: 1 << 30,
			ExcludeIfPresent:  []string{".nobackup", "CACHEDIR.TAG:Signature"},
			ReadConcurrency:   4,
			PackSizeMiB:       32,
			Compression:       "auto",
			SkipIfUnchanged:   true,
			Force:             true,
			DryRun:            true,
			Roots:             []string{`C:\`, `D:\Data`},
		})
		if c.Kind != KindBackup {
			t.Fatalf("kind %q", c.Kind)
		}
		wantArgs(t, argsOf(t, c, err), []string{
			"backup", "--json", "--host", "dev-123",
			"--tag", "cb.customer=cust-1", "--tag", "cb.policy=pol-9.3", "--tag", "cb.run=run-77",
			"--use-fs-snapshot", "-o", "vss.timeout=120s", "-o", "vss.exclude-all-mount-points=true",
			"-o", `vss.exclude-volumes=D:\;E:\`,
			"--exclude-cloud-files",
			"--iexclude-file", `C:\ProgramData\CryoBackup\policy\excludes.txt`,
			"--iexclude", "*.tmp", "--iexclude", "~$*",
			"--exclude-caches", "--exclude-larger-than", "1073741824",
			"--exclude-if-present", ".nobackup", "--exclude-if-present", "CACHEDIR.TAG:Signature",
			"--limit-upload", "512", "--limit-download", "2048",
			"--read-concurrency", "4", "--pack-size", "32", "--compression", "auto",
			"--retry-lock", "90s",
			"--skip-if-unchanged", "--no-scan", "--force", "--dry-run",
			"-o", "rclone.program=rclone", "-o", "rclone.args=serve restic --stdio", "-vv",
			"--", `C:\`, `D:\Data`,
		})
	})
	t.Run("minimal", func(t *testing.T) {
		c, err := Backup(BackupOptions{Host: "h", Roots: []string{`C:\Users`}})
		wantArgs(t, argsOf(t, c, err), []string{"backup", "--json", "--host", "h", "--", `C:\Users`})
	})
	t.Run("root that looks like a flag stays after the separator", func(t *testing.T) {
		c, err := Backup(BackupOptions{Host: "h", Roots: []string{"--delete"}})
		wantArgs(t, argsOf(t, c, err), []string{"backup", "--json", "--host", "h", "--", "--delete"})
	})
	t.Run("vss without timeout and volumes", func(t *testing.T) {
		c, err := Backup(BackupOptions{Host: "h", VSS: &VSSOptions{}, Roots: []string{"r"}})
		wantArgs(t, argsOf(t, c, err), []string{"backup", "--json", "--host", "h", "--use-fs-snapshot", "--", "r"})
	})
	t.Run("sub-second retry lock keeps go syntax", func(t *testing.T) {
		c, err := Backup(BackupOptions{Common: Common{RetryLock: 1500 * time.Millisecond}, Host: "h", Roots: []string{"r"}})
		wantArgs(t, argsOf(t, c, err), []string{"backup", "--json", "--host", "h", "--retry-lock", "1.5s", "--", "r"})
	})
}

func TestBackupErrors(t *testing.T) {
	base := func() BackupOptions { return BackupOptions{Host: "h", Roots: []string{"r"}} }
	tests := []struct {
		name string
		mut  func(*BackupOptions)
	}{
		{"no host", func(o *BackupOptions) { o.Host = "" }},
		{"host control char", func(o *BackupOptions) { o.Host = "h\n" }},
		{"no roots", func(o *BackupOptions) { o.Roots = nil }},
		{"empty root", func(o *BackupOptions) { o.Roots = []string{"r", ""} }},
		{"tag comma in value", func(o *BackupOptions) { o.Tags = []Tag{{"k", "a,b"}} }},
		{"tag comma in key", func(o *BackupOptions) { o.Tags = []Tag{{"k,x", "v"}} }},
		{"tag equals in key", func(o *BackupOptions) { o.Tags = []Tag{{"k=x", "v"}} }},
		{"tag empty key", func(o *BackupOptions) { o.Tags = []Tag{{"", "v"}} }},
		{"tag empty value", func(o *BackupOptions) { o.Tags = []Tag{{"k", ""}} }},
		{"pack size 3", func(o *BackupOptions) { o.PackSizeMiB = 3 }},
		{"pack size 129", func(o *BackupOptions) { o.PackSizeMiB = 129 }},
		{"pack size negative", func(o *BackupOptions) { o.PackSizeMiB = -4 }},
		{"bad compression", func(o *BackupOptions) { o.Compression = "ultra" }},
		{"negative exclude-larger-than", func(o *BackupOptions) { o.ExcludeLargerThan = -1 }},
		{"negative read concurrency", func(o *BackupOptions) { o.ReadConcurrency = -1 }},
		{"empty iexclude", func(o *BackupOptions) { o.IExcludes = []string{""} }},
		{"empty exclude-if-present", func(o *BackupOptions) { o.ExcludeIfPresent = []string{""} }},
		{"negative vss timeout", func(o *BackupOptions) { o.VSS = &VSSOptions{Timeout: -time.Second} }},
		{"semicolon in vss volume", func(o *BackupOptions) { o.VSS = &VSSOptions{ExcludeVolumes: []string{"D:;E:"}} }},
		{"negative retry lock", func(o *BackupOptions) { o.RetryLock = -time.Second }},
		{"negative limit", func(o *BackupOptions) { o.LimitUpload = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := base()
			tt.mut(&o)
			c, err := Backup(o)
			if err == nil {
				t.Fatalf("got %q, want error", c.Args)
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error %v does not wrap ErrInvalidOption", err)
			}
			if len(c.Args) != 0 {
				t.Fatal("an erroring builder must return an empty Command")
			}
		})
	}
	for _, size := range []int{4, 16, 128} {
		o := base()
		o.PackSizeMiB = size
		if _, err := Backup(o); err != nil {
			t.Errorf("pack size %d rejected: %v", size, err)
		}
	}
	for _, comp := range []string{"", "auto", "off", "fastest", "better", "max"} {
		o := base()
		o.Compression = comp
		if _, err := Backup(o); err != nil {
			t.Errorf("compression %q rejected: %v", comp, err)
		}
	}
}

func TestStandardTags(t *testing.T) {
	got := StandardTags("cust", "pol", 12, "run-1")
	want := []Tag{{"cb.customer", "cust"}, {"cb.policy", "pol.12"}, {"cb.run", "run-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRestoreGolden(t *testing.T) {
	tests := []struct {
		name string
		opt  RestoreOptions
		want []string
	}{
		{
			name: "minimal",
			opt:  RestoreOptions{SnapshotID: snapID, Target: `C:\restore`},
			want: []string{"restore", "--json", "--target", `C:\restore`, snapID},
		},
		{
			name: "with verify",
			opt: RestoreOptions{
				Common:     Common{RetryLock: 30 * time.Second, LimitDownload: 100},
				SnapshotID: snapID + ":/C/Data", Target: `C:\restore`, Overwrite: "if-newer",
				Includes: []string{"/C/Data/a.docx", "/C/Data/b"}, IncludeFile: `C:\inc.txt`, Verify: true,
			},
			want: []string{
				"restore", "--json", "--target", `C:\restore`, "--overwrite", "if-newer",
				"--include", "/C/Data/a.docx", "--include", "/C/Data/b", "--include-file", `C:\inc.txt`,
				"--verify", "--limit-download", "100", "--retry-lock", "30s", snapID + ":/C/Data",
			},
		},
		{
			name: "simulate",
			opt:  RestoreOptions{SnapshotID: "latest", Target: `C:\restore`, DryRun: true, Overwrite: "never"},
			want: []string{"restore", "--json", "--target", `C:\restore`, "--overwrite", "never", "--dry-run", "-vv", "latest"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Restore(tt.opt)
			if c.Kind != KindRestore {
				t.Fatalf("kind %q", c.Kind)
			}
			wantArgs(t, argsOf(t, c, err), tt.want)
		})
	}
}

func TestRestoreErrors(t *testing.T) {
	ok := func() RestoreOptions { return RestoreOptions{SnapshotID: snapID, Target: "t"} }
	tests := []struct {
		name string
		mut  func(*RestoreOptions)
	}{
		{"verify and dry run", func(o *RestoreOptions) { o.Verify, o.DryRun = true, true }},
		{"no snapshot", func(o *RestoreOptions) { o.SnapshotID = "" }},
		{"snapshot looks like a flag", func(o *RestoreOptions) { o.SnapshotID = "--delete" }},
		{"snapshot too short", func(o *RestoreOptions) { o.SnapshotID = "abc" }},
		{"snapshot not hex", func(o *RestoreOptions) { o.SnapshotID = "zzzzzzzzzz" }},
		{"empty subfolder", func(o *RestoreOptions) { o.SnapshotID = snapID + ":" }},
		{"no target", func(o *RestoreOptions) { o.Target = "" }},
		{"bad overwrite", func(o *RestoreOptions) { o.Overwrite = "sometimes" }},
		{"empty include", func(o *RestoreOptions) { o.Includes = []string{""} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := ok()
			tt.mut(&o)
			if c, err := Restore(o); err == nil {
				t.Fatalf("got %q, want error", c.Args)
			}
		})
	}
	for _, ow := range []string{"", "always", "if-changed", "if-newer", "never"} {
		o := ok()
		o.Overwrite = ow
		if _, err := Restore(o); err != nil {
			t.Errorf("overwrite %q rejected: %v", ow, err)
		}
	}
}

func TestForgetGolden(t *testing.T) {
	t.Run("every keep flag", func(t *testing.T) {
		c, err := Forget(ForgetOptions{
			Common: Common{RetryLock: 5 * time.Minute, Extra: AdminOptions("rclone", true)},
			Host:   "dev-1", GroupBy: "host,paths",
			KeepLast: 1, KeepHourly: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 5, KeepYearly: 6,
			KeepWithin: "1y2m3d4h", KeepTags: []string{"keep", "legal-hold"}, DryRun: true,
		})
		if c.Kind != KindForget {
			t.Fatalf("kind %q", c.Kind)
		}
		wantArgs(t, argsOf(t, c, err), []string{
			"forget", "--json", "--dry-run", "--host", "dev-1", "--group-by", "host,paths",
			"--keep-last", "1", "--keep-hourly", "2", "--keep-daily", "3", "--keep-weekly", "4",
			"--keep-monthly", "5", "--keep-yearly", "6", "--keep-within", "1y2m3d4h",
			"--keep-tag", "keep", "--keep-tag", "legal-hold",
			"--retry-lock", "300s",
			"-o", "rclone.program=rclone", "-o", "rclone.args=serve restic --stdio --s3-bypass-governance-retention",
		})
	})
	t.Run("defaults and unlimited", func(t *testing.T) {
		c, err := Forget(ForgetOptions{Host: "h", KeepYearly: -1, KeepWithin: "30d"})
		wantArgs(t, argsOf(t, c, err), []string{
			"forget", "--json", "--host", "h", "--group-by", "host,paths",
			"--keep-yearly", "unlimited", "--keep-within", "30d",
		})
	})
}

func TestForgetErrors(t *testing.T) {
	ok := func() ForgetOptions { return ForgetOptions{Host: "h", KeepWithin: "30d"} }
	tests := []struct {
		name string
		mut  func(*ForgetOptions)
	}{
		{"no keep-within", func(o *ForgetOptions) { o.KeepWithin = "" }},
		{"keep-within garbage", func(o *ForgetOptions) { o.KeepWithin = "30 days" }},
		{"keep-within wrong order", func(o *ForgetOptions) { o.KeepWithin = "1d1y" }},
		{"keep-within bare number", func(o *ForgetOptions) { o.KeepWithin = "30" }},
		{"keep-within flag injection", func(o *ForgetOptions) { o.KeepWithin = "--prune" }},
		{"no host", func(o *ForgetOptions) { o.Host = "" }},
		{"keep below -1", func(o *ForgetOptions) { o.KeepDaily = -2 }},
		{"bad group-by", func(o *ForgetOptions) { o.GroupBy = "host; rm" }},
		{"keep tag with comma", func(o *ForgetOptions) { o.KeepTags = []string{"a,b"} }},
		{"empty keep tag", func(o *ForgetOptions) { o.KeepTags = []string{""} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := ok()
			tt.mut(&o)
			if c, err := Forget(o); err == nil {
				t.Fatalf("got %q, want error", c.Args)
			}
		})
	}
	for _, w := range []string{"1y", "6m", "30d", "12h", "1y2m", "2m5d", "0d", "1y2m3d4h"} {
		o := ok()
		o.KeepWithin = w
		if _, err := Forget(o); err != nil {
			t.Errorf("keep-within %q rejected: %v", w, err)
		}
	}
}

func TestPruneGolden(t *testing.T) {
	tests := []struct {
		name string
		opt  PruneOptions
		want []string
	}{
		{"bare", PruneOptions{}, []string{"prune"}},
		{
			"all flags",
			PruneOptions{Common: Common{RetryLock: time.Minute}, MaxUnused: "5%", RepackCacheableOnly: true, MaxRepackSize: "500M", DryRun: true},
			[]string{"prune", "--max-unused", "5%", "--repack-cacheable-only", "--max-repack-size", "500M", "--dry-run", "--retry-lock", "60s"},
		},
		{"unlimited", PruneOptions{MaxUnused: "unlimited"}, []string{"prune", "--max-unused", "unlimited"}},
		{"bytes", PruneOptions{MaxUnused: "10G"}, []string{"prune", "--max-unused", "10G"}},
		{"fractional percent", PruneOptions{MaxUnused: "2.5%"}, []string{"prune", "--max-unused", "2.5%"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Prune(tt.opt)
			if c.Kind != KindPrune {
				t.Fatalf("kind %q", c.Kind)
			}
			wantArgs(t, argsOf(t, c, err), tt.want)
			for _, a := range c.Args {
				if a == "--json" {
					t.Fatal("prune has no --json")
				}
			}
		})
	}
	for _, bad := range []PruneOptions{
		{MaxUnused: "lots"}, {MaxUnused: "5 %"}, {MaxUnused: "-1"}, {MaxUnused: "--prune"},
		{MaxRepackSize: "5%"}, {MaxRepackSize: "unlimited"}, {MaxRepackSize: "1.5G"},
	} {
		if c, err := Prune(bad); err == nil {
			t.Errorf("%+v accepted: %q", bad, c.Args)
		}
	}
}

func TestCheckGolden(t *testing.T) {
	tests := []struct {
		name string
		opt  CheckOptions
		want []string
	}{
		{"bare", CheckOptions{}, []string{"check", "--json"}},
		{"fraction", CheckOptions{ReadDataSubset: "1/12", WithCache: true}, []string{"check", "--json", "--read-data-subset", "1/12", "--with-cache"}},
		{"percent", CheckOptions{ReadDataSubset: "2.5%"}, []string{"check", "--json", "--read-data-subset", "2.5%"}},
		{"size", CheckOptions{ReadDataSubset: "10G", Common: Common{RetryLock: 2 * time.Minute}}, []string{"check", "--json", "--read-data-subset", "10G", "--retry-lock", "120s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Check(tt.opt)
			if c.Kind != KindCheck {
				t.Fatalf("kind %q", c.Kind)
			}
			wantArgs(t, argsOf(t, c, err), tt.want)
		})
	}
	for _, bad := range []string{"0/5", "6/5", "a/b", "5", "x%", "10Q", "--with-cache", "1/0"} {
		if c, err := Check(CheckOptions{ReadDataSubset: bad}); err == nil {
			t.Errorf("subset %q accepted: %q", bad, c.Args)
		}
	}
}

func TestReadOnlyCommandsGolden(t *testing.T) {
	common := Common{RetryLock: 30 * time.Second, LimitUpload: 10, LimitDownload: 20, Extra: []string{"-o", "x.y=z"}}
	tail := []string{"--limit-upload", "10", "--limit-download", "20", "--retry-lock", "30s", "-o", "x.y=z"}
	cat := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	tests := []struct {
		name  string
		build func() (Command, error)
		kind  Kind
		want  []string
	}{
		{"snapshots all hosts", func() (Command, error) { return Snapshots("", Common{}) }, KindSnapshots, []string{"snapshots", "--json"}},
		{"snapshots host", func() (Command, error) { return Snapshots("dev-1", common) }, KindSnapshots, cat([]string{"snapshots", "--json", "--host", "dev-1"}, tail)},
		{"ls roots", func() (Command, error) { return Ls(snapID, "", Common{}) }, KindLs, []string{"ls", "--json", snapID, "/"}},
		{"ls dir", func() (Command, error) { return Ls(snapID, "/C/Users", common) }, KindLs, cat([]string{"ls", "--json", snapID, "/C/Users"}, tail)},
		{"ls latest", func() (Command, error) { return Ls("latest", "/C", Common{}) }, KindLs, []string{"ls", "--json", "latest", "/C"}},
		{
			"find minimal", func() (Command, error) { return Find(FindOptions{Pattern: "*.docx"}) }, KindFind,
			[]string{"find", "--json", "*.docx"},
		},
		{
			"find full", func() (Command, error) {
				return Find(FindOptions{Common: common, Pattern: "report*", Host: "dev-1", IgnoreCase: true, Snapshots: []string{snapID, "latest"}})
			}, KindFind,
			cat([]string{"find", "--json", "-H", "dev-1", "--ignore-case", "--snapshot", snapID, "--snapshot", "latest"}, tail, []string{"report*"}),
		},
		{
			"dump", func() (Command, error) { return Dump(snapID, "/C/Data/x", `C:\out\x.zip`, Common{}) }, KindDump,
			[]string{"dump", "-a", "zip", "--target", `C:\out\x.zip`, snapID, "/C/Data/x"},
		},
		{
			"dump subfolder", func() (Command, error) { return Dump(snapID+":/C/Data", "/", `C:\out\x.zip`, common) }, KindDump,
			cat([]string{"dump", "-a", "zip", "--target", `C:\out\x.zip`, snapID + ":/C/Data", "/"}, tail),
		},
		// ADR 0008: the stdout form has no --target, so no file name reaches restic.
		{
			"dump to stdout", func() (Command, error) { return DumpStdout(snapID, "/C/Data/x", Common{}) }, KindDump,
			[]string{"dump", "-a", "zip", snapID, "/C/Data/x"},
		},
		{
			"dump to stdout subfolder", func() (Command, error) { return DumpStdout(snapID+":/C/Data", "/", common) }, KindDump,
			cat([]string{"dump", "-a", "zip", snapID + ":/C/Data", "/"}, tail),
		},
		{"unlock", func() (Command, error) { return Unlock(false, Common{}) }, KindUnlock, []string{"unlock"}},
		{"unlock remove-all", func() (Command, error) { return Unlock(true, common) }, KindUnlock, cat([]string{"unlock", "--remove-all"}, tail)},
		{"cat config", func() (Command, error) { return CatConfig(Common{}) }, KindCatConfig, []string{"cat", "config"}},
		{"cat config common", func() (Command, error) { return CatConfig(common) }, KindCatConfig, cat([]string{"cat", "config"}, tail)},
		{"stats raw", func() (Command, error) { return Stats(StatsRawData, "", Common{}) }, KindStats, []string{"stats", "--json", "--mode", "raw-data"}},
		{"stats restore-size host", func() (Command, error) { return Stats(StatsRestoreSize, "dev-1", common) }, KindStats, cat([]string{"stats", "--json", "--mode", "restore-size", "--host", "dev-1"}, tail)},
		{"stats files-by-contents", func() (Command, error) { return Stats(StatsFilesByContents, "", Common{}) }, KindStats, []string{"stats", "--json", "--mode", "files-by-contents"}},
		{"stats blobs-per-file", func() (Command, error) { return Stats(StatsBlobsPerFile, "", Common{}) }, KindStats, []string{"stats", "--json", "--mode", "blobs-per-file"}},
		{"version", func() (Command, error) { return Version(), nil }, KindVersion, []string{"version", "--json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := tt.build()
			if c.Kind != tt.kind {
				t.Fatalf("kind %q, want %q", c.Kind, tt.kind)
			}
			wantArgs(t, argsOf(t, c, err), tt.want)
		})
	}
}

func TestReadOnlyCommandsErrors(t *testing.T) {
	tests := []struct {
		name  string
		build func() (Command, error)
	}{
		{"stats debug", func() (Command, error) { return Stats("debug", "", Common{}) }},
		{"stats empty mode", func() (Command, error) { return Stats("", "", Common{}) }},
		{"ls bad snapshot", func() (Command, error) { return Ls("--recursive", "/", Common{}) }},
		{"ls relative dir", func() (Command, error) { return Ls(snapID, "C:/Users", Common{}) }},
		{"ls subfolder snapshot not allowed", func() (Command, error) { return Ls(snapID+":/x", "/", Common{}) }},
		{"find no pattern", func() (Command, error) { return Find(FindOptions{}) }},
		{"find flag-like pattern", func() (Command, error) { return Find(FindOptions{Pattern: "--help"}) }},
		{"find bad snapshot", func() (Command, error) { return Find(FindOptions{Pattern: "x", Snapshots: []string{"nope"}}) }},
		{"dump relative path", func() (Command, error) { return Dump(snapID, "x", "t", Common{}) }},
		{"dump no target", func() (Command, error) { return Dump(snapID, "/x", "", Common{}) }},
		{"dump bad snapshot", func() (Command, error) { return Dump("", "/x", "t", Common{}) }},
		{"dump to stdout relative path", func() (Command, error) { return DumpStdout(snapID, "x", Common{}) }},
		{"dump to stdout bad snapshot", func() (Command, error) { return DumpStdout("--target=/etc/x", "/x", Common{}) }},
		{"dump to stdout bad common", func() (Command, error) { return DumpStdout(snapID, "/x", Common{RetryLock: -1}) }},
		{"snapshots host with newline", func() (Command, error) { return Snapshots("a\nb", Common{}) }},
		{"unlock bad common", func() (Command, error) { return Unlock(false, Common{RetryLock: -1}) }},
		{"cat config bad common", func() (Command, error) { return CatConfig(Common{LimitUpload: -1}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := tt.build()
			if err == nil {
				t.Fatalf("got %q, want error", c.Args)
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error %v does not wrap ErrInvalidOption", err)
			}
		})
	}
}

// allCommands builds every command with every option set. emit receives a
// label and the result of each builder call.
func allCommands(extra []string) map[string]Command {
	common := Common{RetryLock: time.Minute, LimitUpload: 1, LimitDownload: 2, Extra: extra}
	out := map[string]Command{}
	add := func(name string, c Command, err error) {
		if err != nil {
			panic(name + ": " + err.Error())
		}
		out[name] = c
	}
	c, err := Backup(BackupOptions{
		Common: common, Host: "h", Tags: StandardTags("c", "p", 1, "r"),
		VSS:               &VSSOptions{Timeout: time.Minute, ExcludeAllMountPoints: true, ExcludeVolumes: []string{"D:"}},
		ExcludeCloudFiles: true, IExcludeFile: "f", IExcludes: []string{"x"}, ExcludeCaches: true,
		ExcludeLargerThan: 5, ExcludeIfPresent: []string{"y"}, ReadConcurrency: 2, PackSizeMiB: 16,
		Compression: "max", SkipIfUnchanged: true, Force: true, DryRun: true, Roots: []string{"r1", "r2"},
	})
	add("backup", c, err)
	c, err = Restore(RestoreOptions{Common: common, SnapshotID: snapID + ":/x", Target: "t", Overwrite: "always", Includes: []string{"/i"}, IncludeFile: "f", Verify: true})
	add("restore verify", c, err)
	c, err = Restore(RestoreOptions{Common: common, SnapshotID: snapID, Target: "t", Overwrite: "never", DryRun: true})
	add("restore dry run", c, err)
	c, err = Snapshots("h", common)
	add("snapshots", c, err)
	c, err = Ls(snapID, "/C", common)
	add("ls", c, err)
	c, err = Find(FindOptions{Common: common, Pattern: "p", Host: "h", IgnoreCase: true, Snapshots: []string{snapID}})
	add("find", c, err)
	c, err = Dump(snapID, "/p", "t", common)
	add("dump", c, err)
	c, err = Unlock(false, common)
	add("unlock", c, err)
	c, err = Unlock(true, common)
	add("unlock remove-all", c, err)
	c, err = CatConfig(common)
	add("cat config", c, err)
	for _, m := range []StatsMode{StatsRestoreSize, StatsFilesByContents, StatsBlobsPerFile, StatsRawData} {
		c, err = Stats(m, "h", common)
		add("stats "+string(m), c, err)
	}
	out["version"] = Version()
	c, err = Forget(ForgetOptions{Common: common, Host: "h", GroupBy: "host,paths", KeepLast: 1, KeepHourly: 1, KeepDaily: 1, KeepWeekly: 1, KeepMonthly: 1, KeepYearly: -1, KeepWithin: "1y", KeepTags: []string{"t"}, DryRun: true})
	add("forget dry run", c, err)
	c, err = Forget(ForgetOptions{Common: common, Host: "h", KeepWithin: "1d"})
	add("forget", c, err)
	c, err = Prune(PruneOptions{Common: common, MaxUnused: "5%", RepackCacheableOnly: true, MaxRepackSize: "1G", DryRun: true})
	add("prune", c, err)
	c, err = Check(CheckOptions{Common: common, ReadDataSubset: "1/5", WithCache: true})
	add("check", c, err)
	return out
}

func TestForbiddenFlagsNeverEmitted(t *testing.T) {
	cmds := allCommands(AdminOptions(`C:\tools\rclone.exe`, true))
	if len(cmds) < 18 {
		t.Fatalf("only %d commands built", len(cmds))
	}
	for name, c := range cmds {
		positional := false
		for _, a := range c.Args {
			if a == "--" {
				positional = true // everything after is a path
				continue
			}
			if positional {
				continue
			}
			if a == "--remove-all" && name == "unlock remove-all" {
				continue
			}
			if forbiddenArg(a) {
				t.Errorf("%s emits forbidden argument %q (args %q)", name, a, c.Args)
			}
		}
		if c.Kind == "" {
			t.Errorf("%s has no kind", name)
		}
	}
	// --remove-all is emitted by Unlock(true) and by nothing else.
	for name, c := range cmds {
		has := false
		for _, a := range c.Args {
			if a == "--remove-all" {
				has = true
			}
		}
		if has != (name == "unlock remove-all") {
			t.Errorf("%s: --remove-all present = %v", name, has)
		}
	}
	// Spot checks on the individual prohibitions.
	if args := cmds["restore verify"].Args; contains(args, "--delete") {
		t.Error("restore emits --delete")
	}
	if args := cmds["forget"].Args; contains(args, "--prune") {
		t.Error("forget emits --prune")
	}
	if args := cmds["ls"].Args; contains(args, "--recursive") {
		t.Error("ls emits --recursive")
	}
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func TestExtraRejectsForbiddenFlags(t *testing.T) {
	builders := map[string]func(Common) (Command, error){
		"backup": func(c Common) (Command, error) {
			return Backup(BackupOptions{Common: c, Host: "h", Roots: []string{"r"}})
		},
		"restore": func(c Common) (Command, error) {
			return Restore(RestoreOptions{Common: c, SnapshotID: snapID, Target: "t"})
		},
		"snapshots": func(c Common) (Command, error) { return Snapshots("", c) },
		"ls":        func(c Common) (Command, error) { return Ls(snapID, "/", c) },
		"find":      func(c Common) (Command, error) { return Find(FindOptions{Common: c, Pattern: "p"}) },
		"dump":      func(c Common) (Command, error) { return Dump(snapID, "/p", "t", c) },
		"dump out":  func(c Common) (Command, error) { return DumpStdout(snapID, "/p", c) },
		"unlock":    func(c Common) (Command, error) { return Unlock(false, c) },
		"cat":       func(c Common) (Command, error) { return CatConfig(c) },
		"stats":     func(c Common) (Command, error) { return Stats(StatsRawData, "", c) },
		"forget": func(c Common) (Command, error) {
			return Forget(ForgetOptions{Common: c, Host: "h", KeepWithin: "1d"})
		},
		"prune": func(c Common) (Command, error) { return Prune(PruneOptions{Common: c}) },
		"check": func(c Common) (Command, error) { return Check(CheckOptions{Common: c}) },
	}
	bad := []string{
		"--one-file-system", "--no-extra-verify", "--ignore-inode", "--ignore-ctime",
		"--unsafe-allow-remove-all", "--delete", "--prune", "--remove-all", "--recursive",
		"--insecure-no-password", "--password-file", "--password-file=C:\\p.txt", "--password-command",
		"--password-command=type x", "--repo", "--repo=rest:https://x", "-r", "-rrest:https://x",
		"--repository-file", "--repository-file=f", "-p", "-pfile", "--",
	}
	for name, build := range builders {
		for _, flag := range bad {
			if c, err := build(Common{Extra: []string{flag}}); err == nil {
				t.Errorf("%s accepted Extra %q: %q", name, flag, c.Args)
			}
			if c, err := build(Common{Extra: []string{"-o", "a=b", flag}}); err == nil {
				t.Errorf("%s accepted Extra [-o a=b %q]: %q", name, flag, c.Args)
			}
		}
		// Look-alikes that are legitimate pass through untouched.
		for _, fine := range []string{"-o", "rclone.program=rclone", "-vv", "--time", "--repository-version"} {
			if _, err := build(Common{Extra: []string{fine}}); err != nil {
				t.Errorf("%s rejected harmless Extra %q: %v", name, fine, err)
			}
		}
	}
	if got := ForbiddenFlags(); len(got) != len(forbiddenFlags) || &got[0] == &forbiddenFlags[0] {
		t.Error("ForbiddenFlags must return a copy of the full list")
	}
}

func TestNoSecretsInArgv(t *testing.T) {
	repo, err := GatewayRepository("https://gw.example.com", "dev-01", "s3cr3tgatewaypw", false)
	if err != nil {
		t.Fatal(err)
	}
	e := Env{Repository: repo, Password: "s3cr3trepopw", CacheDir: "c"}
	if _, err := e.Environ(nil); err != nil {
		t.Fatal(err)
	}
	for name, c := range allCommands(AdminOptions("rclone", false)) {
		for _, a := range c.Args {
			for _, secret := range []string{repo, "s3cr3tgatewaypw", "s3cr3trepopw", "rest:https://", "@gw.example.com"} {
				if strings.Contains(a, secret) {
					t.Errorf("%s: argv element %q contains a secret", name, a)
				}
			}
		}
	}
}
