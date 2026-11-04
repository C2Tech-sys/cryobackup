package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The saved-view filter (M3-T3, components.md 5.4 "Query / clause builder"). A
// filter is a clause tree:
//
//	{}                                     no filter: all rows
//	{"field": F, "op": O, "value": V}      one clause
//	{"op": "and"|"or"|"not", "clauses": [...]}   a group (never empty)
//
// F is a field of the view's entity, O an operator that field takes and V a
// value of the field's type. The tree is validated when a view is written
// (unknown fields, operators, value types, empty groups, more than 40 clauses
// or 4 levels of groups are refused) and read again, with the same parser, when
// a scoped caller asks to see another account's shared view: every customer,
// device and hostname the filter names must resolve to something inside the
// caller's scope, and a filter that does not parse is hidden. That second rule
// is what keeps a shared view from carrying another customer's identifiers
// across the scope line, so the parser is the single definition of "what a
// filter can name".

const (
	maxFilterClauses = 40
	maxFilterDepth   = 4
	maxFilterList    = 50
)

type vfKind int

const (
	kList  vfKind = iota // 1..50 strings
	kText                // one printable line
	kDur                 // 15m, 24h, 3d
	kVer                 // a version string
	kDate                // RFC 3339 or YYYY-MM-DD
	kRange               // {"from": date, "to": date}
	kBool
	kNone // no value
)

type vfRef int

const (
	refNone vfRef = iota
	refCustomer
	refDevice
	refHostname
	refCustomerName
)

type vfField struct {
	ops  []string
	kind vfKind
	enum []string // for kList: the allowed values; nil means identifier tokens
	ref  vfRef
}

var (
	opsSet    = []string{"in", "not_in"}
	opsEq     = []string{"eq", "ne"}
	opsText   = []string{"eq", "ne", "contains", "starts_with"}
	deviceF   = map[string]vfField{}
	runF      = map[string]vfField{}
	customerF = map[string]vfField{}
)

func init() {
	var states []string
	for _, s := range state.Precedence() {
		states = append(states, string(s))
	}
	deviceF = map[string]vfField{
		"state":            {ops: opsSet, kind: kList, enum: states},
		"customer":         {ops: opsSet, kind: kList, ref: refCustomer},
		"hostname":         {ops: opsText, kind: kText, ref: refHostname},
		"last_success_age": {ops: []string{"older_than", "newer_than", "never"}, kind: kDur},
		"agent_version":    {ops: opsEq, kind: kVer},
		"os":               {ops: []string{"eq", "contains"}, kind: kText},
		"tag":              {ops: []string{"eq", "contains"}, kind: kText},
		"device_class":     {ops: opsSet, kind: kList},
	}
	runF = map[string]vfField{
		"outcome":  {ops: opsSet, kind: kList, enum: RunOutcomes},
		"kind":     {ops: opsSet, kind: kList, enum: RunKinds},
		"customer": {ops: opsSet, kind: kList, ref: refCustomer},
		"device":   {ops: opsSet, kind: kList, ref: refDevice},
		"hostname": {ops: opsText, kind: kText, ref: refHostname},
		"date":     {ops: []string{"after", "before", "between"}, kind: kDate},
	}
	customerF = map[string]vfField{
		"status":   {ops: opsSet, kind: kList, enum: []string{"missing", "unknown", "alarm", "warn", "ok", "onboarding", "empty"}},
		"name":     {ops: []string{"eq", "contains"}, kind: kText, ref: refCustomerName},
		"expected": {ops: []string{"eq"}, kind: kBool},
	}
}

func filterFieldsOf(entity string) map[string]vfField {
	switch entity {
	case "devices":
		return deviceF
	case "runs":
		return runF
	case "customers":
		return customerF
	}
	return nil
}

// textRef is a name a filter matches by text (a hostname or a customer name).
type textRef struct{ Op, Value string }

// viewRefs is everything a filter names that belongs to a customer.
type viewRefs struct {
	Customers     []string
	Devices       []string
	Hostnames     []textRef
	CustomerNames []textRef
}

var (
	idToken   = regexp.MustCompile(`^[0-9A-Za-z_-]{1,40}$`)
	enumToken = regexp.MustCompile(`^[0-9A-Za-z_.:-]{1,64}$`)
	durValue  = regexp.MustCompile(`^[0-9]{1,4}[mhd]$`)
	verValue  = regexp.MustCompile(`^[0-9A-Za-z.+_-]{1,40}$`)
	dateOnly  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

type vfParser struct {
	fields  map[string]vfField
	refs    viewRefs
	clauses int
	why     string
}

func (p *vfParser) fail(why string) bool {
	if p.why == "" {
		p.why = why
	}
	return false
}

// strictObject decodes raw as one JSON object, keeping the raw members.
func strictObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&m) != nil || m == nil || dec.More() {
		return nil, false
	}
	return m, true
}

