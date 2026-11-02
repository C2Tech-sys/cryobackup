package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// The policy slice of M2 (ticket M2-W2A, architecture §5.2 "Policies"): list
// with in-scope counts, create/update/delete with a blast-radius preview whose
// token the confirmed change must carry, and the customer's policy view. Every
// mutation writes exactly one audit row in its own transaction (a refused audit
// refuses the change), and every method takes a Scope.

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

// PolicySummary is one row of the policy list: the policy, the customer a
// customer policy belongs to, and how many customers and active devices run
// under it, counted inside the caller's scope only (a scoped caller never
// learns how many devices other customers have).
type PolicySummary struct {
	PolicyRecord
	CustomerName string
	Customers    int
	Devices      int
}

// ListPolicySummaries lists the default and template policies plus the customer
// policies inside scope, with in-scope usage counts. The default policy is
// "used" by every customer that has no policy of its own.
func (s *Store) ListPolicySummaries(ctx context.Context, scope Scope) ([]PolicySummary, error) {
	recs, err := s.ListPolicies(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make([]PolicySummary, len(recs))
	idx := make(map[string]int, len(recs))
	for i, r := range recs {
		out[i] = PolicySummary{PolicyRecord: r}
		idx[r.ID] = i
	}
	clause, args := scope.clause("c.id")
	err = s.Read(ctx, func(tx *Tx) error {
		names, err := collect(tx, `SELECT id, name FROM customers WHERE `+strings.ReplaceAll(clause, "c.id", "id"), args,
			func(sc scanner) ([2]string, error) {
				var v [2]string
				return v, sc.Scan(&v[0], &v[1])
			})
		if err != nil {
			return err
		}
		custName := map[string]string{}
		for _, n := range names {
			custName[n[0]] = n[1]
		}
		for i := range out {
			if out[i].Scope == "customer" {
				out[i].CustomerName = custName[out[i].CustomerID]
			}
		}
		// customers (not archived) and their active devices, per assigned policy
		rows, err := collect(tx, `SELECT c.policy_id, COUNT(DISTINCT c.id), COUNT(d.id)
			FROM customers c LEFT JOIN devices d ON d.customer_id = c.id AND d.retired_at IS NULL
			WHERE c.state <> 'archived' AND `+clause+` GROUP BY c.policy_id`, args,
			func(sc scanner) (struct {
				pol    sql.NullString
				cs, ds int
			}, error) {
				var v struct {
					pol    sql.NullString
					cs, ds int
				}
				return v, sc.Scan(&v.pol, &v.cs, &v.ds)
			})
		if err != nil {
			return err
		}
		for _, r := range rows {
			id := r.pol.String
			if !r.pol.Valid {
				for i := range out {
					if out[i].Scope == "default" {
						id = out[i].ID
					}
				}
			}
			if i, ok := idx[id]; ok {
				out[i].Customers, out[i].Devices = r.cs, r.ds
			}
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// impact
// ---------------------------------------------------------------------------

// ImpactCustomer is one customer in a blast radius: its active devices and how
// many of them get a different effective policy.
type ImpactCustomer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Devices  int    `json:"devices"`
	Changing int    `json:"changing"`
}

// ImpactReport is the blast radius of a change, taken before it is saved.
type ImpactReport struct {
	// Kind is policy.update, policy.delete or customer.policy_assign.
	Kind       string `json:"kind"`
	PolicyID   string `json:"policy_id,omitempty"`
	PolicyName string `json:"policy_name,omitempty"`
	Scope      string `json:"scope,omitempty"`
	// Version is the policy version the preview was taken against.
	Version int `json:"version,omitempty"`
	// Population is the number of active devices that run under what changes;
	// Devices is how many of them get a different effective policy (a device
	// whose own override covers every changed field keeps it).
	Population int `json:"population"`
	Devices    int `json:"devices"`
	// Invalid counts devices whose effective policy would stop validating; a
	// change with Invalid > 0 is refused.
	Invalid   int              `json:"invalid"`
	Customers []ImpactCustomer `json:"customers"`
	Changes   []FieldChange    `json:"changes"`
	// NameChange is set when the policy is renamed.
	NameChange *FieldChange `json:"name_change,omitempty"`
	// Blockers lists the customers a policy is assigned to (a delete is refused).
	Blockers []ImpactCustomer `json:"blockers,omitempty"`
	// Unchanged is true when applying the proposal would change nothing.
	Unchanged bool `json:"unchanged"`
	// Repairs is true when the stored policy does not compose today and this
	// change is measured against the default alone: it is a repair, and allowed.
	Repairs bool `json:"repairs,omitempty"`
	// pop and defVer are what the token binds besides the proposal.
	pop    string
	defVer int
	// Token binds this preview to the proposal; the confirmed change carries it.
	Token     string    `json:"impact_token,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type impactCust struct {
	ID, Name, PolicyID string
}

type impactDev struct{ CustomerID, Patch string }

// loadImpactInputs reads the in-scope customers (not archived) and their active
// devices with the device's active override patch.
func loadImpactInputs(tx *Tx, scope Scope) ([]impactCust, []impactDev, error) {
	cl, cargs := scope.clause("id")
	custs, err := collect(tx, `SELECT id, name, policy_id FROM customers WHERE state <> 'archived' AND `+cl+` ORDER BY name COLLATE NOCASE, id`, cargs,
		func(sc scanner) (impactCust, error) {
			var c impactCust
			var pol sql.NullString
			err := sc.Scan(&c.ID, &c.Name, &pol)
			c.PolicyID = pol.String
			return c, err
		})
	if err != nil {
		return nil, nil, err
	}
	dl, dargs := scope.clause("d.customer_id")
	args := append([]any{tx.nowMS()}, dargs...)
	devs, err := collect(tx, `SELECT d.customer_id, COALESCE(o.patch, '') FROM devices d
		LEFT JOIN device_policy_overrides o ON o.device_id = d.id AND o.cleared_at IS NULL AND (o.expires_at IS NULL OR o.expires_at > ?)
		WHERE d.retired_at IS NULL AND `+dl, args,
		func(sc scanner) (impactDev, error) {
			var d impactDev
			return d, sc.Scan(&d.CustomerID, &d.Patch)
		})
	return custs, devs, err
}

func sectionsOf(tx *Tx, policyID string) (map[string]json.RawMessage, error) {
	rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{policyID}, scanPolicy)
	if err != nil {
		return nil, err
	}
	return rec.Sections, nil
}

// layerSet is the policy documents a composition reads: the default's stored
// sections and the sparse sections of every customer policy or template.
type layerSet struct {
	def    map[string]json.RawMessage
	sparse map[string]map[string]json.RawMessage
}

func loadLayers(tx *Tx, custs []impactCust, extra ...string) (layerSet, error) {
	l := layerSet{sparse: map[string]map[string]json.RawMessage{}}
	def, _, err := loadDefaultSections(tx)
	if err != nil {
		return l, err
	}
	l.def = def
	ids := append([]string(nil), extra...)
	for _, c := range custs {
		ids = append(ids, c.PolicyID)
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := l.sparse[id]; ok {
			continue
		}
		sec, err := sectionsOf(tx, id)
		if errors.Is(err, ErrNotFound) {
			continue // a dangling id cannot happen (FK), but never break a preview over it
		}
		if err != nil {
			return l, err
		}
		l.sparse[id] = sec
	}
	return l, nil
}

func (l layerSet) with(policyID string, sections map[string]json.RawMessage, isDefault bool) layerSet {
	n := layerSet{def: l.def, sparse: make(map[string]map[string]json.RawMessage, len(l.sparse))}
	for k, v := range l.sparse {
		n.sparse[k] = v
	}
	if isDefault {
		n.def = sections
	} else {
		n.sparse[policyID] = sections
	}
	return n
}

// loadDefaultSections returns the default policy's stored sections and version
// (the proposed defaults at version 0 before init created one).
func loadDefaultSections(tx *Tx) (map[string]json.RawMessage, PolicyRecord, error) {
	rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE scope = 'default'`, nil, scanPolicy)
	if errors.Is(err, ErrNotFound) {
		sec, eerr := policy.Encode(policy.Default(policy.ClassUnknown))
		return sec, PolicyRecord{}, eerr
	}
	return rec.Sections, rec, err
}

func (l layerSet) etag(policyID, patch string) (string, error) {
	var sparse map[string]json.RawMessage
	if policyID != "" {
		sparse = l.sparse[policyID]
	}
	c, err := composeLayers(l.def, sparse, patch)
	if err != nil {
		return "", err
	}
	return policy.ETag(c.Parsed)
}

type impactCounts struct {
	population, changing, invalid int
	invalidName                   string
	customers                     []ImpactCustomer
}

// computeImpact composes the effective policy of every active device of the
// included customers before and after, and counts the ones whose ETag differs.
// before and after hold the same customers in the same order.
func computeImpact(before, after []impactCust, devs []impactDev, lb, la layerSet, include func(impactCust) bool) impactCounts {
	var out impactCounts
	type key struct{ pol, patch string }
	cacheB, cacheA := map[key]string{}, map[key]string{}
	errB, errA := map[key]bool{}, map[key]bool{}
	evalETag := func(l layerSet, cache map[key]string, bad map[key]bool, pol, patch string) (string, bool) {
		k := key{pol, patch}
		if e, ok := cache[k]; ok {
			return e, true
		}
		if bad[k] {
			return "", false
		}
		e, err := l.etag(pol, patch)
		if err != nil {
			bad[k] = true
			return "", false
		}
		cache[k] = e
		return e, true
	}
	idx := map[string]int{}
	for i, c := range before {
		idx[c.ID] = i
	}
	rows := make([]ImpactCustomer, len(before))
	for i, c := range before {
		rows[i] = ImpactCustomer{ID: c.ID, Name: c.Name}
	}
	for _, d := range devs {
		i, ok := idx[d.CustomerID]
		if !ok || !include(before[i]) {
			continue
		}
		out.population++
		rows[i].Devices++
		eb, okB := evalETag(lb, cacheB, errB, before[i].PolicyID, d.Patch)
		ea, okA := evalETag(la, cacheA, errA, after[i].PolicyID, d.Patch)
		switch {
		case !okA && !okB:
			// Broken before and after: not made worse, so it never blocks the
			// change that may be repairing it; the Policy tab raises the alarm.
		case !okA:
			out.invalid++
			if out.invalidName == "" {
				out.invalidName = before[i].Name
			}
		case !okB || eb != ea:
			out.changing++
			rows[i].Changing++
		}
	}
	for _, r := range rows {
		if r.Devices > 0 {
			out.customers = append(out.customers, r)
		}
	}
	return out
}

// ownDocument is the canonical document a policy stands for on its own:
// default ⊕ its sparse sections (or the default alone).
func ownDocument(l layerSet, policyID string) (json.RawMessage, error) {
	var sparse map[string]json.RawMessage
	if policyID != "" {
		sparse = l.sparse[policyID]
	}
	c, err := composeLayers(l.def, sparse, "")
	if err != nil {
		return nil, err
	}
	return c.Final, nil
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

// ProposedPolicy is a change to a policy: the sections present replace the
// stored ones wholesale (a customer policy or template stores only the fields
// that differ from the default), absent ones stay. Name and Description change
// when set. Note is written to the new revision.
type ProposedPolicy struct {
	Name        *string
	Description *string
	Sections    map[string]json.RawMessage
	Note        string
}

// canonSections is the comparable form of a section map: nine entries, keys
// sorted, numbers untouched, an empty section as {}. A sparse map (a template or
// customer policy) also loses its nulls: a null removes the pin and is never stored.
func canonSections(sections map[string]json.RawMessage, sparse bool) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(policy.SectionNames))
	for _, n := range policy.SectionNames {
		raw := sections[n]
		if isEmptyObject(raw) {
			out[n] = json.RawMessage("{}")
			continue
		}
		t, err := decodeTree(raw)
		if err != nil {
			return nil, invalidf("policy section %q is not valid JSON", n)
		}
		if _, ok := t.(map[string]any); !ok {
			return nil, invalidf("policy section %q is not a JSON object", n)
		}
		if sparse {
			t = stripNulls(t)
		}
		b, err := encodeTree(t)
		if err != nil {
			return nil, err
		}
		out[n] = b
	}
	return out, nil
}

type resultingPolicy struct {
	name, desc string
	sections   map[string]json.RawMessage // canonical, all nine
}

// resultOf applies a proposal over the stored record.
func resultOf(rec PolicyRecord, p ProposedPolicy) (resultingPolicy, error) {
	if _, err := sectionArgs(p.Sections); err != nil {
		return resultingPolicy{}, err
	}
	if unknown, err := checkSectionShape(p.Sections); err != nil {
		return resultingPolicy{}, err
	} else if len(unknown) > 0 {
		return resultingPolicy{}, validation(unknown)
	}
	merged := make(map[string]json.RawMessage, len(policy.SectionNames))
	for k, v := range rec.Sections {
		merged[k] = v
	}
	for k, v := range p.Sections {
		merged[k] = v
	}
	canon, err := canonSections(merged, rec.Scope != "default")
	if err != nil {
		return resultingPolicy{}, err
	}
	r := resultingPolicy{name: rec.Name, desc: rec.Description, sections: canon}
	var errs []FieldError
	if p.Name != nil {
		n, ok := cleanLine(*p.Name, 100)
		if !ok || n == "" {
			errs = append(errs, fieldf("name", "Use 1 to 100 characters on one line."))
		}
		r.name = n
	}
	if p.Description != nil {
		d, ok := cleanText(*p.Description, 500)
		if !ok {
			errs = append(errs, fieldf("description", "Use at most 500 characters of plain text."))
		}
		r.desc = d
	}
	if err := validation(errs); err != nil {
		return resultingPolicy{}, err
	}
	return r, nil
}

func (r resultingPolicy) digest() string {
	parts := []any{r.name, r.desc}
	for _, n := range policy.SectionNames {
		parts = append(parts, r.sections[n])
	}
	return digestOf(parts...)
}

func (r resultingPolicy) sameAs(rec PolicyRecord) (bool, error) {
	if r.name != rec.Name || r.desc != rec.Description {
		return false, nil
	}
	cur, err := canonSections(rec.Sections, rec.Scope != "default")
	if err != nil {
		return false, err
	}
	for _, n := range policy.SectionNames {
		if string(cur[n]) != string(r.sections[n]) {
			return false, nil
		}
	}
	return true, nil
}

// validateProposal judges the resulting sections: composed over the default (or,
// for the default itself, over the proposed defaults) they must validate. The
// error is a *ValidationError with one field per problem, keyed by document path.
func validateProposal(tx *Tx, scopeKind string, sections map[string]json.RawMessage) error {
	def, _, err := loadDefaultSections(tx)
	if err != nil {
		return err
	}
	var sparse map[string]json.RawMessage
	if scopeKind == "default" {
		def = sections
	} else {
		sparse = sections
	}
	if _, err := composeLayers(def, sparse, ""); err != nil {
		if fe := PolicyProblems(err); len(fe) > 0 {
			return validation(fe)
		}
		return err
	}
	return nil
}

func nameTaken(tx *Tx, rec PolicyRecord, name string) (bool, error) {
	var found int
	var err error
	if rec.Scope == "customer" {
		err = tx.QueryRow(`SELECT 1 FROM policies WHERE scope = 'customer' AND customer_id = ? AND name = ? COLLATE NOCASE AND id <> ?`,
			rec.CustomerID, name, rec.ID).Scan(&found)
	} else {
		err = tx.QueryRow(`SELECT 1 FROM policies WHERE scope IN ('default','template') AND name = ? COLLATE NOCASE AND id <> ?`,
			name, rec.ID).Scan(&found)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// loadEditable loads a policy for a change and applies the scope rules of
// UpdatePolicy: a customer policy outside scope is ErrNotFound, editing the
// default or a template needs scope.All (ErrScope).
func loadEditable(tx *Tx, scope Scope, id string) (PolicyRecord, error) {
	rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{id}, scanPolicy)
	if err != nil {
		return PolicyRecord{}, err
	}
	if !readablePolicy(scope, rec) {
		return PolicyRecord{}, ErrNotFound
	}
	if rec.Scope != "customer" && !scope.All {
		return PolicyRecord{}, ErrScope
	}
	return rec, nil
}

// previewUpdate computes the blast radius of a proposal inside tx.
func previewUpdate(tx *Tx, scope Scope, rec PolicyRecord, res resultingPolicy) (ImpactReport, error) {
	if err := validateProposal(tx, rec.Scope, res.sections); err != nil {
		return ImpactReport{}, err
	}
	if res.name != rec.Name {
		taken, err := nameTaken(tx, rec, res.name)
		if err != nil {
			return ImpactReport{}, err
		}
		if taken {
			return ImpactReport{}, validation([]FieldError{fieldf("name", "Another policy already has this name.")})
		}
	}
	rep := ImpactReport{Kind: "policy.update", PolicyID: rec.ID, PolicyName: rec.Name, Scope: rec.Scope, Version: rec.Version}
	same, err := res.sameAs(rec)
	if err != nil {
		return ImpactReport{}, err
	}
	rep.Unchanged = same
	if res.name != rec.Name {
		rep.NameChange = &FieldChange{Path: "name", Old: rec.Name, New: res.name}
	}
	custs, devs, err := loadImpactInputs(tx, scope)
	if err != nil {
		return ImpactReport{}, err
	}
	lb, err := loadLayers(tx, custs, rec.ID)
	if err != nil {
		return ImpactReport{}, err
	}
	la := lb.with(rec.ID, res.sections, rec.Scope == "default")
	oldDoc, err := ownDocument(lb, ownerKey(rec))
	if err != nil {
		// The stored policy does not compose (it was edited around, or the default
		// moved under it). That must not lock the operator out of fixing it: the
		// change is measured against the default alone and flagged as a repair.
		rep.Repairs = true
		if oldDoc, err = defaultDoc(lb.def); err != nil {
			oldDoc = json.RawMessage("{}")
		}
	}
	newDoc, err := ownDocument(la, ownerKey(rec))
	if err != nil {
		return ImpactReport{}, err
	}
	if rep.Changes, err = DiffDocuments(oldDoc, newDoc); err != nil {
		return ImpactReport{}, err
	}
	include := func(c impactCust) bool {
		switch rec.Scope {
		case "default":
			return true
		default:
			return c.PolicyID == rec.ID
		}
	}
	if rep.pop, err = populationKey(tx, custs, include, rec.Scope == "default"); err != nil {
		return ImpactReport{}, err
	}
	_, defRec, err := loadDefaultSections(tx)
	if err != nil {
		return ImpactReport{}, err
	}
	rep.defVer = defRec.Version
	cnt := computeImpact(custs, custs, devs, lb, la, include)
	rep.Population, rep.Devices, rep.Invalid, rep.Customers = cnt.population, cnt.changing, cnt.invalid, cnt.customers
	if rec.Scope == "default" {
		// Every stored policy composes over the default, whoever uses it today: a
		// template nobody has yet, a customer policy of an archived customer.
		broken, hidden, err := newlyBrokenPolicies(tx, scope, lb.def, res.sections)
		if err != nil {
			return ImpactReport{}, err
		}
		if len(broken)+hidden > 0 {
			msg := fmt.Sprintf("This change to the default policy would stop %d stored policy(ies) from composing", len(broken)+hidden)
			if len(broken) > 0 {
				msg += ": " + strings.Join(broken, ", ")
			}
			if hidden > 0 {
				msg += fmt.Sprintf(" (and %d outside your scope)", hidden)
			}
			return rep, validation([]FieldError{fieldf("policy", "%s. Fix those first. Nothing was saved.", msg)})
		}
	}
	if cnt.invalid > 0 {
		return rep, validation([]FieldError{fieldf("policy", "This change would leave the effective policy invalid for %d device(s) of %s. Nothing was saved.", cnt.invalid, cnt.invalidName)})
	}
	return rep, nil
}

// populationKey fingerprints the customers a change reaches, with the version of
// the policy each one runs under (and, for the default policy, every stored
// policy): a token measured on it goes stale when any of them moves.
func populationKey(tx *Tx, custs []impactCust, include func(impactCust) bool, allPolicies bool) (string, error) {
	rows, err := collect(tx, `SELECT id, version FROM policies`, nil, func(sc scanner) ([2]string, error) {
		var id string
		var v int
		err := sc.Scan(&id, &v)
		return [2]string{id, strconv.Itoa(v)}, err
	})
	if err != nil {
		return "", err
	}
	ver := map[string]string{"": "0"}
	for _, r := range rows {
		ver[r[0]] = r[1]
	}
	var parts []string
	for _, c := range custs {
		if include(c) {
			parts = append(parts, c.ID+":"+c.PolicyID+":"+ver[c.PolicyID])
		}
	}
	if allPolicies {
		for _, r := range rows {
			parts = append(parts, "p:"+r[0]+":"+r[1])
		}
	}
	sort.Strings(parts)
	return digestOf(strings.Join(parts, "|")), nil
}

// newlyBrokenPolicies lists the stored policies (templates and customer policies,
// whether or not anyone uses them) that compose over the current default but
// would not over newDef. A policy that already fails to compose is not counted:
// a change must stay possible while the operator is repairing things. Policies
// the caller may not see are only counted.
func newlyBrokenPolicies(tx *Tx, scope Scope, curDef, newDef map[string]json.RawMessage) (names []string, hidden int, err error) {
	recs, err := collect(tx, `SELECT `+policyCols+` FROM policies WHERE scope <> 'default' ORDER BY name COLLATE NOCASE, id`, nil, scanPolicy)
	if err != nil {
		return nil, 0, err
	}
	custName := map[string]string{}
	rows, err := collect(tx, `SELECT id, name FROM customers`, nil, func(sc scanner) ([2]string, error) {
		var v [2]string
		return v, sc.Scan(&v[0], &v[1])
	})
	if err != nil {
		return nil, 0, err
	}
	for _, r := range rows {
		custName[r[0]] = r[1]
	}
	for _, rec := range recs {
		if _, e := composeLayers(curDef, rec.Sections, ""); e != nil {
			continue
		}
		if _, e := composeLayers(newDef, rec.Sections, ""); e == nil {
			continue
		}
		if !readablePolicy(scope, rec) {
			hidden++
			continue
		}
		label := rec.Name
		if rec.Scope == "customer" {
			label += " of " + custName[rec.CustomerID]
		}
		names = append(names, label)
	}
	return names, hidden, nil
}

// ownerKey is the key of the policy's own document in a layerSet: the default
// is the base ("" selects no sparse layer), anything else is its id.
func ownerKey(rec PolicyRecord) string {
	if rec.Scope == "default" {
		return ""
	}
	return rec.ID
}

// PreviewPolicyChange takes the blast radius of a proposed change without
// saving anything: the devices that run under the policy, how many get a
// different effective policy, the changed fields (old to new) and a token the
// confirmed change must carry. actorID is the administrator previewing; only
// that administrator's change can use the token. A proposal that does not
// validate is a *ValidationError.
func (s *Store) PreviewPolicyChange(ctx context.Context, scope Scope, id, actorID string, p ProposedPolicy) (ImpactReport, error) {
	if err := scope.check(); err != nil {
		return ImpactReport{}, err
	}
	var rep ImpactReport
	err := s.Read(ctx, func(tx *Tx) error {
		rec, err := loadEditable(tx, scope, id)
		if err != nil {
			return err
		}
		res, err := resultOf(rec, p)
		if err != nil {
			return err
		}
		if rep, err = previewUpdate(tx, scope, rec, res); err != nil {
			return err
		}
		rep.Token, rep.ExpiresAt, err = mintImpact(impactClaims{Kind: "policy.update", Object: rec.ID, Version: rec.Version,
			DefaultVersion: rep.defVer, Population: rep.pop, Digest: res.digest(), Admin: actorID}, tx.Now())
		return err
	})
	return rep, err // on a refusal the report still tells the caller what was measured
}

// changedFieldNames reduces the changed leaves to path-free field names for the
// audit row: at most three segments, and never a path_flags key (a root).
func changedFieldNames(changes []FieldChange) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range changes {
		p := c.Path
		if strings.HasPrefix(p, "selection.path_flags") {
			p = "selection.path_flags"
		} else if parts := strings.Split(p, "."); len(parts) > 3 {
			p = strings.Join(parts[:3], ".")
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// UpdatePolicyAudited applies a previewed change. token is the ImpactReport
// token of the preview of exactly this proposal; without it (ErrInvalid, field
// impact_token), against another proposal (ErrImpactMismatch) or after the
// policy changed or the preview expired (ErrImpactStale) the change is refused
// and nothing is written. It bumps the version, writes a revision, and writes
// one policy.update audit row naming the changed fields, the version move and
// the device count, in the same transaction. A proposal that changes nothing
// writes nothing and reports changed=false.
func (s *Store) UpdatePolicyAudited(ctx context.Context, scope Scope, id string, p ProposedPolicy, token string, actor AuditActor) (PolicyRecord, bool, error) {
	if err := scope.check(); err != nil {
		return PolicyRecord{}, false, err
	}
	var out PolicyRecord
	changed := false
	err := s.Write(ctx, func(tx *Tx) error {
		rec, err := loadEditable(tx, scope, id)
		if err != nil {
			return err
		}
		res, err := resultOf(rec, p)
		if err != nil {
			return err
		}
		rep, err := previewUpdate(tx, scope, rec, res)
		if err != nil {
			return err
		}
		claims, err := checkImpact(token, impactClaims{Kind: "policy.update", Object: rec.ID, Version: rec.Version,
			DefaultVersion: rep.defVer, Population: rep.pop, Digest: res.digest(), Admin: actor.AdminID}, tx.Now())
		if err != nil {
			return err
		}
		if rep.Unchanged {
			out = rec
			return nil
		}
		args, err := sectionArgs(res.sections)
		if err != nil {
			return err
		}
		now, ver := tx.nowMS(), rec.Version+1
		if _, err := tx.Exec(`UPDATE policies SET name = ?, description = ?, version = ?, schedule = ?, conditions = ?,
			selection = ?, retention = ?, throttle = ?, sla = ?, maintenance = ?, agent_permissions = ?,
			notification_overrides = ?, updated_at = ? WHERE id = ?`,
			res.name, nullStr(res.desc), ver, args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7], args[8], now, id); err != nil {
			return constraintPolicyName(mapErr(err))
		}
		body, err := revisionBody(res.sections)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO policy_revisions (id, policy_id, version, body, changed_by, change_note, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, tx.id(), id, ver, body, nullStr(actor.AdminID), nullStr(strings.TrimSpace(p.Note)), now); err != nil {
			return err
		}
		details := map[string]any{"fields": changedFieldNames(rep.Changes), "version": map[string]int{"from": rec.Version, "to": ver},
			"devices": rep.Devices, "population": rep.Population}
		if rep.NameChange != nil {
			details["renamed"] = true
		}
		ev, err := actor.event("policy.update", "policy", id, rec.CustomerID, details)
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		changed = true
		useImpact(claims, tx.Now())
		out, err = one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{id}, scanPolicy)
		return err
	})
	if err != nil {
		return PolicyRecord{}, false, err
	}
	return out, changed, nil
}

// constraintPolicyName maps a unique-name collision to the name field.
func constraintPolicyName(err error) error {
	var ce *ConstraintError
	if errors.As(err, &ce) && ce.Kind == ErrConflict && strings.Contains(ce.Constraint, "policies") {
		return validation([]FieldError{fieldf("name", "Another policy already has this name.")})
	}
	return err
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

// CreatePolicyAudited creates a template (needs scope.All) or a customer policy
// (the customer must be inside scope), with its first revision and one
// policy.create audit row in one transaction. A new policy is assigned to
// nobody, so it needs no preview. Problems are a *ValidationError keyed by field.
func (s *Store) CreatePolicyAudited(ctx context.Context, scope Scope, in PolicyInput, actor AuditActor) (PolicyRecord, error) {
	if err := scope.check(); err != nil {
		return PolicyRecord{}, err
	}
	switch in.Scope {
	case "template":
		if !scope.All {
			return PolicyRecord{}, ErrScope
		}
	case "customer":
		if in.CustomerID == "" {
			return PolicyRecord{}, validation([]FieldError{fieldf("customer_id", "Choose the customer this policy belongs to.")})
		}
		if !scope.Allows(in.CustomerID) {
			return PolicyRecord{}, ErrNotFound
		}
	case "default":
		return PolicyRecord{}, validation([]FieldError{fieldf("scope", "The default policy is created by init; edit it instead.")})
	default:
		return PolicyRecord{}, validation([]FieldError{fieldf("scope", "Use template or customer.")})
	}
	name, ok := cleanLine(in.Name, 100)
	var errs []FieldError
	if !ok || name == "" {
		errs = append(errs, fieldf("name", "Use 1 to 100 characters on one line."))
	}
	desc, ok := cleanText(in.Description, 500)
	if !ok {
		errs = append(errs, fieldf("description", "Use at most 500 characters of plain text."))
	}
	if in.TemplateDeviceClass != "" && (in.Scope != "template" || (in.TemplateDeviceClass != "laptop" && in.TemplateDeviceClass != "desktop")) {
		errs = append(errs, fieldf("template_device_class", "Use laptop or desktop, on a template."))
	}
	if err := validation(errs); err != nil {
		return PolicyRecord{}, err
	}
	if _, err := sectionArgs(in.Sections); err != nil {
		return PolicyRecord{}, err
	}
	if unknown, err := checkSectionShape(in.Sections); err != nil {
		return PolicyRecord{}, err
	} else if len(unknown) > 0 {
		return PolicyRecord{}, validation(unknown)
	}
	canon, err := canonSections(in.Sections, true)
	if err != nil {
		return PolicyRecord{}, err
	}
	args, err := sectionArgs(canon)
	if err != nil {
		return PolicyRecord{}, err
	}
	var out PolicyRecord
	err = s.Write(ctx, func(tx *Tx) error {
		if err := validateProposal(tx, in.Scope, canon); err != nil {
			return err
		}
		probe := PolicyRecord{Scope: in.Scope, CustomerID: in.CustomerID}
		if taken, err := nameTaken(tx, probe, name); err != nil {
			return err
		} else if taken {
			return validation([]FieldError{fieldf("name", "Another policy already has this name.")})
		}
		id, now := tx.id(), tx.nowMS()
		if _, err := tx.Exec(`INSERT INTO policies (`+policyCols+`) VALUES (?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, in.Scope, nullStr(in.CustomerID), name, nullStr(desc), nullStr(in.TemplateDeviceClass),
			args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7], args[8],
			nullStr(in.SourceID), nullStr(actor.AdminID), now, now); err != nil {
			return constraintPolicyName(mapErr(err))
		}
		body, err := revisionBody(canon)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO policy_revisions (id, policy_id, version, body, changed_by, change_note, created_at)
			VALUES (?, ?, 1, ?, ?, ?, ?)`, tx.id(), id, body, nullStr(actor.AdminID), nullStr(strings.TrimSpace(in.Note)), now); err != nil {
			return err
		}
		ev, err := actor.event("policy.create", "policy", id, in.CustomerID, map[string]any{"scope": in.Scope})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{id}, scanPolicy)
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// delete
// ---------------------------------------------------------------------------

// PolicyInUseError is a delete refused because customers still use the policy;
// it names them so the caller can say where to reassign. errors.Is(ErrConflict).
type PolicyInUseError struct{ Customers []ImpactCustomer }

// Error implements error.
func (e *PolicyInUseError) Error() string {
	return fmt.Sprintf("%v: the policy is assigned to %d customer(s)", ErrConflict, len(e.Customers))
}

// Unwrap lets errors.Is match ErrConflict.
func (e *PolicyInUseError) Unwrap() error { return ErrConflict }

// ErrDefaultPolicy is returned for a delete of the default policy (a conflict).
var ErrDefaultPolicy = fmt.Errorf("%w: the default policy cannot be deleted", ErrConflict)

func policyUsers(tx *Tx, scope Scope, id string) ([]ImpactCustomer, error) {
	// All customers, whatever the scope: the foreign key refuses a delete while
	// any customer uses the policy, and a customer policy can only belong to one.
	return collect(tx, `SELECT c.id, c.name, COUNT(d.id) FROM customers c
		LEFT JOIN devices d ON d.customer_id = c.id AND d.retired_at IS NULL
		WHERE c.policy_id = ? GROUP BY c.id ORDER BY c.name COLLATE NOCASE`, []any{id},
		func(sc scanner) (ImpactCustomer, error) {
			var c ImpactCustomer
			return c, sc.Scan(&c.ID, &c.Name, &c.Devices)
		})
}

// PreviewPolicyDelete says what deleting a policy would do. A policy still
// assigned (or the default) cannot be deleted: the report lists the customers
// that use it (Blockers) and carries no token. Otherwise it affects no device,
// and the token lets the confirmed delete through.
func (s *Store) PreviewPolicyDelete(ctx context.Context, scope Scope, id, actorID string) (ImpactReport, error) {
	if err := scope.check(); err != nil {
		return ImpactReport{}, err
	}
	var rep ImpactReport
	err := s.Read(ctx, func(tx *Tx) error {
		rec, err := loadEditable(tx, scope, id)
		if err != nil {
			return err
		}
		rep = ImpactReport{Kind: "policy.delete", PolicyID: rec.ID, PolicyName: rec.Name, Scope: rec.Scope, Version: rec.Version}
		if rec.Scope == "default" {
			return ErrDefaultPolicy
		}
		users, err := policyUsers(tx, scope, id)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			rep.Blockers = users
			for _, u := range users {
				rep.Population += u.Devices
			}
			return &PolicyInUseError{Customers: users}
		}
		_, defRec, err := loadDefaultSections(tx)
		if err != nil {
			return err
		}
		rep.Token, rep.ExpiresAt, err = mintImpact(impactClaims{Kind: "policy.delete", Object: rec.ID, Version: rec.Version,
			DefaultVersion: defRec.Version, Digest: digestOf(rec.Name), Admin: actorID}, tx.Now())
		return err
	})
	return rep, err
}

// DeletePolicyAudited deletes an unassigned template or customer policy (its
// revisions go with it) and writes one policy.delete audit row. It needs the
// token of PreviewPolicyDelete. A policy that is assigned is a *PolicyInUseError
// (naming the customers); the default policy is ErrDefaultPolicy.
func (s *Store) DeletePolicyAudited(ctx context.Context, scope Scope, id, token string, actor AuditActor) error {
	if err := scope.check(); err != nil {
		return err
	}
	return s.Write(ctx, func(tx *Tx) error {
		rec, err := loadEditable(tx, scope, id)
		if err != nil {
			return err
		}
		if rec.Scope == "default" {
			return ErrDefaultPolicy
		}
		users, err := policyUsers(tx, scope, id)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &PolicyInUseError{Customers: users}
		}
		_, defRec, err := loadDefaultSections(tx)
		if err != nil {
			return err
		}
		claims, err := checkImpact(token, impactClaims{Kind: "policy.delete", Object: rec.ID, Version: rec.Version,
			DefaultVersion: defRec.Version, Digest: digestOf(rec.Name), Admin: actor.AdminID}, tx.Now())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM policies WHERE id = ?`, id); err != nil {
			return mapErr(err)
		}
		ev, err := actor.event("policy.delete", "policy", id, rec.CustomerID, map[string]any{"scope": rec.Scope, "version": rec.Version})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		useImpact(claims, tx.Now())
		return nil
	})
}

// ---------------------------------------------------------------------------
// customer assignment
// ---------------------------------------------------------------------------

func loadCustomerForPolicy(tx *Tx, scope Scope, customerID string) (Customer, error) {
	if !scope.Allows(customerID) {
		return Customer{}, ErrNotFound
	}
	c, err := one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{customerID}, scanCustomer)
	if err != nil {
		return Customer{}, err
	}
	if c.State == "archived" {
		return Customer{}, conflictf("customer is archived")
	}
	return c, nil
}

func assignDigest(c Customer, newID string, newVersion int) string {
	return digestOf(c.PolicyID, newID, newVersion)
}

func policyVersionOf(tx *Tx, id string) (int, error) {
	if id == "" {
		_, def, err := loadDefaultSections(tx)
		return def.Version, err
	}
	var v int
	err := tx.QueryRow(`SELECT version FROM policies WHERE id = ?`, id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, policyFieldError(errPolicyUnavailable)
	}
	return v, err
}

func previewAssign(tx *Tx, scope Scope, c Customer, newID string) (ImpactReport, error) {
	if err := checkCustomerPolicy(tx, c.ID, newID); err != nil {
		return ImpactReport{}, policyFieldError(err)
	}
	rep := ImpactReport{Kind: "customer.policy_assign", PolicyID: newID, Scope: "", Unchanged: c.PolicyID == newID}
	custs, devs, err := loadImpactInputs(tx, scope)
	if err != nil {
		return ImpactReport{}, err
	}
	var before, after []impactCust
	for _, x := range custs {
		before = append(before, x)
		if x.ID == c.ID {
			x.PolicyID = newID
		}
		after = append(after, x)
	}
	l, err := loadLayers(tx, custs, c.PolicyID, newID)
	if err != nil {
		return ImpactReport{}, err
	}
	oldDoc, err := ownDocument(l, c.PolicyID)
	if err != nil {
		// The current policy does not compose: reassigning is the way out, so it is
		// measured against the default alone and flagged as a repair.
		rep.Repairs = true
		if oldDoc, err = defaultDoc(l.def); err != nil {
			oldDoc = json.RawMessage("{}")
		}
	}
	newDoc, err := ownDocument(l, newID)
	if err != nil {
		return ImpactReport{}, err
	}
	if rep.Changes, err = DiffDocuments(oldDoc, newDoc); err != nil {
		return ImpactReport{}, err
	}
	cnt := computeImpact(before, after, devs, l, l, func(x impactCust) bool { return x.ID == c.ID })
	rep.Population, rep.Devices, rep.Invalid, rep.Customers = cnt.population, cnt.changing, cnt.invalid, cnt.customers
	if rep.Version, err = policyVersionOf(tx, newID); err != nil {
		return ImpactReport{}, err
	}
	curVer, err := policyVersionOf(tx, c.PolicyID)
	if err != nil {
		return ImpactReport{}, err
	}
	_, defRec, err := loadDefaultSections(tx)
	if err != nil {
		return ImpactReport{}, err
	}
	rep.defVer = defRec.Version
	rep.pop = digestOf(c.ID, c.PolicyID, curVer)
	if newID != "" {
		if rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{newID}, scanPolicy); err == nil {
			rep.PolicyName = rec.Name
			rep.Scope = rec.Scope
		}
	} else {
		rep.PolicyName, rep.Scope = "Default", "default"
	}
	if cnt.invalid > 0 {
		return rep, validation([]FieldError{fieldf("policy_id", "This policy would leave the effective policy invalid for %d device(s) of %s. Nothing was changed.", cnt.invalid, cnt.invalidName)})
	}
	return rep, nil
}

// PreviewPolicyAssignment takes the blast radius of assigning policyID ("" =
// the default policy) to a customer: its active devices, how many get a
// different effective policy, and the changed fields (old to new), with a token
// for AssignCustomerPolicyAudited.
func (s *Store) PreviewPolicyAssignment(ctx context.Context, scope Scope, customerID, policyID, actorID string) (ImpactReport, error) {
	if err := scope.check(); err != nil {
		return ImpactReport{}, err
	}
	var rep ImpactReport
	err := s.Read(ctx, func(tx *Tx) error {
		c, err := loadCustomerForPolicy(tx, scope, customerID)
		if err != nil {
			return err
		}
		if rep, err = previewAssign(tx, scope, c, policyID); err != nil {
			return err
		}
		rep.Token, rep.ExpiresAt, err = mintImpact(impactClaims{Kind: "customer.policy_assign", Object: c.ID, Version: rep.Version,
			DefaultVersion: rep.defVer, Population: rep.pop, Digest: assignDigest(c, policyID, rep.Version), Admin: actorID}, tx.Now())
		return err
	})
	return rep, err
}

// AssignCustomerPolicyAudited assigns policyID ("" = the default policy) to a
// customer, with the token of PreviewPolicyAssignment, and writes one
// customer.update audit row (field policy_id) in the same transaction. It
// reports whether anything changed.
func (s *Store) AssignCustomerPolicyAudited(ctx context.Context, scope Scope, customerID, policyID, token string, actor AuditActor) (bool, error) {
	if err := scope.check(); err != nil {
		return false, err
	}
	changed := false
	err := s.Write(ctx, func(tx *Tx) error {
		c, err := loadCustomerForPolicy(tx, scope, customerID)
		if err != nil {
			return err
		}
		rep, err := previewAssign(tx, scope, c, policyID)
		if err != nil {
			return err
		}
		claims, err := checkImpact(token, impactClaims{Kind: "customer.policy_assign", Object: c.ID, Version: rep.Version,
			DefaultVersion: rep.defVer, Population: rep.pop, Digest: assignDigest(c, policyID, rep.Version), Admin: actor.AdminID}, tx.Now())
		if err != nil {
			return err
		}
		if c.PolicyID == policyID {
			return nil
		}
		if _, err := tx.Exec(`UPDATE customers SET policy_id = ?, updated_at = ? WHERE id = ?`, nullStr(policyID), tx.nowMS(), c.ID); err != nil {
			return mapErr(err)
		}
		ev, err := actor.event("customer.update", "customer", c.ID, c.ID, map[string]any{"fields": []string{"policy_id"},
			"policy": map[string]string{"from": c.PolicyID, "to": policyID}, "devices": rep.Devices, "population": rep.Population})
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		changed = true
		useImpact(claims, tx.Now())
		return nil
	})
	return changed, err
}

// ---------------------------------------------------------------------------
// effective views
// ---------------------------------------------------------------------------

// Field sources of EffectiveField.
const (
	SourceDefault  = "default"
	SourcePolicy   = "policy"   // the customer's policy or template
	SourceOverride = "override" // the device's own override
)

// EffectiveField is one leaf of an effective policy with where its value came
// from. Inherited is the value the layer below would have given (empty for a
// field that comes from the default).
type EffectiveField struct {
	Path      string
	Value     string // compact JSON
	Source    string
	Inherited string
}

// EffectiveView is a composed policy with per-field provenance.
type EffectiveView struct {
	Policy policy.Policy
	ETag   string
	// Version and PolicyID are those of the customer-or-default document the
	// agent reports back (EffectivePolicy); PolicyName/PolicyScope name it.
	Version     int
	PolicyID    string
	PolicyName  string
	PolicyScope string
	Default     PolicyRecord
	Fields      []EffectiveField
}

// provenance attributes every leaf of final to the highest layer that sets it.
func provenance(c composedDocs, sparsePatch, overridePatch json.RawMessage) ([]EffectiveField, error) {
	final, err := FlattenDocument(c.Final)
	if err != nil {
		return nil, err
	}
	pol, err := FlattenDocument(c.Policy)
	if err != nil {
		return nil, err
	}
	def, err := FlattenDocument(c.Default)
	if err != nil {
		return nil, err
	}
	var pm, om marks
	if len(sparsePatch) > 0 {
		pm = patchMarks(sparsePatch, c.Default)
	} else {
		pm = marks{exact: map[string]bool{}}
	}
	if len(overridePatch) > 0 {
		om = patchMarks(overridePatch, c.Policy)
	} else {
		om = marks{exact: map[string]bool{}}
	}
	paths := make([]string, 0, len(final))
	for p := range final {
		paths = append(paths, p)
	}
	sortPaths(paths)
	out := make([]EffectiveField, 0, len(paths))
	for _, p := range paths {
		f := EffectiveField{Path: p, Value: final[p], Source: SourceDefault}
		switch {
		case om.has(p):
			f.Source, f.Inherited = SourceOverride, pol[p]
		case pm.has(p):
			f.Source, f.Inherited = SourcePolicy, def[p]
		}
		out = append(out, f)
	}
	return out, nil
}

// CustomerPolicy is the Policy tab of a customer (S17): the assigned policy,
// the policies it may be assigned, the effective summary of that assignment and
// how many devices carry their own override.
type CustomerPolicy struct {
	CustomerID   string
	CustomerName string
	// Assigned is nil when the default policy applies.
	Assigned *PolicyRecord
	Default  PolicyRecord
	Choices  []PolicyChoice
	Devices  int
	// Overridden counts active devices with an active override.
	Overridden int
	Effective  EffectiveView
	// Broken is true when the assigned policy does not compose over the default:
	// the devices cannot be served a valid policy until it is fixed or replaced.
	Broken bool
}

// GetCustomerPolicy reads the Policy tab data of a customer inside scope.
func (s *Store) GetCustomerPolicy(ctx context.Context, scope Scope, customerID string) (CustomerPolicy, error) {
	if err := scope.check(); err != nil {
		return CustomerPolicy{}, err
	}
	var out CustomerPolicy
	err := s.Read(ctx, func(tx *Tx) error {
		if !scope.Allows(customerID) {
			return ErrNotFound
		}
		c, err := one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{customerID}, scanCustomer)
		if err != nil {
			return err
		}
		out.CustomerID, out.CustomerName = c.ID, c.Name
		defSections, defRec, err := loadDefaultSections(tx)
		if err != nil {
			return err
		}
		out.Default = defRec
		var sparse map[string]json.RawMessage
		var patch json.RawMessage
		out.Effective.PolicyID, out.Effective.Version, out.Effective.PolicyName, out.Effective.PolicyScope = defRec.ID, defRec.Version, defRec.Name, "default"
		if c.PolicyID != "" {
			rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{c.PolicyID}, scanPolicy)
			if err != nil {
				return err
			}
			out.Assigned = &rec
			sparse = rec.Sections
			if patch, err = sectionsPatch(sparse); err != nil {
				return err
			}
			out.Effective.PolicyID, out.Effective.Version, out.Effective.PolicyName, out.Effective.PolicyScope = rec.ID, rec.Version, rec.Name, rec.Scope
		}
		out.Effective.Default = defRec
		docs, err := composeLayers(defSections, sparse, "")
		if err != nil {
			// Do not lock the operator out: the tab still shows the assignment and
			// lets them reassign or fix the policy; it just has no effective settings.
			out.Broken = true
		} else {
			out.Effective.Policy = docs.Parsed
			if out.Effective.ETag, err = policy.ETag(docs.Parsed); err != nil {
				return err
			}
			if out.Effective.Fields, err = provenance(docs, patch, nil); err != nil {
				return err
			}
		}
		rows, err := collect(tx, `SELECT COUNT(d.id), COUNT(o.id) FROM devices d
			LEFT JOIN device_policy_overrides o ON o.device_id = d.id AND o.cleared_at IS NULL AND (o.expires_at IS NULL OR o.expires_at > ?)
			WHERE d.customer_id = ? AND d.retired_at IS NULL`, []any{tx.nowMS(), customerID},
			func(sc scanner) ([2]int, error) {
				var v [2]int
				return v, sc.Scan(&v[0], &v[1])
			})
		if err != nil {
			return err
		}
		if len(rows) == 1 {
			out.Devices, out.Overridden = rows[0][0], rows[0][1]
		}
		out.Choices, err = policyChoices(tx, scope, customerID)
		return err
	})
	return out, err
}

func policyChoices(tx *Tx, scope Scope, customerID string) ([]PolicyChoice, error) {
	return collect(tx, `SELECT id, name, scope FROM policies WHERE scope = 'template' OR (scope = 'customer' AND customer_id = ?)
		ORDER BY CASE scope WHEN 'template' THEN 0 ELSE 1 END, name COLLATE NOCASE, id`, []any{customerID},
		func(sc scanner) (PolicyChoice, error) {
			var c PolicyChoice
			return c, sc.Scan(&c.ID, &c.Name, &c.Scope)
		})
}

// PolicyRevisionView is a revision with the name of who changed it.
type PolicyRevisionView struct {
	PolicyRevision
	ChangedByName string
}

// ListPolicyRevisionViews lists a policy's revisions, newest first, with the
// changing administrator's username (the account may have been removed since).
func (s *Store) ListPolicyRevisionViews(ctx context.Context, scope Scope, policyID string, limit int) ([]PolicyRevisionView, error) {
	revs, err := s.ListPolicyRevisions(ctx, scope, policyID)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(revs) > limit {
		revs = revs[:limit]
	}
	out := make([]PolicyRevisionView, len(revs))
	names := map[string]string{}
	err = s.Read(ctx, func(tx *Tx) error {
		for i, r := range revs {
			out[i] = PolicyRevisionView{PolicyRevision: r}
			if r.ChangedBy == "" {
				continue
			}
			n, ok := names[r.ChangedBy]
			if !ok {
				if err := tx.QueryRow(`SELECT username FROM admin_users WHERE id = ?`, r.ChangedBy).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				names[r.ChangedBy] = n
			}
			out[i].ChangedByName = n
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, err
}
