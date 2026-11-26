package restores

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Restore to the original device and the several-paths download (ADR 0027,
// ADR 0028, ADR 0030; ticket P22-E).
//
// CreateRestore is the one entry point for a new restore of any mode: a
// download (download_zip, one or up to adminpath.MaxDumpPaths paths) or a
// device restore (device_original or device_side). Every mode needs the
// operator role (the caller's route) and a fresh MFA check (Actor.FreshMFA,
// ruling C5). FetchForDevice is the agent's fetch of a device restore's
// selection. Close happens in the store when the agent's restore run arrives;
// ExpireDeviceRestores is the restore-purge job's half for device restores.
//
// No path appears in an error, a log line or an audit row: the selection exists
// in clear only in memory, on its way into the wrapped blob and back out to the
// agent's fetch or the worker's dump.

// Refusals of CreateRestore and FetchForDevice besides the ones restores.go
// declares. The admin API and the agent API map each to its own problem code.
var (
	// ErrStepUp: the caller's last TOTP check is older than the fresh-MFA window
	// (ADR 0019, ruling C5): 403 mfa_step_up_required.
	ErrStepUp = errors.New("restores: a fresh MFA check is required")
	// ErrConfirmHost: replace mode needs the device's host name typed exactly (400).
	ErrConfirmHost = errors.New("restores: the host name confirmation does not match")
	// ErrAgentTooOld: the device's agent predates device restore (409 agent_too_old).
	ErrAgentTooOld = errors.New("restores: the agent is too old for a device restore")
	// ErrRetiredDownloadOnly: a retired device can be downloaded from, not
	// restored to (ADR 0030; 409 device_retired_download_only).
	ErrRetiredDownloadOnly = errors.New("restores: the device is retired, download only")
	// ErrDevicePurging: the device is being purged (ADR 0019; 409 device_purging).
	ErrDevicePurging = errors.New("restores: the device is being purged")
	// ErrRestoreActive: the device already has a device restore queued or running
	// (409 device_restore_active).
	ErrRestoreActive = errors.New("restores: a device restore is already active")
	// ErrRestoreExpired: the device restore waited past its expiry (410 restore_expired).
	ErrRestoreExpired = errors.New("restores: the device restore expired")
	// ErrSelectionUnreadable: a stored selection could not be unwrapped or does not
	// pass the wire validator. A console fault, never the agent's (500).
	ErrSelectionUnreadable = errors.New("restores: the stored selection cannot be read")
)

// Modes of CreateInput.Mode: the admin API's words. The device modes are the
// wire's (api.DeviceRestoreMode), so device_side here is device_path in the
// database (StoredMode).
const (
	ModeDownloadZip    = "download_zip"
	ModeDeviceOriginal = string(api.DeviceRestoreOriginal)
	ModeDeviceSide     = string(api.DeviceRestoreSide)
)

// MinDeviceRestoreAgent is the first agent version that runs a device restore
// (ADR 0027: "the agent ships as 0.4.0").
const MinDeviceRestoreAgent = "0.4.0"

// selectionVersion2 is the version of a wrapped selection that holds a JSON
// list of paths ({"paths":[...]}). Version 1, written before P22, is one raw
// path; Download still reads it.
const selectionVersion2 = 2

// ---- wire <-> stored ----

// The one table between the wire's device restore words and the database's
// CHECK values (migrations/0001_initial.sql restore_requests.mode and
// .overwrite). A test round-trips every row.
var (
	modeTable = []struct {
		wire   api.DeviceRestoreMode
		stored string
	}{
		{api.DeviceRestoreOriginal, store.RestoreModeDeviceOriginal},
		{api.DeviceRestoreSide, store.RestoreModeDevicePath},
	}
	overwriteTable = []struct {
		wire   api.DeviceRestoreOverwrite
		stored string
	}{
		{api.DeviceOverwriteNever, store.RestoreOverwriteNever},
		{api.DeviceOverwriteIfChanged, store.RestoreOverwriteIfChanged},
	}
)

// StoredMode is the restore_requests.mode of a wire device mode.
func StoredMode(m api.DeviceRestoreMode) (string, bool) {
	for _, r := range modeTable {
		if r.wire == m {
			return r.stored, true
		}
	}
	return "", false
}

// WireMode is the wire device mode of a stored restore_requests.mode.
func WireMode(stored string) (api.DeviceRestoreMode, bool) {
	for _, r := range modeTable {
		if r.stored == stored {
			return r.wire, true
		}
	}
	return "", false
}

// StoredOverwrite is the restore_requests.overwrite of a wire overwrite.
func StoredOverwrite(o api.DeviceRestoreOverwrite) (string, bool) {
	for _, r := range overwriteTable {
		if r.wire == o {
			return r.stored, true
		}
	}
	return "", false
}

// WireOverwrite is the wire overwrite of a stored restore_requests.overwrite.
func WireOverwrite(stored string) (api.DeviceRestoreOverwrite, bool) {
	for _, r := range overwriteTable {
		if r.stored == stored {
			return r.wire, true
		}
	}
	return "", false
}

// AgentSupportsDeviceRestore reports whether an agent version runs a device
// restore: its leading major.minor.patch numbers (an optional "v" first, any
// suffix after, as a git-describe or pre-release build has) are at least
// MinDeviceRestoreAgent. A version without three leading numbers ("dev",
// "unknown", "") is refused: a device restore is never sent to an agent that
// may not know the command.
func AgentSupportsDeviceRestore(v string) bool {
	got, ok := leadingVersion(v)
	if !ok {
		return false
	}
	want, _ := leadingVersion(MinDeviceRestoreAgent)
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}

func leadingVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	for i := 0; i < 3; i++ {
		j := 0
		for j < len(v) && v[j] >= '0' && v[j] <= '9' {
			j++
		}
		if j == 0 || j > 6 {
			return out, false
		}
		n, err := strconv.Atoi(v[:j])
		if err != nil {
			return out, false
		}
		out[i] = n
		v = v[j:]
		if i < 2 {
			if !strings.HasPrefix(v, ".") {
				return out, false
			}
			v = v[1:]
		}
	}
	return out, true
}

