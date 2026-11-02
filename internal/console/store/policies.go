package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// ErrNoDefaultPolicy means the policies table has no scope='default' row, so no
// effective policy can be composed. The init wizard creates it; the facts
// loader reports every device UNKNOWN (loud) rather than guessing thresholds.
var ErrNoDefaultPolicy = errors.New("store: no default policy configured")

// PolicyRecord is one row of policies. Sections holds the nine JSON sections
// exactly as stored: the default and template rows are complete documents, a
// customer row may be sparse (a merge patch over the default, DH6).
type PolicyRecord struct {
	ID                  string
	Scope               string // default | template | customer
	CustomerID          string // set iff Scope is customer
	Name                string
	Description         string
	TemplateDeviceClass string // laptop | desktop, templates only
	Version             int
	Sections            map[string]json.RawMessage
	SourceID            string
	CreatedBy           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Document decodes the stored sections tolerantly (missing fields take the
// proposed defaults). For a sparse customer row this is the row read in
// isolation; the composed policy for a device is EffectivePolicyFor.
func (r PolicyRecord) Document() (policy.Policy, error) { return policy.Decode(r.Sections) }

// PolicyInput creates a policy. Sections not present are stored as {}.
type PolicyInput struct {
	Scope               string
	CustomerID          string
	Name                string
	Description         string
	TemplateDeviceClass string
	Sections            map[string]json.RawMessage
	SourceID            string
	CreatedBy           string // admin id, "" for system
	Note                string // written to the first revision
}

// PolicyUpdate edits a policy. Sections present replace the stored section
// wholesale; absent ones are untouched. ExpectedVersion > 0 makes the update
// conditional (ErrConflict when the stored version moved).
type PolicyUpdate struct {
	ExpectedVersion int
	Name            *string
	Description     *string
	Sections        map[string]json.RawMessage
	ChangedBy       string
	Note            string
}

// PolicySections splits a Policy into the per-section JSON the table stores
// (a thin wrapper over policy.Encode so callers need not import both forms).
func PolicySections(p policy.Policy) (map[string]json.RawMessage, error) { return policy.Encode(p) }

const policyCols = `id, scope, customer_id, name, description, template_device_class, version, schedule, conditions, selection,
	retention, throttle, sla, maintenance, agent_permissions, notification_overrides, source_id, created_by, created_at, updated_at`

func scanPolicy(sc scanner) (PolicyRecord, error) {
	var r PolicyRecord
	var cust, desc, class, src, by sql.NullString
	var secs [9]string
	var c, u int64
	err := sc.Scan(&r.ID, &r.Scope, &cust, &r.Name, &desc, &class, &r.Version,
		&secs[0], &secs[1], &secs[2], &secs[3], &secs[4], &secs[5], &secs[6], &secs[7], &secs[8], &src, &by, &c, &u)
	if err != nil {
		return PolicyRecord{}, err
	}
	r.CustomerID, r.Description, r.TemplateDeviceClass = cust.String, desc.String, class.String
	r.SourceID, r.CreatedBy = src.String, by.String
	r.Sections = make(map[string]json.RawMessage, 9)
	for i, name := range policy.SectionNames {
		r.Sections[name] = json.RawMessage(secs[i])
	}
	r.CreatedAt, r.UpdatedAt = timeOf(c), timeOf(u)
	return r, nil
}

// sectionArgs returns the nine section texts in column order, validating names and JSON.
func sectionArgs(sections map[string]json.RawMessage) ([9]any, error) {
	var out [9]any
	for name := range sections {
		known := false
		for _, n := range policy.SectionNames {
			if n == name {
				known = true
			}
		}
		if !known {
			return out, invalidf("unknown policy section %q", name)
		}
	}
	for i, name := range policy.SectionNames {
		raw := sections[name]
		if len(bytes.TrimSpace(raw)) == 0 {
			out[i] = "{}"
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return out, invalidf("policy section %q is not a JSON object", name)
		}
		out[i] = string(raw)
	}
	return out, nil
}

// normalizeSection makes a section patch behave as a replacement of the two
// "one of" unions the policy document has, so that a customer policy naming
// retention rules does not inherit the default's preset (and vice versa), and a
// selection naming roots or presets does not inherit the other list. This is
// the same rule policy.Decode applies to a tolerant read.
func normalizeSection(name string, raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return raw
	}
	has := func(k string) bool { v, ok := obj[k]; return ok && string(bytes.TrimSpace(v)) != "null" }
	switch name {
	case "retention":
		_, presetKey := obj["preset"]
		_, rulesKey := obj["rules"]
		if has("rules") && !presetKey {
			obj["preset"] = json.RawMessage("null")
		}
		if has("preset") && !rulesKey {
			obj["rules"] = json.RawMessage("null")
		}
	case "selection":
		_, rootsKey := obj["roots"]
		_, presetsKey := obj["presets"]
		if (rootsKey || presetsKey) && !(rootsKey && presetsKey) {
			if !rootsKey {
				obj["roots"] = json.RawMessage("[]")
			}
			if !presetsKey {
				obj["presets"] = json.RawMessage("[]")
			}
		}
	default:
		return raw
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return b
}

func isEmptyObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "{}" || string(t) == "null"
}

// applySections merges per-section patches onto a single-document form.
func applySections(doc json.RawMessage, sections map[string]json.RawMessage) (json.RawMessage, error) {
	patch := make(map[string]json.RawMessage)
	for name, raw := range sections {
		if isEmptyObject(raw) {
			continue
		}
		patch[name] = normalizeSection(name, raw)
	}
	if len(patch) == 0 {
		return doc, nil
	}
	pb, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	return policy.MergePatch(doc, pb)
}

// applyOverride merges a device override patch, which is already in the
// single-document form {"sla":{...}}.
func applyOverride(doc json.RawMessage, patch json.RawMessage) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(patch, &top); err != nil {
		return nil, err
	}
	norm := make(map[string]json.RawMessage, len(top))
	for name, raw := range top {
		norm[name] = normalizeSection(name, raw)
	}
	pb, err := json.Marshal(norm)
	if err != nil {
		return nil, err
	}
	return policy.MergePatch(doc, pb)
}

