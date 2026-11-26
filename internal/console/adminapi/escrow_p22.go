package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Escrow verification on demand (ADR 0030). POST /api/v1/devices/{id}/escrow/verify runs one
// check now, the same one the maintenance scheduler runs weekly: the worker unwraps the device's
// escrowed key and runs `restic cat config` through the admin path, and the outcome is recorded on
// the key's escrow row (maintain.ClassifyEscrowCheck, the scheduler's own classes). It proves that
// the escrowed key opens the repository's config, not that the data is intact.
//
// Admin role, like retire, and no fresh-MFA step-up: the check reads nothing but the repository's
// config, reveals no key and changes no device; the codebase asks for fresh MFA on restores and on
// destructive owner routes (ADR 0019, ADR 0027 ruling C5), and this is neither. An API token with
// the admin role may call it.
//
// Refused, before the worker is asked: 404 for a device that does not exist or is outside the
// caller's scope; 409 custody_customer_held for a customer-held key (refused at every layer);
// 409 device_purging for a device being purged (its repository is going away); 409 conflict for a
// device with no active escrowed key or an unconfirmed enrollment (there is nothing to open yet).
// A retired device is verified like any other.
//
// Exactly one audit row (device.escrow.verify, path-free: the verdict and a fixed class) for every
// request that asks the worker. When the check ran, the row and the record commit together, so a
// verification whose audit row cannot be written is not recorded either (500). When the worker
// could not be reached, the answer is 503 and the row says so; nothing is recorded.

// PatternDeviceEscrowVerify runs one escrow verification now.
const PatternDeviceEscrowVerify = "POST /api/v1/devices/{id}/escrow/verify"

// escrowPatterns lists the patterns registerEscrow adds.
func escrowPatterns() []string { return []string{PatternDeviceEscrowVerify} }

// escrowVerifyTimeout bounds the one check: the worker's wait for a free slot plus its cat config
// timeout (55 s by default), with room. The request holds its connection open for at most this.
const escrowVerifyTimeout = 2 * time.Minute

// escrowPingTimeout bounds the worker ping that comes first.
const escrowPingTimeout = 3 * time.Second

// Verdicts of an escrow verification.
const (
	// EscrowVerified: the key opened the repository's config.
	EscrowVerified = "verified"
	// EscrowFailed: a verdict on the key or the repository (error_class wrong_password or repo_missing).
	EscrowFailed = "failed"
	// EscrowNotVerified: the check reached no verdict (storage unreachable, a timeout, a busy worker);
	// only the attempt is recorded and the key's previous proof stands, and keeps ageing.
	EscrowNotVerified = "not_verified"
)

// escrowProves is the fixed sentence every answer carries.
const escrowProves = "A verified key opens the repository's config. This does not prove that the backup data is intact."

// EscrowVerifyResponse is the answer of POST /api/v1/devices/{id}/escrow/verify.
type EscrowVerifyResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	DeviceID   string    `json:"device_id"`
	// Verdict is verified, failed or not_verified.
	Verdict string `json:"verdict"`
	// ErrorClass is the fixed class of a failed or not_verified check; absent for verified.
	ErrorClass string `json:"error_class,omitempty"`
	// Retired says the device is retired (ADR 0030: retired devices are verified too).
	Retired bool `json:"retired"`
	// VerifiedAt is the key's newest proof after this check, LastAttemptAt its newest attempt.
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	// Proves is the fixed sentence of what the verdict means.
	Proves string `json:"proves"`
}

// registerEscrow adds the ADR 0030 route.
func (s *Service) registerEscrow(r httpx.Registrar) {
	r.Handle(PatternDeviceEscrowVerify, s.Auth.Require(auth.RoleAdmin, auth.ModeAPI, http.HandlerFunc(s.escrowVerify)))
}

