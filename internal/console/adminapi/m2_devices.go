package adminapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// NextDueJSON is the estimate of a device's next scheduled backup. Kind is
// scheduled, due, paused, retired or unknown; At is set for scheduled and due.
type NextDueJSON struct {
	Kind string     `json:"kind"`
	At   *time.Time `json:"at,omitempty"`
}

// DeviceRowJSON is a row of a customer's device table.
type DeviceRowJSON struct {
	DeviceJSON
	FriendlyName string       `json:"friendly_name,omitempty"`
	UpdateRing   string       `json:"update_ring"`
	NextDue      *NextDueJSON `json:"next_due,omitempty"`
}

// CustomerDevicesResponse is GET /api/v1/customers/{id}/devices.
type CustomerDevicesResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	LoadedAt   time.Time `json:"loaded_at"`
	Complete   bool      `json:"complete"`
	// Total counts the devices that match, Of the customer's devices before the state filter.
	Total      int             `json:"total"`
	Of         int             `json:"of"`
	Truncated  bool            `json:"truncated"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Devices    []DeviceRowJSON `json:"devices"`
}

// PatchDeviceRequest is the body of PATCH /api/v1/devices/{id}. Absent fields
// are unchanged; null clears friendly_name and notes.
type PatchDeviceRequest struct {
	FriendlyName optional[string] `json:"friendly_name"`
	Notes        optional[string] `json:"notes"`
	DeviceClass  optional[string] `json:"device_class"`
	UpdateRing   optional[string] `json:"update_ring"`
}

// DeviceSettingsResponse is the answer to PATCH /api/v1/devices/{id}.
type DeviceSettingsResponse struct {
	ComputedAt   time.Time `json:"computed_at"`
	ID           string    `json:"id"`
	CustomerID   string    `json:"customer_id"`
	Hostname     string    `json:"hostname"`
	FriendlyName string    `json:"friendly_name,omitempty"`
	Notes        string    `json:"notes,omitempty"`
	DeviceClass  string    `json:"device_class"`
	UpdateRing   string    `json:"update_ring"`
	Changed      bool      `json:"changed"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DeviceRunsResponse is GET /api/v1/devices/{id}/runs.
type DeviceRunsResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	Total      int       `json:"total"`
	Truncated  bool      `json:"truncated"`
	NextCursor string    `json:"next_cursor,omitempty"`
	Runs       []RunJSON `json:"runs"`
}

func nextDueJSON(n store.NextDue) *NextDueJSON {
	out := &NextDueJSON{Kind: n.Kind}
	if n.Kind == store.DueNow || n.Kind == store.DueScheduled {
		out.At = ptrTime(n.At)
	}
	return out
}

func runJSON(ru store.Run) RunJSON {
	return RunJSON{ID: ru.ID, Kind: ru.Kind, Trigger: ru.Trigger, Origin: ru.Origin, StartedAt: ru.StartedAt, FinishedAt: ru.FinishedAt,
		Outcome: ru.Outcome, ExitCode: ru.ResticExitCode, ErrorClass: ru.ErrorClass, ErrorCounts: ru.ErrorCounts, DeferredReason: ru.DeferredReason, Attempt: ru.Attempt}
}

// splitList reads a comma-separated parameter, repeated or not.
func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (s *Service) listCustomerDevices(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	// 404 for a customer outside scope, exactly as for one that does not exist.
	if _, err := s.Store.GetCustomer(r.Context(), p.Scope, id); err != nil {
		s.failM2(w, r, err)
		return
	}
	q := r.URL.Query()
	offset, limit, ok := pageParams(q, 500)
	retired, ok2 := boolParam(q.Get("include_retired"))
	if d := q.Get("dir"); !ok || !ok2 || (d != "" && d != "asc" && d != "desc") {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	fleet, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tb, err := s.Store.DeviceTable(r.Context(), p.Scope, store.DeviceFilter{CustomerID: id, Search: q.Get("q"), DeviceClass: q.Get("class"),
		UpdateRing: q.Get("ring"), IncludeRetired: retired != nil && *retired}, fleet.View.Devices,
		store.DeviceTableQuery{States: splitList(q["state"]), Sort: q.Get("sort"), Desc: q.Get("dir") == "desc", Offset: offset, Limit: limit},
		fleet.Computed, s.loc())
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	resp := CustomerDevicesResponse{ComputedAt: fleet.Computed, LoadedAt: fleet.View.LoadedAt, Complete: fleet.View.Complete,
		Total: tb.Page.Total, Of: tb.Total, Devices: make([]DeviceRowJSON, 0, len(tb.Rows))}
	resp.NextCursor, resp.Truncated = nextCursor(tb.Page)
	for _, row := range tb.Rows {
		ds := row.State
		if !row.InView { // no verdict: loud, never healthy
			ds = state.DeviceState{ID: row.ID, CustomerID: row.CustomerID, Hostname: row.Hostname, Primary: state.StateUnknown, Alarm: state.SeverityCritical}
		}
		resp.Devices = append(resp.Devices, DeviceRowJSON{DeviceJSON: deviceJSON(ds, row.CustomerName, row.Device), FriendlyName: row.FriendlyName,
			UpdateRing: row.UpdateRing, NextDue: nextDueJSON(row.Next)})
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) patchDevice(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req PatchDeviceRequest
	if !decodeBodyMax(w, r, 16<<10, &req) {
		return
	}
	var patch store.DeviceSettingsPatch
	free := func(o optional[string]) *string {
		if !o.Set {
			return nil
		}
		v := o.Value // null clears (the empty string)
		return &v
	}
	patch.FriendlyName, patch.Notes = free(req.FriendlyName), free(req.Notes)
	for _, f := range []struct {
		o   optional[string]
		dst **string
	}{{req.DeviceClass, &patch.DeviceClass}, {req.UpdateRing, &patch.UpdateRing}} {
		if !f.o.Set {
			continue
		}
		if f.o.Null { // an enumerated setting cannot be cleared
			httpx.Problem(w, r, api.CodeInvalidRequest, 0)
			return
		}
		v := f.o.Value
		*f.dst = &v
	}
	d, changed, err := s.Store.UpdateDeviceSettings(r.Context(), p.Scope, r.PathValue("id"), patch, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, DeviceSettingsResponse{ComputedAt: s.now(), ID: d.ID, CustomerID: d.CustomerID, Hostname: d.Hostname,
		FriendlyName: d.FriendlyName, Notes: d.Notes, DeviceClass: d.DeviceClass, UpdateRing: d.UpdateRing, Changed: changed, UpdatedAt: d.UpdatedAt.UTC()})
}

func (s *Service) listDeviceRuns(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	offset, limit, ok := pageParams(q, 200)
	var since, until time.Time
	for _, f := range []struct {
		key string
		dst *time.Time
	}{{"since", &since}, {"until", &until}} {
		if v := q.Get(f.key); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				ok = false
			}
			*f.dst = t.UTC()
		}
	}
	if !ok {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	page, err := s.Store.ListDeviceRunsAt(r.Context(), p.Scope, r.PathValue("id"),
		store.RunFilter{Kind: q.Get("kind"), Outcome: q.Get("outcome"), Since: since, Until: until}, offset, limit)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	resp := DeviceRunsResponse{ComputedAt: s.now(), Total: page.Page.Total, Runs: make([]RunJSON, 0, len(page.Items))}
	resp.NextCursor, resp.Truncated = nextCursor(page.Page)
	for _, ru := range page.Items {
		resp.Runs = append(resp.Runs, runJSON(ru))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
