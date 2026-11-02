package store

import (
	"encoding/json"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// The enrollment token's policy (M2-W3B). A token can name a policy (a template,
// or one of the customer's own). Devices have no policy pointer (DH6 composes
// default, customer policy, device override), so the token's policy reaches the
// enrolled device as its device-level layer: the token policy's sparse sections
// become the device's override, composed over default + the customer's policy.
// A field the token's policy does not set follows the customer's policy.
//
// The override is a snapshot taken at enrollment: later edits to the template do
// not move devices that already enrolled with it. It is an ordinary override (the
// device's Policy tab shows it, with no admin name, and an admin can replace or
// clear it).
//
// Enrollment is never refused over the TOKEN's policy. If the composed policy
// would not validate, or the policy has vanished, the device enrolls under its
// customer's policy; the device.enroll audit row says token_policy "skipped",
// EnrollResult carries the reason class and the agent API logs one warning. An
// invalid CUSTOMER policy is different: it is refused before anything is written
// (ErrInvalid), exactly as before.

const (
	tokenPolicyNone    = ""        // the token names no policy
	tokenPolicyApplied = "applied" // an override was written
	tokenPolicySame    = "same"    // the token's policy adds nothing to the customer's
	tokenPolicySkipped = "skipped" // the policy could not be applied; the device follows its customer's
)

// Reason classes of a skipped token policy (path-free, safe to log).
const (
	skipUnusable    = "policy_unusable"   // missing, the default policy, or another customer's
	skipUnreadable  = "policy_unreadable" // its sections could not be read or encoded
	skipNotComposes = "does_not_compose"  // default + customer + token policy fails validation
)

// tokenPolicyPatch resolves the override patch for a token's policy. patch is
// "" when nothing is to be written; status is one of the tokenPolicy* values;
// why is the reason class when status is "skipped".
func tokenPolicyPatch(tx *Tx, tok EnrollmentToken, customerPolicyID string) (patch, polName, status, why string) {
	if tok.PolicyID == "" {
		return "", "", tokenPolicyNone, ""
	}
	rec, err := one(tx, `SELECT `+policyCols+` FROM policies WHERE id = ?`, []any{tok.PolicyID}, scanPolicy)
	if err != nil || rec.Scope == "default" || (rec.Scope == "customer" && rec.CustomerID != tok.CustomerID) {
		return "", "", tokenPolicySkipped, skipUnusable
	}
	if tok.PolicyID == customerPolicyID {
		return "", rec.Name, tokenPolicySame, ""
	}
	top := map[string]json.RawMessage{}
	for name, raw := range rec.Sections {
		if !isEmptyObject(raw) {
			top[name] = raw
		}
	}
	if len(top) == 0 {
		return "", rec.Name, tokenPolicySame, ""
	}
	b, err := json.Marshal(top)
	if err != nil {
		return "", rec.Name, tokenPolicySkipped, skipUnreadable
	}
	def, _, err := loadDefaultSections(tx)
	if err != nil {
		return "", rec.Name, tokenPolicySkipped, skipUnreadable
	}
	var sparse map[string]json.RawMessage
	if customerPolicyID != "" {
		if sparse, err = sectionsOf(tx, customerPolicyID); err != nil {
			return "", rec.Name, tokenPolicySkipped, skipUnreadable
		}
	}
	if _, err := composeLayers(def, sparse, string(b)); err != nil {
		return "", rec.Name, tokenPolicySkipped, skipNotComposes
	}
	return string(b), rec.Name, tokenPolicyApplied, ""
}

// insertTokenOverride writes the token's policy as the new device's override.
func insertTokenOverride(tx *Tx, deviceID, patch, policyName string, now int64) error {
	reason, ok := cleanLine("Enrollment token policy: "+policyName, 200)
	if !ok || reason == "" {
		reason = "Enrollment token policy"
	}
	_, err := tx.Exec(`INSERT INTO device_policy_overrides (id, device_id, patch, reason, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		tx.id(), deviceID, patch, reason, now, now)
	return err
}

// graceWithPatch is the first-backup grace of the policy the device will run
// under: default + customer policy + the token's override patch (if any). The
// composed policy is validated here, before the enrollment writes anything: a
// customer policy that does not validate is ErrInvalid (as it was before the
// token policy existed), so no device row is created and no token use is spent.
func graceWithPatch(c *composer, customerPolicyID, patch string) (int, error) {
	l, err := c.layer(customerPolicyID)
	if err != nil {
		return 0, err
	}
	doc := l.doc
	if patch != "" {
		if doc, err = applyOverride(doc, json.RawMessage(patch)); err != nil {
			return 0, invalidf("effective policy: %v", err)
		}
	}
	p, err := policy.DecodeDocument(doc, false)
	if err != nil {
		return 0, invalidf("effective policy: %v", err)
	}
	if err := p.Validate(); err != nil {
		return 0, invalidf("effective policy: %v", err)
	}
	return p.SLA.FirstBackupGraceHours, nil
}
