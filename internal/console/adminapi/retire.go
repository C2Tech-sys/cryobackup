package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
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
//
// The answer tells the truth about the gateway. Revoking the login in the database
// does not take it out of the file the gateway reads: the htpasswd-sync job does, and
// it can fail. So a retire waits a few seconds for that job and answers gateway_login
// removed or pending. Banner C17 stays up from the same judgement
// (state.JudgeLoginSync) while it is pending, and clears when a sync passes.

// PatternDeviceRetire retires a device.
const PatternDeviceRetire = "POST /api/v1/devices/{id}/retire"

// DefaultGatewaySyncWait bounds a retire's wait for the login sync. A healthy sync
// takes well under a second; a failing one answers as soon as it has failed.
const DefaultGatewaySyncWait = 5 * time.Second

// Values of DeviceRetireResponse.GatewayLogin.
const (
	GatewayLoginRemoved = "removed"
	GatewayLoginPending = "pending"
	// gatewaySyncNotWired is the reason when this console has no sync to signal.
	gatewaySyncNotWired = "sync_not_wired"
)

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
	// GatewayLogin says whether the login has left the file the gateway reads: removed, or pending
	// (the gateway may still accept it until the sync succeeds). Absent while not retired.
	GatewayLogin string `json:"gateway_login,omitempty"`
	// GatewayLoginReason is the reason class of a pending login.
	GatewayLoginReason string `json:"gateway_login_reason,omitempty"`
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

// gatewayLogin words a verdict for the answer and the audit row: anything not known
// to be removed is pending, with its reason.
func gatewayLogin(v state.LoginSyncVerdict) (status, reason string) {
	if v.Status == state.LoginRemoved {
		return GatewayLoginRemoved, ""
	}
	return GatewayLoginPending, v.Class
}

// awaitLoginSync signals the sync and waits, within a bound, for a run that started
// after the retire. It returns as soon as that run has an outcome. A signal can be
// lost to a run holding the job's lease, so it is repeated while waiting.
func (s *Service) awaitLoginSync(ctx context.Context, retiredAt time.Time) state.LoginSyncVerdict {
	if s.SyncGateway == nil {
		return state.LoginSyncVerdict{Status: state.LoginPending, Class: gatewaySyncNotWired}
	}
	wait := s.GatewaySyncWait
	if wait <= 0 {
		wait = DefaultGatewaySyncWait
	}
	s.SyncGateway()
	deadline, poll, again := time.NewTimer(wait), time.NewTicker(25*time.Millisecond), time.NewTicker(time.Second)
	defer deadline.Stop()
	defer poll.Stop()
	defer again.Stop()
	for {
		v := s.Fleet.LoginSync(ctx, retiredAt)
		if v.Status == state.LoginRemoved || (v.Status == state.LoginPending && v.Class != state.LoginNoSyncSinceRetire) {
			return v
		}
		select {
		case <-deadline.C:
			return v
		case <-ctx.Done():
			return v
		case <-again.C:
			s.SyncGateway()
		case <-poll.C:
		}
	}
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
	var verdict state.LoginSyncVerdict
	if res.Changed {
		s.committed(r)
		verdict = s.awaitLoginSync(r.Context(), *res.Device.RetiredAt)
		status, reason := gatewayLogin(verdict)
		// The retire stands whatever this row does; the answer below is still the truth.
		if aerr := s.Store.RecordRetireSyncAudited(context.WithoutCancel(r.Context()), p.Scope, id, status, reason, actorOf(r, p)); aerr != nil {
			s.log().Error("admin api: the retire's sync outcome was not audited", logx.Class("audit"), logx.Err(aerr))
		}
		s.deviceChanged(id) // an open command poll returns, the agent's next call answers 401, and pages see the outcome
	} else {
		verdict = s.Fleet.LoginSync(r.Context(), *res.Device.RetiredAt)
		if verdict.Status != state.LoginRemoved && s.SyncGateway != nil {
			s.SyncGateway() // the repeat is a good moment to try again; the banner stays until it works
		}
	}
	out := s.retireResponse(res)
	out.GatewayLogin, out.GatewayLoginReason = gatewayLogin(verdict)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// retireFinalBackup queues the last backup and retires nothing. On a device that
// is already retired it answers the retired state, like a repeated retire. The
// reason goes in the command's audit row, and a replacement is held to the rule the
// retire will apply, so a bad one is refused now.
func (s *Service) retireFinalBackup(w http.ResponseWriter, r *http.Request, p *auth.Principal, id string, req DeviceRetireRequest) {
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if dev.RetiredAt != nil {
		out := s.retireResponse(store.RetireResult{Device: dev})
		out.GatewayLogin, out.GatewayLoginReason = gatewayLogin(s.Fleet.LoginSync(r.Context(), *dev.RetiredAt))
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	reason, bad := store.ValidateRetireReason(req.Reason)
	if len(bad) > 0 {
		s.failM2(w, r, &store.ValidationError{Fields: bad})
		return
	}
	if err := s.Store.CheckRetireReplacement(r.Context(), p.Scope, id, req.ReplacedBy); err != nil {
		s.failM2(w, r, err)
		return
	}
	q, err := s.Store.QueueCommandAudited(r.Context(), p.Scope, id, store.CommandInput{Type: store.CommandBackupNow, IdempotencyKey: newKey("retire"),
		AuditNote: "final backup before retire: " + reason}, actorOf(r, p))
	if err != nil {
		s.failCommand(w, r, err)
		return
	}
	if q.Created {
		s.committed(r)
		s.deviceChanged(id)
	}
	out := s.retireResponse(store.RetireResult{Device: dev})
	out.Reason, out.ReplacedBy, out.FinalBackupCommandID = reason, req.ReplacedBy, q.Command.ID
	httpx.WriteJSON(w, http.StatusAccepted, out)
}
