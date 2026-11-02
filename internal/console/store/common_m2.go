package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// This file holds the small pieces the M2 customer, device and run queries
// share: who is acting (for the audit row), field-level validation errors, and
// numbered paging. The M0 helpers (Page, Listing) are cursor based; the console
// tables show "Showing 51-100 of 212" and numbered pages, so they page by number.

// AuditActor says who makes an audited change and over which transport. The
// M2 mutations write their audit row in the same transaction as the change, so
// a change whose audit row cannot be written is refused (ErrAudit wraps the
// cause) and nothing is changed.
type AuditActor struct {
	AdminID  string
	Username string
	IP       string
	// Method is "web" for the console pages and "api" for the admin JSON API.
	Method string
}

func (a AuditActor) event(action, objectType, objectID, customerID string, details map[string]any) (AuditEvent, error) {
	ev := AuditEvent{ActorType: "admin", ActorID: a.AdminID, ActorLabel: a.Username, Method: a.Method, SourceIP: a.IP,
		Action: action, ObjectType: objectType, ObjectID: objectID, CustomerID: customerID, Result: "ok"}
	if ev.Method == "" {
		ev.Method = "web"
	}
	if len(details) > 0 {
		b, err := json.Marshal(details)
		if err != nil {
			return AuditEvent{}, err
		}
		ev.Details = b
	}
	return ev, nil
}

// ErrAudit means the audit row of a change could not be written, so the change
// was not made ("NOT AUDITED"): the caller reports it and the data is unchanged.
var ErrAudit = errors.New("store: change refused because it could not be audited")

// audited appends the event inside tx; a failure refuses the whole change.
func audited(tx *Tx, ev AuditEvent) error {
	if _, err := tx.AppendAudit(ev); err != nil {
		return fmt.Errorf("%w: %v", ErrAudit, err)
	}
	return nil
}

// FieldError names one rejected input field and says, in a fixed sentence that
// never echoes the input, what is accepted.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError is an ErrInvalid that carries one FieldError per rejected
// field, so a page can mark each field and the API can list them.
type ValidationError struct{ Fields []FieldError }

// Error implements error.
func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Message
	}
	return ErrInvalid.Error() + ": " + strings.Join(parts, "; ")
}

// Unwrap lets errors.Is match ErrInvalid.
func (e *ValidationError) Unwrap() error { return ErrInvalid }

// Message returns the sentence of the named field ("" when it was not rejected).
func (e *ValidationError) Message(field string) string {
	for _, f := range e.Fields {
		if f.Field == field {
			return f.Message
		}
	}
	return ""
}

func validation(fields []FieldError) error {
	if len(fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: fields}
}

// FieldErrors returns the per-field errors inside err (nil when it has none).
func FieldErrors(err error) []FieldError {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Fields
	}
	return nil
}

// PageSizes are the page sizes the console tables offer (components 5.2).
var PageSizes = []int{25, 50, 100, 200}

// DefaultPageSize is the table page size when none is asked for.
const DefaultPageSize = 50

// PageInfo describes the page a numbered listing returned.
type PageInfo struct {
	Total int
	// Page is the 1-based page returned (after clamping), Pages the number of pages (at least 1).
	Page, Pages, Size int
	// From and To are the 1-based first and last row numbers shown; both 0 for an empty listing.
	From, To int
	// Clamped is true when the requested page was past the end and the last page was returned.
	Clamped bool
}

// NormalPageSize returns size when it is one of PageSizes, else DefaultPageSize.
func NormalPageSize(size int) int {
	for _, s := range PageSizes {
		if s == size {
			return size
		}
	}
	return DefaultPageSize
}

// Paginate resolves a requested page of total rows into the offset and limit to
// read and the PageInfo to show. A page past the end clamps to the last page.
func Paginate(total, page, size int) (offset, limit int, info PageInfo) {
	size = NormalPageSize(size)
	if total < 0 {
		total = 0
	}
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	info = PageInfo{Total: total, Pages: pages, Size: size}
	if page > pages {
		page, info.Clamped = pages, true
	}
	info.Page = page
	offset = (page - 1) * size
	limit = size
	if total > 0 {
		info.From = offset + 1
		info.To = offset + size
		if info.To > total {
			info.To = total
		}
	}
	return offset, limit, info
}

// windowOf resolves the slice [from:to) of a total-row listing: by page number
// and size (the console tables) or, when limit > 0, by row offset and limit (the
// JSON API, whose cursor is a row offset). PageInfo describes what is returned.
func windowOf(total, page, size, offset, limit int) (from, to int, info PageInfo) {
	if limit <= 0 {
		off, lim, info := Paginate(total, page, size)
		to = off + lim
		if to > total {
			to = total
		}
		return off, to, info
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	to = offset + limit
	if to > total {
		to = total
	}
	pages := (total + limit - 1) / limit
	if pages < 1 {
		pages = 1
	}
	info = PageInfo{Total: total, Page: offset/limit + 1, Pages: pages, Size: limit}
	if to > offset {
		info.From, info.To = offset+1, to
	}
	return offset, to, info
}

// OffsetCursor builds the cursor of a Page that starts at the given row offset,
// for the audit listing, which pages by cursor. It is the one place outside the
// store that needs the cursor's form.
func OffsetCursor(offset int) string {
	if offset <= 0 {
		return ""
	}
	return "o" + strconv.Itoa(offset)
}

// cleanLine trims s and reports whether it is a single printable line of at
// most max runes.
func cleanLine(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > max {
		return s, false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return s, false
		}
	}
	return s, true
}

// cleanText trims s and reports whether it is printable text (newlines and
// tabs allowed) of at most max runes.
func cleanText(s string, max int) (string, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len([]rune(s)) > max {
		return s, false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return s, false
		}
	}
	return s, true
}

func fieldf(field, format string, args ...any) FieldError {
	return FieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}
