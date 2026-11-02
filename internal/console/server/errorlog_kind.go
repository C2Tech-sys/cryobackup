package server

import "strings"

// classify reduces a net/http error message to a class and a kind from a fixed
// vocabulary. It reads the message and keeps none of it: the kind is chosen by
// matching the start of the part net/http and crypto/tls wrote, never a
// substring of text a peer could shape, and an unrecognised reason is "other".
func classify(msg string) (class, kind string) {
	m := strings.TrimSpace(msg)
	switch {
	case strings.HasPrefix(m, handshakePrefix):
		return ClassTLSHandshakeFailed, handshakeKind(strings.TrimPrefix(m, handshakePrefix))
	case strings.HasPrefix(m, "http: Accept error"):
		return ClassHTTPServerError, "accept"
	case strings.HasPrefix(m, "http: panic serving"):
		return ClassHTTPServerError, "panic"
	case strings.HasPrefix(m, "http: superfluous response.WriteHeader"):
		return ClassHTTPServerError, "write_header"
	case strings.HasPrefix(m, "http2:"):
		return ClassHTTPServerError, "http2"
	}
	return ClassHTTPServerError, "other"
}

const handshakePrefix = "http: TLS handshake error from "

// handshakeKinds is the vocabulary of tls_handshake_failed reasons. A scanner
// produces not_tls, eof, reset, timeout, client_hello_malformed and
// unsupported_version; the others point at a real client or at this server's own
// certificate and deserve a look.
var handshakeKinds = []struct{ prefix, kind string }{
	{"EOF", "eof"},
	{"read tcp", "reset"}, // "read tcp a:b->c:d: read: connection reset by peer" and its kin
	{"write tcp", "reset"},
	{"tls: first record does not look like a TLS handshake", "not_tls"},
	{"client sent an HTTP request to an HTTPS server", "not_tls"},
	{"tls: client offered only unsupported versions", "unsupported_version"},
	{"tls: no cipher suite supported by both client and server", "no_shared_cipher"},
	{"tls: client requested unsupported application protocols", "no_shared_protocol"},
	{"tls: unsupported SSLv2 handshake received", "client_hello_malformed"},
	{"tls: received unexpected handshake message", "client_hello_malformed"},
	{"tls: oversized record received", "client_hello_malformed"},
	{"tls: handshake message of length", "client_hello_malformed"},
	{"tls: invalid ClientHello", "client_hello_malformed"},
	{"tls: client sent", "client_hello_malformed"},
	{"remote error: tls: bad certificate", "bad_certificate"},
	{"remote error: tls: certificate expired", "certificate_expired"},
	{"remote error: tls: unknown certificate authority", "unknown_ca"},
	{"remote error: tls: unknown certificate", "unknown_certificate"},
	{"remote error: tls: certificate revoked", "certificate_revoked"},
	{"remote error: tls: handshake failure", "handshake_failure"},
	{"remote error: tls: protocol version", "unsupported_version"},
	{"remote error: tls: internal error", "peer_internal_error"},
	{"remote error: tls: user canceled", "peer_closed"},
	{"remote error: tls: unexpected message", "client_hello_malformed"},
	{"tls: no certificates configured", "server_certificate"},
	{"tls: internal error", "server_certificate"},
	{"tls: failed to sign", "server_certificate"},
}

// handshakeKind derives the kind from what follows "from ": "ip:port: reason".
// The address ends at the first ": " (an address holds no space). A timeout
// carries no fixed text of its own, so it is recognised by its suffix.
func handshakeKind(rest string) string {
	i := strings.Index(rest, ": ")
	if i < 0 {
		return "other"
	}
	reason := rest[i+2:]
	if strings.HasSuffix(reason, "i/o timeout") || strings.HasSuffix(reason, "deadline exceeded") {
		return "timeout" // before the prefixes: "read tcp ...: i/o timeout" is a timeout, not a reset
	}
	for _, k := range handshakeKinds {
		if strings.HasPrefix(reason, k.prefix) {
			return k.kind
		}
	}
	return "other"
}