func onlyKeys(m map[string]json.RawMessage, keys ...string) bool {
	for k := range m {
		if !slices.Contains(keys, k) {
			return false
		}
	}
	return true
}

func (p *vfParser) node(raw json.RawMessage, depth int) bool {
	m, ok := strictObject(raw)
	if !ok {
		return p.fail("a clause or group must be a JSON object")
	}
	if _, isClause := m["field"]; isClause {
		return p.clause(m)
	}
	if depth > maxFilterDepth {
		return p.fail("groups are nested more than 4 levels deep")
	}
	var op string
	if json.Unmarshal(m["op"], &op) != nil || (op != "and" && op != "or" && op != "not") || !onlyKeys(m, "op", "clauses") {
		return p.fail("a group is {\"op\": and, or or not, \"clauses\": [...]}")
	}
	var kids []json.RawMessage
	if json.Unmarshal(m["clauses"], &kids) != nil || len(kids) == 0 {
		return p.fail("a group needs at least one clause")
	}
	for _, k := range kids {
		if !p.node(k, depth+1) {
			return false
		}
	}
	return true
}

func (p *vfParser) clause(m map[string]json.RawMessage) bool {
	p.clauses++
	if p.clauses > maxFilterClauses {
		return p.fail("a filter has at most 40 clauses")
	}
	var field, op string
	if !onlyKeys(m, "field", "op", "value") || json.Unmarshal(m["field"], &field) != nil || json.Unmarshal(m["op"], &op) != nil {
		return p.fail("a clause is {\"field\", \"op\", \"value\"}")
	}
	spec, known := p.fields[field]
	if !known {
		return p.fail("a clause names a field this table does not have")
	}
	if !slices.Contains(spec.ops, op) {
		return p.fail("a clause uses an operator its field does not take")
	}
	val := bytes.TrimSpace(m["value"])
	if spec.kind == kNone || op == "never" {
		if len(val) != 0 && string(val) != "null" {
			return p.fail("this operator takes no value")
		}
		return true
	}
	if len(val) == 0 || string(val) == "null" {
		return p.fail("a clause needs a value")
	}
	switch spec.kind {
	case kList:
		var vs []string
		if json.Unmarshal(val, &vs) != nil || len(vs) == 0 || len(vs) > maxFilterList {
			return p.fail("this field takes a list of 1 to 50 values")
		}
		for _, v := range vs {
			switch {
			case spec.enum != nil && !slices.Contains(spec.enum, v):
				return p.fail("a value is not one this field takes")
			case spec.enum == nil && spec.ref != refNone && !idToken.MatchString(v):
				return p.fail("a value is not an identifier")
			case spec.enum == nil && spec.ref == refNone && !enumToken.MatchString(v):
				return p.fail("a value is not a plain token")
			}
			switch spec.ref {
			case refCustomer:
				p.refs.Customers = append(p.refs.Customers, v)
			case refDevice:
				p.refs.Devices = append(p.refs.Devices, v)
			}
		}
	case kText:
		var s string
		if json.Unmarshal(val, &s) != nil {
			return p.fail("this field takes text")
		}
		if t, ok := cleanLine(s, 100); !ok || t == "" || looksLikePath(t) || strings.ContainsAny(t, `\`) {
			return p.fail("text must be one printable line of at most 100 characters and must not be a file path")
		}
		switch spec.ref {
		case refHostname:
			p.refs.Hostnames = append(p.refs.Hostnames, textRef{op, strings.TrimSpace(s)})
		case refCustomerName:
			p.refs.CustomerNames = append(p.refs.CustomerNames, textRef{op, strings.TrimSpace(s)})
		}
	case kDur:
		var s string
		if json.Unmarshal(val, &s) != nil || !durValue.MatchString(s) {
			return p.fail("a duration is a number and m, h or d, such as 3d")
		}
	case kVer:
		var s string
		if json.Unmarshal(val, &s) != nil || !verValue.MatchString(s) {
			return p.fail("a version is letters, digits and . + _ -")
		}
	case kBool:
		var b bool
		if json.Unmarshal(val, &b) != nil {
			return p.fail("this field takes true or false")
		}
	case kDate:
		if op == "between" {
			r, ok := strictObject(val)
			var from, to string
			if !ok || !onlyKeys(r, "from", "to") || json.Unmarshal(r["from"], &from) != nil || json.Unmarshal(r["to"], &to) != nil || !validDate(from) || !validDate(to) {
				return p.fail("a date range is {\"from\", \"to\"} in RFC 3339 or YYYY-MM-DD")
			}
			return true
		}
		var s string
		if json.Unmarshal(val, &s) != nil || !validDate(s) {
			return p.fail("a date is RFC 3339 or YYYY-MM-DD")
		}
	}
	return true
}

func validDate(s string) bool {
	if dateOnly.MatchString(s) {
		_, err := time.Parse("2006-01-02", s)
		return err == nil
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// parseViewFilter parses a filter for the entity. ok is false (and why says,
// in a fixed sentence that never echoes the input) when it is not a valid
// clause tree. An empty object is "no filter".
func parseViewFilter(entity string, raw json.RawMessage) (refs viewRefs, why string, ok bool) {
	fields := filterFieldsOf(entity)
	if fields == nil {
		return viewRefs{}, "the view's table is unknown", false
	}
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "{}" {
		return viewRefs{}, "", true
	}
	p := &vfParser{fields: fields}
	if !p.node(t, 1) {
		return viewRefs{}, p.why, false
	}
	return p.refs, "", true
}

// viewFilterError is the FieldError of a filter that does not parse.
func viewFilterError(why string) FieldError {
	return fieldf("filter", "Use a clause tree of {field, op, value} clauses and and/or/not groups (at most 40 clauses, 4 levels): %s.", why)
}

var columnToken = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// validateViewShape checks the columns and sort lists: plain identifiers only.
func validateViewShape(columns, sort *json.RawMessage) []FieldError {
	var bad []FieldError
	if columns != nil && len(*columns) > 0 {
		var cs []string
		if json.Unmarshal(*columns, &cs) != nil || len(cs) > 50 || slices.ContainsFunc(cs, func(c string) bool { return !columnToken.MatchString(c) }) {
			bad = append(bad, fieldf("columns", "Use a list of at most 50 column ids (lowercase letters, digits and _)."))
		}
	}
	if sort != nil && len(*sort) > 0 {
		var ss []struct {
			Column string `json:"column"`
			Dir    string `json:"dir"`
		}
		dec := json.NewDecoder(bytes.NewReader(*sort))
		dec.DisallowUnknownFields()
		if dec.Decode(&ss) != nil || len(ss) > 5 || slices.ContainsFunc(ss, func(s struct {
			Column string `json:"column"`
			Dir    string `json:"dir"`
		}) bool {
			return !columnToken.MatchString(s.Column) || (s.Dir != "asc" && s.Dir != "desc")
		}) {
			bad = append(bad, fieldf("sort", "Use a list of at most 5 {column, dir} entries, dir asc or desc."))
		}
	}
	return bad
}

// allInScope reports whether everything the filter names resolves, and inside
// scope. A customer, device, hostname or customer name that matches nothing is
// unresolvable (false); one that also matches something outside scope is false.
func (r viewRefs) allInScope(tx *Tx, scope Scope) (bool, error) {
	for _, id := range r.Customers {
		if !scope.Allows(id) {
			return false, nil
		}
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM customers WHERE id = ?`, id).Scan(&one); err != nil {
			return false, noRowsIsFalse(err)
		}
	}
	for _, id := range r.Devices {
		var cust string
		if err := tx.QueryRow(`SELECT customer_id FROM devices WHERE id = ?`, id).Scan(&cust); err != nil {
			return false, noRowsIsFalse(err)
		}
		if !scope.Allows(cust) {
			return false, nil
		}
	}
	for _, h := range r.Hostnames {
		if ok, err := textRefInScope(tx, scope, `SELECT customer_id FROM devices WHERE `, `hostname`, h); err != nil || !ok {
			return false, err
		}
	}
	for _, n := range r.CustomerNames {
		if ok, err := textRefInScope(tx, scope, `SELECT id FROM customers WHERE `, `name`, n); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// textRefInScope resolves a text reference: every row it matches must be inside
// scope, and it must match at least one.
func textRefInScope(tx *Tx, scope Scope, selectFrom, col string, ref textRef) (bool, error) {
	var cond string
	arg := ref.Value
	switch ref.Op {
	case "eq", "ne":
		cond = col + ` = ? COLLATE NOCASE`
	case "contains":
		cond = `instr(lower(` + col + `), lower(?)) > 0`
	case "starts_with":
		cond, arg = `lower(`+col+`) LIKE lower(?) ESCAPE '\'`, escapeLike(ref.Value)+"%"
	default:
		return false, nil
	}
	rows, err := tx.Query(selectFrom+cond, arg)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var cust string
		if err := rows.Scan(&cust); err != nil {
			return false, err
		}
		if !scope.Allows(cust) {
			return false, nil
		}
		n++
	}
	return n > 0, rows.Err()
}

// noRowsIsFalse turns "no such row" into a plain false answer; any other error stays an error.
func noRowsIsFalse(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
