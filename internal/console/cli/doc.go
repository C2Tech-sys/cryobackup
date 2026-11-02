// Package cli implements the cryo-console subcommands (architecture §2, ticket
// P1-T5): init, token, user, serve, doctor, maintain and version;
// cmd/cryo-console is wiring only. "maintain" runs internal/console/maintain with
// the console's key unwrapper (MaintainKeys, built from escrow.LoadMasterKey).
//
// # init
//
// Creates the data directory, the database, the master key (once, never
// overwritten), the default policy from policy.Default, the first storage target
// (credentials are named by credential_ref, never stored), the first owner
// account with a generated password printed once, and config.json (mode 0600).
// It refuses to finish until the operator confirms an offline copy of the master
// key exists (interactive prompt, or --master-key-copy-confirmed with
// --non-interactive).
//
// # token
//
// `token --customer <slug>` or `token --create-customer <name>` (M0 has no
// customers page), with --policy, --expires (12h, 7d, 2w), --uses and --label.
// The secret is printed once; only its SHA-256 is stored.
//
// # user
//
// add, passwd, list, disable. Passwords are generated and printed once, or read
// from standard input with --password-stdin. disable ends the account's sessions.
//
// # serve
//
// Build wires the store, the master key, the auth manager, the three route sets,
// the SSE hub, the maintenance probe and the job runner; serve runs it.
// --fixtures serves the fictional fleet from OpenMemory with a frozen clock and
// an in-process fake admin path, with no agent listener. Admin pages still need
// the sign-in: the fixtures admin is "admin" and its password is printed at
// start (or set CRYO_FIXTURES_PASSWORD).
package cli
