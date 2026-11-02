package adminapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Commands and identity conflicts (M2-W2C, architecture §5.2 "Commands" and
// POST /devices/{id}/identity/resolve, POST /runs/{id}/cancel). Reads need
// read_only, queueing and cancelling operator, resolving an identity conflict
// admin. Every mutation is audited by the store in the same transaction; a
// command or a device outside the caller's scope is 404, never 403.

// Route patterns (exported for the route-coverage tests and the UI).
const (
	PatternDeviceCommandCreate  = "POST /api/v1/devices/{id}/commands"
	PatternDeviceCommands       = "GET /api/v1/devices/{id}/commands"
	PatternDeviceCommandCancel  = "DELETE /api/v1/devices/{id}/commands/{cid}"
	PatternRunCancel            = "POST /api/v1/runs/{id}/cancel"
	PatternDeviceIdentityResolv = "POST /api/v1/devices/{id}/identity/resolve"
)

// commandPatterns lists the patterns registerCommands adds.
func commandPatterns() []string {
	return []string{PatternDeviceCommandCreate, PatternDeviceCommands, PatternDeviceCommandCancel, PatternRunCancel, PatternDeviceIdentityResolv}
}

func (s *Service) registerCommands(r httpx.Registrar) {
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeAPI, h) }
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeAPI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeAPI, h) }
	r.Handle(PatternDeviceCommandCreate, op(s.queueCommand))
	r.Handle(PatternDeviceCommands, ro(s.listCommands))
	r.Handle(PatternDeviceCommandCancel, op(s.cancelCommand))
	r.Handle(PatternRunCancel, op(s.cancelRun))
	r.Handle(PatternDeviceIdentityResolv, ad(s.resolveIdentity))
}

// ---- shapes ----

// CommandJSON is one command. Status is the effective status: a queued or
// delivered command past its expiry is "expired" even before the command-expiry
// job (every 60 s) records it.
type CommandJSON struct {
	ID          string          `json:"id"`
	DeviceID    string          `json:"device_id"`
	Type        string          `json:"type"`
	Params      json.RawMessage `json:"params"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
	AckedAt     *time.Time      `json:"acked_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// QueueCommandRequest is the body of POST /api/v1/devices/{id}/commands. The
// idempotency key may instead come in the Idempotency-Key header. TTLSeconds 0
// means the default lifetime of the type.
type QueueCommandRequest struct {
	Type           string          `json:"type"`
	Params         json.RawMessage `json:"params,omitempty"`
	IdempotencyKey string          `json:"idempotency_key"`
	TTLSeconds     int             `json:"ttl_seconds,omitempty"`
}

// CommandResponse answers a queue or run-cancel request. Coalesced is true when
// an equal command was already in flight and is returned instead of a new one.
type CommandResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	CommandJSON
	Coalesced bool `json:"coalesced,omitempty"`
}

// CommandListResponse is GET /api/v1/devices/{id}/commands.
type CommandListResponse struct {
	ComputedAt time.Time     `json:"computed_at"`
	Total      int           `json:"total"`
	Truncated  bool          `json:"truncated"`
	NextCursor string        `json:"next_cursor,omitempty"`
	Commands   []CommandJSON `json:"commands"`
}

// IdentityResolveRequest is the body of POST /api/v1/devices/{id}/identity/resolve.
// Action is keep or retire; Reason is required and audited.
type IdentityResolveRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// IdentityResolveResponse is the answer: the device after the resolution.
type IdentityResolveResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	ID         string    `json:"id"`
	CustomerID string    `json:"customer_id"`
	Hostname   string    `json:"hostname"`
	Resolution string    `json:"resolution"`
	Retired    bool      `json:"retired"`
}

// commandConflictProblem is a 409 with a stable machine reason beside the fixed
// sentence, so a script can tell "parked" from "delivered" without parsing text.
type commandConflictProblem struct {
	*api.Error
	Reason string `json:"reason"`
}

func commandJSON(c store.Command, now time.Time) CommandJSON {
	params := c.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	return CommandJSON{ID: c.ID, DeviceID: c.DeviceID, Type: c.Type, Params: params, Status: c.EffectiveStatus(now), CreatedAt: c.CreatedAt.UTC(),
		ExpiresAt: c.ExpiresAt.UTC(), DeliveredAt: utcPtr(c.DeliveredAt), AckedAt: utcPtr(c.AckedAt), CompletedAt: utcPtr(c.CompletedAt), Result: c.Result}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// conflictReason names why the store refused a request with ErrConflict ("" when
// the conflict has no more specific reason).
func conflictReason(err error) string {
	switch {
	case errors.Is(err, store.ErrDeviceRetired):
		return "device_retired"
	case errors.Is(err, store.ErrDeviceParked):
		return "device_parked"
	case errors.Is(err, store.ErrCommandDelivered):
		return "command_delivered"
	case errors.Is(err, store.ErrCommandExpired):
		return "command_expired"
	case errors.Is(err, store.ErrNoIdentityConflict):
		return "no_identity_conflict"
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	}
	return ""
}

// failCommand is failM2 plus the reason on a 409.
func (s *Service) failCommand(w http.ResponseWriter, r *http.Request, err error) {
	if reason := conflictReason(err); reason != "" && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalid) {
		e := api.NewError(api.CodeConflict)
		e.RequestID = httpx.RequestID(r.Context())
		h := w.Header()
		h.Set("Content-Type", api.ContentTypeProblem)
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(e.Status)
		_ = json.NewEncoder(w).Encode(commandConflictProblem{Error: e, Reason: reason})
		return
	}
	s.failM2(w, r, err)
}

