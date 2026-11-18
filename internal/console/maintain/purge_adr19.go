package maintain

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// purge_repo (ADR 0019): the one place in the product that deletes backup data
// outside restic. A retired device's repository is the storage prefix
// <bucket>/<prefix>/<device id> on the worker's admin remote; PurgeRepo
//
//  1. refuses, running nothing, unless that location is exactly one canonical
//     device id under a non-empty configured bucket and prefix (purgeLocation),
//     the device is known, retired and marked for purge (ADR 0019: mark first,
//     storage second), and its recorded repo_path is the same prefix
//     (purgeEligible);
//  2. takes an in-process claim on the device (claimPurge: a second purge of
//     the same device in this worker is refused at once, whether or not the
//     device has a maintenance_schedule row) and then the per-device
//     maintenance lease (maintenance_schedule.lease_*), the exclusion the
//     scheduler's mutating steps take, under a holder of its own so that
//     neither the scheduler nor a second purge can share it;
//  3. lists the parent <remote>:<bucket>/<prefix> (`rclone lsjson
//     --dirs-only`, not recursive): when the parent itself is not found or
//     cannot be listed, the purge stops with a classified error, so a wrong
//     bucket or remote can never read as "nothing left";
//  4. measures the prefix (`rclone size --json`), purges it (`rclone purge`)
//     when it holds anything, and lists it again (`rclone lsjson -R
//     --files-only`) to report what is left.
//
// The whole call runs under purgeDeadline, which is shorter than the lease
// (purgeLeaseTTL) and than the admin-path client's timeout for purge_repo, so
// neither the lease nor the caller can give up under a running purge; a call
// that reaches it, waiting for a concurrency slot included, is
// adminpath.CodeTimeout.
//
// rclone runs as a child through the worker's runner, with the same minimal
// environment as restic (RCLONE_CONFIG, the scratch directories, PATH), under
// a concurrency slot and a per-step timeout, and is cancelled with the call.
// Its output is never logged or returned: stdout is decoded into counts, and
// stderr goes through the path-free classifier like restic's. The command
// line, the remote, the bucket, the prefix and object names never reach a log
// line or an error.

// Per-step deadlines of a purge. Together they stay below purgeDeadline, which
// stays below the lease, so the lease cannot expire under a running purge
// (TestPurgeDeadlinesADR19 pins the arithmetic).
const (
	// purgeParentListTimeout bounds the listing of the parent prefix.
	purgeParentListTimeout = 2 * time.Minute
	// purgeListTimeout bounds the measurement before the purge and the listing
	// after it, each.
	purgeListTimeout = 10 * time.Minute
	// purgeRemoveTimeout bounds `rclone purge`.
	purgeRemoveTimeout = 85 * time.Minute
	// purgeLeaseTTL is how long the per-device lease of a purge is held at most
	// (a crashed worker's lease frees itself after it).
	purgeLeaseTTL = 2 * time.Hour
	// rcloneExitDirNotFound is rclone's exit code for "directory not found"
	// (size and lsjson of a prefix that does not exist; verified with rclone
	// 1.75.1). `rclone purge` of a missing directory exits 1 instead, which is
	// why the purge runs only after the measurement found objects.
	rcloneExitDirNotFound = 3
	// purgeStdoutCap bounds what is read of `rclone size --json`.
	purgeStdoutCap = 1 << 20
)

// purgeDeadline bounds a whole PurgeRepo call: the waits for concurrency slots,
// every rclone step and the bookkeeping. It is below purgeLeaseTTL (the lease
// is taken after the deadline starts, so it outlives the call) and below the
// admin-path client's two hours for purge_repo, so the worker answers before
// the console gives up. A variable only so that a test can shorten it.
var purgeDeadline = 110 * time.Minute

