package adminapi

import (
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// POST /api/v1/devices/{id}/retire (architecture §5.2, F09 F14 F20; ADR 0016).
// The one handler behind every retire control: the API, and the UI through its
// in-process call. It needs the admin role.
//
// final_backup asks for a last backup first, and that is a separate step: the
// device must still hold its credentials to run a backup, and a retire is what
// takes them away. So final_backup queues a backup_now and answers 202 with the
// device not retired; the admin retires it once that run has finished, by calling
// this route again without final_backup (ADR 0016 says why this is not a deferred
// retire).

// PatternDeviceRetire retires a device.
const PatternDeviceRetire = "POST /api/v1/devices/{id}/retire"

// DeviceRetireRequest is the body of POST /api/v1/devices/{id}/retire.
type DeviceRetireRequest struct {
	// Reason is required: one line, no file paths. It is kept on the device and in the audit row.
	Reason string `json:"reason"`
	// FinalBackup queues a last backup instead of retiring: see the note above.
	FinalBackup bool `json:"final_backup,omitempty"`
	// ReplacedBy names the device that takes over: active, and of the same customer.
	ReplacedBy string `json:"replaced_by,omitempty"`
}

// DeviceRetireResponse says what state the device is in after the call.
type DeviceRetireResponse struct {
	ComputedAt time.Time  `json:"computed_at"`
	ID         string     `json:"id"`
	CustomerID string     `json:"customer_id"`
	Hostname   string     `json:"hostname"`
	Retired    bool       `json:"retired"`
	State      string     `json:"state,omitempty"` // RETIRED once retired; absent while a final backup is pending
	RetiredAt  *time.Time `json:"retired_at,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	ReplacedBy string     `json:"replaced_by,omitempty"`
	// Changed is false when the device was already retired: nothing was written and no audit row exists for this call.
	Changed           bool  `json:"changed"`
	TokensRevoked     int64 `json:"tokens_revoked"`
	LoginsRevoked     int64 `json:"logins_revoked"`
	CommandsCancelled int64 `json:"commands_cancelled"`
	// FinalBackupCommandID is the backup_now that final_backup queued (or found already in flight).
	FinalBackupCommandID string `json:"final_backup_command_id,omitempty"`
}

func (s *Service) retireResponse(res store.RetireResult) DeviceRetireResponse {
	d := res.Device
	out := DeviceRetireResponse{ComputedAt: s.now(), ID: d.ID, CustomerID: d.CustomerID, Hostname: d.Hostname, Retired: d.RetiredAt != nil, RetiredAt: utcPtr(d.RetiredAt),
		Reason: d.RetiredReason, ReplacedBy: d.ReplacedByDeviceID, Changed: res.Changed, TokensRevoked: res.TokensRevoked, LoginsRevoked: res.LoginsRevoked, CommandsCancelled: res.CommandsCancelled}
	if out.Retired {
		out.State = string(state.StateRetired)
	}
	return out
}

func (s *Service) deviceRetire(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req DeviceRetireRequest
	if !decodeBodyMax(w, r, 8<<10, &req) {
		return
	}
	id := r.PathValue("id")
	if req.FinalBackup {
		s.retireFinalBackup(w, r, p, id, req)
		return
	}
	res, err := s.Store.RetireDeviceAudited(r.Context(), p.Scope, id, store.RetireRequest{Reason: req.Reason, ReplacedBy: req.ReplacedBy}, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if res.Changed {
		s.committed(r)
		if s.SyncGateway != nil { // the gateway login must stop working now, not at the next five-minute sync
			s.SyncGateway()
		}
		s.deviceChanged(id) // an open command poll returns, and the agent's next call answers 401
	}
	httpx.WriteJSON(w, http.StatusOK, s.retireResponse(res))
}

// retireFinalBackup queues the last backup and retires nothing. On a device that
// is already retired it answers the retired state, like a repeated retire.
func (s *Service) retireFinalBackup(w http.ResponseWriter, r *http.Request, p *auth.Principal, id string, req DeviceRetireRequest) {
	if _, bad := store.ValidateRetireReason(req.Reason); len(bad) > 0 {
		s.failM2(w, r, &store.ValidationError{Fields: bad})
		return
	}
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if dev.RetiredAt != nil {
		httpx.WriteJSON(w, http.StatusOK, s.retireResponse(store.RetireResult{Device: dev}))
		return
	}
	q, err := s.Store.QueueCommandAudited(r.Context(), p.Scope, id, store.CommandInput{Type: store.CommandBackupNow, IdempotencyKey: newKey("retire")}, actorOf(r, p))
	if err != nil {
		s.failCommand(w, r, err)
		return
	}
	if q.Created {
		s.committed(r)
		s.deviceChanged(id)
	}
	out := s.retireResponse(store.RetireResult{Device: dev})
	out.FinalBackupCommandID = q.Command.ID
	httpx.WriteJSON(w, http.StatusAccepted, out)
}
