package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// This file holds the small conversions shared by every table's methods: the
// schema stores times as epoch milliseconds, JSON as TEXT, booleans as 0/1 and
// "absent" as NULL; the Go types use time.Time, json.RawMessage, bool and the
// empty string or nil pointer.

// scanner is the part of *sql.Row and *sql.Rows that the row-mapping functions use.
type scanner interface{ Scan(dest ...any) error }

func msOf(t time.Time) int64 { return t.UnixMilli() }

func timeOf(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// nullTime maps *time.Time to a SQL argument (NULL when nil).
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// ptrTime maps a nullable INTEGER column to *time.Time.
func ptrTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := timeOf(v.Int64)
	return &t
}

// nullStr maps "" to NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt maps a nil *int to NULL.
func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullInt64 maps a nil *int64 to NULL.
func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullBool maps a nil *bool to NULL and otherwise to 0/1.
func nullBool(p *bool) any {
	if p == nil {
		return nil
	}
	return boolInt(*p)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullBytes maps an empty slice to NULL.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func ptrInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func ptrInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func ptrBool(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

// jsonOr returns raw as TEXT for a json_valid column, or def when raw is empty.
// Invalid JSON is rejected here (ErrInvalid) so the caller sees the argument at
// fault rather than a bare CHECK failure.
func jsonOr(raw json.RawMessage, def string) (string, error) {
	if len(raw) == 0 {
		return def, nil
	}
	if !json.Valid(raw) {
		return "", invalidf("not valid JSON")
	}
	return string(raw), nil
}

// nullJSON is jsonOr for a nullable column (nil/empty becomes NULL).
func nullJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, invalidf("not valid JSON")
	}
	return string(raw), nil
}

func rawOf(s sql.NullString) json.RawMessage {
	if !s.Valid {
		return nil
	}
	return json.RawMessage(s.String)
}

// placeholders returns "?,?,?" for n arguments.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return "?" + strings.Repeat(",?", n-1)
}

// Page selects a window of a list. Cursor is opaque: pass back the previous
// Listing.NextCursor. (In this version it encodes an offset; handlers must not
// parse it, so it can become a keyset cursor without an API change.)
type Page struct {
	// Limit is the maximum number of items; 0 means 100, and it is capped at 1000.
	Limit int
	// Cursor is "" for the first page.
	Cursor string
}

// Listing is one page of a list. NextCursor is non-empty exactly when more rows
// exist, so a caller can always tell a complete list from a truncated one
// (architecture §5: lists are never silently truncated).
type Listing[T any] struct {
	Items      []T
	NextCursor string
}

const (
	defaultPageLimit = 100
	maxPageLimit     = 1000
)

// window resolves a Page into (limit, offset) and validates the cursor.
func (p Page) window() (limit, offset int, err error) {
	limit = p.Limit
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if p.Cursor != "" {
		if !strings.HasPrefix(p.Cursor, "o") {
			return 0, 0, invalidf("bad cursor")
		}
		n, perr := strconv.Atoi(p.Cursor[1:])
		if perr != nil || n < 0 {
			return 0, 0, invalidf("bad cursor")
		}
		offset = n
	}
	return limit, offset, nil
}

// finish trims the over-fetched row (queries ask for limit+1) and sets NextCursor.
func finish[T any](items []T, limit, offset int) Listing[T] {
	l := Listing[T]{Items: items}
	if len(items) > limit {
		l.Items = items[:limit]
		l.NextCursor = "o" + strconv.Itoa(offset+limit)
	}
	if l.Items == nil {
		l.Items = []T{}
	}
	return l
}

// collect runs a query and maps every row with fn.
func collect[T any](tx *Tx, query string, args []any, fn func(scanner) (T, error)) ([]T, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := fn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// one runs a query expected to return one row and maps ErrNoRows to ErrNotFound.
func one[T any](tx *Tx, query string, args []any, fn func(scanner) (T, error)) (T, error) {
	v, err := fn(tx.QueryRow(query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		var zero T
		return zero, ErrNotFound
	}
	return v, err
}

// prefixCols prefixes every column in a comma-separated list ("r." for a joined query).
func prefixCols(cols, prefix string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = prefix + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