// Reasons of a PurgeRefusedError. Each names the check that failed, never a
// value.
const (
	// PurgeRefusedDeviceID: the device id is not a canonical ULID.
	PurgeRefusedDeviceID = "device_id"
	// PurgeRefusedRequestID: the request id is malformed.
	PurgeRefusedRequestID = "request_id"
	// PurgeRefusedDeviceUnknown: no device has this id.
	PurgeRefusedDeviceUnknown = "device_unknown"
	// PurgeRefusedNotRetired: the device is not retired (ADR 0019: only a
	// retired device can be purged).
	PurgeRefusedNotRetired = "not_retired"
	// PurgeRefusedNotRequested: the device is retired but not marked for purge
	// (devices.purge_requested_at is NULL). ADR 0019 orders the mark first and
	// the storage step second, so storage is never touched for a device the
	// console has not recorded as purging.
	PurgeRefusedNotRequested = "not_requested"
	// PurgeRefusedRepoPath: the device's recorded repo_path is not
	// <prefix>/<device id>/ under the worker's configured prefix, so the worker
	// would delete somewhere else than where the device wrote.
	PurgeRefusedRepoPath = "repo_path"
	// PurgeRefusedRemote: the configured remote is not a plain rclone remote
	// name of at least two characters.
	PurgeRefusedRemote = "remote"
	// PurgeRefusedBucket: the configured bucket is empty or has a segment that
	// is empty, ".", "..", or holds a wildcard, a control character, a
	// backslash, a colon or surrounding white space.
	PurgeRefusedBucket = "bucket"
	// PurgeRefusedPrefix: the configured prefix is empty (a purge never runs at
	// the bucket root) or has a segment refused as for the bucket.
	PurgeRefusedPrefix = "prefix"
	// PurgeRefusedLocation: the location built from the configuration is not
	// exactly <bucket>/<prefix>/<device id> after cleaning, or differs from
	// the repository location restic uses for the device.
	PurgeRefusedLocation = "location"
	// PurgeRefusedRcloneProgram: Config.RcloneProgram cannot be found.
	PurgeRefusedRcloneProgram = "rclone_program"
)

// PurgeRefusedError is the error PurgeRepo returns when it refuses to run: no
// rclone process was started and nothing was deleted. It unwraps to the
// *adminpath.Error that goes on the wire: invalid_request for the request or
// the device's state (device_id, request_id, not_retired, not_requested),
// not_found for an unknown device, unavailable for the worker's own
// configuration (the other reasons).
type PurgeRefusedError struct {
	// Reason is one of the PurgeRefused constants.
	Reason string
	wire   *adminpath.Error
}

// Error implements error. The text names the reason only.
func (e *PurgeRefusedError) Error() string { return "maintain: purge refused: " + e.Reason }

// Unwrap returns the admin-path error the refusal is answered with.
func (e *PurgeRefusedError) Unwrap() error { return e.wire }

func purgeRefused(reason string) *PurgeRefusedError {
	code := adminpath.CodeUnavailable
	switch reason {
	case PurgeRefusedDeviceID, PurgeRefusedRequestID, PurgeRefusedNotRetired, PurgeRefusedNotRequested:
		code = adminpath.CodeInvalidRequest
	case PurgeRefusedDeviceUnknown:
		code = adminpath.CodeNotFound
	}
	return &PurgeRefusedError{Reason: reason, wire: adminpath.NewError(code, "")}
}

var _ adminpath.RepoPurger = (*Worker)(nil)

