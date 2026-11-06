package adminapi

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/probe"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T4 fix round 4.

// The same mistake survived two audits: evidence that a thing ran, not that it passed.
// This drives every row with its newest attempt failed while the facts beside it look
// fresh. A row added without a case here fails the test.
func TestR4NoRowReadsOKWhenItsLastAttemptFailedButItsDataLooksFresh(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ran, next := now.Add(-time.Minute), now.Add(time.Hour)
	long := func(d time.Duration) state.Millis { return ms(now.Add(-d)) }
	soon := ms(now.Add(90 * 24 * time.Hour))
	fresh := long(20 * time.Second)
	failedAfterOK := func(class string) *state.ProbeRun {
		return &state.ProbeRun{LastRunAt: long(10 * time.Second), LastOKAt: long(70 * time.Second), ConsecutiveFailures: 1, LastErrorClass: class, CertNotAfter: soon}
	}
	inBuild := func(f *healthFacts) {
		f.Probes.Release = &state.ReleaseFacts{CheckedAt: long(time.Minute)}
		f.Probes.Quota = &state.QuotaFacts{WithQuota: 1, Targets: []state.TargetUsage{{SoftQuotaBytes: 10, MeasuredAt: long(time.Hour)}}}
		f.Probes.Digest = &state.DigestFacts{LocalTime: "07:00", LastSentAt: long(time.Hour)}
		f.Probes.Database = &state.DatabaseFacts{IntegrityCheckedAt: long(time.Hour), IntegrityOK: true, MeasuredAt: fresh,
			SelfBackup: &state.SelfBackupFacts{LastAt: long(time.Hour)}}
	}
	cases := map[string][]struct {
		why  string
		want string
		mut  func(*healthFacts)
	}{
		"gateway": {{"failed after a fresh pass, certificate still known", statusWarn, func(f *healthFacts) { f.Probes.Gateway = failedAfterOK("status_502") }}},
		"storage": {{"failed after a fresh pass", statusWarn, func(f *healthFacts) { f.Probes.Storage = failedAfterOK("repo_missing") }}},
		"storage_quota": {{"a target with a quota is missing from the newest listing, the other is fresh", statusNotChecked, func(f *healthFacts) {
			f.Probes.Quota = &state.QuotaFacts{WithQuota: 2, Targets: []state.TargetUsage{{SoftQuotaBytes: 10, MeasuredAt: long(time.Hour)}}}
		}}},
		"smtp": {
			{"the probe failed, a send went out an hour ago", statusWarn, func(f *healthFacts) {
				f.Probes.SMTP = &state.SMTPFacts{Probe: *failedAfterOK("refused"), LastSendOKAt: long(time.Hour)}
			}},
			{"the probe passes, the newest send failed", statusWarn, func(f *healthFacts) { f.Probes.SMTP.ConsecutiveSendFailures = 1 }},
			{"the probe passes, the newest test send failed", statusWarn, func(f *healthFacts) { f.Probes.SMTP.LastTestFailed = true }},
			{"the probe passes, alerts wait and nothing was ever delivered", statusWarn, func(f *healthFacts) { f.Probes.SMTP.PendingAlerts = 2 }},
		},
		"digest": {{"sent once, three days ago", statusWarn, func(f *healthFacts) {
			f.Probes.Digest = &state.DigestFacts{LocalTime: "07:00", LastSentAt: long(72 * time.Hour)}
		}}},
		"certificates": {
			{"the gateway's newest handshake refused its certificate, an expiry is on file", statusFailed, func(f *healthFacts) { f.Probes.Gateway = failedAfterOK("untrusted") }},
			{"the gateway's certificate expired on its newest run", statusFailed, func(f *healthFacts) { f.Probes.Gateway = failedAfterOK("expired") }},
			{"the agent listener's certificate expired on its newest run", statusFailed, func(f *healthFacts) {
				f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: false, Stage: probe.StageCertExpired, Class: "expired", NotAfter: now.Add(-time.Hour)}
			}},
			{"the self-probe's own verdict is expired, with no expiry on file to compare", statusFailed, func(f *healthFacts) {
				f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: false, Stage: probe.StageCertExpired, Class: "expired"}
			}},
			{"the agent listener's certificate is untrusted, with a good expiry", statusFailed, func(f *healthFacts) {
				f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: false, Stage: probe.StageTLS, Class: "untrusted", NotAfter: soon2(now)}
			}},
			{"the agent listener's certificate names another host", statusFailed, func(f *healthFacts) {
				f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: false, Stage: probe.StageTLS, Class: "hostname", NotAfter: soon2(now)}
			}},
			{"the agent listener's key fails the pin", statusFailed, func(f *healthFacts) {
				f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: false, Stage: probe.StageTLS, Class: "pin_mismatch", NotAfter: soon2(now)}
			}},
			{"the agent listener's certificate ran out after its newest passing run", statusFailed, func(f *healthFacts) {
				f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: true, NotAfter: now.Add(-time.Second)}
			}},
			{"the gateway's certificate ran out after its newest passing run", statusFailed, func(f *healthFacts) { f.Probes.Gateway.CertNotAfter = long(time.Second) }},
		},
		"agent_api": {{"failed after a fresh pass", statusWarn, func(f *healthFacts) {
			f.Endpoint.LastOK = now.Add(-time.Minute)
			f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: false, Stage: probe.StageResponder, Class: "status_502", NotAfter: soon2(now)}
		}}},
		"maintenance_worker": {
			{"the ping failed, the periodic probe passed", statusFailed, func(f *healthFacts) { f.WorkerReachable = false }},
			{"the scheduler's newest pass failed and finished just now", statusWarn, func(f *healthFacts) { f.SchedulerLast.LastStatus = "error" }},
		},
		"database": {
			{"the newest read failed, the measurements are fresh", statusFailed, func(f *healthFacts) { f.SchemaErr = context.DeadlineExceeded }},
			{"the newest integrity check failed, the measurements are fresh", statusFailed, func(f *healthFacts) {
				f.Probes.Database.IntegrityOK = false
			}},
			{"the worker cannot read the database", statusWarn, func(f *healthFacts) { f.Probes.Database.WorkerStoreFailures = 1 }},
		},
		"self_backup": {{"the newest tested restore failed, the backup is fresh", statusFailed, func(f *healthFacts) {
			f.Probes.Database.SelfBackup = &state.SelfBackupFacts{LastAt: long(time.Hour), VerifyFailed: true}
		}}},
		"evaluator": {{"the newest run failed, the last good one is fresh", statusWarn, func(f *healthFacts) { f.EvaluatorLast.LastStatus = "error" }}},
		"master_key": {
			{"the prober says the key is not loaded", statusFailed, func(f *healthFacts) { f.Probes.MasterKey = &state.MasterKeyFacts{} }},
			{"loaded, and an escrow is wrapped under another generation", statusFailed, func(f *healthFacts) {
				f.Probes.MasterKey = &state.MasterKeyFacts{Loaded: true, KeyID: "new", EscrowKeyIDs: []string{"new", "old"}}
			}},
		},
		"jobs": {{"a job's newest run failed, its next run is on time", statusWarn, func(f *healthFacts) {
			f.Jobs = []store.JobState{{Name: "j", LastStatus: "error", LastFinishedAt: &ran, NextDueAt: &next}}
		}}},
		"fleet_data": {{"the newest load was incomplete", statusWarn, func(f *healthFacts) { f.Complete = false }}},
		"clock": {{"the check ran on enough devices and the devices agree against the server", statusWarn, func(f *healthFacts) {
			f.Banners = []state.Banner{{ID: "C13", Severity: state.SeverityAlarm}}
		}}},
		"release_manifest": {{"it ran a minute ago and rejected a manifest", statusWarn, func(f *healthFacts) {
			f.Probes.Release = &state.ReleaseFacts{CheckedAt: long(time.Minute), Rejected: true}
		}}},
	}
	base := func() healthFacts {
		f := fullEvidence(now, &ran, &next)
		g, st, sm := *f.Probes.Gateway, *f.Probes.Storage, *f.Probes.SMTP
		f.Probes.Gateway, f.Probes.Storage, f.Probes.SMTP = &g, &st, &sm
		inBuild(&f)
		return f
	}
	// The control: with every check in the build and every attempt passing, every row is ok.
	for id, c := range rowsOf(base()) {
		if c.Status != statusOK {
			t.Fatalf("control: %s = %+v", id, c)
		}
	}
	for _, row := range healthRowOrder {
		list, covered := cases[row.id]
		if !covered {
			t.Errorf("row %s has no failed-attempt case: add one", row.id)
		}
		for _, tc := range list {
			f := base()
			tc.mut(&f)
			got := rowsOf(f)[row.id]
			if got.Status != tc.want {
				t.Errorf("%s (%s) = %s %q, want %s", row.id, tc.why, got.Status, got.Detail, tc.want)
			}
			if len(got.Banners) == 0 && got.Detail == "" {
				t.Errorf("%s (%s): %s with no reason", row.id, tc.why, got.Status)
			}
		}
	}
}

