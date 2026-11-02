# ADR 0009 - Policy changes are previewed, and the save applies exactly the preview

**Date:** 2026-10-30 - **Status:** Accepted - **Decider:** foreman (M2 build), recorded for the operator

M2 requires that "saving a policy shows the device count and changed fields first" (architecture §10.2, M2
acceptance). A preview that the save does not enforce is decoration: the form can be edited after the review, a
second admin can change the policy in between, or an API client can skip the review entirely. Policies also have
to be stored in a form that composes predictably (DH6) while the editor shows complete documents.

## Decision

- **Every policy mutation with fleet impact is two steps: preview, then confirm.** This covers editing a policy,
  deleting one, and assigning one to a customer. Per-device overrides affect only one device, so they are excluded.
  The preview returns the number of affected devices (within the caller's scope), the changed fields (old -> new)
  and an **impact token**.
- **The token binds the confirmation to exactly what was previewed.** It is an HMAC over the kind of change, the
  object id, the object's version at preview time, a digest of the proposed change and the previewing admin. It
  expires after 15 minutes. The save recomputes the claims, and the save refuses when:
  - the token is missing: a field error on `impact_token`;
  - the token was minted for a different change or by a different admin: 409 `ErrImpactMismatch`;
  - the object moved on since the preview, or the token expired: 409 `ErrImpactStale`.
  A stale token means: preview again.
- **The HMAC key lives only in memory and changes with every console process.** A restart invalidates every
  outstanding preview, which costs the operator one more click. In exchange there is no key to store, rotate or
  leak, and no token survives an upgrade. This follows the same rule as restore download tokens and password-reset
  links.
- **There is no bypass.** API clients get the same flow: `POST /api/v1/policies/{id}/impact` previews an edit, a
  delete (`{"delete":true}`) or an assignment to a customer. The matching save, delete or customer PATCH requires
  the token. Creating a customer with an initial policy needs no token, because the customer has no devices yet.
- **Policies are stored sparse.** Template and customer policies are stored as RFC 7386 merge patches against the
  default policy, as DH6 requires. The editor shows and edits the fully composed document, and turns it back into
  a patch on save. A change to the default policy is previewed against every customer whose composed policy it
  alters. A default edit that would leave any customer's composed policy invalid is refused at preview and again
  at save.
- **Audit rows record field names and counts, never values.** The audit store's path guard would reject values
  such as selection roots anyway, and the revisions table keeps the full documents.

## Consequences

- An admin cannot save a policy they have not just reviewed, and cannot save one someone else changed after their
  review.
- A console restart between preview and save forces a new preview. That is deliberate.
- A scripted change takes two calls, preview and confirm. That is the intended friction for a fleet-wide change.
