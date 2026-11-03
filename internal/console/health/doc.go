// Package health builds the one fleet view every page, API response and stream
// event is rendered from (architecture §4.6, §5.7): state.Evaluate over a fresh
// facts load plus the console-level banners M0 can see. It persists nothing: a
// health state is never stored (D10).
//
// M0 banners: C08 and C09 come from state.Evaluate; this package adds C04, C05
// and C17 from the self-probe of the public agent endpoint and the C18 warning
// for devices that all stopped together (endpoint.go, ADR 0013: fleet silence
// alone is no longer a console banner), C06 (only when an
// alarm-evaluator job_state row exists; the evaluator is M3/M4), C07 (the
// maintenance worker did not answer its last adminpath probe) and C12 (no master
// key). C10 is raised client-side by fresh.js. The probe-driven C01-C03, the
// gateway half of C05, C11, C13-C16 arrive with their probers; fixtures mode and /design render them
// from canned data (internal/console/fixtures). Entity banners (a MISSING
// customer, customer-wide silence) are derived from the customer rollup.
//
// The ten-day strips (components §4.1) are computed here from finished backup
// runs, bucketed into calendar days in each customer's zone.
package health
