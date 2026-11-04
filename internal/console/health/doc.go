// Package health builds the one fleet view every page, API response and stream
// event is rendered from (architecture §4.6, §5.7): state.Evaluate over a fresh
// facts load plus the console-level banners M0 can see. It persists nothing: a
// health state is never stored (D10).
//
// Banners: state.Evaluate derives every §4.6 condition from the World this
// package assembles (probes.go): the health-prober's probe facts, the job_state
// heartbeats of alarm-evaluator (C06) and maintenance-scheduler (C07), the
// view facts of the page path (C10, set by Fleet and never by Alarms) and the
// NotInBuild markers of checks whose producer is a later milestone. This package
// adds C04, C04b and the agent-listener C05 from the self-probe of the public
// agent endpoint (endpoint.go, ADR 0013: devices being offline never raise a
// console banner). An Evaluator without a probe source is not wired (C00) and
// keeps the pre-M3 C06, C07 and C12 rules. C10 is also raised client-side by
// fresh.js. Fixtures mode uses FixtureProbes; the /design page renders every
// banner from canned data (internal/console/fixtures). Entity banners (a
// MISSING customer, customer-wide silence) are derived from the customer rollup.
//
// The ten-day strips (components §4.1) are computed here from finished backup
// runs, bucketed into calendar days in each customer's zone.
package health