// ---- create ----

// CreateInput is a new restore as the admin API or the wizard asks for it.
type CreateInput struct {
	SnapshotID string
	// Mode is ModeDownloadZip, ModeDeviceOriginal or ModeDeviceSide.
	Mode string
	// Paths are the selected files and folders, snapshot-absolute ("/C/Users/x").
	// A path inside another selected folder is dropped (adminpath.CollapsePaths).
	Paths []string
	// Overwrite is for ModeDeviceOriginal only: never (the default, keep what
	// exists) or if_changed (replace). ModeDeviceSide takes never or nothing.
	Overwrite api.DeviceRestoreOverwrite
	// ConfirmHostName must equal the device's host name exactly for replace
	// mode (ModeDeviceOriginal with if_changed). It is not stored or logged.
	ConfirmHostName string
}

// selectionDoc is what a version-2 wrapped selection holds.
type selectionDoc struct {
	Paths []string `json:"paths"`
}

// selectionHash is the audit hash of a collapsed selection: SHA-256 over the
// paths joined by a line feed (no path holds one), lower-case hex. For one path
// it is that path's SHA-256, the hash a version-1 selection always had.
func selectionHash(paths []string) string {
	sum := sha256.Sum256([]byte(strings.Join(paths, "\n")))
	return hex.EncodeToString(sum[:])
}

// CreateRestore validates and records a restore of any mode (see CreateInput).
//
// A download is ready at once (nothing is built; ADR 0008) and is allowed for a
// retired device that is not being purged (ADR 0030). A device restore is
// queued with its restore command in one transaction (store.CreateDeviceRestoreAudited),
// waits ttl (24 hours, ruling C6) and needs an agent of MinDeviceRestoreAgent or
// newer. Refusals, each its own error: ErrStepUp, ErrInvalid, ErrConfirmHost,
// store.ErrNotFound, ErrDevicePurging, ErrRetiredDownloadOnly (device modes),
// ErrCustomerHeld, ErrMasterKey, ErrAgentTooOld, ErrRestoreActive, and
// store.ErrAudit when the audit row cannot be written (nothing is created then).
func (s *Service) CreateRestore(ctx context.Context, who Actor, scope store.Scope, deviceID string, in CreateInput) (store.RestoreRequest, error) {
	if !who.FreshMFA {
		return store.RestoreRequest{}, ErrStepUp
	}
	switch in.Mode {
	case ModeDownloadZip:
		return s.createZip(ctx, who, scope, deviceID, in)
	case ModeDeviceOriginal, ModeDeviceSide:
		return s.createDevice(ctx, who, scope, deviceID, in)
	}
	return store.RestoreRequest{}, ErrInvalid
}

