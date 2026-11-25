package adminpath

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Method names as they appear in the URL path (POST /v1/<method>).
const (
	MethodPing      = "ping"
	MethodInit      = "init"
	MethodCatConfig = "cat_config"
	MethodLs        = "ls"
	MethodFind      = "find"
	MethodSnapshots = "snapshots"
	MethodDump      = "dump"
	MethodStats     = "stats"
	MethodUnlock    = "unlock"
	MethodForget    = "forget"
	MethodPrune     = "prune"
	MethodCheck     = "check"
	// MethodPurgeRepo deletes a retired device's whole repository prefix from
	// storage (ADR 0019). It is the only admin-path method that deletes outside
	// restic; see RepoPurger.
	MethodPurgeRepo = "purge_repo"
)

// Methods lists every method name in documentation order.
func Methods() []string {
	return []string{
		MethodPing, MethodInit, MethodCatConfig, MethodLs, MethodFind, MethodSnapshots,
		MethodDump, MethodStats, MethodUnlock, MethodForget, MethodPrune, MethodCheck,
		MethodPurgeRepo,
	}
}

// Limits of the wire contract.
const (
	// MaxRequestBytes caps a request body (1 MiB). The handler answers a larger
	// body with 413 and invalid_request; the client refuses to send one.
	MaxRequestBytes = 1 << 20
	// DefaultLsLimit is the number of nodes an Ls returns when LsRequest.Limit
	// is zero; MaxLsLimit is the largest Limit a request may ask for.
	DefaultLsLimit = 5000
	MaxLsLimit     = 50000
	// DefaultFindLimit is the number of matches a Find returns when
	// FindRequest.Limit is zero; MaxFindLimit is the largest Limit allowed.
	DefaultFindLimit = 1000
	MaxFindLimit     = 10000
	// MaxDumpPaths is the most paths one DumpRequest.Paths may name (ADR
	// 0028): each is its own restic run, and with --no-cache each run loads
	// the repository index again, so the cap bounds the cost of one download.
	MaxDumpPaths = 50
)

// Backend is the maintenance worker's side of the admin path (architecture DH1,
// §1.1 "maintenance worker"). The worker (`cryo-console maintain`, P1-T6)
// implements it; Client implements it by calling the worker over the socket, so
// the console (`cryo-console serve`, P1-T5) can be written and tested against a
// fake.
//
// Contract for implementers:
//
//   - Requests arrive already validated (Handler calls Validate), but an
//     implementation may call Validate again.
//   - Return an *Error, a context error or any other error. Handler maps a
//     context.DeadlineExceeded to CodeTimeout, a context.Canceled to
//     CodeUnavailable and everything else that is not an *Error to
//     CodeInternal; the underlying error text is never serialised.
//   - For Init, Forget, Prune, Check and Unlock, a restic run that reached an
//     exit code is a result, not an error: return a nil error and fill
//     ResultMeta (ExitCode, Outcome, DeferredReason, ErrorClass) so the console
//     can write the runs row (architecture §7.5). Exit 11 is Outcome
//     "deferred", never an error (architecture §7.3 step 2). Return an *Error
//     only when the call could not run at all (custody_customer_held,
//     not_found, storage_unreachable, timeout, ...).
//   - For the read-only calls (CatConfig, Ls, Find, Snapshots, Stats, Dump) a
//     failed restic run is an *Error whose Code and Class come from the exit
//     code and the classifier. Dump may fail after it has written part of the
//     archive; the wire then carries the error in a trailer and Client returns
//     it, so a cut stream is never mistaken for a complete one.
//   - Elapsed is the backend's to set. Handler echoes RequestID.
//   - The backend must honour ctx: it is cancelled when the client times out or
//     disconnects.
type Backend interface {
	// Ping is a liveness probe of the worker (architecture §1.1). It takes no
	// device and runs no restic.
	Ping(ctx context.Context) (PingResult, error)
	// Init runs restic init for a device repository. It is idempotent: an
	// existing repository whose key verifies is success with Created false
	// (architecture DH17).
	Init(ctx context.Context, req InitRequest) (InitResult, error)
	// CatConfig runs restic cat config: escrow verification and the storage
	// probe (architecture §4.6 C02, §6.3).
	CatConfig(ctx context.Context, req DeviceRequest) (restic.Config, error)
	// Ls lists one directory level of a snapshot (architecture §6.3). Dir "/"
	// lists the snapshot roots.
	Ls(ctx context.Context, req LsRequest) (LsResult, error)
	// Find searches snapshots for a name pattern (architecture §6.3).
	Find(ctx context.Context, req FindRequest) ([]restic.FindResult, error)
	// Snapshots lists the snapshots of a device repository.
	Snapshots(ctx context.Context, req DeviceRequest) ([]restic.Snapshot, error)
	// Dump streams snapshot paths as one zip archive into w (ADR 0008, ADR
	// 0028): restore bytes travel only over the socket, never to a disk on
	// either side. Path is one selection and Paths several; the archive is
	// built the same way for both: every selected file and folder becomes
	// entries of one zip, named relative to the selected item's parent (a
	// file is one entry, a folder its own entry and everything below it),
	// links and other special entries are left out and counted
	// (DumpResult.Skipped), and the zip's central directory is written only
	// after every restic run exited 0. The implementation writes nothing
	// but the archive into w and stops (cancelling restic) as soon as a write
	// to w fails. Bytes and SHA256 of the result may be left zero: Handler and
	// Client count and hash what actually crossed the socket.
	Dump(ctx context.Context, req DumpRequest, w io.Writer) (DumpResult, error)
	// Stats runs restic stats in the requested mode (architecture §7.3 step 8).
	Stats(ctx context.Context, req StatsRequest) (restic.StatsResult, error)
	// Unlock removes stale locks, or every lock when RemoveAll and Quiesced are
	// both set (architecture §7.3 step 2 and 7, §6.3).
	Unlock(ctx context.Context, req UnlockRequest) (UnlockResult, error)
	// Forget runs restic forget with the given keep rules, without --prune
	// (architecture §7.3 steps 3-4).
	Forget(ctx context.Context, req ForgetRequest) (ForgetResult, error)
	// Prune runs restic prune (architecture §7.3 step 5).
	Prune(ctx context.Context, req PruneRequest) (PruneResult, error)
	// Check runs restic check (architecture §7.3 step 6).
	Check(ctx context.Context, req CheckRequest) (CheckResult, error)
	// RepoPurger is purge_repo (ADR 0019): delete a retired device's
	// repository prefix.
	RepoPurger
}

