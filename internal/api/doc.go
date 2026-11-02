// Package api holds the wire types of the agent-console protocol, version 1,
// served under /api/v1/agent/*: the request and response structs of all fifteen
// routes, the closed vocabularies they use, the error model, the route, header
// and limit constants, the server-time helpers, and the path-free guard. It is
// shared by the agent (the uplink, ticket P1-T7) and the console (the public
// listener's handlers, ticket P1-T5), which are written in parallel against it.
//
// The package contains types and pure helpers only: no handlers, no HTTP
// client, no I/O, no clock, no logging. It imports the standard library,
// internal/restic (for Kind, Outcome, DeferredReason, ErrorClass,
// ErrorProjection and Snapshot) and internal/policy (for Policy), nothing
// else. The admin API (/api/v1/*, architecture §5.2) is not here yet.
//
// Sources: architecture §5.1 (the route table; its input and output columns are
// the field lists), §5.5 (limits), §1.3 DH4 (the seven calls of ADR D6 and the
// additions), DH5 (server time), DH13 (gateway-password rotation rides the
// policy pull), DH15 (the run error projection), DH17 (idempotent enrollment),
// DH22 (customer-held custody), DH28 (relay restore), §6.1 "uplink", §6.2 (the
// run record), §6.5 (stats and error counts), §8.4 (the path-free rule).
//
// # Who calls what
//
// The agent is the client of every route; the console serves them on the
// public listener (agent listener, TLS required, architecture §5). The device
// id always comes from the bearer token, never from the body.
//
//	enroll     POST /enroll                     EnrollRequest -> EnrollResponse        once, at install; enrollment token
//	lobby      POST /enroll/lobby               LobbyRequest -> 202 LobbyResponse      install without a token; anonymous
//	           GET  /enroll/lobby/{nonce}       -> LobbyResponse                       poll until approved or rejected
//	policy     GET  /policy                     If-None-Match -> PolicyResponse | 304  at start, on the heartbeat's etag hint, 6/min at most
//	heartbeat  POST /heartbeat                  HeartbeatRequest -> HeartbeatResponse  every 60 s +/- jitter, not faster than 20 s
//	runs       POST /runs                       RunsRequest -> RunsResponse            outbox delivery; open runs at start, closed later
//	snapshots  POST /snapshots                  SnapshotsRequest -> SnapshotsResponse  after each successful backup
//	commands   GET  /commands?wait=N            -> CommandsResponse                    long poll, N up to 30 s, one poller per device
//	           POST /commands/{id}/result       CommandResultRequest -> 204            per command, idempotent
//	token      POST /token/rotate               -> TokenRotateResponse                 on token rotation (F09, F21)
//	restores   GET  /restores/{id}              -> RestoreSelection                    after a restore command; the only path-bearing body
//	           GET  /restores/{id}/artifact     Range -> archive stream                relay restore to a replacement device (DH28)
//	progress   POST /progress                   Progress -> 204                        during a run, at most one per 5 s
//	release    GET  /release                    -> ReleaseResponse | 204               self-update check
//	           GET  /release/artifact/{v}/{n}   Range -> bytes                         self-update download
//
// The console is passive: it never connects to an endpoint. Everything an
// operator wants done on a device is a Command that the device fetches.
//
// # Idempotency
//
// A retry must never create a second thing. The keys are: the enrollment nonce
// (EnrollRequest.Nonce; a retried enroll returns the same device and, for
// escrowed custody, re-delivers the repository key until the first heartbeat
// confirms enrollment, DH17); the run id (RunRecord.ID, a ULID the agent
// generates before restic starts; the journal delivers at least once and the
// console answers created, updated or duplicate); the command id (Command.ID;
// the agent keeps a command log and does nothing twice, but posts the result
// again). Lobby requests are idempotent on their nonce too.
//
// # Time
//
// Every response carries HeaderServerTime, the console's clock in RFC 3339
// with milliseconds in UTC (FormatServerTime, ParseServerTime). The agent
// computes its clock offset from it (ClockOffset: device minus server, in
// milliseconds) and the console computes the same from HeartbeatRequest.DeviceTime;
// HeartbeatResponse.ClockOffsetMS reports what the console saw. Run
// timestamps are stored in server time: device time minus the offset, clamped
// to the receipt time (DH5), so a device clock that is three hours fast cannot
// make a backup look fresh. All times on the wire are RFC 3339 strings
// (time.Time) and senders use UTC; durations are integer seconds in fields with
// an _s suffix; sizes are bytes. A timestamp before MinTimestamp (2001-09-09,
// the schema's lower bound) is a clock that was never set and is rejected in
// run records and snapshots.
//
// # Rate limits and body caps
//
// The limits of architecture §5.5 are constants: MaxEnrollBody 4 KiB,
// MaxRunsBody 512 KiB, MaxSnapshotsBody 2 MiB, MaxHeartbeatBody 16 KiB,
// MaxCommandResultBody 64 KiB, MaxProgressBody 4 KiB; HeartbeatMinInterval 20 s,
// ProgressMinInterval 5 s, CommandsLongPollMax 30 s; RunsPerMinute 30,
// SnapshotsPerMinute 6, PolicyPerMinute 6 per device and EnrollPerMinutePerIP 10
// (the only per-IP limit, because agents sit behind customer NATs). Exceeding a
// limit is 429 rate_limited with Retry-After (also Error.RetryAfterS), or 413
// payload_too_large for a body. The agent honours Retry-After and otherwise
// backs off from one minute doubling to thirty (§6.1 uplink). A second
// concurrent long poll is 409 conflict.
//
// # Errors
//
// Every non-2xx answer except 304 is an RFC 9457 problem document with
// Content-Type application/problem+json (ContentTypeProblem), flat, no wrapper
// object (Error):
//
//	{"type":"urn:cryobackup:error:rate_limited","title":"Too many requests; retry later.",
//	 "status":429,"code":"rate_limited","retry_after_s":30,"request_id":"..."}
//
// code is the stable part; type, title and status follow from it. HTTPStatus
// maps a code to its status (invalid_request 400, unauthorized 401, forbidden
// 403, not_found 404, conflict 409, custody_customer_held 409, rate_limited 429,
// payload_too_large 413, not_modified 304, master_key_unavailable 503,
// unavailable 503, internal 500). The title is a fixed sentence per code, never
// built from a request, a path or an exception text. Error.Error renders the
// table sentence rather than the wire title, and for a code that is not in the
// table it renders the fixed form "unknown: Unknown error." without the wire
// code, so neither side can put attacker-chosen text into a log through an
// error (the title, type, request_id and an unknown code are all wire text
// that Error() never repeats). An unknown code from a newer peer is treated as
// 500. Extra members of a problem document are ignored.
//
// # Validation
//
// Every request type, and RunRecord, Command and Progress, has a Validate
// method that returns nil or a *ValidationError listing every problem as a
// {Path, Message} pair sorted by path, like internal/policy. Paths are JSON
// paths ("runs[3].snapshot_id"). Messages are fixed phrases and never repeat a
// value. The rules are the schema's CHECKs and the ticket's: ULIDs are the
// canonical 26-character upper-case form, hex is lower case (nonce 32,
// fingerprint 16 to 128, snapshot id 64); a run record's outcome is set if and
// only if its finish time is set, a deferred outcome carries a deferred reason
// and nothing else does, a snapshot id appears only on a backup. Enum values
// are closed: the console answers an unknown value with 400 invalid_request,
// and Enums lists them all so a test can hold them against replica/schema.sql.
//
// # The path-free rule
//
// CryoBackup carries HIPAA and NDA obligations: no path, path fragment,
// directory name or file name crosses the wire from an endpoint (architecture
// §8.4 items 4 and 8). The wire types therefore have no field that can hold
// one: errors travel as the projection {during, class, ext, count}
// (restic.ErrorProjection, DH15) where ext is an allow-listed extension and
// nothing else, progress is counts, and a command's params are small typed
// objects (a restore command carries only the id of a restore request).
//
// AssertPathFree is the safety net, not the control: the control is that the
// types have no field that can hold a path, and the net catches the day one is
// added by mistake or a free-text field is filled with one. The console runs
// it on every body it ingests and the agent runs it on every body it sends. It
// rejects a string that holds
//
//   - a drive-letter path anywhere in the string (one ASCII letter, ':' and a
//     separator, the letter not following a letter or digit: "C:\x", "x C:/foo",
//     "path:C:\foo"; not "https://", "12:30", "tag:value");
//   - a UNC or device prefix (two backslashes, so "\\server\share" and "\\?\");
//   - a POSIX path of two or more segments after any non-word character or at
//     the start ("/home/x/y", "path:/home/x/y", "url=/etc/passwd"; not
//     "windows/amd64", "2026/10/22" or the "//host/a/b" of a URL);
//   - "~/", "./" or "../" at a token start, and the "file:" and "smb:" URI
//     schemes;
//   - a %VAR% expansion or a backslash followed by a word character.
//
// It looks inside raw JSON (command params and results, the release manifest)
// and at map keys, and returns the JSON path of the first offender, never the
// value. Known limit: a relative path with forward slashes and no leading '/'
// or "./" ("Users/x/a.docx") cannot be told from "windows/amd64" and is not
// detected; neither is a bare file name. Nothing may rely on the net alone.
//
// The guard has exactly two exemptions (architecture addendum §13.3 "Wire
// protocol"), each documented in one place so it can be removed in one place.
//
// The first is RestoreSelection: a restore cannot be described without the
// target and the files to restore, so GET /restores/{id} carries them. It
// travels only on that device-authenticated response, never in the command
// queue; the console stores it encrypted with a purge date; the agent keeps it
// in a SYSTEM-only temp file and deletes it after the restore; nobody logs it.
// The type implements the unexported pathBearing marker and AssertPathFree does
// not look inside it. No other type may implement the marker.
//
// The second is a field skip for restic.Snapshot.Paths in a SnapshotsRequest,
// the configured backup roots as the policy defined them (§5.1 "roots only";
// they are what the restore browser starts from). Validate allows at most
// MaxPathsPerSnapshot of them, each at most 1024 bytes, and the rest of the
// snapshot (host name, tags, program version) is still checked.
// restic.Snapshot.Username is an account name, not a root: it must be empty on
// the wire. Whether roots should travel at all, given that an all-user-profiles
// selection expands to profile directory names, is a question for the operator.
//
// A third exemption needs an ADR, not a convenience.
//
// # Release manifest
//
// ReleaseResponse carries Version, Channel, ResticVersion, the signed Manifest
// (raw JSON, exactly the signed bytes) and Signature, and nothing else. The
// artifact names, sizes and SHA-256 hashes are inside the signed manifest and
// the agent takes them from nowhere else: there is no unsigned artifact list on
// the wire, so an unsigned field cannot steer a download (architecture addendum
// §13.3 "Wire protocol"). The agent verifies Signature over Manifest before it
// reads a name or a hash out of it.
//
// # Snapshot tag ids
//
// Backup snapshots are tagged cb.customer=<customer_id> and
// cb.policy=<policy_id>.<version> (architecture 6.3), and the agent builds the
// tags itself, so the two ids travel to it: EnrollResponse.CustomerID (the
// customer the device belongs to; also delivered in a lobby approval) and
// PolicyResponse.PolicyID (the customer-or-default policy document whose
// Version the response reports; a device override changes neither the id nor
// the version). Both are 26-character ULIDs, are identifiers for tags and
// nothing else, and are not secrets: they are plain strings, not Secret, and
// Redacted leaves them in place. The responses have no Validate method, so this
// package does not check the ids; the console fills them from stored ULIDs.
//
// # Secrets
//
// A credential travels only in the three responses the architecture says
// deliver it once: EnrollResponse (device token, gateway password, repository
// password), PolicyResponse.PendingGatewaySecret (a rotated gateway password,
// DH13) and TokenRotateResponse (a rotated device token); a lobby approval
// carries an EnrollResponse. Each credential is a Secret: it marshals to JSON as
// the plain string and prints as "<redacted>" under every fmt verb. Each of
// those responses has a Redacted method that returns a copy with every Secret
// blanked, which is the form that may reach a log. The repository password is
// empty when custody is customer-held (DH22: the console never had it). The
// endpoint never holds a storage credential: the gateway login is for the
// append-only gateway, not for the bucket.
//
// # Compatibility
//
// The protocol evolves by adding fields. A change that removes or renames a
// field, changes a type or a meaning, or removes an enum value is a new
// version and a new path prefix. Within v1: the agent ignores unknown fields in
// every response (and unknown commands, which it answers failed); the console
// tolerates unknown fields in request bodies from a newer agent but rejects an
// unknown enum value with 400 invalid_request, because a vocabulary the console
// does not know cannot be stored or reasoned about. A new enum value is
// therefore added to the console before the agent that sends it. Optional
// fields are pointers or omitempty and absent means "not known". A nil slice
// encodes as null where the field is not omitempty; readers treat null as
// empty. The JSON keys are snake_case and are the protocol: the golden files
// under testdata pin them.
//
// # Tests
//
// go test ./internal/api: round trip of every type with all fields set, golden
// JSON for EnrollResponse, PolicyResponse, HeartbeatRequest, RunRecord and
// CommandsResponse (go test -update ./internal/api rewrites them), a Validate
// table with passing and failing rows for every rule, the path-free guard
// against Windows, UNC, POSIX and %VAR% forms, enum agreement with
// replica/schema.sql, secret redaction, and the route constants against §5.1
// registered on a real http.ServeMux.
//
// Ticket P1-T4c.
package api