// zipPaths checks the paths of a download: 1 to adminpath.MaxDumpPaths after
// collapsing, each absolute, clean, not the snapshot root, no control
// character, at most 4096 bytes (the rule adminpath.DumpRequest.Validate applies
// to Paths).
func zipPaths(in []string) ([]string, bool) {
	if len(in) == 0 {
		return nil, false
	}
	total := 0
	for _, p := range in {
		total += len(p)
		if !ValidPath(p) || p == "/" || path.Clean(p) != p {
			return nil, false
		}
	}
	if total > api.MaxRestorePathsTotalBytes {
		return nil, false
	}
	out := adminpath.CollapsePaths(in)
	if len(out) == 0 || len(out) > adminpath.MaxDumpPaths {
		return nil, false
	}
	return out, true
}

// devicePaths checks the paths of a device restore with the wire's rules
// (api.ValidateRestorePath, api.MaxRestorePaths, api.MaxRestorePathsTotalBytes)
// and collapses them.
func devicePaths(in []string) ([]string, bool) {
	if len(in) == 0 || len(in) > api.MaxRestorePaths {
		return nil, false
	}
	total := 0
	for _, p := range in {
		total += len(p)
		if api.ValidateRestorePath(p) != nil {
			return nil, false
		}
	}
	if total > api.MaxRestorePathsTotalBytes {
		return nil, false
	}
	return adminpath.CollapsePaths(in), true
}

// knownSnapshot reports whether the device's snapshot cache holds snapshotID.
func (s *Service) knownSnapshot(ctx context.Context, scope store.Scope, deviceID, snapshotID string) (bool, error) {
	snaps, err := s.Store.ListSnapshots(ctx, scope, deviceID)
	if err != nil {
		return false, err
	}
	for _, sn := range snaps {
		if sn.SnapshotID == snapshotID {
			return true, nil
		}
	}
	return false, nil
}

// wrapSelection wraps paths as a version-2 selection under the master key.
func (s *Service) wrapSelection(deviceID, snapshotID string, paths []string) ([]byte, error) {
	doc, err := json.Marshal(selectionDoc{Paths: paths})
	if err != nil {
		return nil, err
	}
	return s.Master.Wrap(escrow.ContextRestoreSelection, deviceID+"/"+snapshotID, selectionVersion2, doc)
}

// unwrapSelection returns the paths of a stored selection and whether it is the
// version-1 form (one raw path, written before P22).
func unwrapSelection(mk *escrow.MasterKey, r store.RestoreRequest) ([]string, bool, error) {
	id := r.DeviceID + "/" + r.SnapshotID
	if pt, err := mk.Unwrap(escrow.ContextRestoreSelection, id, selectionVersion2, r.SelectionWrapped); err == nil {
		var doc selectionDoc
		if json.Unmarshal(pt, &doc) != nil || len(doc.Paths) == 0 {
			return nil, false, ErrSelectionUnreadable
		}
		return doc.Paths, false, nil
	}
	pt, err := mk.Unwrap(escrow.ContextRestoreSelection, id, 1, r.SelectionWrapped)
	if err != nil {
		return nil, false, ErrSelectionUnreadable
	}
	return []string{string(pt)}, true, nil
}

// gate applies the refusals every mode shares, in a fixed order: the device in
// scope, purging, then custody and the master key. retiredOK says whether a
// retired device may continue (a download, ADR 0030).
func (s *Service) gate(ctx context.Context, scope store.Scope, deviceID string, retiredOK bool) (store.Device, error) {
	dev, err := s.Store.GetDevice(ctx, scope, deviceID)
	if err != nil {
		return store.Device{}, err
	}
	switch {
	case dev.Purging():
		return dev, ErrDevicePurging
	case dev.RetiredAt != nil && !retiredOK:
		return dev, ErrRetiredDownloadOnly
	}
	esc, err := s.Store.ActiveEscrow(ctx, scope, deviceID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return dev, ErrCustomerHeld // no escrowed key: nothing the console can restore with
	case err != nil:
		return dev, err
	case esc.Custody != "escrowed":
		return dev, ErrCustomerHeld
	}
	if !s.Master.Loaded() {
		return dev, ErrMasterKey
	}
	return dev, nil
}

