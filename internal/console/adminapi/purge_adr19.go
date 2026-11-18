package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Purge of a retired device, delete of a customer, and the fresh-MFA step-up they need (ADR 0019).
//
// The three destructive routes are for the owner role, on a signed-in session only, and behind
// auth.RequireFreshMFA: an API token is refused with plain forbidden by RequireSession before any
// step-up prompt, a non-owner session with forbidden by the role check, and an owner whose last TOTP
// check is older than auth.FreshMFAWindow with 403 mfa_step_up_required. The client then calls
// POST /api/v1/auth/step-up with a current code and repeats the request.
//
// Each carries a typed confirmation: the device's host name, or the customer's name, compared here
// exactly. A mismatch is 400 with the field named and neither name in the answer or a log line.
//
// A purge request only marks the device (store.RequestDevicePurge, one audit row
// device.purge.request) and wakes the device-purge job; the job removes the repository and deletes
// the row (jobs.DevicePurge). No route here deletes a device row.

// Route patterns of ADR 0019.
const (
	PatternStepUp               = "POST /api/v1/auth/step-up"
	PatternDevicePurge          = "DELETE /api/v1/devices/{id}"
	PatternCustomerDelete       = "DELETE /api/v1/customers/{id}"
	PatternCustomerPurgeRetired = "POST /api/v1/customers/{id}/purge-retired-devices"
)

// purgePatterns lists the patterns registerPurge adds.
func purgePatterns() []string {
	return []string{PatternStepUp, PatternDevicePurge, PatternCustomerDelete, PatternCustomerPurgeRetired}
}

// Machine reasons of the 409 answers (the problem document's "reason").
const (
	ReasonDeviceNotRetired   = "device_not_retired"
	ReasonCustomerHasDevices = "customer_has_devices"
)

// purgeWakeTimeout bounds the device-purge run a request starts: one purge_repo call may take two
// hours (adminpath's timeout for the method).
const purgeWakeTimeout = 2*time.Hour + 15*time.Minute

// registerPurge adds the ADR 0019 routes.
func (s *Service) registerPurge(r httpx.Registrar) {
	ow := func(h http.HandlerFunc) http.Handler {
		return s.Auth.RequireSession(auth.RoleOwner, auth.ModeAPI, s.Auth.RequireFreshMFA(h))
	}
	r.Handle(PatternStepUp, s.Auth.RequireSession(auth.RoleReadOnly, auth.ModeAPI, http.HandlerFunc(s.stepUp)))
	r.Handle(PatternDevicePurge, ow(s.devicePurge))
	r.Handle(PatternCustomerDelete, ow(s.customerDelete))
	r.Handle(PatternCustomerPurgeRetired, ow(s.customerPurgeRetired))
}

// ---- shapes ----

// StepUpRequest is the body of POST /api/v1/auth/step-up.
type StepUpRequest struct {
	Code string `json:"code"`
}

// DevicePurgeRequest is the body of DELETE /api/v1/devices/{id}.
type DevicePurgeRequest struct {
	// ConfirmHostName must equal the device's host name exactly.
	ConfirmHostName string `json:"confirm_host_name"`
	// Reason is required on the first request: one line, no file paths.
	Reason string `json:"reason"`
}

// CustomerConfirmRequest is the body of DELETE /api/v1/customers/{id} and of
// POST /api/v1/customers/{id}/purge-retired-devices.
type CustomerConfirmRequest struct {
	// ConfirmName must equal the customer's name exactly.
	ConfirmName string `json:"confirm_name"`
	// Reason is required: one line, no file paths.
	Reason string `json:"reason"`
}

// DevicePurgeJSON is the purge state of a purging device, in every device JSON that has one
// (absent when the device is not purging). It never carries a host name.
type DevicePurgeJSON struct {
	RequestedAt time.Time `json:"requested_at"`
	// LastAttemptAt is when the job last tried to remove the repository; absent before the first try.
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	// LastErrorClass is the fixed class of the last failed try; absent when none is outstanding.
	LastErrorClass string `json:"last_error_class,omitempty"`
}

