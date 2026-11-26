package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The restore section of /design (P22-G): every step of the wizard in its states and every status
// of a restore, rendered by the same partials the pages use (restore_steps.html,
// restore_parts.html), from fixed sample values: the fixtures' invented names, no device, no
// worker. It is served at /design/restore (restore_design.html).

// DesignRestore is the restore section of /design.
type DesignRestore struct {
	Bars     []designRestoreBar
	Select   []designWizard
	Dest     []designWizard
	Review   []designWizard
	Outcomes []designOutcome
}

type designRestoreBar struct {
	Name  string
	Steps []Step
}

type designWizard struct {
	Name string
	D    RestoreData
}

type designOutcome struct {
	Name string
	Key  string
	V    RestoreView
}

// p22Row is the restore row fixtures.SeedP22 writes for r, built in memory (the design page and
// the tests read it without a database).
func p22Row(r fixtures.P22Restore, now time.Time, deviceID string) store.RestoreRequest {
	created := now.Add(-r.Age)
	row := store.RestoreRequest{ID: r.ID, DeviceID: deviceID, SnapshotID: fixtures.FixtureSnapshotID, Mode: r.Mode, Overwrite: r.Overwrite, Status: r.Status,
		ErrorClass: r.ErrorClass, SelectionCount: r.Count, Bytes: r.Bytes, CreatedAt: created, UpdatedAt: created, ExpiresAt: created.Add(restores.DefaultTTL)}
	if r.Expired && !row.ExpiresAt.Before(now) {
		row.ExpiresAt = now.Add(-time.Minute)
	}
	if r.Wrapped {
		row.SelectionWrapped = []byte("sample")
	}
	if r.Progress != nil {
		row.Progress, _ = json.Marshal(r.Progress)
	}
	if r.Started > 0 {
		t := now.Add(-r.Started)
		row.StartedAt = &t
	}
	if r.Finished > 0 {
		t := now.Add(-r.Finished)
		row.FinishedAt = &t
	}
	return row
}

// p22Live is what the status page knows about each fixture row's device.
func p22Live(key string, now time.Time) liveFacts {
	switch key {
	case "device-queued-offline":
		return liveFacts{Offline: true, SeenKnown: true, LastSeen: now.Add(-9 * 24 * time.Hour)}
	case "device-waiting":
		return liveFacts{SeenKnown: true, LastSeen: now.Add(-40 * time.Second), BackupOpen: true}
	}
	return liveFacts{SeenKnown: true, LastSeen: now.Add(-40 * time.Second)}
}

