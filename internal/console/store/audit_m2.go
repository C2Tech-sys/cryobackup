package store

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The audit viewer's reads (M2-W3A, architecture §5.2 GET /audit, screen B09).
// Every method takes a Scope: an event is visible when its customer_id is in
// scope, and an event with no customer (sign-ins, settings, system) is visible
// to Scope.All only. Nothing here writes; the audit write path is audit.go.

// AuditResults, in display order, are the values of the result column.
var AuditResults = []string{"ok", "denied", "error"}

// AuditQuery is the filter and the page of the audit table. Zero fields do not
// filter. Since is inclusive and Until exclusive, both UTC instants (the caller
// converts the operator's dates in the display zone).
type AuditQuery struct {
	// Actor matches the actor id exactly, or the actor label case-insensitively.
	Actor string
	// Action is an action code ("policy.update"), or a family ending in ".*"
	// ("policy.*").
	Action     string
	ObjectType string
	CustomerID string
	Result     string
	Since      time.Time
	Until      time.Time
	Page, Size int
}

var actionToken = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*(\.\*)?$`)
var objectToken = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// Validate returns one FieldError per filter that cannot match anything
// meaningful, in fixed sentences that never echo the input.
func (q AuditQuery) Validate() []FieldError {
	var bad []FieldError
	if q.Action != "" && (len(q.Action) > 80 || !actionToken.MatchString(q.Action)) {
		bad = append(bad, fieldf("action", "Use an action code such as policy.update, or a family such as policy.*."))
	}
	if q.ObjectType != "" && !objectToken.MatchString(q.ObjectType) {
		bad = append(bad, fieldf("object_type", "Use an object type such as customer or device."))
	}
	if q.Result != "" {
		found := false
		for _, r := range AuditResults {
			found = found || r == q.Result
		}
		if !found {
			bad = append(bad, fieldf("result", "Use ok, denied or error."))
		}
	}
	if len(q.Actor) > 100 || strings.ContainsAny(q.Actor, "\r\n\x00") {
		bad = append(bad, fieldf("actor", "Use a username or an account id."))
	}
	if len(q.CustomerID) > 40 || strings.ContainsAny(q.CustomerID, "\r\n\x00") {
		bad = append(bad, fieldf("customer", "Choose one of the listed customers."))
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Since.Before(q.Until) {
		bad = append(bad, fieldf("to", "The end of the range is before its start."))
	}
	return bad
}

func (q AuditQuery) where(scope Scope) (string, []any) {
	var where []string
	var args []any
	clause, a := scope.clause("customer_id")
	where, args = append(where, clause), append(args, a...)
	if q.Actor != "" {
		where, args = append(where, "(actor_id = ? OR actor_label = ? COLLATE NOCASE)"), append(args, q.Actor, q.Actor)
	}
	if q.Action != "" {
		if strings.HasSuffix(q.Action, ".*") {
			where, args = append(where, `action LIKE ? ESCAPE '\'`), append(args, escapeLike(strings.TrimSuffix(q.Action, "*"))+"%")
		} else {
			where, args = append(where, "action = ?"), append(args, q.Action)
		}
	}
	if q.ObjectType != "" {
		where, args = append(where, "object_type = ?"), append(args, q.ObjectType)
	}
	if q.CustomerID != "" {
		where, args = append(where, "customer_id = ?"), append(args, q.CustomerID)
	}
	if q.Result != "" {
		where, args = append(where, "result = ?"), append(args, q.Result)
	}
	if !q.Since.IsZero() {
		where, args = append(where, "occurred_at >= ?"), append(args, msOf(q.Since))
	}
	if !q.Until.IsZero() {
		where, args = append(where, "occurred_at < ?"), append(args, msOf(q.Until))
	}
	return strings.Join(where, " AND "), args
}

// AuditTable is one page of the audit listing with its totals.
type AuditTable struct {
	Rows []AuditEvent
	Page PageInfo
	// Total is the number of events in scope without the filters, so an empty
	// result can say "0 of N".
	Total int
}

// AuditTable returns the events matching q, newest first, one numbered page of
// them (windowOf clamps a page past the end to the last page). An invalid
// filter is a *ValidationError.
func (s *Store) AuditTable(ctx context.Context, scope Scope, q AuditQuery) (AuditTable, error) {
	if err := scope.check(); err != nil {
		return AuditTable{}, err
	}
	if bad := q.Validate(); len(bad) > 0 {
		return AuditTable{}, validation(bad)
	}
	where, args := q.where(scope)
	allWhere, allArgs := AuditQuery{}.where(scope)
	var out AuditTable
	err := s.Read(ctx, func(tx *Tx) error {
		var matching int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE `+where, args...).Scan(&matching); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE `+allWhere, allArgs...).Scan(&out.Total); err != nil {
			return err
		}
		off, lim, info := Paginate(matching, q.Page, q.Size)
		out.Page = info
		rows, err := collect(tx, `SELECT `+auditCols+` FROM audit_events WHERE `+where+` ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`,
			append(append([]any(nil), args...), lim, off), scanAudit)
		out.Rows = rows
		return err
	})
	return out, err
}

