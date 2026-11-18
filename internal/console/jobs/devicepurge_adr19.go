package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The device-purge job (ADR 0019). An owner's purge request marks a retired device "purging"
// (store.RequestDevicePurge); this job does the rest, on its interval and whenever a request
// wakes it (the admin API runs it at once after a request):
//
//  1. list the purging devices (store.ListDevicesPurging, scope All);
//  2. for each device whose last attempt is older than Backoff (or that has none), ask the
//     maintenance worker to remove the repository prefix (adminpath purge_repo);
//  3. the answer is a success ONLY when the call returned no error, no error class, and
//     RemainingObjects == 0. A nil error alone is never enough: the worker reports objects it
//     could not remove as a result, not an error;
//  4. on success, delete any restore archive an earlier version staged for the device
//     (restore_requests.artifact_ref), then store.CompleteDevicePurge with the counts: that
//     deletes the device row and writes device.purge.done. This is the only path that
//     deletes a device;
//  5. on anything else, store.RecordDevicePurgeAttempt with a fixed class word: the device stays
//     purging, the banner "Purge not finished" (health) is raised, and the next attempt waits
//     out Backoff.
//
// One device failing does not stop the others. There is no force path and no skip. The job
// never reads or logs a host name: device ids, counts and class words only.

// NameDevicePurge is the job's name (job_state.name); health.PurgeJob is the same string.
const NameDevicePurge = "device-purge"

const (
	// DefaultDevicePurgeEvery is the job's interval.
	DefaultDevicePurgeEvery = 5 * time.Minute
	// DefaultDevicePurgeBackoff is how long after an attempt the same device is tried again.
	DefaultDevicePurgeBackoff = 15 * time.Minute
	// devicePurgeLease outlasts one purge_repo call (adminpath allows it two hours), so a second
	// run cannot take the lease while the first is still waiting on the worker.
	devicePurgeLease = 2*time.Hour + 15*time.Minute
)

// Classes the job records on a device (purge_last_error_class) or returns as its own failure.
const (
	PurgeClassWorkerUnreachable = "worker_unreachable" // the admin-path socket did not answer
	PurgeClassObjectsRemaining  = "objects_remaining"  // the call reported success but objects are still listed
	PurgeClassBadResult         = "bad_result"         // the worker answered counts that cannot be true
	PurgeClassArtifactCleanup   = "artifact_cleanup"   // a staged restore archive of the device could not be deleted
	PurgeClassComplete          = "complete_failed"    // the row could not be deleted after storage reported it gone
	PurgeClassNotWired          = "not_wired"          // the job has no admin-path client
	PurgeClassNotRecorded       = "not_recorded"       // a failed attempt could not be written to the device
)

// purgeClassRE is what store.RecordDevicePurgeAttempt accepts; anything else is recorded as internal.
var purgeClassRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// DevicePurge is the device-purge job.
type DevicePurge struct {
	Store *store.Store
	// Purger is the admin-path client (adminpath.Client, or the fixtures backend). Nil: every run
	// fails with class not_wired, which the banner shows.
	Purger adminpath.RepoPurger
	// ArtifactsDir is restores.Service.ArtifactsDir: where versions before ADR 0008 staged
	// restore archives. Empty: no archive can exist on disk.
	ArtifactsDir string
	Log          *logx.Logger
	// Every and Backoff override the defaults (tests).
	Every, Backoff time.Duration
}

// Job returns the schedule for the runner.
func (j *DevicePurge) Job() Job {
	every := j.Every
	if every <= 0 {
		every = DefaultDevicePurgeEvery
	}
	return Job{Name: NameDevicePurge, Every: every, Lease: devicePurgeLease, Run: j.Run}
}

func (j *DevicePurge) log() *logx.Logger {
	if j.Log != nil {
		return j.Log
	}
	return logx.Default()
}

func (j *DevicePurge) backoff() time.Duration {
	if j.Backoff > 0 {
		return j.Backoff
	}
	return DefaultDevicePurgeBackoff
}

// Run makes one pass over the purging devices. It returns an error (and the runner records the
// job as failed) only when it could not do its work: no client, the list unreadable, or a
// failure it could not record on the device. A device whose storage step failed is a recorded
// attempt, not a job failure.
func (j *DevicePurge) Run(ctx context.Context) error {
	if j.Purger == nil {
		return &ClassError{Class: PurgeClassNotWired}
	}
	list, err := j.Store.ListDevicesPurging(ctx, store.ScopeAll())
	if err != nil {
		return &ClassError{Class: ClassStore, Err: err}
	}
	var unrecorded error
	for _, d := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !j.due(d) {
			continue
		}
		if err := j.purgeOne(ctx, d.ID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			unrecorded = err
		}
	}
	return unrecorded
}