func (s *Service) escrowVerify(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	id := r.PathValue("id")
	t, err := s.Store.GetEscrowVerifyTarget(ctx, p.Scope, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	switch {
	case t.Custody == "customer_held":
		httpx.Problem(w, r, api.CodeCustodyCustomerHeld, 0)
		return
	case t.Purging:
		httpx.Problem(w, r, api.CodeDevicePurging, 0)
		return
	case t.Custody != "escrowed" || !t.Confirmed:
		httpx.Problem(w, r, api.CodeConflict, 0) // no active escrowed key, or no repository yet
		return
	}
	const action = "device.escrow.verify"
	method := actorOf(r, p).Method // "api" for a real API call, "web" for a console page's in-process call
	audit := func(result string, details map[string]any) store.AuditEvent {
		ev := store.AuditEvent{ActorType: "admin", ActorID: p.Admin.ID, ActorLabel: p.Admin.Username, Method: method,
			SourceIP: httpx.ClientIP(r), Action: action, ObjectType: "device", ObjectID: t.DeviceID, CustomerID: t.CustomerID, Result: result}
		if result != "ok" {
			ev.Severity = "warning"
		}
		ev.Details, _ = json.Marshal(details) // fixed words and fixed classes only: cannot fail
		return ev
	}
	unavailable := func(outcome string, code string) {
		if _, err := s.Store.AppendAudit(context.WithoutCancel(ctx), audit("error", map[string]any{"outcome": outcome})); err != nil {
			s.log().Error("admin api: action not audited", logx.Class("audit"), logx.Err(err))
			httpx.Problem(w, r, api.CodeInternal, 0)
			return
		}
		httpx.Problem(w, r, code, 30*time.Second)
	}
	if s.Backend == nil {
		unavailable("worker_unavailable", api.CodeUnavailable)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, escrowPingTimeout)
	pr, perr := s.Backend.Ping(pctx)
	cancel()
	switch {
	case perr != nil:
		unavailable("worker_unavailable", api.CodeUnavailable)
		return
	case !pr.MasterKeyLoaded:
		unavailable("master_key_unavailable", api.CodeMasterKeyUnavailable)
		return
	}

	cctx, cancel := context.WithTimeout(ctx, escrowVerifyTimeout)
	_, cerr := s.Backend.CatConfig(cctx, adminpath.DeviceRequest{DeviceID: t.DeviceID})
	cancel()
	if ctx.Err() != nil {
		// The client went away: the check's outcome is unknown here, so nothing is recorded.
		_, _ = s.Store.AppendAudit(context.WithoutCancel(ctx), audit("error", map[string]any{"outcome": "cancelled"}))
		return
	}
	v, class, skip := maintain.ClassifyEscrowCheck(cerr)
	switch {
	case skip:
		// The worker found the key customer-held after all. It was asked, so the request is audited
		// like every other one that asks it (path-free: a verdict word and a fixed outcome).
		if _, err := s.Store.AppendAudit(context.WithoutCancel(ctx), audit("error", map[string]any{"outcome": "custody_customer_held"})); err != nil {
			s.log().Error("admin api: action not audited", logx.Class("audit"), logx.Err(err))
			httpx.Problem(w, r, api.CodeInternal, 0)
			return
		}
		httpx.Problem(w, r, api.CodeCustodyCustomerHeld, 0)
		return
	case class == adminpath.CodeUnavailable:
		unavailable("worker_unavailable", api.CodeUnavailable) // the worker went away mid-check
		return
	}
	verdict, result := EscrowVerified, "ok"
	switch {
	case v.ErrorClass != "":
		verdict, result = EscrowFailed, "error"
	case !v.OK:
		verdict, result = EscrowNotVerified, "error"
	}
	details := map[string]any{"verdict": verdict}
	if class != "" {
		details["class"] = class
	}
	if err := s.Store.RecordEscrowVerificationAudited(context.WithoutCancel(ctx), p.Scope, t.DeviceID, v, audit(result, details)); err != nil {
		s.fail(w, r, err)
		return
	}
	out := EscrowVerifyResponse{ComputedAt: s.now(), DeviceID: t.DeviceID, Verdict: verdict, ErrorClass: class, Retired: t.Retired, Proves: escrowProves}
	if esc, err := s.Store.ActiveEscrow(ctx, p.Scope, t.DeviceID); err == nil {
		out.VerifiedAt, out.LastAttemptAt = utcPtr(esc.VerifiedAt), utcPtr(esc.LastVerifyAttemptAt)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
