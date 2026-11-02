package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// Per-device policy overrides (ticket M2-W2A, architecture §3.2 table 20 and
// §5.2 "devices/{id}/override"): a sparse RFC 7386 patch over the nine sections
// with a mandatory reason and an optional expiry. History is kept: setting a new
// override clears the old row (cleared_at) and inserts another; clearing sets
// cleared_at. One active row per device is a database rule.

// maxOverridePatch bounds the stored patch.
const maxOverridePatch = 16 << 10

// DeviceOverride is one row of device_policy_overrides.
type DeviceOverride struct {
	ID        string
	DeviceID  string
	Patch     json.RawMessage
	Reason    string
	SetBy     string
	SetByName string
	ExpiresAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	// Expired is true when ExpiresAt has passed: the row is still uncleared, but
	// the agent no longer sees it (the composer ignores it).
	Expired bool
}

// OverrideInput sets an override.
type OverrideInput struct {
	Patch     json.RawMessage
	Reason    string
	ExpiresAt *time.Time
}

const overrideCols = `o.id, o.device_id, o.patch, o.reason, o.set_by, COALESCE(u.username, ''), o.expires_at, o.created_at, o.updated_at`

func scanOverride(now int64) func(scanner) (DeviceOverride, error) {
	return func(sc scanner) (DeviceOverride, error) {
		var o DeviceOverride
		var by sql.NullString
		var exp sql.NullInt64
		var patch string
		var c, u int64
		if err := sc.Scan(&o.ID, &o.DeviceID, &patch, &o.Reason, &by, &o.SetByName, &exp, &c, &u); err != nil {
			return DeviceOverride{}, err
		}
		o.Patch, o.SetBy, o.ExpiresAt, o.CreatedAt, o.UpdatedAt = json.RawMessage(patch), by.String, ptrTime(exp), timeOf(c), timeOf(u)
		o.Expired = o.ExpiresAt != nil && o.ExpiresAt.UnixMilli() <= now
		return o, nil
	}
}

// deviceCustomerPolicy resolves a device inside scope: its customer, whether it
// is retired, and the customer's policy id.
func deviceCustomerPolicy(tx *Tx, scope Scope, deviceID string) (customerID, customerName, policyID string, retired bool, err error) {
	var pol sql.NullString
	var ret sql.NullInt64
	err = tx.QueryRow(`SELECT d.customer_id, c.name, c.policy_id, d.retired_at FROM devices d JOIN customers c ON c.id = d.customer_id WHERE d.id = ?`,
		deviceID).Scan(&customerID, &customerName, &pol, &ret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, ErrNotFound
	}
	if err != nil {
		return "", "", "", false, err
	}
	if !scope.Allows(customerID) {
		return "", "", "", false, ErrNotFound
	}
	return customerID, customerName, pol.String, ret.Valid, nil
}

