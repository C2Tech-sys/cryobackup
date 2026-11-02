package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

func TestThresholdsFromPolicy(t *testing.T) {
	// The proposed defaults map onto the engine's own defaults exactly.
	if got, want := ThresholdsFromPolicy(policy.Default(policy.ClassUnknown)), state.DefaultThresholds(); got != want {
		t.Fatalf("default policy thresholds = %+v\nwant state.DefaultThresholds() = %+v", got, want)
	}
	mod := func(f func(p *policy.Policy)) policy.Policy {
		p := policy.Default(policy.ClassLaptop)
		f(&p)
		return p
	}
	cases := []struct {
		name string
		p    policy.Policy
		want func(th *state.Thresholds)
	}{
		{"every sla field", mod(func(p *policy.Policy) {
			p.SLA = policy.SLA{StaleAfterHours: 48, UnseenAfterHours: 96, FirstBackupGraceHours: 24, SeedExtensionHours: 12,
				PartialTolerance: policy.PartialTolerance{MaxFailedFiles: 5, MaxFailedPct: 1.5}, ConsecutiveFailuresToAlarm: 3,
				MaxRunHours: 6, InitialMaxRunHours: 100, StallAfterMinutes: 90, MissedFactor: 3, EscrowVerifyGraceHours: 12,
				EscrowRecheckDays: 7, MaintenanceDeferralAlarmAfter: 4}
		}), func(th *state.Thresholds) {
			*th = state.Thresholds{StaleAfter: 48 * time.Hour, UnseenAfter: 96 * time.Hour, FirstBackupGrace: 24 * time.Hour,
				SeedExtension: 12 * time.Hour, MaxFailedFiles: 5, MaxFailedPct: 1.5, ConsecutiveFailuresToAlarm: 3, MaxRun: 6 * time.Hour,
				InitialMaxRun: 100 * time.Hour, StallAfter: 90 * time.Minute, MissedFactor: 3, EscrowVerifyGrace: 12 * time.Hour,
				EscrowRecheck: 7 * 24 * time.Hour, MaintenanceDeferralAlarmAfter: 4, Interval: 24 * time.Hour}
		}},
		{"interval minutes", mod(func(p *policy.Policy) { p.Schedule.IntervalMinutes = 90 }), func(th *state.Thresholds) { th.Interval = 90 * time.Minute }},
		{"fixed times: 24 h over the number of slots", mod(func(p *policy.Policy) {
			p.Schedule.Mode, p.Schedule.FixedTimes = policy.ModeFixed, []string{"02:00", "10:00", "18:00"}
		}), func(th *state.Thresholds) { th.Interval = 8 * time.Hour }},
		{"fixed with unusable times falls back to the interval", mod(func(p *policy.Policy) {
			p.Schedule.Mode, p.Schedule.FixedTimes, p.Schedule.IntervalMinutes = policy.ModeFixed, []string{"noon", "25:99"}, 600
		}), func(th *state.Thresholds) { th.Interval = 10 * time.Hour }},
		{"fixed with one usable time among junk", mod(func(p *policy.Policy) {
			p.Schedule.Mode, p.Schedule.FixedTimes = policy.ModeFixed, []string{"03:30", "junk"}
		}), func(th *state.Thresholds) { th.Interval = 24 * time.Hour }},
		{"no schedule at all", mod(func(p *policy.Policy) { p.Schedule.IntervalMinutes = 0 }), func(th *state.Thresholds) { th.Interval = 0 }},
		{"outdated is an alarm", mod(func(p *policy.Policy) { p.NotificationOverrides.OutdatedAlarm = true }), func(th *state.Thresholds) { th.OutdatedIsAlarm = true }},
	}
	for _, c := range cases {
		want := state.DefaultThresholds()
		c.want(&want)
		if got := ThresholdsFromPolicy(c.p); got != want {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, got, want)
		}
	}
	if th := ThresholdsFromPolicy(policy.Default(policy.ClassUnknown)); th.WindowSlack != 0 {
		t.Fatalf("WindowSlack is zero in v0, got %v", th.WindowSlack)
	}
}

