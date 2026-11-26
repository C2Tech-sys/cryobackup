package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P22-R2b G5 (ADR 0033): sla.escrow_recheck_days. A WRITE refuses 1 to 7 naming the field (the floor is
// 8, state.EscrowVerifyMaxAge in days: a weekly verifier needs a day of slack); 0 keeps its meaning and
// stays accepted. A READ never fails on it: Validate accepts the value and ClampStored raises it to 8.

func hasPath(err error, path string) bool {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	for _, p := range ve.Problems {
		if p.Path == path {
			return true
		}
	}
	return false
}

// Killed mutants: the floor dropped from ValidateWrite, the floor moved to 7 or 9, 0 refused.
func TestValidateWriteEscrowRecheckDays(t *testing.T) {
	if EscrowRecheckMinDays*24*time.Hour != state.EscrowVerifyMaxAge {
		t.Fatalf("EscrowRecheckMinDays = %d days, want state.EscrowVerifyMaxAge %v", EscrowRecheckMinDays, state.EscrowVerifyMaxAge)
	}
	if EscrowRecheckMinDays != 8 {
		t.Fatalf("EscrowRecheckMinDays = %d, want 8", EscrowRecheckMinDays)
	}
	for d := 1; d <= 7; d++ {
		p := Default(ClassUnknown)
		p.SLA.EscrowRecheckDays = d
		err := p.ValidateWrite()
		if !hasPath(err, "sla.escrow_recheck_days") || !strings.Contains(err.Error(), "at least 8 days") {
			t.Errorf("write of %d: %v", d, err)
		}
		if err := p.Validate(); err != nil { // the read-side check never refuses it
			t.Errorf("read of %d: %v", d, err)
		}
	}
	for _, d := range []int{0, 8, 9, 30} {
		p := Default(ClassUnknown)
		p.SLA.EscrowRecheckDays = d
		if err := p.ValidateWrite(); err != nil {
			t.Errorf("write of %d: %v", d, err)
		}
	}
	p := Default(ClassUnknown)
	p.SLA.EscrowRecheckDays = -1
	if err := p.ValidateWrite(); !hasPath(err, "sla.escrow_recheck_days") || !strings.Contains(err.Error(), ">= 0") {
		t.Errorf("-1: %v", err)
	}
	if d := Default(ClassUnknown).SLA.EscrowRecheckDays; d != 30 {
		t.Fatalf("default escrow_recheck_days = %d", d)
	}
}

// The patch a write carries (an override, a policy section) is judged on its own input.
func TestCheckEscrowRecheckWritten(t *testing.T) {
	for _, c := range []struct {
		sla  string
		fail bool
	}{
		{`{"escrow_recheck_days":3}`, true}, {`{"escrow_recheck_days":7}`, true}, {`{"escrow_recheck_days":1}`, true},
		{`{"escrow_recheck_days":8}`, false}, {`{"escrow_recheck_days":0}`, false}, {`{"stale_after_hours":9}`, false},
		{``, false}, {`null`, false}, {`{"escrow_recheck_days":"x"}`, false},
	} {
		err := CheckEscrowRecheckWritten(json.RawMessage(c.sla))
		if c.fail != (err != nil) || (c.fail && !hasPath(err, "sla.escrow_recheck_days")) {
			t.Errorf("%q: %v", c.sla, err)
		}
	}
	// Patch (a write) refuses 3 and 7 and accepts 8.
	sections, err := Encode(Default(ClassUnknown))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Decode(sections)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		days int
		ok   bool
	}{{3, false}, {7, false}, {8, true}} {
		_, err := Patch(p, json.RawMessage(fmt.Sprintf(`{"sla":{"escrow_recheck_days":%d}}`, c.days)))
		if (err == nil) != c.ok {
			t.Errorf("Patch %d: %v", c.days, err)
		}
	}
}

// A read clamps 1-7 to 8, never touches 0 or 8 and up, and Validate then accepts. Killed mutants: the
// clamp dropped, 0 clamped.
func TestClampStoredEscrowRecheckDays(t *testing.T) {
	for _, c := range []struct {
		in, want int
		clamped  bool
	}{
		{1, 8, true}, {3, 8, true}, {7, 8, true}, {0, 0, false}, {8, 8, false}, {30, 30, false},
	} {
		p := Default(ClassUnknown)
		p.SLA.EscrowRecheckDays = c.in
		_, got := p.ClampStored()
		if got != c.clamped || p.SLA.EscrowRecheckDays != c.want {
			t.Errorf("%d: clamped %v days %d, want %v %d", c.in, got, p.SLA.EscrowRecheckDays, c.clamped, c.want)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("%d: %v", c.in, err)
		}
	}
}
