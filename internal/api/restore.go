package api

import "time"

// pathBearing marks a wire type that carries file-system paths on purpose.
// AssertPathFree does not look inside a value whose type implements it.
//
// The guard has exactly two exemptions (architecture addendum §13.3 "Wire
// protocol"): the pathBearing marker, which only RestoreSelection implements
// (architecture §8.4 item 4), and the field skip for restic.Snapshot.Paths,
// the configured backup roots of a snapshot. Adding a third is a decision for
// an ADR, not a convenience.
type pathBearing interface{ pathBearing() }

// RestoreSelection is the answer of GET /restores/{id} (architecture §5.1, DH4,
// DH28, §6.1 "restore executor"): what the agent restores and where. It is the
// single path-bearing type of the protocol: Target and Includes are paths on
// the endpoint, because a restore cannot be described without them.
//
// Handling rules (§8.4 item 4): it travels only on this device-authenticated
// response, never in the command queue; the console stores it encrypted with a
// purge date; the agent writes it to a SYSTEM-only temp file and deletes it
// after the restore; nobody logs it. AssertPathFree skips this type by its
// pathBearing marker.
//
// Mode device_original restores to the original locations (Target empty),
// device_path to Target, device_relay downloads ArtifactURL (the route
// RouteRestoreArtifact(ID), DH28), checks ArtifactSHA256 and extracts into
// Target. IncludeFile says Includes are to be given to restic as an include
// file, not as --include flags. Verify and Simulate are mutually exclusive
// (restic cannot verify a dry run).
type RestoreSelection struct {
	ID             string      `json:"id"`          // restore request id, 26-character ULID
	SnapshotID     string      `json:"snapshot_id"` // 64 lowercase hex
	Mode           RestoreMode `json:"mode"`
	Target         string      `json:"target,omitempty"`
	Includes       []string    `json:"includes,omitempty"`
	IncludeFile    bool        `json:"include_file"`
	Overwrite      Overwrite   `json:"overwrite"`
	Verify         bool        `json:"verify"`
	Simulate       bool        `json:"simulate"`
	ArtifactURL    string      `json:"artifact_url,omitempty"`    // relay only
	ArtifactSHA256 string      `json:"artifact_sha256,omitempty"` // relay only, 64 lowercase hex
	ExpiresAt      time.Time   `json:"expires_at"`
}

func (RestoreSelection) pathBearing() {}
