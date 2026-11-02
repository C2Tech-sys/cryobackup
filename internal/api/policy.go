package api

import (
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// GatewaySecret is a gateway password the console generated for a rotation and
// keeps (wrapped) until the agent acknowledges it (architecture DH13). The
// agent acknowledges it implicitly: the next heartbeat carries
// HeartbeatRequest.GatewaySecretAcked = true once it has stored the password.
// Until then every policy pull delivers it again.
type GatewaySecret struct {
	Password Secret    `json:"password"`
	IssuedAt time.Time `json:"issued_at"`
}

// PolicyResponse is the 200 body of GET /policy (architecture §5.1, DH6, DH13);
// a matching If-None-Match gets 304 and no body. Policy is the effective
// policy (default, customer, device override merged); Version is its version
// and ETag its entity tag (policy.ETag), which is also the ETag header.
//
// Besides the policy it carries what rides the pull: a pending gateway-password
// rotation, the maintenance hold, the pause state and the release the device
// should move to. ReleaseTarget is empty when none.
//
// PolicyID is the 26-character ULID of the customer-or-default policy document
// whose Version is reported here; a device override is merged into Policy but
// does not change PolicyID or Version. The agent tags every backup snapshot
// cb.policy=<policy_id>.<version> (architecture 6.3) from the two. It is an
// identifier, not a secret.
type PolicyResponse struct {
	Policy               policy.Policy  `json:"policy"`
	PolicyID             string         `json:"policy_id"`
	Version              int            `json:"version"`
	ETag                 string         `json:"etag"`
	PendingGatewaySecret *GatewaySecret `json:"pending_gateway_secret,omitempty"`
	HoldUntil            *time.Time     `json:"hold_until,omitempty"`
	Paused               bool           `json:"paused"`
	PausedUntil          *time.Time     `json:"paused_until,omitempty"`
	ReleaseTarget        string         `json:"release_target,omitempty"`
	ServerTime           time.Time      `json:"server_time"`
}

// Redacted returns a copy of r with the pending gateway password blanked. The
// pointer is replaced by a pointer to a copy, so the original is not touched.
func (r PolicyResponse) Redacted() PolicyResponse {
	if r.PendingGatewaySecret != nil {
		g := *r.PendingGatewaySecret
		g.Password = ""
		r.PendingGatewaySecret = &g
	}
	return r
}
