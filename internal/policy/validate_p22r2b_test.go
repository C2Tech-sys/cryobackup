package policy

import (
	"fmt"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P22-R2b G5: sla.escrow_recheck_days is 0 (recheck off) or at least 7, because the verifier re-proves
// every escrowed key weekly (ADR 0030): 1 to 6 would raise ESCROW_UNVERIFIED between two checks of a
// healthy key. The refusal names the field. Killed mutants: the lower bound dropped (>= 0 only), the
// bound moved to 6 or 8, 0 refused.
func TestValidateEscrowRecheckDays(t *testing.T) {
	if EscrowRecheckMinDays*24*time.Hour < state.EscrowVerifyEvery {
		t.Fatalf("EscrowRecheckMinDays = %d is shorter than the verifier's cadence %v", EscrowRecheckMinDays, state.EscrowVerifyEvery)
	}
	if EscrowRecheckMinDays != 7 {
		t.Fatalf("EscrowRecheckMinDays = %d, want 7", EscrowRecheckMinDays)
	}
	var cases []validateCase
	for d := 1; d <= 6; d++ {
		d := d
		cases = append(cases, validateCase{fmt.Sprintf("recheck %d days", d), func(p *Policy) { p.SLA.EscrowRecheckDays = d }, "sla.escrow_recheck_days", "0 (recheck off) or at least 7 days"})
	}
	cases = append(cases,
		validateCase{"recheck 0 stays legal (off)", func(p *Policy) { p.SLA.EscrowRecheckDays = 0 }, "", ""},
		validateCase{"recheck 7", func(p *Policy) { p.SLA.EscrowRecheckDays = 7 }, "", ""},
		validateCase{"recheck 30 (the default)", func(p *Policy) { p.SLA.EscrowRecheckDays = 30 }, "", ""},
		validateCase{"the default policy", func(p *Policy) {}, "", ""},
	)
	runValidateCases(t, cases)
	if d := Default(ClassUnknown).SLA.EscrowRecheckDays; d != 30 {
		t.Fatalf("default escrow_recheck_days = %d", d)
	}
}