// ProbeRepoID is the reserved repository name of the storage probe (banner
// C02, ADR 0017): CatConfig with this DeviceID addresses the probe repository
// <bucket>/<prefix>/probe instead of a device's, the same location the
// gateway's reserved probe login requests as /probe/config. Device ids are
// ULIDs, so it cannot collide with one. Only CatConfig accepts it; every other
// method answers not_found for it.
const ProbeRepoID = "probe"

// DeviceRequest identifies the device repository a call addresses and carries the
// audit id. Every request type embeds it except Ping, which has no request.
// The RPC never carries a repository password, a gateway password, a storage
// credential or a master key: the worker unwraps the device's key itself
// (architecture DH1).
type DeviceRequest struct {
	// DeviceID is the device's id (a ULID in production): 1 to 64 characters of
	// [A-Za-z0-9_-], starting with a letter or digit.
	DeviceID string `json:"device_id"`
	// RequestID is an opaque audit id of the same charset, up to 64 characters.
	// Client generates a random one when it is empty; the response echoes it.
	RequestID string `json:"request_id,omitempty"`
}

// ResultMeta is embedded in every result of a restic-backed call so the console
// can write a runs row (architecture §7.5). JSON field names are flat
// (request_id, elapsed_ns, exit_code, ...).
type ResultMeta struct {
	// RequestID echoes the request's RequestID (set by Handler).
	RequestID string `json:"request_id,omitempty"`
	// Elapsed is the wall time the backend spent on the call (nanoseconds on
	// the wire).
	Elapsed time.Duration `json:"elapsed_ns"`
	// ExitCode is restic's exit code, 0 when restic did not run.
	ExitCode int `json:"exit_code"`
	// Outcome is the runs.outcome the exit code maps to (restic.Map).
	Outcome restic.Outcome `json:"outcome,omitempty"`
	// DeferredReason is set when Outcome is "deferred".
	DeferredReason restic.DeferredReason `json:"deferred_reason,omitempty"`
	// ErrorClass is set when Outcome is "failed" or "deferred".
	ErrorClass restic.ErrorClass `json:"error_class,omitempty"`
}

func (m *ResultMeta) setRequestID(id string) { m.RequestID = id }

// PingResult is the answer to Ping.
type PingResult struct {
	// Version is the worker's build version (buildinfo.Version).
	Version string `json:"version"`
	// Uptime is how long the worker has been running (nanoseconds on the wire).
	Uptime time.Duration `json:"uptime_ns"`
	// MasterKeyLoaded reports that the master key is in memory, so key unwrap
	// can work.
	MasterKeyLoaded bool `json:"master_key_loaded"`
	// StorageReachable is the result of the worker's last storage probe; nil
	// when it has not probed yet.
	StorageReachable *bool `json:"storage_reachable,omitempty"`
}

