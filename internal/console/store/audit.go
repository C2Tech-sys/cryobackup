package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// AuditEvent is one row of the immutable, path-free audit trail. The table has
// no foreign keys by design: a row must outlive the admin, customer and device
// it describes, so the ids here are plain text. Rows cannot be updated (a
// trigger aborts it); only the retention job deletes, by age.
type AuditEvent struct {
	ID         string
	OccurredAt time.Time // zero = now (store clock)
	ActorType  string    // admin | api_token | device | system | cli
	ActorID    string
	ActorLabel string // username / hostname snapshot
	Method     string // web | api | agent | system | cli ("" = system)
	SourceIP   string
	Action     string
	ObjectType string
	ObjectID   string
	CustomerID string
	Result     string // ok | denied | error ("" = ok)
	Severity   string // info | warning | critical ("" = info)
	// Details is a JSON object; it must be path-free (architecture §8.4). The
	// store rejects text that looks like a file path as a last line of defence;
	// the caller's own path-free guard comes first.
	Details   json.RawMessage
	CreatedAt time.Time
}

const auditCols = `id, occurred_at, actor_type, actor_id, actor_label, method, source_ip, action, object_type, object_id,
	customer_id, result, severity, details, created_at`

func scanAudit(sc scanner) (AuditEvent, error) {
	var e AuditEvent
	var actorID, label, ip, objType, objID, cust sql.NullString
	var details string
	var occ, c int64
	err := sc.Scan(&e.ID, &occ, &e.ActorType, &actorID, &label, &e.Method, &ip, &e.Action, &objType, &objID, &cust,
		&e.Result, &e.Severity, &details, &c)
	if err != nil {
		return AuditEvent{}, err
	}
	e.OccurredAt, e.ActorID, e.ActorLabel, e.SourceIP = timeOf(occ), actorID.String, label.String, ip.String
	e.ObjectType, e.ObjectID, e.CustomerID = objType.String, objID.String, cust.String
	e.Details, e.CreatedAt = json.RawMessage(details), timeOf(c)
	return e, nil
}

// posixPathRE is an absolute POSIX path of two or more segments, at the start of a value or
// after a space or an opening mark: /home/bob/x.txt. Prose with a slash ("and/or", "on/off",
// "1/2", "gateway/storage") has no slash at a token start, and "/24" has one segment. A URL
// does not match: what comes before its slashes is ":" or a host.
var posixPathRE = regexp.MustCompile(`(?:^|["\s(=,;'])/[^/\s"]+/[^\s"]`)

// looksLikePath is deliberately crude: a drive-letter path, a UNC prefix, a
// "word\word" Windows path or an absolute POSIX path inside a JSON string. It exists so
// a bug elsewhere cannot silently write a client path into the one table that is never
// edited.
func looksLikePath(details string) bool {
	if posixPathRE.MatchString(details) {
		return true
	}
	for i := 0; i+2 < len(details); i++ {
		c := details[i]
		if (c|0x20) >= 'a' && (c|0x20) <= 'z' && details[i+1] == ':' && (details[i+2] == '\\' || details[i+2] == '/') {
			// "x:\" or "x:/" preceded by a non-letter (so a JSON key like "a":/ never matches)
			if i == 0 || !((details[i-1]|0x20) >= 'a' && (details[i-1]|0x20) <= 'z') {
				return true
			}
		}
	}
	// An escaped backslash in the JSON text: a UNC prefix or a Windows path between words.
	return strings.Contains(details, `\\`)
}

// AppendAudit appends an event inside an open Write (so a change and its audit
// row commit together) and returns it with its id and timestamps filled in.
func (tx *Tx) AppendAudit(ev AuditEvent) (AuditEvent, error) {
	if ev.ActorType == "" || ev.Action == "" {
		return AuditEvent{}, invalidf("audit event needs an actor type and an action")
	}
	if ev.Method == "" {
		ev.Method = "system"
	}
	if ev.Result == "" {
		ev.Result = "ok"
	}
	if ev.Severity == "" {
		ev.Severity = "info"
	}
	details, err := jsonOr(ev.Details, "{}")
	if err != nil {
		return AuditEvent{}, err
	}
	if looksLikePath(details) {
		return AuditEvent{}, invalidf("audit details look like a file path; events must be path-free")
	}
	now := tx.nowMS()
	occ := now
	if !ev.OccurredAt.IsZero() {
		occ = msOf(ev.OccurredAt)
	}
	ev.ID = tx.id()
	if _, err := tx.Exec(`INSERT INTO audit_events (`+auditCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.ID, occ, ev.ActorType, nullStr(ev.ActorID), nullStr(ev.ActorLabel), ev.Method, nullStr(ev.SourceIP), ev.Action,
		nullStr(ev.ObjectType), nullStr(ev.ObjectID), nullStr(ev.CustomerID), ev.Result, ev.Severity, details, now); err != nil {
		return AuditEvent{}, err
	}
	ev.OccurredAt, ev.CreatedAt, ev.Details = timeOf(occ), timeOf(now), json.RawMessage(details)
	return ev, nil
}

// AppendAudit appends one event in its own transaction. Prefer Tx.AppendAudit
// inside a Write when the audited change is made by the same caller.
func (s *Store) AppendAudit(ctx context.Context, ev AuditEvent) (AuditEvent, error) {
	var out AuditEvent
	err := s.Write(ctx, func(tx *Tx) error {
		var err error
		out, err = tx.AppendAudit(ev)
		return err
	})
	return out, err
}

// AuditFilter narrows ListAudit. Zero fields do not filter.
type AuditFilter struct {
	CustomerID string
	ActorID    string
	Action     string
	ObjectType string
	ObjectID   string
	Since      time.Time // inclusive
	Until      time.Time // exclusive
}

// ListAudit returns events newest first. A scoped caller sees only events whose
// customer_id is inside its scope: events with no customer (system, login,
// console-wide settings) are visible to Scope.All only.
func (s *Store) ListAudit(ctx context.Context, scope Scope, f AuditFilter, page Page) (Listing[AuditEvent], error) {
	if err := scope.check(); err != nil {
		return Listing[AuditEvent]{}, err
	}
	limit, offset, err := page.window()
	if err != nil {
		return Listing[AuditEvent]{}, err
	}
	var where []string
	var args []any
	if !scope.All {
		clause, a := scope.clause("customer_id")
		where, args = append(where, clause), append(args, a...)
	}
	eq := func(col, v string) {
		if v != "" {
			where, args = append(where, col+" = ?"), append(args, v)
		}
	}
	eq("customer_id", f.CustomerID)
	eq("actor_id", f.ActorID)
	eq("action", f.Action)
	eq("object_type", f.ObjectType)
	eq("object_id", f.ObjectID)
	if !f.Since.IsZero() {
		where, args = append(where, "occurred_at >= ?"), append(args, msOf(f.Since))
	}
	if !f.Until.IsZero() {
		where, args = append(where, "occurred_at < ?"), append(args, msOf(f.Until))
	}
	q := `SELECT ` + auditCols + ` FROM audit_events`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`
	var items []AuditEvent
	err = s.Read(ctx, func(tx *Tx) error {
		var err error
		items, err = collect(tx, q, append(args, limit+1, offset), scanAudit)
		return err
	})
	if err != nil {
		return Listing[AuditEvent]{}, err
	}
	return finish(items, limit, offset), nil
}
