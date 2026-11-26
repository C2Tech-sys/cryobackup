package fixtures

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-G: the restore world of fixtures mode (ADR 0027, ADR 0028, ADR 0030). With it the restore
// wizard and the restore status page show every state the operator can meet without a device, a
// worker or a repository: a folder of 2 000 entries to page through, nested folders to select and
// collapse, a link the zip leaves out, a device whose agent is too old for a device restore, and a
// restore row in every status and failure class the status page words.
//
// The rows are written with SQL, as the M4 world is, so their times can lie in the past (the store
// stamps its own clock and the fixtures clock is frozen). Every value is one the console itself
// writes: the restore_requests CHECK words, the progress documents of store.DeviceRestoreProgress
// and of a completed download, the class words of api.RestoreResultClass. Nothing here is a real
// name: the files, folders and hosts are invented.

// P22 fixture names: the tests, the design page and the screenshots refer to them.
const (
	// P22BigFolder holds P22BigCount files: the wizard pages it 500 at a time.
	P22BigFolder = "/D/Practice/Scans"
	P22BigCount  = 2000
	// P22FormsFolder holds nested folders, a long name and a link (left out of a zip).
	P22FormsFolder = "/D/Practice/Forms"
	// P22LongName is a file name long enough to wrap on a phone.
	P22LongName = "Signed and scanned consent form for the annual eye examination, second copy for the practice records 2026.pdf"
	// P22OldAgentHost runs P22OldAgentVersion, older than restores.MinDeviceRestoreAgent: the
	// wizard offers it a download only.
	P22OldAgentHost    = "LT-SURGERY-02"
	P22OldAgentVersion = "0.3.2"
)

// P22Files is the fake snapshot of the restore world: DefaultFiles plus a second drive with a big
// folder, nested folders and a link.
func P22Files() []File {
	m := Now().Add(-26 * time.Hour)
	out := DefaultFiles()
	out = append(out,
		File{Path: P22FormsFolder + "/Intake/new-patient.pdf", Content: []byte("intake form (fixture)\n"), Mtime: m},
		File{Path: P22FormsFolder + "/Intake/returning-patient.pdf", Content: []byte("intake form (fixture)\n"), Mtime: m},
		File{Path: P22FormsFolder + "/Consent/consent-2026.pdf", Content: []byte("consent form (fixture)\n"), Mtime: m},
		File{Path: P22FormsFolder + "/" + P22LongName, Content: []byte("long name (fixture)\n"), Mtime: m},
		File{Path: P22FormsFolder + "/readme.txt", Content: []byte("forms folder (fixture)\n"), Mtime: m},
		File{Path: P22FormsFolder + "/latest", Mtime: m, Skip: adminpath.SkipSymlink},
	)
	for i := 1; i <= P22BigCount; i++ {
		out = append(out, File{Path: fmt.Sprintf("%s/scan-%04d.pdf", P22BigFolder, i), Content: []byte("scan (fixture)\n"), Mtime: m.Add(-time.Duration(i) * time.Minute)})
	}
	return out
}

// P22Restore is one restore row of the restore world.
type P22Restore struct {
	// Key names the row in the tests and the screenshots (the file slug).
	Key string
	// Title says what the row shows, for the design page and the screenshot README.
	Title      string
	Host       string
	Mode       string // store.RestoreMode*
	Overwrite  string // store.RestoreOverwrite*
	Status     string
	ErrorClass string
	// Count is the number of selected items.
	Count int
	// Progress is the row's progress document (nil: none).
	Progress any
	Bytes    *int64
	// Age is how long ago the row was created; Started and Finished, when set, how long ago those happened.
	Age, Started, Finished time.Duration
	// Expired rows lie past their expiry; Wrapped rows hold a selection (an open download, an active device restore).
	Expired, Wrapped bool
	// ID is the row id (P22RestoreID of its index).
	ID string
}