// composer builds effective policies inside one transaction, caching the
// default and customer layers so a fleet-wide load composes each layer once.
type composer struct {
	tx      *Tx
	haveDef bool
	defDoc  json.RawMessage
	defVer  int
	defID   string
	cust    map[string]composedLayer // keyed by customers.policy_id ("" = none)
}

type composedLayer struct {
	doc json.RawMessage
	ver int
	id  string // the policy document this layer's version belongs to
}

func newComposer(tx *Tx) (*composer, error) {
	c := &composer{tx: tx, cust: map[string]composedLayer{}}
	rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE scope = 'default'`, nil, scanPolicy)
	if errors.Is(err, ErrNotFound) {
		return c, nil // haveDef stays false; effective() reports ErrNoDefaultPolicy
	}
	if err != nil {
		return nil, err
	}
	p, err := policy.Decode(rec.Sections)
	if err != nil {
		return nil, invalidf("default policy: %v", err)
	}
	doc, err := policy.Canonical(p)
	if err != nil {
		return nil, err
	}
	c.haveDef, c.defDoc, c.defVer, c.defID = true, doc, rec.Version, rec.ID
	return c, nil
}

// layer returns default ⊕ the customer's policy (if any).
func (c *composer) layer(customerPolicyID string) (composedLayer, error) {
	if !c.haveDef {
		return composedLayer{}, ErrNoDefaultPolicy
	}
	if customerPolicyID == "" {
		return composedLayer{doc: c.defDoc, ver: c.defVer, id: c.defID}, nil
	}
	if l, ok := c.cust[customerPolicyID]; ok {
		return l, nil
	}
	rec, err := one(c.tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{customerPolicyID}, scanPolicy)
	if err != nil {
		return composedLayer{}, fmt.Errorf("customer policy %s: %w", customerPolicyID, err)
	}
	doc, err := applySections(c.defDoc, rec.Sections)
	if err != nil {
		return composedLayer{}, invalidf("customer policy: %v", err)
	}
	l := composedLayer{doc: doc, ver: rec.Version, id: rec.ID}
	c.cust[customerPolicyID] = l
	return l, nil
}

// effective composes default ⊕ customer policy ⊕ the device's active override.
func (c *composer) effective(deviceID, customerPolicyID string) (policy.Policy, int, string, error) {
	e, err := c.effectiveDoc(deviceID, customerPolicyID)
	return e.Policy, e.Version, e.ETag, err
}

// effectiveDoc is effective plus the id of the document the version belongs to.
func (c *composer) effectiveDoc(deviceID, customerPolicyID string) (EffectivePolicy, error) {
	l, err := c.layer(customerPolicyID)
	if err != nil {
		return EffectivePolicy{}, err
	}
	doc := l.doc
	var patch string
	err = c.tx.QueryRow(`SELECT patch FROM device_policy_overrides
		WHERE device_id = ? AND cleared_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
		deviceID, c.tx.nowMS()).Scan(&patch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return EffectivePolicy{}, err
	default:
		if doc, err = applyOverride(doc, json.RawMessage(patch)); err != nil {
			return EffectivePolicy{}, invalidf("device override: %v", err)
		}
	}
	p, err := policy.DecodeDocument(doc, false)
	if err != nil {
		return EffectivePolicy{}, invalidf("effective policy: %v", err)
	}
	if err := p.Validate(); err != nil {
		return EffectivePolicy{}, invalidf("effective policy: %v", err)
	}
	etag, err := policy.ETag(p)
	if err != nil {
		return EffectivePolicy{}, err
	}
	return EffectivePolicy{Policy: p, PolicyID: l.id, Version: l.ver, ETag: etag}, nil
}