func (s *Service) createZip(ctx context.Context, who Actor, scope store.Scope, deviceID string, in CreateInput) (store.RestoreRequest, error) {
	paths, ok := zipPaths(in.Paths)
	if !ok || !snapshotIDRE.MatchString(in.SnapshotID) || in.Overwrite != "" || in.ConfirmHostName != "" {
		return store.RestoreRequest{}, ErrInvalid
	}
	if _, err := s.gate(ctx, scope, deviceID, true); err != nil {
		return store.RestoreRequest{}, err
	}
	if known, err := s.knownSnapshot(ctx, scope, deviceID, in.SnapshotID); err != nil {
		return store.RestoreRequest{}, err
	} else if !known {
		return store.RestoreRequest{}, ErrInvalid
	}
	wrapped, err := s.wrapSelection(deviceID, in.SnapshotID, paths)
	if err != nil {
		return store.RestoreRequest{}, err
	}
	return s.Store.CreateRestoreRequestAudited(ctx, scope, store.RestoreRequest{
		DeviceID: deviceID, SnapshotID: in.SnapshotID, Mode: store.RestoreModeDownloadZip, Overwrite: store.RestoreOverwriteIfChanged,
		SelectionCount: len(paths), SelectionHash: selectionHash(paths), SelectionWrapped: wrapped,
		Status: "ready", RequestedBy: who.AdminID, ExpiresAt: s.now().Add(s.ttl()),
	}, store.AuditActor{AdminID: who.AdminID, Username: who.Username, IP: who.IP, Method: who.Method})
}

func (s *Service) createDevice(ctx context.Context, who Actor, scope store.Scope, deviceID string, in CreateInput) (store.RestoreRequest, error) {
	wireMode := api.DeviceRestoreMode(in.Mode)
	over := in.Overwrite
	if over == "" {
		over = api.DeviceOverwriteNever
	}
	storedMode, okMode := StoredMode(wireMode)
	storedOver, okOver := StoredOverwrite(over)
	paths, okPaths := devicePaths(in.Paths)
	switch {
	case !okMode || !okOver || !okPaths || !snapshotIDRE.MatchString(in.SnapshotID):
		return store.RestoreRequest{}, ErrInvalid
	case wireMode == api.DeviceRestoreSide && over != api.DeviceOverwriteNever:
		return store.RestoreRequest{}, ErrInvalid // the side folder is new and empty: nothing to replace
	case over != api.DeviceOverwriteIfChanged && in.ConfirmHostName != "":
		return store.RestoreRequest{}, ErrInvalid // a confirmation only belongs to replace mode
	}
	dev, err := s.gate(ctx, scope, deviceID, false)
	if err != nil {
		return store.RestoreRequest{}, err
	}
	if over == api.DeviceOverwriteIfChanged && (in.ConfirmHostName == "" || in.ConfirmHostName != dev.Hostname) {
		return store.RestoreRequest{}, ErrConfirmHost
	}
	if !AgentSupportsDeviceRestore(dev.AgentVersion) {
		return store.RestoreRequest{}, ErrAgentTooOld
	}
	if known, err := s.knownSnapshot(ctx, scope, deviceID, in.SnapshotID); err != nil {
		return store.RestoreRequest{}, err
	} else if !known {
		return store.RestoreRequest{}, ErrInvalid
	}
	wrapped, err := s.wrapSelection(deviceID, in.SnapshotID, paths)
	if err != nil {
		return store.RestoreRequest{}, err
	}
	row, _, err := s.Store.CreateDeviceRestoreAudited(ctx, scope, store.DeviceRestoreInput{
		DeviceID: deviceID, SnapshotID: in.SnapshotID, Mode: storedMode, Overwrite: storedOver,
		SelectionCount: len(paths), SelectionHash: selectionHash(paths), SelectionWrapped: wrapped,
		ExpiresAt: s.now().Add(s.ttl()),
	}, store.AuditActor{AdminID: who.AdminID, Username: who.Username, IP: who.IP, Method: who.Method})
	if err != nil {
		return store.RestoreRequest{}, mapStoreRefusal(err)
	}
	s.log().Info("restore: device restore queued", logx.ID("restore_id", row.ID), logx.ID("device_id", deviceID), logx.Str("mode", storedMode),
		logx.Int("paths", len(paths)))
	return row, nil
}

