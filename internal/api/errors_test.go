package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPStatusTable(t *testing.T) {
	want := map[string]int{
		"invalid_request":        400,
		"unauthorized":           401,
		"forbidden":              403,
		"not_found":              404,
		"conflict":               409,
		"custody_customer_held":  409,
		"rate_limited":           429,
		"payload_too_large":      413,
		"not_modified":           304,
		"master_key_unavailable": 503,
		"unavailable":            503,
		"internal":               500,
	}
	codes := ErrorCodes()
	if len(codes) != len(want) {
		t.Errorf("ErrorCodes() has %d codes, want %d", len(codes), len(want))
	}
	for code, status := range want {
		if got := HTTPStatus(code); got != status {
			t.Errorf("HTTPStatus(%q) = %d, want %d", code, got, status)
		}
	}
	for _, code := range codes {
		if _, ok := want[code]; !ok {
			t.Errorf("code %q is not in the ticket's table", code)
		}
	}
	if HTTPStatus("from_the_future") != http.StatusInternalServerError || HTTPStatus("") != 500 {
		t.Error("an unknown code must map to 500")
	}
	if CodeNotModified != "not_modified" || HTTPStatus(CodeNotModified) != http.StatusNotModified {
		t.Error("not_modified must be 304")
	}
}

func TestMessagesAreFixedAndPathFree(t *testing.T) {
	seen := map[string]string{}
	for _, code := range ErrorCodes() {
		m := MessageFor(code)
		if m == "" || m == unknownCodeMessage {
			t.Errorf("%s has no message", code)
		}
		if other, dup := seen[m]; dup {
			t.Errorf("%s and %s share a message", code, other)
		}
		seen[m] = code
		if !strings.HasSuffix(m, ".") || strings.ContainsAny(m, `\/%`) || looksLikePath(m) {
			t.Errorf("message of %s is not a plain sentence: %q", code, m)
		}
		if e := NewError(code); e.Code != code || e.Title != m || e.Status != HTTPStatus(code) || e.Type != "urn:cryobackup:error:"+code {
			t.Errorf("NewError(%q) = %+v", code, e)
		}
	}
	if MessageFor("zzz") != unknownCodeMessage {
		t.Error("an unknown code needs the generic message")
	}
}

func TestErrorTypeAndShape(t *testing.T) {
	var err error = NewError(CodeCustodyCustomerHeld)
	var e *Error
	if !errors.As(err, &e) || e.Code != "custody_customer_held" {
		t.Fatalf("errors.As: %v", err)
	}
	if got := err.Error(); got != "custody_customer_held: "+MessageFor(CodeCustodyCustomerHeld) {
		t.Errorf("Error() = %q", got)
	}
	// Error() never prints the wire message: a hostile console cannot reach the log through it.
	hostile := &Error{Code: CodeInternal, Title: `open C:\Users\patient\x.docx: access denied`}
	if strings.Contains(hostile.Error(), "patient") || strings.Contains(hostile.Error(), "Users") {
		t.Errorf("Error() repeated the wire message: %q", hostile.Error())
	}
	if got := (&Error{Code: "new_code"}).Error(); got != "unknown: "+unknownCodeMessage {
		t.Errorf("an unknown code needs the fixed form, got %q", got)
	}

	b, err := json.Marshal(okError())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"urn:cryobackup:error:rate_limited","title":"Too many requests; retry later.","status":429,"code":"rate_limited","retry_after_s":30,"request_id":"req-1"}`
	if string(b) != want {
		t.Errorf("Error JSON: got %s, want %s", b, want)
	}
	b, _ = json.Marshal(NewError(CodeNotFound))
	if string(b) != `{"type":"urn:cryobackup:error:not_found","title":"The requested resource was not found.","status":404,"code":"not_found"}` {
		t.Errorf("minimal Error: %s", b)
	}
	if ContentTypeProblem != "application/problem+json" {
		t.Errorf("ContentTypeProblem = %q", ContentTypeProblem)
	}
}

// A problem+json body written by hand (as another implementation, or a newer
// console with extra members, would send it) decodes into Error.
func TestDecodeProblemJSON(t *testing.T) {
	body := `{
	  "type": "urn:cryobackup:error:custody_customer_held",
	  "title": "free text that the agent must not trust",
	  "status": 409,
	  "code": "custody_customer_held",
	  "retry_after_s": 12,
	  "request_id": "01JAZK3Q8W5N6X7Y8Z9A0B1C2D",
	  "detail": "an unknown member from a newer console",
	  "instance": "/api/v1/agent/enroll"
	}`
	var e Error
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	want := Error{
		Type: "urn:cryobackup:error:custody_customer_held", Title: "free text that the agent must not trust",
		Status: 409, Code: "custody_customer_held", RetryAfterS: 12, RequestID: "01JAZK3Q8W5N6X7Y8Z9A0B1C2D",
	}
	if e != want {
		t.Errorf("decoded %+v, want %+v", e, want)
	}
	if strings.Contains(e.Error(), "free text") || e.Error() != "custody_customer_held: "+MessageFor(CodeCustodyCustomerHeld) {
		t.Errorf("Error() = %q", e.Error())
	}
}

// Error() must not echo an unknown wire code: the code is attacker-chosen text
// and Error() is what reaches a log.
func TestErrorDoesNotEchoUnknownCode(t *testing.T) {
	var e Error
	body := `{"code":"C:\\Users\\patient\\x","title":"open C:\\Users\\patient\\x.docx","status":500}`
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	got := e.Error()
	if strings.Contains(got, "Users") || strings.Contains(got, "patient") {
		t.Errorf("Error() echoed the wire code: %q", got)
	}
	if got != "unknown: Unknown error." {
		t.Errorf("Error() = %q, want the fixed unknown form", got)
	}
	if (&Error{}).Error() != "unknown: Unknown error." {
		t.Error("an empty code is unknown too")
	}
}
