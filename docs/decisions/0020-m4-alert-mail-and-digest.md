# ADR 0020 - M4: alert e-mail, the daily digest and the test-mail button

**Date:** 2026-11-18 - **Status:** Accepted (operator, 2026-11-18) - **Decider:** the operator, recorded by the foreman

architecture.md §9 gives M4 one goal: "Alarms reach a human and resolve themselves". Matrix rows r157 to r165 (all
`alerting, must`) define it: daily digest, immediate failure, silence, unseen and expected-customer-missing alerts,
auto-resolve, acknowledge and snooze, SMTP delivery with a test button, and delivery failure as an alarm of its own.

What exists today: the `smtp` block in config.json (never used to send), a connect-only SMTP probe, banners C03/C03b
(e-mail) and C14/C14b (digest) whose send-side facts no code fills, the digest view (`state.DigestOf`), and the tables
`notification_rules`, `notification_log` and `alarm_acks` from migration 0001 that only fixtures write. No sender, no
message builder, no rule, acknowledge or test route, and no mail settings page exist.

The documents disagree with each other in six places. This ADR settles them so that the build has one answer.

## Decision: what M4 builds

1. **Sender.** Standard library only (`net/smtp`, `crypto/tls`), relay-agnostic: STARTTLS (587), implicit TLS (465),
   and plain (25) only with a new explicit `smtp.allow_plaintext: true`. Credentials are never sent over a plain
   connection. The password comes from `smtp.password_file`. Messages are multipart text and HTML, with no remote
   images and no link that changes state.
2. **SMTP settings stay in config.json**, not in the database. The relay and its credential are host configuration
   like the storage location: a console account must not be able to point alert mail somewhere else. Recipients,
   the digest time and the on/off switches are operator settings in the database (ADR 0012).
3. **Recipients are global rules.** A rule is an e-mail address plus what it receives (alerts, recoveries, digest).
   `notification_rules` keeps its scope column, but M4 writes scope `global` only. Per-customer and per-device
   overrides and the customer Notifications tab (S18) move to M11 with row r20, which already says so; this corrects
   architecture.md §9, which listed inheritance in M4. A customer is left out of the digest today through the
   existing policy field `digest_include`; a suspended customer is muted (DH10).
4. **Alerts.** `alarm-evaluator` starts to notify. It mails when a device enters any critical state (FAILED, STALE,
   UNSEEN, BLANK, STUCK, IDENTITY_CONFLICT, UNKNOWN) or a customer becomes MISSING. Rows r158 to r161 name four of
   these; the other critical states are added because a backup that ran and stored nothing is not less urgent than
   one that failed. Alarm-level states (PARTIAL, MAINT_FAILED, ESCROW_UNVERIFIED, OUTDATED) appear in the digest only.
5. **One mail per evaluator pass, not one per device.** Everything that became critical in a pass goes to each
   recipient as one message, grouped by customer. A site that loses power produces one mail, not forty.
6. **Episodes are stored.** States are computed on read and nothing remembers that a mail went out. A new table
   `alarm_episodes` (migration 0008) holds one open row per subject and alarm type: opened at, notified at, closed
   at, recovery notified at. An open episode is never mailed twice, whatever happens to the state's `Since` value
   (which only approximates the first failure of a streak). When the alarm is gone the episode closes and one
   recovery notice goes out (r162; architecture.md §9 acceptance). Nag intervals and failure thresholds stay in M11
   (r166, r167).
7. **Acknowledge and snooze** (r163): per device or customer and per alarm type, with a note, until a date or until
   the next success. It mutes mail and marks the line as acknowledged in the digest. It never changes a state: the
   row stays red (DH8). A snooze that ends while the alarm persists raises the alert again.
8. **Daily digest.** Sent once per local day at the configured time (default 07:00 in the display zone), exceptions
   first, banner state first, grouped by customer. **It is sent every day, also when everything is green**: a digest
   that only arrives on bad days cannot be told apart from a console that has stopped. Dedupe key
   `digest:<local-date>:<rule>`, so one recipient can be retried without mailing the others twice; the state code
   that uses `digest:<local-date>` changes to match. Catch-up at start-up if today's was missed. Preview and
   "send now" (a no-op for a day already sent unless forced) in the UI. No weekly digest: that is r181, M14.
9. **Delivery log and retry.** Every message is a `notification_log` row. A failed send is retried up to three
   times (1, 5 and 15 minutes) by `notification-retry`. Three consecutive failed attempts raise critical banner C03;
   a missed digest raises C14. The loaders for those facts are built here, so the banners stop being blind.
10. **No recipient is an alarm.** SMTP configured and no rule that receives alerts raises a warning banner: alerts
    that go to nobody must not look like a working set-up.
11. **Test mail.** `POST /api/v1/settings/smtp/test` and a button on the new `/settings/email` page send one
    message to an address the admin types (default: their own). The answer names the stage that failed (connect,
    TLS, login, sender refused, recipient refused) as a class word, never the server's raw text. Rate limited,
    audited, owner and admin roles.
12. **Outbound heartbeat ping.** architecture.md lists `healthcheck-ping` for M3; no code for it was found. M4 builds
    it: an optional GET to an operator-configured URL while the evaluator is healthy, so an external monitor notices
    a console that died. Without it "the mail system is down" can only be reported by the mail system.
13. **`cryo-console doctor`** gains a mail check (config valid, password file readable, probe result).

## Decision: what an e-mail may contain

Operator mail carries customer names, host names, states, times in the display zone and error classes. It never
carries a file path, a file name or file contents (architecture.md, the notification rows of the job table; replica/design/components.md, "Email templates"). Messages
are built from the state view only, and the test suite greps rendered bodies for seeded path-like and patient-like
strings. Mail leaves through the operator's relay: whether that relay is acceptable for customer and host names is
the operator's choice, and the deployment guide says so.

## Not in M4

Webhooks, the Syncro adapter and other PSA tickets (r168, r169, r172); per-customer rules and S18 (r20); nag
intervals and consecutive-failure thresholds (r166, r167); mail to a customer's own contacts (r180); per-customer
status reports (r174); weekly and grouped-status variants (r181); per-customer time zones (r241).

## Consequences

- Schema v8: `alarm_episodes`. The three tables of migration 0001 come into use as designed.
- New pages `/settings/email` (delivery status, test, recipients, digest time, delivery log) and an Acknowledge
  action on device and customer pages. C03 and C14 link to the e-mail page instead of the System row.
- The `daily-digest` "not in this build" marker is removed, so C14 becomes a live alarm on every installation:
  after the upgrade a console with no SMTP block shows it until mail is configured or the digest is switched off.
- A console without SMTP keeps working as today, with the banners saying mail is not set up.
- Build order: sender and test mail first (usable by itself), then episodes and alerts, then the digest, then
  acknowledge. Each under fable-foreman with blind verification, then a lab run against a real relay.

## Operator rulings (2026-11-18)

1. Immediate mail for every critical state (decision 4): yes.
2. Per-customer recipients deferred to M11 (decision 3): yes.
3. Digest every day, all-green days included (decision 8): yes.
4. Heartbeat ping in M4 (decision 12): yes.