func facts(t *testing.T, w *world, deviceID string) state.DeviceFacts {
	t.Helper()
	return must[state.DeviceFacts](t)(w.s.LoadDeviceFacts(w.ctx, ScopeAll(), deviceID))
}

func ms(t time.Time) state.Millis { return state.Millis(t.UnixMilli()) }

func TestFactsFromRuns(t *testing.T) {
	w := seed(t)
	w.heartbeat(w.devA.ID)

	// Before any run: nothing, but the facts are complete and carry the identity of the device.
	f := facts(t, w, w.devA.ID)
	if !f.Complete || f.LastSuccess != 0 || f.LastBackup != nil || f.OpenRun != nil || f.ConsecutiveFailures != 0 {
		t.Fatalf("empty facts = %+v", f)
	}
	if f.ID != w.devA.ID || f.CustomerID != w.custA.ID || f.Hostname != "A-DESK-1" || f.EnrolledAt != ms(t0) {
		t.Fatalf("identity facts = %+v", f)
	}
	if f.LastSeen != ms(t0) || f.ServiceState != "idle" || f.AgentVersion != "1.4.2" {
		t.Fatalf("presence facts = %+v", f)
	}

	// Three successes, the newest by finish time wins regardless of insertion order.
	ingest(t, w, w.devA.ID,
		runInput("01J9Y4K3A0B1C2D3E4F5G6H7Q2", t0.Add(-30*time.Hour)),
		runInput("01J9Y4K3A0B1C2D3E4F5G6H7Q1", t0.Add(-50*time.Hour)),
		runInput("01J9Y4K3A0B1C2D3E4F5G6H7Q3", t0.Add(-10*time.Hour)))
	f = facts(t, w, w.devA.ID)
	if f.LastSuccess != ms(t0.Add(-10*time.Hour)) {
		t.Fatalf("LastSuccess = %v, want the newest finish", f.LastSuccess)
	}
	if f.LastBackup == nil || f.LastBackup.Outcome != "success" || f.LastBackup.FinishedAt != ms(t0.Add(-10*time.Hour)) {
		t.Fatalf("LastBackup = %+v", f.LastBackup)
	}
	if !f.LastBackup.CountsKnown || f.LastBackup.TotalFiles != 1000 || f.LastBackup.FailedFiles != 0 {
		t.Fatalf("counts = %+v", f.LastBackup)
	}
	if f.LastAttemptStart != ms(t0.Add(-10*time.Hour-10*time.Minute)) {
		t.Fatalf("LastAttemptStart = %v", f.LastAttemptStart)
	}
	// These successes finished BEFORE the device enrolled (it enrolled at t0): runs from before a
	// (re)enrollment say nothing about the new one, so the device is still on its initial backup.
	if !f.LastBackup.Initial {
		t.Fatal("successes from before enrollment must not end the initial run")
	}
	w.clk.Advance(3 * time.Hour)
	ingest(t, w, w.devA.ID, runInput("01J9Y4K3A0B1C2D3E4F5G6H7Q5", t0.Add(2*time.Hour)))
	if g := facts(t, w, w.devA.ID); g.LastBackup.Initial || g.LastSuccess != ms(t0.Add(2*time.Hour)) {
		t.Fatalf("a success after enrollment ends the initial run: %+v", g.LastBackup)
	}

	// An open run is reported as the open run, not as the last backup.
	ingest(t, w, w.devA.ID, RunInput{ID: "01J9Y4K3A0B1C2D3E4F5G6H7Q4", Kind: "backup", Trigger: "schedule", StartedAt: t0.Add(-20 * time.Minute)})
	f = facts(t, w, w.devA.ID)
	if f.OpenRun == nil || f.OpenRun.Outcome != "" || f.OpenRun.StartedAt != ms(t0.Add(-20*time.Minute)) || f.OpenRun.FinishedAt != 0 {
		t.Fatalf("OpenRun = %+v", f.OpenRun)
	}
	if f.LastBackup.FinishedAt != ms(t0.Add(2*time.Hour)) {
		t.Fatalf("an open run replaced LastBackup: %+v", f.LastBackup)
	}
	// An open maintenance run is not a backup open run.
	must[Run](t)(w.s.CreateConsoleRun(w.ctx, ScopeAll(), ConsoleRunInput{DeviceID: w.devA.ID, Kind: "check"}))
	if g := facts(t, w, w.devA.ID); g.OpenRun == nil || g.OpenRun.StartedAt != ms(t0.Add(-20*time.Minute)) {
		t.Fatalf("OpenRun picked up a console run: %+v", g.OpenRun)
	}
}