// PurgeRepo implements adminpath.RepoPurger (ADR 0019): it deletes the storage
// prefix of one retired device and reports what was removed and what is left.
// See the comment at the top of this file for the steps and
// adminpath.RepoPurger for the contract. Refusals are *PurgeRefusedError; a
// failed rclone run is an *adminpath.Error (storage_unreachable for the network,
// tls and storage classes, restic_failed with the class otherwise, timeout); a
// call that reaches purgeDeadline is adminpath.CodeTimeout; a cancelled call
// returns the context error.
func (w *Worker) PurgeRepo(ctx context.Context, req adminpath.PurgeRepoRequest) (adminpath.PurgeRepoResult, error) {
	pctx, cancel := context.WithTimeout(ctx, purgeDeadline)
	defer cancel()
	res, err := w.purgeRepo(pctx, req)
	if err != nil && ctx.Err() == nil && errors.Is(pctx.Err(), context.DeadlineExceeded) {
		attrs := []any{attrMethod(adminpath.MethodPurgeRepo), attrClass(adminpath.CodeTimeout)}
		if req.Validate() == nil {
			attrs = append(attrs, attrDevice(req.DeviceID))
		}
		w.log.Warn("repository purge stopped at its deadline", attrs...)
		return adminpath.PurgeRepoResult{}, adminpath.NewError(adminpath.CodeTimeout, "")
	}
	return res, err
}

// purgeRepo is PurgeRepo under the call's deadline.
func (w *Worker) purgeRepo(ctx context.Context, req adminpath.PurgeRepoRequest) (adminpath.PurgeRepoResult, error) {
	start := w.clock()
	if err := req.Validate(); err != nil {
		reason := PurgeRefusedDeviceID
		var ve *adminpath.ValidationError
		if errors.As(err, &ve) && ve.Field == "request_id" {
			reason = PurgeRefusedRequestID
		}
		return adminpath.PurgeRepoResult{}, w.refuse("", purgeRefused(reason))
	}
	id := req.DeviceID
	loc, prefix, rerr := purgeLocation(w.cfg.Remote, w.cfg.Bucket, w.cfg.Prefix, id)
	if rerr != nil {
		return adminpath.PurgeRepoResult{}, w.refuse(id, rerr)
	}
	if err := w.purgeEligible(ctx, id, prefix); err != nil {
		var r *PurgeRefusedError
		if errors.As(err, &r) {
			return adminpath.PurgeRepoResult{}, w.refuse(id, r)
		}
		return adminpath.PurgeRepoResult{}, err
	}
	prog, err := exec.LookPath(w.cfg.RcloneProgram)
	if err != nil {
		return adminpath.PurgeRepoResult{}, w.refuse(id, purgeRefused(PurgeRefusedRcloneProgram))
	}
	unclaim, ok := w.claimPurge(id)
	if !ok {
		w.log.Warn("repository purge deferred: another purge of the device is running in this worker",
			attrMethod(adminpath.MethodPurgeRepo), attrDevice(id), attrClass(string(restic.ClassLock)))
		return adminpath.PurgeRepoResult{}, adminpath.NewError(adminpath.CodeLocked, restic.ClassLock)
	}
	defer unclaim()
	release, err := w.leasePurge(ctx, id)
	if err != nil {
		return adminpath.PurgeRepoResult{}, err
	}
	defer release()
	rc := &runner{program: prog, dir: w.tmpDir()}
	w.log.Info("repository purge started", attrMethod(adminpath.MethodPurgeRepo), attrDevice(id))

	// 1. The parent <remote>:<bucket>/<prefix> must be there. Without this, a
	// wrong bucket or remote would make the device prefix "not found" below,
	// which reads as zeros, and the console would delete the record while the
	// data stays where it really is.
	if err := w.rcloneParent(ctx, rc, id, purgeParent(loc, id)); err != nil {
		w.purgeFailed(id, err)
		return adminpath.PurgeRepoResult{}, err
	}

	// 2. Measure. A device prefix that does not exist under a parent that does
	// is not an error: purge is idempotent and the console retries it until it
	// succeeds.
	before, err := w.rcloneSize(ctx, rc, id, loc)
	if err != nil {
		w.purgeFailed(id, err)
		return adminpath.PurgeRepoResult{}, err
	}

	// 3. Purge, only when there is something to purge.
	var purgeErr error
	if before.Count > 0 {
		pr, err := w.rcloneRun(ctx, rc, id, "purge", purgeRemoveTimeout, []string{"purge", "--", loc}, nil)
		if err != nil {
			w.purgeFailed(id, err)
			return adminpath.PurgeRepoResult{}, err
		}
		if abort := abortError(ctx, execResult{procResult: pr}); abort != nil {
			w.purgeFailed(id, abort)
			return adminpath.PurgeRepoResult{}, abort
		}
		if pr.Exit != 0 {
			// Not final yet: the listing below decides whether anything is left.
			purgeErr = rcloneError(pr)
		}
	}

	// 4. List again and report what is left.
	left, err := w.rcloneRemaining(ctx, rc, id, loc)
	if err != nil {
		if purgeErr != nil && ctx.Err() == nil {
			err = purgeErr // the purge's own failure says more than the listing's
		}
		w.purgeFailed(id, err)
		return adminpath.PurgeRepoResult{}, err
	}
	res := adminpath.PurgeRepoResult{
		ObjectsRemoved:   max(before.Count-left.Count, 0),
		BytesRemoved:     max(before.Bytes-left.Bytes, 0),
		RemainingObjects: left.Count,
		Elapsed:          w.clock().Sub(start),
	}
	if left.Count > 0 {
		res.ErrorClass = restic.ClassStorage
		var ae *adminpath.Error
		if errors.As(purgeErr, &ae) && ae.Class != "" {
			res.ErrorClass = ae.Class
		}
		w.log.Warn("repository purge incomplete: objects are still listed under the device prefix",
			attrMethod(adminpath.MethodPurgeRepo), attrDevice(id), attrClass(string(res.ErrorClass)),
			logx.Count("objects_removed", int(res.ObjectsRemoved)), logx.Count("remaining_objects", int(res.RemainingObjects)),
			attrElapsed(res.Elapsed))
		return res, nil
	}
	w.log.Info("repository purge finished", attrMethod(adminpath.MethodPurgeRepo), attrDevice(id),
		logx.Count("objects_removed", int(res.ObjectsRemoved)), logx.Count("remaining_objects", 0), attrElapsed(res.Elapsed))
	return res, nil
}

