package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"modernc.org/sqlite"

	"github.com/c2tech-sys/cryobackup/internal/console/migrations"
)

// The error taxonomy. Callers branch with errors.Is; the typed ConstraintError
// carries the detail for the two constraint classes.
var (
	// ErrNotFound: no such row, or a row outside the caller's Scope. The two are
	// deliberately indistinguishable (architecture §3.5 item 2: 404, not 403).
	ErrNotFound = errors.New("store: not found")
	// ErrConflict: a UNIQUE / PRIMARY KEY collision, an exhausted or revoked
	// enrollment token, or an idempotency collision with different content. The
	// error is a *ConstraintError when a database constraint raised it.
	ErrConflict = errors.New("store: conflict")
	// ErrInvalid: a CHECK, NOT NULL, FOREIGN KEY or trigger violation, or an
	// argument the store rejects before the database sees it. A
	// *ConstraintError carries the SQLite message.
	ErrInvalid = errors.New("store: invalid")
	// ErrScope: a scoped method was called with the zero Scope (fail closed), or a global
	// operation (creating a customer, editing the default policy) without scope.All.
	ErrScope = errors.New("store: invalid or missing scope")
	// ErrMigrationChanged: an applied migration was edited; the store refuses to open.
	ErrMigrationChanged = migrations.ErrMigrationChanged
	// ErrMigrationAhead: the database was written by a newer binary; the store refuses to open.
	ErrMigrationAhead = migrations.ErrMigrationAhead
	// ErrCorrupt: PRAGMA quick_check failed on open, or SQLite reported corruption.
	ErrCorrupt = errors.New("store: database is corrupt")
	// ErrBusy: SQLite stayed busy longer than busy_timeout (another process holds the write lock).
	ErrBusy = errors.New("store: database is busy")
	// ErrNestedWrite: Write was called from inside a Write on the same goroutine;
	// there is only one write connection, so it would deadlock.
	ErrNestedWrite = errors.New("store: nested Write")
	// ErrClosed: the Store was closed.
	ErrClosed = errors.New("store: closed")
	// ErrCloseAbandoned: Close gave up after CloseWait on work still in flight. The
	// caller should exit; the WAL recovers on the next open.
	ErrCloseAbandoned = errors.New("store: close abandoned: work still in flight")
)

// ConstraintError is a database constraint violation mapped onto ErrConflict
// (unique / primary key) or ErrInvalid (check, not null, foreign key, trigger).
type ConstraintError struct {
	// Kind is ErrConflict or ErrInvalid; errors.Is(err, Kind) is true.
	Kind error
	// Constraint names what was violated: for a unique collision the schema's
	// index or "table.column" list SQLite reported; for a CHECK the expression
	// SQLite printed; for a trigger "trigger".
	Constraint string
	// Message is SQLite's own message, path-free by construction (it names
	// tables, columns and CHECK expressions, never row values).
	Message string
}

// Error implements error.
func (e *ConstraintError) Error() string {
	return fmt.Sprintf("%v: %s (%s)", e.Kind, e.Constraint, e.Message)
}

// Unwrap lets errors.Is match ErrConflict / ErrInvalid.
func (e *ConstraintError) Unwrap() error { return e.Kind }

// SQLite result codes, primary and extended, written out so that the driver's
// generated lib package is not imported just for constants.
const (
	rcBusy        = 5
	rcLocked      = 6
	rcCorrupt     = 11
	rcNotADB      = 26
	rcConstraint  = 19
	rcCheck       = 275  // SQLITE_CONSTRAINT_CHECK
	rcFK          = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	rcNotNull     = 1299 // SQLITE_CONSTRAINT_NOTNULL
	rcPK          = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	rcTrigger     = 1811 // SQLITE_CONSTRAINT_TRIGGER
	rcUnique      = 2067 // SQLITE_CONSTRAINT_UNIQUE
	rcRowID       = 2579 // SQLITE_CONSTRAINT_ROWID
	rcBusyRecover = 261
	rcBusySnap    = 517
)