// P22RestoreID is the id of the n-th row of P22Restores (26 characters, as every id of the schema).
func P22RestoreID(n int) string { return fmt.Sprintf("01JP22G0RSTR%014d", n) }

func i64(n int64) *int64 { return &n }

// P22Restores lists the rows SeedP22 writes, one per status and failure class of the status page.
// Each device restore that is still active sits on its own device (one active restore per device).
func P22Restores() []P22Restore {
	h, m := time.Hour, time.Minute
	zipDone := func(skipped adminpath.DumpSkipped) map[string]any {
		return map[string]any{"bytes": 18_432_000, "sha256": "5e1f7c0d2a9b4c6e8f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6a", "skipped": skipped, "selection_count": 3,
			"selection_hash": "9b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c"}
	}
	rows := []P22Restore{
		{Key: "zip-ready", Title: "Download ready", Host: "FRONTDESK-H1", Mode: store.RestoreModeDownloadZip, Overwrite: store.RestoreOverwriteIfChanged, Status: "ready", Count: 3, Age: 4 * m, Wrapped: true},
		{Key: "zip-delivered", Title: "Downloaded", Host: "FRONTDESK-H1", Mode: store.RestoreModeDownloadZip, Overwrite: store.RestoreOverwriteIfChanged, Status: "delivered", Count: 3,
			Progress: zipDone(adminpath.DumpSkipped{}), Bytes: i64(18_432_000), Age: 50 * m, Started: 48 * m, Finished: 47 * m, Wrapped: true},
		{Key: "zip-delivered-skipped", Title: "Downloaded, links left out", Host: "FRONTDESK-H1", Mode: store.RestoreModeDownloadZip, Overwrite: store.RestoreOverwriteIfChanged, Status: "delivered", Count: 2,
			Progress: zipDone(adminpath.DumpSkipped{Symlinks: 2, Other: 1}), Bytes: i64(9_120_000), Age: 2 * h, Started: 2 * h, Finished: 2*h - 3*m, Wrapped: true},
		{Key: "zip-cut", Title: "Download cut off: no zip saved", Host: "FRONTDESK-H1", Mode: store.RestoreModeDownloadZip, Overwrite: store.RestoreOverwriteIfChanged, Status: "ready", ErrorClass: "disconnected",
			Count: 1, Age: 20 * m, Started: 18 * m, Finished: 17 * m, Wrapped: true},
		{Key: "zip-expired", Title: "Download expired", Host: "FRONTDESK-H1", Mode: store.RestoreModeDownloadZip, Overwrite: store.RestoreOverwriteIfChanged, Status: "expired", Count: 1, Age: 30 * h, Expired: true},

		{Key: "device-queued", Title: "Queued, device online", Host: "OPS-ND-01", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteNever, Status: "queued", Count: 4, Age: 1 * m, Wrapped: true},
		{Key: "device-queued-offline", Title: "Queued, device offline", Host: "LAB-RETINA-02", Mode: store.RestoreModeDevicePath, Overwrite: store.RestoreOverwriteNever, Status: "queued", Count: 2, Age: 3 * h, Wrapped: true},
		{Key: "device-running", Title: "Running on the device", Host: "HYGIENE-PC-2", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteNever, Status: "running", Count: 6, Age: 9 * m, Started: 8 * m, Wrapped: true},
		{Key: "device-waiting", Title: "Waiting for the running backup", Host: "DR-OFFICE-PC", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteIfChanged, Status: "running", Count: 1, Age: 5 * m, Started: 4 * m, Wrapped: true},
		{Key: "device-restored", Title: "Restored, some kept because they exist", Host: "BILLING-H3", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteNever, Status: "delivered", Count: 5,
			Progress: store.DeviceRestoreProgress{Outcome: "success", FilesRestored: 1284, FilesSkipped: 37, BytesRestored: 2_140_000_000}, Age: 3 * h, Started: 3 * h, Finished: 2*h + 40*m},
		{Key: "device-partial", Title: "RESTORED WITH ERRORS", Host: "EXAM-H2", Mode: store.RestoreModeDevicePath, Overwrite: store.RestoreOverwriteNever, Status: "delivered", ErrorClass: store.RestoreClassPartial, Count: 3,
			Progress: store.DeviceRestoreProgress{Outcome: "partial", FilesRestored: 412, FilesSkipped: 0, BytesRestored: 380_000_000,
				Errors: []store.RestoreClassCounted{{Class: "access_denied", Count: 3}, {Class: "target_locked", Count: 1}, {Class: "other", Count: 1}}}, Age: 5 * h, Started: 5 * h, Finished: 4*h + 50*m},
		{Key: "device-failed-auth", Title: "Failed: gateway refused the login", Host: "FRONTDESK-H1", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteNever, Status: "failed", ErrorClass: "auth", Count: 1, Age: 26 * h, Started: 26 * h, Finished: 26 * h},
		{Key: "device-failed-disk", Title: "Failed: not enough free space", Host: "FRONTDESK-H1", Mode: store.RestoreModeDevicePath, Overwrite: store.RestoreOverwriteNever, Status: "failed", ErrorClass: "disk", Count: 2, Age: 27 * h, Started: 27 * h, Finished: 27 * h},
		{Key: "device-failed-guard", Title: "Failed: safety check refused the folder", Host: "FRONTDESK-H1", Mode: store.RestoreModeDevicePath, Overwrite: store.RestoreOverwriteNever, Status: "failed", ErrorClass: "guard_refused", Count: 1, Age: 28 * h, Started: 28 * h, Finished: 28 * h},
		{Key: "device-failed-nothing", Title: "Failed: nothing matched", Host: "FRONTDESK-H1", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteNever, Status: "failed", ErrorClass: "nothing_matched", Count: 1, Age: 29 * h, Started: 29 * h, Finished: 29 * h},
		{Key: "device-failed-interrupted", Title: "Failed: interrupted", Host: "FRONTDESK-H1", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteIfChanged, Status: "failed", ErrorClass: "interrupted", Count: 2,
			Progress: store.DeviceRestoreProgress{Outcome: "interrupted", FilesRestored: 88, BytesRestored: 64_000_000}, Age: 30 * h, Started: 30 * h, Finished: 29 * h},
		{Key: "device-failed-repo", Title: "Failed: repository unreachable", Host: "FRONTDESK-H1", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteNever, Status: "failed", ErrorClass: "repo_unreachable", Count: 1, Age: 31 * h, Started: 31 * h, Finished: 31 * h},
		{Key: "device-cancelled", Title: "Cancelled", Host: "FRONTDESK-H1", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteNever, Status: "cancelled", Count: 1, Age: 32 * h, Finished: 32 * h},
		{Key: "device-expired", Title: "Expired: the device never picked it up", Host: "FRONTDESK-H1", Mode: store.RestoreModeDevicePath, Overwrite: store.RestoreOverwriteNever, Status: "expired", Count: 1, Age: 50 * h, Expired: true},
	}
	for i := range rows {
		rows[i].ID = P22RestoreID(i + 1)
	}
	return rows
}

