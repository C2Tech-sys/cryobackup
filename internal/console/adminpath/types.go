package adminpath

import (
	"context"
	"io"
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
)

// Methods lists every method name in documentation order.
func Methods() []string {
	return []string{
		MethodPing, MethodInit, MethodCatConfig, MethodLs, MethodFind, MethodSnapshots,
		MethodDump, MethodStats, MethodUnlock, MethodForget, MethodPrune, MethodCheck,
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
	// Dump streams one path of a snapshot as a zip archive into w (ADR 0008):
	// restore bytes travel only over the socket, never to a disk on either
	// side. A directory is restic's own `dump -a zip`; a single file is wrapped
	// into a one-entry zip on the fly. The implementation writes nothing but
	// the archive into w and stops (cancelling restic) as soon as a write to w
	// fails. Bytes and SHA256 of the result may be left zero: Handler and
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
}

// LsResult is one directory level.
//
// PATH-BEARING: Snapshot and Nodes carry file names; never log them.
type LsResult struct {
	ResultMeta
	Snapshot restic.LsSnapshot `json:"snapshot"`
	Nodes    []restic.Node     `json:"nodes"`
	// Truncated is set when the listing was cut at the limit.
	Truncated bool `json:"truncated"`
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

// DumpRequest asks for one snapshot path as a zip stream (ADR 0008).
type DumpRequest struct {
	DeviceRequest
	// SnapshotID is a hex id or "latest", optionally followed by ":<subfolder>".
	SnapshotID string `json:"snapshot_id"`
	// Path is an absolute snapshot path starting with "/": a file or a
	// directory. One path per call (architecture DH27). PATH-BEARING: never
	// log it.
	Path string `json:"path"`
}

// DumpResult describes the archive that was streamed. It is known only once
// the last byte went out.
type DumpResult struct {
	ResultMeta
	// Bytes is the size of the archive as it crossed the socket.
	Bytes int64 `json:"bytes"`
	// SHA256 is the lower-case hex SHA-256 of the archive as it crossed the socket.
	SHA256 string `json:"sha256"`
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
