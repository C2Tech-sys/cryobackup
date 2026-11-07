package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T5: the board's filter and the saved-view clause tree.
//
// A saved view (store/viewfilter.go) is a clause tree: {} for no filter, one
// {"field","op","value"} clause, or a group {"op": and|or|not, "clauses": [...]}.
// The store validates the tree when a view is written; this file reads it back
// and applies it. The board's own filters (a state, a ring segment) are turned
// into the same tree, so what a view saves is what the board applies, through
// one path: tree -> JSON -> parse -> match.

// vfNode is one node of a clause tree: a clause (Field set) or a group (Op and Kids).
type vfNode struct {
	Field string
	Op    string
	Value json.RawMessage
	Kids  []*vfNode
}

// parseVF reads a clause tree. {} (or empty) is no filter: nil, nil. It does not
// check field names or values (the store did when the view was written), only
// the shape it needs to apply; a shape it cannot read is an error the page shows.
func parseVF(raw []byte) (*vfNode, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "{}" || string(t) == "null" {
		return nil, nil
	}
	return parseVFNode(t, 1)
}

func parseVFNode(raw []byte, depth int) (*vfNode, error) {
	if depth > 6 {
		return nil, fmt.Errorf("nested too deep")
	}
	var m struct {
		Field   string            `json:"field"`
		Op      string            `json:"op"`
		Value   json.RawMessage   `json:"value"`
		Clauses []json.RawMessage `json:"clauses"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Field != "" {
		return &vfNode{Field: m.Field, Op: m.Op, Value: m.Value}, nil
	}
	if (m.Op != "and" && m.Op != "or" && m.Op != "not") || len(m.Clauses) == 0 {
		return nil, fmt.Errorf("not a clause or a group")
	}
	n := &vfNode{Op: m.Op}
	for _, k := range m.Clauses {
		kid, err := parseVFNode(k, depth+1)
		if err != nil {
			return nil, err
		}
		n.Kids = append(n.Kids, kid)
	}
	return n, nil
}

// devFacts is what a device clause reads: the engine's verdict and the registry row.
// namesCustomer reports whether the tree asks for this customer: a customer clause
// listing it, outside any "not".
func (n *vfNode) namesCustomer(id string) bool {
	if n == nil || n.Op == "not" {
		return false
	}
	for _, k := range n.Kids {
		if k.namesCustomer(id) {
			return true
		}
	}
	return n.Field == "customer" && n.Op == "in" && containsStr(n.list(), id)
}

type devFacts struct {
	DS    state.DeviceState
	OS    string
	Class string
}

// match applies the tree to one device. now is the evaluation time. A device has
// no tags yet, so a tag clause matches nothing (its negation matches everything).
func (n *vfNode) match(d devFacts, now time.Time) bool {
	if n == nil {
		return true
	}
	switch n.Op {
	case "and":
		for _, k := range n.Kids {
			if !k.match(d, now) {
				return false
			}
		}
		return true
	case "or":
		for _, k := range n.Kids {
			if k.match(d, now) {
				return true
			}
		}
		return false
	case "not": // not (a and b ...): the store's rule for a group of several is "none of them"
		for _, k := range n.Kids {
			if k.match(d, now) {
				return false
			}
		}
		return true
	}
	return n.clause(d, now)
}

func (n *vfNode) list() []string {
	var out []string
	_ = json.Unmarshal(n.Value, &out)
	return out
}

func (n *vfNode) text() string {
	var s string
	_ = json.Unmarshal(n.Value, &s)
	return s
}

func textOp(op, have, want string) bool {
	h, w := strings.ToLower(have), strings.ToLower(want)
	switch op {
	case "eq":
		return h == w
	case "ne":
		return h != w
	case "contains":
		return strings.Contains(h, w)
	case "starts_with":
		return strings.HasPrefix(h, w)
	}
	return false
}

// parseDur reads "15m", "24h", "3d".
func parseDur(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &n); err != nil || n < 0 {
		return 0, false
	}
	switch s[len(s)-1] {
	case 'm':
		return time.Duration(n) * time.Minute, true
	case 'h':
		return time.Duration(n) * time.Hour, true
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}

func (n *vfNode) clause(d devFacts, now time.Time) bool {
	ds := d.DS
	switch n.Field {
	case "state":
		in := containsStr(n.list(), string(ds.Primary))
		return in == (n.Op == "in")
	case "customer":
		in := containsStr(n.list(), ds.CustomerID)
		return in == (n.Op == "in")
	case "device_class":
		in := containsStr(n.list(), d.Class)
		return in == (n.Op == "in")
	case "hostname":
		return textOp(n.Op, ds.Hostname, n.text())
	case "agent_version":
		return textOp(n.Op, ds.Detail.AgentVersion, n.text())
	case "os":
		return textOp(n.Op, d.OS, n.text())
	case "tag":
		return n.Op == "ne"
	case "last_success_age":
		ls := ds.Detail.LastSuccess
		if n.Op == "never" {
			return ls.IsZero()
		}
		dur, ok := parseDur(n.text())
		if !ok || ls.IsZero() {
			return false
		}
		age := now.Sub(ls)
		if n.Op == "older_than" {
			return age > dur
		}
		return age <= dur // newer_than
	}
	return false
}

// ---- the board's own filter ----

// Age ring segments (components 4.4) and the customer ring segments.
var (
	ageSegs  = []string{"under24", "mid", "over", "parked"}
	custSegs = []string{"ok", "warn", "crit", "other"}
)

// boardQuery is the board's filter, all of it in the query string: state
// (repeatable), age (a segment of the age ring), cust (a segment of the customer
// ring) and view (a saved view's id).
type boardQuery struct {
	States []string
	Age    string
	Cust   string
	View   string
}

func parseBoardQuery(v url.Values) boardQuery {
	var q boardQuery
	for _, s := range v["state"] {
		s = strings.ToUpper(strings.TrimSpace(s))
		if validStateName(s) && !containsStr(q.States, s) {
			q.States = append(q.States, s)
		}
	}
	if a := v.Get("age"); containsStr(ageSegs, a) {
		q.Age = a
	}
	if c := v.Get("cust"); containsStr(custSegs, c) {
		q.Cust = c
	}
	if id := v.Get("view"); idParam(id) {
		q.View = id
	}
	return q
}

func (q boardQuery) values() url.Values {
	out := url.Values{}
	for _, s := range q.States {
		out.Add("state", s)
	}
	if q.Age != "" {
		out.Set("age", q.Age)
	}
	if q.Cust != "" {
		out.Set("cust", q.Cust)
	}
	if q.View != "" {
		out.Set("view", q.View)
	}
	return out
}

// Encode is the canonical query string ("" when there is no filter).
func (q boardQuery) Encode() string { return q.values().Encode() }

// QS is "?" and the query, or "".
func (q boardQuery) QS() string {
	if e := q.Encode(); e != "" {
		return "?" + e
	}
	return ""
}

func (q boardQuery) empty() bool { return q.Encode() == "" }

// withAge and withCust set (or, when it is already set, clear) a ring filter.
func (q boardQuery) withAge(seg string) boardQuery {
	if q.Age == seg {
		seg = ""
	}
	q.Age = seg
	return q
}

func (q boardQuery) withCust(seg string) boardQuery {
	if q.Cust == seg {
		seg = ""
	}
	q.Cust = seg
	return q
}

func (q boardQuery) withoutState(s string) boardQuery {
	var keep []string
	for _, x := range q.States {
		if x != s {
			keep = append(keep, x)
		}
	}
	q.States = keep
	return q
}

func href(q boardQuery) string { return "/" + q.QS() }

// customerSeg is the customer-ring segment a customer falls in.
func customerSeg(s state.CustomerStatus) string {
	switch s {
	case state.CustomerOK:
		return "ok"
	case state.CustomerWarn:
		return "warn"
	case state.CustomerAlarm, state.CustomerMissing, state.CustomerUnknown:
		return "crit"
	}
	return "other"
}

// ageClauses is the clause tree of the age ring segment seg (the ring's own buckets:
// parked devices are their own segment, retired ones are in none).
func ageClauses(seg string, stale time.Duration) map[string]any {
	h := fmt.Sprintf("%dh", int(stale/time.Hour))
	notParked := clauseOf("state", "not_in", []string{string(state.StatePaused), string(state.StateRetired)})
	switch seg {
	case "under24":
		return groupOf("and", notParked, clauseOf("last_success_age", "newer_than", "24h"))
	case "mid":
		return groupOf("and", notParked, clauseOf("last_success_age", "older_than", "24h"), clauseOf("last_success_age", "newer_than", h))
	case "over":
		return groupOf("and", notParked, groupOf("or", clauseOf("last_success_age", "older_than", h), clauseOf("last_success_age", "never", nil)))
	case "parked":
		return clauseOf("state", "in", []string{string(state.StatePaused)})
	}
	return nil
}

func clauseOf(field, op string, value any) map[string]any {
	m := map[string]any{"field": field, "op": op}
	if value != nil {
		m["value"] = value
	}
	return m
}

func groupOf(op string, kids ...map[string]any) map[string]any {
	cl := make([]any, 0, len(kids))
	for _, k := range kids {
		if k != nil {
			cl = append(cl, k)
		}
	}
	return map[string]any{"op": op, "clauses": cl}
}

// ownClauses are the clauses the query's own filters (not the view) stand for.
func (q boardQuery) ownClauses(stale time.Duration) []map[string]any {
	var out []map[string]any
	if len(q.States) > 0 {
		out = append(out, clauseOf("state", "in", q.States))
	}
	if q.Age != "" {
		out = append(out, ageClauses(q.Age, stale))
	}
	return out
}

// filterJSON is the tree to save: the loaded view's tree (if any) and the query's
// own filters, all of which must hold. {} when there is nothing.
func (q boardQuery) filterJSON(view json.RawMessage, stale time.Duration) json.RawMessage {
	var kids []map[string]any
	if t := bytes.TrimSpace(view); len(t) > 0 && string(t) != "{}" {
		var m map[string]any
		if json.Unmarshal(t, &m) == nil {
			kids = append(kids, m)
		}
	}
	kids = append(kids, q.ownClauses(stale)...)
	switch len(kids) {
	case 0:
		return json.RawMessage(`{}`)
	case 1:
		b, _ := json.Marshal(kids[0])
		return b
	}
	b, _ := json.Marshal(groupOf("and", kids...))
	return b
}

// ---- saved views of runs (S21) ----

// runsFilterJSON turns a run search into a clause tree: the conditions of one
// field are one "in" clause; the two dates are one between (or after, or before).
func runsFilterJSON(q runQuery) json.RawMessage {
	var kids []map[string]any
	for _, f := range []struct{ param, field string }{{"status", "outcome"}, {"kind", "kind"}, {"customer", "customer"}, {"device", "device"}} {
		if v := q.of(f.param); len(v) > 0 {
			kids = append(kids, clauseOf(f.field, "in", v))
		}
	}
	from, to := q.date("from"), q.date("to")
	switch {
	case from != "" && to != "":
		kids = append(kids, clauseOf("date", "between", map[string]string{"from": from, "to": to}))
	case from != "":
		kids = append(kids, clauseOf("date", "after", from))
	case to != "":
		kids = append(kids, clauseOf("date", "before", to))
	}
	switch len(kids) {
	case 0:
		return json.RawMessage(`{}`)
	case 1:
		b, _ := json.Marshal(kids[0])
		return b
	}
	b, _ := json.Marshal(groupOf("and", kids...))
	return b
}

// runsQueryOf reads a saved runs view back into search parameters. A tree the
// flat builder cannot show (an or or not group, a hostname clause) is not
// applied: ok is false and the page says so, never a silently different search.
func runsQueryOf(raw []byte) (url.Values, bool) {
	root, err := parseVF(raw)
	if err != nil {
		return nil, false
	}
	out := url.Values{}
	if root == nil {
		return out, true
	}
	nodes := []*vfNode{root}
	if root.Op == "and" {
		nodes = root.Kids
	}
	for _, n := range nodes {
		if n.Field == "" {
			return nil, false
		}
		switch n.Field {
		case "outcome", "kind", "customer", "device":
			if n.Op != "in" {
				return nil, false
			}
			param := map[string]string{"outcome": "status"}[n.Field]
			if param == "" {
				param = n.Field
			}
			for _, v := range n.list() {
				out.Add(param, v)
			}
		case "date":
			switch n.Op {
			case "after":
				out.Set("from", dayOf(n.text()))
			case "before":
				out.Set("to", dayOf(n.text()))
			case "between":
				var r struct{ From, To string }
				var m map[string]string
				if json.Unmarshal(n.Value, &m) != nil {
					return nil, false
				}
				r.From, r.To = m["from"], m["to"]
				out.Set("from", dayOf(r.From))
				out.Set("to", dayOf(r.To))
			default:
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return out, true
}

// dayOf is the YYYY-MM-DD of a date given as a day or as RFC 3339.
func dayOf(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// compile-time use of the store type the views carry.
var _ = store.ViewEntities
