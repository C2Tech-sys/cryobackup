package adminpath

import (
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Bounds that restic's builders do not impose.
const (
	maxIDLen      = 64
	maxPathLen    = 4096
	maxPatternLen = 1024
	maxList       = 64
	maxTagLen     = 256
)

// Validate checks the device id and the request id. It returns a
// *ValidationError naming the field.
func (r DeviceRequest) Validate() error {
	if !validID(r.DeviceID) {
		return invalidField("device_id")
	}
	if r.RequestID != "" && !validID(r.RequestID) {
		return invalidField("request_id")
	}
	return nil
}

// Validate checks the embedded DeviceRequest.
func (r InitRequest) Validate() error { return r.DeviceRequest.Validate() }

// Validate checks the device, the snapshot id (hex 8 to 64 digits or "latest"),
// Dir (absolute, starting with "/") and Limit (0 to MaxLsLimit).
func (r LsRequest) Validate() error {
	if err := r.DeviceRequest.Validate(); err != nil {
		return err
	}
	if !validSnapshot(r.SnapshotID, false) {
		return invalidField("snapshot_id")
	}
	if !validSnapPath(r.Dir) {
		return invalidField("dir")
	}
	if r.Limit < 0 || r.Limit > MaxLsLimit {
		return invalidField("limit")
	}
	return nil
}

// Validate checks the device, the pattern (non-empty, not starting with "-"),
// the snapshot ids and Limit (0 to MaxFindLimit).
func (r FindRequest) Validate() error {
	if err := r.DeviceRequest.Validate(); err != nil {
		return err
	}
	if len(r.Pattern) > maxPatternLen {
		return invalidField("pattern")
	}
	if _, err := restic.Find(restic.FindOptions{Pattern: r.Pattern}); err != nil {
		return invalidField("pattern")
	}
	if len(r.SnapshotIDs) > maxList {
		return invalidField("snapshot_ids")
	}
	for _, id := range r.SnapshotIDs {
		if !validSnapshot(id, false) {
			return invalidField("snapshot_ids")
		}
	}
	if r.Limit < 0 || r.Limit > MaxFindLimit {
		return invalidField("limit")
	}
	return nil
}

// Validate checks the device, the snapshot id (optionally with ":<subfolder>")
// and Path (absolute, starting with "/").
func (r DumpRequest) Validate() error {
	if err := r.DeviceRequest.Validate(); err != nil {
		return err
	}
	if !validSnapshot(r.SnapshotID, true) {
		return invalidField("snapshot_id")
	}
	if !validSnapPath(r.Path) {
		return invalidField("path")
	}
	return nil
}

// Validate checks the device and the stats mode.
func (r StatsRequest) Validate() error {
	if err := r.DeviceRequest.Validate(); err != nil {
		return err
	}
	if _, err := restic.Stats(r.Mode, "", restic.Common{}); err != nil {
		return invalidField("mode")
	}
	return nil
}

// Validate checks the device and rejects RemoveAll without Quiesced.
func (r UnlockRequest) Validate() error {
	if err := r.DeviceRequest.Validate(); err != nil {
		return err
	}
	if r.RemoveAll && !r.Quiesced {
		return invalidField("quiesced")
	}
	return nil
}

// Validate checks the device, the keep counts, KeepWithin (required) and the
// tags, with the rules of restic.Forget.
func (r ForgetRequest) Validate() error {
	if err := r.DeviceRequest.Validate(); err != nil {
		return err
	}
	const okWithin = "1d"
	base := restic.ForgetOptions{
		Host:        r.DeviceID,
		KeepLast:    r.Rules.KeepLast,
		KeepHourly:  r.Rules.KeepHourly,
		KeepDaily:   r.Rules.KeepDaily,
		KeepWeekly:  r.Rules.KeepWeekly,
		KeepMonthly: r.Rules.KeepMonthly,
		KeepYearly:  r.Rules.KeepYearly,
		KeepWithin:  okWithin,
	}
	if _, err := restic.Forget(base); err != nil {
		return invalidField("rules")
	}
	if _, err := restic.Forget(restic.ForgetOptions{Host: r.DeviceID, KeepWithin: r.KeepWithin}); err != nil {
		return invalidField("keep_within")
	}
	if len(r.KeepTags) > maxList {
		return invalidField("keep_tags")
	}
	for _, t := range r.KeepTags {
		if len(t) > maxTagLen {
			return invalidField("keep_tags")
		}
	}
	base.KeepTags = r.KeepTags
	if _, err := restic.Forget(base); err != nil {
		return invalidField("keep_tags")
	}
	return nil
}

// Validate checks the device, MaxUnused and MaxRepackSize with the rules of
// restic.Prune.
func (r PruneRequest) Validate() error {
	if err := r.DeviceRequest.Validate(); err != nil {
		return err
	}
	if _, err := restic.Prune(restic.PruneOptions{MaxUnused: r.MaxUnused}); err != nil {
		return invalidField("max_unused")
	}
	if _, err := restic.Prune(restic.PruneOptions{MaxRepackSize: r.MaxRepackSize}); err != nil {
		return invalidField("max_repack_size")
	}
	return nil
}

// Validate checks the device and ReadDataSubset with the rules of restic.Check.
func (r CheckRequest) Validate() error {
	if err := r.DeviceRequest.Validate(); err != nil {
		return err
	}
	if _, err := restic.Check(restic.CheckOptions{ReadDataSubset: r.ReadDataSubset}); err != nil {
		return invalidField("read_data_subset")
	}
	return nil
}

// validID accepts 1 to 64 characters of [A-Za-z0-9_-] starting with a letter or
// a digit, so an id can never be read as a flag or contain a separator.
func validID(s string) bool {
	if s == "" || len(s) > maxIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if alnum || (i > 0 && (c == '_' || c == '-')) {
			continue
		}
		return false
	}
	return true
}

// validSnapshot applies internal/restic's snapshot id rule by asking its
// builders, so the two can never drift apart.
func validSnapshot(id string, allowSub bool) bool {
	if len(id) > maxPathLen {
		return false
	}
	if allowSub {
		_, err := restic.Dump(id, "/", "x", restic.Common{})
		return err == nil
	}
	_, err := restic.Ls(id, "/", restic.Common{})
	return err == nil
}

// validSnapPath accepts an absolute snapshot path: non-empty, starting with "/",
// no control characters, at most 4096 bytes.
func validSnapPath(p string) bool {
	if p == "" || p[0] != '/' || len(p) > maxPathLen {
		return false
	}
	_, err := restic.Ls("latest", p, restic.Common{})
	return err == nil
}