// Neutral outcomes (cancelled, skipped, interrupted, deferred) neither count as failures nor break a streak.
func TestFactsConsecutiveFailuresSkipNeutralOutcomes(t *testing.T) {
	w := seed(t)
	mk := func(id string, ago time.Duration, outcome string, deferred string) RunInput {
		r := runInput(id, t0.Add(-ago))
		r.Outcome, r.DeferredReason = outcome, deferred
		return r
	}
	ingest(t, w, w.devA.ID,
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R1", 20*time.Hour, "success", ""),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R2", 18*time.Hour, "failed", ""),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R3", 16*time.Hour, "cancelled", ""),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R4", 14*time.Hour, "failed", ""),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R5", 12*time.Hour, "deferred", "gateway"),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R6", 10*time.Hour, "skipped", ""),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R7", 8*time.Hour, "interrupted", ""),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R8", 6*time.Hour, "abandoned", ""),
		mk("01J9Y4K3A0B1C2D3E4F5G6H7R9", 25*time.Hour, "failed", ""), // before the success: not in the streak
	)
	f := facts(t, w, w.devA.ID)
	if f.ConsecutiveFailures != 3 {
		t.Fatalf("ConsecutiveFailures = %d, want 3 (failed, failed, abandoned; neutral runs skipped; the failure before the success excluded)", f.ConsecutiveFailures)
	}
	if f.LastBackup.Outcome != "abandoned" {
		t.Fatalf("LastBackup = %+v", f.LastBackup)
	}
	if f.LastSuccess != ms(t0.Add(-20*time.Hour)) {
		t.Fatalf("LastSuccess = %v", f.LastSuccess)
	}
	// A deferred run carries its reason.
	ingest(t, w, w.devA.ID, mk("01J9Y4K3A0B1C2D3E4F5G6H7S1", 1*time.Hour, "deferred", "battery"))
	if g := facts(t, w, w.devA.ID); g.LastBackup.DeferredReason != "battery" {
		t.Fatalf("DeferredReason = %q", g.LastBackup.DeferredReason)
	}
	// A success ends the streak.
	ingest(t, w, w.devA.ID, mk("01J9Y4K3A0B1C2D3E4F5G6H7S2", 30*time.Minute, "success", ""))
	if g := facts(t, w, w.devA.ID); g.ConsecutiveFailures != 0 {
		t.Fatalf("a success must reset the streak, got %d", g.ConsecutiveFailures)
	}
}