// EffectivePolicyFor composes the policy a device runs under (DH6): the default
// policy, then the customer's policy (customers.policy_id: a customer row or a
// template), then the device's active override, each as an RFC 7386 merge patch
// over the nine sections. It returns the policy, the version of the customer
// (else default) document, which is what devices.policy_version_applied
// records, and the strong ETag of the composed result, which is what the agent
// sends in If-None-Match. The override changes the ETag but not the version.
//
// A device whose composed policy fails policy.Validate is an error (ErrInvalid):
// the agent must never be served a policy the engine would reject.
func (s *Store) EffectivePolicyFor(ctx context.Context, scope Scope, deviceID string) (policy.Policy, int, string, error) {
	e, err := s.EffectivePolicyDoc(ctx, scope, deviceID)
	return e.Policy, e.Version, e.ETag, err
}

// EffectivePolicy is a composed device policy and what identifies it: the id of
// the policy document (the customer's own, else the default) whose Version it
// carries, so the agent can tag a run with policy_id and policy_version. A device
// override changes ETag only, never PolicyID or Version.
type EffectivePolicy struct {
	Policy   policy.Policy
	PolicyID string // 26-character ULID of the customer-or-default policy document
	Version  int
	ETag     string
}

// EffectivePolicyDoc is EffectivePolicyFor returning the document id as well.
func (s *Store) EffectivePolicyDoc(ctx context.Context, scope Scope, deviceID string) (EffectivePolicy, error) {
	if err := scope.check(); err != nil {
		return EffectivePolicy{}, err
	}
	var e EffectivePolicy
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		e, err = effectiveFor(tx, scope, deviceID)
		return err
	})
	return e, err
}

// AgentEffectivePolicy is EffectivePolicyFor for the authenticated device itself
// (the agent policy pull): the device id comes from the token, not from a Scope.
func (s *Store) AgentEffectivePolicy(ctx context.Context, deviceID string) (policy.Policy, int, string, error) {
	e, err := s.AgentEffectivePolicyDoc(ctx, deviceID)
	return e.Policy, e.Version, e.ETag, err
}

// AgentEffectivePolicyDoc is AgentEffectivePolicy returning the document id as well.
func (s *Store) AgentEffectivePolicyDoc(ctx context.Context, deviceID string) (EffectivePolicy, error) {
	var e EffectivePolicy
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		e, err = effectiveFor(tx, ScopeAll(), deviceID)
		return err
	})
	return e, err
}

func effectiveFor(tx *Tx, scope Scope, deviceID string) (EffectivePolicy, error) {
	var customerID string
	var customerPolicy sql.NullString
	err := tx.QueryRow(`SELECT d.customer_id, c.policy_id FROM devices d JOIN customers c ON c.id = d.customer_id WHERE d.id = ?`,
		deviceID).Scan(&customerID, &customerPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return EffectivePolicy{}, ErrNotFound
	}
	if err != nil {
		return EffectivePolicy{}, err
	}
	if !scope.Allows(customerID) {
		return EffectivePolicy{}, ErrNotFound
	}
	c, err := newComposer(tx)
	if err != nil {
		return EffectivePolicy{}, err
	}
	return c.effectiveDoc(deviceID, customerPolicy.String)
}

