// Package scheduler decides when the agent starts a backup (architecture §6.1
// "scheduler", §6.2, §6.6, §13.3).
//
// Plan is a pure function of an Input (device-local time, policy, journal
// facts, backoff state, pending triggers) and returns an Output: start, skip or
// nothing, the trigger label for the run, the reason (a fixed vocabulary shown
// by "cryo-agent status") and the earliest time worth re-planning. It is
// tested without timers. Due-ness is recomputed from the journal's last success
// on every plan; no next-run is stored.
//
// What Plan does, in order: a backup_now bypasses window, conditions, startup
// delay and backoff (not pause or hold); a run in progress, pause, hold and
// startup_delay_s hold everything back; a pending retry starts once its
// backoff gate has passed and the window is open; otherwise policy.Due
// decides, with policy.Jitter applied once per slot, counted from the first
// time the slot was seen due. The first backup ("never") and a clock-skew slot
// start without jitter. The window gates starts only and never stops a running
// job (policy.Due and the retry path apply it).
//
// Fleet cap (M1, ADR 0005): the agent asks the console for a start lease
// before a scheduled run. A refusal is journaled as a deferred run with reason
// fleet_cap, which FactsFromRuns ignores like a skipped run (no attempt, no
// backoff step, the slot stays open), and Input.LeaseNotBefore holds every
// start but backup_now until the console's retry_after_s (plus jitter) passes.
//
// RetryDelay is the failure backoff: 5 min x 2^n capped at 60 min (the
// policy's retries section) with plus or minus 20 % jitter; lock deferrals use
// 2 min to 15 min. Loop runs Plan every 60 s and on triggers (boot, command,
// policy change, connectivity, resume) and calls Start or Skip.
//
// Resume (M1): Loop.NotifyResume re-plans at once; resumes within
// ResumeDebounce (30 s) of an accepted one are dropped. The loop passes the
// fact to Plan as Input.Resume, and a run Plan finds due is labelled
// api.TriggerWake ("wake", the run vocabulary; TriggerResume is only the
// loop's name for the reason) when the policy's run_on_wake is on, ahead of
// catch-up and behind boot and retry. A resumed plan that only finds the run
// waiting out its jitter keeps the label for the plan that starts it.
//
// Not in M0/M1: the conditions evaluator (battery, metered, idle, Wi-Fi, VPN)
// and therefore run_anyway_after_minutes, and a logon trigger (session
// changes are recorded and ignored). A due run is never delayed by a
// condition.
package scheduler
