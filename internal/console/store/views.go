package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// Saved views (M3-T3, architecture §5.2 /views, table saved_views): a named
// filter, column list and sort for the devices, runs or customers table. A view
// belongs to the admin who made it; a shared view is visible to the accounts of
// its audience as well.
//
// A view's name and its free-text clauses (a tag, an OS, a hostname pattern) can
// carry anything, including another customer's name, and text cannot be resolved
// to a customer reliably. So who may see a shared view is never inferred from its
// name or its filter. The sharer states it, as the view's audience:
//
//   - all customers: visible only to accounts that see every customer;
//   - a set of customer ids: visible to an account only when every one of them is
//     inside that account's scope.
//
// It fails closed: a shared view without a readable audience is visible to its
// owner only. On create and update the audience must lie inside the sharer's own
// scope (all customers needs an all-customer sharer), and the customers and
// devices the filter names (by id, hostname or customer name, resolved as
// viewfilter.go does) must lie inside the audience; otherwise the write is a
// *ValidationError (400). An invisible view is ErrNotFound, exactly like a view
// that does not exist. Views shared before the audience existed (migration 0005)
// have the audience all customers.

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
	// Audience is who besides the owner sees a shared view; nil for a private view.
	Audience  *ViewAudience
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ViewAudience is the audience of a shared view: every customer (All), or the
// listed customers (Customers, at least one). Exactly one of the two is set.
type ViewAudience struct {
	All       bool
	Customers []string
}

// audienceAll is the audience column's value for "all customers"; a set of
// customers is stored as a JSON array of ids.
const audienceAll = "all"

// encode is the audience column's value.
func (a ViewAudience) encode() (string, error) {
	if a.All {
		return audienceAll, nil
	}
	b, err := json.Marshal(a.Customers)
	return string(b), err
}

// decodeAudience reads the audience column. NULL is no audience (nil). A value
// that is neither "all" nor an array of ids decodes to an audience nobody but
// the owner sees (fail closed).
func decodeAudience(col sql.NullString) *ViewAudience {
	if !col.Valid {
		return nil
	}
	if col.String == audienceAll {
		return &ViewAudience{All: true}
	}
	var ids []string
	if json.Unmarshal([]byte(col.String), &ids) != nil {
		return &ViewAudience{}
	}
	return &ViewAudience{Customers: ids}
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
	// Audience is required when Shared is true and refused when it is false.
	Audience *ViewAudience
}

// SavedViewPatch is a partial update; nil fields are left alone. Making a view
// private clears its audience; making it shared needs an Audience in the patch.
type SavedViewPatch struct {
	Name     *string
	Filter   *json.RawMessage
	Columns  *json.RawMessage
	Sort     *json.RawMessage
	Shared   *bool
	Audience *ViewAudience
}

const viewCols = `v.id, v.owner_id, COALESCE(a.username, ''), v.name, v.entity, v.filter, v.columns, v.sort, v.shared, v.audience, v.created_at, v.updated_at`

const viewFrom = ` FROM saved_views v LEFT JOIN admin_users a ON a.id = v.owner_id`

func scanView(sc scanner) (SavedView, error) {
	var v SavedView
	var owner sql.NullString
	var filter, cols, sort string
	var shared int
	var audience sql.NullString
	var c, u int64
	if err := sc.Scan(&v.ID, &owner, &v.OwnerName, &v.Name, &v.Entity, &filter, &cols, &sort, &shared, &audience, &c, &u); err != nil {
		return SavedView{}, err
	}
	v.OwnerID, v.Shared = owner.String, shared != 0
	if v.Shared {
		v.Audience = decodeAudience(audience)
	}
	v.Filter, v.Columns, v.Sort = json.RawMessage(filter), json.RawMessage(cols), json.RawMessage(sort)
	v.CreatedAt, v.UpdatedAt = timeOf(c), timeOf(u)
	return v, nil
}

// visibleView reports whether viewer (an admin id) may see v under scope. The
// owner sees their own view. Anyone else sees it only when it is shared and its
// audience is inside scope: the audience all customers needs an all-customer
// scope, a set of customers needs every one of them in scope. The view's name and
// filter are not read here; a shared view without a readable audience is hidden.
func visibleView(scope Scope, viewer string, v SavedView) bool {
	if v.OwnerID != "" && v.OwnerID == viewer {
		return true
	}
	if !v.Shared || v.Audience == nil {
		return false
	}
	if v.Audience.All {
		return scope.All
	}
	if len(v.Audience.Customers) == 0 {
		return false
	}
	for _, id := range v.Audience.Customers {
		if !scope.Allows(id) {
			return false
		}
	}
	return true
}

