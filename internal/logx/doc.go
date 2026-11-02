// Package logx is the structured, path-free logging boundary of the console
// (architecture §8.4 item 2). Device-local run logs are written elsewhere and
// never pass through here.
//
// # Rules
//
//  1. Typed attributes only. Call sites build attributes with ID, Class, Kind,
//     Count, Int, Bool, Dur, Code, Hash, Host, Str and Err. Str and ID accept
//     short tokens ([A-Za-z0-9_.-], 64 bytes, a dot before a letter or digit
//     only in a version); anything else is logged as "invalid", so free text
//     cannot be smuggled in through the typed API.
//  2. Free text is opaque by default (redact.go). Handler (a
//     slog.JSONHandler with a ReplaceAttr) writes a string value, or the
//     message, unchanged only when every white-space-separated token of it is
//     a plain ASCII word, a number or hex code, a version, a duration or size,
//     or one of - : = -> |; anything else (a dot elsewhere, a quote, a slash,
//     a backslash, @ % # ? &, a non-ASCII rune) turns the whole value into
//     "<path:hmac8>", an HMAC of the value under a per-process random key.
//     Over-redaction is accepted; a leak is not. Values the code itself
//     controls go in typed fields, which the handler writes as built: Host
//     (an IP literal or DNS name from configuration), Class, Code, ID, Kind,
//     Str, Hash, Count, Int, Bool, Dur, Err.
//  3. Errors from the os, io/fs and os/exec packages are never printed with
//     err.Error(): *fs.PathError, *os.LinkError and *exec.Error anywhere in the
//     chain reduce to "op:<op> class:<class>" (not_exist, permission, exist,
//     not_found, closed, io).
//
// # Use
//
// Packages log through *logx.Logger (an alias of *slog.Logger) obtained from
// Default(); they never import log/slog, which a repository test enforces for
// the console packages. cmd/cryo-console calls Install() once: it makes the
// path-free logger the process default for logx.Default(), slog.Default() and
// the standard log package. New(w, level) builds a logger for tests.
//
// # Testing
//
// logx_test.go runs Windows, UNC and POSIX paths through every attribute type
// and through raw slog attributes; FuzzNoPathSurvives (go test -fuzz
// FuzzNoPathSurvives ./internal/logx) fails if a path-shaped secret survives.
package logx
