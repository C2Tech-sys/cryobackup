// Package probe is the console's self-probe of its own public agent endpoint
// (ADR 0013, superseding the fleet-silence rule of banner C04 in architecture
// §4.6). On a timer it does what an agent does before its first request:
//
//  1. dns: resolve the public_url host (skipped for an IP literal);
//  2. connect: open a TCP connection to one of the addresses (or to the
//     configured agent_probe.connect_address, for a host behind NAT without
//     hairpin; DNS is still resolved and checked);
//  3. tls: a TLS 1.2+ handshake, then the chain and the host name are verified
//     against the system roots, or against agent_probe.ca_file when the
//     deployment uses a private CA; with agent_probe.spki_pin set, the leaf's
//     SubjectPublicKeyInfo must also match it, as a pinned agent requires;
//  4. certificate_expired: the leaf's notAfter against the console clock;
//  5. responder: an unauthenticated GET of the agent policy route, which this
//     console's agent API answers in its own known way: a 401 (or 429) RFC 9457
//     problem document with code "unauthorized" (or "rate_limited") and the
//     X-CryoBackup-Server-Time header. Anything else (a proxy's error page, an
//     old host, another service) is the wrong responder.
//
// The first stage that fails is the result. Every network stage, the responder
// included, has its own bounded timeout and the whole check is bounded, so a hung network never stalls the loop; Run
// is started after the listeners are bound and never blocks startup. Logs are
// path-free and token-free: the stage, an error class, and only on a change.
// No request carries a credential.
//
// The package keeps the last result in memory; internal/console/health turns
// a Status into banners (C04, C05, C04b). Nothing is persisted.
package probe