func soon2(now time.Time) time.Time { return now.Add(90 * 24 * time.Hour) }

// A weaker banner beside a worse fact does not soften the row, and the reason stays.
func TestR4ABannerNeverSoftensWhatTheRowKnows(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ran, next := now.Add(-time.Minute), now.Add(time.Hour)
	f := fullEvidence(now, &ran, &next)
	f.Endpoint.Last = probe.Result{At: now.Add(-10 * time.Second), OK: false, Stage: probe.StageCertExpired, Class: "expired", NotAfter: now.Add(-time.Hour)}
	f.Banners = []state.Banner{{ID: "C05b", Severity: state.SeverityAlarm}}
	c := rowsOf(f)["certificates"]
	if c.Status != statusFailed || !strings.Contains(c.Detail, "expired") {
		t.Errorf("an expired certificate under a not-checked banner = %+v", c)
	}
}

// ---- MEDIUM-1: a leftover row ----

func TestR4AJobNoLongerRegisteredDoesNotCountAgainstHealth(t *testing.T) { // r4 M1
	e := newR3Env(t)
	e.svc.Jobs = staticJobs{"alarm-evaluator"}
	if c := e.row("jobs"); c.Status != statusOK {
		t.Fatalf("control: %+v", c)
	}
	// A real job's row from before the operator removed healthcheck_ping.url: failed, and long past due.
	last := e.clk.Now().Add(-10 * 24 * time.Hour)
	due := last.Add(5 * time.Minute)
	for name, status := range map[string]string{"healthcheck-ping": "error", "old-ok-job": "ok", "old-never-finished": ""} {
		if status == "" {
			if _, err := e.st.AcquireJobLease(context.Background(), name, "gone", time.Nanosecond); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := e.st.RecordJobRun(context.Background(), name, store.JobRun{Status: status, ErrorClass: "x", StartedAt: last, FinishedAt: last, NextDueAt: &due}); err != nil {
			t.Fatal(err)
		}
	}
	if c := e.row("jobs"); c.Status != statusOK {
		t.Errorf("rows of jobs this process does not run count against health: %+v", c)
	}
	// The table still lists them, as what they are, and calls none overdue.
	jl := decode[JobListResponse](t, mustGet(e.env, "/api/v1/system/jobs", e.sess))
	seen := false
	for _, j := range jl.Jobs {
		if j.Name == "healthcheck-ping" {
			seen = true
			if j.Registered || j.Overdue {
				t.Errorf("the leftover row in the table: %+v", j)
			}
		}
	}
	if !seen {
		t.Error("the leftover row is not listed")
	}
	// The maintenance scheduler runs in another process: not judged here, and the worker's row judges it.
	jl2 := decode[JobListResponse](t, mustGet(e.env, "/api/v1/system/jobs", e.sess))
	for _, j := range jl2.Jobs {
		if j.Name == "alarm-evaluator" && !j.Registered {
			t.Errorf("a registered job reads unregistered: %+v", j)
		}
	}
}

// ---- LOW 3: a job in its first run ----

func TestR4AJobInItsFirstRunIsNotCheckedNotOK(t *testing.T) { // r4 low 3
	e := newR3Env(t)
	e.svc.Jobs = staticJobs{"alarm-evaluator", "slow-first-run"}
	if _, err := e.st.AcquireJobLease(context.Background(), "slow-first-run", "this-console", time.Hour); err != nil {
		t.Fatal(err)
	}
	c := e.row("jobs")
	if c.Status != statusNotChecked || !strings.Contains(c.Detail, "first run") {
		t.Errorf("a job with no finished run, an hour after start, in its first run = %+v", c)
	}
}

// ---- MEDIUM-3: the clock row says what the engine saw ----

// r4Reporting leaves exactly the first n devices with a current heartbeat, offset by skew.
func (e *r3Env) r4Reporting(n int, skew time.Duration) []string {
	e.t.Helper()
	var ids []string
	now := e.clk.Now()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT p.device_id FROM device_presence p JOIN devices d ON d.id = p.device_id WHERE d.retired_at IS NULL ORDER BY p.device_id`)
		if err != nil {
			return err
		}
		var all []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			all = append(all, id)
		}
		_ = rows.Close()
		if _, err := tx.Exec(`UPDATE devices SET paused_at = NULL, paused_until = NULL, pause_reason = NULL, paused_by = NULL`); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE device_presence SET received_at = ?, clock_offset_ms = 0`, now.Add(-30*24*time.Hour).UnixMilli()); err != nil {
			return err
		}
		if n < 0 {
			n = len(all)
		}
		if n > len(all) {
			e.t.Fatalf("the fixture fleet has %d devices with a heartbeat", len(all))
		}
		ids = all[:n]
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE device_presence SET received_at = ?, clock_offset_ms = ? WHERE device_id = ?`, now.Add(-time.Minute).UnixMilli(), skew.Milliseconds(), id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
	return ids
}

func TestR4ClockRowAtTheEnginesBoundary(t *testing.T) { // r4 M3, C13
	// 4 reporting: C13 cannot run, however much they agree. Exactly 5: it can, and does.
	for _, tc := range []struct {
		n      int
		skew   time.Duration
		status string
		banner bool
	}{
		{4, 0, statusNotApplicable, false},
		{4, time.Hour, statusNotApplicable, false},
		{5, 0, statusOK, false},
		{5, time.Hour, statusWarn, true},
	} {
		e := newR3Env(t)
		e.r4Reporting(tc.n, tc.skew)
		h := e.health()
		var c SystemComponentJSON
		for _, x := range h.Components {
			if x.ID == "clock" {
				c = x
			}
		}
		raised := false
		for _, b := range h.Banners {
			raised = raised || b.ID == "C13"
		}
		if raised != tc.banner {
			t.Errorf("%d reporting, skew %v: C13 raised %v, want %v", tc.n, tc.skew, raised, tc.banner)
		}
		if c.Status != tc.status {
			t.Errorf("%d reporting, skew %v: clock = %+v, want %s", tc.n, tc.skew, c, tc.status)
		}
		if tc.n == 4 && (!strings.Contains(c.Detail, "needs at least 5 devices") || !strings.Contains(c.Detail, "4 report")) {
			t.Errorf("4 reporting: the row does not say what the engine saw: %q", c.Detail)
		}
		if tc.n == 5 && tc.skew == 0 && !strings.Contains(c.Detail, "5 ") {
			t.Errorf("5 reporting: the row does not say what the engine saw: %q", c.Detail)
		}
	}
}

// The engine counts a paused device with a current heartbeat.
func TestR4ClockRowCountsPausedDevicesAsTheEngineDoes(t *testing.T) { // r4 M3
	e := newR3Env(t)
	ids := e.r4Reporting(5, 0)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		for _, id := range ids[:2] {
			if _, err := tx.Exec(`UPDATE devices SET paused_at = ?, pause_reason = 'r' WHERE id = ?`, e.clk.Now().Add(-time.Hour).UnixMilli(), id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c := e.row("clock"); c.Status != statusOK {
		t.Errorf("5 reporting, 2 of them paused: %+v", c)
	}
}

// The engine does not count a device inside its first-backup grace with an old heartbeat, though UNSEEN is suppressed for it.
func TestR4ClockRowDoesNotCountPendingDevicesWithOldHeartbeats(t *testing.T) { // r4 M3
	e := newR3Env(t)
	e.r4Reporting(0, 0)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`DELETE FROM runs`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE devices SET first_backup_due_by = ? WHERE retired_at IS NULL`, e.clk.Now().Add(48*time.Hour).UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c := e.row("clock")
	if c.Status != statusNotApplicable || !strings.Contains(c.Detail, "0 report") {
		t.Errorf("no device reports, every one PENDING_FIRST: clock = %+v", c)
	}
}

