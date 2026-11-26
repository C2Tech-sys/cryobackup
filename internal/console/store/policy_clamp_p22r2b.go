package store

import (
	"sync"

	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

var clampWarned sync.Once

// clampStored is the READ-side rule for sla.escrow_recheck_days (ADR 0033): a stored value of 1 to
// EscrowRecheckMinDays-1, saved before the floor existed, is raised to the floor and never refused, so no
// effective policy, enrollment or facts load fails on it. Writes are refused on their input
// (policy.CheckEscrowRecheckWritten). The first clamp in a process logs one warning: a class and two
// numbers, no path, no policy or device name.
func clampStored(p *policy.Policy) {
	if stored, clamped := p.ClampStored(); clamped {
		clampWarned.Do(func() {
			logx.Default().Warn("a stored policy has an escrow recheck interval below the minimum; using the minimum",
				logx.Class("policy_clamped"), logx.Int("stored_days", stored), logx.Int("used_days", policy.EscrowRecheckMinDays))
		})
	}
}