func TestFactsPartialCountsAndVSS(t *testing.T) {
	w := seed(t)
	custom := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "D drive only",
		Sections: sections("selection", `{"roots":["D:\\Data"]}`)}))
	a := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custA.ID))
	a.PolicyID = custom.ID
	must[Customer](t)(w.s.UpdateCustomer(w.ctx, ScopeAll(), w.custA.ID, a))

	partial := func(id, stats string, finish time.Time) RunInput {
		r := runInput(id, finish)
		r.Outcome, r.Stats = "partial", []byte(stats)
		r.ErrorClass, r.ErrorCounts = "permission", map[string]int{"permission": 23}
		return r
	}
	cases := []struct {
		name        string
		stats       string
		known       bool
		failed, tot int64
		vss         []string
		onRoot      bool
	}{
		{"counts and a fallback on a volume that is not a root", `{"total_files_processed":2000,"failed_files":23,"vss_fallback_volumes":["C"]}`, true, 23, 2000, []string{"C"}, false},
		{"fallback on the D: root (explicit root of the customer policy)", `{"total_files_processed":2000,"failed_files":23,"vss_fallback_volumes":["d:"]}`, true, 23, 2000, []string{"D"}, true},
		{"counts missing: fail loud", `{}`, false, 0, 0, nil, false},
		{"only one of the counts: still unknown", `{"total_files_processed":2000}`, false, 0, 0, nil, false},
		{"garbage counts", `{"total_files_processed":"many","failed_files":-1}`, false, 0, 0, nil, false},
	}
	for i, c := range cases {
		id := "01J9Y4K3A0B1C2D3E4F5G6H7T" + string(rune('0'+i))
		ingest(t, w, w.devA.ID, partial(id, c.stats, t0.Add(-time.Hour+time.Duration(i)*time.Minute))) // each case is the newest run
		f := facts(t, w, w.devA.ID)
		lb := f.LastBackup
		if lb == nil || lb.Outcome != "partial" || lb.CountsKnown != c.known || lb.FailedFiles != c.failed || lb.TotalFiles != c.tot {
			t.Errorf("%s: LastBackup = %+v", c.name, lb)
			continue
		}
		if strings.Join(lb.VSSFallback, ",") != strings.Join(c.vss, ",") || lb.VSSFallbackOnRoot != c.onRoot {
			t.Errorf("%s: VSSFallback=%v OnRoot=%v, want %v %v", c.name, lb.VSSFallback, lb.VSSFallbackOnRoot, c.vss, c.onRoot)
		}
		if lb.ErrorClass != "permission" || lb.ErrorCounts["permission"] != 23 {
			t.Errorf("%s: error facts = %+v", c.name, lb)
		}
		w.clk.Advance(time.Minute)
	}
	// The default policy's profile preset lives on the system drive: a C: fallback is on a root there (customer B).
	r := runInput("01J9Y4K3A0B1C2D3E4F5G6H7V1", t0.Add(-time.Hour))
	r.Outcome, r.Stats = "partial", []byte(`{"total_files_processed":2000,"failed_files":23,"vss_fallback_volumes":["C"]}`)
	r.ErrorClass = "permission"
	ingest(t, w, w.devB.ID, r)
	if lb := facts(t, w, w.devB.ID).LastBackup; !lb.VSSFallbackOnRoot {
		t.Fatalf("a C: fallback under all_user_profiles must be on a root: %+v", lb)
	}
	if wd := must[state.World](t)(w.s.LoadWorld(w.ctx, ScopeAll(), LoadOptions{SystemDrive: "E"})); len(wd.Devices) == 2 {
		for _, d := range wd.Devices {
			if d.ID == w.devB.ID && d.LastBackup.VSSFallbackOnRoot {
				t.Fatal("with SystemDrive E a C: fallback is not on the profile preset's drive")
			}
		}
	}
	// fixed_drives covers every volume.
	fx := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "all drives",
		Sections: sections("selection", `{"presets":["fixed_drives"]}`)}))
	b := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custB.ID))
	b.PolicyID = fx.ID
	must[Customer](t)(w.s.UpdateCustomer(w.ctx, ScopeAll(), w.custB.ID, b))
	r2 := runInput("01J9Y4K3A0B1C2D3E4F5G6H7V2", t0)
	r2.Outcome, r2.Stats = "partial", []byte(`{"total_files_processed":10,"failed_files":1,"vss_fallback_volumes":["Q"]}`)
	ingest(t, w, w.devB.ID, r2)
	if lb := facts(t, w, w.devB.ID).LastBackup; !lb.VSSFallbackOnRoot {
		t.Fatalf("fixed_drives covers every drive: %+v", lb)
	}
}

