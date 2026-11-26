package ui

import (
	"context"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-R2a: the restore fixes of the opus review (r1a-findings.log).
//   A1 a running device restore whose device has gone silent says so, and the sweep closes it as
//      interrupted after store.DeviceRestoreSilenceLimit, so the device can take another restore
//      (ADR 0032); the agent's late report is shown beside the console's verdict.
//   A2 a create never substitutes the snapshot.
//   A4 refreshSnapshots reads custody itself.

const r2aActive = "is already queued or running"

// listedSnapshot is the snapshot a create for device id may name since A2: the fixtures snapshot
// when the device's cache lists it, else the newest one the cache lists (what the wizard shows).
func (e *env) listedSnapshot(id string) string {
	e.t.Helper()
	snaps, err := e.st.ListSnapshots(context.Background(), store.ScopeAll(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	newest := ""
	var at time.Time
	for _, sn := range snaps {
		if sn.SnapshotID == fixtures.FixtureSnapshotID {
			return sn.SnapshotID
		}
		if newest == "" || sn.TakenAt.After(at) {
			newest, at = sn.SnapshotID, sn.TakenAt
		}
	}
	return newest
}

// r2aDestStep posts the select step's continue for a device and returns the body (the r1a probe).
func r2aDestStep(e *env, id string) string {
	e.t.Helper()
	_, body := e.post("/ui/devices/"+id+"/restore", vv("snapshot", "", "step", "select", "action", "submit", "path", "/C/Users"), false)
	return html.UnescapeString(body)
}

func TestR2A_SilentRunningRestoreSaysSoAndTheSweepFreesTheDevice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedP22(t)
	ctx := context.Background()
	host := "HYGIENE-PC-2"
	id := e.dev[host]
	fx, ok := fixtures.P22RestoreByKey("device-running")
	if !ok || fx.Host != host {
		t.Fatalf("fixture device-running: %+v", fx)
	}

	// Online: the plain running sentence, no warning.
	body := html.UnescapeString(e.page("/restores/"+fx.ID, http.StatusOK))
	mustContain(t, "online", body, "Running on "+host+".", "Files are being restored.")
	if strings.Contains(body, "has not been heard from") || strings.Contains(body, "Last seen") {
		t.Fatal("an online device's running restore warns")
	}

	// The r1a case: 72 hours later, nothing from the device, the purge job has run.
	e.advance(72 * time.Hour)
	e.login()
	if _, err := e.svc.Restores.ExpireDeviceRestores(ctx); err != nil {
		t.Fatal(err)
	}
	pr, err := e.st.GetPresence(ctx, store.ScopeAll(), id)
	if err != nil {
		t.Fatal(err)
	}
	c := e.svc.fc(ctx)
	want := host + " has not been heard from since " + c.dayClock(pr.ReceivedAt) + "."
	for _, path := range []string{"/restores/" + fx.ID, "/ui/partials/restore/" + fx.ID} {
		b := html.UnescapeString(e.page(path, http.StatusOK))
		mustContain(t, path, b, "RUNNING", want, "The outcome is unknown until it reports.", "closes the restore as interrupted at about")
		if strings.Contains(b, "Files are being restored.") {
			t.Errorf("%s: the silent restore still says files are being restored", path)
		}
		if !strings.Contains(b, `banner warn`) {
			t.Errorf("%s: the banner is not a warning", path)
		}
	}
	page := html.UnescapeString(e.page("/restores/"+fx.ID, http.StatusOK))
	mustContain(t, "facts", page, "Last seen", "3 d ago")
	if !strings.Contains(r2aDestStep(e, id), r2aActive) {
		t.Fatal("before the sweep the device restore is not counted as active")
	}

	// The sweep closes it as interrupted: the device is silent past the limit.
	res, err := e.st.SweepAbandonedRuns(ctx, store.SweepOptions{})
	if err != nil || res.RestoresClosedBy[store.RestoreClosedAgentSilent] < 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	rr, err := e.st.GetRestoreRequest(ctx, store.ScopeAll(), fx.ID)
	if err != nil || rr.Status != "failed" || rr.ErrorClass != "interrupted" || rr.SelectionWrapped != nil {
		t.Fatalf("closed row %+v %v", rr, err)
	}
	closed := html.UnescapeString(e.page("/restores/"+fx.ID, http.StatusOK))
	mustContain(t, "closed", closed, "FAILED", "the device stopped reporting this restore, so the console closed it as interrupted",
		"Nothing came from "+host+" for 24 hours while the restore was open, so the console closed it.")
	if strings.Contains(closed, "hx-trigger") {
		t.Error("a closed restore still polls")
	}
	rows := e.auditRows("restore.finish", fx.ID)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Details), `"reason":"agent_silent"`) {
		t.Fatalf("audit %+v", rows)
	}
	if d := string(rows[0].Details); strings.Contains(d, "/") || strings.Contains(d, `\`) || strings.Contains(d, "Practice") {
		t.Errorf("the audit row carries a path: %s", d)
	}

	// The device may take another device restore.
	if b := r2aDestStep(e, id); strings.Contains(b, r2aActive) {
		t.Fatal("after the sweep the device is still blocked by the closed restore")
	}

	// The agent's late report is shown beside the verdict, which stays FAILED.
	fin := e.now
	out, err := e.st.AgentIngestRuns(ctx, id, []store.RunInput{{ID: fx.ID, Kind: "restore", Trigger: "manual", StartedAt: e.now.Add(-73 * time.Hour),
		FinishedAt: &fin, Outcome: "success", Stats: []byte(`{"files_restored":12,"files_skipped":3,"bytes_restored":4096}`)}}, e.now)
	if err != nil || out[0].Status == store.IngestRejected {
		t.Fatalf("late report %+v %v", out, err)
	}
	late := html.UnescapeString(e.page("/restores/"+fx.ID, http.StatusOK))
	mustContain(t, "late", late, "FAILED", "The device reported it later: restored, 12 restored, 3 skipped because they already exist.",
		host+" reported it afterwards", "This restore stays closed", "Late report")
	if strings.Contains(late, "RESTORED:") {
		t.Error("the late report turned the page into RESTORED")
	}
	if n := len(e.auditRows("restore.late_report", fx.ID)); n != 1 {
		t.Fatalf("%d restore.late_report rows", n)
	}
}

// A device that has never been heard from at all: the sentence without a time, and "Never".
func TestR2A_RunningRestoreOnADeviceNeverSeen(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.svc.fc(context.Background())
	started := e.now.Add(-time.Hour)
	rr := store.RestoreRequest{ID: "01JR2A00000000000000000001", Mode: store.RestoreModeDeviceOriginal, Overwrite: store.RestoreOverwriteNever,
		Status: "running", SelectionCount: 1, StartedAt: &started, CreatedAt: started, ExpiresAt: started.Add(24 * time.Hour)}
	v := RestoreView{Host: "PC-X"}
	e.svc.deviceRestoreView(context.Background(), &v, rr, restores.DescribeOutcome(rr), liveFacts{Offline: true}, c)
	if v.Headline != "PC-X has not been heard from." || !strings.HasPrefix(v.Message, "The outcome is unknown until it reports.") || v.Class != ClassWarn {
		t.Fatalf("never seen: %q %q %s", v.Headline, v.Message, v.Class)
	}
	if !strings.Contains(v.Message, c.dayClock(started.Add(store.DeviceRestoreSilenceLimit))) {
		t.Errorf("the closing time is not the fetch plus the limit: %q", v.Message)
	}
}

func TestR2A_CreateWithAnUnknownSnapshotIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	doc := "/C/Users/Public/Documents/Quarterly-Report.txt"
	count := func() int {
		l, err := e.st.ListRestoreRequests(context.Background(), store.ScopeAll(), id, store.Page{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		return len(l.Items)
	}
	for name, snap := range map[string]string{"unknown": strings.Repeat("ab", 32), "none": ""} {
		res, body := e.post("/ui/devices/"+id+"/restore", vv("snapshot", snap, "action", "submit", "path", doc), false)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status %d", name, res.StatusCode)
		}
		mustContainText(t, name, body, "That snapshot is not one the console knows.", "nothing was started")
		if n := count(); n != 0 {
			t.Fatalf("%s: %d restores created from a snapshot nobody chose", name, n)
		}
	}
	// The snapshot the console knows still creates.
	res, _ := e.post("/ui/devices/"+id+"/restore", vv("snapshot", fixtures.FixtureSnapshotID, "action", "submit", "path", doc), false)
	if res.StatusCode != http.StatusSeeOther || count() != 1 {
		t.Fatalf("a known snapshot: %d, %d restores", res.StatusCode, count())
	}
}

func TestR2A_RefreshSnapshotsReadsCustodyItself(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedP22(t)
	ctx := context.Background()
	handBuilt := func(host string) RestoreData {
		dev, err := e.st.GetDevice(ctx, store.ScopeAll(), e.dev[host])
		if err != nil {
			t.Fatal(err)
		}
		return RestoreData{DeviceID: dev.ID, Host: dev.Hostname, dev: dev} // not built by restoreBase: Blocked is nil
	}
	held := handBuilt("LEGAL-PC-01")
	before := e.be.Count(adminpath.MethodSnapshots)
	b := e.svc.refreshSnapshots(ctx, &held)
	if n := e.be.Count(adminpath.MethodSnapshots) - before; n != 0 {
		t.Fatalf("a customer-held device was read from the repository (%d worker calls)", n)
	}
	if b.Class == ClassInfo || b.Title != "Customer-held key: no technician restore" {
		t.Fatalf("customer-held: %+v", b)
	}
	ok := handBuilt("FRONTDESK-01")
	before = e.be.Count(adminpath.MethodSnapshots)
	if b := e.svc.refreshSnapshots(ctx, &ok); b.Class != ClassInfo {
		t.Fatalf("an escrowed device: %+v", b)
	}
	if n := e.be.Count(adminpath.MethodSnapshots) - before; n != 1 {
		t.Fatalf("an escrowed device: %d worker calls", n)
	}
}