// validateComposed checks that sections, applied over the default policy (the
// stored one, else the proposed defaults), give a valid policy.
func validateComposed(tx *Tx, scopeKind string, sections map[string]json.RawMessage) error {
	base := policy.Default(policy.ClassUnknown)
	if rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE scope = 'default'`, nil, scanPolicy); err == nil {
		if p, derr := policy.Decode(rec.Sections); derr == nil {
			base = p
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	doc, err := policy.Canonical(base)
	if err != nil {
		return err
	}
	if scopeKind == "default" {
		// A default row is complete in its own right: judge it on top of the proposed defaults only.
		if doc, err = policy.Canonical(policy.Default(policy.ClassUnknown)); err != nil {
			return err
		}
	}
	merged, err := applySections(doc, sections)
	if err != nil {
		return invalidf("policy sections: %v", err)
	}
	p, err := policy.DecodeDocument(merged, false)
	if err != nil {
		return invalidf("policy sections: %v", err)
	}
	if err := p.Validate(); err != nil {
		return invalidf("policy invalid: %v", err)
	}
	return nil
}

// revisionBody is the JSON document stored in policy_revisions.body: the nine
// stored sections of that version.
func revisionBody(sections map[string]json.RawMessage) (string, error) {
	norm := make(map[string]json.RawMessage, 9)
	for _, n := range policy.SectionNames {
		raw := sections[n]
		if len(bytes.TrimSpace(raw)) == 0 {
			raw = json.RawMessage("{}")
		}
		norm[n] = raw
	}
	b, err := json.Marshal(norm)
	return string(b), err
}

// CreatePolicy inserts a policy and its first revision in one transaction.
// A default or template policy is global, so it needs scope.All (ErrScope
// otherwise); a customer policy needs the customer inside scope (ErrNotFound
// otherwise). The sections, applied over the default policy, must validate
// (ErrInvalid carries the problems).
func (s *Store) CreatePolicy(ctx context.Context, scope Scope, in PolicyInput) (PolicyRecord, error) {
	if err := scope.check(); err != nil {
		return PolicyRecord{}, err
	}
	switch in.Scope {
	case "default", "template":
		if !scope.All {
			return PolicyRecord{}, ErrScope
		}
	case "customer":
		if in.CustomerID == "" {
			return PolicyRecord{}, invalidf("a customer policy needs a customer id")
		}
		if !scope.Allows(in.CustomerID) {
			return PolicyRecord{}, ErrNotFound
		}
	default:
		return PolicyRecord{}, invalidf("policy scope must be default, template or customer")
	}
	args, err := sectionArgs(in.Sections)
	if err != nil {
		return PolicyRecord{}, err
	}
	var out PolicyRecord
	err = s.Write(ctx, func(tx *Tx) error {
		if err := validateComposed(tx, in.Scope, in.Sections); err != nil {
			return err
		}
		id, now := tx.id(), tx.nowMS()
		if _, err := tx.Exec(`INSERT INTO policies (`+policyCols+`) VALUES (?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, in.Scope, nullStr(in.CustomerID), in.Name, nullStr(in.Description), nullStr(in.TemplateDeviceClass),
			args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7], args[8],
			nullStr(in.SourceID), nullStr(in.CreatedBy), now, now); err != nil {
			return err
		}
		body, err := revisionBody(in.Sections)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO policy_revisions (id, policy_id, version, body, changed_by, change_note, created_at)
			VALUES (?, ?, 1, ?, ?, ?, ?)`, tx.id(), id, body, nullStr(in.CreatedBy), nullStr(in.Note), now); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{id}, scanPolicy)
		return err
	})
	return out, err
}

// readablePolicy reports whether scope may read rec: default and template
// policies are visible to every valid scope; a customer policy only inside it.
func readablePolicy(scope Scope, rec PolicyRecord) bool {
	return rec.Scope != "customer" || scope.Allows(rec.CustomerID)
}

// GetPolicy returns a policy by id (ErrNotFound when it is a customer policy outside scope).
func (s *Store) GetPolicy(ctx context.Context, scope Scope, id string) (PolicyRecord, error) {
	if err := scope.check(); err != nil {
		return PolicyRecord{}, err
	}
	var out PolicyRecord
	err := s.Read(ctx, func(tx *Tx) error {
		rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{id}, scanPolicy)
		if err != nil {
			return err
		}
		if !readablePolicy(scope, rec) {
			return ErrNotFound
		}
		out = rec
		return nil
	})
	return out, err
}

// DefaultPolicy returns the one default policy (global; ErrNotFound before init creates it).
func (s *Store) DefaultPolicy(ctx context.Context) (PolicyRecord, error) {
	var out PolicyRecord
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+policyCols+` FROM policies WHERE scope = 'default'`, nil, scanPolicy)
		return err
	})
	return out, err
}

// ListPolicies returns the default and template policies plus the customer
// policies inside scope, ordered by scope then name.
func (s *Store) ListPolicies(ctx context.Context, scope Scope) ([]PolicyRecord, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause("customer_id")
	var out []PolicyRecord
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT `+policyCols+` FROM policies
			WHERE scope IN ('default','template') OR (scope = 'customer' AND `+clause+`)
			ORDER BY CASE scope WHEN 'default' THEN 0 WHEN 'template' THEN 1 ELSE 2 END, name COLLATE NOCASE, id`, args, scanPolicy)
		return err
	})
	return out, err
}

