package ui

import (
	"net/http"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
)

// The M2-W2A routes (architecture §5.3: S24, S25, S17 and the device Policy
// tab). Registered from one place so ui.go keeps a single additive line.
const (
	patPolicies         = "GET /policies"
	patPolicyNew        = "GET /policies/new"
	patPolicyCreate     = "POST /ui/policies"
	patPolicyEdit       = "GET /policies/{id}"
	patPolicyImpact     = "POST /ui/policies/{id}/impact"
	patPolicyEditAgain  = "POST /ui/policies/{id}/edit"
	patPolicySave       = "POST /ui/policies/{id}"
	patPolicyDeletePage = "GET /policies/{id}/delete"
	patPolicyDelete     = "POST /ui/policies/{id}/delete"
	patPolicyWindow     = "POST /ui/policy/window/validate"
	patCustPolicy       = "GET /customers/{id}/policy"
	patCustPolicyImpact = "POST /ui/customers/{id}/policy/impact"
	patCustPolicyAssign = "POST /ui/customers/{id}/policy"
	patDevPolicy        = "GET /devices/{id}/policy"
	patDevOverride      = "POST /ui/devices/{id}/override"
	patDevOverrideClear = "POST /ui/devices/{id}/override/clear"
)

// Body caps of the policy writes, below the console's 1 MiB default.
const (
	policyFormCap  = 256 << 10 // a whole policy form
	smallFormCap   = 8 << 10   // a token and a policy id
	windowCheckCap = 16 << 10  // one window or throttle text
)

// policyUIPatterns lists what registerPolicies adds.
func policyUIPatterns() []string {
	return []string{patPolicies, patPolicyNew, patPolicyCreate, patPolicyEdit, patPolicyImpact, patPolicyEditAgain, patPolicySave, patPolicyDeletePage,
		patPolicyDelete, patPolicyWindow, patCustPolicy, patCustPolicyImpact, patCustPolicyAssign, patDevPolicy, patDevOverride, patDevOverrideClear}
}

// registerPolicies adds the routes. Reading the list and a device's effective
// policy needs read_only; the editor, every save, the customer's Policy tab and
// the override writes need admin. Writes are CSRF-checked by Require.
func (s *Service) registerPolicies(r httpx.Registrar) {
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeUI, h) }
	ad := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleAdmin, auth.ModeUI, h) }
	// adCap is ad with a body cap smaller than the console's default, applied before
	// the gate parses anything (a bigger post is 413, not a 403 after a 10 MB parse).
	adCap := func(max int64, h http.HandlerFunc) http.Handler { return auth.LimitBody(max, ad(h)) }
	r.Handle(patPolicies, ro(s.policies))
	r.Handle(patPolicyNew, ad(s.policyNew))
	r.Handle(patPolicyCreate, adCap(policyFormCap, s.policyCreate))
	r.Handle(patPolicyEdit, ad(s.policyEdit))
	r.Handle(patPolicyImpact, adCap(policyFormCap, s.policyImpact))
	r.Handle(patPolicyEditAgain, adCap(policyFormCap, s.policyEditAgain))
	r.Handle(patPolicySave, adCap(policyFormCap, s.policySave))
	r.Handle(patPolicyDeletePage, ad(s.policyDeletePage))
	r.Handle(patPolicyDelete, adCap(smallFormCap, s.policyDelete))
	r.Handle(patPolicyWindow, adCap(windowCheckCap, s.policyWindowValidate))
	r.Handle(patCustPolicy, ad(s.customerPolicy))
	r.Handle(patCustPolicyImpact, adCap(smallFormCap, s.customerPolicyImpact))
	r.Handle(patCustPolicyAssign, adCap(smallFormCap, s.customerPolicyAssign))
	r.Handle(patDevPolicy, ro(s.devicePolicy))
	r.Handle(patDevOverride, adCap(policyFormCap, s.deviceOverrideSet))
	r.Handle(patDevOverrideClear, adCap(smallFormCap, s.deviceOverrideClear))
}
