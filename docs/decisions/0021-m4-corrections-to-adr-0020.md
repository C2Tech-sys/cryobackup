# ADR 0021 - M4: two corrections to ADR 0020 (the heartbeat job, and what a retried alert carries)

**Date:** 2026-11-19 - **Status:** Accepted (operator, 2026-11-19) - **Decider:** the operator, recorded by the foreman

ADR 0020 stands. Two of its statements were wrong or incomplete once the build met the code. Decisions are not
edited in place, so they are corrected here.

## 1. The heartbeat job already existed (corrects decision 12)

ADR 0020 decision 12 says of `healthcheck-ping`: "no code for it was found. M4 builds it." That was a miss in the
recon: a job of that name (`jobs.HealthcheckPing`) had been registered since M3. What M4 does is **replace** it with
a hardened job of the same name, in one commit, because the runner refuses two jobs with one name.

What changed for an operator who already had a ping URL configured:

- The URL must be `https`, unless the host is loopback. A console whose `healthcheck_ping.url` is plain `http` to
  another host refuses to start; `cryo-console doctor` reports it before the upgrade is applied.
- A URL with embedded credentials, surrounding white space or a port outside 1 to 65535 is refused, with a fixed
  message that never repeats the URL (it carries a secret token).
- The ping is withheld while the alarm evaluator is unhealthy (its last pass failed or is older than three of its
  intervals). The missing ping is the alarm.
- Redirects are not followed. A 2xx reply whose body stalls or errors counts as a failure. Three consecutive
  failures raise a warning banner. The interval must be between 60 and 86400 seconds.
- Proxy environment variables are honoured: the proxy sees the host, never the path or the token.

## 2. A retried alert is the original notification, delayed (completes decisions 7 and 9)

ADR 0020 says an acknowledgement or snooze "mutes mail" (decision 7) and that a failed send is retried at 1, 5 and
15 minutes (decision 9). It does not say what happens when an alarm is muted between the first attempt and a
retry. Three designs that dropped such an alarm from the retried message each lost mail in some interleaving: a
mute can be undone, one alert message covers many alarms and goes to several recipients, and the delivery log does
not record which alarms a given message carried.

The rule:

- Mute rules (acknowledged, snoozed, customer suspended) apply **when an alert batch is formed**, never to a retry.
- A retried alert carries every alarm of its batch that is **still open**, including one that was acknowledged,
  snoozed or whose customer was suspended after the first attempt.
- The only reason an alarm is left out of a retried alert is that it has **closed**. Closing is permanent, which is
  what makes this rule safe where the others were not.
- A recovery notice is owed only for an alarm whose batch had at least one alert delivered.

Cost, accepted: an alarm acknowledged inside the retry window (at most about 21 minutes after the first attempt)
can still arrive by mail once.

One residual class, accepted: if an alarm closes while its alert is still undelivered to every recipient, and at
least one of those deliveries is still queued and has never been attempted, each recipient who receives recovery
notices can get a "resolved" notice for an alarm nobody was mailed. Three known ways in: a send held in flight
that then fails, a console restart between forming the batch and sending it, and a delivery deferred because an
earlier send used up the pass's time budget. It is a spurious notice, never a lost alert.

The alternative that was not taken: recording, per delivery, the alarms the message carried. It needs a schema
change and a second source of truth for "was this alarm mailed"; it can be revisited if the cost above is felt.

## Also recorded: banners added during the build

ADR 0020 decision 10 asked for one new warning (no alert recipient). The build added four, each because a silent
path was found:

| Id | Level | Raised when |
|---|---|---|
| C18 | warning | SMTP is configured and no recipient receives alerts |
| C19 | warning | the external heartbeat ping has failed three times in a row |
| C20 | warning | the digest is on, SMTP is configured, and no recipient receives the digest |
| C21 | warning | one or more messages could not be delivered in the last 24 hours |

`cryo-console doctor` no longer applies migrations: it opens the database read-only and reports pending
migrations, so it is safe to run with a new binary before an upgrade.