// P22RestoreByKey returns the row of P22Restores with key k.
func P22RestoreByKey(k string) (P22Restore, bool) {
	for _, r := range P22Restores() {
		if r.Key == k {
			return r, true
		}
	}
	return P22Restore{}, false
}

// p22Selection is the version-2 wrapped selection document the restores service writes
// ({"paths":[...]}, escrow.ContextRestoreSelection, id "<device>/<snapshot>"). Repeated here
// because the restores package imports fixtures in its tests; a test pins the two together.
func p22Selection(mk *escrow.MasterKey, deviceID, snapshotID string, paths []string) ([]byte, error) {
	doc, err := json.Marshal(struct {
		Paths []string `json:"paths"`
	}{paths})
	if err != nil {
		return nil, err
	}
	return mk.Wrap(escrow.ContextRestoreSelection, deviceID+"/"+snapshotID, 2, doc)
}

// SeedP22 adds the restore world to a database LoadFixtures filled: the snapshot of the fake
// repository in the cache of every device a row names (where the cache is empty), the old agent of
// P22OldAgentHost, and the rows of P22Restores. A row that holds a selection is wrapped under mk
// when mk is loaded, so its download streams; with no key it holds a placeholder that the download
// refuses as unreadable. Call it once; a second call fails on the ids.
func SeedP22(ctx context.Context, st *store.Store, now time.Time, mk *escrow.MasterKey) error {
	now = now.UTC()
	devs := map[string]string{}
	l, err := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 500}})
	if err != nil {
		return err
	}
	for _, d := range l.Items {
		devs[d.Hostname] = d.ID
	}
	rows := P22Restores()
	for _, r := range rows {
		id, ok := devs[r.Host]
		if !ok {
			return errors.New("fixtures: no device " + r.Host)
		}
		snaps, err := st.ListSnapshots(ctx, store.ScopeAll(), id)
		if err != nil {
			return err
		}
		if len(snaps) == 0 {
			if _, _, err := st.AgentReplaceSnapshots(ctx, id, []store.Snapshot{{DeviceID: id, SnapshotID: FixtureSnapshotID, TakenAt: now.Add(-26 * time.Hour), Hostname: r.Host,
				Paths: []string{`C:\Users`, `D:\Practice`}}}); err != nil {
				return err
			}
		}
	}
	if _, ok := devs[P22OldAgentHost]; !ok {
		return errors.New("fixtures: no device " + P22OldAgentHost)
	}
	return st.Write(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE devices SET agent_version = ? WHERE hostname = ?`, P22OldAgentVersion, P22OldAgentHost); err != nil {
			return err
		}
		var admin sql.NullString
		if err := tx.QueryRow(`SELECT id FROM admin_users WHERE username = ?`, store.FixtureAdminUsername).Scan(&admin); err != nil {
			return errors.New("fixtures: the restore world needs LoadFixtures first")
		}
		ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
		opt := func(d time.Duration) any {
			if d == 0 {
				return nil
			}
			return ms(d)
		}
		for _, r := range rows {
			dev := devs[r.Host]
			expires := now.Add(-r.Age).Add(24 * time.Hour) // ruling C6 and restores.DefaultTTL: 24 hours
			if r.Expired && !expires.Before(now) {
				expires = now.Add(-time.Minute)
			}
			var wrapped any
			if r.Wrapped {
				paths := []string{P22FormsFolder}
				if mk != nil && mk.Loaded() {
					b, err := p22Selection(mk, dev, FixtureSnapshotID, paths)
					if err != nil {
						return err
					}
					wrapped = b
				} else {
					wrapped = []byte("fixtures: no master key")
				}
			}
			var progress any
			if r.Progress != nil {
				b, err := json.Marshal(r.Progress)
				if err != nil {
					return err
				}
				progress = string(b)
			}
			var bytes any
			if r.Bytes != nil {
				bytes = *r.Bytes
			}
			if _, err := tx.Exec(`INSERT INTO restore_requests (id, device_id, snapshot_id, mode, overwrite, selection_count, selection_hash, selection_wrapped,
					status, requested_by, bytes, progress, error_class, started_at, finished_at, expires_at, download_count, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				r.ID, dev, FixtureSnapshotID, r.Mode, r.Overwrite, r.Count, fmt.Sprintf("%064x", r.Count), wrapped,
				r.Status, admin, bytes, progress, nullable(r.ErrorClass), opt(r.Started), opt(r.Finished), expires.UnixMilli(),
				downloads(r), ms(r.Age), ms(r.Age)); err != nil {
				return fmt.Errorf("fixtures: restore %s: %w", r.Key, err)
			}
		}
		return nil
	})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// downloads is a row's download_count: one for a download that was delivered.
func downloads(r P22Restore) int {
	if r.Mode == store.RestoreModeDownloadZip && r.Status == "delivered" {
		return 1
	}
	return 0
}