// Fixed sentences of the audience checks; they never echo the input.
const (
	audienceNeeded  = "Say who the shared view is for: all customers, or a list of customer ids."
	audienceOneForm = "Give either all customers or a list of 1 to 50 customer ids, not both."
	audiencePrivate = "Only a shared view has an audience."
	audienceAllOnly = "Only an account that sees every customer can share a view with all customers."
	audienceUnseen  = "The audience names a customer you cannot see."
	audienceFilter  = "The filter names a customer or device outside the view's audience."
)

// cleanAudience checks the shape of an audience and returns it with its ids
// de-duplicated and sorted. It does not read the database.
func cleanAudience(a ViewAudience) (ViewAudience, []FieldError) {
	if a.All {
		if len(a.Customers) > 0 {
			return a, []FieldError{fieldf("audience", "%s", audienceOneForm)}
		}
		return ViewAudience{All: true}, nil
	}
	if len(a.Customers) == 0 || len(a.Customers) > maxFilterList {
		return a, []FieldError{fieldf("audience", "%s", audienceOneForm)}
	}
	ids := make([]string, 0, len(a.Customers))
	for _, id := range a.Customers {
		if !idToken.MatchString(id) {
			return a, []FieldError{fieldf("audience", "%s", audienceOneForm)}
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ViewAudience{Customers: ids}, nil
}

// checkAudience is the database half of the audience rules, inside the write:
// the audience lies inside the sharer's scope (every customer exists and is in
// it; all customers needs an all-customer sharer), and everything the filter
// names lies inside the audience. A failure is a *ValidationError.
func checkAudience(tx *Tx, sharer Scope, a ViewAudience, entity string, filter json.RawMessage) error {
	if a.All {
		if !sharer.All {
			return validation([]FieldError{fieldf("audience", "%s", audienceAllOnly)})
		}
		return nil // every customer is inside "all customers"
	}
	for _, id := range a.Customers {
		if !sharer.Allows(id) {
			return validation([]FieldError{fieldf("audience", "%s", audienceUnseen)})
		}
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM customers WHERE id = ?`, id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// The same sentence as a customer out of scope: no existence oracle.
				return validation([]FieldError{fieldf("audience", "%s", audienceUnseen)})
			}
			return err
		}
	}
	refs, why, ok := parseViewFilter(entity, filter)
	if !ok {
		return validation([]FieldError{viewFilterError(why)})
	}
	in, err := refs.allInScope(tx, ScopeOf(a.Customers...))
	if err != nil {
		return err
	}
	if !in {
		return validation([]FieldError{fieldf("filter", "%s", audienceFilter)})
	}
	return nil
}

// validateView checks the user-supplied parts. Rejected input is a
// *ValidationError whose sentences never echo the input.
func validateView(name *string, entity string, filter, columns, sort *json.RawMessage) []FieldError {
	var bad []FieldError
	if name != nil {
		if n, ok := cleanLine(*name, 100); !ok || n == "" {
			bad = append(bad, fieldf("name", "Use a name of 1 to 100 characters on one line."))
		} else if looksLikePath(n) {
			bad = append(bad, fieldf("name", "Name the view in words, without a file path."))
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
	if len(bad) == 0 {
		bad = append(bad, validateViewShape(columns, sort)...)
		if entity != "" && filter != nil && len(*filter) > 0 {
			if _, why, ok := parseViewFilter(entity, *filter); !ok {
				bad = append(bad, viewFilterError(why))
			}
		}
	}
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
	out := []SavedView{}
	err := s.Read(ctx, func(tx *Tx) error {
		all, err := collect(tx, q+` ORDER BY v.entity, v.name COLLATE NOCASE, v.id`, args, scanView)
		if err != nil {
			return err
		}
		for _, v := range all {
			if visibleView(scope, viewer, v) {
				out = append(out, v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
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
		if v, err = one(tx, `SELECT `+viewCols+viewFrom+` WHERE v.id = ?`, []any{id}, scanView); err != nil {
			return err
		}
		if !visibleView(scope, viewer, v) {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return SavedView{}, err
	}
	return v, nil
}

// CreateSavedViewAudited makes a view owned by actor and writes one "view.create"
// audit row in the same transaction. A shared view needs an audience inside the
// caller's scope that holds everything its filter names (*ValidationError
// otherwise, see the package comment above). A second view of the same owner,
// entity and name is ErrConflict.
func (s *Store) CreateSavedViewAudited(ctx context.Context, scope Scope, in SavedViewInput, actor AuditActor) (SavedView, error) {
	if err := scope.check(); err != nil {
		return SavedView{}, err
	}
	name := in.Name
	bad := validateView(&name, in.Entity, &in.Filter, &in.Columns, &in.Sort)
	if in.Entity == "" {
		bad = append(bad, fieldf("entity", "Use devices, runs or customers."))
	}
	var aud ViewAudience
	switch {
	case in.Shared && in.Audience == nil:
		bad = append(bad, fieldf("audience", "%s", audienceNeeded))
	case !in.Shared && in.Audience != nil:
		bad = append(bad, fieldf("audience", "%s", audiencePrivate))
	case in.Shared:
		var abad []FieldError
		aud, abad = cleanAudience(*in.Audience)
		bad = append(bad, abad...)
	}
	if err := validation(bad); err != nil {
		return SavedView{}, err
	}
	def := func(raw json.RawMessage, d string) string {
		if len(raw) == 0 {
			return d
		}
		return string(raw)
	}
	var out SavedView
	err := s.Write(ctx, func(tx *Tx) error {
		var audCol any // NULL: a private view has no audience
		if in.Shared {
			if err := checkAudience(tx, scope, aud, in.Entity, json.RawMessage(def(in.Filter, "{}"))); err != nil {
				return err
			}
			enc, err := aud.encode()
			if err != nil {
				return err
			}
			audCol = enc
		}
		now, id := tx.nowMS(), tx.id()
		if _, err := tx.Exec(`INSERT INTO saved_views (id, owner_id, name, entity, filter, columns, sort, shared, audience, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, id, nullStr(actor.AdminID), name, in.Entity, def(in.Filter, "{}"), def(in.Columns, "[]"),
			def(in.Sort, "[]"), boolInt(in.Shared), audCol, now, now); err != nil {
			return err
		}
		ev, err := actor.event("view.create", "saved_view", id, "", map[string]any{"name": name, "entity": in.Entity, "shared": in.Shared, "audience": auditAudience(in.Shared, aud)})
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
// shared needs an audience in the patch; a shared view's audience is checked
// again (against the caller's scope and the filter) whenever the audience, the
// filter or the sharing changes. Making a view private clears its audience. A
// patch that changes nothing writes nothing (changed false).
func (s *Store) UpdateSavedViewAudited(ctx context.Context, scope Scope, id string, p SavedViewPatch, actor AuditActor, asAdmin bool) (v SavedView, changed bool, err error) {
	if err := scope.check(); err != nil {
		return SavedView{}, false, err
	}
	bad := validateView(p.Name, "", p.Filter, p.Columns, p.Sort)
	var newAud ViewAudience
	if p.Audience != nil {
		var abad []FieldError
		newAud, abad = cleanAudience(*p.Audience)
		bad = append(bad, abad...)
	}
	if len(bad) > 0 {
		return SavedView{}, false, validation(bad)
	}
	err = s.Write(ctx, func(tx *Tx) error {
		cur, err := editableView(tx, scope, actor, id, asAdmin)
		if err != nil {
			return err
		}
		if p.Filter != nil && len(*p.Filter) > 0 {
			if _, why, ok := parseViewFilter(cur.Entity, *p.Filter); !ok {
				return validation([]FieldError{viewFilterError(why)})
			}
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
		// The audience of the view after the patch.
		shared := cur.Shared
		if p.Shared != nil {
			shared = *p.Shared
		}
		switch {
		case !shared && p.Audience != nil:
			return validation([]FieldError{fieldf("audience", "%s", audiencePrivate)})
		case !shared:
			if cur.Audience != nil {
				set("audience", "audience", nil)
			}
		default:
			aud := cur.Audience
			if p.Audience != nil {
				aud = &newAud
			}
			if aud == nil || (!cur.Shared && p.Audience == nil) {
				return validation([]FieldError{fieldf("audience", "%s", audienceNeeded)})
			}
			filter := cur.Filter
			if p.Filter != nil {
				filter = *p.Filter
			}
			if p.Audience != nil || !same(p.Filter, cur.Filter) || !cur.Shared {
				if err := checkAudience(tx, scope, *aud, cur.Entity, filter); err != nil {
					return err
				}
			}
			if p.Audience != nil && !sameAudience(cur.Audience, aud) {
				enc, err := aud.encode()
				if err != nil {
					return err
				}
				set("audience", "audience", enc)
			}
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

// sameAudience reports whether two audiences are the same (both nil, both all,
// or the same ids; ids are kept sorted).
func sameAudience(a, b *ViewAudience) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.All == b.All && slices.Equal(a.Customers, b.Customers)
}

// auditAudience is the audience as the audit row records it: "all", the number
// of customers, or "none" for a private view. Customer ids stay out of the
// details (the row is the sharer's, not any one customer's).
func auditAudience(shared bool, a ViewAudience) any {
	switch {
	case !shared:
		return "none"
	case a.All:
		return audienceAll
	}
	return len(a.Customers)
}
