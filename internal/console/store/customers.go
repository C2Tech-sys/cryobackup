package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Customer is the grouping, policy, cost-attribution and expectation unit.
type Customer struct {
	ID              string
	Name            string
	Slug            string // lower-case letters, digits and '-'; derived from Name when empty on create
	State           string // active | suspended | archived
	Expected        bool   // an expected customer with no devices is MISSING (architecture §4.5)
	OnboardingUntil *time.Time
	Contacts        json.RawMessage // JSON array; nil is stored as []
	PolicyID        string          // "" = the default policy applies
	StorageTargetID string
	DisplayTZ       string // IANA zone; "" = the global display_timezone setting
	DeviceCap       *int
	KeyCustody      string // escrowed | customer_held
	Notes           string
	ArchivedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const customerCols = `id, name, slug, state, expected, onboarding_until, contacts, policy_id, storage_target_id, display_tz,
	device_cap, key_custody, notes, archived_at, created_at, updated_at`

func scanCustomer(sc scanner) (Customer, error) {
	var c Customer
	var expected int
	var onboarding, devCap, archived sql.NullInt64
	var contacts string
	var pol, tz, notes sql.NullString
	var cr, up int64
	err := sc.Scan(&c.ID, &c.Name, &c.Slug, &c.State, &expected, &onboarding, &contacts, &pol, &c.StorageTargetID, &tz,
		&devCap, &c.KeyCustody, &notes, &archived, &cr, &up)
	if err != nil {
		return Customer{}, err
	}
	c.Expected, c.OnboardingUntil, c.Contacts = expected != 0, ptrTime(onboarding), json.RawMessage(contacts)
	c.PolicyID, c.DisplayTZ, c.DeviceCap, c.Notes = pol.String, tz.String, ptrInt(devCap), notes.String
	c.ArchivedAt, c.CreatedAt, c.UpdatedAt = ptrTime(archived), timeOf(cr), timeOf(up)
	return c, nil
}

// sameIntPtr reports whether two optional ints are both unset or equal.
func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Slugify turns a customer name into a slug the schema accepts.
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// errPolicyUnavailable is the one answer for a policy id that is unknown and for
// one that exists but is not this customer's to use.
var errPolicyUnavailable = invalidf("policy is not available to this customer")

// checkCustomerPolicy enforces that customers.policy_id names a template or a
// policy of this same customer (never another customer's, never the default,
// which is expressed by NULL). An unknown id and an id that exists but is not
// this customer's to use answer identically (errPolicyUnavailable), so the check
// is not an existence oracle.
func checkCustomerPolicy(tx *Tx, customerID, policyID string) error {
	if policyID == "" {
		return nil
	}
	var scope string
	var owner sql.NullString
	err := tx.QueryRow(`SELECT scope, customer_id FROM policies WHERE id = ?`, policyID).Scan(&scope, &owner)
	if err == sql.ErrNoRows {
		return errPolicyUnavailable
	}
	if err != nil {
		return err
	}
	switch scope {
	case "template":
		return nil
	case "customer":
		if owner.String == customerID {
			return nil
		}
	}
	return errPolicyUnavailable
}

// CreateCustomer inserts a customer. Creating one is a global act, so it needs
// scope.All (ErrScope otherwise: a scoped operator cannot mint customers).
func (s *Store) CreateCustomer(ctx context.Context, scope Scope, c Customer) (Customer, error) {
	if err := scope.check(); err != nil {
		return Customer{}, err
	}
	if !scope.All {
		return Customer{}, ErrScope
	}
	if c.Slug == "" {
		c.Slug = Slugify(c.Name)
	}
	if c.State == "" {
		c.State = "active"
	}
	if c.KeyCustody == "" {
		c.KeyCustody = "escrowed"
	}
	contacts, err := jsonOr(c.Contacts, "[]")
	if err != nil {
		return Customer{}, err
	}
	var out Customer
	err = s.Write(ctx, func(tx *Tx) error {
		c.ID = tx.id()
		now := tx.nowMS()
		if c.State == "archived" && c.ArchivedAt == nil {
			t := timeOf(now)
			c.ArchivedAt = &t
		}
		if c.StorageTargetID == "" {
			err := tx.QueryRow(`SELECT id FROM storage_targets WHERE is_default = 1`).Scan(&c.StorageTargetID)
			if err == sql.ErrNoRows {
				return invalidf("no storage target given and none is the default")
			}
			if err != nil {
				return err
			}
		}
		if err := checkCustomerPolicy(tx, c.ID, c.PolicyID); err != nil {
			// A brand-new customer cannot own a customer policy yet; only templates pass here.
			return err
		}
		if _, err := tx.Exec(`INSERT INTO customers (`+customerCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			c.ID, c.Name, c.Slug, c.State, boolInt(c.Expected), nullTime(c.OnboardingUntil), contacts, nullStr(c.PolicyID),
			c.StorageTargetID, nullStr(c.DisplayTZ), nullInt(c.DeviceCap), c.KeyCustody, nullStr(c.Notes),
			nullTime(c.ArchivedAt), now, now); err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{c.ID}, scanCustomer)
		return err
	})
	return out, err
}

