package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The audit trail (M2-W3A, architecture §5.2 "Audit"): GET /api/v1/audit,
// admin and above. Read-only, scoped (an event is visible when its customer is
// in the caller's scope; console-wide events need an all-customer caller) and
// path-free: the store refuses paths on write and this view withholds anything
// that looks like one, or like a secret, on read. Export is M11.

// PatternAudit is the audit list route.
const PatternAudit = "GET /api/v1/audit"

func auditPatterns() []string { return []string{PatternAudit} }

func (s *Service) registerAudit(r httpx.Registrar) {
	r.Handle(PatternAudit, s.Auth.Require(auth.RoleAdmin, auth.ModeAPI, http.HandlerFunc(s.listAudit)))
}

// AuditEventJSON is one audit event. OccurredAt is the stored UTC instant;
// OccurredLocal is the same instant in the operator's display zone on a
// 12-hour clock, so a script can show it as the console does.
type AuditEventJSON struct {
	ID            string          `json:"id"`
	OccurredAt    time.Time       `json:"occurred_at"`
	OccurredLocal string          `json:"occurred_local"`
	ActorType     string          `json:"actor_type"`
	ActorID       string          `json:"actor_id,omitempty"`
	Actor         string          `json:"actor,omitempty"`
	Method        string          `json:"method"`
	SourceIP      string          `json:"source_ip,omitempty"`
	Action        string          `json:"action"`
	ObjectType    string          `json:"object_type,omitempty"`
	ObjectID      string          `json:"object_id,omitempty"`
	CustomerID    string          `json:"customer_id,omitempty"`
	Result        string          `json:"result"`
	Severity      string          `json:"severity"`
	Details       json.RawMessage `json:"details"`
}

// AuditListResponse is GET /api/v1/audit. Total counts the events that match
// the filters, Of every event the caller may see, so "0 of N" is expressible.
type AuditListResponse struct {
	ComputedAt      time.Time        `json:"computed_at"`
	DisplayTimezone string           `json:"display_timezone"`
	Total           int              `json:"total"`
	Of              int              `json:"of"`
	Truncated       bool             `json:"truncated"`
	NextCursor      string           `json:"next_cursor,omitempty"`
	Events          []AuditEventJSON `json:"events"`
}

// displayStamp is the 12-hour display-zone form of t, e.g. "Mon 2 Jan 2006, 3:04:05 PM CST".
func displayStamp(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon 2 Jan 2006, 3:04:05 PM MST")
}

func auditEventJSON(e store.AuditEvent, loc *time.Location) AuditEventJSON {
	return AuditEventJSON{ID: e.ID, OccurredAt: e.OccurredAt.UTC(), OccurredLocal: displayStamp(e.OccurredAt, loc), ActorType: e.ActorType,
		ActorID: e.ActorID, Actor: e.ActorLabel, Method: e.Method, SourceIP: e.SourceIP, Action: e.Action, ObjectType: e.ObjectType,
		ObjectID: e.ObjectID, CustomerID: e.CustomerID, Result: e.Result, Severity: e.Severity, Details: store.SafeAuditDetails(e.Details)}
}

func (s *Service) listAudit(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	v := r.URL.Query()
	offset, limit, ok := pageParams(v, 500)
	if !ok {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	q := store.AuditQuery{Actor: strings.TrimSpace(v.Get("actor")), Action: strings.TrimSpace(v.Get("action")), ObjectType: strings.TrimSpace(v.Get("object_type")),
		CustomerID: strings.TrimSpace(v.Get("customer_id")), Result: strings.TrimSpace(v.Get("result"))}
	for name, dst := range map[string]*time.Time{"since": &q.Since, "until": &q.Until} {
		if raw := v.Get(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				httpx.Problem(w, r, api.CodeInvalidRequest, 0)
				return
			}
			*dst = t.UTC()
		}
	}
	tb, err := s.Store.AuditTableAt(r.Context(), p.Scope, q, offset, limit)
	if err != nil {
		s.failM2(w, r, err)
		return
	}
	disp := s.display()
	loc := disp.Loc
	resp := AuditListResponse{ComputedAt: s.now(), DisplayTimezone: disp.Zone, Total: tb.Page.Total, Of: tb.Total, Events: make([]AuditEventJSON, 0, len(tb.Rows))}
	resp.NextCursor, resp.Truncated = nextCursor(tb.Page)
	for _, e := range tb.Rows {
		resp.Events = append(resp.Events, auditEventJSON(e, loc))
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