// newKey is a fresh idempotency key for requests that did not bring one.
func newKey(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func (s *Service) wake(deviceID string) {
	if s.WakeAgent != nil {
		s.WakeAgent(deviceID)
	}
}

// ---- handlers ----

func (s *Service) queueCommand(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req QueueCommandRequest
	if !decodeBodyMax(w, r, 8<<10, &req) {
		return
	}
	key := req.IdempotencyKey
	if h := strings.TrimSpace(r.Header.Get("Idempotency-Key")); h != "" {
		if key != "" && key != h {
			s.failM2(w, r, &store.ValidationError{Fields: []store.FieldError{{Field: "idempotency_key", Message: "The header and the body name different keys."}}})
			return
		}
		key = h
	}
	s.queue(w, r, p, r.PathValue("id"), store.CommandInput{Type: req.Type, Params: req.Params, IdempotencyKey: key,
		TTL: time.Duration(req.TTLSeconds) * time.Second})
}

// queue is the shared tail of POST /commands and POST /runs/{id}/cancel.
func (s *Service) queue(w http.ResponseWriter, r *http.Request, p *auth.Principal, deviceID string, in store.CommandInput) {
	res, err := s.Store.QueueCommandAudited(r.Context(), p.Scope, deviceID, in, actorOf(r, p))
	if err != nil {
		s.failCommand(w, r, err)
		return
	}
	if res.Created {
		s.wake(deviceID) // the device's long poll returns now, not at the end of its wait
		w.Header().Set("Location", "/api/v1/devices/"+deviceID+"/commands")
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	now := s.now()
	httpx.WriteJSON(w, status, CommandResponse{ComputedAt: now, CommandJSON: commandJSON(res.Command, now), Coalesced: res.Coalesced})
}

func (s *Service) listCommands(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	offset, limit, ok := pageParams(q, 200)
	if !ok {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	now := s.now()
	page, err := s.Store.ListDeviceCommands(r.Context(), p.Scope, r.PathValue("id"), store.CommandFilter{Status: q.Get("status"), Type: q.Get("type")}, now, 0, 0, offset, limit)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	resp := CommandListResponse{ComputedAt: now, Total: page.Page.Total, Commands: make([]CommandJSON, 0, len(page.Items))}
	resp.NextCursor, resp.Truncated = nextCursor(page.Page)
	for _, c := range page.Items {
		resp.Commands = append(resp.Commands, commandJSON(c, now))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) cancelCommand(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	_, _, err := s.Store.CancelCommandAudited(r.Context(), p.Scope, r.PathValue("id"), r.PathValue("cid"), actorOf(r, p))
	if err != nil {
		s.failCommand(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cancelRun queues a cancel command for the device of an open run. The agent
// stops whatever run it has active (restic gets the graceful cancel), so the run
// must be open and agent-owned: a finished run, or a console-side maintenance
// run, is a 409 with its own reason.
func (s *Service) cancelRun(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	run, err := s.Store.GetRun(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.failCommand(w, r, err)
		return
	}
	if reason := notCancellable(run); reason != "" {
		e := api.NewError(api.CodeConflict)
		e.RequestID = httpx.RequestID(r.Context())
		h := w.Header()
		h.Set("Content-Type", api.ContentTypeProblem)
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(e.Status)
		_ = json.NewEncoder(w).Encode(commandConflictProblem{Error: e, Reason: reason})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = newKey("api-cancel-")
	}
	params, _ := json.Marshal(map[string]string{"run_id": run.ID})
	s.queue(w, r, p, run.DeviceID, store.CommandInput{Type: store.CommandCancel, Params: params, IdempotencyKey: key})
}

// notCancellable says why a run cannot be cancelled ("" when it can).
func notCancellable(run store.Run) string {
	switch {
	case run.Outcome != "":
		return "run_finished"
	case run.Origin != "agent":
		return "run_not_on_device"
	}
	return ""
}

func (s *Service) resolveIdentity(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req IdentityResolveRequest
	if !decodeBodyMax(w, r, 8<<10, &req) {
		return
	}
	d, err := s.Store.ResolveIdentityConflict(r.Context(), p.Scope, r.PathValue("id"), store.IdentityResolution{Action: req.Action, Reason: req.Reason}, actorOf(r, p))
	if err != nil {
		if errors.Is(err, store.ErrAudit) {
			s.log().Error("admin api: identity resolution refused, not audited", logx.Class("audit"), logx.Err(err))
		}
		s.failCommand(w, r, err)
		return
	}
	if d.RetiredAt != nil && s.SyncGateway != nil { // the retired device's gateway login must stop working now
		s.SyncGateway()
	}
	httpx.WriteJSON(w, http.StatusOK, IdentityResolveResponse{ComputedAt: s.now(), ID: d.ID, CustomerID: d.CustomerID, Hostname: d.Hostname,
		Resolution: req.Action, Retired: d.RetiredAt != nil})
}
