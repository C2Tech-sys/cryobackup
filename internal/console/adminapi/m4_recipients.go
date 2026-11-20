package adminapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// Mail recipients and the delivery log (ADR 0020 decisions 3, 9 and 10). A recipient is a global
// e-mail notification rule (store/recipients_m4.go): the routes live under
// /api/v1/notification-rules as architecture §5.2 names them, and M4 lists, writes and deletes
// rules of scope global and channel email only. Address and name validation is the store's.

// RecipientJSON is one recipient.
type RecipientJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	// Alerts: immediate mail when an alarm opens. Recoveries: one notice when it ends. Digest: the daily digest.
	Alerts     bool      `json:"alerts"`
	Recoveries bool      `json:"recoveries"`
	Digest     bool      `json:"digest"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RecipientCounts says who receives what after the answer's change (or now, for the list).
// NoAlertRecipient is the condition of banner C18 when a relay is configured: alarms are mailed to
// nobody, and Notice says so in a fixed sentence.
type RecipientCounts struct {
	AlertRecipients  int    `json:"alert_recipients"`
	DigestRecipients int    `json:"digest_recipients"`
	NoAlertRecipient bool   `json:"no_alert_recipient"`
	Notice           string `json:"notice,omitempty"`
}

// noAlertRecipientNotice is the fixed sentence of RecipientCounts.Notice.
const noAlertRecipientNotice = "No recipient receives alerts now: the console mails alarms to nobody."

// RecipientsResponse is GET /api/v1/notification-rules.
type RecipientsResponse struct {
	ComputedAt time.Time       `json:"computed_at"`
	Recipients []RecipientJSON `json:"recipients"`
	RecipientCounts
}

// RecipientRequest is the body of POST (name and address required; a switch left out is on) and
// PATCH (every field optional; what is left out keeps its value).
type RecipientRequest struct {
	Name       *string `json:"name"`
	Address    *string `json:"address"`
	Alerts     *bool   `json:"alerts"`
	Recoveries *bool   `json:"recoveries"`
	Digest     *bool   `json:"digest"`
}

// RecipientResponse is the answer of POST and PATCH.
type RecipientResponse struct {
	ComputedAt time.Time     `json:"computed_at"`
	Recipient  RecipientJSON `json:"recipient"`
	RecipientCounts
}

// RecipientDeleteResponse is the answer of DELETE: what is left.
type RecipientDeleteResponse struct {
	ComputedAt time.Time `json:"computed_at"`
	ID         string    `json:"id"`
	RecipientCounts
}

func recipientJSON(r store.Recipient) RecipientJSON {
	return RecipientJSON{ID: r.ID, Name: r.Name, Address: r.Address, Alerts: r.Alerts, Recoveries: r.Recoveries, Digest: r.Digest,
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

func recipientCounts(rs []store.Recipient) RecipientCounts {
	var c RecipientCounts
	for _, r := range rs {
		if r.Alerts {
			c.AlertRecipients++
		}
		if r.Digest {
			c.DigestRecipients++
		}
	}
	if c.AlertRecipients == 0 {
		c.NoAlertRecipient, c.Notice = true, noAlertRecipientNotice
	}
	return c
}

// failRecipient is failM4 with the reason of the one conflict a recipient write has.
func (s *Service) failRecipient(w http.ResponseWriter, r *http.Request, route string, err error) {
	if errors.Is(err, store.ErrConflict) {
		m4Fail(w, r, api.CodeConflict, "address_taken", detailAddressTaken, 0)
		return
	}
	s.failM4(w, r, route, err)
}

// countsAfter reads the recipients after a committed change. A read that fails is a 500: the
// change stands (the caller signalled it), but the answer cannot say who receives alerts now.
func (s *Service) countsAfter(w http.ResponseWriter, r *http.Request, p *auth.Principal, route string) (RecipientCounts, bool) {
	rs, err := s.Store.ListRecipients(r.Context(), p.Scope)
	if err != nil {
		s.failM4(w, r, route, err)
		return RecipientCounts{}, false
	}
	return recipientCounts(rs), true
}

func (s *Service) listRecipients(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	rs, err := s.Store.ListRecipients(r.Context(), p.Scope)
	if err != nil {
		s.failM4(w, r, "recipients", err)
		return
	}
	out := RecipientsResponse{ComputedAt: s.now(), Recipients: make([]RecipientJSON, 0, len(rs)), RecipientCounts: recipientCounts(rs)}
	for _, rc := range rs {
		out.Recipients = append(out.Recipients, recipientJSON(rc))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func orBool(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func orStr(v *string, def string) string {
	if v == nil {
		return def
	}
	return *v
}

func (s *Service) createRecipient(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req RecipientRequest
	if !decodeBodyMax(w, r, 4<<10, &req) {
		return
	}
	in := store.RecipientInput{Name: orStr(req.Name, ""), Address: orStr(req.Address, ""),
		Alerts: orBool(req.Alerts, true), Recoveries: orBool(req.Recoveries, true), Digest: orBool(req.Digest, true)}
	rc, err := s.Store.CreateRecipient(r.Context(), p.Scope, in, actorOf(r, p))
	if err != nil {
		s.failRecipient(w, r, "recipient_create", err)
		return
	}
	s.committed(r)
	c, ok := s.countsAfter(w, r, p, "recipient_create")
	if !ok {
		return
	}
	w.Header().Set("Location", "/api/v1/notification-rules/"+rc.ID)
	httpx.WriteJSON(w, http.StatusCreated, RecipientResponse{ComputedAt: s.now(), Recipient: recipientJSON(rc), RecipientCounts: c})
}

func (s *Service) patchRecipient(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	var req RecipientRequest
	if !decodeBodyMax(w, r, 4<<10, &req) {
		return
	}
	// One store call applies the named fields in one transaction: two edits at the same moment
	// that name different fields both stand.
	patch := store.RecipientPatch{Name: req.Name, Address: req.Address, Alerts: req.Alerts, Recoveries: req.Recoveries, Digest: req.Digest}
	rc, err := s.Store.PatchRecipient(r.Context(), p.Scope, r.PathValue("id"), patch, actorOf(r, p))
	if err != nil {
		s.failRecipient(w, r, "recipient_patch", err)
		return
	}
	s.committed(r)
	c, ok := s.countsAfter(w, r, p, "recipient_patch")
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, RecipientResponse{ComputedAt: s.now(), Recipient: recipientJSON(rc), RecipientCounts: c})
}

func (s *Service) deleteRecipient(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id := r.PathValue("id")
	if err := s.Store.DeleteRecipient(r.Context(), p.Scope, id, actorOf(r, p)); err != nil {
		s.failRecipient(w, r, "recipient_delete", err)
		return
	}
	s.committed(r)
	c, ok := s.countsAfter(w, r, p, "recipient_delete")
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, RecipientDeleteResponse{ComputedAt: s.now(), ID: id, RecipientCounts: c})
}

// ---- delivery log ----

// DeliveryJSON is one row of the delivery log. The log keeps no address, subject or body
// (store/deliveries_m4.go): a row is a kind, an outcome, class words, counts and times, and the
// recipient's name is looked up from its rule while the rule exists.
type DeliveryJSON struct {
	ID string `json:"id"`
	// At is the time of the newest attempt or decision; AtLocal the same in the display zone.
	At       time.Time `json:"at"`
	AtLocal  string    `json:"at_local"`
	QueuedAt time.Time `json:"queued_at"`
	Kind     string    `json:"kind"`
	// RecipientID is the rule the message was for and Recipient its name. Both are empty for a test
	// to a typed address, and for a message whose rule was deleted since (RecipientDeleted true).
	RecipientID      string `json:"recipient_id,omitempty"`
	Recipient        string `json:"recipient,omitempty"`
	RecipientDeleted bool   `json:"recipient_deleted,omitempty"`
	// Outcome is queued, sent, failed or suppressed.
	Outcome        string     `json:"outcome"`
	Attempt        int        `json:"attempt"`
	ErrorClass     string     `json:"error_class,omitempty"`
	ErrorHint      string     `json:"error_hint,omitempty"`
	SuppressReason string     `json:"suppress_reason,omitempty"`
	NextRetryAt    *time.Time `json:"next_retry_at,omitempty"`
}

// DeliveryLogResponse is GET /api/v1/notification-log, newest first, one page at a time.
type DeliveryLogResponse struct {
	ComputedAt      time.Time      `json:"computed_at"`
	DisplayTimezone string         `json:"display_timezone"`
	Truncated       bool           `json:"truncated"`
	NextCursor      string         `json:"next_cursor,omitempty"`
	Deliveries      []DeliveryJSON `json:"deliveries"`
}

var deliveryKinds = map[string]bool{store.DeliveryAlert: true, store.DeliveryRecovery: true, store.DeliveryNag: true, store.DeliveryDigest: true,
	store.DeliveryReport: true, store.DeliveryTest: true, store.DeliveryEvent: true}

var deliveryOutcomes = map[string]bool{store.DeliveryQueued: true, store.DeliverySent: true, store.DeliveryFailed: true, store.DeliverySuppressed: true}

func (s *Service) listDeliveries(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	offset, limit, ok := pageParams(q, 500)
	if !ok {
		httpx.Problem(w, r, api.CodeInvalidRequest, 0)
		return
	}
	f := store.DeliveryFilter{Kind: q.Get("kind"), Result: q.Get("outcome")}
	var bad []store.FieldError
	if f.Kind != "" && !deliveryKinds[f.Kind] {
		bad = append(bad, store.FieldError{Field: "kind", Message: "Use alert, recovery, nag, digest, report, test or event."})
	}
	if f.Result != "" && !deliveryOutcomes[f.Result] {
		bad = append(bad, store.FieldError{Field: "outcome", Message: "Use queued, sent, failed or suppressed."})
	}
	if len(bad) > 0 {
		s.failM2(w, r, &store.ValidationError{Fields: bad})
		return
	}
	page := store.Page{Limit: limit}
	if offset > 0 {
		page.Cursor = "o" + strconv.Itoa(offset)
	}
	l, err := s.Store.ListRecentDeliveries(r.Context(), p.Scope, f, page)
	if err != nil {
		s.failM4(w, r, "delivery_log", err)
		return
	}
	rs, err := s.Store.ListRecipients(r.Context(), p.Scope)
	if err != nil {
		s.failM4(w, r, "delivery_log", err)
		return
	}
	names := make(map[string]string, len(rs))
	for _, rc := range rs {
		names[rc.ID] = rc.Name
	}
	disp := s.display()
	out := DeliveryLogResponse{ComputedAt: s.now(), DisplayTimezone: disp.Zone, NextCursor: l.NextCursor, Truncated: l.NextCursor != "",
		Deliveries: make([]DeliveryJSON, 0, len(l.Items))}
	for _, d := range l.Items {
		j := DeliveryJSON{ID: d.ID, At: d.SentAt.UTC(), AtLocal: displayStamp(d.SentAt, disp.Loc), QueuedAt: d.CreatedAt.UTC(), Kind: d.Kind,
			RecipientID: d.RuleID, Outcome: d.Result, Attempt: d.Attempt, ErrorClass: d.ErrorClass, SuppressReason: d.SuppressReason, NextRetryAt: utcPtr(d.NextRetryAt)}
		switch name, found := names[d.RuleID]; {
		case found:
			j.Recipient = name
		case d.RuleID != "" || d.Kind != store.DeliveryTest:
			// Deleting a rule sets its rows' rule_id to NULL (the log outlives the rule); only a test
			// to a typed address never had one.
			j.RecipientDeleted = true
		}
		if d.ErrorClass != "" {
			j.ErrorHint = mail.Hint(mail.Class(d.ErrorClass))
		}
		out.Deliveries = append(out.Deliveries, j)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