// refuse logs a refusal (reason only; the device id when it is a valid one)
// and returns it.
func (w *Worker) refuse(deviceID string, r *PurgeRefusedError) error {
	attrs := []any{attrMethod(adminpath.MethodPurgeRepo), attrClass(r.Reason)}
	if deviceID != "" {
		attrs = append(attrs, attrDevice(deviceID))
	}
	w.log.Warn("repository purge refused; nothing was run", attrs...)
	return r
}

// purgeFailed logs a purge that stopped on an error, with its code and class.
func (w *Worker) purgeFailed(deviceID string, err error) {
	class := "context"
	var ae *adminpath.Error
	if errors.As(err, &ae) {
		class = ae.Code
		if ae.Class != "" {
			class = string(ae.Class)
		}
	}
	w.log.Warn("repository purge failed", attrMethod(adminpath.MethodPurgeRepo), attrDevice(deviceID), attrClass(class))
}

// purgeLocation builds the rclone location of a device's repository,
// <remote>:<bucket>/<prefix>/<device id>, from the worker's configuration and a
// device id only, and returns it with the cleaned prefix. It refuses
// (*PurgeRefusedError) unless:
//
//   - the device id is a canonical ULID (adminpath.PurgeRepoRequest.Validate);
//   - the remote is a plain rclone remote name: [A-Za-z0-9_.-], starting with a
//     letter or digit, at least two characters (a one-letter remote reads as a
//     drive letter on Windows; ',' would open rclone's connection-string
//     syntax);
//   - the bucket and the prefix, with leading and trailing '/' dropped as
//     restic.AdminRepository drops them, are both non-empty and every segment
//     passes cleanSegment;
//   - the result is unchanged by path.Clean, has exactly the bucket's and the
//     prefix's segments plus one, ends in the device id, and equals the
//     location restic.AdminRepository gives restic for the same device, so the
//     purge addresses exactly the repository the worker's restic runs use.
func purgeLocation(remote, bucket, prefix, deviceID string) (loc, cleanPrefix string, refusal *PurgeRefusedError) {
	if (adminpath.PurgeRepoRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: deviceID}}).Validate() != nil {
		return "", "", purgeRefused(PurgeRefusedDeviceID)
	}
	if !validRemoteName(remote) {
		return "", "", purgeRefused(PurgeRefusedRemote)
	}
	b, ok := cleanPart(bucket)
	if !ok || b == "" {
		return "", "", purgeRefused(PurgeRefusedBucket)
	}
	p, ok := cleanPart(prefix)
	if !ok || p == "" {
		return "", "", purgeRefused(PurgeRefusedPrefix)
	}
	rel := b + "/" + p + "/" + deviceID
	segs := strings.Split(rel, "/")
	want := strings.Count(b, "/") + strings.Count(p, "/") + 3
	if path.Clean(rel) != rel || strings.HasPrefix(rel, "/") || len(segs) != want || segs[len(segs)-1] != deviceID {
		return "", "", purgeRefused(PurgeRefusedLocation)
	}
	for _, s := range segs {
		if !cleanSegment(s) {
			return "", "", purgeRefused(PurgeRefusedLocation)
		}
	}
	repo, err := restic.AdminRepository(remote, bucket, prefix, deviceID)
	if err != nil || repo != "rclone:"+remote+":"+rel {
		return "", "", purgeRefused(PurgeRefusedLocation)
	}
	return remote + ":" + rel, p, nil
}

