// Package mail builds and sends the console's operator e-mail (ADR 0020):
// alerts, recoveries, the daily digest and the test mail. It owns three things
// and nothing else: the message builder (Message, Build), the sender (Sender,
// SMTPSender, FakeSender) and the typed failure (SendError). What a message says
// is the business of the callers; no alert or digest template lives here.
//
// # The sender
//
// Standard library only (net/smtp, crypto/tls, mime). The relay comes from the
// smtp block of config.json (Settings, SettingsFromConfig):
//
//   - starttls: STARTTLS is required. A relay that does not offer it is a
//     failure (class starttls_missing), never a silent fall back to clear text.
//   - tls: implicit TLS from the first byte.
//   - plain: only with smtp.allow_plaintext, and never with credentials.
//
// TLS is 1.2 or later and the relay certificate is verified against the host
// name of the configuration. AUTH is attempted only on an encrypted connection
// (PLAIN, or LOGIN when that is all the relay offers). The password file is read
// at send time, so a rotated password needs no restart; on Unix a file readable
// by group or others is refused.
//
// A send is all or nothing: if any recipient is refused, no message is
// transmitted, so a retry cannot deliver twice to the recipients that were
// accepted.
//
// # Failures
//
// Every error Send returns is a *SendError: a stage (connect, tls, auth,
// sender, recipient, data, config), a class from a fixed word list, and whether
// a retry may succeed. The text a relay answers with, the host, the addresses
// and the password never appear in an error string or in a log line: they can
// carry customer and host names (architecture.md, the HIPAA posture of
// CLAUDE.md), so the class words are all an operator sees. Hint turns a class
// into one fixed sentence for a page.
//
// # Messages
//
// Build renders RFC 5322: multipart/alternative with a text part and an HTML
// part, UTF-8, quoted-printable, encoded-word subjects, a Date, a Message-ID
// under the From domain and Auto-Submitted: auto-generated. The builder adds no
// remote content (no image, no tracking pixel, no link). A CR or LF in any
// header value is an error; nothing is stripped silently.
package mail
