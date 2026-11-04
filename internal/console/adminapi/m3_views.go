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
// account that made it; a shared view is visible to every account. Any signed-in
// account may make views; the owner edits and deletes their own, an admin any
// view they can see (the SELF/AD rule). Only an account that sees every
// customer may share a view, and a scoped account does not see another account's
// shared view whose filter names a customer outside its scope.

// SavedViewJSON is one view. Filter, Columns and Sort are opaque to the server:
// the console pages define their shape.
type SavedViewJSON struct {
	ID        string          `json:"id"`
	OwnerID   string          `json:"owner_id,omitempty"`
	Owner     string          `json:"owner,omitempty"`
	Mine      bool            `json:"mine"`
	Name      string          `json:"name"`
	Entity    string          `json:"entity"`
	Filter    json.RawMessage `json:"filter"`
	Columns   json.RawMessage `json:"columns"`
	Sort      json.RawMessage `json:"sort"`
	Shared    bool            `json:"shared"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func viewJSON(v store.SavedView, viewer string) SavedViewJSON {
	return SavedViewJSON{ID: v.ID, OwnerID: v.OwnerID, Owner: v.OwnerName, Mine: v.OwnerID != "" && v.OwnerID == viewer, Name: v.Name, Entity: v.Entity,
		Filter: v.Filter, Columns: v.Columns, Sort: v.Sort, Shared: v.Shared, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
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
}

// PatchViewRequest is the body of PATCH /api/v1/views/{id}; absent fields are
// unchanged. The entity of a view does not change.
type PatchViewRequest struct {
	Name    *string          `json:"name"`
	Filter  *json.RawMessage `json:"filter"`
	Columns *json.RawMessage `json:"columns"`
	Sort    *json.RawMessage `json:"sort"`
	Shared  *bool            `json:"shared"`
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
		out.Views = append(out.Views, viewJSON(v, p.Admin.ID))
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
		Sort: req.Sort, Shared: req.Shared}, actorOf(r, p))
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/views/"+v.ID)
	httpx.WriteJSON(w, http.StatusCreated, ViewResponse{ComputedAt: s.now(), SavedViewJSON: viewJSON(v, p.Admin.ID), Changed: true})
}

func (s *Service) patchView(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req PatchViewRequest
	if !decodeBodyMax(w, r, 64<<10, &req) {
		return
	}
	v, changed, err := s.Store.UpdateSavedViewAudited(r.Context(), p.Scope, r.PathValue("id"), store.SavedViewPatch{Name: req.Name, Filter: req.Filter,
		Columns: req.Columns, Sort: req.Sort, Shared: req.Shared}, actorOf(r, p), p.Role >= auth.RoleAdmin)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ViewResponse{ComputedAt: s.now(), SavedViewJSON: viewJSON(v, p.Admin.ID), Changed: changed})
}

func (s *Service) deleteView(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if err := s.Store.DeleteSavedViewAudited(r.Context(), p.Scope, r.PathValue("id"), actorOf(r, p), p.Role >= auth.RoleAdmin); err != nil {
		s.failM2(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