// mapStoreRefusal turns the store's restore refusals into this package's.
func mapStoreRefusal(err error) error {
	switch {
	case errors.Is(err, store.ErrRestoreCustomerHeld):
		return ErrCustomerHeld
	case errors.Is(err, store.ErrRestoreDevicePurging):
		return ErrDevicePurging
	case errors.Is(err, store.ErrDeviceRetired):
		return ErrRetiredDownloadOnly
	case errors.Is(err, store.ErrDeviceRestoreActive):
		return ErrRestoreActive
	case errors.Is(err, store.ErrRestoreExpired):
		return ErrRestoreExpired
	}
	return err
}

// ---- fetch (the agent) ----

// FetchForDevice answers the agent's GET /api/v1/agent/restores/{id}: the
// selection of the authenticated device's restore, unwrapped and validated with
// the wire's own validator, mapped to the wire's words. The fetch is recorded
// (the row becomes running) and audited as restore.fetch only after the
// selection was read, so a console that cannot read it changes nothing.
//
// Refusals: store.ErrNotFound (unknown, another device's, a download, finished),
// ErrRestoreExpired, ErrDevicePurging, ErrRetiredDownloadOnly, ErrCustomerHeld,
// ErrMasterKey, ErrSelectionUnreadable, store.ErrAudit. The caller must never
// log the returned selection.
func FetchForDevice(ctx context.Context, st *store.Store, mk *escrow.MasterKey, deviceID, id, sourceIP string) (api.RestoreSelection, error) {
	r, err := st.AgentDeviceRestore(ctx, deviceID, id)
	if err != nil {
		return api.RestoreSelection{}, mapStoreRefusal(err)
	}
	if !mk.Loaded() {
		return api.RestoreSelection{}, ErrMasterKey
	}
	paths, legacy, err := unwrapSelection(mk, r)
	if err != nil || legacy {
		return api.RestoreSelection{}, ErrSelectionUnreadable
	}
	mode, okMode := WireMode(r.Mode)
	over, okOver := WireOverwrite(r.Overwrite)
	if !okMode || !okOver {
		return api.RestoreSelection{}, ErrSelectionUnreadable
	}
	sel := api.RestoreSelection{ID: r.ID, SnapshotID: r.SnapshotID, Mode: mode, Overwrite: over, Paths: paths, ExpiresAt: r.ExpiresAt.UTC()}
	if sel.Validate() != nil {
		return api.RestoreSelection{}, ErrSelectionUnreadable
	}
	if _, err := st.AgentMarkRestoreFetched(ctx, deviceID, id, sourceIP); err != nil {
		return api.RestoreSelection{}, mapStoreRefusal(err)
	}
	return sel, nil
}

// ---- expiry (the restore-purge job) ----

// ExpireDeviceRestores closes the device restores the device never picked up
// (store.ExpireDeviceRestores): EXPIRED past the 24-hour wait, CANCELLED when
// the operator cancelled the queued command. Each is logged with its count only.
func (s *Service) ExpireDeviceRestores(ctx context.Context) (int, error) {
	n, err := s.Store.ExpireDeviceRestores(ctx, s.now())
	if n > 0 {
		s.log().Warn("restore: device restores closed unfetched", logx.Int("count", n))
	}
	if err != nil {
		return n, fmt.Errorf("restores: cannot close the device restores that were not picked up: %w", err)
	}
	return n, nil
}

// ---- what the operator reads ----

// Outcome is a restore as the operator reads it: a fixed sentence and the
// counts. It never carries a path or a message from the device.
type Outcome struct {
	// Text is a fixed sentence per status and class (ADR 0027, "What the operator sees").
	Text          string
	FilesRestored int64
	FilesSkipped  int64
	BytesRestored uint64
	Errors        []store.RestoreClassCounted
	// Skipped counts what a download left out (links, devices), by kind; nil
	// before a completed download and for a device restore.
	Skipped *adminpath.DumpSkipped
	// ClosedBy is set when the console closed the device restore because the
	// agent stopped reporting it (store.DeviceRestoreProgress.ClosedBy, ADR 0032).
	ClosedBy string
	// Late is the agent's report that arrived after the console closed it; nil
	// when none did.
	Late *store.DeviceRestoreLate
}