// AuditTableAt is AuditTable with a row offset and limit instead of a page
// number, for the JSON API whose cursor is a row offset.
func (s *Store) AuditTableAt(ctx context.Context, scope Scope, q AuditQuery, offset, limit int) (AuditTable, error) {
	if err := scope.check(); err != nil {
		return AuditTable{}, err
	}
	if bad := q.Validate(); len(bad) > 0 {
		return AuditTable{}, validation(bad)
	}
	where, args := q.where(scope)
	allWhere, allArgs := AuditQuery{}.where(scope)
	var out AuditTable
	err := s.Read(ctx, func(tx *Tx) error {
		var matching int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE `+where, args...).Scan(&matching); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE `+allWhere, allArgs...).Scan(&out.Total); err != nil {
			return err
		}
		from, to, info := windowOf(matching, 0, 0, offset, limit)
		out.Page = info
		if to <= from {
			return nil
		}
		rows, err := collect(tx, `SELECT `+auditCols+` FROM audit_events WHERE `+where+` ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`,
			append(append([]any(nil), args...), to-from, from), scanAudit)
		out.Rows = rows
		return err
	})
	return out, err
}

// GetAuditEvent returns one event, or ErrNotFound when it does not exist or lies
// outside scope (indistinguishable by design).
func (s *Store) GetAuditEvent(ctx context.Context, scope Scope, id string) (AuditEvent, error) {
	if err := scope.check(); err != nil {
		return AuditEvent{}, err
	}
	clause, cargs := scope.clause("customer_id")
	var out AuditEvent
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+auditCols+` FROM audit_events WHERE id = ? AND `+clause, append([]any{id}, cargs...), scanAudit)
		return err
	})
	return out, err
}

// AuditFacets are the values the audit filters offer: what the table actually
// holds within scope, so every choice matches at least one event.
type AuditFacets struct {
	Actions     []string
	ObjectTypes []string
	Actors      []string
}

const facetLimit = 500

// AuditFacets lists the distinct actions, object types and actor labels in
// scope (at most facetLimit of each).
func (s *Store) AuditFacets(ctx context.Context, scope Scope) (AuditFacets, error) {
	if err := scope.check(); err != nil {
		return AuditFacets{}, err
	}
	clause, args := scope.clause("customer_id")
	var f AuditFacets
	err := s.Read(ctx, func(tx *Tx) error {
		list := func(col string) ([]string, error) {
			return collect(tx, `SELECT DISTINCT `+col+` FROM audit_events WHERE `+clause+` AND `+col+` IS NOT NULL AND `+col+` <> '' ORDER BY `+col+` COLLATE NOCASE LIMIT `+strconv.Itoa(facetLimit),
				args, func(sc scanner) (string, error) { var v string; return v, sc.Scan(&v) })
		}
		var err error
		if f.Actions, err = list("action"); err != nil {
			return err
		}
		if f.ObjectTypes, err = list("object_type"); err != nil {
			return err
		}
		f.Actors, err = list("actor_label")
		return err
	})
	sort.Strings(f.Actions)
	return f, err
}

// AuditDetail is the stored details of an event, flattened for display. The
// audit store refuses path-like text on write; this view refuses it again on
// read, and never shows a value stored under a secret-looking key, so a row
// written by an older build or a bug cannot leak through the viewer.
type AuditDetail struct {
	Key, Value string
	// Withheld says the value was not shown, and why in a fixed sentence.
	Withheld string
}

// secretKey matches a detail key that suggests a secret, anywhere in the name
// and in any case: token, code, totp, secret, password, passphrase, key,
// credential, private, recovery. It errs on the side of withholding, so a key
// that only mentions one of these words is listed in safeKeys or matched by
// safeKeySuffixes first.
var secretKey = regexp.MustCompile(`(?i)(token|code|totp|secret|passw|passphrase|key|credential|private|recovery)`)

// safeKeys are detail keys the audit writers use for plain facts that contain a
// secret-looking word: the name of a setting, an exit or HTTP status, and counts
// of revoked tokens or ended sessions. They are checked before secretKey.
var safeKeys = map[string]bool{
	"key": true, "exit_code": true, "status_code": true, "tokens_revoked": true, "sessions_ended": true,
}

// safeKeySuffixes are the counters: "recovery_codes_left", "token_count". They are safe only while the
// value is a JSON number: any other value under such a key goes back to the secret pattern, so
// "recovery_codes_left": "abc123" is withheld.
var safeKeySuffixes = []string{"_count", "_left"}