// InitRequest creates the repository of a device (architecture DH17).
type InitRequest struct {
	DeviceRequest
}

// InitResult is the answer to Init.
type InitResult struct {
	ResultMeta
	// RepositoryID is restic's repository id (not a location).
	RepositoryID string `json:"repository_id"`
	// Created is false when the repository already existed and its key verified.
	Created bool `json:"created"`
}

// LsRequest lists one directory level of one snapshot.
type LsRequest struct {
	DeviceRequest
	// SnapshotID is a hex snapshot id (8 to 64 digits) or "latest".
	SnapshotID string `json:"snapshot_id"`
	// Dir is an absolute snapshot path starting with "/"; "/" lists the roots.
	// PATH-BEARING: never log it.
	Dir string `json:"dir"`
	// Limit caps the nodes returned: 0 means DefaultLsLimit, the maximum is
	// MaxLsLimit.
	Limit int `json:"limit,omitempty"`
	// Offset skips that many children before the returned window: a page of
	// a large folder is Offset n*Limit, Limit L. restic has no paging, so the
	// worker reads the whole listing on every call and keeps only the window.
	// 0 or more.
	Offset int `json:"offset,omitempty"`
}

// LsResult is one directory level, or one window of it (LsRequest.Offset and
// Limit). Nodes come in restic's order, which for one snapshot is the same on
// every call (the order of the folder's tree, sorted by name), so consecutive
// windows neither repeat nor skip a child.
//
// PATH-BEARING: Snapshot and Nodes carry file names; never log them.
type LsResult struct {
	ResultMeta
	Snapshot restic.LsSnapshot `json:"snapshot"`
	Nodes    []restic.Node     `json:"nodes"`
	// Truncated is set when children follow the returned window (the listing
	// was cut at the limit).
	Truncated bool `json:"truncated"`
	// Total is the number of children of Dir, whatever the window. A worker
	// that predates paging (P22) leaves it 0.
	Total int `json:"total"`
}