func TestFactsEscrowMaintenanceAckAndVersions(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	f := facts(t, w, w.devA.ID)
	if f.Escrow == nil || f.Escrow.Custody != "escrowed" || f.Escrow.VerifiedAt != 0 || f.Escrow.CreatedAt != ms(t0) {
		t.Fatalf("escrow = %+v", f.Escrow)
	}
	if f.CustomerKeyCustody != "escrowed" {
		t.Fatalf("customer custody = %q", f.CustomerKeyCustody)
	}
	if f.Maintenance == nil || f.Maintenance.ForgetCadence != 7*24*time.Hour || f.Maintenance.DeferredCount != 0 || f.Maintenance.LastForget != nil {
		t.Fatalf("maintenance = %+v", f.Maintenance)
	}

	// Verification, a failed verification, deferrals and maintenance runs.
	ok(t, w.s.Write(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(`UPDATE repo_key_escrow SET verified_at = ?, last_verify_error_class = 'auth' WHERE device_id = ?`, t0.Add(-time.Hour).UnixMilli(), w.devA.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE maintenance_schedule SET deferred_count = 4 WHERE device_id = ?`, w.devA.ID)
		return err
	}))
	for _, r := range []struct {
		kind, outcome string
		ago           time.Duration
	}{{"forget", "success", 80 * time.Hour}, {"forget", "failed", 30 * time.Hour}, {"prune", "success", 50 * time.Hour}, {"check", "success", 40 * time.Hour}, {"forget", "success", 70 * time.Hour}} {
		run := must[Run](t)(w.s.CreateConsoleRun(ctx, ScopeAll(), ConsoleRunInput{DeviceID: w.devA.ID, Kind: r.kind}))
		ok(t, w.s.Write(ctx, func(tx *Tx) error {
			_, err := tx.Exec(`UPDATE runs SET started_at = ?, finished_at = ?, received_at = ?, outcome = ? WHERE id = ?`,
				t0.Add(-r.ago-time.Minute).UnixMilli(), t0.Add(-r.ago).UnixMilli(), t0.Add(-r.ago).UnixMilli(), r.outcome, run.ID)
			return err
		}))
	}
	f = facts(t, w, w.devA.ID)
	if f.Escrow.VerifiedAt != ms(t0.Add(-time.Hour)) || f.Escrow.LastVerifyErrorClass != "auth" {
		t.Fatalf("escrow after verification = %+v", f.Escrow)
	}
	m := f.Maintenance
	if m.DeferredCount != 4 || m.LastForget == nil || m.LastForget.Outcome != "failed" || m.LastPrune == nil || m.LastPrune.Outcome != "success" ||
		m.LastCheck == nil || m.LastForgetSuccessAt != ms(t0.Add(-70*time.Hour)) {
		t.Fatalf("maintenance = %+v last forget %+v", m, m.LastForget)
	}

	// Active acknowledgement and snooze.
	snooze := t0.Add(24 * time.Hour).UnixMilli()
	ingest(t, w, w.devA.ID, runInput(runID1, t0.Add(-5*time.Hour)))
	ok(t, w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO alarm_acks (id, subject_type, device_id, alarm_type, acknowledged_by, acknowledged_at, snooze_until, snooze_until_next_success, created_at, updated_at)
			VALUES (?, 'device', ?, 'stale', ?, ?, ?, 1, 1, 1)`, tx.id(), w.devA.ID, w.admin.ID, t0.UnixMilli(), snooze)
		return err
	}))
	a := facts(t, w, w.devA.ID).Ack
	if a == nil || a.AckedAt != ms(t0) || a.SnoozeUntil != state.Millis(snooze) || !a.SnoozeUntilNextSuccess || a.AckedAtSuccess != ms(t0.Add(-5*time.Hour)) {
		t.Fatalf("ack = %+v", a)
	}

	// Expected version: the pin wins; else the newest non-withdrawn release on the device's ring.
	rel := func(version, channel string, releasedAgo time.Duration, withdrawn bool) {
		ok(t, w.s.Write(ctx, func(tx *Tx) error {
			var wd any
			if withdrawn {
				wd = t0.UnixMilli()
			}
			_, err := tx.Exec(`INSERT INTO agent_releases (id, version, channel, sha256, signature, restic_version, restic_sha256, min_os, manifest, artifact_ref, released_at, withdrawn_at, created_at, updated_at)
				VALUES (?,?,?,?, 'sig', '0.19.1', ?, '10.0', '{}', 'a.msi', ?, ?, 1, 1)`, tx.id(), version, channel, strings.Repeat("a", 64), strings.Repeat("b", 64), t0.Add(-releasedAgo).UnixMilli(), wd)
			return err
		}))
	}
	if f = facts(t, w, w.devA.ID); f.ExpectedVersion != "" {
		t.Fatalf("no releases: expected version = %q", f.ExpectedVersion)
	}
	rel("1.4.0", "stable", 90*24*time.Hour, false)
	rel("1.5.0", "stable", 10*24*time.Hour, false)
	rel("1.6.0", "stable", 24*time.Hour, true) // withdrawn: ignored
	rel("1.7.0", "pilot", 24*time.Hour, false)
	if f = facts(t, w, w.devA.ID); f.ExpectedVersion != "1.5.0" {
		t.Fatalf("stable ring expects %q, want 1.5.0", f.ExpectedVersion)
	}
	ring := "pilot"
	must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), w.devA.ID, DeviceUpdate{UpdateRing: &ring}))
	if f = facts(t, w, w.devA.ID); f.ExpectedVersion != "1.7.0" {
		t.Fatalf("pilot ring expects %q, want 1.7.0", f.ExpectedVersion)
	}
	pin := "1.4.1"
	must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), w.devA.ID, DeviceUpdate{TargetVersion: &pin}))
	if f = facts(t, w, w.devA.ID); f.ExpectedVersion != "1.4.1" {
		t.Fatalf("a pinned device expects %q, want its pin 1.4.1", f.ExpectedVersion)
	}
}

