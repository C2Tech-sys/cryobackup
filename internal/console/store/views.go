package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Saved views (M3-T3, architecture §5.2 /views, table saved_views): a named
// filter, column list and sort for the devices, runs or customers table. A view
// belongs to the admin who made it; a shared view is visible to every admin.
//
// A view holds no customer data of its own, but its filter may name customers
// by id. So the same Scope rules apply as everywhere: only an all-customer
// caller may share a view (a scoped caller's view is private), and a scoped
// caller does not see another admin's shared view whose filter names a customer
// outside the caller's scope. An invisible view is ErrNotFound, exactly like a
// view that does not exist.

// ViewEntities are the tables a view can describe.
var ViewEntities = []string{"devices", "runs", "customers"}

const maxViewJSON = 16 << 10

// SavedView is one row of saved_views. Filter, Columns and Sort are JSON the
// console pages interpret; the store keeps them opaque beyond their shape.
type SavedView struct {
	ID        string
	OwnerID   string // "" when the owner's account was deleted: the view is then shared-only
	OwnerName string
	Name      string
	Entity    string
	Filter    json.RawMessage // a JSON object (the clause tree)
	Columns   json.RawMessage // a JSON array of column ids
	Sort      json.RawMessage // a JSON array of {column, dir}
	Shared    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SavedViewInput is the body of a create. Nil Filter, Columns or Sort default to
// {}, [] and [].
type SavedViewInput struct {
	Name    string
	Entity  string
	Filter  json.RawMessage
	Columns json.RawMessage
	Sort    json.RawMessage
	Shared  bool
}

// SavedViewPatch is a partial update; nil fields are left alone.
type SavedViewPatch struct {
	Name    *string
	Filter  *json.RawMessage
	Columns *json.RawMessage
	Sort    *json.RawMessage
	Shared  *bool
}

const viewCols = `v.id, v.owner_id, COALESCE(a.username, ''), v.name, v.entity, v.filter, v.columns, v.sort, v.shared, v.created_at, v.updated_at`

const viewFrom = ` FROM saved_views v LEFT JOIN admin_users a ON a.id = v.owner_id`

func scanView(sc scanner) (SavedView, error) {
	var v SavedView
	var owner sql.NullString
	var filter, cols, sort string
	var shared int
	var c, u int64
	if err := sc.Scan(&v.ID, &owner, &v.OwnerName, &v.Name, &v.Entity, &filter, &cols, &sort, &shared, &c, &u); err != nil {
		return SavedView{}, err
	}
	v.OwnerID, v.Shared = owner.String, shared != 0
	v.Filter, v.Columns, v.Sort = json.RawMessage(filter), json.RawMessage(cols), json.RawMessage(sort)
	v.CreatedAt, v.UpdatedAt = timeOf(c), timeOf(u)
	return v, nil
}

// viewCustomerKeys are the filter keys that name customers.
var viewCustomerKeys = map[string]bool{"customer": true, "customer_id": true, "customer_ids": true, "customers": true}

// viewCustomerRefs lists every customer id a filter names, at any depth.
func viewCustomerRefs(filter json.RawMessage) []string {
	var doc any
	if json.Unmarshal(filter, &doc) != nil {
		return nil
	}
	var out []string
	var walk func(v any, named bool)
	walk = func(v any, named bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(e, named || viewCustomerKeys[k])
			}
		case []any:
			for _, e := range x {
				walk(e, named)
			}
		case string:
			if named && x != "" {
				out = append(out, x)
			}
		}
	}
	walk(doc, false)
	return out
}

// visibleView reports whether viewer (an admin id) may see v under scope.
func visibleView(scope Scope, viewer string, v SavedView) bool {
	if v.OwnerID != "" && v.OwnerID == viewer {
		return true
	}
	if !v.Shared {
		return false
	}
	if scope.All {
		return true
	}
	for _, id := range viewCustomerRefs(v.Filter) {
		if !scope.Allows(id) {
			return false
		}
	}
	return true
}

