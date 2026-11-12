# ADR 0018 - What the agent does when the console refuses its token, and how fast it comes back after an outage

**Date:** 2026-11-11 - **Status:** Proposed (operator rulings M3-L4 and M3-L9 of 2026-11-10; wording to be accepted with the v0.3.0 agent) - **Decider:** operator, recorded by the foreman

The M3 lab run found two agent behaviours worth a decision (docs/lab/m3-run.md, findings L4 and L9):

- **L4.** An agent on a retired device kept calling the console every 3 minutes with its revoked token and got 401
  every time, for days. The console side is right (the token is dead); the agent had no idea what a 401 meant.
- **L9.** After a 20-minute console outage the fleet came back over half an hour, one agent at a time, as each one's
  uplink backoff (1 min doubling to 30 min, architecture §1062) ran out. The board read OFFLINE for them meanwhile,
  which was true, but an operator who has just repaired the console watched the fleet "come back" slowly.

## Decision 1: a 401 from the console means the token is dead; say so once, then knock once an hour

- **What a console 401 is.** The agent API answers `401 unauthorized` only when the bearer token is missing,
  unknown, revoked, expired or belongs to a retired device (`agentapi` doc). Device tokens are not rotated before
  M5, and a valid token used wrongly gets 403, not 401. So a 401 is never a passing phase: it is retire (the usual
  case), a console restored from a copy taken before this enrollment, or an agent pointed at the wrong console.
- **When the agent believes it.** After **three consecutive heartbeats answered 401**. One 401 is not enough (a
  proxy or a half-restored console could produce one); three take at least 1 + 2 min of backoff. A 403, a 5xx or a
  transport failure in between restarts the count: those are not the same signal.
- **Then: REFUSED.** The agent writes one ERROR line (`class=token_refused`, the first 401's time), one Event Log
  entry (id 1008, the existing "retired" id, whose text is the agent's own line, so no message-table change), and
  the journal key `token_refused_since`. `cryo-agent status` shows it in plain words with what to do (re-enroll,
  or uninstall). Every uplink loop (heartbeat, policy, run upload, command poll, snapshot upload) stops its normal
  cadence. **One heartbeat per hour** (jittered) keeps asking, so a console that comes back with the token valid
  again (the restore case, or an operator who re-creates the device row) is found within the hour, with one line
  ("the console accepts the device token again") and every loop kicked. Network failures during the hourly probe
  change nothing: they say nothing about the token.
- **Backups keep running on the last policy.** The agent never concludes on its own that its data is unwanted;
  only `cryo-agent uninstall` does that. If the device was retired, its gateway login is gone too, so each run
  defers with class `auth` on the scheduler's own ladder (5 min doubling to 60 min, architecture §1092): bounded,
  local, and the right outcome if the gateway still accepts the login (the console-restore case, where the data
  keeps landing). A refused agent skips the start lease (the console would answer it 401 too, and the lease is the
  fleet cap, not the right to back up: fail open, as for a console that cannot answer, ADR 0005). Run records
  queue in the outbox (cap 50 000) until the console accepts the token again.
- **Why not stop altogether.** A retired device's agent that stops forever cannot recover from the operator's
  mistake or a console restore without hands on the machine, and a silent agent is what this product exists to
  prevent. The cost is one heartbeat an hour, plus the heartbeat a finished run or a resume from sleep kicks
  (so a retired device on the run ladder sends a few 401s an hour, not one; still nothing like every 3 minutes).
- **Amended 2026-11-12 (operator ruling M3-L10, agent 0.3.1).** Lab step 15 showed that the ladder leaves a
  retired device starting restic about once an hour for good. While the agent is REFUSED, a run that ended
  deferred with class `auth` is next attempted one policy interval after that deferral (a fixed-time schedule
  waits for its next time), not on the ladder: one deferred run and one kicked heartbeat per interval. An `auth`
  deferral while the console accepts the token keeps the ladder. When REFUSED ends, the ladder applies again from
  the last deferral, so the next run starts at once or at most about 72 min later.
- **Why not re-enroll by itself.** It has no enrollment token; minting one is an operator act (ADR 0011).

## Decision 2: the uplink backoff caps at 5 minutes, and one answer wakes every loop

- **Cap 30 min -> 5 min** for the uplink backoff (`uplink.BackoffMax`), still 1 min doubling with 20 % jitter.
  Thundering herd: a fleet of 10 000 agents spread over the last 5 minutes of an outage is about 35 heartbeats a
  second at the worst instant, each a few milliseconds of console time; and the console already has the lever for a
  real herd, `Retry-After`, which the agent honours exactly. The scheduler's run ladder (5 min to 60 min) is a
  different mechanism for a different failure (gateway, restic) and does not change; neither does the enroll retry
  (1 s to 30 s).
- **One answer wakes every loop.** Today only the heartbeat's recovery tells the scheduler the console is back. Now
  any uplink call that succeeds after its loop had failures kicks the other loops (heartbeat first, since the board
  turns ONLINE on it; then policy, outbox, commands). A kick is a non-blocking send on a one-slot channel and the
  heartbeat keeps its minimum gap, so a burst of successes cannot become a burst of requests.
- **Worst case after an outage** becomes about 5 minutes for the last agent, plus one heartbeat; and the moment any
  loop gets through, that agent is ONLINE within the heartbeat's minimum gap.

## Consequences

- Architecture §1062 ("1 min doubling to 30 min") is superseded by this ADR for the uplink. §1162 ("heartbeats keep
  retrying with backoff") holds, with the shorter cap.
- Both are agent changes and ship in the agent 0.3.0 MSI; the console needs nothing.
- The lab sheet gains a step for each: retire a lab device and watch its agent go REFUSED (three 401s, one Event Log
  1008, hourly probes in the console journal, backups deferring `auth` locally); stop the console for 10 minutes and
  watch the fleet come back inside 5 minutes of the restart.
- Backlog: when M5 brings device-token rotation, the 401 rule must be revisited (a rotation window will need its
  own signal; a 401 during rotation must not count towards REFUSED).