func TestFactsPolicyThresholdsAndDrift(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	rec := must[PolicyRecord](t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "tight",
		Sections: sections("sla", `{"stale_after_hours":48,"unseen_after_hours":96}`, "schedule", `{"interval_minutes":480}`, "notification_overrides", `{"outdated_alarm":true}`)}))
	a := must[Customer](t)(w.s.GetCustomer(ctx, ScopeAll(), w.custA.ID))
	a.PolicyID = rec.ID
	must[Customer](t)(w.s.UpdateCustomer(ctx, ScopeAll(), w.custA.ID, a))
	f := facts(t, w, w.devA.ID)
	th := f.Thresholds
	if th.StaleAfter != 48*time.Hour || th.UnseenAfter != 96*time.Hour || th.Interval != 8*time.Hour || !th.OutdatedIsAlarm {
		t.Fatalf("thresholds = %+v", th)
	}
	if f.PolicyVersion != 1 || f.PolicyVersionApplied != 0 || !f.HasSchedule {
		t.Fatalf("policy facts: version %d applied %d schedule %v", f.PolicyVersion, f.PolicyVersionApplied, f.HasSchedule)
	}
	// Customer B keeps the default thresholds.
	if g := facts(t, w, w.devB.ID).Thresholds; g.StaleAfter != 72*time.Hour {
		t.Fatalf("customer B thresholds = %+v", g)
	}
	// Applying the policy removes the drift.
	pv := 1
	ok(t, w.s.AgentUpsertPresence(ctx, w.devA.ID, Presence{ServiceState: "idle", PolicyVersionApplied: &pv}))
	if f = facts(t, w, w.devA.ID); f.PolicyVersionApplied != 1 {
		t.Fatalf("applied = %d", f.PolicyVersionApplied)
	}
}

func TestFactsRetiredAndCustomerHeldDevices(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	held := must[Customer](t)(w.s.CreateCustomer(ctx, ScopeAll(), Customer{Name: "Held Co", KeyCustody: "customer_held"}))
	must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(ctx, ScopeAll(), EnrollmentToken{CustomerID: held.ID, TokenHash: sha256Hex("tok-h"), ExpiresAt: t0.Add(time.Hour)}))
	in := enrollInput("tok-h", "H-1")
	in.RepoKeyWrapped, in.WrapAlg, in.MasterKeyID = nil, "", ""
	h := must[EnrollResult](t)(w.s.AgentEnroll(ctx, in)).Device
	f := facts(t, w, h.ID)
	if f.Maintenance != nil || f.Escrow == nil || f.Escrow.Custody != "customer_held" || f.CustomerKeyCustody != "customer_held" || !f.Complete {
		t.Fatalf("customer-held facts = %+v", f)
	}
	must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), w.devB.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "gone"}}))
	g := facts(t, w, w.devB.ID)
	if g.RetiredAt == 0 || !g.Complete {
		t.Fatalf("retired facts = %+v", g)
	}
	// Retired devices are not queried: even a broken table leaves them complete.
	ok(t, w.s.Write(ctx, func(tx *Tx) error { _, err := tx.Exec(`DROP TABLE device_presence`); return err }))
	if g = facts(t, w, w.devB.ID); !g.Complete {
		t.Fatalf("a retired device went incomplete: %+v", g)
	}
}