// A scoped account's row is what the engine saw for that account, says so, and carries no number from outside it.
func TestR4ClockRowForAScopedAccountSaysItIsThatAccountsView(t *testing.T) { // r4 M3
	e := newR3Env(t)
	total := len(e.r4Reporting(-1, 0))
	nw := e.customerNamed("Northwind Dental")
	e.addAdmin("r4-scoped", "operator", []string{nw.ID})
	sc := e.login("r4-scoped", pw)
	h := decode[SystemHealthResponse](t, mustGet(e.env, "/api/v1/system/health", sc))
	var c SystemComponentJSON
	for _, x := range h.Components {
		if x.ID == "clock" {
			c = x
		}
	}
	if c.Status != statusNotApplicable || !strings.Contains(c.Detail, "this account") || !strings.Contains(c.Detail, "3 report") {
		t.Errorf("scoped: %+v, want not_applicable saying it is this account's view, with its own 3 devices", c)
	}
	if strings.Contains(c.Detail, strconv.Itoa(total)+" ") {
		t.Errorf("scoped: the row carries the whole fleet's count: %q", c.Detail)
	}
	if o := e.row("clock"); o.Status != statusOK {
		t.Errorf("the owner's view of the same fleet: %+v", o)
	}
}

// ---- LOW 4: every banner is pinned to its row, by name ----

func TestR4EveryBannerIsPinnedToItsRow(t *testing.T) { // r4 low 4 (A20)
	want := map[string]string{
		"C01": "gateway", "C01b": "gateway", "C02": "storage", "C02b": "storage", "C15": "storage_quota", "C15b": "storage_quota",
		"C03": "smtp", "C03b": "smtp", "C14": "digest", "C14b": "digest", "C05": "certificates", "C05b": "certificates",
		"C04": "agent_api", "C04b": "agent_api", "C07": "maintenance_worker", "C07b": "maintenance_worker", "C11": "database",
		"C06": "evaluator", "C06b": "evaluator", "C12": "master_key", "C12b": "master_key", "C08": "fleet_data", "C09": "fleet_data", "C10": "fleet_data",
		"C13": "clock", "C16": "release_manifest", "C16b": "release_manifest",
	}
	got := map[string]string{}
	for row, ids := range healthRowBanners {
		for _, id := range ids {
			got[id] = row
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d banners mapped, %d pinned here", len(got), len(want))
	}
	for id, row := range want {
		if got[id] != row {
			t.Errorf("banner %s is on row %q, want %q", id, got[id], row)
		}
	}
}
