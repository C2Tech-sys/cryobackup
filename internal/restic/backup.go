package restic

import (
	"strconv"
	"strings"
	"time"
)

// Tag is one snapshot tag rendered as "--tag key=value".
type Tag struct {
	Key   string
	Value string
}

// StandardTags returns the tags architecture §6.3 puts on every backup, in
// this order: cb.customer=<id>, cb.policy=<id>.<version>, cb.run=<run id>.
func StandardTags(customerID, policyID string, policyVersion int, runID string) []Tag {
	return []Tag{
		{Key: "cb.customer", Value: customerID},
		{Key: "cb.policy", Value: policyID + "." + strconv.Itoa(policyVersion)},
		{Key: "cb.run", Value: runID},
	}
}

// VSSOptions turns on Volume Shadow Copy for a backup (--use-fs-snapshot) and
// carries restic's vss.* backend options (engine §1.1).
type VSSOptions struct {
	// Timeout renders -o vss.timeout=<n>s when greater than zero.
	Timeout time.Duration
	// ExcludeAllMountPoints renders -o vss.exclude-all-mount-points=true.
	// restic 0.19.1 requires the "=true": the bare form fails with a ParseBool
	// error (testdata/v0.19.1/MANIFEST.md, divergence 2).
	ExcludeAllMountPoints bool
	// ExcludeVolumes renders -o vss.exclude-volumes=<a;b>; entries are joined
	// with ';' and must not contain one.
	ExcludeVolumes []string
}

// BackupOptions describes one "restic backup" run (architecture §6.3).
type BackupOptions struct {
	Common

	// Host is the stable device id passed as --host (engine §1.7). Required.
	Host string
	// Tags are rendered as repeated --tag key=value, in order. Neither key nor
	// value may contain ',' (restic splits on it) and the key may not contain '='.
	Tags []Tag
	// VSS, when non-nil, adds --use-fs-snapshot and its options.
	VSS *VSSOptions
	// ExcludeCloudFiles renders --exclude-cloud-files.
	ExcludeCloudFiles bool
	// IExcludeFile renders --iexclude-file (case-insensitive excludes, engine §1.5).
	IExcludeFile string
	// IExcludes renders one --iexclude per entry.
	IExcludes []string
	// ExcludeCaches renders --exclude-caches.
	ExcludeCaches bool
	// ExcludeLargerThan renders --exclude-larger-than in bytes (no suffix) when
	// greater than zero.
	ExcludeLargerThan int64
	// ExcludeIfPresent renders one --exclude-if-present per entry.
	ExcludeIfPresent []string
	// ReadConcurrency renders --read-concurrency when greater than zero.
	ReadConcurrency int
	// PackSizeMiB renders --pack-size; 0 omits it, otherwise 4 to 128.
	PackSizeMiB int
	// Compression is "", auto, off, fastest, better or max; "" omits the flag.
	Compression string
	// SkipIfUnchanged renders --skip-if-unchanged --no-scan.
	SkipIfUnchanged bool
	// Force renders --force (operator "full rescan" only).
	Force bool
	// DryRun renders --dry-run.
	DryRun bool
	// Roots are the paths to back up. At least one. They follow a "--".
	Roots []string
}