// classText is the sentence of a failed device restore's class. ADR 0027 says
// the operator sees the space needed and free for class disk; no wire field
// carries those numbers (api.RestoreResultDisk), so the sentence sends the
// operator to the device's log.
var classText = map[string]string{
	string(api.RestoreResultDisk):            "FAILED: not enough free space on the device (see the device's log for the numbers).",
	string(api.RestoreResultAuth):            "FAILED: the gateway refused the device's login.",
	string(api.RestoreResultGuardRefused):    "FAILED: a restore folder on the device failed its safety check; remove the folder and run the restore again.",
	string(api.RestoreResultNothingMatched):  "FAILED: nothing in the backup matched the selection.",
	string(api.RestoreResultInterrupted):     "FAILED: the restore was cut short; files already placed stay, run the restore again.",
	string(api.RestoreResultRepoUnreachable): "FAILED: the device could not reach its repository.",
}

// lateText is the sentence of a device restore's late report (ADR 0032), in the
// words of the status it would have closed the row with.
func lateText(l store.DeviceRestoreLate) string {
	counts := fmt.Sprintf("%d restored, %d skipped because they already exist", l.FilesRestored, l.FilesSkipped)
	switch {
	case l.Status == "delivered" && l.Class == store.RestoreClassPartial:
		var parts []string
		for _, e := range l.Errors {
			parts = append(parts, fmt.Sprintf("%s %d", e.Class, e.Count))
		}
		return "restored with errors, " + counts + ". Errors by class: " + strings.Join(parts, ", ") + "."
	case l.Status == "delivered":
		return "restored, " + counts + "."
	case l.Status == "cancelled":
		return "cancelled, " + counts + "."
	case l.Class != "":
		return "failed (" + l.Class + "), " + counts + "."
	}
	return strings.ToLower(l.Status) + ", " + counts + "."
}

// DescribeOutcome is what the operator reads for r.
func DescribeOutcome(r store.RestoreRequest) Outcome {
	var out Outcome
	if r.Mode == store.RestoreModeDownloadZip {
		var pr struct {
			Skipped *adminpath.DumpSkipped `json:"skipped"`
		}
		if len(r.Progress) > 0 && json.Unmarshal(r.Progress, &pr) == nil {
			out.Skipped = pr.Skipped
		}
		switch r.Status {
		case "ready":
			out.Text = "Ready to download."
		case "delivered":
			out.Text = "Downloaded."
			if out.Skipped != nil && out.Skipped.Total() > 0 {
				out.Text = fmt.Sprintf("Downloaded. %d item(s) that are not regular files or folders were left out.", out.Skipped.Total())
			}
		case "expired", "purged":
			out.Text = "EXPIRED."
		default:
			out.Text = strings.ToUpper(r.Status) + "."
		}
		return out
	}
	var pr store.DeviceRestoreProgress
	if len(r.Progress) > 0 && json.Unmarshal(r.Progress, &pr) == nil {
		out.FilesRestored, out.FilesSkipped, out.BytesRestored, out.Errors = pr.FilesRestored, pr.FilesSkipped, pr.BytesRestored, pr.Errors
		out.ClosedBy, out.Late = pr.ClosedBy, pr.Late
	}
	counts := fmt.Sprintf("%d restored, %d skipped because they already exist", out.FilesRestored, out.FilesSkipped)
	switch {
	case r.Status == "failed" && out.ClosedBy != "":
		out.Text = "FAILED: the device stopped reporting this restore, so the console closed it as interrupted. What it restored is unknown."
		if out.Late != nil {
			out.Text += " The device reported it later: " + lateText(*out.Late)
		}
	case r.Status == "queued":
		out.Text = "Queued: starts when the device is online, until the request expires. For files now, use Download as zip."
	case r.Status == "running":
		out.Text = "Running on the device."
	case r.Status == "delivered" && r.ErrorClass == store.RestoreClassPartial:
		var parts []string
		for _, e := range out.Errors {
			parts = append(parts, fmt.Sprintf("%s %d", e.Class, e.Count))
		}
		out.Text = "RESTORED WITH ERRORS: " + counts + ". Errors by class: " + strings.Join(parts, ", ") + "."
	case r.Status == "delivered":
		out.Text = "RESTORED: " + counts + "."
	case r.Status == "cancelled":
		out.Text = "CANCELLED."
	case r.Status == "expired":
		out.Text = "EXPIRED: the device did not pick the restore up in time."
	case r.Status == "failed" && classText[r.ErrorClass] != "":
		out.Text = classText[r.ErrorClass]
	case r.Status == "failed":
		out.Text = "FAILED (" + r.ErrorClass + ")."
	default:
		out.Text = strings.ToUpper(r.Status) + "."
	}
	return out
}