// FindRequest searches snapshots for a name pattern.
type FindRequest struct {
	DeviceRequest
	// Pattern is a restic find pattern; it must not start with "-".
	// PATH-BEARING: never log it.
	Pattern    string `json:"pattern"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	// SnapshotIDs restricts the search (hex ids or "latest"); empty searches
	// every snapshot. At most 64.
	SnapshotIDs []string `json:"snapshot_ids,omitempty"`
	// Limit caps the matches returned in total: 0 means DefaultFindLimit, the
	// maximum is MaxFindLimit. The backend enforces it.
	Limit int `json:"limit,omitempty"`
}

// DumpRequest asks for one snapshot path (Path) or several (Paths) as one zip
// stream (ADR 0008, ADR 0028). Exactly one of Path and Paths is set.
type DumpRequest struct {
	DeviceRequest
	// SnapshotID is a hex id or "latest", optionally followed by
	// ":<subfolder>" with Path; with Paths it is a hex id or "latest" only.
	SnapshotID string `json:"snapshot_id"`
	// Path is an absolute snapshot path starting with "/": a file or a
	// directory (architecture DH27). The zip is the one Paths: [Path] would
	// give (ADR 0028), with the subfolder of SnapshotID joined in front of
	// Path. The snapshot root ("/" with no subfolder) has no name of its own,
	// so its top-level entries are the snapshot's roots. PATH-BEARING: never
	// log it.
	Path string `json:"path,omitempty"`
	// Paths are 1 to MaxDumpPaths absolute, clean snapshot paths, files or
	// folders, never "/" (ADR 0028). A path inside another selected folder is
	// dropped, because the folder holds it (CollapsePaths); the rest are
	// streamed in the order given. Each selected item is a top-level entry of
	// the zip named by its base name (a repeated name gets " (2)", " (3)", ...),
	// a folder's contents are named relative to the folder's parent, and
	// symlinks, junctions and devices are left out and counted in a final
	// CRYOBACKUP-SKIPPED.txt entry that names no file. PATH-BEARING: never log
	// them.
	Paths []string `json:"paths,omitempty"`
}

// CollapsePaths returns paths without duplicates and without any path that
// lies inside another of them (the outer folder already holds it), in the
// order given. Paths are compared byte for byte: callers pass clean absolute
// snapshot paths (DumpRequest.Validate requires them).
func CollapsePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for i, p := range paths {
		if seen[p] {
			continue
		}
		inner := false
		for j, q := range paths {
			if j != i && q != p && (q == "/" || strings.HasPrefix(p, q+"/")) {
				inner = true
				break
			}
		}
		if inner {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// DumpResult describes the archive that was streamed. It is known only once
// the last byte went out.
type DumpResult struct {
	ResultMeta
	// Bytes is the size of the archive as it crossed the socket.
	Bytes int64 `json:"bytes"`
	// SHA256 is the lower-case hex SHA-256 of the archive as it crossed the socket.
	SHA256 string `json:"sha256"`
	// Skipped counts what the archive left out because it is not a regular
	// file or folder (ADR 0028), for the download's record. All zero when
	// nothing was left out, and from a worker that predates the count.
	Skipped DumpSkipped `json:"skipped"`
}

// Kinds of snapshot entries a download leaves out (DumpSkipped.Add).
const (
	SkipSymlink  = "symlink"  // a symbolic link, or a junction (restic stores one as a symlink)
	SkipHardLink = "hardlink" // a hard link entry of restic's tar
	SkipDevice   = "device"   // a character or block device
	SkipPipe     = "fifo"     // a named pipe
	SkipSocket   = "socket"   // a Unix socket
	SkipOther    = "other"    // any other entry that is not a regular file or a folder
)

// DumpSkipped counts the snapshot entries a download left out, by kind. It
// holds counts only: the names of what was left out are never kept, sent or
// logged anywhere.
type DumpSkipped struct {
	Symlinks  int `json:"symlinks"` // symbolic links and junctions
	HardLinks int `json:"hard_links"`
	Devices   int `json:"devices"`
	Pipes     int `json:"named_pipes"`
	Sockets   int `json:"sockets"`
	Other     int `json:"other"`
}

// Add counts one entry of kind (a Skip* constant); an unknown kind counts as
// SkipOther.
func (s *DumpSkipped) Add(kind string) {
	switch kind {
	case SkipSymlink:
		s.Symlinks++
	case SkipHardLink:
		s.HardLinks++
	case SkipDevice:
		s.Devices++
	case SkipPipe:
		s.Pipes++
	case SkipSocket:
		s.Sockets++
	default:
		s.Other++
	}
}

// Total is the number of entries left out.
func (s DumpSkipped) Total() int {
	return s.Symlinks + s.HardLinks + s.Devices + s.Pipes + s.Sockets + s.Other
}

// StatsRequest runs restic stats.
type StatsRequest struct {
	DeviceRequest
	// Mode is a restic.StatsMode ("raw-data" for usage sampling).
	Mode restic.StatsMode `json:"mode"`
}

// UnlockRequest removes locks.
type UnlockRequest struct {
	DeviceRequest
	// RemoveAll removes every lock, not just stale ones. It is accepted only
	// when Quiesced is true.
	RemoveAll bool `json:"remove_all,omitempty"`
	// Quiesced is the console's statement that the device has no backup running
	// and none will start (architecture §6.3). RemoveAll without it is rejected.
	Quiesced bool `json:"quiesced,omitempty"`
}

// UnlockResult is the answer to Unlock.
type UnlockResult struct {
	ResultMeta
}

// KeepRules are the policy-shaped keep counts. Zero omits a rule, -1 means
// unlimited, anything below -1 is invalid (the semantics of restic.ForgetOptions).
type KeepRules struct {
	KeepLast    int `json:"keep_last,omitempty"`
	KeepHourly  int `json:"keep_hourly,omitempty"`
	KeepDaily   int `json:"keep_daily,omitempty"`
	KeepWeekly  int `json:"keep_weekly,omitempty"`
	KeepMonthly int `json:"keep_monthly,omitempty"`
	KeepYearly  int `json:"keep_yearly,omitempty"`
}

// ForgetRequest runs restic forget for the device's own host, without --prune.
type ForgetRequest struct {
	DeviceRequest
	Rules KeepRules `json:"rules"`
	// KeepWithin is required, restic's duration form such as "30d" or "1y2m"
	// (architecture §7.3 step 3).
	KeepWithin string `json:"keep_within"`
	// KeepTags lists tags whose snapshots are always kept. At most 64.
	KeepTags []string `json:"keep_tags,omitempty"`
	// DryRun only reports what would be removed.
	DryRun bool `json:"dry_run,omitempty"`
}

// ForgetResult is the answer to Forget. Exit 3 gives Outcome "partial".
//
// PATH-BEARING: Groups carries snapshot paths; never log it.
type ForgetResult struct {
	ResultMeta
	Groups []restic.ForgetGroup `json:"groups"`
}

// PruneRequest runs restic prune.
type PruneRequest struct {
	DeviceRequest
	// MaxUnused is --max-unused: "5%", "unlimited" or a size; "" omits it.
	MaxUnused string `json:"max_unused,omitempty"`
	// RepackCacheableOnly renders --repack-cacheable-only.
	RepackCacheableOnly bool `json:"repack_cacheable_only,omitempty"`
	// MaxRepackSize is --max-repack-size, a size such as "500M"; "" omits it.
	MaxRepackSize string `json:"max_repack_size,omitempty"`
	// DryRun only reports what would be done.
	DryRun bool `json:"dry_run,omitempty"`
}

// PruneResult is the answer to Prune; prune has no JSON, so Stats holds the
// numbers parsed from its text (restic.ParsePruneText).
type PruneResult struct {
	ResultMeta
	Stats restic.PruneStats `json:"stats"`
}

// CheckRequest runs restic check.
type CheckRequest struct {
	DeviceRequest
	// ReadDataSubset is --read-data-subset: "n/t", "x%" or a size; "" checks
	// structure only.
	ReadDataSubset string `json:"read_data_subset,omitempty"`
	// WithCache renders --with-cache.
	WithCache bool `json:"with_cache,omitempty"`
}

// CheckResult is the answer to Check. NumErrors or BrokenPacks above zero gives
// Outcome "failed" (architecture §7.3 step 6).
type CheckResult struct {
	ResultMeta
	Summary restic.CheckSummary `json:"summary"`
}

// RepoPurger is the admin-path method purge_repo (ADR 0019): it deletes the
// whole storage prefix of one retired device, <bucket>/<prefix>/<device id>,
// with `rclone purge` and the worker's admin credential, then lists the prefix
// again and reports what is left. It is the only admin-path method that deletes
// anything outside restic.
//
// Contract for implementers:
//
//   - The location is built from the worker's own configuration and the
//     validated device id only. The request carries no path, remote, bucket or
//     prefix, and none may ever be added to it.
//   - A refusal (an empty bucket or prefix, an id that is not a ULID, a
//     location that is not exactly one device id under the configured bucket
//     and prefix) is an *Error and runs nothing.
//   - A prefix that does not exist is success with every count zero: the call
//     is idempotent, so the console may retry it until it succeeds.
//   - A nil error does not by itself mean the prefix is gone:
//     PurgeRepoResult.RemainingObjects does. Objects that are still listed after
//     the purge are a result with RemainingObjects above zero and ErrorClass
//     set, not an error, so the console sees how far the purge got. A caller
//     that deletes the device record must require RemainingObjects == 0.
type RepoPurger interface {
	// PurgeRepo deletes the repository prefix of req.DeviceID.
	PurgeRepo(ctx context.Context, req PurgeRepoRequest) (PurgeRepoResult, error)
}

// PurgeRepoRequest names the retired device whose repository prefix is to be
// deleted. DeviceID must be a canonical ULID (26 upper-case Crockford base32
// characters, the first at most '7'): unlike the other methods, purge_repo
// accepts nothing else, not even ProbeRepoID.
type PurgeRepoRequest struct {
	DeviceRequest
}

// PurgeRepoResult is the answer to PurgeRepo. The counts are rclone's object
// counts and byte sums for the device's prefix (not restic blobs or snapshots).
type PurgeRepoResult struct {
	// RequestID echoes the request's RequestID (set by Handler).
	RequestID string `json:"request_id,omitempty"`
	// Elapsed is the wall time the backend spent on the call (nanoseconds on
	// the wire).
	Elapsed time.Duration `json:"elapsed_ns"`
	// ObjectsRemoved is the number of objects listed before the purge less the
	// number still listed after it; zero when the prefix did not exist.
	ObjectsRemoved int64 `json:"objects_removed"`
	// BytesRemoved is the size of the objects listed before the purge less the
	// size of those still listed after it.
	BytesRemoved int64 `json:"bytes_removed"`
	// RemainingObjects is the number of objects listed under the prefix after
	// the purge. Zero, and only zero, means the repository is gone.
	RemainingObjects int64 `json:"remaining_objects"`
	// ErrorClass is set when RemainingObjects is above zero: the path-free class
	// of the purge's failure, or "storage" when the purge reported success and
	// objects are still listed.
	ErrorClass restic.ErrorClass `json:"error_class,omitempty"`
}

func (r *PurgeRepoResult) setRequestID(id string) { r.RequestID = id }