// purgeJSON is d's purge state, or nil when d is not purging.
func purgeJSON(d store.Device) *DevicePurgeJSON {
	if d.PurgeRequestedAt == nil {
		return nil
	}
	return &DevicePurgeJSON{RequestedAt: d.PurgeRequestedAt.UTC(), LastAttemptAt: utcPtr(d.PurgeLastAttemptAt), LastErrorClass: d.PurgeLastErrorClass}
}

// DevicePurgeResponse is the 202 answer of DELETE /api/v1/devices/{id}: the device as it stands.
type DevicePurgeResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	ID         string    `json:"id"`
	CustomerID string    `json:"customer_id"`
	State      string    `json:"state"` // RETIRED: a purging device is retired until its row is deleted
	// Changed is false when the device was already purging: nothing was written, no audit row exists
	// for this call, and the first request's reason and time stand.
	Changed bool            `json:"changed"`
	Purge   DevicePurgeJSON `json:"purge"`
}

// CustomerPurgeRetiredResponse is the 202 answer of POST /api/v1/customers/{id}/purge-retired-devices.
type CustomerPurgeRetiredResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	CustomerID string    `json:"customer_id"`
	// Requested is how many retired devices this call marked for purge (one audit row each);
	// RequestedIDs names them.
	Requested    int      `json:"requested"`
	RequestedIDs []string `json:"requested_ids"`
	// AlreadyPurging is how many retired devices were purging before this call (left as they were).
	AlreadyPurging int `json:"already_purging"`
	// ActiveUntouched is how many active devices the customer has; they are not touched.
	ActiveUntouched int `json:"active_untouched"`
}

// customerHasDevicesProblem is the 409 of DELETE /api/v1/customers/{id} while devices exist.
type customerHasDevicesProblem struct {
	*api.Error
	Reason  string `json:"reason"`
	Active  int    `json:"active"`
	Retired int    `json:"retired"`
}

// ---- handlers ----