// GetCustomer returns a customer inside scope (ErrNotFound otherwise).
func (s *Store) GetCustomer(ctx context.Context, scope Scope, id string) (Customer, error) {
	if err := scope.check(); err != nil {
		return Customer{}, err
	}
	if !scope.Allows(id) {
		return Customer{}, ErrNotFound
	}
	var out Customer
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{id}, scanCustomer)
		return err
	})
	return out, err
}

// ListCustomers returns the customers inside scope ordered by name, archived
// ones included (callers filter; the facts loader needs them).
func (s *Store) ListCustomers(ctx context.Context, scope Scope) ([]Customer, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause("id")
	var out []Customer
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, `SELECT `+customerCols+` FROM customers WHERE `+clause+` ORDER BY name COLLATE NOCASE, id`, args, scanCustomer)
		return err
	})
	return out, err
}

// UpdateCustomer replaces a customer's mutable fields (name, slug, state,
// expected, onboarding_until, contacts, policy_id, storage_target_id,
// display_tz, device_cap, key_custody, notes) with those of c, read-modify-write
// style: fetch with GetCustomer, change, pass back. archived_at follows State.
// The schema's rules (an expected customer cannot be archived) surface as
// ErrInvalid. Changing storage_target_id (the empty string keeps the current
// one), key_custody or device_cap needs scope.All: a scoped caller that passes a
// value differing from the stored one gets ErrScope. A changed storage target
// must exist (ErrInvalid otherwise).
func (s *Store) UpdateCustomer(ctx context.Context, scope Scope, id string, c Customer) (Customer, error) {
	if err := scope.check(); err != nil {
		return Customer{}, err
	}
	if !scope.Allows(id) {
		return Customer{}, ErrNotFound
	}
	contacts, err := jsonOr(c.Contacts, "[]")
	if err != nil {
		return Customer{}, err
	}
	var out Customer
	err = s.Write(ctx, func(tx *Tx) error {
		cur, err := one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{id}, scanCustomer)
		if err != nil {
			return err
		}
		if err := checkCustomerPolicy(tx, id, c.PolicyID); err != nil {
			return err
		}
		if c.StorageTargetID == "" {
			c.StorageTargetID = cur.StorageTargetID
		}
		// Where the data lands, who holds the key and how many devices may enroll
		// are owner-level fields: a scoped caller may pass them back unchanged
		// (read-modify-write) but not change them.
		if !scope.All && (c.StorageTargetID != cur.StorageTargetID || c.KeyCustody != cur.KeyCustody || !sameIntPtr(c.DeviceCap, cur.DeviceCap)) {
			return ErrScope
		}
		if c.StorageTargetID != cur.StorageTargetID {
			var exists int
			if err := tx.QueryRow(`SELECT 1 FROM storage_targets WHERE id = ?`, c.StorageTargetID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
				return invalidf("storage target %s does not exist", c.StorageTargetID)
			} else if err != nil {
				return err
			}
		}
		now := tx.nowMS()
		var archived any
		if c.State == "archived" {
			if cur.ArchivedAt != nil {
				archived = cur.ArchivedAt.UnixMilli()
			} else {
				archived = now
			}
		}
		_, err = tx.Exec(`UPDATE customers SET name = ?, slug = ?, state = ?, expected = ?, onboarding_until = ?, contacts = ?,
			policy_id = ?, storage_target_id = ?, display_tz = ?, device_cap = ?, key_custody = ?, notes = ?,
			archived_at = ?, updated_at = ? WHERE id = ?`,
			c.Name, c.Slug, c.State, boolInt(c.Expected), nullTime(c.OnboardingUntil), contacts, nullStr(c.PolicyID),
			c.StorageTargetID, nullStr(c.DisplayTZ), nullInt(c.DeviceCap), c.KeyCustody, nullStr(c.Notes), archived, now, id)
		if err != nil {
			return err
		}
		out, err = one(tx, `SELECT `+customerCols+` FROM customers WHERE id = ?`, []any{id}, scanCustomer)
		return err
	})
	return out, err
}
