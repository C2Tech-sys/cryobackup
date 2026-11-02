package restic

import "time"

// Message is one decoded line of a restic JSON stream. The concrete types are
// the ones below; use a type switch. The method is unexported so that only
// this package can add a type: the parse boundary (doc.go) stays closed.
type Message interface {
	messageType() string
}

// BackupStatus is a backup progress line. restic's "current_files" list is
// deliberately not a field: it is a list of paths.
type BackupStatus struct {
	SecondsElapsed   uint64  `json:"seconds_elapsed"`
	SecondsRemaining uint64  `json:"seconds_remaining"`
	PercentDone      float64 `json:"percent_done"`
	TotalFiles       uint64  `json:"total_files"`
	FilesDone        uint64  `json:"files_done"`
	TotalBytes       uint64  `json:"total_bytes"`
	BytesDone        uint64  `json:"bytes_done"`
	ErrorCount       uint64  `json:"error_count"`
}

// BackupSummary is the final backup message (and, with the snapshot id absent,
// the statistics block inside Snapshot).
type BackupSummary struct {
	DryRun              bool      `json:"dry_run"`
	FilesNew            int64     `json:"files_new"`
	FilesChanged        int64     `json:"files_changed"`
	FilesUnmodified     int64     `json:"files_unmodified"`
	DirsNew             int64     `json:"dirs_new"`
	DirsChanged         int64     `json:"dirs_changed"`
	DirsUnmodified      int64     `json:"dirs_unmodified"`
	DataBlobs           int64     `json:"data_blobs"`
	TreeBlobs           int64     `json:"tree_blobs"`
	DataAdded           uint64    `json:"data_added"`
	DataAddedPacked     uint64    `json:"data_added_packed"`
	TotalFilesProcessed int64     `json:"total_files_processed"`
	TotalBytesProcessed uint64    `json:"total_bytes_processed"`
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	TotalDuration       float64   `json:"total_duration"`
	// SnapshotID is 8 to 64 lower-case hex digits; the decoder drops anything
	// else. It is empty when --skip-if-unchanged skipped the snapshot and for
	// the statistics block of a listed snapshot.
	SnapshotID string `json:"snapshot_id"`
}

// VerboseStatus is a per-item line printed with -v / -vv. restic's "item" (a
// path) is deliberately not a field. Backup lines carry the sizes and
// duration; restore lines carry Size.
type VerboseStatus struct {
	// Action is one of restic's action words (new, unchanged, modified,
	// scan_finished, restored, updated, deleted), "" when absent and "?" for
	// anything else: the decoder never keeps the raw string.
	Action             string  `json:"action"`
	DataSize           uint64  `json:"data_size"`
	DataSizeInRepo     uint64  `json:"data_size_in_repo"`
	MetadataSize       uint64  `json:"metadata_size"`
	MetadataSizeInRepo uint64  `json:"metadata_size_in_repo"`
	TotalFiles         int64   `json:"total_files"`
	Duration           float64 `json:"duration"`
	// Size is the item size on restore lines.
	Size uint64 `json:"size"`
}

// ErrorEvent is one counted error. It is built from restic's error message,
// which names a file, by classifying the text and the item's extension and then
// forgetting both: the struct has no field that could hold them.
type ErrorEvent struct {
	// During is restic's phase word ("archival", "scan", "restore", ...), or
	// "check" for check errors. Restricted to [a-z0-9_-], at most 32 bytes.
	During string
	// Class is Classify of the message.
	Class ErrorClass
	// Ext is Ext of the item, "" when not allow-listed.
	Ext string
}

// ExitError is restic's final stderr message (engine §1.9): the exit code it is
// about to return and the class of its message. The message text is dropped.
type ExitError struct {
	Code  int
	Class ErrorClass
}

// RestoreStatus is a restore progress line.
type RestoreStatus struct {
	SecondsElapsed uint64  `json:"seconds_elapsed"`
	PercentDone    float64 `json:"percent_done"`
	TotalFiles     uint64  `json:"total_files"`
	FilesRestored  uint64  `json:"files_restored"`
	FilesSkipped   uint64  `json:"files_skipped"`
	FilesDeleted   uint64  `json:"files_deleted"`
	TotalBytes     uint64  `json:"total_bytes"`
	BytesRestored  uint64  `json:"bytes_restored"`
	BytesSkipped   uint64  `json:"bytes_skipped"`
}

// RestoreSummary is the final restore message; it carries the same counters as
// RestoreStatus. restic 0.19.1 prints it even when the run exits 1.
type RestoreSummary struct {
	SecondsElapsed uint64  `json:"seconds_elapsed"`
	PercentDone    float64 `json:"percent_done"`
	TotalFiles     uint64  `json:"total_files"`
	FilesRestored  uint64  `json:"files_restored"`
	FilesSkipped   uint64  `json:"files_skipped"`
	FilesDeleted   uint64  `json:"files_deleted"`
	TotalBytes     uint64  `json:"total_bytes"`
	BytesRestored  uint64  `json:"bytes_restored"`
	BytesSkipped   uint64  `json:"bytes_skipped"`
}

// CheckSummary is the final check message. BrokenPacks is the number of damaged
// packs (restic sends the list of pack ids; only the count is kept).
type CheckSummary struct {
	NumErrors          int
	BrokenPacks        int
	SuggestRepairIndex bool
	SuggestPrune       bool
}

// LsSnapshot is the first line of "ls --json": the snapshot being listed.
//
// PATH-BEARING: Paths holds the backed-up roots. It exists for restore browsing
// and the snapshot cache only; never pass it to a logger.
type LsSnapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
}

// Node is one file or directory line of "ls --json".
//
// PATH-BEARING: Name and Path are file names. They exist for restore browsing
// only; never pass a Node to a logger. Mode is restic's numeric mode (Go
// os.FileMode bits), a number in the JSON, not a string.
type Node struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Path        string    `json:"path"`
	Size        uint64    `json:"size"`
	Mode        uint64    `json:"mode"`
	Permissions string    `json:"permissions"`
	Mtime       time.Time `json:"mtime"`
	Atime       time.Time `json:"atime"`
	Ctime       time.Time `json:"ctime"`
	UID         uint32    `json:"uid"`
	GID         uint32    `json:"gid"`
	Inode       uint64    `json:"inode"`
}

// Unknown is a line with a message_type this package does not know. restic may
// add types at any time (engine §1.9), so this is not an error.
type Unknown struct {
	// Type is the message_type, restricted to [a-z0-9_-] and 64 bytes ("?" if
	// it was anything else).
	Type string
}

// Unparsed is a line that is not a JSON object (plain-text progress, a Go
// warning, a line that was too long or cut off). Only the count of such lines
// is meaningful; the text is dropped. Class is Classify of the text, so a plain
// "Fatal: ..." or "... does not exist, skipping" line still contributes a
// path-free class.
type Unparsed struct {
	Class ErrorClass
}

func (BackupStatus) messageType() string   { return "status" }
func (BackupSummary) messageType() string  { return "summary" }
func (VerboseStatus) messageType() string  { return "verbose_status" }
func (ErrorEvent) messageType() string     { return "error" }
func (ExitError) messageType() string      { return "exit_error" }
func (RestoreStatus) messageType() string  { return "status" }
func (RestoreSummary) messageType() string { return "summary" }
func (CheckSummary) messageType() string   { return "summary" }
func (LsSnapshot) messageType() string     { return "snapshot" }
func (Node) messageType() string           { return "node" }
func (Unknown) messageType() string        { return "unknown" }
func (Unparsed) messageType() string       { return "unparsed" }
