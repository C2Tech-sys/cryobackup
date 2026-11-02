package store

import "time"

// seedFixtureCommands adds a handful of commands to the fixtures so the
// Commands tab of the device panel shows every status a fixtures console is
// meant to demonstrate (M2-W2C): a finished back-up-now, a queued one waiting
// for an unseen device, an expired one, a cancelled one and a finished cancel.
// exec is loadFixtures' error-latching exec. Times are relative to now.
func seedFixtureCommands(exec func(q string, args ...any), now time.Time, adminID string, devID map[string]string) {
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	n := 0
	add := func(host, typ, status string, createdAgo, ttl time.Duration, delivered, acked, completed time.Duration, result string) {
		id, ok := devID[host]
		if !ok {
			return
		}
		n++
		opt := func(d time.Duration) any {
			if d == 0 {
				return nil
			}
			return ms(-d)
		}
		var res any
		if result != "" {
			res = result
		}
		created := ms(-createdAgo)
		exec(`INSERT INTO commands (id, device_id, type, params, idempotency_key, status, created_by, expires_at, delivered_at, acked_at,
				completed_at, result, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			"01FXCMD"+pad19(n), id, typ, "{}", "fixture-"+typ+"-"+pad19(n), status, adminID, created+ttl.Milliseconds(),
			opt(delivered), opt(acked), opt(completed), res, created, created)
	}
	add("DR-OFFICE-PC", "backup_now", "succeeded", 30*time.Minute, 4*time.Hour, 30*time.Minute-2*time.Second, 30*time.Minute-2*time.Second, 30*time.Minute-2*time.Second, `{"started":true}`)
	add("LAB-RETINA-02", "backup_now", "queued", 5*time.Minute, 4*time.Hour, 0, 0, 0, "")
	add("FRONTDESK-01", "backup_now", "expired", 30*time.Hour, 4*time.Hour, 0, 0, 0, "")
	add("FRONTDESK-01", "refresh_policy", "failed", 52*time.Hour, 4*time.Hour, 52*time.Hour-time.Second, 52*time.Hour-time.Second, 52*time.Hour-time.Second, `{"reason":"failed","error_class":"gateway"}`)
	add("EXAM-ROOM-3", "backup_now", "cancelled", 3*time.Hour, 4*time.Hour, 0, 0, 3*time.Hour-time.Minute, "")
	add("EXAM-ROOM-3", "cancel", "succeeded", 90*time.Minute, 15*time.Minute, 90*time.Minute-time.Second, 90*time.Minute-time.Second, 90*time.Minute-time.Second, `{"cancelled":true}`)
}

// pad19 is n as 19 digits, so a fixture command id is 26 characters.
func pad19(n int) string {
	s := make([]byte, 19)
	for i := 18; i >= 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}
