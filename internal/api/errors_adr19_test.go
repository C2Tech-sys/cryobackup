package api

import (
	"encoding/json"
	"testing"
)

// ADR 0019: mfa_step_up_required is a table code, 403, with the fixed sentence the console's
// fresh-MFA gate writes (adminapi TestAdr19StepUpProblemMatchesTheAuthGate compares the bytes).
func TestAdr19StepUpRequiredIsInTheTable(t *testing.T) {
	if CodeMFAStepUpRequired != "mfa_step_up_required" || HTTPStatus(CodeMFAStepUpRequired) != 403 {
		t.Fatalf("%q -> %d, want mfa_step_up_required -> 403", CodeMFAStepUpRequired, HTTPStatus(CodeMFAStepUpRequired))
	}
	const want = "A recent MFA check is required. Confirm with a current authenticator code."
	if MessageFor(CodeMFAStepUpRequired) != want {
		t.Fatalf("message %q, want %q", MessageFor(CodeMFAStepUpRequired), want)
	}
	b, _ := json.Marshal(NewError(CodeMFAStepUpRequired))
	if string(b) != `{"type":"urn:cryobackup:error:mfa_step_up_required","title":"`+want+`","status":403,"code":"mfa_step_up_required"}` {
		t.Fatalf("problem document: %s", b)
	}
	if got := NewError(CodeMFAStepUpRequired).Error(); got != "mfa_step_up_required: "+want {
		t.Fatalf("Error() = %q", got)
	}
}
