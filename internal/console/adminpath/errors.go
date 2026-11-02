package adminpath

import (
	"errors"
	"net/http"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Error codes. Each code has one HTTP status (HTTPStatus) and one fixed
// message (Message). The set is closed: a code that is not listed here is
// treated as CodeInternal on both sides of the socket.
const (
	// CodeInvalidRequest: the request failed Validate, was not JSON, was too
	// large or used the wrong method (architecture §6.3).
	CodeInvalidRequest = "invalid_request"
	// CodeNotFound: no such device, snapshot or route.
	CodeNotFound = "not_found"
	// CodeCustodyCustomerHeld: the device's repository key is held by the
	// customer, so browse, restore, escrow and maintenance are refused
	// (architecture DH22).
	CodeCustodyCustomerHeld = "custody_customer_held"
	// CodeRepoMissing: the device repository does not exist (restic exit 10).
	CodeRepoMissing = "repo_missing"
	// CodeWrongPassword: the key did not unlock the repository (restic exit 12).
	CodeWrongPassword = "wrong_password"
	// CodeLocked: the repository is locked (restic exit 11) and the operation
	// is one that reports that as an error instead of a deferral.
	CodeLocked = "locked"
	// CodeStorageUnreachable: the storage target could not be reached.
	CodeStorageUnreachable = "storage_unreachable"
	// CodeResticFailed: restic ran and failed for a reason that has no code of
	// its own; Error.Class says which class.
	CodeResticFailed = "restic_failed"
	// CodeTimeout: the operation exceeded its deadline.
	CodeTimeout = "timeout"
	// CodeUnavailable: the maintenance worker is shutting down or not ready.
	CodeUnavailable = "unavailable"
	// CodeBusy: every dump stream slot of the worker is in use. Dump streams
	// have their own budget below the worker's total, so maintenance always
	// keeps slots; a stream over the budget is refused at once, never queued,
	// and the caller may try again later.
	CodeBusy = "busy"
	// CodeInternal: a bug or an unclassified failure inside the worker.
	CodeInternal = "internal"
)

type codeInfo struct {
	code    string
	status  int
	message string
}

// codeTable is the single source of truth for codes, statuses and messages.
// The messages are fixed sentences: they never contain restic text, a path, an
// identifier or any value from the request.
var codeTable = []codeInfo{
	{CodeInvalidRequest, http.StatusBadRequest, "The request was rejected as invalid."},
	{CodeNotFound, http.StatusNotFound, "The requested device, snapshot or route was not found."},
	{CodeCustodyCustomerHeld, http.StatusConflict, "The repository key is held by the customer; this operation is not available."},
	{CodeRepoMissing, http.StatusNotFound, "The device repository does not exist."},
	{CodeWrongPassword, http.StatusUnauthorized, "The repository key did not unlock the repository."},
	{CodeLocked, http.StatusLocked, "The repository is locked by another operation."},
	{CodeStorageUnreachable, http.StatusServiceUnavailable, "The storage target could not be reached."},
	{CodeResticFailed, http.StatusBadGateway, "The repository operation failed."},
	{CodeTimeout, http.StatusGatewayTimeout, "The operation timed out."},
	{CodeUnavailable, http.StatusServiceUnavailable, "The maintenance worker is unavailable."},
	{CodeBusy, http.StatusServiceUnavailable, "Every restore stream is in use; try again shortly."},
	{CodeInternal, http.StatusInternalServerError, "An internal error occurred."},
}

func lookupCode(code string) (codeInfo, bool) {
	for _, ci := range codeTable {
		if ci.code == code {
			return ci, true
		}
	}
	return codeInfo{}, false
}

// Codes lists every error code in documentation order.
func Codes() []string {
	out := make([]string, len(codeTable))
	for i, ci := range codeTable {
		out[i] = ci.code
	}
	return out
}

// HTTPStatus returns the HTTP status that carries code on the wire, and the
// status the console should answer with when it passes the error through
// (architecture §6.3): invalid_request 400, not_found 404,
// custody_customer_held 409, repo_missing 404, wrong_password 401,
// locked 423, storage_unreachable 503, restic_failed 502, timeout 504,
// unavailable 503, busy 503, internal 500. An unknown code is 500.
func HTTPStatus(code string) int {
	if ci, ok := lookupCode(code); ok {
		return ci.status
	}
	return http.StatusInternalServerError
}

// Message returns the fixed sentence for code. An unknown code gets the
// message of CodeInternal.
func Message(code string) string {
	if ci, ok := lookupCode(code); ok {
		return ci.message
	}
	return codeTable[len(codeTable)-1].message
}

// Error is the only error that crosses the admin path. It is path-free by
// construction: Message is the fixed sentence for Code (never restic output,
// never a path, never a value from the request) and Class is restic's
// path-free error class. Both the handler and the client rewrite Message from
// Code, so even a misbehaving backend cannot put text on the wire.
//
// The wire form is {"error": {"code": ..., "class": ..., "message": ...}}.
type Error struct {
	Code    string            `json:"code"`
	Class   restic.ErrorClass `json:"class,omitempty"`
	Message string            `json:"message"`
}

// NewError returns the Error for code with its fixed message and the given
// restic class (use "" when there is no class). An unknown code becomes
// CodeInternal.
func NewError(code string, class restic.ErrorClass) *Error {
	if _, ok := lookupCode(code); !ok {
		code = CodeInternal
	}
	return &Error{Code: code, Class: class, Message: Message(code)}
}

// Error implements error. The text is "adminpath: <code>: <fixed message>".
func (e *Error) Error() string {
	return "adminpath: " + e.Code + ": " + e.Message
}

// Is reports whether target is an *Error with the same Code, so callers can
// write errors.Is(err, &adminpath.Error{Code: adminpath.CodeLocked}).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t != nil && t.Code == e.Code
}

// normalize returns a copy whose Code is in the closed set, whose Message is the
// fixed sentence for that code and whose Class is empty or one of the schema
// classes (an unrecognised class becomes restic.ClassUnknown).
func (e *Error) normalize() *Error {
	n := NewError(e.Code, e.Class)
	if n.Class != "" && !n.Class.Valid() {
		n.Class = restic.ClassUnknown
	}
	return n
}

// ErrUnavailable is returned (wrapped) by Client when the maintenance worker
// could not be reached: the socket is missing, refused the connection or
// dropped it. The returned error's text never contains the socket path; test
// with errors.Is(err, ErrUnavailable).
var ErrUnavailable = errors.New("adminpath: maintenance worker unavailable")

// unavailableError is what Client returns for a transport failure. Its text is
// ErrUnavailable's; cause is the inner syscall error with the dial address
// stripped, so neither Error nor Unwrap exposes the socket path.
type unavailableError struct{ cause error }

func (e *unavailableError) Error() string { return ErrUnavailable.Error() }

func (e *unavailableError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrUnavailable}
	}
	return []error{ErrUnavailable, e.cause}
}

// ValidationError is returned by Validate. Field names the request field that
// was rejected (one of this package's fixed field names, never a value), so the
// text is safe to log.
type ValidationError struct{ Field string }

// Error implements error.
func (e *ValidationError) Error() string {
	return "adminpath: invalid request: " + e.Field
}

func invalidField(field string) error { return &ValidationError{Field: field} }
