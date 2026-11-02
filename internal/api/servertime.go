package api

import (
	"errors"
	"time"
)

// serverTimeLayout is RFC 3339 with exactly three fractional digits, in UTC.
const serverTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// FormatServerTime renders t as the value of HeaderServerTime: RFC 3339 in UTC
// with millisecond precision ("2026-10-22T14:03:09.512Z"). Sub-millisecond
// precision is truncated.
func FormatServerTime(t time.Time) string {
	return t.UTC().Format(serverTimeLayout)
}

// ParseServerTime parses the value of HeaderServerTime (or any RFC 3339 time
// with a zone) and returns it in UTC. An empty or malformed value is an error;
// the agent treats a missing header as "no offset information", never as zero.
func ParseServerTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("api: empty server time")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errors.New("api: malformed server time")
	}
	return t.UTC(), nil
}

// ClockOffset returns the device clock minus the server clock in milliseconds
// (architecture §1.3 DH5, §6.1 uplink): positive means the device runs ahead.
// The agent computes it from HeaderServerTime when a response arrives; the
// console computes it from HeartbeatRequest.DeviceTime when the request
// arrives and subtracts it from device-reported times (clamped to receipt
// time) so that a wrong device clock cannot make a backup look fresh.
func ClockOffset(deviceTime, serverTime time.Time) int64 {
	return deviceTime.Sub(serverTime).Milliseconds()
}
