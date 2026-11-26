package agent

import (
	"encoding/json"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// P22-R2b H1 (ADR 0033): a last-known-good policy saved before the escrow_recheck_days floor existed (a
// value of 1 to 7) is accepted and used as 8; it never makes the agent drop its policy. Killed mutant:
// the clamp dropped from decodePolicy (a non-nil error only if Validate refused it, so the days stay 3).
func TestDecodePolicyClampsALowStoredEscrowRecheck(t *testing.T) {
	for _, c := range []struct{ stored, want int }{{3, 8}, {1, 8}, {7, 8}, {0, 0}, {8, 8}, {30, 30}} {
		pol := testPolicy()
		pol.SLA.EscrowRecheckDays = c.stored
		raw, err := json.Marshal(api.PolicyResponse{Policy: pol, PolicyID: testPolicyID, Version: 4, ETag: `"p4"`})
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodePolicy(raw)
		if err != nil || got.Policy.SLA.EscrowRecheckDays != c.want {
			t.Errorf("stored %d: decodePolicy = %d days, %v; want %d", c.stored, got.Policy.SLA.EscrowRecheckDays, err, c.want)
		}
	}
}
