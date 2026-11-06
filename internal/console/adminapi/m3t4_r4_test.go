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
		// Only these five exist: an aged device inside its first-backup grace would otherwise round the old count back up to 5.
		if _, err := tx.Exec(`UPDATE devices SET retired_at = ?, retired_reason = 'replaced' WHERE id NOT IN (?,?,?,?,?)`,
			e.clk.Now().Add(-time.Hour).UnixMilli(), ids[0], ids[1], ids[2], ids[3], ids[4]); err != nil {
			return err
		}
		for _, id := range ids[:2] {
			if _, err := tx.Exec(`UPDATE devices SET paused_at = ?, pause_reason = 'r' WHERE id = ?`, e.clk.Now().Add(-time.Hour).UnixMilli(), id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c := e.row("clock"); c.Status != statusOK || !strings.Contains(c.Detail, "compared with 5 reporting devices") {
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
