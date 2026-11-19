// Package notify turns alarm episodes into operator mail (ADR 0020 decisions 4 to 10): the alert
// and recovery messages, the daily digest, the dispatcher that owns one delivery end to end, the
// alerting step of every alarm-evaluator pass (Notifier), the daily-digest job's work, send-now and
// preview (Digester; its doc says how one digest per local date per recipient is kept) and the
// notification-retry job's work (Retrier).
//
// # What a message says
//
// A message is rendered from the state view (state.FleetView) and the episodes of its batch,
// nothing else: customer names, host names, state words, times in the operator's display zone,
// error class words and the console's URL. Never a path, a file name, file contents, a note, a
// pause reason or a policy name: none of those is read here. Bodies are multipart text and HTML
// with inline styles only, no remote content, no image and no link that changes state.
//
// # One message per pass
//
// Everything that became critical in a pass goes to each recipient with the alerts switch as one
// message, grouped by customer (decision 5). Alarm-level states (PARTIAL, MAINT_FAILED,
// ESCROW_UNVERIFIED, OUTDATED) open episodes, so an operator can acknowledge them, but are never
// mailed here: they belong to the digest. An acknowledged alarm, or one of a suspended customer
// (DH10), is held back while that lasts; a snooze that ends while the alarm persists raises it
// again (store.RearmLapsedSnoozes).
//
// A recovery notice follows only an alert that reached someone: an episode closes owing one when a
// delivery row of its alert batch was sent. When every row of the batch failed for good or was
// suppressed, the recovery is settled without a message; while a row is still in retry, it waits.
//
// A retried alert is the original notification, delayed: it carries every episode of its batch
// that is still open, whether or not it has been acknowledged, snoozed or its customer suspended
// since (the mute rules apply when a batch is formed, never to a retry). Closing is the only reason
// an episode is left out of a retry, and its recovery is then settled without a message, unless
// another row of the batch is sent or still in flight from the pass that formed it.
//
// One residual case is accepted. A send held in flight past another recipient's retry that then
// fails, plus a close inside that window, can produce a recovery notice for an alarm that recipient
// was not mailed. It is a spurious "resolved", never a lost alert, and telling it apart would need
// the delivery log to record which episodes each message carried.

// # Batches and crash safety
//
// A batch is the episodes one pass mails together. Its id is the time the pass marks them
// notified, in epoch milliseconds, and every delivery row of the batch carries it in its dedupe
// key: "alert:<ms>:<rule id>" (recovery notices: "recovery:<ms>:<rule id>"). A pass writes in
// this order:
//
//  1. one delivery row per recipient (store.EnqueueDelivery, result queued);
//  2. the episodes marked notified at the batch id (store.MarkEpisodesNotified, one transaction);
//  3. the sends, each followed by its mark on the delivery row.
//
// A crash between 1 and 2 leaves queued rows whose batch id no episode carries. The next pass
// mails the still unnotified episodes under a new batch id, and the retry job, which re-renders
// a row from the episodes of its batch id, finds none for the orphaned rows and suppresses them
// (reason deduped): nothing is mailed twice and nothing is lost. A crash between 2 and 3 leaves
// marked episodes and queued rows; the retry job picks those rows up once they are
// store.QueuedStaleAfter old and sends them. From step 2 on the delivery row owns the message.
//
// A pass sends only rows it created. Two passes that overlap both write rows, but only one marks
// the episodes: the other's mark changes nothing, so it suppresses its rows and sends nothing. A
// batch id that already has a row (another process used that millisecond) is never reused: the
// pass suppresses what it wrote under it and takes the next millisecond, five tries at most.
//
// # Failing loudly
//
// Mail trouble never fails or stalls an evaluator pass: every send has its own deadline and its
// outcome ends in the delivery log, which feeds banner C03. A store error does fail the pass:
// alerting that cannot read its own state is stalled alerting (C06), never a quiet green.
// Log lines carry ids, kinds, counts and class words only.
package notify
