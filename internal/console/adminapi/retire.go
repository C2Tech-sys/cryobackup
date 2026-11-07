package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
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
// it can fail. So a retire signals that job and reads the file, repeatedly, for a few
// seconds. It answers gateway_login removed only when a read after its commit found the
// login gone, and pending otherwise. Banner C17 comes from the same read.
//
// How long the answer can take after the commit: GatewaySyncWait, at most 5 s, for the
// reads, plus retireAuditWait, at most 2 s, for the outcome's audit row: 7 s on a working
// disk. A COMMIT cannot be cancelled, so a disk that hangs inside the audit row's commit holds
// the answer past that, for as long as it hangs. Before the commit a retire queues for the
// store's one writer like every other write, so that part is bounded only by the request.

// PatternDeviceRetire retires a device.
const PatternDeviceRetire = "POST /api/v1/devices/{id}/retire"

// DefaultGatewaySyncWait bounds a retire's wait for the login to leave the file. A
// healthy sync takes well under a second; a failing one answers as soon as it has failed.
const DefaultGatewaySyncWait = 5 * time.Second

// retireAuditWait bounds the write of the outcome's audit row, for a store whose
// writer has stalled: the retire has committed, and the answer must not hang on it.
const retireAuditWait = 2 * time.Second

// Values of DeviceRetireResponse.GatewayLogin.
const (
	GatewayLoginRemoved = "removed"
	GatewayLoginPending = "pending"
	// gatewayCheckNotWired is the reason when this console cannot read the file.
	gatewayCheckNotWired = "check_not_wired"
	// gatewayStillInFile is the reason when the login is in the file and the sync job has not reported a failure.
	gatewayStillInFile = "still_in_file"
	// gatewayCheckTimedOut is the reason when the read did not finish in its time.
	gatewayCheckTimedOut = "check_timed_out"
	// gatewayCheckMargin is how much of a caller's bound is kept back from the read.
	gatewayCheckMargin = 50 * time.Millisecond
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

// GatewayLoginState is one reading of a retired device's login in the file the gateway reads.
type GatewayLoginState struct {
	// Status is GatewayLoginRemoved or GatewayLoginPending.
	Status string
	// Reason is the class of a pending login.
	Reason string
	// InFile: a clean read found the login. False with a pending status means the file was not read.
	InFile bool
}

// GatewayLoginNow reads the file once for a device retired at retiredAt. Only a clean read that
// began after the retire and does not find the login says removed. A sync in flight holds the
// slot, which is the normal state right after a fault is repaired: the read that met it answers
// from the newest clean read, so a page does not call a removed login pending because a run is
// busy. The job's result only explains a login that is in the file.
func (s *Service) GatewayLoginNow(ctx context.Context, deviceID string, retiredAt time.Time) GatewayLoginState {
	if s.CheckGatewayLogins == nil {
		return GatewayLoginState{Status: GatewayLoginPending, Reason: gatewayCheckNotWired}
	}
	// In its own goroutine: a read of a hung mount cannot be cancelled, and the caller's bound must still hold.
	// The read gets a little less than that bound, so one that only waited for its turn can still answer with
	// what the newest clean read found.
	readCtx, cancel := ctx, context.CancelFunc(func() {})
	if dl, ok := ctx.Deadline(); ok {
		readCtx, cancel = context.WithDeadline(ctx, dl.Add(-gatewayCheckMargin))
	}
	defer cancel()
	done := make(chan jobs.LoginFileCheck, 1)
	go func() { done <- s.CheckGatewayLogins(readCtx) }()
	var c jobs.LoginFileCheck
	select {
	case c = <-done:
	case <-ctx.Done():
		return GatewayLoginState{Status: GatewayLoginPending, Reason: gatewayCheckTimedOut}
	}
	switch {
	case c.Has(deviceID):
		return GatewayLoginState{Status: GatewayLoginPending, Reason: s.inFileReason(ctx, c), InFile: true}
	case c.ReadClass == jobs.LoginFileBusy && c.ReadSince(retiredAt):
		return GatewayLoginState{Status: GatewayLoginRemoved}
	case c.ReadClass != "":
		return GatewayLoginState{Status: GatewayLoginPending, Reason: c.ReadClass}
	}
	return GatewayLoginState{Status: GatewayLoginRemoved}
}

// inFileReason names why a login is still in the file: the file cannot be read now, or the job's
// newest run failed, or neither is known.
func (s *Service) inFileReason(ctx context.Context, c jobs.LoginFileCheck) string {
	if c.ReadClass != "" && c.ReadClass != jobs.LoginFileBusy {
		return c.ReadClass
	}
	if j, err := s.Store.GetJobState(ctx, store.JobHtpasswdSync); err == nil && j.LastStatus == "error" && j.LastErrorClass != "" {
		return j.LastErrorClass
	}
	return gatewayStillInFile
}

// syncFailedSince says whether the job's newest run began at or after t and failed, and its class.
// It can only end the wait early and name the reason: a pending answer may be pessimistic, a removed
// one may not.
func (s *Service) syncFailedSince(ctx context.Context, t time.Time) (string, bool) {
	j, err := s.Store.GetJobState(ctx, store.JobHtpasswdSync)
	if err != nil || j.LastStatus != "error" || j.LastStartedAt == nil || j.LastStartedAt.Before(t) {
		return "", false
	}
	return j.LastErrorClass, true
}

// awaitLoginRemoved signals the sync and reads the file until the login is gone, the file cannot
// be read, a run after the retire has failed, or the bound ends. A signal can be lost to a run
// holding the job's lease, so it is repeated while waiting.
func (s *Service) awaitLoginRemoved(ctx context.Context, deviceID string, retiredAt time.Time) GatewayLoginState {
	if s.SyncGateway == nil || s.CheckGatewayLogins == nil {
		return s.GatewayLoginNow(ctx, deviceID, retiredAt)
	}
	wait := s.GatewaySyncWait
	if wait <= 0 {
		wait = DefaultGatewaySyncWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	s.SyncGateway()
	poll, again := time.NewTicker(25*time.Millisecond), time.NewTicker(time.Second)
	defer poll.Stop()
	defer again.Stop()
	var busy *GatewayLoginState // a read that met a run; it says more than the bound running out afterwards
	for {
		v := s.GatewayLoginNow(ctx, deviceID, retiredAt)
		if v.Reason == jobs.LoginFileBusy {
			b := v
			busy = &b
		} else if v.Reason == gatewayCheckTimedOut && busy != nil {
			v = *busy // the page reads sync_busy here too: the answer and the page name one cause
		}
		switch {
		case v.Status == GatewayLoginRemoved:
			return v
		case !v.InFile && v.Reason != jobs.LoginFileBusy && v.Reason != gatewayCheckTimedOut: // the file cannot be read
			return v
		}
		if v.InFile {
			// One read of the job's row decides and names the reason: the run can fail after the read above.
			if class, failed := s.syncFailedSince(ctx, retiredAt); failed {
				if class != "" {
					v.Reason = class
				}
				return v
			}
		}
		select {
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
	var login GatewayLoginState
	if res.Changed {
		s.committed(r)
		s.deviceChanged(id) // at the commit: an open command poll returns, the agent's next call answers 401, and pages see RETIRED
		login = s.awaitLoginRemoved(r.Context(), id, *res.Device.RetiredAt)
		// The retire stands whatever this row does; the answer below is still the truth. A client that has gone
		// away does not take the row with it, and a stalled writer cannot hold the answer.
		actx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), retireAuditWait)
		defer cancel()
		if aerr := s.Store.RecordRetireSyncAudited(actx, p.Scope, id, login.Status, login.Reason, actorOf(r, p)); aerr != nil {
			s.log().Error("admin api: the retire's sync outcome was not audited", logx.Class("audit"), logx.Err(aerr))
		}
		s.deviceChanged(id) // and again with the outcome: the banner may have moved
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), DefaultGatewaySyncWait)
		defer cancel()
		login = s.GatewayLoginNow(ctx, id, *res.Device.RetiredAt)
		if login.Status != GatewayLoginRemoved && s.SyncGateway != nil {
			s.SyncGateway() // the repeat is a good moment to try again; the banner stays until it works
		}
	}
	out := s.retireResponse(res)
	out.GatewayLogin, out.GatewayLoginReason = login.Status, login.Reason
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
		ctx, cancel := context.WithTimeout(r.Context(), DefaultGatewaySyncWait)
		defer cancel()
		login := s.GatewayLoginNow(ctx, id, *dev.RetiredAt)
		out.GatewayLogin, out.GatewayLoginReason = login.Status, login.Reason
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
