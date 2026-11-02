package api

import "time"

// Fleet concurrency cap by console start-lease (ADR 0005, feature row r30).
//
// Before a scheduled backup (triggers schedule, catchup, boot, wake, retry) the
// agent asks POST /start-lease with the id of the run it is about to start. The
// console grants while fewer than the storage target's max_concurrent_backups
// leases are live, and otherwise answers "not granted" with a retry_after_s.
// Both answers are 200: a refusal is a scheduling fact, not an error.
//
//	{"granted":true,"lease_until":"2026-10-22T14:18:09.512Z","server_time":...}  a lease is held (counted against the cap)
//	{"granted":true,"server_time":...}                                          no cap applies (max_concurrent_backups = 0): nothing is held
//	{"granted":false,"retry_after_s":420,"server_time":...}                     the cap is full; ask again after retry_after_s
//
// The agent never needs to know the cap itself. A lease lives StartLeaseTTL
// and is renewed by every heartbeat that names the run (HeartbeatRequest.
// ActiveRunID); the upload of the closed run releases it. A retry of the same
// request (same device, same run id) returns the same lease.
//
// Fail open: an agent that cannot reach the console, times out
// (StartLeaseTimeout), or gets 5xx or 429 starts anyway and records
// LeaseBypassed on the run (RunStats.Lease). A backup_now (trigger manual)
// does not ask and records LeaseNotRequired.

// StartLeaseRequest is the body of POST /start-lease (cap MaxStartLeaseBody).
type StartLeaseRequest struct {
	RunID string `json:"run_id"` // 26-character ULID of the run the agent is about to start
}

// Validate checks the request.
func (r StartLeaseRequest) Validate() error {
	var c collector
	c.ulid("run_id", r.RunID)
	return c.err()
}

// StartLeaseResponse is the 200 body of POST /start-lease. Granted with
// LeaseUntil set: a lease is held until then. Granted without LeaseUntil: the
// device's storage target has no cap and nothing is held. Not granted:
// RetryAfterS (StartLeaseRetryMin to StartLeaseRetryMax seconds, never random:
// the agent adds its own jitter) is when the earliest live lease expires.
type StartLeaseResponse struct {
	Granted     bool       `json:"granted"`
	LeaseUntil  *time.Time `json:"lease_until,omitempty"`
	RetryAfterS int        `json:"retry_after_s,omitempty"`
	ServerTime  time.Time  `json:"server_time"`
}

// Validate checks the couplings: a grant carries no retry_after_s, a refusal
// carries a retry_after_s in range and no lease_until.
func (r StartLeaseResponse) Validate() error {
	var c collector
	if r.Granted {
		if r.RetryAfterS != 0 {
			c.add("retry_after_s", "is only allowed when granted is false")
		}
		if r.LeaseUntil != nil && r.LeaseUntil.IsZero() {
			c.add("lease_until", msgRequired)
		}
	} else {
		if r.LeaseUntil != nil {
			c.add("lease_until", "is only allowed when granted is true")
		}
		min, max := int(StartLeaseRetryMin/time.Second), int(StartLeaseRetryMax/time.Second)
		if r.RetryAfterS < min || r.RetryAfterS > max {
			c.add("retry_after_s", "must be between 60 and 900 when granted is false")
		}
	}
	return c.err()
}

// LeaseOutcome is the per-run fact of how the start lease went
// (RunStats.Lease, kept in runs.stats). It is empty on runs that never asked:
// non-backup kinds, agents older than ADR 0005, and a run deferred because the
// lease was refused (that run carries deferred_reason fleet_cap instead).
type LeaseOutcome string

// Lease outcomes.
const (
	// LeaseGranted: the console granted a lease that counts against the cap.
	LeaseGranted LeaseOutcome = "granted"
	// LeaseBypassed: the console was unreachable, timed out, or answered 5xx
	// or 429, and the agent started anyway (fail open, ADR 0005).
	LeaseBypassed LeaseOutcome = "bypassed"
	// LeaseNotRequired: an operator backup_now (no lease is asked), or the
	// storage target has no cap (max_concurrent_backups = 0).
	LeaseNotRequired LeaseOutcome = "not_required"
)

// Valid reports whether o is one of the three outcomes.
func (o LeaseOutcome) Valid() bool { return has(leaseOutcomes, o) }