// §4.1 / DH7: the queries must use the partial indexes the schema built for them.
func TestFactQueriesUseTheirIndexes(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	plan := func(q string, args ...any) string {
		var lines []string
		ok(t, s.Read(ctx, func(tx *Tx) error {
			rows, err := tx.Query(`EXPLAIN QUERY PLAN `+q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					return err
				}
				lines = append(lines, detail)
			}
			return rows.Err()
		}))
		return strings.Join(lines, " | ")
	}
	for _, c := range []struct{ name, query, index string }{
		{"last success", lastSuccessSQL, "idx_runs_last_success"},
		{"last finished backup", backupRunSQL(doneBackupWhere, doneBackupOrder), "idx_runs_last_backup_done"},
		{"open backup run", backupRunSQL(openBackupWhere, openBackupOrder), "idx_runs_open"},
		{"maintenance runs", strings.Replace(maintRunSQL, "kind = ?", "kind = 'forget'", 1), "idx_runs_maint_done"},
	} {
		args := []any{"x"}
		got := plan(c.query, args...)
		if !strings.Contains(got, c.index) {
			t.Errorf("%s: plan %q does not use %s", c.name, got, c.index)
		}
	}
	if got := plan(`SELECT 1 FROM repo_key_escrow WHERE device_id = ? AND retired_at IS NULL`, "x"); !strings.Contains(got, "uq_repo_key_escrow_active") {
		t.Errorf("escrow: plan %q", got)
	}
	if got := plan(`SELECT 1 FROM alarm_acks WHERE device_id = ? AND cleared_at IS NULL`, "x"); !strings.Contains(got, "uq_alarm_acks_active_device") && !strings.Contains(got, "idx_alarm_acks_device") {
		t.Errorf("acks: plan %q", got)
	}
	if got := plan(`SELECT version FROM agent_releases WHERE channel = ? AND withdrawn_at IS NULL ORDER BY released_at DESC LIMIT 1`, "stable"); !strings.Contains(got, "idx_agent_releases_channel") {
		t.Errorf("releases: plan %q", got)
	}
}

// ---------------------------------------------------------------------------
// fail loud
// ---------------------------------------------------------------------------

func TestLoaderFailsLoudWhenAQueryFails(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	w.heartbeat(w.devA.ID)
	w.heartbeat(w.devB.ID)
	ingest(t, w, w.devA.ID, runInput(runID1, t0.Add(-time.Hour)))
	ok(t, w.s.Write(ctx, func(tx *Tx) error { _, err := tx.Exec(`DROP TABLE device_presence`); return err }))

	world, err := w.s.LoadWorld(ctx, ScopeAll(), LoadOptions{})
	if err != nil {
		t.Fatalf("a per-device failure must not fail the load: %v", err)
	}
	if world.LoadComplete || world.LoadErrors != 2 {
		t.Fatalf("LoadComplete=%v LoadErrors=%d, want false and 2", world.LoadComplete, world.LoadErrors)
	}
	for _, d := range world.Devices {
		if d.Complete || d.LoadErrorClass == "" {
			t.Fatalf("device %s: Complete=%v class=%q", d.Hostname, d.Complete, d.LoadErrorClass)
		}
		if strings.ContainsAny(d.LoadErrorClass, " :\\/") {
			t.Fatalf("the error class %q carries text", d.LoadErrorClass)
		}
	}
	view := state.Evaluate(world, t0)
	if view.AllClear() || view.Complete {
		t.Fatal("a partial load must never be all clear")
	}
	var c09 bool
	for _, b := range view.Banners {
		if b.ID == "C09" {
			c09 = true
		}
	}
	if !c09 {
		t.Fatalf("banners = %+v, want C09", view.Banners)
	}
	for _, d := range view.Devices {
		if d.Primary != state.StateUnknown {
			t.Errorf("device %s is %s, want UNKNOWN", d.Hostname, d.Primary)
		}
	}
	// A single device through LoadDeviceFacts: incomplete, not an error.
	if f := facts(t, w, w.devA.ID); f.Complete {
		t.Fatal("LoadDeviceFacts reported complete facts after the failure")
	}
}