// validRemoteName accepts an rclone remote name of 2 to 64 characters of
// [A-Za-z0-9_.-] that starts with a letter or a digit (so it can never be read
// as a flag).
func validRemoteName(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if alnum || (i > 0 && (c == '_' || c == '.' || c == '-')) {
			continue
		}
		return false
	}
	return true
}

// cleanPart drops leading and trailing '/' (as restic.AdminRepository does)
// and reports whether every remaining segment passes cleanSegment. An empty
// part is ("", true): the caller decides whether empty is allowed.
func cleanPart(s string) (string, bool) {
	s = strings.Trim(s, "/")
	if s == "" {
		return "", true
	}
	for _, seg := range strings.Split(s, "/") {
		if !cleanSegment(seg) {
			return "", false
		}
	}
	return s, true
}

// cleanSegment accepts one path segment: non-empty, not "." or "..", no
// control character, no wildcard or brace (* ? [ ] { }), no backslash, no
// colon, and no white space at either end.
func cleanSegment(s string) bool {
	if s == "" || s == "." || s == ".." || strings.TrimSpace(s) != s {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
		switch c {
		case '*', '?', '[', ']', '{', '}', '\\', ':':
			return false
		}
	}
	return true
}

// purgeParent is the parent of a purge location: loc without its last segment,
// "/<device id>" (<remote>:<bucket>/<prefix>). purgeLocation has already
// proved that loc ends in exactly that segment.
func purgeParent(loc, deviceID string) string {
	return strings.TrimSuffix(loc, "/"+deviceID)
}

// purgeEligible checks the device record: it must exist, be retired, be marked
// for purge (store.Device.Purging: devices.purge_requested_at, set by
// store.RequestDevicePurge with its audit row), and have been enrolled under
// the prefix the worker purges (devices.repo_path is <prefix>/<device id>/,
// written at enrollment from the console's storage target). A repo_path
// mismatch would make the purge delete an empty location and report success
// while the device's data stays where it was written.
func (w *Worker) purgeEligible(ctx context.Context, deviceID, prefix string) error {
	dev, err := w.st.GetDevice(ctx, store.ScopeAll(), deviceID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return purgeRefused(PurgeRefusedDeviceUnknown)
	case err != nil:
		return w.storeErr(ctx, err)
	case dev.RetiredAt == nil:
		return purgeRefused(PurgeRefusedNotRetired)
	case !dev.Purging():
		return purgeRefused(PurgeRefusedNotRequested)
	case dev.RepoPath != prefix+"/"+deviceID+"/":
		return purgeRefused(PurgeRefusedRepoPath)
	}
	return nil
}