// UpdatePolicy applies u, bumps the version and writes a revision, in one
// transaction. Editing a default or template policy needs scope.All (ErrScope);
// a customer policy must lie inside scope (ErrNotFound).
func (s *Store) UpdatePolicy(ctx context.Context, scope Scope, id string, u PolicyUpdate) (PolicyRecord, error) {
	if err := scope.check(); err != nil {
		return PolicyRecord{}, err
	}
	var out PolicyRecord
	err := s.Write(ctx, func(tx *Tx) error {
		rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{id}, scanPolicy)
		if err != nil {
			return err
		}
		if !readablePolicy(scope, rec) {
			return ErrNotFound
		}
		if rec.Scope != "customer" && !scope.All {
			return ErrScope
		}
		if u.ExpectedVersion > 0 && u.ExpectedVersion != rec.Version {
			return conflictf("policy %s is at version %d, not %d", id, rec.Version, u.ExpectedVersion)
		}
		sections := make(map[string]json.RawMessage, 9)
		for k, v := range rec.Sections {
			sections[k] = v
		}
		if _, err := sectionArgs(u.Sections); err != nil {
			return err
		}
		for k, v := range u.Sections {
			sections[k] = v
		}
		name, desc := rec.Name, rec.Description
		if u.Name != nil {
			name = *u.Name
		}
		if u.Description != nil {
			desc = *u.Description
		}
		args, err := sectionArgs(sections)
		if err != nil {
			return err
		}
		if err := validateComposed(tx, rec.Scope, sections); err != nil {
			return err
		}
		now := tx.nowMS()
		ver := rec.Version + 1
		if _, err := tx.Exec(`UPDATE policies SET name = ?, description = ?, version = ?, schedule = ?, conditions = ?,
			selection = ?, retention = ?, throttle = ?, sla = ?, maintenance = ?, agent_permissions = ?,
			notification_overrides = ?, updated_at = ? WHERE id = ?`,
			name, nullStr(desc), ver, args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7], args[8],
			now, id); err != nil {
			return err
		}
		body, err := revisionBody(sections)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO policy_revisions (id, policy_id, version, body, changed_by, change_note, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, tx.id(), id, ver, body, nullStr(u.ChangedBy), nullStr(u.Note), now); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{id}, scanPolicy)
		return err
	})
	return out, err
}

// PolicyRevision is one stored version of a policy.
type PolicyRevision struct {
	PolicyID   string
	Version    int
	Body       json.RawMessage
	ChangedBy  string
	ChangeNote string
	CreatedAt  time.Time
}

// ListPolicyRevisions returns a policy's revisions, newest first.
func (s *Store) ListPolicyRevisions(ctx context.Context, scope Scope, policyID string) ([]PolicyRevision, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	var out []PolicyRevision
	err := s.Read(ctx, func(tx *Tx) error {
		rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{policyID}, scanPolicy)
		if err != nil {
			return err
		}
		if !readablePolicy(scope, rec) {
			return ErrNotFound
		}
		out, err = collect(tx, `SELECT policy_id, version, body, changed_by, change_note, created_at
			FROM policy_revisions WHERE policy_id = ? ORDER BY version DESC`, []any{policyID},
			func(sc scanner) (PolicyRevision, error) {
				var r PolicyRevision
				var body string
				var by, note sql.NullString
				var c int64
				if err := sc.Scan(&r.PolicyID, &r.Version, &body, &by, &note, &c); err != nil {
					return r, err
				}
				r.Body, r.ChangedBy, r.ChangeNote, r.CreatedAt = json.RawMessage(body), by.String, note.String, timeOf(c)
				return r, nil
			})
		return err
	})
	return out, err
}