func activeOverride(tx *Tx, deviceID string) (*DeviceOverride, error) {
	rows, err := collect(tx, `SELECT `+overrideCols+` FROM device_policy_overrides o LEFT JOIN admin_users u ON u.id = o.set_by
		WHERE o.device_id = ? AND o.cleared_at IS NULL`, []any{deviceID}, scanOverride(tx.nowMS()))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// GetDeviceOverride returns the device's uncleared override, or nil when it has
// none. A device outside scope is ErrNotFound.
func (s *Store) GetDeviceOverride(ctx context.Context, scope Scope, deviceID string) (*DeviceOverride, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	var out *DeviceOverride
	err := s.Read(ctx, func(tx *Tx) error {
		if _, _, _, _, err := deviceCustomerPolicy(tx, scope, deviceID); err != nil {
			return err
		}
		var err error
		out, err = activeOverride(tx, deviceID)
		return err
	})
	return out, err
}

// overrideSchema is a document whose every settable field is present, used to
// reject an override that names a field the policy does not have.
func overrideSchema() (any, error) {
	p := policy.Default(policy.ClassUnknown)
	p.Retention.Rules = &policy.KeepCounts{}
	b, err := policy.Canonical(p)
	if err != nil {
		return nil, err
	}
	return decodeTree(b)
}

// fieldOr names the field of an error: the fixed field, or the path when none is given.
func fieldOr(field, path string) string {
	if field != "" {
		return field
	}
	return path
}

// checkOverrideShape reports every field in patch that the policy document does
// not have. Sections are objects; selection.path_flags is a map keyed by root.
func checkOverrideShape(patch, schema any, path, field string, errs *[]FieldError) {
	pm, ok := patch.(map[string]any)
	if !ok {
		return
	}
	sm, _ := schema.(map[string]any)
	for k, v := range pm {
		p := joinPath(path, k)
		sv, known := sm[k]
		if !known {
			*errs = append(*errs, fieldf(fieldOr(field, p), "%s is not a policy field.", p))
			continue
		}
		if p == "selection.path_flags" {
			continue
		}
		if _, nested := v.(map[string]any); nested {
			if _, isObj := sv.(map[string]any); !isObj {
				*errs = append(*errs, fieldf(fieldOr(field, p), "%s is not a section; set its fields.", p))
				continue
			}
			checkOverrideShape(v, sv, p, field, errs)
		}
	}
}

// validateOverrideInput checks the patch and reason without the database.
func validateOverrideInput(in OverrideInput, now time.Time) (patch json.RawMessage, reason string, err error) {
	var errs []FieldError
	reason, ok := cleanLine(in.Reason, 200)
	if !ok || reason == "" {
		errs = append(errs, fieldf("reason", "Say why this device differs from its policy (1 to 200 characters, one line)."))
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		errs = append(errs, fieldf("expires_at", "Choose a time in the future, or leave it empty for no expiry."))
	}
	if len(in.Patch) > maxOverridePatch {
		errs = append(errs, fieldf("patch", "The override is too large."))
	}
	if len(errs) == 0 {
		t, derr := decodeTree(in.Patch)
		top, isObj := t.(map[string]any)
		switch {
		case derr != nil || !isObj:
			errs = append(errs, fieldf("patch", "The override must be a JSON object of policy sections."))
		case len(top) == 0:
			errs = append(errs, fieldf("patch", "Choose at least one field to override, or clear the override."))
		default:
			schema, serr := overrideSchema()
			if serr != nil {
				return nil, "", serr
			}
			for k, v := range top {
				if _, obj := v.(map[string]any); !obj {
					errs = append(errs, fieldf("patch", "%s must be an object of fields.", k))
				}
			}
			if len(errs) == 0 {
				checkOverrideShape(t, schema, "", "patch", &errs)
			}
			if len(errs) == 0 {
				var cerr error
				if patch, cerr = encodeTree(t); cerr != nil {
					return nil, "", cerr
				}
			}
		}
	}
	if err := validation(errs); err != nil {
		return nil, "", err
	}
	return patch, reason, nil
}

// SetDeviceOverride sets or replaces the device's override and writes one
// device.override_set audit row naming the overridden fields (not their values
// and not the reason) in the same transaction. The patch composed over the
// device's default ⊕ customer policy must validate: otherwise the problems come
// back as a *ValidationError keyed by document path. Setting the very same
// override again is a no-op (changed=false, no audit row). A retired device is
// ErrConflict; a device outside scope ErrNotFound.
func (s *Store) SetDeviceOverride(ctx context.Context, scope Scope, deviceID string, in OverrideInput, actor AuditActor) (DeviceOverride, bool, error) {
	if err := scope.check(); err != nil {
		return DeviceOverride{}, false, err
	}
	patch, reason, err := validateOverrideInput(in, s.Now())
	if err != nil {
		// A bad input still has to be a 404 for a device the caller cannot see.
		if rerr := s.Read(ctx, func(tx *Tx) error { _, _, _, _, e := deviceCustomerPolicy(tx, scope, deviceID); return e }); rerr != nil {
			return DeviceOverride{}, false, rerr
		}
		return DeviceOverride{}, false, err
	}
	var out DeviceOverride
	changed := false
	err = s.Write(ctx, func(tx *Tx) error {
		custID, _, polID, retired, err := deviceCustomerPolicy(tx, scope, deviceID)
		if err != nil {
			return err
		}
		if retired {
			return conflictf("device is retired")
		}
		def, _, err := loadDefaultSections(tx)
		if err != nil {
			return err
		}
		var sparse map[string]json.RawMessage
		if polID != "" {
			if sparse, err = sectionsOf(tx, polID); err != nil {
				return err
			}
		}
		if _, err := composeLayers(def, sparse, string(patch)); err != nil {
			if fe := PolicyProblems(err); len(fe) > 0 {
				return validation(fe)
			}
			return validation([]FieldError{fieldf("patch", "A value in the override has the wrong type for its field.")})
		}
		cur, err := activeOverride(tx, deviceID)
		if err != nil {
			return err
		}
		if cur != nil && !cur.Expired && sameTree2(cur.Patch, patch) && cur.Reason == reason && sameTime(cur.ExpiresAt, in.ExpiresAt) {
			out = *cur
			return nil
		}
		now := tx.nowMS()
		if cur != nil {
			if _, err := tx.Exec(`UPDATE device_policy_overrides SET cleared_at = ?, updated_at = ? WHERE id = ?`, now, now, cur.ID); err != nil {
				return mapErr(err)
			}
		}
		id := tx.id()
		var exp any
		if in.ExpiresAt != nil {
			exp = in.ExpiresAt.UTC().UnixMilli()
		}
		if _, err := tx.Exec(`INSERT INTO device_policy_overrides (id, device_id, patch, reason, set_by, expires_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, id, deviceID, string(patch), reason, nullStr(actor.AdminID), exp, now, now); err != nil {
			return mapErr(err)
		}
		sorted := pathChanges(OverriddenPaths(patch))
		details := map[string]any{"fields": changedFieldNames(sorted), "replaced": cur != nil, "expires": in.ExpiresAt != nil}
		ev, err := actor.event("device.override_set", "device", deviceID, custID, details)
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		changed = true
		got, err := activeOverride(tx, deviceID)
		if err != nil || got == nil {
			return errors.New("store: the override just written is missing")
		}
		out = *got
		return nil
	})
	if err != nil {
		return DeviceOverride{}, false, err
	}
	return out, changed, nil
}

func sameTree2(a, b json.RawMessage) bool {
	x, err1 := decodeTree(a)
	y, err2 := decodeTree(b)
	return err1 == nil && err2 == nil && sameTree(x, y)
}

// ClearDeviceOverride clears the device's override (history is kept) and writes
// one device.override_clear audit row. A device with no override is a no-op
// (changed=false, no audit row), so a repeated DELETE is safe.
func (s *Store) ClearDeviceOverride(ctx context.Context, scope Scope, deviceID string, actor AuditActor) (bool, error) {
	if err := scope.check(); err != nil {
		return false, err
	}
	changed := false
	err := s.Write(ctx, func(tx *Tx) error {
		custID, _, _, _, err := deviceCustomerPolicy(tx, scope, deviceID)
		if err != nil {
			return err
		}
		cur, err := activeOverride(tx, deviceID)
		if err != nil || cur == nil {
			return err
		}
		now := tx.nowMS()
		if _, err := tx.Exec(`UPDATE device_policy_overrides SET cleared_at = ?, updated_at = ? WHERE id = ?`, now, now, cur.ID); err != nil {
			return mapErr(err)
		}
		ev, err := actor.event("device.override_clear", "device", deviceID, custID, map[string]any{"fields": changedFieldNames(pathChanges(OverriddenPaths(cur.Patch))), "expired": cur.Expired})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// DeviceEffective is the effective policy of a device with the source of every
// field, and the override that contributes to it.
type DeviceEffective struct {
	DeviceID     string
	CustomerID   string
	CustomerName string
	EffectiveView
	// Override is the uncleared override row (possibly expired), nil for none.
	Override *DeviceOverride
}

// overridePatchDoc is the single-document patch an override stands for, with
// the two unions normalised exactly as the composer does.
func overridePatchDoc(patch json.RawMessage) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(patch, &top); err != nil {
		return nil, err
	}
	norm := make(map[string]json.RawMessage, len(top))
	for name, raw := range top {
		norm[name] = normalizeSection(name, raw)
	}
	return json.Marshal(norm)
}

// GetDeviceEffective composes the device's effective policy (default, then the
// customer's policy, then an unexpired override) and labels every field with
// the layer that set it. The ETag and version are those the agent pulls.
func (s *Store) GetDeviceEffective(ctx context.Context, scope Scope, deviceID string) (DeviceEffective, error) {
	if err := scope.check(); err != nil {
		return DeviceEffective{}, err
	}
	var out DeviceEffective
	err := s.Read(ctx, func(tx *Tx) error {
		custID, custName, polID, _, err := deviceCustomerPolicy(tx, scope, deviceID)
		if err != nil {
			return err
		}
		out.DeviceID, out.CustomerID, out.CustomerName = deviceID, custID, custName
		def, defRec, err := loadDefaultSections(tx)
		if err != nil {
			return err
		}
		out.Default = defRec
		out.PolicyID, out.Version, out.PolicyName, out.PolicyScope = defRec.ID, defRec.Version, defRec.Name, "default"
		var sparse map[string]json.RawMessage
		var sparsePatch json.RawMessage
		if polID != "" {
			rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{polID}, scanPolicy)
			if err != nil {
				return err
			}
			sparse = rec.Sections
			if sparsePatch, err = sectionsPatch(sparse); err != nil {
				return err
			}
			out.PolicyID, out.Version, out.PolicyName, out.PolicyScope = rec.ID, rec.Version, rec.Name, rec.Scope
		}
		if out.Override, err = activeOverride(tx, deviceID); err != nil {
			return err
		}
		var patch string
		var opatch json.RawMessage
		if out.Override != nil && !out.Override.Expired {
			patch = string(out.Override.Patch)
			if opatch, err = overridePatchDoc(out.Override.Patch); err != nil {
				return invalidf("device override: %v", err)
			}
		}
		docs, err := composeLayers(def, sparse, patch)
		if err != nil {
			return err
		}
		out.Policy = docs.Parsed
		if out.ETag, err = policy.ETag(docs.Parsed); err != nil {
			return err
		}
		out.Fields, err = provenance(docs, sparsePatch, opatch)
		return err
	})
	return out, err
}

func pathChanges(paths []string) []FieldChange {
	out := make([]FieldChange, len(paths))
	for i, p := range paths {
		out[i] = FieldChange{Path: p}
	}
	return out
}

// OverriddenPaths lists the dotted paths an override patch sets (leaves), in
// section order, for display.
func OverriddenPaths(patch json.RawMessage) []string {
	flat, err := FlattenDocument(patch)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sortPaths(paths)
	return paths
}