func TestLoaderCancelledContext(t *testing.T) {
	w := seed(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	world, err := w.s.LoadWorld(ctx, ScopeAll(), LoadOptions{})
	if err == nil || world.LoadComplete {
		t.Fatalf("a cancelled load returned err=%v complete=%v", err, world.LoadComplete)
	}
	if _, err := w.s.LoadDeviceFacts(ctx, ScopeAll(), w.devA.ID); err == nil {
		t.Fatal("LoadDeviceFacts ignored a cancelled context")
	}
}

func TestLoaderWithoutDefaultPolicyIsUnknownNotGuessed(t *testing.T) {
	w := seed(t)
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error { _, err := tx.Exec(`DELETE FROM policies WHERE scope = 'default'`); return err }))
	world := must[state.World](t)(w.s.LoadWorld(w.ctx, ScopeAll(), LoadOptions{}))
	if world.LoadComplete || world.LoadErrors != 2 {
		t.Fatalf("complete=%v errors=%d", world.LoadComplete, world.LoadErrors)
	}
	for _, d := range world.Devices {
		if d.LoadErrorClass != "policy_missing" {
			t.Fatalf("class = %q, want policy_missing", d.LoadErrorClass)
		}
	}
}

func TestPreviousDeviceCountTrackingPerScope(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	load := func(sc Scope) state.World {
		return must[state.World](t)(w.s.LoadWorld(ctx, sc, LoadOptions{}))
	}
	if got := load(ScopeAll()).PreviousDeviceCount; got != -1 {
		t.Fatalf("first load: PreviousDeviceCount = %d, want -1", got)
	}
	if got := load(ScopeAll()).PreviousDeviceCount; got != 2 {
		t.Fatalf("second load: %d, want 2", got)
	}
	// Another scope has its own history: a scoped load does not inherit (or poison) the global count.
	if got := load(ScopeOf(w.custA.ID)).PreviousDeviceCount; got != -1 {
		t.Fatalf("first scoped load: %d, want -1", got)
	}
	if got := load(ScopeOf(w.custA.ID)).PreviousDeviceCount; got != 1 {
		t.Fatalf("second scoped load: %d, want 1", got)
	}
	// An empty poll after a non-empty one keeps the previous count, so C09 keeps firing until devices return.
	ok(t, w.s.Write(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(`DELETE FROM devices`); err != nil {
			return err
		}
		return nil
	}))
	empty := load(ScopeAll())
	if len(empty.Devices) != 0 || empty.PreviousDeviceCount != 2 {
		t.Fatalf("empty poll: %d devices, previous %d", len(empty.Devices), empty.PreviousDeviceCount)
	}
	view := state.Evaluate(empty, t0)
	var c09 bool
	for _, b := range view.Banners {
		c09 = c09 || b.ID == "C09"
	}
	if !c09 || view.AllClear() {
		t.Fatalf("an empty poll after 2 devices must raise C09: %+v", view.Banners)
	}
	if again := load(ScopeAll()); again.PreviousDeviceCount != 2 {
		t.Fatalf("the empty poll replaced the previous count: %d", again.PreviousDeviceCount)
	}
}

func TestLoadWorldFleetEverEnrolled(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	w1 := must[state.World](t)(s.LoadWorld(ctx, ScopeAll(), LoadOptions{}))
	if w1.FleetEverEnrolled || len(w1.Devices) != 0 || !w1.LoadComplete {
		t.Fatalf("empty database: %+v", w1)
	}
	if v := state.Evaluate(w1, t0); !v.FirstRun || len(v.Banners) != 0 {
		t.Fatalf("a first run is a neutral panel, not a banner: %+v", v)
	}
	// Scoped loads with an invalid scope fail closed.
	if _, err := s.LoadWorld(ctx, Scope{}, LoadOptions{}); !errors.Is(err, ErrScope) {
		t.Fatalf("LoadWorld(zero) = %v", err)
	}
}