// purgeClaims holds the devices a purge is running on, per worker: the
// exclusion that does not depend on a maintenance_schedule row.
var purgeClaims = struct {
	mu  sync.Mutex
	ids map[purgeClaimKey]struct{}
}{ids: map[purgeClaimKey]struct{}{}}

type purgeClaimKey struct {
	w  *Worker
	id string
}

// claimPurge claims deviceID for one purge in this worker; false when another
// purge of the device holds the claim. The returned release frees it.
//
// Why in memory: the database lease (leasePurge) needs a maintenance_schedule
// row, and a device can lack one (none is created for it outside enrollment).
// One worker process holds the admin credential and is the only thing that can
// run rclone against the repositories, so a per-device claim inside it
// excludes every concurrent purge of that device, row or no row, without
// writing a schedule row for a retired device. The database lease is still
// taken after the claim, for the scheduler's sake.
func (w *Worker) claimPurge(deviceID string) (release func(), ok bool) {
	k := purgeClaimKey{w: w, id: deviceID}
	purgeClaims.mu.Lock()
	defer purgeClaims.mu.Unlock()
	if _, held := purgeClaims.ids[k]; held {
		return nil, false
	}
	purgeClaims.ids[k] = struct{}{}
	return func() {
		purgeClaims.mu.Lock()
		delete(purgeClaims.ids, k)
		purgeClaims.mu.Unlock()
	}, true
}

