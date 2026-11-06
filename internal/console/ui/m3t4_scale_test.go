package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// TestM3T4ScaleMeasurement times each page on a console of about 3000 devices,
// with and without the per-request evaluation memo. It runs only with M3T4_SCALE=1
// and prints; it asserts nothing about time (a loaded PC is not a gate).
func TestM3T4ScaleMeasurement(t *testing.T) {
	if os.Getenv("M3T4_SCALE") == "" {
		t.Skip("M3T4_SCALE is not set")
	}
	e, _ := newM3Env(t)
	copies := 114
	cols := func(tx *store.Tx, table string) []string {
		rows, err := tx.Query(`SELECT name FROM pragma_table_info('` + table + `')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			out = append(out, n)
		}
		return out
	}
	err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		clone := func(table string, repl map[string]string, where string) error {
			cs := cols(tx, table)
			sel := make([]string, len(cs))
			for i, c := range cs {
				sel[i] = c
				if r, ok := repl[c]; ok {
					sel[i] = r
				}
			}
			for k := 1; k <= copies; k++ {
				pre := fmt.Sprintf("printf('%06d', %d) || substr(%%s, 7)", k, k)
				q := `INSERT INTO ` + table + ` (` + strings.Join(cs, ",") + `) SELECT ` + strings.Join(sel, ",") + ` FROM ` + table + ` ` + where
				q = strings.ReplaceAll(q, "{ID}", strings.Replace(pre, "%s", "id", 1))
				q = strings.ReplaceAll(q, "{DEV}", strings.Replace(pre, "%s", "device_id", 1))
				q = strings.ReplaceAll(q, "{K}", fmt.Sprint(k))
				if _, err := tx.Exec(q); err != nil {
					return fmt.Errorf("%s copy %d: %w", table, k, err)
				}
			}
			return nil
		}
		if err := clone("devices", map[string]string{"id": "{ID}", "hostname": "hostname || '-{K}'", "enroll_nonce": "enroll_nonce || '-{K}'", "repo_path": "repo_path || 'c{K}/'",
			"replaced_by_device_id": "NULL", "identity_conflict_at": "NULL"}, `WHERE retired_at IS NULL AND substr(id,1,6) NOT GLOB '[0-9][0-9][0-9][0-9][0-9][0-9]'`); err != nil {
			return err
		}
		if err := clone("device_presence", map[string]string{"device_id": "{DEV}"}, `WHERE device_id IN (SELECT id FROM devices WHERE retired_at IS NULL AND length(id) = 26 AND substr(id,1,6) NOT GLOB '[0-9][0-9][0-9][0-9][0-9][0-9]' )`); err != nil {
			return err
		}
		return clone("runs", map[string]string{"id": "{ID}", "device_id": "{DEV}"}, `WHERE device_id IN (SELECT id FROM devices WHERE retired_at IS NULL AND substr(id,1,6) NOT GLOB '[0-9][0-9][0-9][0-9][0-9][0-9]')`)
	})
	if err != nil {
		t.Fatal(err)
	}
	e.login()
	var n int
	_ = e.st.Read(context.Background(), func(tx *store.Tx) error { return tx.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&n) })
	t.Logf("devices: %d", n)
	var evals int
	e.svc.Fleet.Extra = func(time.Time) []state.Banner { evals++; return nil }
	summit := e.customerID("Summit Eye Care")
	for _, memo := range []bool{false, true} {
		m3Memo = memo
		for _, p := range []string{"/", "/fleet/overdue", "/fleet/unseen", "/activity", "/activity?customer=" + e.customerID("Northwind Dental"), "/system", "/runs", "/customers/" + summit + "/runs"} {
			evals = 0
			start := time.Now()
			res, body := e.do("GET", p, nil)
			t.Logf("memo=%-5v %-40s %d %5.2fs %4d KB, %d evaluations", memo, p, res.StatusCode, time.Since(start).Seconds(), len(body)/1024, evals)
		}
	}
	m3Memo = true
}
