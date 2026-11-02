package api

// Error codes of the agent API (architecture §5, §5.5, DH22). The code is the
// stable, machine-readable part of an error; the HTTP status follows from it
// (HTTPStatus) and the sentence from MessageFor.
const (
	CodeInvalidRequest       = "invalid_request"        // 400: malformed body, failed Validate, unknown enum value, failed AssertPathFree
	CodeUnauthorized         = "unauthorized"           // 401: missing, unknown, revoked or expired token
	CodeForbidden            = "forbidden"              // 403: the token is valid but not for this route or device
	CodeNotFound             = "not_found"              // 404: no such command, restore, release or lobby request
	CodeConflict             = "conflict"               // 409: state conflict, for example a second concurrent long poll (§5.5)
	CodeCustodyCustomerHeld  = "custody_customer_held"  // 409: the operation needs the escrowed key (DH22)
	CodeRateLimited          = "rate_limited"           // 429: §5.5; carries Retry-After
	CodePayloadTooLarge      = "payload_too_large"      // 413: body over the §5.5 cap
	CodeNotModified          = "not_modified"           // 304: policy ETag matched; no body
	CodeMasterKeyUnavailable = "master_key_unavailable" // 503: the console cannot unwrap an escrowed secret right now
	CodeUnavailable          = "unavailable"            // 503: temporary; carries Retry-After
	CodeInternal             = "internal"               // 500: a bug on the console; never details
)

// errorTable is the one place that maps a code to its HTTP status and its
// fixed sentence. A message is never built from a request, a path or an
// exception text (architecture §8.4).
var errorTable = []struct {
	code    string
	status  int
	message string
}{
	{CodeInvalidRequest, 400, "The request is not valid."},
	{CodeUnauthorized, 401, "Authentication is required or the credential is not valid."},
	{CodeForbidden, 403, "The credential does not permit this request."},
	{CodeNotFound, 404, "The requested resource was not found."},
	{CodeConflict, 409, "The request conflicts with the current state."},
	{CodeCustodyCustomerHeld, 409, "The repository key is held by the customer, so this operation is not available."},
	{CodeRateLimited, 429, "Too many requests; retry later."},
	{CodePayloadTooLarge, 413, "The request body is too large."},
	{CodeNotModified, 304, "Not modified."},
	{CodeMasterKeyUnavailable, 503, "The console cannot use its master key right now."},
	{CodeUnavailable, 503, "The service is temporarily unavailable."},
	{CodeInternal, 500, "Internal error."},
}

const (
	unknownCodeMessage = "Unknown error."
	// unknownCodeLabel stands in for the code in Error.Error when the code is
	// not in the table.
	unknownCodeLabel = "unknown"
)

// ErrorCodes lists every code in table order.
func ErrorCodes() []string {
	out := make([]string, len(errorTable))
	for i, e := range errorTable {
		out[i] = e.code
	}
	return out
}

// HTTPStatus returns the HTTP status of an error code. An unknown code (from a
// newer console) maps to 500, so an agent never treats it as success.
func HTTPStatus(code string) int {
	for _, e := range errorTable {
		if e.code == code {
			return e.status
		}
	}
	return 500
}

// MessageFor returns the fixed sentence of a code ("Unknown error." for a code
// that is not in the table).
func MessageFor(code string) string {
	for _, e := range errorTable {
		if e.code == code {
			return e.message
		}
	}
	return unknownCodeMessage
}

// ErrorTypePrefix precedes the code in Error.Type: "urn:cryobackup:error:" +
// code (RFC 9457 problem type).
const ErrorTypePrefix = "urn:cryobackup:error:"

// Error is the body of every non-2xx response except 304: an RFC 9457
// problem document (architecture §5), flat, sent with ContentTypeProblem.
//
//	{"type":"urn:cryobackup:error:rate_limited","title":"Too many requests; retry later.",
//	 "status":429,"code":"rate_limited","retry_after_s":30,"request_id":"01JA..."}
//
// Code is the stable, machine-readable part; Type, Title and Status follow from
// it (NewError fills them from the table). Title is a fixed sentence per code,
// never free text and never a path. RetryAfterS repeats the Retry-After header
// for 429 and 503. RequestID is the value of HeaderRequestID.
//
// The agent must not trust Title or Code from the wire: Error() renders the
// sentence from the table for a known code, and the fixed "unknown: Unknown
// error." for an unknown one, so a hostile or newer console cannot put text
// into the agent's log through it.
type Error struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Status      int    `json:"status"`
	Code        string `json:"code"`
	RetryAfterS int    `json:"retry_after_s,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
}

// NewError returns an Error for code with Type, Title and Status filled from
// the table (an unknown code gets status 500 and the generic title).
func NewError(code string) *Error {
	return &Error{
		Type:   ErrorTypePrefix + code,
		Title:  MessageFor(code),
		Status: HTTPStatus(code),
		Code:   code,
	}
}

// Error implements the error interface: "code: fixed sentence" for a code in
// the table, and the fixed form "unknown: Unknown error." for any other code.
// The wire code is never echoed unless it is one of ours: a hostile or newer
// peer can put any string in Code, and Error() is what reaches a log.
func (e *Error) Error() string {
	for _, t := range errorTable {
		if t.code == e.Code {
			return t.code + ": " + t.message
		}
	}
	return unknownCodeLabel + ": " + unknownCodeMessage
}
