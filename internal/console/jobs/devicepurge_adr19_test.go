package jobs

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// ADR 0019: the device-purge job. A device's row is deleted only when the worker's answer had no
// error, no error class and RemainingObjects == 0; anything else is a recorded attempt that leaves
// the device purging.

// fakePurger answers purge_repo per device id.
type fakePurger struct {
	mu    sync.Mutex
	res   map[string]adminpath.PurgeRepoResult
	err   map[string]error
	all   error // when set, every call fails with it
	calls []string
}

func (f *fakePurger) PurgeRepo(_ context.Context, req adminpath.PurgeRepoRequest) (adminpath.PurgeRepoResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.DeviceID)
	if f.all != nil {
		return adminpath.PurgeRepoResult{}, f.all
	}
	if err := f.err[req.DeviceID]; err != nil {
		return adminpath.PurgeRepoResult{}, err
	}
	return f.res[req.DeviceID], nil
}

func (f *fakePurger) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == id {
			n++
		}
	}
	return n
}

type purgeRig struct {
	e      *env
	logs   *syncBuf
	p      *fakePurger
	job    *DevicePurge
	owner  store.AuditActor
	hosts  map[string]string // device id -> host name, for the leak checks
	spare  store.Device      // SPARE-PC-09: purging in the fixtures, last attempt 5 minutes ago (storage_unreachable)
	retire store.Device      // OLD-RECEPTION-PC: retired, not purging
}

func newPurgeRig(t *testing.T) *purgeRig {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()
	admin, err := e.st.GetAdminByUsername(ctx, store.FixtureAdminUsername)
	if err != nil {
		t.Fatal(err)
	}
	r := &purgeRig{e: e, logs: &syncBuf{}, p: &fakePurger{res: map[string]adminpath.PurgeRepoResult{}, err: map[string]error{}},
		owner: store.AuditActor{AdminID: admin.ID, Username: admin.Username, Method: "api"}, hosts: map[string]string{}}
	r.job = &DevicePurge{Store: e.st, Purger: r.p, ArtifactsDir: e.dir, Log: logx.New(r.logs, logx.LevelDebug)}
	l, err := e.st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range l.Items {
		r.hosts[d.ID] = d.Hostname
		switch d.Hostname {
		case "SPARE-PC-09":
			r.spare = d
		case "OLD-RECEPTION-PC":
			r.retire = d
		}
	}
	if !r.spare.Purging() || r.retire.RetiredAt == nil || r.retire.Purging() {
		t.Fatalf("fixtures changed: spare purging=%v, old-reception retired=%v purging=%v", r.spare.Purging(), r.retire.RetiredAt != nil, r.retire.Purging())
	}
	return r
}

func (r *purgeRig) request(t *testing.T, d store.Device) {
	t.Helper()
	if _, err := r.e.st.RequestDevicePurge(context.Background(), store.ScopeAll(), d.ID, "end of lease", r.owner); err != nil {
		t.Fatal(err)
	}
}

