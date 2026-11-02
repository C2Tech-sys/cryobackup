// Package server owns the console's two HTTP listeners, their middleware, TLS
// and the route registry. Ticket P1-T5.
//
// The agent and admin listeners are built from two separate http.ServeMux
// values wrapped by Router, which records every registered pattern so tests can
// prove the trees are disjoint and that every route is covered. Each mux has
// its own catch-all: problem+json 404 on the agent side, plain 404 on the admin
// side.
//
// Listen binds the sockets (idempotent, so tests can learn the :0 addresses),
// and Serve runs both until the context ends, then shuts down gracefully.
// Binding rules: the admin listener (plain HTTP, fronted by the operator's TLS
// terminator) refuses a non-loopback, non-private address unless the operator
// passes --admin-allow-public; the agent listener requires tls.cert and
// tls.key unless --insecure-http-for-lab is set on a loopback or private bind.
// The certificate is re-read when the files change; the minimum TLS version is
// 1.2.
//
// Logging: the access log writes the route pattern (PathOf), status, duration
// and request id. It never writes the query string, so a download token in a
// query cannot reach a log on either listener.
package server
