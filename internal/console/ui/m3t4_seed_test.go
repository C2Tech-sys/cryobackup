package ui

import (
	"context"
	"encoding/json"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// hostileRunID is the run of m3Seed that tries to put a file name and a path
// on every page that shows runs.
var hostileRunID = ulid(900)

// Strings the hostile run carries; none may reach a page.
var hostileTexts = []string{`secret-payroll.xlsx`, `C:\Users\someone`, `\\fileserver\share`, `/home/someone`}

// m3Seed adds runs of every outcome, a retried run, an open run and the hostile
// run (a path in an error class, a stats text and an agent version) to the
// fixture fleet, so every outcome chip and the path-free rule are on screen.
func (e *env) m3Seed() {
	e.t.Helper()
	fin := func(ago time.Duration) *time.Time { t := e.now.Add(-ago); return &t }
	mk := func(n int, host, outcome, kind string, started time.Duration, took time.Duration, mod func(*store.RunInput)) {
		in := store.RunInput{ID: ulid(n), Kind: kind, Trigger: "schedule", StartedAt: e.now.Add(-started), FinishedAt: fin(started - took), Outcome: outcome, Stats: json.RawMessage(`{"total_files_processed":1200,"failed_files":0,"files_new":14}`)}
		if outcome == "" {
			in.FinishedAt = nil
		}
		if mod != nil {
			mod(&in)
		}
		e.ingest(host, in)
	}
	h := time.Hour
	mk(1, "FRONTDESK-01", "success", "backup", 30*h, 12*time.Minute, func(r *store.RunInput) {
		r.SnapshotID = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
		v := int64(412 << 20)
		r.DataAddedBytes = &v
	})
	mk(2, "RECEPTION-2", "partial", "backup", 20*h, 9*time.Minute, func(r *store.RunInput) {
		r.ErrorClass, r.ErrorCounts = "permission", map[string]int{"permission": 23, "locked": 4}
		r.Stats = json.RawMessage(`{"total_files_processed":2000,"failed_files":27}`)
	})
	mk(3, "FRONTDESK-01", "failed", "backup", 18*h, 2*time.Minute, func(r *store.RunInput) { r.ErrorClass, r.ErrorCounts, r.Attempt = "vss", map[string]int{"vss": 1}, 2 })
	mk(4, "LT-OPTOM-04", "cancelled", "backup", 17*h, 3*time.Minute, nil)
	mk(5, "OPS-ND-01", "skipped", "backup", 16*h, time.Second, nil)
	mk(6, "LT-BILLING-1", "deferred", "backup", 15*h, 0, func(r *store.RunInput) { r.DeferredReason = "battery" })
	mk(7, "LEGAL-PC-01", "interrupted", "backup", 14*h, 5*time.Minute, nil)
	mk(8, "LEGAL-PC-02", "abandoned", "backup", 13*h, 6*time.Minute, func(r *store.RunInput) { r.ErrorClass = "network" })
	mk(9, "DR-OFFICE-PC", "", "backup", 20*time.Minute, 0, nil)
	mk(10, "PRINT-SRV-01", "success", "prune", 12*h, 20*time.Minute, nil)
	mk(11, "FRONTDESK-01", "failed", "backup", 2*h, 90*time.Second, func(r *store.RunInput) {
		r.ErrorClass, r.ErrorCounts = "vss", map[string]int{"vss": 3, "io": 1}
	})
	mk(900, "HYGIENE-PC-2", "failed", "backup", 3*h, 4*time.Minute, func(r *store.RunInput) {
		r.ErrorClass = "permission"
		r.ErrorCounts = map[string]int{"permission": 7, hostileTexts[1] + `\` + hostileTexts[0]: 2, hostileTexts[2]: 1}
		r.AgentVersion = "1.4.2"
		r.Stats = json.RawMessage(`{"total_files_processed":900,"failed_files":9,"note":"C:\\Users\\someone\\secret-payroll.xlsx","path":"/home/someone/secret-payroll.xlsx","nested":{"a":1}}`)
	})
}

// m3Silence makes seven of Summit Eye Care's devices go silent together (the
// engine's customer-wide silence, at least 5 active devices and half of them).
func (e *env) m3Silence() {
	e.t.Helper()
	hosts := []string{"FRONTDESK-01", "LT-OPTOM-04", "RECEPTION-2", "DR-OFFICE-PC", "EXAM-ROOM-3", "TECH-BENCH-07", "PRINT-SRV-01", "LT-SURGERY-02", "DR-LAPTOP-03"}
	old := e.now.Add(-10 * 24 * time.Hour).UnixMilli() // the engine calls a device unseen after 168 h
	err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		for _, h := range hosts {
			if _, err := tx.Exec(`UPDATE device_presence SET received_at = ? WHERE device_id = ?`, old, e.dev[h]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}
