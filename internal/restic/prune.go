package restic

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// PruneStats holds the numbers restic prints at the end of "prune" (which has no
// JSON output, engine §1.9). A line that is absent leaves its fields zero. Byte
// counts are converted from restic's human-readable sizes ("11.250 KiB"), so
// they are exact only to the precision restic prints.
type PruneStats struct {
	RepackBlobs        uint64  // "to repack"
	RepackBytes        uint64  // "to repack"
	RemoveBlobs        uint64  // "this removes"
	RemoveBytes        uint64  // "this removes"
	DeleteBlobs        uint64  // "to delete"
	DeleteBytes        uint64  // "to delete"
	TotalPruneBlobs    uint64  // "total prune"
	TotalPruneBytes    uint64  // "total prune"
	RemainingBlobs     uint64  // "remaining"
	RemainingBytes     uint64  // "remaining"
	UnusedBytesAfter   uint64  // "unused size after prune"
	UnusedPercentAfter float64 // "unused size after prune", percent of the remaining size
}

// ParsePruneText extracts the summary numbers from prune's text output, best
// effort. It never returns the text, never fails, and ignores every line it does
// not recognize (progress bars, the file names restic never prints here, any
// future wording): architecture §7.3 step 5 stores only parsed numbers.
func ParsePruneText(r io.Reader) PruneStats {
	var s PruneStats
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		// restic redraws progress with carriage returns; keep the last segment.
		if i := strings.LastIndexByte(line, '\r'); i >= 0 {
			line = strings.TrimSpace(line[i+1:])
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		switch strings.TrimSpace(key) {
		case "to repack":
			s.RepackBlobs, s.RepackBytes = parseBlobsBytes(rest)
		case "this removes":
			s.RemoveBlobs, s.RemoveBytes = parseBlobsBytes(rest)
		case "to delete":
			s.DeleteBlobs, s.DeleteBytes = parseBlobsBytes(rest)
		case "total prune":
			s.TotalPruneBlobs, s.TotalPruneBytes = parseBlobsBytes(rest)
		case "remaining":
			s.RemainingBlobs, s.RemainingBytes = parseBlobsBytes(rest)
		case "unused size after prune":
			s.UnusedBytesAfter, s.UnusedPercentAfter = parseUnused(rest)
		}
	}
	return s
}

// parseBlobsBytes parses "<n> blobs / <size> <unit>".
func parseBlobsBytes(s string) (blobs, bytes uint64) {
	count, size, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0
	}
	fields := strings.Fields(count)
	if len(fields) == 0 {
		return 0, 0
	}
	n, err := strconv.ParseUint(strings.ReplaceAll(fields[0], ",", ""), 10, 64)
	if err != nil {
		return 0, 0
	}
	return n, parseSize(size)
}

// parseUnused parses "<size> <unit> (<p>% of remaining size)".
func parseUnused(s string) (bytes uint64, percent float64) {
	size, tail, _ := strings.Cut(s, "(")
	bytes = parseSize(size)
	if p, _, ok := strings.Cut(tail, "%"); ok {
		if v, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err == nil && v >= 0 {
			percent = v
		}
	}
	return bytes, percent
}

// parseSize converts "11.250 KiB" (or "3 B", "1.5 GiB") to bytes, 0 if unparsable.
func parseSize(s string) uint64 {
	f := strings.Fields(s)
	if len(f) != 2 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || v < 0 {
		return 0
	}
	var mult float64
	switch f[1] {
	case "b":
		mult = 1
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "tib":
		mult = 1 << 40
	case "pib":
		mult = 1 << 50
	default:
		return 0
	}
	return uint64(v*mult + 0.5)
}
