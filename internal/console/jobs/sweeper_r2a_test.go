package jobs

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P22-R2a fixes F3, G3, G5: the abandoned-run-sweeper's log lines. A close is one warning with the
// count per reason, agent_silent among them; a close the reconnect grace holds back is one info
// line with the count and the end of the grace. Neither names an id or a path, and the message
// itself survives the log redaction (the old "abandoned-run-sweeper: ..." text was redacted whole).

// sweeperRig is a fixtures store with one running device restore whose device was last heard
// from 25 hours ago (past store.DeviceRestoreSilenceLimit).
func sweeperRig(t *testing.T) (*store.Store, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.Tx) error {
		var dev string
		if err := tx.QueryRow(`SELECT id FROM devices WHERE retired_at IS NULL ORDER BY id LIMIT 1`).Scan(&dev); err != nil {
			return err
		}
		fetched := now.Add(-26 * time.Hour).UnixMilli()
		if _, err := tx.Exec(`INSERT INTO restore_requests (id, device_id, snapshot_id, mode, overwrite, selection_count, selection_wrapped, status,
				started_at, expires_at, created_at, updated_at)
			VALUES ('01JR2A0000000000000000SW01', ?, ?, 'device_path', 'never', 1, X'0102', 'running', ?, ?, ?, ?)`,
			dev, strings.Repeat("ab", 32), fetched, fetched+24*3600*1000, fetched, fetched); err != nil {
			return err
		}
		heard := now.Add(-25 * time.Hour).UnixMilli()
		_, err := tx.Exec(`INSERT INTO device_presence (device_id, received_at, service_state, uptime_s, created_at, updated_at)
			VALUES (?, ?, 'running', ?, ?, ?) ON CONFLICT(device_id) DO UPDATE SET received_at = excluded.received_at,
			uptime_s = excluded.uptime_s, progress = NULL`, dev, heard, int64(30*24*3600), heard, heard)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return st, now
}

func TestR2A_SweeperLogsAgentSilentCloses(t *testing.T) {
	st, _ := sweeperRig(t)
	var buf bytes.Buffer
	j := &AbandonedRunSweeper{Store: st, Log: logx.New(&buf, logx.LevelDebug)}
	if err := j.Job().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"closed running device restores whose agent stopped reporting", `"count":1`, `"agent_silent":1`, `"agent_restarted":0`, `"no_run_record":0`} {
		if !strings.Contains(out, want) {
			t.Errorf("the sweep log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "had no run record") || strings.Contains(out, "01JR2A") || strings.Contains(out, "<path:") || !strings.Contains(out, `"job":"abandoned-run-sweeper"`) {
		t.Errorf("old wording or an id in the log:\n%s", out)
	}
}

func TestR2A_SweeperLogsCloseHeldBackByTheGrace(t *testing.T) {
	st, now := sweeperRig(t)
	var buf bytes.Buffer
	j := &AbandonedRunSweeper{Store: st, Log: logx.New(&buf, logx.LevelDebug), Options: store.SweepOptions{ConsoleStartedAt: now}}
	if err := j.Job().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	until := now.Add(store.DeviceRestoreReconnectGrace).Format("20060102T150405Z")
	for _, want := range []string{"device restores held open while agents reconnect", `"held_back":1`, `"grace_until_utc":"` + until + `"`, `"level":"INFO"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the held-back log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "closed running device restores") || strings.Contains(out, "<path:") {
		t.Errorf("a close was logged inside the grace:\n%s", out)
	}
}
