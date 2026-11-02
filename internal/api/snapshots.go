package api

import (
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// maxRootBytes caps one backup root in restic.Snapshot.Paths.
const maxRootBytes = 1024

// SnapshotsRequest is the body of POST /snapshots (architecture §5.1): the
// whole snapshot list of the device from "restic snapshots --json", sent after
// each successful backup, 0 to MaxSnapshotsPerRequest entries, at most
// MaxSnapshotsBody bytes, SnapshotsPerMinute per minute. The list replaces the
// console's cache for the device (an empty list is a list).
//
// Paths of each snapshot are the backup roots only, as policy defined them;
// never file-level paths. Validate rejects more than MaxPathsPerSnapshot of
// them and AssertPathFree exempts exactly this field, which is the one place a
// root travels on an ingest route (see the package documentation). Username is
// an account name and must be empty: the console has no use for it.
type SnapshotsRequest struct {
	Snapshots []restic.Snapshot `json:"snapshots"`
}

// Validate checks the list and every snapshot in it; problems of snapshot i
// are reported under "snapshots[i]".
func (r SnapshotsRequest) Validate() error {
	var c collector
	if len(r.Snapshots) > MaxSnapshotsPerRequest {
		c.add("snapshots", "has too many snapshots")
	}
	for i, s := range r.Snapshots {
		validateSnapshot(&c, idx("snapshots", i), s)
	}
	return c.err()
}

// validateSnapshot checks one snapshot (see SnapshotsRequest).
func validateSnapshot(c *collector, path string, s restic.Snapshot) {
	if !isLowerHex(s.ID, 64, 64) {
		c.add(path+".id", "must be 64 lowercase hex characters")
	}
	if s.ShortID != "" && !isLowerHex(s.ShortID, 8, 8) {
		c.add(path+".short_id", "must be 8 lowercase hex characters")
	}
	if s.Time.IsZero() {
		c.add(path+".time", msgRequired)
	} else if s.Time.Before(MinTimestamp) {
		c.add(path+".time", msgBeforeMin)
	}
	if s.Parent != "" && !isLowerHex(s.Parent, 64, 64) {
		c.add(path+".parent", "must be 64 lowercase hex characters")
	}
	if s.Tree != "" && !isLowerHex(s.Tree, 64, 64) {
		c.add(path+".tree", "must be 64 lowercase hex characters")
	}
	if len(s.Paths) > MaxPathsPerSnapshot {
		c.add(path+".paths", "has too many roots")
	}
	for i, p := range s.Paths {
		switch {
		case p == "":
			c.add(idx(path+".paths", i), msgRequired)
		case len(p) > maxRootBytes:
			c.add(idx(path+".paths", i), msgTooLong)
		case hasControl(p):
			c.add(idx(path+".paths", i), msgControl)
		}
	}
	if s.Hostname != "" {
		c.hostname(path+".hostname", s.Hostname)
	}
	if s.Username != "" {
		c.add(path+".username", "must be empty")
	}
	if len(s.Tags) > 16 {
		c.add(path+".tags", "has too many tags")
	}
	for i, t := range s.Tags {
		c.text(idx(path+".tags", i), t, 64, true)
	}
	c.text(path+".program_version", s.ProgramVersion, maxText, false)
}

// SnapshotsResponse is the 200 body of POST /snapshots (architecture §5.1):
// how many cache rows were inserted or refreshed and how many the list no
// longer contains.
type SnapshotsResponse struct {
	Upserted   int       `json:"upserted"`
	Removed    int       `json:"removed"`
	ServerTime time.Time `json:"server_time"`
}
