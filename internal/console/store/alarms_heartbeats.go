package store

import (
	"context"
	"database/sql"
	"time"
)

// The heartbeat ring of one device (M3-T3, architecture §5.2
// GET /devices/{id}/heartbeats, F13 F14 F18). The ring itself is kept by the
// heartbeat-ring-trim job (24 hours); this file only reads it, through Scope.

// HeartbeatRow is one heartbeat as the ring stores it. It holds presence and
// health counters only: no hostname, no path.
type HeartbeatRow struct {
	ReceivedAt     time.Time
	AgentVersion   string
	UptimeS        *int64
	OnAC           *bool
	BatteryPct     *int
	Metered        *bool
	VPN            *bool
	QueueDepth     *int
	JournalDepth   *int
	ClockOffsetMS  *int64
	ServiceState   string
	LastErrorClass string
}

// MaxHeartbeatRows bounds one ring read (a day of one-minute heartbeats is 1440).
const MaxHeartbeatRows = 2000

// ListHeartbeats returns the device's heartbeats received at or after since
// (zero: the whole ring), newest first, at most limit (0 or more than
// MaxHeartbeatRows: MaxHeartbeatRows). truncated is true when older rows in the
// window were left out. A device outside scope is ErrNotFound.
func (s *Store) ListHeartbeats(ctx context.Context, scope Scope, deviceID string, since time.Time, limit int) (rows []HeartbeatRow, truncated bool, err error) {
	if err := scope.check(); err != nil {
		return nil, false, err
	}
	if limit <= 0 || limit > MaxHeartbeatRows {
		limit = MaxHeartbeatRows
	}
	err = s.Read(ctx, func(tx *Tx) error {
		if _, err := scopedDevice(tx, scope, deviceID); err != nil {
			return err
		}
		q := `SELECT received_at, agent_version, uptime_s, on_ac, battery_pct, metered, vpn, queue_depth, journal_depth,
			clock_offset_ms, service_state, last_error_class FROM heartbeats WHERE device_id = ?`
		args := []any{deviceID}
		if !since.IsZero() {
			q, args = q+` AND received_at >= ?`, append(args, msOf(since))
		}
		var err error
		rows, err = collect(tx, q+` ORDER BY received_at DESC, id DESC LIMIT ?`, append(args, limit+1), func(sc scanner) (HeartbeatRow, error) {
			var h HeartbeatRow
			var recv int64
			var av, errc sql.NullString
			var up, ac, bat, met, vpn, qd, jd, off sql.NullInt64
			if err := sc.Scan(&recv, &av, &up, &ac, &bat, &met, &vpn, &qd, &jd, &off, &h.ServiceState, &errc); err != nil {
				return HeartbeatRow{}, err
			}
			h.ReceivedAt, h.AgentVersion, h.LastErrorClass = timeOf(recv), av.String, errc.String
			h.UptimeS, h.OnAC, h.BatteryPct, h.Metered, h.VPN = ptrInt64(up), ptrBool(ac), ptrInt(bat), ptrBool(met), ptrBool(vpn)
			h.QueueDepth, h.JournalDepth, h.ClockOffsetMS = ptrInt(qd), ptrInt(jd), ptrInt64(off)
			return h, nil
		})
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		rows, truncated = rows[:limit], true
	}
	if rows == nil {
		rows = []HeartbeatRow{}
	}
	return rows, truncated, nil
}