var constraintRE = regexp.MustCompile(`(?:UNIQUE|CHECK|NOT NULL|FOREIGN KEY|PRIMARY KEY) constraint failed: ?(.*?)(?: \(\d+\))?$`)

// uniqueNames maps the column list SQLite reports for a failed unique index to
// the schema's index name, so a caller (and a test) can name the rule that
// fired. Only the partial and composite uniques whose message does not already
// carry the index name are listed.
var uniqueNames = map[string]string{
	"storage_targets.is_default": "uq_storage_targets_default",
	"storage_targets.endpoint, storage_targets.bucket, storage_targets.prefix": "uq_storage_targets_location",
	"policies.scope":                                "uq_policies_one_default",
	"policies.name":                                 "uq_policies_shared_name",
	"policies.customer_id, policies.name":           "uq_policies_customer_name",
	"device_tokens.device_id":                       "uq_device_tokens_current",
	"gateway_logins.device_id":                      "uq_gateway_logins_active_device",
	"gateway_logins.username":                       "uq_gateway_logins_active_username",
	"repo_key_escrow.device_id":                     "uq_repo_key_escrow_active",
	"device_policy_overrides.device_id":             "uq_device_policy_overrides_active",
	"alarm_acks.device_id, alarm_acks.alarm_type":   "uq_alarm_acks_active_device",
	"alarm_acks.customer_id, alarm_acks.alarm_type": "uq_alarm_acks_active_customer",
	"api_tokens.admin_id, api_tokens.name":          "uq_api_tokens_active_name",
}

// mapErr is the one place a driver error becomes a store error. Errors that are
// already store errors pass through unchanged.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrNotFound, ErrConflict, ErrInvalid, ErrScope, ErrMigrationChanged,
		ErrMigrationAhead, ErrCorrupt, ErrBusy, ErrNestedWrite, ErrClosed,
		context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return err
		}
	}
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return err
	}
	code := se.Code()
	msg := se.Error()
	switch code {
	case rcUnique, rcPK, rcRowID:
		name := ""
		if m := constraintRE.FindStringSubmatch(msg); m != nil {
			name = strings.TrimSpace(m[1])
		}
		name = strings.TrimPrefix(name, "index '")
		name = strings.TrimSuffix(name, "'")
		if friendly, ok := uniqueNames[name]; ok {
			name = friendly
		}
		return &ConstraintError{Kind: ErrConflict, Constraint: name, Message: msg}
	case rcCheck, rcNotNull, rcFK:
		name := ""
		if m := constraintRE.FindStringSubmatch(msg); m != nil {
			name = strings.TrimSpace(m[1])
		}
		if code == rcFK {
			name = "foreign key"
		}
		return &ConstraintError{Kind: ErrInvalid, Constraint: name, Message: msg}
	case rcTrigger:
		// A RESTRICT foreign key is enforced through the trigger machinery and arrives with this code too.
		if strings.Contains(msg, "FOREIGN KEY constraint failed") {
			return &ConstraintError{Kind: ErrInvalid, Constraint: "foreign key", Message: msg}
		}
		return &ConstraintError{Kind: ErrInvalid, Constraint: "trigger", Message: msg}
	case rcConstraint:
		return &ConstraintError{Kind: ErrInvalid, Constraint: "constraint", Message: msg}
	case rcBusy, rcLocked, rcBusyRecover, rcBusySnap:
		return fmt.Errorf("%w: %v", ErrBusy, err)
	case rcCorrupt, rcNotADB:
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	// Extended codes share the low byte with their primary code.
	switch code & 0xff {
	case rcConstraint:
		return &ConstraintError{Kind: ErrInvalid, Constraint: "constraint", Message: msg}
	case rcBusy, rcLocked:
		return fmt.Errorf("%w: %v", ErrBusy, err)
	case rcCorrupt, rcNotADB:
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return err
}

// invalidf builds an ErrInvalid for an argument rejected before it reaches the database.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// conflictf builds an ErrConflict that is not a database constraint (an exhausted token, a mismatched idempotent replay).
func conflictf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}