// secretLooking reports whether the detail k with value v suggests a secret: the allow-list first, then the
// pattern. v is the decoded value (a json.Number for a number, see decodeNumbers).
func secretLooking(k string, v any) bool {
	// Exact, lower-case names only: the audit writers use these spellings, and a
	// bare "KEY" or "Token" is still withheld.
	if safeKeys[k] {
		return false
	}
	for _, suf := range safeKeySuffixes {
		if strings.HasSuffix(k, suf) {
			if _, isNum := v.(json.Number); isNum {
				return false
			}
			return secretKey.MatchString(k) // not a number: no longer a counter, so the pattern decides
		}
	}
	return secretKey.MatchString(k)
}

const (
	withheldSecret = "Not shown: the key name suggests a secret."
	withheldPath   = "Not shown: the value looks like a file path."
)

// DetailsLookLikePath reports whether text would be refused by the audit write
// path as a file path.
func DetailsLookLikePath(text string) bool { return looksLikePath(text) }

// AuditDetails flattens an event's details object into sorted key/value lines.
// Nested objects are walked: a nested key is shown as "outer.inner", and a
// nested secret-looking key or path-looking value is withheld on its own line,
// never dumped as part of a compact JSON string. A list of plain values is
// shown as compact JSON; a value longer than 400 runes is cut with an ellipsis
// (the stored row is unchanged).
func AuditDetails(raw json.RawMessage) []AuditDetail {
	if len(raw) == 0 {
		return nil
	}
	v, err := decodeNumbers(raw)
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		return []AuditDetail{{Key: "details", Withheld: "The stored details are not a JSON object, so they are not shown."}}
	}
	var out []AuditDetail
	walkDetails("", m, &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func decodeNumbers(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

func joinKey(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

func textOfValue(v any) string {
	if str, ok := v.(string); ok {
		return str
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func walkDetails(prefix string, m map[string]any, out *[]AuditDetail) {
	for k, v := range m {
		key := joinKey(prefix, k)
		switch {
		case secretLooking(k, v):
			*out = append(*out, AuditDetail{Key: key, Withheld: withheldSecret})
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			if len(t) == 0 {
				*out = append(*out, AuditDetail{Key: key, Value: "{}"})
				continue
			}
			walkDetails(key, t, out)
		case []any:
			walkList(key, t, out)
		default:
			*out = append(*out, leafDetail(key, v))
		}
	}
}

func walkList(key string, list []any, out *[]AuditDetail) {
	nested := false
	for _, e := range list {
		switch e.(type) {
		case map[string]any, []any:
			nested = true
		}
	}
	if !nested {
		*out = append(*out, leafDetail(key, list))
		return
	}
	for i, e := range list {
		ek := key + "[" + strconv.Itoa(i) + "]"
		switch t := e.(type) {
		case map[string]any:
			walkDetails(ek, t, out)
		case []any:
			walkList(ek, t, out)
		default:
			*out = append(*out, leafDetail(ek, e))
		}
	}
}

// pathLike reports whether a detail value would be refused as a file path on
// write. A string is checked decoded and JSON-encoded: the write guard sees the
// encoded text, where a single backslash is `\\`, so a relative `dir\file` is
// a path there although the decoded text has no doubled backslash.
func pathLike(v any) bool {
	txt := textOfValue(v)
	if looksLikePath(txt) {
		return true
	}
	if _, isStr := v.(string); isStr {
		enc, err := json.Marshal(txt)
		return err != nil || looksLikePath(string(enc))
	}
	return false
}

func leafDetail(key string, v any) AuditDetail {
	txt := textOfValue(v)
	if pathLike(v) {
		return AuditDetail{Key: key, Withheld: withheldPath}
	}
	if r := []rune(txt); len(r) > 400 {
		txt = string(r[:400]) + "…"
	}
	return AuditDetail{Key: key, Value: txt}
}

// scrubDetails returns v with every secret-keyed value and path-looking string
// replaced by "[withheld]", at any depth.
func scrubDetails(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if secretLooking(k, e) {
				t[k] = "[withheld]"
				continue
			}
			t[k] = scrubDetails(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = scrubDetails(e)
		}
		return t
	case string:
		if pathLike(t) {
			return "[withheld]"
		}
	}
	return v
}

// SafeAuditDetails returns the details object of an event with every value the
// viewer would withhold replaced by the string "[withheld]", at any depth,
// keeping the JSON type of the rest. Details that are not an object come back
// as an empty object.
func SafeAuditDetails(raw json.RawMessage) json.RawMessage {
	v, err := decodeNumbers(raw)
	m, ok := v.(map[string]any)
	if len(raw) == 0 || err != nil || !ok {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(scrubDetails(m))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
