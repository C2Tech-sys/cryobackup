package restic

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// ErrMalformed is returned by the whole-document decoders when the output is
// not the JSON document they expect. It carries no text from the output.
var ErrMalformed = errors.New("restic: malformed output")

// maxDocument bounds how much a whole-document decoder reads (256 MiB: a
// snapshot list of a long-lived repository, or an ls of a large directory).
const maxDocument = 256 << 20

// Snapshot is one snapshot as "snapshots --json" and "forget --json" print it.
//
// PATH-BEARING: Paths holds the backed-up roots, Hostname and Username name the
// machine and account. It exists for the snapshot cache and restore browsing
// only; never pass it to a logger.
type Snapshot struct {
	ID             string         `json:"id"`
	ShortID        string         `json:"short_id"`
	Time           time.Time      `json:"time"`
	Parent         string         `json:"parent"`
	Tree           string         `json:"tree"`
	Paths          []string       `json:"paths"`
	Hostname       string         `json:"hostname"`
	Username       string         `json:"username"`
	Tags           []string       `json:"tags"`
	ProgramVersion string         `json:"program_version"`
	Summary        *BackupSummary `json:"summary"`
}

// StatsResult is the object "stats --json" prints (named StatsResult because Stats
// is the command builder). Which fields are present depends on
// the mode; absent ones are zero.
type StatsResult struct {
	TotalSize              uint64  `json:"total_size"`
	TotalUncompressedSize  uint64  `json:"total_uncompressed_size"`
	CompressionRatio       float64 `json:"compression_ratio"`
	CompressionProgress    float64 `json:"compression_progress"`
	CompressionSpaceSaving float64 `json:"compression_space_saving"`
	TotalFileCount         uint64  `json:"total_file_count"`
	TotalBlobCount         uint64  `json:"total_blob_count"`
	SnapshotsCount         int     `json:"snapshots_count"`
}

// FindMatch is one hit inside a snapshot for "find --json".
//
// PATH-BEARING: never pass it to a logger.
type FindMatch struct {
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Permissions string    `json:"permissions"`
	Size        uint64    `json:"size"`
	Mtime       time.Time `json:"mtime"`
	Atime       time.Time `json:"atime"`
	Ctime       time.Time `json:"ctime"`
	UID         uint32    `json:"uid"`
	GID         uint32    `json:"gid"`
	Inode       uint64    `json:"inode"`
}

// FindResult is the hits of "find --json" in one snapshot.
//
// PATH-BEARING (through Matches): never pass it to a logger.
type FindResult struct {
	Hits     int         `json:"hits"`
	Snapshot string      `json:"snapshot"`
	Matches  []FindMatch `json:"matches"`
}

// ForgetReason says why one snapshot is kept: the keep rules that matched it.
type ForgetReason struct {
	Snapshot Snapshot `json:"snapshot"`
	Matches  []string `json:"matches"`
}

// ForgetGroup is one snapshot group of "forget --json": the snapshots it keeps
// and removes. The safety guard (architecture §7.3 step 3) counts Keep and
// Remove.
//
// PATH-BEARING (through Paths and Snapshot): never pass it to a logger.
type ForgetGroup struct {
	Tags    []string       `json:"tags"`
	Host    string         `json:"host"`
	Paths   []string       `json:"paths"`
	Keep    []Snapshot     `json:"keep"`
	Remove  []Snapshot     `json:"remove"`
	Reasons []ForgetReason `json:"reasons"`
}

// VersionInfo is the object "version --json" prints.
type VersionInfo struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	GoOS      string `json:"go_os"`
	GoArch    string `json:"go_arch"`
}

// Config is the repository config "cat config" prints.
type Config struct {
	Version           int    `json:"version"`
	ID                string `json:"id"`
	ChunkerPolynomial string `json:"chunker_polynomial"`
}

// InitResult is the object "init --json" prints. Repository is the repository
// location as restic shows it: treat it like a path and do not log it.
type InitResult struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
}

// DecodeSnapshots reads the array that "snapshots --json" prints.
func DecodeSnapshots(r io.Reader) ([]Snapshot, error) {
	return decodeAs[[]Snapshot](r)
}

// DecodeStats reads the object that "stats --json" prints.
func DecodeStats(r io.Reader) (StatsResult, error) {
	return decodeAs[StatsResult](r)
}

// DecodeFind reads the array that "find --json" prints.
func DecodeFind(r io.Reader) ([]FindResult, error) {
	return decodeAs[[]FindResult](r)
}

// DecodeForget reads the array that "forget --json" prints (null and an empty
// array both give an empty slice).
func DecodeForget(r io.Reader) ([]ForgetGroup, error) {
	return decodeAs[[]ForgetGroup](r)
}

// DecodeVersion reads the object that "version --json" prints.
func DecodeVersion(r io.Reader) (VersionInfo, error) {
	return decodeAs[VersionInfo](r)
}

// DecodeConfig reads the object that "cat config" prints.
func DecodeConfig(r io.Reader) (Config, error) {
	return decodeAs[Config](r)
}

// DecodeInit reads the object that "init --json" prints.
func DecodeInit(r io.Reader) (InitResult, error) {
	return decodeAs[InitResult](r)
}

// decodeDocument reads r (bounded), skips any plain-text lines that precede
// the document (restic prints notices such as `Ignoring "filters"...` on a
// stream a caller may have merged), and decodes the first JSON value that starts
// a line. On failure it returns ErrMalformed and keeps none of the input.
func decodeDocument(r io.Reader, v any) error {
	data, err := io.ReadAll(io.LimitReader(r, maxDocument+1))
	if err != nil {
		return err
	}
	if len(data) > maxDocument {
		return ErrMalformed
	}
	start := documentStart(data)
	if start < 0 {
		return ErrMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(data[start:]))
	if err := dec.Decode(v); err != nil {
		return ErrMalformed
	}
	return nil
}

// documentStart returns the offset of the first line that begins (after
// spaces) with '[', '{' or the literal null, or -1.
func documentStart(data []byte) int {
	for off := 0; off < len(data); {
		end := bytes.IndexByte(data[off:], '\n')
		var line []byte
		next := len(data)
		if end >= 0 {
			line, next = data[off:off+end], off+end+1
		} else {
			line = data[off:]
		}
		trimmed := bytes.TrimLeft(line, " \t\r")
		if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{' || bytes.HasPrefix(trimmed, []byte("null"))) {
			return off + (len(line) - len(trimmed))
		}
		off = next
	}
	return -1
}

// decodeAs decodes one document into a fresh T.
func decodeAs[T any](r io.Reader) (T, error) {
	var v T
	err := decodeDocument(r, &v)
	return v, err
}
