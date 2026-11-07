package agent

import (
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
)

// versionLabel is the build stamp as the agent reports it: on the wire, in the
// journal and in its log. A stamp that is not version-shaped becomes
// api.UnknownVersion, as the console would store it. The path-free guards in the
// journal and the uplink stay as they are for real run data; a backup must never
// stop over a label they refuse.
func versionLabel() string { return api.VersionLabel(buildinfo.Version) }