func (s *Service) designRestore(ctx context.Context, now time.Time) *DesignRestore {
	c := s.fc(ctx)
	host := "FRONTDESK-01"
	snapLabel := now.Add(-26*time.Hour).In(c.loc()).Format("Mon 2 Jan 3:04 PM") + " · " + fixtures.FixtureSnapshotID[:8]
	base := func() RestoreData {
		d := RestoreData{DeviceID: "sample", Host: host, BasketMax: restoreBasketMax, MFAWindow: int(auth.FreshMFAWindow / time.Minute), Step: stepSelect,
			Snapshots:  []SnapOption{{ID: fixtures.FixtureSnapshotID, Label: snapLabel, Selected: true}, {ID: "e2" + fixtures.FixtureSnapshotID[2:], Label: now.Add(-50*time.Hour).In(c.loc()).Format("Mon 2 Jan 3:04 PM") + " · e2f1f1f1"}},
			SnapshotID: fixtures.FixtureSnapshotID, SnapLabel: snapLabel, CanSubmit: true, Listed: true}
		return d
	}
	when := now.Add(-26 * time.Hour).In(c.loc()).Format("2006-01-02 3:04 PM")
	listing := func(d *RestoreData, dir string, entries []Entry, basket []string) {
		d.Dir, d.Crumbs = dir, crumbs(dir)
		d.Entries = entries
		d.setBasket(basket)
		d.markListing()
		if d.PageText == "" {
			d.PageText = plural(len(entries), "entry", "entries")
		}
	}
	file := func(dir, name, size string) Entry {
		return Entry{Name: name, Path: path.Join(dir, name), Kind: "file", Size: size, When: when}
	}
	folder := func(dir, name string) Entry {
		return Entry{Name: name, Path: path.Join(dir, name), Kind: "dir", When: when}
	}
	forms := fixtures.P22FormsFolder
	scans := fixtures.P22BigFolder

	// Browse and select.
	page := base()
	var scanRows []Entry
	for i := 501; i <= 506; i++ {
		scanRows = append(scanRows, file(scans, fmt.Sprintf("scan-%04d.pdf", i), "15 B"))
	}
	listing(&page, scans, scanRows, []string{scans + "/scan-0502.pdf", scans + "/scan-0504.pdf", forms + "/Intake"})
	page.PageText, page.Offset, page.HasPrev, page.HasNext, page.PrevOff, page.NextOff = "501–1,000 of 2,000 entries", 500, true, true, 0, 1000

	mixed := base()
	listing(&mixed, forms, []Entry{folder(forms, "Consent"), folder(forms, "Intake"), file(forms, fixtures.P22LongName, "20 B"), file(forms, "readme.txt", "23 B"),
		{Name: "latest", Path: forms + "/latest", Kind: "other", What: nodeWhat("symlink"), When: when}}, []string{forms + "/Intake", forms + "/readme.txt"})

	inside := base()
	listing(&inside, forms+"/Intake", []Entry{file(forms+"/Intake", "new-patient.pdf", "22 B"), file(forms+"/Intake", "returning-patient.pdf", "22 B")}, []string{forms + "/Intake"})

	folded := base()
	listing(&folded, forms, []Entry{folder(forms, "Consent"), folder(forms, "Intake"), file(forms, "readme.txt", "23 B")}, []string{forms})
	folded.Notice = &Banner{ClassInfo, "2 items are inside a selected folder.", "The folder already holds them, so they were folded into it."}

	over := base()
	var many []string
	for i := 1; i <= 53; i++ {
		many = append(many, fmt.Sprintf("%s/scan-%04d.pdf", scans, i))
	}
	listing(&over, scans, scanRows[:3], many)
	over.Error = overBanner(len(over.Basket))

	cleared := base()
	listing(&cleared, "/", []Entry{folder("/", "C"), folder("/", "D")}, nil)
	cleared.Notice = &Banner{ClassInfo, "The selection was cleared.", "It belonged to the snapshot you left; select again in this one."}

	retired := base()
	retired.Retired = true
	listing(&retired, "/", []Entry{folder("/", "C"), folder("/", "D")}, nil)

	blocked := base()
	blocked.Blocked = &Banner{ClassWarn, "Customer-held key: no technician restore", "CryoBackup never stores this key, so the console cannot open the repository. Restore on the machine with the customer's key."}
	blocked.CanSubmit = false

	noSnaps := base()
	noSnaps.Snapshots, noSnaps.NoSnaps, noSnaps.SnapshotID, noSnaps.Listed = nil, true, "", false
	noSnaps.Error = &Banner{ClassCrit, "The snapshot list was not refreshed", "The maintenance worker is not running or not reachable (banner C07)."}

	// Destination.
	picked := []string{forms + "/Intake", scans + "/scan-0502.pdf", scans + "/scan-0504.pdf"}
	dest := func(why string, retiredDev bool, mode string) RestoreData {
		d := base()
		d.Step, d.DeviceWhy, d.Retired = stepDest, why, retiredDev
		d.setBasket(picked)
		d.modes(mode)
		return d
	}
	destAll := dest("", false, "")
	destRetired := dest("This device is retired: it can be downloaded from, not restored to (ADR 0030).", true, "")
	destOld := dest("The agent on "+fixtures.P22OldAgentHost+" is "+fixtures.P22OldAgentVersion+". Restoring to the device needs agent "+restores.MinDeviceRestoreAgent+" or newer.", false, "")
	destActive := dest("A restore to "+host+" is already queued or running. It must finish, expire or be cancelled first.", false, "")
	destActive.ActiveID = fixtures.P22RestoreID(6)

	// Review.
	review := func(mode string, live liveFacts, mut func(*RestoreData)) RestoreData {
		d := dest("", false, mode)
		reviewWith(c, &d, live)
		if mut != nil {
			mut(&d)
		}
		return d
	}
	online := liveFacts{SeenKnown: true, LastSeen: now.Add(-40 * time.Second)}
	offline := liveFacts{Offline: true, SeenKnown: true, LastSeen: now.Add(-9 * 24 * time.Hour)}

	d := &DesignRestore{
		Bars: []designRestoreBar{
			{"Step 2: browse and select", func() []Step { x := base(); x.steps(); return x.Steps }()},
			{"Step 3: destination", destAll.stepsNow()},
			{"Step 4: review", func() []Step { x := review(modeZip, online, nil); return x.stepsNow() }()},
		},
		Select: []designWizard{
			{"A page of a 2 000-entry folder, with a selection kept across folders", page},
			{"Folders, files, a long name and a link (not selectable)", mixed},
			{"Inside a selected folder: everything is included", inside},
			{"Nested items folded into their folder", folded},
			{"Over the 50-item limit: refused with the reason", over},
			{"Another snapshot: the selection is cleared, with a notice", cleared},
			{"A retired device: download only", retired},
			{"Blocked: customer-held key", blocked},
			{"No snapshot known, and the refresh failed", noSnaps},
		},
		Dest: []designWizard{
			{"Every destination offered (the default: original location, keep existing)", destAll},
			{"A retired device: download only", destRetired},
			{"An agent older than " + restores.MinDeviceRestoreAgent, destOld},
			{"A restore to the device already active", destActive},
		},
		Review: []designWizard{
			{"Download as a zip", review(modeZip, online, nil)},
			{"Original location, keep existing; the device is online", review(modeOriginal, online, nil)},
			{"A new folder; the device is offline", review(modeSide, offline, nil)},
			{"Replace changed files: the host name typed wrong", review(modeReplace, liveFacts{SeenKnown: true, LastSeen: now.Add(-40 * time.Second), BackupOpen: true}, func(x *RestoreData) {
				x.Error = &Banner{ClassWarn, "The host name does not match", "Replacing files needs the device's host name typed exactly as shown. Nothing was started."}
				x.ConfirmErr = "Type " + host + " exactly as shown, then start the restore."
			})},
			{"The session's authenticator check is stale: the code field", review(modeOriginal, online, func(x *RestoreData) {
				x.NeedCode = true
				x.Error = &Banner{ClassWarn, "A recent MFA check is required", "Every restore needs an authenticator check from the last 5 minutes on this session. Enter the current code from your authenticator app below and continue; a recovery code is not accepted."}
			})},
		},
	}
	p := &auth.Principal{Scope: store.ScopeAll(), Role: auth.RoleOperator}
	for _, r := range fixtures.P22Restores() {
		v := s.restoreView(ctx, p, p22Row(r, now, "sample"), r.Host, p22Live(r.Key, now))
		d.Outcomes = append(d.Outcomes, designOutcome{Name: r.Title, Key: r.Key, V: v})
	}
	return d
}

// stepsNow is d's wizard bar as d stands.
func (d RestoreData) stepsNow() []Step {
	d.steps()
	return d.Steps
}