// leasePurge takes the per-device lease on maintenance_schedule under a holder
// unique to this call (the worker's holder id plus a random suffix): the
// scheduler, which leases under the bare holder id, cannot step into a running
// purge, and a second purge of the same device cannot either. A lease held by
// anyone else and not yet expired is adminpath.CodeLocked. A device without a
// maintenance_schedule row (none is created for it outside enrollment) has no
// lease to take, and nothing in the scheduler can act on it; the exclusion of
// a second purge is then the in-process claim (claimPurge) that PurgeRepo
// takes first. The returned release clears the lease if it is still this
// call's; it never fails the call.
func (w *Worker) leasePurge(ctx context.Context, deviceID string) (func(), error) {
	var sfx [4]byte
	if _, err := rand.Read(sfx[:]); err != nil {
		return nil, adminpath.NewError(adminpath.CodeInternal, "")
	}
	holder := w.holder + "-purge-" + hex.EncodeToString(sfx[:])
	now := w.clock()
	var got, exists bool
	err := w.st.Write(ctx, func(tx *store.Tx) error {
		res, err := tx.Exec(`UPDATE maintenance_schedule SET lease_holder = ?, lease_until = ?, updated_at = ?
			WHERE device_id = ? AND (lease_holder IS NULL OR lease_until <= ?)`,
			holder, now.Add(purgeLeaseTTL).UnixMilli(), tx.Now().UnixMilli(), deviceID, now.UnixMilli())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if got = n == 1; got {
			return nil
		}
		var one int
		switch err := tx.QueryRow(`SELECT 1 FROM maintenance_schedule WHERE device_id = ?`, deviceID).Scan(&one); {
		case err == nil:
			exists = true
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		return nil
	})
	if err != nil {
		return nil, w.storeErr(ctx, err)
	}
	if !got && exists {
		w.log.Warn("repository purge deferred: the device's maintenance lease is held", attrMethod(adminpath.MethodPurgeRepo),
			attrDevice(deviceID), attrClass(string(restic.ClassLock)))
		return nil, adminpath.NewError(adminpath.CodeLocked, restic.ClassLock)
	}
	if !got {
		return func() {}, nil
	}
	return func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		err := w.st.Write(rctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE maintenance_schedule SET lease_holder = NULL, lease_until = NULL, updated_at = ?
				WHERE device_id = ? AND lease_holder = ?`, tx.Now().UnixMilli(), deviceID, holder)
			return err
		})
		if err != nil {
			w.log.Error("purge lease not released; it expires on its own", attrDevice(deviceID))
		}
	}, nil
}

// rcloneCount is what a listing step extracted: an object count and a byte sum.
type rcloneCount struct {
	Count int64
	Bytes int64
}

// rcloneParent runs `rclone lsjson --dirs-only -- <parent>` on the parent
// <remote>:<bucket>/<prefix> of the device prefix, not recursive, and returns
// nil only when rclone exited 0 with a well-formed JSON list. The entries are
// read and dropped as they stream past (no name is kept); only the shape of
// the answer is checked. A parent that is not found (exit
// rcloneExitDirNotFound) means the configured bucket, prefix or remote does
// not point at the fleet's repositories: storage_unreachable with class
// storage, logged with no value. Any other failure is classified like every
// rclone step (rcloneError); an unreadable list is restic_failed, class parse.
func (w *Worker) rcloneParent(ctx context.Context, rc *runner, deviceID, parent string) error {
	args := []string{"lsjson", "--dirs-only", "--no-modtime", "--no-mimetype", "--", parent}
	pr, err := w.rcloneRun(ctx, rc, deviceID, "list_parent", purgeParentListTimeout, args, func(r io.Reader) error {
		dec := json.NewDecoder(r)
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return errors.New("lsjson: not a list")
		}
		for dec.More() {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
			return errors.New("lsjson: list not closed")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if abort := abortError(ctx, execResult{procResult: pr}); abort != nil {
		return abort
	}
	switch pr.Exit {
	case 0:
		if pr.StdoutErr != nil {
			return adminpath.NewError(adminpath.CodeResticFailed, restic.ClassParse)
		}
		return nil
	case rcloneExitDirNotFound:
		w.log.Warn("repository purge stopped: the configured bucket and prefix were not found on the remote; nothing was purged",
			attrMethod(adminpath.MethodPurgeRepo), attrDevice(deviceID), attrClass(string(restic.ClassStorage)))
		return adminpath.NewError(adminpath.CodeStorageUnreachable, restic.ClassStorage)
	}
	return rcloneError(pr)
}

// rcloneSize runs `rclone size --json -- <loc>`. A prefix that does not exist
// (exit rcloneExitDirNotFound) is a zero count; PurgeRepo calls it only after
// rcloneParent found the parent, so "not found" here is the device prefix
// alone.
func (w *Worker) rcloneSize(ctx context.Context, rc *runner, deviceID, loc string) (rcloneCount, error) {
	var out rcloneCount
	pr, err := w.rcloneRun(ctx, rc, deviceID, "size", purgeListTimeout, []string{"size", "--json", "--", loc}, func(r io.Reader) error {
		var doc struct {
			Count *int64 `json:"count"`
			Bytes *int64 `json:"bytes"`
		}
		if err := json.NewDecoder(io.LimitReader(r, purgeStdoutCap)).Decode(&doc); err != nil {
			return err
		}
		if doc.Count == nil || doc.Bytes == nil || *doc.Count < 0 || *doc.Bytes < 0 {
			return errors.New("size: count or bytes missing or negative")
		}
		out = rcloneCount{Count: *doc.Count, Bytes: *doc.Bytes}
		return nil
	})
	if err != nil {
		return rcloneCount{}, err
	}
	return out, listOutcome(ctx, pr)
}

// rcloneRemaining runs `rclone lsjson -R --files-only` on the prefix and counts
// the objects and their bytes as the listing streams past; no name is kept. A
// prefix that does not exist (exit rcloneExitDirNotFound) is a zero count.
func (w *Worker) rcloneRemaining(ctx context.Context, rc *runner, deviceID, loc string) (rcloneCount, error) {
	var out rcloneCount
	args := []string{"lsjson", "-R", "--files-only", "--no-modtime", "--no-mimetype", "--", loc}
	pr, err := w.rcloneRun(ctx, rc, deviceID, "lsjson", purgeListTimeout, args, func(r io.Reader) error {
		dec := json.NewDecoder(r)
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return errors.New("lsjson: not a list")
		}
		var n rcloneCount
		for dec.More() {
			var item struct {
				Size int64 `json:"Size"`
			}
			if err := dec.Decode(&item); err != nil {
				return err
			}
			n.Count++
			if item.Size > 0 {
				n.Bytes += item.Size
			}
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
			return errors.New("lsjson: list not closed")
		}
		out = n
		return nil
	})
	if err != nil {
		return rcloneCount{}, err
	}
	if pr.Exit == rcloneExitDirNotFound && !pr.TimedOut && !pr.Canceled {
		return rcloneCount{}, nil
	}
	return out, listOutcome(ctx, pr)
}

// listOutcome turns a finished size or lsjson run into nil (exit 0 with a
// decoded stdout, or exit rcloneExitDirNotFound) or the error the call
// returns.
func listOutcome(ctx context.Context, pr procResult) error {
	if abort := abortError(ctx, execResult{procResult: pr}); abort != nil {
		return abort
	}
	switch pr.Exit {
	case 0:
		if pr.StdoutErr != nil {
			return adminpath.NewError(adminpath.CodeResticFailed, restic.ClassParse)
		}
		return nil
	case rcloneExitDirNotFound:
		return nil
	}
	return rcloneError(pr)
}

// rcloneError is the admin-path error of a failed rclone run, by the path-free
// class of its stderr: the network, tls and storage classes are
// storage_unreachable, anything else restic_failed ("the repository operation
// failed") with the class, unknown when stderr said nothing classifiable.
func rcloneError(pr procResult) error {
	class := pr.Stderr
	if class == "" {
		class = restic.ClassUnknown
	}
	if storageClass(class) {
		return adminpath.NewError(adminpath.CodeStorageUnreachable, class)
	}
	return adminpath.NewError(adminpath.CodeResticFailed, class)
}

// rcloneRun starts one rclone child the way exec starts restic: it waits for a
// concurrency slot (Config.MaxConcurrent, shared with restic), runs the
// program under timeout with the worker's minimal environment (baseEnv: no
// RESTIC_* entry, no repository password), records the storage verdict for
// Ping and logs the step, exit code and elapsed time. The returned error is
// non-nil only when the child could not run at all.
func (w *Worker) rcloneRun(ctx context.Context, rc *runner, deviceID, step string, timeout time.Duration,
	args []string, onStdout func(io.Reader) error) (procResult, error) {
	select {
	case w.sem <- struct{}{}:
		defer func() { <-w.sem }()
	case <-ctx.Done():
		return procResult{}, ctx.Err()
	}
	t0 := w.clock()
	pr, err := rc.run(ctx, timeout, args, w.baseEnv(), onStdout)
	if err != nil {
		return procResult{}, w.ctxOr(ctx, adminpath.NewError(adminpath.CodeInternal, ""))
	}
	res := execResult{procResult: pr, Elapsed: w.clock().Sub(t0)}
	w.noteStorage(res)
	w.log.Info("rclone run finished", attrMethod(adminpath.MethodPurgeRepo), logx.Str("step", step), attrDevice(deviceID),
		attrExit(pr.Exit), attrElapsed(res.Elapsed))
	if storageClass(pr.Stderr) {
		w.log.Warn("rclone could not reach the storage", attrMethod(adminpath.MethodPurgeRepo), logx.Str("step", step),
			attrDevice(deviceID), attrClass(string(pr.Stderr)), attrExit(pr.Exit), w.attrStorageHost())
	}
	return pr, nil
}
