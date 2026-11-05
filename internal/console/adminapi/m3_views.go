package adminapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Saved views (architecture §5.2 /views, flow F06). A view belongs to the
// account that made it. Any signed-in account may make views; the owner edits
// and deletes their own, an admin any view they can see (the SELF/AD rule).
//
// An owner who has lost one of the customers their shared view is for still sees
// it (marked locked) and may make it private or delete it, both of which only
// reduce who sees it; a rename or a filter change is refused (404).
//
// A shared view names its audience when it is shared: all customers, or a list of
// customer ids. Another account sees it only when the audience lies inside that
// account's scope (all customers: only accounts that see every customer). The
// audience must lie inside the sharer's own scope and hold every customer and
// device the filter names, or the write is a 400. The view's name and its text
// clauses are never read for scope (store/views.go says why).

// SavedViewJSON is one view. Filter, Columns and Sort are opaque to the server:
// the console pages define their shape.
type SavedViewJSON struct {
	ID      string          `json:"id"`
	OwnerID string          `json:"owner_id,omitempty"`
	Owner   string          `json:"owner,omitempty"`
	Mine    bool            `json:"mine"`
	Name    string          `json:"name"`
	Entity  string          `json:"entity"`
	Filter  json.RawMessage `json:"filter"`
	Columns json.RawMessage `json:"columns"`
	Sort    json.RawMessage `json:"sort"`
	Shared  bool            `json:"shared"`
	// Audience is present on a shared view: who besides the owner sees it.
	Audience *ViewAudienceJSON `json:"audience,omitempty"`
	// Locked is true on your own shared view whose audience reaches past your
	// scope (you lost one of its customers): you may make it private or delete
	// it; any other change answers 404.
	Locked    bool      `json:"locked,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ViewAudienceJSON is the audience of a shared view: exactly one of
// all_customers (true) or customer_ids (1 to 50 ids).
type ViewAudienceJSON struct {
	AllCustomers bool     `json:"all_customers,omitempty"`
	CustomerIDs  []string `json:"customer_ids,omitempty"`
}

func (a *ViewAudienceJSON) store() *store.ViewAudience {
	if a == nil {
		return nil
	}
	return &store.ViewAudience{All: a.AllCustomers, Customers: a.CustomerIDs}
}

func viewJSON(v store.SavedView, viewer string, scope store.Scope) SavedViewJSON {
	out := SavedViewJSON{ID: v.ID, OwnerID: v.OwnerID, Owner: v.OwnerName, Mine: v.OwnerID != "" && v.OwnerID == viewer, Name: v.Name, Entity: v.Entity,
		Filter: v.Filter, Columns: v.Columns, Sort: v.Sort, Shared: v.Shared, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	out.Locked = out.Mine && !v.AudienceInside(scope)
	if v.Audience != nil {
		out.Audience = &ViewAudienceJSON{AllCustomers: v.Audience.All, CustomerIDs: v.Audience.Customers}
	}
	return out
}

// ViewListResponse is GET /api/v1/views.
type ViewListResponse struct {
	ComputedAt time.Time       `json:"computed_at"`
	Views      []SavedViewJSON `json:"views"`
}

// ViewResponse is the body of POST and PATCH on a view.
type ViewResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	SavedViewJSON
	// Changed is false for a PATCH that matched the stored view (nothing written).
	Changed bool `json:"changed"`
}

// CreateViewRequest is the body of POST /api/v1/views.
type CreateViewRequest struct {
	Name    string          `json:"name"`
	Entity  string          `json:"entity"`
	Filter  json.RawMessage `json:"filter"`
	Columns json.RawMessage `json:"columns"`
	Sort    json.RawMessage `json:"sort"`
	Shared  bool            `json:"shared"`
	// Audience is required when shared is true and refused otherwise.
	Audience *ViewAudienceJSON `json:"audience"`
}

// PatchViewRequest is the body of PATCH /api/v1/views/{id}; absent fields are
// unchanged. The entity of a view does not change.
type PatchViewRequest struct {
	Name    *string          `json:"name"`
	Filter  *json.RawMessage `json:"filter"`
	Columns *json.RawMessage `json:"columns"`
	Sort    *json.RawMessage `json:"sort"`
	Shared  *bool            `json:"shared"`
	// Audience replaces the audience of a shared view; making a view shared needs it.
	Audience *ViewAudienceJSON `json:"audience"`
}

func (s *Service) listViews(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	vs, err := s.Store.ListSavedViews(r.Context(), p.Scope, p.Admin.ID, r.URL.Query().Get("entity"))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	out := ViewListResponse{ComputedAt: s.now(), Views: make([]SavedViewJSON, 0, len(vs))}
	for _, v := range vs {
		out.Views = append(out.Views, viewJSON(v, p.Admin.ID, p.Scope))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) createView(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CreateViewRequest
	if !decodeBodyMax(w, r, 64<<10, &req) {
		return
	}
	for _, f := range []*json.RawMessage{&req.Filter, &req.Columns, &req.Sort} {
		if string(*f) == "null" { // an explicit null is "not given"
			*f = nil
		}
	}
	v, err := s.Store.CreateSavedViewAudited(r.Context(), p.Scope, store.SavedViewInput{Name: req.Name, Entity: req.Entity, Filter: req.Filter, Columns: req.Columns,
		Sort: req.Sort, Shared: req.Shared, Audience: req.Audience.store()}, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/views/"+v.ID)
	httpx.WriteJSON(w, http.StatusCreated, ViewResponse{ComputedAt: s.now(), SavedViewJSON: viewJSON(v, p.Admin.ID, p.Scope), Changed: true})
}

func (s *Service) patchView(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req PatchViewRequest
	if !decodeBodyMax(w, r, 64<<10, &req) {
		return
	}
	v, changed, err := s.Store.UpdateSavedViewAudited(r.Context(), p.Scope, r.PathValue("id"), store.SavedViewPatch{Name: req.Name, Filter: req.Filter,
		Columns: req.Columns, Sort: req.Sort, Shared: req.Shared, Audience: req.Audience.store()}, actorOf(r, p), p.Role >= auth.RoleAdmin)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ViewResponse{ComputedAt: s.now(), SavedViewJSON: viewJSON(v, p.Admin.ID, p.Scope), Changed: changed})
}

func (s *Service) deleteView(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := s.Store.DeleteSavedViewAudited(r.Context(), p.Scope, r.PathValue("id"), actorOf(r, p), p.Role >= auth.RoleAdmin); err != nil {
		s.failM2(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