// validateView checks the user-supplied parts. Rejected input is a
// *ValidationError whose sentences never echo the input.
func validateView(name *string, entity string, filter, columns, sort *json.RawMessage) []FieldError {
	var bad []FieldError
	if name != nil {
		if n, ok := cleanLine(*name, 100); !ok || n == "" {
			bad = append(bad, fieldf("name", "Use a name of 1 to 100 characters on one line."))
		} else {
			*name = n
		}
	}
	if entity != "" && !oneOf(entity, ViewEntities) {
		bad = append(bad, fieldf("entity", "Use devices, runs or customers."))
	}
	shape := func(field string, raw *json.RawMessage, kind byte, what string) {
		if raw == nil || len(*raw) == 0 {
			return
		}
		t := strings.TrimSpace(string(*raw))
		if len(t) > maxViewJSON || !json.Valid([]byte(t)) || t[0] != kind {
			bad = append(bad, fieldf(field, "Send %s of at most 16 KiB.", what))
			return
		}
		*raw = json.RawMessage(t)
	}
	shape("filter", filter, '{', "a JSON object")
	shape("columns", columns, '[', "a JSON array")
	shape("sort", sort, '[', "a JSON array")
	return bad
}

// ListSavedViews returns the views viewer (an admin id) may see under scope:
// their own and the shared ones, optionally of one entity, ordered by entity and
// name. An unknown entity is a *ValidationError.
func (s *Store) ListSavedViews(ctx context.Context, scope Scope, viewer, entity string) ([]SavedView, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if entity != "" && !oneOf(entity, ViewEntities) {
		return nil, validation([]FieldError{fieldf("entity", "Use devices, runs or customers.")})
	}
	q := `SELECT ` + viewCols + viewFrom + ` WHERE (v.owner_id = ? OR v.shared = 1)`
	args := []any{viewer}
	if entity != "" {
		q, args = q+` AND v.entity = ?`, append(args, entity)
	}
	var all []SavedView
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		all, err = collect(tx, q+` ORDER BY v.entity, v.name COLLATE NOCASE, v.id`, args, scanView)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]SavedView, 0, len(all))
	for _, v := range all {
		if visibleView(scope, viewer, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

// GetSavedView returns one view the viewer may see (ErrNotFound otherwise).
func (s *Store) GetSavedView(ctx context.Context, scope Scope, viewer, id string) (SavedView, error) {
	if err := scope.check(); err != nil {
		return SavedView{}, err
	}
	var v SavedView
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		v, err = one(tx, `SELECT `+viewCols+viewFrom+` WHERE v.id = ?`, []any{id}, scanView)
		return err
	})
	if err != nil {
		return SavedView{}, err
	}
	if !visibleView(scope, viewer, v) {
		return SavedView{}, ErrNotFound
	}
	return v, nil
}

