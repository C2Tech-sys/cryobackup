package adminapi

import (
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Acknowledgements and snoozes (ADR 0020 decision 7, matrix row r163; store/acks_m4.go). An
// acknowledgement mutes mail for one alarm (subject and alarm type) and labels the row; it never
// changes a state (DH8). It answers an open alarm only: store.ErrAlarmNotOpen is a 409 with reason
// alarm_not_open. The note is free text an operator types: it is stored and shown in the console
// (this list), never mailed, never logged and never copied to the audit trail.
//
// The longest dated snooze is the store's (store.MaxSnooze), so the API and the console's own
// acknowledge form accept the same dates; the store refuses a snooze end that is not in the future.

// AckJSON is one acknowledgement or snooze in force.
type AckJSON struct {
	ID          string `json:"id"`
	SubjectType string `json:"subject_type"` // device or customer
	SubjectID   string `json:"subject_id"`
	CustomerID  string `json:"customer_id"`
	AlarmType   string `json:"alarm_type"`
	// Kind is acknowledge (in force until cleared) or snooze (until SnoozeUntil, or until the next
	// success when UntilNextSuccess).
	Kind             string     `json:"kind"`
	SnoozeUntil      *time.Time `json:"snooze_until,omitempty"`
	UntilNextSuccess bool       `json:"until_next_success"`
	Note             string     `json:"note"`
	// AcknowledgedBy is the account id, AcknowledgedByName its user name; both empty once the account was deleted.
	AcknowledgedBy     string     `json:"acknowledged_by,omitempty"`
	AcknowledgedByName string     `json:"acknowledged_by_name,omitempty"`
	AcknowledgedAt     time.Time  `json:"acknowledged_at"`
	ClearedAt          *time.Time `json:"cleared_at,omitempty"`
}

// AcksResponse is GET /api/v1/acks: the acknowledgements in force inside the caller's scope, newest first.
type AcksResponse struct {
	ComputedAt      time.Time `json:"computed_at"`
	DisplayTimezone string    `json:"display_timezone"`
	Acks            []AckJSON `json:"acks"`
}

// AckResponse is the answer of POST /api/v1/acks.
type AckResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	AckJSON
}

// AckKind values.
const (
	AckKindAcknowledge = "acknowledge"
	AckKindSnooze      = "snooze"
)

// CreateAckRequest is the body of POST /api/v1/acks. Kind acknowledge takes neither snooze field;
// kind snooze takes exactly one of SnoozeUntil (RFC 3339) and UntilNextSuccess.
type CreateAckRequest struct {
	SubjectType      string     `json:"subject_type"`
	SubjectID        string     `json:"subject_id"`
	AlarmType        string     `json:"alarm_type"`
	Kind             string     `json:"kind"`
	SnoozeUntil      *time.Time `json:"snooze_until"`
	UntilNextSuccess bool       `json:"until_next_success"`
	Note             string     `json:"note"`
}

func ackJSON(a store.Ack, names map[string]string) AckJSON {
	kind := AckKindAcknowledge
	if a.SnoozeUntil != nil || a.UntilNextSuccess {
		kind = AckKindSnooze
	}
	return AckJSON{ID: a.ID, SubjectType: a.SubjectType, SubjectID: a.SubjectID, CustomerID: a.CustomerID, AlarmType: a.AlarmType, Kind: kind,
		SnoozeUntil: utcPtr(a.SnoozeUntil), UntilNextSuccess: a.UntilNextSuccess, Note: a.Note, AcknowledgedBy: a.AcknowledgedBy,
		AcknowledgedByName: names[a.AcknowledgedBy], AcknowledgedAt: a.AcknowledgedAt.UTC(), ClearedAt: utcPtr(a.ClearedAt)}
}

// adminNames resolves the user names of the given account ids; an id that does not resolve is left
// out (a deleted account), and a read error leaves the names empty: they are a label, not the answer.
func (s *Service) adminNames(r *http.Request, acks []store.Ack) map[string]string {
	out := map[string]string{}
	for _, a := range acks {
		if a.AcknowledgedBy == "" {
			continue
		}
		if _, seen := out[a.AcknowledgedBy]; seen {
			continue
		}
		out[a.AcknowledgedBy] = ""
		if ad, err := s.Store.GetAdmin(r.Context(), a.AcknowledgedBy); err == nil {
			out[a.AcknowledgedBy] = ad.Username
		}
	}
	return out
}

func (s *Service) listAcks(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	acks, err := s.Store.ListActiveAcks(r.Context(), p.Scope)
	if err != nil {
		s.failM4(w, r, "acks", err)
		return
	}
	if sid := r.URL.Query().Get("subject_id"); sid != "" {
		kept := acks[:0]
		for _, a := range acks {
			if a.SubjectID == sid {
				kept = append(kept, a)
			}
		}
		acks = kept
	}
	names := s.adminNames(r, acks)
	out := AcksResponse{ComputedAt: s.now(), DisplayTimezone: s.display().Zone, Acks: make([]AckJSON, 0, len(acks))}
	for _, a := range acks {
		out.Acks = append(out.Acks, ackJSON(a, names))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) createAck(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req CreateAckRequest
	if !decodeBodyMax(w, r, 8<<10, &req) {
		return
	}
	switch req.Kind {
	case AckKindAcknowledge:
		if req.SnoozeUntil != nil || req.UntilNextSuccess {
			s.failM2(w, r, &store.ValidationError{Fields: []store.FieldError{{Field: "kind", Message: "An acknowledgement has no snooze end: use kind snooze for one."}}})
			return
		}
	case AckKindSnooze:
		if (req.SnoozeUntil == nil) == !req.UntilNextSuccess {
			s.failM2(w, r, &store.ValidationError{Fields: []store.FieldError{{Field: "snooze_until", Message: "A snooze needs an end: a snooze_until time or until_next_success, not both."}}})
			return
		}
	default:
		s.failM2(w, r, &store.ValidationError{Fields: []store.FieldError{{Field: "kind", Message: "Use acknowledge or snooze."}}})
		return
	}
	a, err := s.Store.CreateAck(r.Context(), p.Scope, store.AckInput{SubjectType: req.SubjectType, SubjectID: req.SubjectID, AlarmType: req.AlarmType,
		Note: req.Note, SnoozeUntil: req.SnoozeUntil, UntilNextSuccess: req.UntilNextSuccess}, actorOf(r, p))
	if err != nil {
		s.failM4(w, r, "ack_create", err)
		return
	}
	s.committed(r)
	w.Header().Set("Location", "/api/v1/acks/"+a.ID)
	httpx.WriteJSON(w, http.StatusCreated, AckResponse{ComputedAt: s.now(), AckJSON: ackJSON(a, map[string]string{p.Admin.ID: p.Admin.Username})})
}

func (s *Service) clearAck(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if _, err := s.Store.ClearAck(r.Context(), p.Scope, r.PathValue("id"), actorOf(r, p)); err != nil {
		s.failM4(w, r, "ack_clear", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
