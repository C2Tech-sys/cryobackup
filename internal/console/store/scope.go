package store

import (
	"encoding/json"
	"strings"
)

// Scope says which customers a caller may see and change (architecture §3.5).
// It comes from admin_users.customer_scope (NULL = all customers) or from an
// API token's owner. Every store method that reads or writes customer-owned
// data takes one as its first argument after ctx; there is no overload without
// it.
//
// The zero Scope is invalid and every scoped method returns ErrScope for it:
// forgetting to build a Scope fails closed instead of meaning "everything".
// An empty, non-nil CustomerIDs is valid and means "no customers at all".
type Scope struct {
	// All grants every customer. Roles owner and admin always have it; an
	// operator or read_only account has it when customer_scope is NULL.
	All bool
	// CustomerIDs is the allowed set when All is false.
	CustomerIDs []string
}

// ScopeAll grants every customer.
func ScopeAll() Scope { return Scope{All: true} }

// ScopeOf grants exactly the listed customers. ScopeOf() with no ids is a valid
// scope that sees nothing.
func ScopeOf(ids ...string) Scope {
	out := make([]string, len(ids))
	copy(out, ids)
	return Scope{CustomerIDs: out}
}

// Valid reports whether s is usable: All, or a non-nil (possibly empty) id list.
func (s Scope) Valid() bool { return s.All || s.CustomerIDs != nil }

// Allows reports whether the scope grants customerID. An invalid scope allows nothing.
func (s Scope) Allows(customerID string) bool {
	if s.All {
		return true
	}
	for _, id := range s.CustomerIDs {
		if id == customerID {
			return true
		}
	}
	return false
}

// check returns ErrScope for the zero Scope.
func (s Scope) check() error {
	if !s.Valid() {
		return ErrScope
	}
	return nil
}

// clause returns a SQL fragment restricting column to the scope plus its
// arguments, to be ANDed into a WHERE clause. For All it is "1 = 1"; for an
// empty id list "0 = 1".
func (s Scope) clause(column string) (string, []any) {
	if s.All {
		return "1 = 1", nil
	}
	if len(s.CustomerIDs) == 0 {
		return "0 = 1", nil
	}
	args := make([]any, len(s.CustomerIDs))
	for i, id := range s.CustomerIDs {
		args[i] = id
	}
	return column + " IN (?" + strings.Repeat(",?", len(args)-1) + ")", args
}

// ScopeFromJSON builds a Scope from admin_users.customer_scope: SQL NULL (the
// empty string here) is all customers, otherwise a JSON array of customer ids.
func ScopeFromJSON(customerScope string) (Scope, error) {
	if customerScope == "" {
		return ScopeAll(), nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(customerScope), &ids); err != nil {
		return Scope{}, invalidf("customer_scope is not a JSON array of ids: %v", err)
	}
	return ScopeOf(ids...), nil
}