func (r *purgeRig) device(t *testing.T, id string) (store.Device, bool) {
	t.Helper()
	d, err := r.e.st.GetDevice(context.Background(), store.ScopeAll(), id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Device{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return d, true
}

type auditRow struct{ action, objectID, label, details string }

func (r *purgeRig) audits(t *testing.T, action string) []auditRow {
	t.Helper()
	var out []auditRow
	err := r.e.st.Read(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT action, COALESCE(object_id, ''), COALESCE(actor_label, ''), COALESCE(details, '') FROM audit_events WHERE action = ? ORDER BY rowid`, action)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a auditRow
			var det sql.NullString
			if err := rows.Scan(&a.action, &a.objectID, &a.label, &det); err != nil {
				return err
			}
			a.details = det.String
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// noHostNames fails when any device host name appears in the captured logs or in any audit row.
func (r *purgeRig) noHostNames(t *testing.T) {
	t.Helper()
	logs := r.logs.String()
	var all strings.Builder
	_ = r.e.st.Read(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT COALESCE(action,'') || '|' || COALESCE(object_id,'') || '|' || COALESCE(actor_label,'') || '|' || COALESCE(details,'') FROM audit_events WHERE action LIKE 'device.purge.%'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			all.WriteString(s + "\n")
		}
		return rows.Err()
	})
	for _, h := range r.hosts {
		if strings.Contains(logs, h) {
			t.Errorf("host name %q in the job's log:\n%s", h, logs)
		}
		if strings.Contains(all.String(), h) {
			t.Errorf("host name %q in a purge audit row:\n%s", h, all.String())
		}
	}
}

func TestAdr19JobNameMatchesTheBanner(t *testing.T) {
	if NameDevicePurge != health.PurgeJob {
		t.Fatalf("jobs.NameDevicePurge %q != health.PurgeJob %q: the banner would never see the job's failures", NameDevicePurge, health.PurgeJob)
	}
	j := (&DevicePurge{}).Job()
	if j.Name != NameDevicePurge || j.Every != DefaultDevicePurgeEvery || j.Lease < 2*time.Hour {
		t.Fatalf("Job() = %s every %v lease %v; the lease must outlast one purge_repo call (2 h)", j.Name, j.Every, j.Lease)
	}
}

func TestAdr19SuccessDeletesTheRowAndAuditsDone(t *testing.T) {
	r := newPurgeRig(t)
	r.request(t, r.retire)
	r.p.res[r.retire.ID] = adminpath.PurgeRepoResult{ObjectsRemoved: 12, BytesRemoved: 34567}
	if err := r.job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := r.device(t, r.retire.ID); ok {
		t.Fatal("the device row is still there after a clean purge")
	}
	done := r.audits(t, "device.purge.done")
	if len(done) != 1 || done[0].objectID != r.retire.ID || !strings.Contains(done[0].details, `"objects":12`) || !strings.Contains(done[0].details, `"bytes":34567`) {
		t.Fatalf("device.purge.done rows = %+v", done)
	}
	// SPARE-PC-09's last attempt is 5 minutes old: inside the backoff, so it was not tried.
	if n := r.p.count(r.spare.ID); n != 0 {
		t.Fatalf("a device inside its backoff was tried %d times", n)
	}
	r.noHostNames(t)
}

func TestAdr19FailuresLeaveTheDeviceAndRecordTheClass(t *testing.T) {
	cases := []struct {
		name      string
		res       adminpath.PurgeRepoResult
		err       error
		wantClass string
	}{
		{"transport", adminpath.PurgeRepoResult{}, adminpath.ErrUnavailable, PurgeClassWorkerUnreachable},
		{"method error", adminpath.PurgeRepoResult{}, adminpath.NewError(adminpath.CodeStorageUnreachable, ""), adminpath.CodeStorageUnreachable},
		{"refused by the worker", adminpath.PurgeRepoResult{}, adminpath.NewError(adminpath.CodeInvalidRequest, ""), adminpath.CodeInvalidRequest},
		{"internal with a restic class", adminpath.PurgeRepoResult{}, adminpath.NewError(adminpath.CodeInternal, restic.ClassUnknown), string(restic.ClassUnknown)},
		{"timeout", adminpath.PurgeRepoResult{}, context.DeadlineExceeded, ClassTimeout},
		{"error class with nil error", adminpath.PurgeRepoResult{ObjectsRemoved: 3, RemainingObjects: 2, ErrorClass: restic.ClassUnknown}, nil, string(restic.ClassUnknown)},
		{"objects left with nil error and no class", adminpath.PurgeRepoResult{ObjectsRemoved: 3, RemainingObjects: 1}, nil, PurgeClassObjectsRemaining},
		{"negative counts", adminpath.PurgeRepoResult{ObjectsRemoved: -1}, nil, PurgeClassBadResult},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPurgeRig(t)
			r.request(t, r.retire)
			r.p.res[r.retire.ID], r.p.err[r.retire.ID] = tc.res, tc.err
			if err := r.job.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v (a recorded device failure is not a job failure)", err)
			}
			d, ok := r.device(t, r.retire.ID)
			if !ok {
				t.Fatal("the device row was deleted after a failed attempt")
			}
			if !d.Purging() || d.PurgeLastErrorClass != tc.wantClass || d.PurgeLastAttemptAt == nil {
				t.Fatalf("device after the attempt: purging=%v class=%q attempt=%v; want class %q", d.Purging(), d.PurgeLastErrorClass, d.PurgeLastAttemptAt, tc.wantClass)
			}
			if done := r.audits(t, "device.purge.done"); len(done) != 0 {
				t.Fatalf("device.purge.done written after a failed attempt: %+v", done)
			}
			if !strings.Contains(r.logs.String(), tc.wantClass) {
				t.Errorf("the failure is not logged by its class %q:\n%s", tc.wantClass, r.logs.String())
			}
			r.noHostNames(t)
		})
	}
}

func TestAdr19OneDeviceFailingDoesNotStopTheOthers(t *testing.T) {
	r := newPurgeRig(t)
	r.request(t, r.retire)
	r.e.now = r.e.now.Add(time.Hour) // SPARE-PC-09's backoff has passed
	r.p.err[r.spare.ID] = adminpath.ErrUnavailable
	r.p.res[r.retire.ID] = adminpath.PurgeRepoResult{ObjectsRemoved: 1, BytesRemoved: 1}
	if err := r.job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := r.device(t, r.retire.ID); ok {
		t.Error("the healthy device was not purged after another one failed")
	}
	if d, ok := r.device(t, r.spare.ID); !ok || d.PurgeLastErrorClass != PurgeClassWorkerUnreachable {
		t.Errorf("the failing device: present=%v class=%q", ok, d.PurgeLastErrorClass)
	}
}

func TestAdr19BackoffUsesTheLastAttempt(t *testing.T) {
	r := newPurgeRig(t)
	r.request(t, r.retire)
	r.p.all = adminpath.ErrUnavailable
	ctx := context.Background()
	if err := r.job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := r.p.count(r.retire.ID); n != 1 {
		t.Fatalf("first run tried %d times, want 1", n)
	}
	r.e.now = r.e.now.Add(DefaultDevicePurgeBackoff - time.Second)
	_ = r.job.Run(ctx)
	if n := r.p.count(r.retire.ID); n != 1 {
		t.Fatalf("a run inside the backoff tried again (%d calls)", n)
	}
	r.e.now = r.e.now.Add(2 * time.Second)
	_ = r.job.Run(ctx)
	if n := r.p.count(r.retire.ID); n != 2 {
		t.Fatalf("a run after the backoff did not try again (%d calls)", n)
	}
}

func TestAdr19StagedArtifactsAreRemovedBeforeTheRow(t *testing.T) {
	r := newPurgeRig(t)
	ctx := context.Background()
	staged := "restore-01JAAAAAAAAAAAAAAAAAAAAAAA.zip"
	if err := os.WriteFile(filepath.Join(r.e.dir, staged), []byte("old archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	purgeAfter := r.e.now.Add(time.Hour)
	if _, err := r.e.st.CreateRestoreRequest(ctx, store.ScopeAll(), store.RestoreRequest{DeviceID: r.retire.ID, SnapshotID: strings.Repeat("ab", 32), Mode: "download_zip",
		Status: "ready", RequestedBy: r.owner.AdminID, ArtifactRef: staged, ExpiresAt: r.e.now.Add(time.Hour), PurgeAfter: &purgeAfter}); err != nil {
		t.Fatal(err)
	}
	r.request(t, r.retire)

	// The archive cannot be deleted (a directory of that name is in the way): the row stays.
	if err := os.Remove(filepath.Join(r.e.dir, staged)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.e.dir, staged, "busy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if d, ok := r.device(t, r.retire.ID); !ok || d.PurgeLastErrorClass != PurgeClassArtifactCleanup {
		t.Fatalf("an archive that could not be deleted: present=%v class=%q", ok, d.PurgeLastErrorClass)
	}

	// The way is clear: the archive goes, then the row.
	if err := os.RemoveAll(filepath.Join(r.e.dir, staged)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.e.dir, staged), []byte("old archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.e.now = r.e.now.Add(time.Hour)
	if err := r.job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.device(t, r.retire.ID); ok {
		t.Fatal("the device row stayed after the archive was deleted and storage reported it gone")
	}
	if _, err := os.Stat(filepath.Join(r.e.dir, staged)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged archive is still there: %v", err)
	}
}

func TestAdr19NoPurgerFailsTheJobLoudly(t *testing.T) {
	r := newPurgeRig(t)
	r.job.Purger = nil
	err := r.job.Run(context.Background())
	var ce *ClassError
	if !errors.As(err, &ce) || ce.Class != PurgeClassNotWired {
		t.Fatalf("Run without a client = %v, want class %s", err, PurgeClassNotWired)
	}
}

func TestAdr19StoppingRecordsNothingAndDeletesNothing(t *testing.T) {
	r := newPurgeRig(t)
	r.request(t, r.retire)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.p.err[r.retire.ID] = context.Canceled
	_ = r.job.Run(ctx)
	d, ok := r.device(t, r.retire.ID)
	if !ok || d.PurgeLastAttemptAt != nil {
		t.Fatalf("a run stopped by shutdown: present=%v attempt=%v; want the device untouched", ok, d.PurgeLastAttemptAt)
	}
}

// The fail-loudly rule end to end: a worker that does not answer raises "Purge not finished".
func TestAdr19UnreachableWorkerRaisesTheBanner(t *testing.T) {
	r := newPurgeRig(t)
	r.request(t, r.retire)
	r.p.all = adminpath.ErrUnavailable
	runner := NewRunner(r.e.st, "adr19-test", logx.New(r.logs, logx.LevelDebug))
	runner.Add(r.job.Job())
	if ran, err := runner.RunOnce(context.Background(), NameDevicePurge); !ran || err != nil {
		t.Fatalf("RunOnce = %v, %v", ran, err)
	}
	ev := &health.Evaluator{Store: r.e.st, Clock: func() time.Time { return r.e.now }, PurgeBanner: true}
	f, err := ev.Fleet(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	var got *string
	for _, b := range f.View.Banners {
		if b.ID == health.BannerPurgeNotFinished {
			got = &b.Text
			if b.Count != 2 || b.Subject != r.spare.ID || !strings.Contains(b.Text, PurgeClassWorkerUnreachable) {
				t.Errorf("banner = %+v; want count 2 (both purging devices), the oldest request's id, and the class", b)
			}
		}
	}
	if got == nil {
		t.Fatalf("no %s banner after the worker was unreachable: %+v", health.BannerPurgeNotFinished, f.View.Banners)
	}
	for _, h := range r.hosts {
		if strings.Contains(*got, h) {
			t.Errorf("the banner names host %q: %s", h, *got)
		}
	}
	r.noHostNames(t)
}
