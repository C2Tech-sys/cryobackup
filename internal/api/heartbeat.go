package api

import (
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Progress is the live count of a running job (architecture §5.1 /progress,
// §6.2). It is the body of POST /progress (cap MaxProgressBody, at most one per
// ProgressMinInterval) and the optional Progress field of a heartbeat. It has
// no path, no file name and no message: counts, a fraction and flags.
type Progress struct {
	RunID          string      `json:"run_id"` // 26-character ULID of the run (a restore is a run too)
	Kind           restic.Kind `json:"kind"`
	PercentDone    float64     `json:"percent_done"` // restic's fraction, 0 to 1 (not 0 to 100)
	BytesDone      uint64      `json:"bytes_done"`
	TotalBytes     uint64      `json:"total_bytes"` // 0 until restic has finished scanning
	FilesDone      uint64      `json:"files_done"`
	TotalFiles     uint64      `json:"total_files"` // 0 until restic has finished scanning
	ErrorCount     uint64      `json:"error_count"`
	SecondsElapsed uint64      `json:"seconds_elapsed"`
	Stalled        bool        `json:"stalled"` // no byte moved for the stall window
	UpdatedAt      time.Time   `json:"updated_at"`
}

// Validate checks the progress record.
func (p Progress) Validate() error {
	var c collector
	c.ulid("run_id", p.RunID)
	if !p.Kind.Recorded() {
		c.add("kind", msgUnknown)
	}
	if !(p.PercentDone >= 0 && p.PercentDone <= 1) {
		c.add("percent_done", "must be a fraction between 0 and 1")
	}
	if p.UpdatedAt.IsZero() {
		c.add("updated_at", msgRequired)
	}
	return c.err()
}

// HeartbeatRequest is the body of POST /heartbeat (architecture §5.1, DH2),
// sent every 60 s plus or minus jitter and no faster than HeartbeatMinInterval.
// The first heartbeat after enrollment sets enroll_confirmed_at. Pointers are
// "not known": a desktop has no battery, a failed probe has no answer.
//
// DeviceTime lets the console compute the clock offset (DH5). LastErrorClass is
// the class of the last failed run or gateway contact and DeferredReason says
// why a deferred service is deferred (architecture §6.2); both are path-free
// vocabulary. GatewaySecretAcked is the implicit acknowledgement of a pending
// gateway password (DH13). HWFingerprint lets the console notice a disk clone
// or a swapped machine (identity alarm); it may be empty only if the agent
// could not compute it.
//
// ActiveRunID names the backup the agent has active (it is set exactly while
// a backup run is in progress): the console extends that run's start lease by
// StartLeaseTTL on every heartbeat (ADR 0005). An agent without an active
// backup leaves it empty; an older agent never sends it, and the console then
// falls back to Progress.RunID.
type HeartbeatRequest struct {
	AgentVersion         string                `json:"agent_version"`
	ResticVersion        string                `json:"restic_version"`
	UptimeS              int64                 `json:"uptime_s"`
	OnAC                 *bool                 `json:"on_ac,omitempty"`
	BatteryPct           *int                  `json:"battery_pct,omitempty"` // 0 to 100
	Metered              *bool                 `json:"metered,omitempty"`
	VPN                  *bool                 `json:"vpn,omitempty"`
	QueueDepth           int                   `json:"queue_depth"`   // undelivered results in the outbox
	JournalDepth         int                   `json:"journal_depth"` // open journal rows
	DeviceTime           time.Time             `json:"device_time"`
	ServiceState         ServiceState          `json:"service_state"`
	LastErrorClass       restic.ErrorClass     `json:"last_error_class,omitempty"`
	DeferredReason       restic.DeferredReason `json:"deferred_reason,omitempty"`
	Progress             *Progress             `json:"progress,omitempty"`
	PolicyETag           string                `json:"policy_etag"`
	PolicyVersionApplied int                   `json:"policy_version_applied"`
	HWFingerprint        string                `json:"hw_fingerprint"`
	GatewaySecretAcked   bool                  `json:"gateway_secret_acked"`
	ActiveRunID          string                `json:"active_run_id,omitempty"`
}

// Validate checks the heartbeat. A deferred service must say why, and only a
// deferred service has a reason.
func (r HeartbeatRequest) Validate() error {
	var c collector
	c.text("agent_version", r.AgentVersion, maxText, true)
	c.text("restic_version", r.ResticVersion, maxText, false)
	c.notNegative("uptime_s", r.UptimeS)
	if r.BatteryPct != nil && (*r.BatteryPct < 0 || *r.BatteryPct > 100) {
		c.add("battery_pct", "must be between 0 and 100")
	}
	c.notNegative("queue_depth", int64(r.QueueDepth))
	c.notNegative("journal_depth", int64(r.JournalDepth))
	if r.DeviceTime.IsZero() {
		c.add("device_time", msgRequired)
	}
	if !has(states, r.ServiceState) {
		c.add("service_state", msgUnknown)
	}
	if r.LastErrorClass != "" && !r.LastErrorClass.Valid() {
		c.add("last_error_class", msgUnknown)
	}
	switch {
	case r.DeferredReason != "" && !has(deferredReasons, r.DeferredReason):
		c.add("deferred_reason", msgUnknown)
	case r.ServiceState == ServiceDeferred && r.DeferredReason == "":
		c.add("deferred_reason", "is required when service_state is deferred")
	case r.ServiceState != ServiceDeferred && r.DeferredReason != "":
		c.add("deferred_reason", "is only allowed when service_state is deferred")
	}
	if r.Progress != nil {
		c.sub("progress", r.Progress.Validate())
	}
	c.text("policy_etag", r.PolicyETag, maxText, false)
	c.notNegative("policy_version_applied", int64(r.PolicyVersionApplied))
	if r.HWFingerprint != "" {
		c.hexRange("hw_fingerprint", r.HWFingerprint, 16, 128)
	}
	if r.ActiveRunID != "" {
		c.ulid("active_run_id", r.ActiveRunID)
	}
	return c.err()
}

// HeartbeatResponse is the 200 body of POST /heartbeat (architecture §5.1).
// ClockOffsetMS is the device clock minus the server clock (ClockOffset).
// CommandsPending hints that GET /commands has something; PolicyETag lets the
// agent pull the policy only when it changed; ReleaseHint is the version the
// device's ring should run (empty when none).
type HeartbeatResponse struct {
	ServerTime      time.Time `json:"server_time"`
	ClockOffsetMS   int64     `json:"clock_offset_ms"`
	CommandsPending bool      `json:"commands_pending"`
	PolicyETag      string    `json:"policy_etag"`
	ReleaseHint     string    `json:"release_hint,omitempty"`
}