// due reports whether the device's last attempt is at least Backoff old (or there is none).
func (j *DevicePurge) due(d store.Device) bool {
	if d.PurgeLastAttemptAt == nil {
		return true
	}
	return j.Store.Now().Sub(*d.PurgeLastAttemptAt) >= j.backoff()
}

// purgeOne makes one attempt on one device. It returns an error only when the outcome could not
// be written down (or the job is stopping): the caller reports that as the job's failure.
func (j *DevicePurge) purgeOne(ctx context.Context, id string) error {
	res, err := j.Purger.PurgeRepo(ctx, adminpath.PurgeRepoRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: id}})
	if err != nil && ctx.Err() != nil {
		// The console is stopping: nothing is recorded and nothing is deleted. The device stays
		// purging and the next run tries again.
		return ctx.Err()
	}
	if class := purgeOutcome(res, err); class != "" {
		return j.fail(ctx, id, class)
	}
	// Storage reports the repository gone. Staged archives first: once the row is deleted, nothing
	// names them any more.
	if err := j.removeArtifacts(ctx, id); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return j.fail(ctx, id, PurgeClassArtifactCleanup)
	}
	if err := j.Store.CompleteDevicePurge(ctx, store.ScopeAll(), id, res.ObjectsRemoved, res.BytesRemoved); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNotPurging) {
			return nil // another run completed it between the list and now
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return j.fail(ctx, id, PurgeClassComplete)
	}
	j.log().Warn("device purged: repository removed and record deleted", logx.ID("device_id", id),
		logx.Count("objects", int(res.ObjectsRemoved)), logx.Count("bytes", int(res.BytesRemoved)))
	return nil
}

// purgeOutcome is the class of a failed attempt, or "" for the one outcome that is a success:
// no error, no error class, nothing left, and counts that can be true.
func purgeOutcome(res adminpath.PurgeRepoResult, err error) string {
	switch {
	case err != nil:
		return purgeErrClass(err)
	case res.ErrorClass != "":
		return string(res.ErrorClass)
	case res.RemainingObjects != 0:
		return PurgeClassObjectsRemaining
	case res.ObjectsRemoved < 0 || res.BytesRemoved < 0:
		return PurgeClassBadResult
	}
	return ""
}

// purgeErrClass names an admin-path error by a fixed word: the error's code (or its restic class
// when the code says only "internal"), the transport, or a timeout.
func purgeErrClass(err error) string {
	var ae *adminpath.Error
	var ve *adminpath.ValidationError
	switch {
	case errors.As(err, &ae):
		if ae.Code == adminpath.CodeInternal && ae.Class != "" {
			return string(ae.Class)
		}
		return ae.Code
	case errors.Is(err, adminpath.ErrUnavailable):
		return PurgeClassWorkerUnreachable
	case errors.As(err, &ve):
		return adminpath.CodeInvalidRequest
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	}
	return ClassInternal
}

// fail records a failed attempt on the device and logs it by class. The device stays purging.
func (j *DevicePurge) fail(ctx context.Context, id, class string) error {
	if !purgeClassRE.MatchString(class) {
		class = ClassInternal
	}
	j.log().Warn("device purge attempt failed; the device stays purging", logx.ID("device_id", id), logx.Class(class))
	if err := j.Store.RecordDevicePurgeAttempt(context.WithoutCancel(ctx), store.ScopeAll(), id, class); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNotPurging) {
			return nil // the device is gone or no longer purging: nothing left to record
		}
		j.log().Error("device purge attempt not recorded", logx.ID("device_id", id), logx.Class(PurgeClassNotRecorded))
		return &ClassError{Class: PurgeClassNotRecorded, Err: err}
	}
	return nil
}

// removeArtifacts deletes every restore archive an earlier version staged for the device
// (restore_requests.artifact_ref, ADR 0008). A name that is not a bare file name inside
// ArtifactsDir is not ours and is left alone, as restores.Service.Purge does; a file already
// gone is fine. Any other failure keeps the device purging: its rows are the only thing that
// names the file.
func (j *DevicePurge) removeArtifacts(ctx context.Context, id string) error {
	for cursor := ""; ; {
		l, err := j.Store.ListRestoreRequests(ctx, store.ScopeAll(), id, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, r := range l.Items {
			if err := removeStaged(j.ArtifactsDir, r.ArtifactRef); err != nil {
				return err
			}
		}
		if l.NextCursor == "" {
			return nil
		}
		cursor = l.NextCursor
	}
}

func removeStaged(dir, ref string) error {
	if ref == "" || dir == "" || ref != filepath.Base(ref) || strings.ContainsAny(ref, `/\`) || ref == "." || ref == ".." {
		return nil
	}
	if err := os.Remove(filepath.Join(dir, ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
