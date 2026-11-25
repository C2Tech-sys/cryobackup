package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// P22-E: GET /api/v1/agent/restores/{id} and the close of a device restore by
// its run record, driven by a fake agent over HTTP.

const (
	p22Snap     = "c3c3c3c3d4d4d4d4e5e5e5e5f6f6f6f6a7a7a7a7b8b8b8b8c9c9c9c9d0d0d0d0"
	p22Sentinel = "/C/Users/SENTINEL-p22e/Payroll"
)

type p22Env struct {
	*harness
	rest *restores.Service
	logs *bytes.Buffer
	a, b api.EnrollResponse
}

func newP22Env(t *testing.T) *p22Env {
	t.Helper()
	h := newHarness(t)
	e := &p22Env{harness: h, logs: &bytes.Buffer{}}
	h.svc.Log = logx.New(e.logs, logx.LevelDebug)
	e.rest = &restores.Service{Store: h.st, Backend: h.be, Master: h.keys.Master, Clock: h.clk.Now, Log: logx.New(e.logs, logx.LevelDebug)}
	var r resp
	if e.a, r = h.enroll(nonce(41)); r.code != 200 {
		t.Fatalf("enroll a: %d %s", r.code, r.body)
	}
	if e.b, r = h.enroll(nonce(42)); r.code != 200 {
		t.Fatalf("enroll b: %d %s", r.code, r.body)
	}
	for _, d := range []string{e.a.DeviceID, e.b.DeviceID} {
		if _, _, err := h.st.AgentReplaceSnapshots(context.Background(), d, []store.Snapshot{{DeviceID: d, SnapshotID: p22Snap, TakenAt: h.clk.Now().Add(-time.Hour), Paths: []string{`C:\Users`}}}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *p22Env) create(t *testing.T, dev string, mode string) store.RestoreRequest {
	t.Helper()
	rr, err := e.rest.CreateRestore(context.Background(), restores.Actor{Username: "op", Method: "api", FreshMFA: true}, store.ScopeAll(), dev,
		restores.CreateInput{SnapshotID: p22Snap, Mode: mode, Paths: []string{p22Sentinel, p22Sentinel + "/inside.xlsx", "/D/Data/Report.docx"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return rr
}

func (e *p22Env) fetch(tok, id string) resp {
	e.t.Helper()
	return e.do("GET", api.RouteRestore(id), tok, nil)
}

func (e *p22Env) row(t *testing.T, id string) store.RestoreRequest {
	t.Helper()
	r, err := e.st.GetRestoreRequest(context.Background(), store.ScopeAll(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *p22Env) audits(t *testing.T, action, id string) []store.AuditEvent {
	t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action, ObjectID: id}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func (e *p22Env) noSentinel(t *testing.T) {
	t.Helper()
	if strings.Contains(e.logs.String(), "SENTINEL") || strings.Contains(e.logs.String(), "Payroll") {
		t.Fatalf("a path reached a log line:\n%s", e.logs.String())
	}
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range l.Items {
		if strings.Contains(string(a.Details), "SENTINEL") || strings.Contains(string(a.Details), "Payroll") {
			t.Fatalf("a path reached audit row %s: %s", a.Action, a.Details)
		}
	}
}

// TestFakeAgentRestoreEndToEnd: create -> command -> fetch -> run record ->
// delivered, with another device's token answered exactly like a missing id.
func TestFakeAgentRestoreEndToEnd(t *testing.T) {
	e := newP22Env(t)
	tokA, tokB := string(e.a.DeviceToken), string(e.b.DeviceToken)
	rr := e.create(t, e.a.DeviceID, restores.ModeDeviceSide)
	if rr.Mode != store.RestoreModeDevicePath || rr.Overwrite != store.RestoreOverwriteNever || rr.SelectionCount != 2 {
		t.Fatalf("stored row %+v", rr)
	}

	// 1. the command: params carry only the request id.
	r := e.do("GET", api.RouteCommands, tokA, nil)
	var cr api.CommandsResponse
	r.json(t, &cr)
	if r.code != 200 || len(cr.Commands) != 1 || cr.Commands[0].Type != api.CommandRestore || cr.Commands[0].Validate() != nil {
		t.Fatalf("commands: %d %s", r.code, r.body)
	}
	var params api.RestoreParams
	if json.Unmarshal(cr.Commands[0].Params, &params) != nil || params.RestoreRequestID != rr.ID || strings.Contains(string(r.body), "SENTINEL") {
		t.Fatalf("restore params %s", cr.Commands[0].Params)
	}
	if r := e.do("GET", api.RouteCommands, tokB, nil); strings.Contains(string(r.body), rr.ID) {
		t.Fatal("device B was handed device A's restore command")
	}

	// 2. another device's token: exactly like a missing restore.
	other, missing := e.fetch(tokB, rr.ID), e.fetch(tokB, "01J9ZZ0000000000000000ZZZZ")
	po, pm := other.problem(t), missing.problem(t)
	if other.code != 404 || missing.code != 404 || po.Code != api.CodeNotFound || po.Title != pm.Title || len(other.body) != len(missing.body) {
		t.Fatalf("other device: %d %s / missing: %d %s", other.code, other.body, missing.code, missing.body)
	}
	if bad := e.fetch(tokA, "not-a-ulid"); bad.code != 404 || bad.problem(t).Code != api.CodeNotFound {
		t.Fatalf("malformed id: %d %s", bad.code, bad.body)
	}
	if anon := e.fetch("", rr.ID); anon.code != 401 {
		t.Fatalf("no token: %d", anon.code)
	}
	if e.row(t, rr.ID).Status != "queued" || len(e.audits(t, "restore.fetch", rr.ID)) != 0 {
		t.Fatal("a refused fetch changed or audited the row")
	}

	// 3. the device's own fetch: the validated selection, never cached.
	r = e.fetch(tokA, rr.ID)
	if r.code != 200 || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("fetch: %d %s %v", r.code, r.body, r.hdr)
	}
	var sel api.RestoreSelection
	r.json(t, &sel)
	if err := sel.Validate(); err != nil || sel.ID != rr.ID || sel.Mode != api.DeviceRestoreSide || sel.Overwrite != api.DeviceOverwriteNever ||
		len(sel.Paths) != 2 || sel.Paths[0] != p22Sentinel || sel.Paths[1] != "/D/Data/Report.docx" || !sel.ExpiresAt.Equal(rr.ExpiresAt) {
		t.Fatalf("selection %+v (%v)", sel, err)
	}
	if got := e.row(t, rr.ID); got.Status != "running" || got.StartedAt == nil {
		t.Fatalf("row after fetch %+v", got)
	}
	if a := e.audits(t, "restore.fetch", rr.ID); len(a) != 1 || a[0].ActorID != e.a.DeviceID {
		t.Fatalf("fetch audit %+v", a)
	}

	// 4. the run record (run id = restore id) closes the row.
	start := e.clk.Now().Add(-time.Minute)
	fin := e.clk.Now()
	rec := api.RunRecord{ID: rr.ID, Kind: restic.KindRestore, Trigger: api.TriggerManual, StartedAt: start, FinishedAt: &fin, Outcome: restic.OutcomeSuccess,
		Attempt: 1, PolicyVersion: 1, AgentVersion: "0.4.0", Stats: api.RunStats{FilesRestored: 12, FilesSkipped: 3, BytesRestored: 1 << 20}}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	r = e.do("POST", api.RouteRuns, tokA, api.RunsRequest{Runs: []api.RunRecord{rec}})
	var rres api.RunsResponse
	r.json(t, &rres)
	if r.code != 200 || rres.Results[0].Status != api.RunCreated {
		t.Fatalf("runs: %d %s", r.code, r.body)
	}
	got := e.row(t, rr.ID)
	out := restores.DescribeOutcome(got)
	if got.Status != "delivered" || got.ErrorClass != "" || got.SelectionWrapped != nil || out.FilesRestored != 12 || out.FilesSkipped != 3 ||
		out.Text != "RESTORED: 12 restored, 3 skipped because they already exist." {
		t.Fatalf("closed row %+v outcome %+v", got, out)
	}
	// A finished restore is not handed out again.
	if r := e.fetch(tokA, rr.ID); r.code != 404 {
		t.Fatalf("fetch after delivery: %d %s", r.code, r.body)
	}
	e.noSentinel(t)
}

// TestRestoreFetchRefusals: expiry 410 (and the job closes it EXPIRED), retired,
// purging and customer-held each by their own code, a failed audit 500 with
// nothing handed out, no master key 503.
func TestRestoreFetchRefusals(t *testing.T) {
	e := newP22Env(t)
	ctx := context.Background()
	tokA, tokB := string(e.a.DeviceToken), string(e.b.DeviceToken)
	set := func(q string, args ...any) {
		t.Helper()
		if err := e.st.Write(ctx, func(tx *store.Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}

	// A failed audit write refuses the fetch: 500, no selection, the row unchanged.
	rr := e.create(t, e.a.DeviceID, restores.ModeDeviceOriginal)
	set(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
	r := e.fetch(tokA, rr.ID)
	if r.code != 500 || r.problem(t).Code != api.CodeInternal || strings.Contains(string(r.body), "SENTINEL") || e.row(t, rr.ID).Status != "queued" {
		t.Fatalf("unaudited fetch: %d %s", r.code, r.body)
	}
	set(`DROP TRIGGER refuse_audits`)

	// No master key: 503, nothing changed.
	keys := e.svc.Keys
	e.svc.Keys = nil
	if r := e.fetch(tokA, rr.ID); r.code != 503 || r.problem(t).Code != api.CodeMasterKeyUnavailable {
		t.Fatalf("no master key: %d %s", r.code, r.body)
	}
	e.svc.Keys = keys

	// Customer-held (device B's key turned customer-held after the request was made).
	rb := e.create(t, e.b.DeviceID, restores.ModeDeviceOriginal)
	set(`UPDATE repo_key_escrow SET custody = 'customer_held', wrapped_key = NULL, verified_at = NULL WHERE device_id = ?`, e.b.DeviceID)
	if r := e.fetch(tokB, rb.ID); r.code != 409 || r.problem(t).Code != api.CodeCustodyCustomerHeld {
		t.Fatalf("customer-held: %d %s", r.code, r.body)
	}

	// Expired: 410 restore_expired, and the job closes the row EXPIRED.
	e.clk.Advance(24 * time.Hour)
	if r := e.fetch(tokA, rr.ID); r.code != 410 || r.problem(t).Code != api.CodeRestoreExpired {
		t.Fatalf("expired: %d %s", r.code, r.body)
	}
	if n, err := e.rest.ExpireDeviceRestores(ctx); err != nil || n < 1 {
		t.Fatalf("expiry job: %d %v", n, err)
	}
	if got := e.row(t, rr.ID); got.Status != "expired" || restores.DescribeOutcome(got).Text != "EXPIRED: the device did not pick the restore up in time." {
		t.Fatalf("after the job: %+v", got)
	}
	if r := e.fetch(tokA, rr.ID); r.code != 410 {
		t.Fatalf("expired row: %d %s", r.code, r.body)
	}

	// Retired, then purging. A retired device's token no longer authenticates
	// (401 before the route runs); the route's own refusal stands behind it.
	r2 := e.create(t, e.a.DeviceID, restores.ModeDeviceSide)
	set(`UPDATE devices SET retired_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), e.a.DeviceID)
	if r := e.fetch(tokA, r2.ID); r.code != 401 {
		t.Fatalf("retired device's token: %d %s", r.code, r.body)
	}
	if _, err := restores.FetchForDevice(ctx, e.st, e.keys.Master, e.a.DeviceID, r2.ID, ""); !errors.Is(err, restores.ErrRetiredDownloadOnly) {
		t.Fatalf("retired: %v", err)
	}
	set(`UPDATE devices SET purge_requested_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), e.a.DeviceID)
	if _, err := restores.FetchForDevice(ctx, e.st, e.keys.Master, e.a.DeviceID, r2.ID, ""); !errors.Is(err, restores.ErrDevicePurging) {
		t.Fatalf("purging: %v", err)
	}
	if e.row(t, r2.ID).Status != "queued" {
		t.Fatal("a refused fetch changed the row")
	}
	e.noSentinel(t)
}

// TestRestoreRunClosesWithClassAndCounts: a partial run is RESTORED WITH ERRORS
// with the counts by class; a disk failure says where the numbers are.
func TestRestoreRunClosesWithClassAndCounts(t *testing.T) {
	e := newP22Env(t)
	tokA, tokB := string(e.a.DeviceToken), string(e.b.DeviceToken)
	post := func(tok string, rec api.RunRecord) {
		t.Helper()
		if err := rec.Validate(); err != nil {
			t.Fatal(err)
		}
		if r := e.do("POST", api.RouteRuns, tok, api.RunsRequest{Runs: []api.RunRecord{rec}}); r.code != 200 {
			t.Fatalf("runs: %d %s", r.code, r.body)
		}
	}
	fin := e.clk.Now()
	ra := e.create(t, e.a.DeviceID, restores.ModeDeviceOriginal)
	post(tokA, api.RunRecord{ID: ra.ID, Kind: restic.KindRestore, Trigger: api.TriggerManual, StartedAt: fin.Add(-time.Minute), FinishedAt: &fin,
		Outcome: restic.OutcomePartial, Attempt: 1, PolicyVersion: 1, AgentVersion: "0.4.0",
		Stats: api.RunStats{FilesRestored: 5, FilesSkipped: 1, RestoreResult: api.RestoreResultPartial,
			RestoreErrors: []api.RestoreClassCount{{Class: api.RestoreResultTargetLocked, Count: 2}, {Class: api.RestoreResultGuardRefused, Count: 1}}}})
	got := e.row(t, ra.ID)
	if txt := restores.DescribeOutcome(got).Text; got.Status != "delivered" || got.ErrorClass != "partial" ||
		txt != "RESTORED WITH ERRORS: 5 restored, 1 skipped because they already exist. Errors by class: target_locked 2, guard_refused 1." {
		t.Fatalf("partial: %+v %q", got, txt)
	}
	rb := e.create(t, e.b.DeviceID, restores.ModeDeviceSide)
	post(tokB, api.RunRecord{ID: rb.ID, Kind: restic.KindRestore, Trigger: api.TriggerManual, StartedAt: fin.Add(-time.Minute), FinishedAt: &fin,
		Outcome: restic.OutcomeFailed, Attempt: 1, PolicyVersion: 1, AgentVersion: "0.4.0", Stats: api.RunStats{RestoreResult: api.RestoreResultDisk}})
	got = e.row(t, rb.ID)
	if txt := restores.DescribeOutcome(got).Text; got.Status != "failed" || got.ErrorClass != "disk" ||
		txt != "FAILED: not enough free space on the device (see the device's log for the numbers)." {
		t.Fatalf("disk: %+v %q", got, txt)
	}
	// Device B posting a restore record under device A's restore id is refused and closes nothing.
	ra2 := e.create(t, e.a.DeviceID, restores.ModeDeviceSide)
	r := e.do("POST", api.RouteRuns, tokB, api.RunsRequest{Runs: []api.RunRecord{{ID: ra2.ID, Kind: restic.KindRestore, Trigger: api.TriggerManual,
		StartedAt: fin.Add(-time.Minute), FinishedAt: &fin, Outcome: restic.OutcomeSuccess, Attempt: 1, PolicyVersion: 1, AgentVersion: "0.4.0",
		Stats: api.RunStats{FilesRestored: 1}}}})
	if r.code != 200 || e.row(t, ra2.ID).Status != "queued" {
		t.Fatalf("another device's record closed the row: %d %s, status %s", r.code, r.body, e.row(t, ra2.ID).Status)
	}
	e.noSentinel(t)
}

// TestRestoreFetchRateLimit (fix E5): six fetches a minute per device, then 429
// with Retry-After; another device keeps its own allowance; the next minute
// allows again.
func TestRestoreFetchRateLimit(t *testing.T) {
	e := newP22Env(t)
	tokA, tokB := string(e.a.DeviceToken), string(e.b.DeviceToken)
	rr := e.create(t, e.a.DeviceID, restores.ModeDeviceSide)
	for i := 0; i < restoreFetchPerMinute; i++ {
		if r := e.fetch(tokA, rr.ID); r.code != 200 {
			t.Fatalf("fetch %d: %d %s", i+1, r.code, r.body)
		}
	}
	r := e.fetch(tokA, rr.ID)
	if r.code != 429 || r.problem(t).Code != api.CodeRateLimited || r.hdr.Get(api.HeaderRetryAfter) == "" {
		t.Fatalf("fetch 7: %d %s", r.code, r.body)
	}
	if r := e.fetch(tokB, "01J9ZZ0000000000000000ZZZZ"); r.code != 404 {
		t.Fatalf("another device's allowance: %d", r.code)
	}
	if n := len(e.audits(t, "restore.fetch", rr.ID)); n != restoreFetchPerMinute {
		t.Fatalf("%d fetch audit rows, want %d (a refused fetch is not audited)", n, restoreFetchPerMinute)
	}
	e.clk.Advance(time.Minute)
	if r := e.fetch(tokA, rr.ID); r.code != 200 {
		t.Fatalf("after a minute: %d %s", r.code, r.body)
	}
}