// Backup builds the backup command. The rendered order is fixed (golden-tested):
//
//	backup --json --host H [--tag k=v]... [--use-fs-snapshot -o vss.timeout=Ns
//	[-o vss.exclude-all-mount-points=true] [-o vss.exclude-volumes=a;b]]
//	[--exclude-cloud-files] [--iexclude-file F] [--iexclude P]... [--exclude-caches]
//	[--exclude-larger-than N] [--exclude-if-present X]... [--limit-upload N]
//	[--limit-download N] [--read-concurrency N] [--pack-size N] [--compression M]
//	[--retry-lock D] [--skip-if-unchanged --no-scan] [--force] [--dry-run]
//	[Extra...] -- ROOT...
//
// restic 0.19.1 accepts "--" before the roots, so a root that starts with '-'
// cannot be mistaken for a flag.
//
// NTFS metadata (matrix r52) needs no flag. restic 0.19.1 stores it for every
// file and directory by default and has no backup flag that turns it off
// (cmd/restic/cmd_backup.go registers none): internal/fs/node.go
// nodeFromFileInfo calls nodeFillGenericAttributes and
// nodeFillExtendedAttributes unconditionally, and on Windows
// (internal/fs/node_windows.go) these record the creation time, the file
// attributes and the security descriptor of files and directories (not of a
// volume root, and not of alternate data streams), plus the extended
// attributes on volumes that support them. The full security descriptor
// (owner, group, DACL, SACL) needs SeBackupPrivilege and SeSecurityPrivilege,
// which restic enables at start (internal/fs/fs_local.go init,
// internal/fs/priv_windows.go) and LocalSystem holds; without them it falls
// back to owner, group and DACL (internal/fs/sd_windows.go,
// getSecurityDescriptor). Under --use-fs-snapshot the metadata is read
// through the snapshot like the data. Only --stdin and --stdin-from-command
// back up without file metadata, and Backup never renders them.
func Backup(o BackupOptions) (Command, error) {
	if err := o.Common.validate(); err != nil {
		return Command{}, err
	}
	if hasControlOrEmpty(o.Host) {
		return Command{}, invalid("backup: host is required")
	}
	if len(o.Roots) == 0 {
		return Command{}, invalid("backup: at least one root is required")
	}
	for _, r := range o.Roots {
		if hasControlOrEmpty(r) {
			return Command{}, invalid("backup: roots must be non-empty")
		}
	}
	for _, t := range o.Tags {
		if t.Key == "" || t.Value == "" || strings.ContainsAny(t.Key, ",=") || strings.ContainsRune(t.Value, ',') ||
			hasControl(t.Key) || hasControl(t.Value) {
			return Command{}, invalid("backup: tags need a key and a value without ',' (and no '=' in the key)")
		}
	}
	if o.PackSizeMiB != 0 && (o.PackSizeMiB < 4 || o.PackSizeMiB > 128) {
		return Command{}, invalid("backup: pack size must be 0 or 4..128 MiB")
	}
	switch o.Compression {
	case "", "auto", "off", "fastest", "better", "max":
	default:
		return Command{}, invalid("backup: compression must be auto, off, fastest, better or max")
	}
	if o.ExcludeLargerThan < 0 || o.ReadConcurrency < 0 {
		return Command{}, invalid("backup: numeric options must not be negative")
	}
	for _, v := range [][]string{o.IExcludes, o.ExcludeIfPresent} {
		for _, s := range v {
			if hasControlOrEmpty(s) {
				return Command{}, invalid("backup: exclude patterns must be non-empty")
			}
		}
	}
	if o.IExcludeFile != "" && hasControl(o.IExcludeFile) {
		return Command{}, invalid("backup: exclude file path has a control character")
	}

	args := []string{"backup", "--json", "--host", o.Host}
	for _, t := range o.Tags {
		args = append(args, "--tag", t.Key+"="+t.Value)
	}
	if v := o.VSS; v != nil {
		if v.Timeout < 0 {
			return Command{}, invalid("backup: vss timeout must not be negative")
		}
		args = append(args, "--use-fs-snapshot")
		if v.Timeout > 0 {
			args = append(args, "-o", "vss.timeout="+formatDuration(v.Timeout))
		}
		if v.ExcludeAllMountPoints {
			args = append(args, "-o", "vss.exclude-all-mount-points=true")
		}
		if len(v.ExcludeVolumes) > 0 {
			for _, vol := range v.ExcludeVolumes {
				if hasControlOrEmpty(vol) || strings.ContainsRune(vol, ';') {
					return Command{}, invalid("backup: vss volumes must be non-empty and free of ';'")
				}
			}
			args = append(args, "-o", "vss.exclude-volumes="+strings.Join(v.ExcludeVolumes, ";"))
		}
	}
	if o.ExcludeCloudFiles {
		args = append(args, "--exclude-cloud-files")
	}
	if o.IExcludeFile != "" {
		args = append(args, "--iexclude-file", o.IExcludeFile)
	}
	for _, p := range o.IExcludes {
		args = append(args, "--iexclude", p)
	}
	if o.ExcludeCaches {
		args = append(args, "--exclude-caches")
	}
	if o.ExcludeLargerThan > 0 {
		args = append(args, "--exclude-larger-than", strconv.FormatInt(o.ExcludeLargerThan, 10))
	}
	for _, x := range o.ExcludeIfPresent {
		args = append(args, "--exclude-if-present", x)
	}
	args = o.limits(args)
	if o.ReadConcurrency > 0 {
		args = append(args, "--read-concurrency", strconv.Itoa(o.ReadConcurrency))
	}
	if o.PackSizeMiB > 0 {
		args = append(args, "--pack-size", strconv.Itoa(o.PackSizeMiB))
	}
	if o.Compression != "" {
		args = append(args, "--compression", o.Compression)
	}
	args = o.retry(args)
	if o.SkipIfUnchanged {
		args = append(args, "--skip-if-unchanged", "--no-scan")
	}
	if o.Force {
		args = append(args, "--force")
	}
	if o.DryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, o.Extra...)
	args = append(args, "--")
	args = append(args, o.Roots...)
	return Command{Kind: KindBackup, Args: args}, nil
}