func (s *Service) stepUp(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req StepUpRequest
	if !decodeBodyMax(w, r, 1<<10, &req) {
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	if err := s.Auth.StepUp(r.Context(), p, strings.TrimSpace(req.Code), httpx.ClientIP(r), r.UserAgent()); err != nil {
		s.authFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// confirmMismatch answers 400 naming the field; it never echoes what was typed or what was expected.
func (s *Service) confirmMismatch(w http.ResponseWriter, r *http.Request, field, msg string) {
	s.failM2(w, r, &store.ValidationError{Fields: []store.FieldError{{Field: field, Message: msg}}})
}

func (s *Service) devicePurge(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req DevicePurgeRequest
	if !decodeBodyMax(w, r, 8<<10, &req) {
		return
	}
	id := r.PathValue("id")
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if req.ConfirmHostName == "" || req.ConfirmHostName != dev.Hostname {
		s.confirmMismatch(w, r, "confirm_host_name", "Type the device's host name exactly as shown to confirm.")
		return
	}
	res, err := s.Store.RequestDevicePurge(r.Context(), p.Scope, id, req.Reason, actorOf(r, p))
	if err != nil {
		if errors.Is(err, store.ErrDeviceNotRetired) {
			conflictWithReason(w, r, ReasonDeviceNotRetired)
			return
		}
		s.failM2(w, r, err)
		return
	}
	if res.Changed {
		s.committed(r)
		s.deviceChanged(id)
		s.wakePurge()
	}
	out := DevicePurgeResponse{ComputedAt: s.now(), ID: res.Device.ID, CustomerID: res.Device.CustomerID, State: string(state.StateRetired), Changed: res.Changed}
	if pj := purgeJSON(res.Device); pj != nil {
		out.Purge = *pj
	}
	httpx.WriteJSON(w, http.StatusAccepted, out)
}

func (s *Service) customerDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CustomerConfirmRequest
	if !decodeBodyMax(w, r, 8<<10, &req) {
		return
	}
	id := r.PathValue("id")
	cust, err := s.Store.GetCustomer(r.Context(), p.Scope, id)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if req.ConfirmName == "" || req.ConfirmName != cust.Name {
		s.confirmMismatch(w, r, "confirm_name", "Type the customer's name exactly as shown to confirm.")
		return
	}
	if _, err := s.Store.DeleteCustomer(r.Context(), p.Scope, id, req.Reason, actorOf(r, p)); err != nil {
		var has *store.CustomerHasDevicesError
		if errors.As(err, &has) {
			e := api.NewError(api.CodeConflict)
			e.RequestID = httpx.RequestID(r.Context())
			h := w.Header()
			h.Set("Content-Type", api.ContentTypeProblem)
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(e.Status)
			_ = json.NewEncoder(w).Encode(customerHasDevicesProblem{Error: e, Reason: ReasonCustomerHasDevices, Active: has.Active, Retired: has.Retired})
			return
		}
		s.failM2(w, r, err)
		return
	}
	s.committed(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) customerPurgeRetired(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CustomerConfirmRequest
	if !decodeBodyMax(w, r, 8<<10, &req) {
		return
	}
	id := r.PathValue("id")
	cust, err := s.Store.GetCustomer(r.Context(), p.Scope, id)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	if req.ConfirmName == "" || req.ConfirmName != cust.Name {
		s.confirmMismatch(w, r, "confirm_name", "Type the customer's name exactly as shown to confirm.")
		return
	}
	// The reason is checked up front, so the answer to bad input does not depend on which devices
	// happen to be purging already.
	if _, bad := store.ValidatePurgeReason(req.Reason); len(bad) > 0 {
		s.failM2(w, r, &store.ValidationError{Fields: bad})
		return
	}
	var devs []store.Device
	for cursor := ""; ; {
		l, err := s.Store.ListDevices(r.Context(), p.Scope, store.ListDevicesOptions{CustomerID: id, IncludeRetired: true, Page: store.Page{Limit: 1000, Cursor: cursor}})
		if err != nil {
			s.failM2(w, r, err)
			return
		}
		devs = append(devs, l.Items...)
		if l.NextCursor == "" {
			break
		}
		cursor = l.NextCursor
	}
	out := CustomerPurgeRetiredResponse{CustomerID: id, RequestedIDs: []string{}}
	// Each request is its own transaction with its own audit row. A failure part-way answers the
	// error; the requests already made stand (each is audited) and the job is still woken for them.
	finish := func() {
		if out.Requested > 0 {
			s.committed(r)
			s.wakePurge()
		}
	}
	for _, d := range devs {
		switch {
		case d.RetiredAt == nil:
			out.ActiveUntouched++
			continue
		case d.Purging():
			out.AlreadyPurging++
			continue
		}
		res, err := s.Store.RequestDevicePurge(r.Context(), p.Scope, d.ID, req.Reason, actorOf(r, p))
		if err != nil {
			finish()
			if errors.Is(err, store.ErrDeviceNotRetired) {
				conflictWithReason(w, r, ReasonDeviceNotRetired)
				return
			}
			s.failM2(w, r, err)
			return
		}
		if res.Changed {
			out.Requested++
			out.RequestedIDs = append(out.RequestedIDs, d.ID)
		} else {
			out.AlreadyPurging++
		}
	}
	finish()
	out.ComputedAt = s.now()
	httpx.WriteJSON(w, http.StatusAccepted, out)
}

// conflictWithReason writes a 409 whose problem document carries a machine reason.
func conflictWithReason(w http.ResponseWriter, r *http.Request, reason string) {
	e := api.NewError(api.CodeConflict)
	e.RequestID = httpx.RequestID(r.Context())
	h := w.Header()
	h.Set("Content-Type", api.ContentTypeProblem)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(commandConflictProblem{Error: e, Reason: reason})
}

// wakePurge runs the device-purge job now, in the background, when this console runs it: a purge
// request starts at once (ADR 0019 ruling 2). Another run holding the job's lease makes this one a
// no-op; that run, or the next interval, picks the device up.
func (s *Service) wakePurge() {
	if s.Jobs == nil || !slices.Contains(s.Jobs.Names(), jobs.NameDevicePurge) {
		return
	}
	if !s.enterJobRun() {
		return // shutting down: the next start's first run picks it up
	}
	go func() {
		defer jobBackground.Done()
		ctx, cancel := context.WithTimeout(s.jobContext(), purgeWakeTimeout)
		defer cancel()
		if _, err := s.Jobs.RunOnce(ctx, jobs.NameDevicePurge); err != nil {
			s.log().Warn("device-purge run after a purge request failed", logx.Str("job", jobs.NameDevicePurge), logx.Err(err))
		}
	}()
}
