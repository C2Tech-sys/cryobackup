package restores

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-E: CreateRestore, the wire <-> stored mapping, the agent version rule and
// the several-paths download record (device_p22.go).

const p22Sentinel = "/C/Users/SENTINEL-p22e/Taxes"

var fresh = Actor{Username: "op", Method: "api", FreshMFA: true}

func (e *env) setAgent(t *testing.T, id, version string) {
	t.Helper()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET agent_version = ? WHERE id = ?`, version, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

func deviceIn(mode string, paths ...string) CreateInput {
	return CreateInput{SnapshotID: fixtures.FixtureSnapshotID, Mode: mode, Paths: paths}
}

// TestWireStoredMappingRoundTrips: every wire mode and overwrite maps to a value
// the migrations' CHECK lists accept and back; the wire spellings themselves are
// refused by those CHECKs, so the mapping cannot be skipped by accident.
func TestWireStoredMappingRoundTrips(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, m := range []api.DeviceRestoreMode{api.DeviceRestoreOriginal, api.DeviceRestoreSide} {
		st, ok := StoredMode(m)
		back, ok2 := WireMode(st)
		if !ok || !ok2 || back != m {
			t.Fatalf("mode %s -> %q -> %s", m, st, back)
		}
		if _, err := e.st.CreateRestoreRequest(ctx, store.ScopeAll(), store.RestoreRequest{DeviceID: e.device.ID, SnapshotID: fixtures.FixtureSnapshotID,
			Mode: st, Overwrite: store.RestoreOverwriteNever, ExpiresAt: e.now.Add(time.Hour)}); err != nil {
			t.Fatalf("stored mode %q refused by the schema: %v", st, err)
		}
	}
	for _, o := range []api.DeviceRestoreOverwrite{api.DeviceOverwriteNever, api.DeviceOverwriteIfChanged} {
		st, ok := StoredOverwrite(o)
		back, ok2 := WireOverwrite(st)
		if !ok || !ok2 || back != o {
			t.Fatalf("overwrite %s -> %q -> %s", o, st, back)
		}
		if _, err := e.st.CreateRestoreRequest(ctx, store.ScopeAll(), store.RestoreRequest{DeviceID: e.device.ID, SnapshotID: fixtures.FixtureSnapshotID,
			Mode: store.RestoreModeDeviceOriginal, Overwrite: st, ExpiresAt: e.now.Add(time.Hour)}); err != nil {
			t.Fatalf("stored overwrite %q refused by the schema: %v", st, err)
		}
	}
	if st, _ := StoredMode(api.DeviceRestoreSide); st != "device_path" {
		t.Fatalf("device_side is stored as %q, want device_path", st)
	}
	if st, _ := StoredOverwrite(api.DeviceOverwriteIfChanged); st != "if-changed" {
		t.Fatalf("if_changed is stored as %q, want if-changed", st)
	}
	for _, raw := range []store.RestoreRequest{
		{Mode: "device_side", Overwrite: "never"},
		{Mode: "device_original", Overwrite: "if_changed"},
	} {
		raw.DeviceID, raw.SnapshotID, raw.ExpiresAt = e.device.ID, fixtures.FixtureSnapshotID, e.now.Add(time.Hour)
		if _, err := e.st.CreateRestoreRequest(ctx, store.ScopeAll(), raw); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("the wire spelling %q/%q reached the database: %v", raw.Mode, raw.Overwrite, err)
		}
	}
	for _, unknown := range []string{"download_zip", "device_relay", "always", ""} {
		if _, ok := WireMode(unknown); ok && unknown != "" {
			t.Fatalf("%q mapped to a wire mode", unknown)
		}
		if _, ok := WireOverwrite(unknown); ok && (unknown == "always" || unknown == "") {
			t.Fatalf("%q mapped to a wire overwrite", unknown)
		}
	}
}

func TestAgentSupportsDeviceRestore(t *testing.T) {
	for v, want := range map[string]bool{
		"0.4.0": true, "v0.4.0": true, "0.4.1": true, "0.10.0": true, "1.0.0": true, "1.4.2": true, "0.4.0-3-gabcdef1": true, "0.4.0-rc1": true,
		"0.3.9": false, "0.3.99": false, "0.0.1": false, "dev": false, "unknown": false, "": false, "0.4": false, "x0.4.0": false, "0.4.x": false,
	} {
		if got := AgentSupportsDeviceRestore(v); got != want {
			t.Errorf("%q: got %v, want %v", v, got, want)
		}
	}
}

// TestCreateRestoreRefusals: each rule its own error, the sentinel path never in
// an error, a log line or an audit row.
func TestCreateRestoreRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setAgent(t, e.device.ID, "0.4.0")
	side := deviceIn(ModeDeviceSide, p22Sentinel)
	cases := []struct {
		name string
		who  Actor
		dev  string
		in   CreateInput
		want error
	}{
		{"stale MFA (device)", Actor{Username: "op"}, e.device.ID, side, ErrStepUp},
		{"stale MFA (zip)", Actor{Username: "op"}, e.device.ID, deviceIn(ModeDownloadZip, goodPath), ErrStepUp},
		{"unknown mode", fresh, e.device.ID, deviceIn("device_relay", p22Sentinel), ErrInvalid},
		{"side with replace", fresh, e.device.ID, CreateInput{SnapshotID: fixtures.FixtureSnapshotID, Mode: ModeDeviceSide, Paths: []string{p22Sentinel}, Overwrite: api.DeviceOverwriteIfChanged}, ErrInvalid},
		{"drive root", fresh, e.device.ID, deviceIn(ModeDeviceSide, "/C"), ErrInvalid},
		{"no paths", fresh, e.device.ID, deviceIn(ModeDeviceOriginal), ErrInvalid},
		{"replace without confirmation", fresh, e.device.ID, CreateInput{SnapshotID: fixtures.FixtureSnapshotID, Mode: ModeDeviceOriginal, Paths: []string{p22Sentinel}, Overwrite: api.DeviceOverwriteIfChanged}, ErrConfirmHost},
		{"replace with the wrong host", fresh, e.device.ID, CreateInput{SnapshotID: fixtures.FixtureSnapshotID, Mode: ModeDeviceOriginal, Paths: []string{p22Sentinel}, Overwrite: api.DeviceOverwriteIfChanged, ConfirmHostName: "dr-office-pc"}, ErrConfirmHost},
		{"confirmation without replace", fresh, e.device.ID, CreateInput{SnapshotID: fixtures.FixtureSnapshotID, Mode: ModeDeviceOriginal, Paths: []string{p22Sentinel}, ConfirmHostName: e.device.Hostname}, ErrInvalid},
		{"customer-held (device)", fresh, e.held.ID, side, ErrCustomerHeld},
		{"customer-held (zip)", fresh, e.held.ID, deviceIn(ModeDownloadZip, goodPath), ErrCustomerHeld},
		{"unknown snapshot", fresh, e.device.ID, CreateInput{SnapshotID: strings.Repeat("a", 64), Mode: ModeDeviceSide, Paths: []string{p22Sentinel}}, ErrInvalid},
		{"zip of the snapshot root", fresh, e.device.ID, deviceIn(ModeDownloadZip, "/"), ErrInvalid},
		{"zip of an unclean path", fresh, e.device.ID, deviceIn(ModeDownloadZip, "/C/Users/../Windows"), ErrInvalid},
	}
	for _, c := range cases {
		_, err := e.svc.CreateRestore(ctx, c.who, store.ScopeAll(), c.dev, c.in)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
		if err != nil && strings.Contains(err.Error(), "SENTINEL") {
			t.Errorf("%s: the error names the path: %v", c.name, err)
		}
	}
	// The agent is too old.
	e.setAgent(t, e.device.ID, "0.3.9")
	if _, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, side); !errors.Is(err, ErrAgentTooOld) {
		t.Fatalf("old agent: %v", err)
	}
	e.setAgent(t, e.device.ID, "0.4.0")
	// No master key.
	noKey := &Service{Store: e.st, Backend: e.be, Clock: e.svc.Clock}
	if _, err := noKey.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, side); !errors.Is(err, ErrMasterKey) {
		t.Fatalf("no master key: %v", err)
	}
	// One active device restore per device; the replace confirmation works.
	if _, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, CreateInput{SnapshotID: fixtures.FixtureSnapshotID, Mode: ModeDeviceOriginal,
		Paths: []string{p22Sentinel}, Overwrite: api.DeviceOverwriteIfChanged, ConfirmHostName: e.device.Hostname}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, side); !errors.Is(err, ErrRestoreActive) {
		t.Fatalf("second active: %v", err)
	}
	// A download is not a device restore: still allowed.
	if _, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, deviceIn(ModeDownloadZip, goodPath)); err != nil {
		t.Fatalf("zip beside an active device restore: %v", err)
	}
	if e.be.Count(adminpath.MethodDump) != 0 {
		t.Fatal("a create ran a dump")
	}
	p22NoSentinel(t, e)
}

func p22NoSentinel(t *testing.T, e *env) {
	t.Helper()
	if strings.Contains(e.logs.String(), "SENTINEL") || strings.Contains(e.logs.String(), "Taxes") {
		t.Fatalf("a path reached a log line:\n%s", e.logs.String())
	}
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range l.Items {
		if strings.Contains(string(a.Details), "SENTINEL") || strings.Contains(string(a.Details), "Taxes") {
			t.Fatalf("a path reached audit row %s: %s", a.Action, a.Details)
		}
	}
}

// TestRetiredDeviceDownloadOnly (ADR 0030): a retired device may be downloaded
// from; device modes are refused by name; a purging one refuses both.
func TestRetiredDeviceDownloadOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setAgent(t, e.device.ID, "0.4.0")
	e.retireDevice(t, e.device.ID)
	if _, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, deviceIn(ModeDownloadZip, goodPath)); err != nil {
		t.Fatalf("zip of a retired device: %v", err)
	}
	if _, err := e.svc.Create(ctx, fresh, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID, goodPath); err != nil {
		t.Fatalf("single-path create for a retired device: %v", err)
	}
	for _, m := range []string{ModeDeviceOriginal, ModeDeviceSide} {
		if _, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, deviceIn(m, p22Sentinel)); !errors.Is(err, ErrRetiredDownloadOnly) {
			t.Fatalf("%s on a retired device: %v", m, err)
		}
	}
	e.exec(t, `UPDATE devices SET purge_requested_at = ? WHERE id = ?`, e.now.UnixMilli(), e.device.ID)
	if _, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, deviceIn(ModeDownloadZip, goodPath)); !errors.Is(err, ErrDevicePurging) {
		t.Fatalf("zip of a purging device: %v", err)
	}
	if _, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, deviceIn(ModeDeviceSide, p22Sentinel)); !errors.Is(err, ErrDevicePurging) {
		t.Fatalf("device mode on a purging device: %v", err)
	}
	if _, err := e.svc.Create(ctx, fresh, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID, goodPath); !errors.Is(err, ErrDevicePurging) {
		t.Fatalf("single-path create for a purging device: %v", err)
	}
}

// TestSeveralPathsDownloadRecord (ADR 0028): the collapsed selection is stored
// wrapped (count and hash of the collapsed list), streams as Paths, and the
// download's record keeps the skipped counts by kind, the count and the hash.
func TestSeveralPathsDownloadRecord(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.be.Files = append(fixtures.DefaultFiles(), fixtures.File{Path: "/C/Users/Public/Documents/shortcut", Skip: adminpath.SkipSymlink})
	r, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, deviceIn(ModeDownloadZip,
		goodDir, goodPath, "/C/Users/Public/Desktop/Welcome.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{goodDir, "/C/Users/Public/Desktop/Welcome.txt"}
	if r.SelectionCount != 2 || r.SelectionHash != selectionHash(want) || bytes.Contains(r.SelectionWrapped, []byte("Welcome")) {
		t.Fatalf("row %+v", r)
	}
	var out bytes.Buffer
	if _, err := e.svc.Download(ctx, Actor{}, r, &out); err != nil {
		t.Fatal(err)
	}
	got := readZip(t, out.Bytes())
	if _, ok := got["Documents/Quarterly-Report.txt"]; !ok || got["Welcome.txt"] == nil || got[fixtures.SkippedEntryName] == nil {
		t.Fatalf("zip entries %v", keys(got))
	}
	if c := e.be.Calls(); len(c) != 1 || c[0].Path != "" || len(c[0].Paths) != 2 {
		t.Fatalf("the worker got %+v", c)
	}
	row := e.get(t, r.ID)
	var pr struct {
		Skipped        adminpath.DumpSkipped `json:"skipped"`
		SelectionCount int                   `json:"selection_count"`
		SelectionHash  string                `json:"selection_hash"`
		SHA256         string                `json:"sha256"`
	}
	if json.Unmarshal(row.Progress, &pr) != nil || pr.Skipped != (adminpath.DumpSkipped{Symlinks: 1}) || pr.SelectionCount != 2 || pr.SelectionHash != r.SelectionHash || pr.SHA256 == "" {
		t.Fatalf("download record %s", row.Progress)
	}
	if oc := DescribeOutcome(row); oc.Skipped == nil || oc.Skipped.Symlinks != 1 || !strings.Contains(oc.Text, "1 item(s)") {
		t.Fatalf("outcome %+v", oc)
	}
	// A cut stream saves nothing: no delivery, no record.
	e.be.FailDumpAfter = 30
	r2, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, deviceIn(ModeDownloadZip, goodDir))
	if err != nil {
		t.Fatal(err)
	}
	var cut bytes.Buffer
	var fe *FailedError
	if _, err := e.svc.Download(ctx, Actor{}, r2, &cut); !errors.As(err, &fe) {
		t.Fatalf("cut stream: %v", err)
	}
	if row := e.get(t, r2.ID); row.Status != "ready" || row.Bytes != nil || len(row.Progress) != 0 {
		t.Fatalf("a cut stream left a record: %+v", row)
	}
	if strings.Contains(e.logs.String(), "Welcome") || strings.Contains(e.logs.String(), "/C/Users") {
		t.Fatalf("a path reached a log line:\n%s", e.logs.String())
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestFetchForDeviceSelection: the agent gets the wire words, the collapsed
// paths and the request's expiry; another device gets ErrNotFound.
func TestFetchForDeviceSelection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setAgent(t, e.device.ID, "1.0.0")
	r, err := e.svc.CreateRestore(ctx, fresh, store.ScopeAll(), e.device.ID, CreateInput{SnapshotID: fixtures.FixtureSnapshotID, Mode: ModeDeviceOriginal,
		Paths: []string{p22Sentinel + "/a.txt", p22Sentinel}, Overwrite: api.DeviceOverwriteIfChanged, ConfirmHostName: e.device.Hostname})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchForDevice(ctx, e.st, e.svc.Master, e.held.ID, r.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another device: %v", err)
	}
	sel, err := FetchForDevice(ctx, e.st, e.svc.Master, e.device.ID, r.ID, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Mode != api.DeviceRestoreOriginal || sel.Overwrite != api.DeviceOverwriteIfChanged || len(sel.Paths) != 1 || sel.Paths[0] != p22Sentinel ||
		!sel.ExpiresAt.Equal(r.ExpiresAt) || sel.Validate() != nil {
		t.Fatalf("selection %+v", sel)
	}
	if got := e.get(t, r.ID); got.Status != "running" || DescribeOutcome(got).Text != "Running on the device." {
		t.Fatalf("row %+v", got)
	}
	p22NoSentinel(t, e)
}

// TestEveryCreatePathNeedsFreshMFA (ruling C5): both entry points, Create (the
// web wizard's single-path download) and CreateRestore (the admin API, every
// mode), refuse a caller without a fresh MFA check before anything is read or
// written; with it they create.
func TestEveryCreatePathNeedsFreshMFA(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setAgent(t, e.device.ID, "0.4.0")
	stale := Actor{AdminID: "", Username: "tech", Method: "web"}
	if _, err := e.svc.Create(ctx, stale, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID, goodPath); !errors.Is(err, ErrStepUp) {
		t.Fatalf("Create without a fresh check: %v", err)
	}
	for _, in := range []CreateInput{deviceIn(ModeDownloadZip, goodPath), deviceIn(ModeDeviceSide, p22Sentinel), deviceIn(ModeDeviceOriginal, p22Sentinel)} {
		if _, err := e.svc.CreateRestore(ctx, stale, store.ScopeAll(), e.device.ID, in); !errors.Is(err, ErrStepUp) {
			t.Fatalf("CreateRestore %s without a fresh check: %v", in.Mode, err)
		}
	}
	l, err := e.st.ListRestoreRequests(ctx, store.ScopeAll(), e.device.ID, store.Page{Limit: 10})
	if err != nil || len(l.Items) != 0 {
		t.Fatalf("a refused create stored %d restores (%v)", len(l.Items), err)
	}
	stale.FreshMFA = true
	if _, err := e.svc.Create(ctx, stale, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID, goodPath); err != nil {
		t.Fatalf("Create with a fresh check: %v", err)
	}
}
