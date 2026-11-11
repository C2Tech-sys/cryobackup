// Package uplink is the agent's HTTPS client for the console's agent API
// (architecture §5.1, §6.1 "uplink"). It speaks internal/api: Enroll, PullPolicy,
// Heartbeat, UploadRuns, UploadSnapshots, PollCommands, CommandResult.
//
// # Rules the client enforces
//
//   - Every outgoing body passes api.AssertPathFree before it is encoded;
//     a failure returns ErrPathLeak and nothing is sent (§8.4 item 8). The
//     error names the JSON path of the offender, never its value.
//   - Authentication is the device token as Bearer (the enrollment token on
//     /enroll); the token is read through a function on every request so a
//     rotated token applies at once. No secret is ever part of an error string.
//   - Every response's X-CryoBackup-Server-Time updates the clock offset
//     (device minus server, milliseconds; OffsetMS), the figure the heartbeat
//     reports and "status" prints (DH5).
//   - Errors are *Error values with fixed text: the HTTP status, the stable api
//     code, Retry-After (header or retry_after_s, capped at an hour) and a
//     class (network, tls, auth) for last_error_class. The problem title and
//     any other wire text are discarded.
//   - Retry-After is honoured by Backoff.Delay; otherwise Backoff doubles from
//     one minute to five (ADR 0018) with 20 % jitter and is reset after a success. Each
//     loop of the agent owns a Backoff; the client itself never sleeps.
//   - The command long poll is single-flight: a second concurrent PollCommands
//     returns ErrPollInFlight without a request (§5.5, the console answers 409).
//   - The policy pull sends If-None-Match and reports a 304 as "unchanged".
//   - TLS 1.2 or newer with the system roots. InsecureLab (the lab flag) allows
//     http and skips verification; SPKIPin (base64 SHA-256 of the server's
//     public key info) is enforced whenever it is set; Proxy overrides the
//     environment's proxy. Redirects are never followed.
//
// The client holds no state beyond the clock offset and the poll guard; the
// scheduler and the outbox decide when to call it, so it never blocks them.
package uplink
