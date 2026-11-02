// Package config loads and validates the console configuration: a JSON file
// (--config) with a CRYO_* environment override for every key. The file is JSON
// rather than the TOML that architecture §1.1 and §7.1 mention because the Go
// standard library has no TOML parser and the dependency budget (§1.2) does not
// stretch to one; this supersedes those sentences.
//
// config.example.json in this directory is the authoritative example and the
// schema: a test loads it, proves every key in it exists in Config and has an
// environment override (tls.cert is CRYO_TLS_CERT, rate_limits.runs_per_min is
// CRYO_RATE_LIMITS_RUNS_PER_MIN), and that an unknown key in a file is an error.
//
// Secrets never live here. The master key is a file (master_key_path), the TLS
// private key a file, the SMTP password a file (smtp.password_file); this file
// names them. Nothing operator-specific is a default: the display zone defaults
// to America/Chicago because the design says so and is a setting.
//
// Validate enforces the listener rules of architecture §5: the admin listener
// must bind a loopback or private address unless admin_allow_public is set, the
// agent listener needs a certificate and key unless insecure_http_for_lab is on,
// and insecure_http_for_lab itself is refused unless the agent bind address is
// loopback or private.
package config