// CreateSavedViewAudited makes a view owned by actor and writes one "view.create"
// audit row in the same transaction. Sharing needs an all-customer scope
// (ErrScope otherwise). A second view of the same owner, entity and name is
// ErrConflict.
func (s *Store) CreateSavedViewAudited(ctx context.Context, scope Scope, in SavedViewInput, actor AuditActor) (SavedView, error) {
	if err := scope.check(); err != nil {
		return SavedView{}, err
	}
	name := in.Name
	bad := validateView(&name, in.Entity, &in.Filter, &in.Columns, &in.Sort)
	if in.Entity == "" {
		bad = append(bad, fieldf("entity", "Use devices, runs or customers."))
	}
	if err := validation(bad); err != nil {
		return SavedView{}, err
	}
	if in.Shared && !scope.All {
		return SavedView{}, ErrScope
	}
	def := func(raw json.RawMessage, d string) string {
		if len(raw) == 0 {
			return d
		}
		return string(raw)
	}
	var out SavedView
	err := s.Write(ctx, func(tx *Tx) error {
		now, id := tx.nowMS(), tx.id()
		if _, err := tx.Exec(`INSERT INTO saved_views (id, owner_id, name, entity, filter, columns, sort, shared, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, id, nullStr(actor.AdminID), name, in.Entity, def(in.Filter, "{}"), def(in.Columns, "[]"),
			def(in.Sort, "[]"), boolInt(in.Shared), now, now); err != nil {
			return err
		}
		ev, err := actor.event("view.create", "saved_view", id, "", map[string]any{"name": name, "entity": in.Entity, "shared": in.Shared})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+viewCols+viewFrom+` WHERE v.id = ?`, []any{id}, scanView)
		return err
	})
	return out, err
}

// editableView loads view id for a change by actor. Not visible is ErrNotFound;
// visible but another admin's is ErrScope unless asAdmin (the AD role).
func editableView(tx *Tx, scope Scope, actor AuditActor, id string, asAdmin bool) (SavedView, error) {
	v, err := one(tx, `SELECT `+viewCols+viewFrom+` WHERE v.id = ?`, []any{id}, scanView)
	if err != nil {
		return SavedView{}, err
	}
	if !visibleView(scope, actor.AdminID, v) {
		return SavedView{}, ErrNotFound
	}
	if !(v.OwnerID != "" && v.OwnerID == actor.AdminID) && !asAdmin {
		return SavedView{}, ErrScope
	}
	return v, nil
}

// UpdateSavedViewAudited applies p to a view and writes one "view.update" audit
// row. Only the owner may edit a view, or an admin (asAdmin). Making a view
// shared needs an all-customer scope. A patch that changes nothing writes
// nothing (changed false).
func (s *Store) UpdateSavedViewAudited(ctx context.Context, scope Scope, id string, p SavedViewPatch, actor AuditActor, asAdmin bool) (v SavedView, changed bool, err error) {
	if err := scope.check(); err != nil {
		return SavedView{}, false, err
	}
	if bad := validateView(p.Name, "", p.Filter, p.Columns, p.Sort); len(bad) > 0 {
		return SavedView{}, false, validation(bad)
	}
	if p.Shared != nil && *p.Shared && !scope.All {
		return SavedView{}, false, ErrScope
	}
	err = s.Write(ctx, func(tx *Tx) error {
		cur, err := editableView(tx, scope, actor, id, asAdmin)
		if err != nil {
			return err
		}
		sets, args, fields := []string{"updated_at = ?"}, []any{tx.nowMS()}, []string{}
		set := func(col, field string, v any) {
			sets, args, fields = append(sets, col+" = ?"), append(args, v), append(fields, field)
		}
		same := func(raw *json.RawMessage, old json.RawMessage) bool { return raw == nil || sameJSON(*raw, old) }
		if p.Name != nil && *p.Name != cur.Name {
			set("name", "name", *p.Name)
		}
		if !same(p.Filter, cur.Filter) {
			set("filter", "filter", string(*p.Filter))
		}
		if !same(p.Columns, cur.Columns) {
			set("columns", "columns", string(*p.Columns))
		}
		if !same(p.Sort, cur.Sort) {
			set("sort", "sort", string(*p.Sort))
		}
		if p.Shared != nil && *p.Shared != cur.Shared {
			set("shared", "shared", boolInt(*p.Shared))
		}
		if len(fields) == 0 {
			v = cur
			return nil
		}
		if _, err := tx.Exec(`UPDATE saved_views SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...); err != nil {
			return err
		}
		ev, err := actor.event("view.update", "saved_view", id, "", map[string]any{"name": cur.Name, "entity": cur.Entity, "fields": fields})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		changed = true
		v, err = one(tx, `SELECT `+viewCols+viewFrom+` WHERE v.id = ?`, []any{id}, scanView)
		return err
	})
	if err != nil {
		return SavedView{}, false, err
	}
	return v, changed, nil
}

// DeleteSavedViewAudited removes a view and writes one "view.delete" audit row.
// Only the owner may delete a view, or an admin (asAdmin). Reports with a view
// that is gone are unaffected (reports.saved_view_id is ON DELETE SET NULL).
func (s *Store) DeleteSavedViewAudited(ctx context.Context, scope Scope, id string, actor AuditActor, asAdmin bool) error {
	if err := scope.check(); err != nil {
		return err
	}
	return s.Write(ctx, func(tx *Tx) error {
		cur, err := editableView(tx, scope, actor, id, asAdmin)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM saved_views WHERE id = ?`, id); err != nil {
			return err
		}
		ev, err := actor.event("view.delete", "saved_view", id, "", map[string]any{"name": cur.Name, "entity": cur.Entity, "shared": cur.Shared})
		if err != nil {
			return err
		}
		return audited(tx, ev)
	})
}
