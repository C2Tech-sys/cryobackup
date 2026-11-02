// Package buildinfo carries the version stamp that the build injects with
// -ldflags "-X github.com/c2tech-sys/cryobackup/internal/buildinfo.Version=...".
// Every binary prints it, every heartbeat reports it, and the console compares
// it against the release manifest to flag OUTDATED agents.
package buildinfo

import "fmt"

var (
	// Version is the semantic version of this build, "dev" when built from a tree.
	Version = "dev"
	// Commit is the short git commit the build came from.
	Commit = "unknown"
	// Date is the UTC build date in RFC 3339.
	Date = "unknown"
)

// String renders the stamp the way every binary's "version" subcommand prints it.
func String(binary string) string {
	return fmt.Sprintf("%s %s (%s, %s)", binary, Version, Commit, Date)
}
