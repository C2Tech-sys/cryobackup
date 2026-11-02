package restic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// maxLine is the longest line the Decoder will parse (16 MiB). restic's
// "current_files" list can push a status line far beyond bufio's 64 KiB
// default. A longer line is drained and reported as Unparsed.
const maxLine = 16 << 20

// MessageType returns the restic message_type word of m ("status", "summary",
// "error", ...), or "unknown" / "unparsed" for the catch-all types.
func MessageType(m Message) string { return m.messageType() }

// Decoder reads one restic JSON stream (stdout or stderr of a --json run) and
// turns every line into a path-free Message. It is the parse boundary of
// architecture §8.4 item 1: the "item" and "message" fields and the
// "current_files" list are consumed here and gone; no method returns a raw line.
type Decoder struct {
	r    *bufio.Reader
	kind Kind
}

// NewDecoder returns a Decoder reading from r. Without ForKind it infers
// backup, restore and check messages from their fields.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReaderSize(r, 64<<10)}
}

// ForKind tells the decoder which command it is reading, which selects the
// struct for the "status" and "summary" message types that backup, restore and
// check share. KindBackup, KindRestore and KindCheck are meaningful; any other
// kind leaves inference on. It returns d for chaining.
func (d *Decoder) ForKind(k Kind) *Decoder {
	d.kind = k
	return d
}

// Next returns the next message, or io.EOF at the end of the stream. A line
// that is not valid JSON, is too long, or whose fields do not fit the expected
// shape becomes Unparsed; a message_type this package does not know becomes
// Unknown. Neither is an error. Next returns a non-EOF error only when the
// underlying reader fails. Blank lines are skipped.
func (d *Decoder) Next() (Message, error) {
	for {
		line, tooLong, err := d.readLine()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err // the reader failed; a partial line is dropped
		}
		if !tooLong && len(bytes.TrimSpace(line)) == 0 {
			if err != nil {
				return nil, err // io.EOF, nothing pending
			}
			continue // blank line
		}
		// A partial last line (no trailing newline) is parsed like any other
		// and, being cut off, usually comes out as Unparsed. The EOF is
		// reported by the following call.
		return d.decodeLine(line, tooLong), nil
	}
}

// readLine reads one line without its terminator. When the stream ends in the
// middle of a line it returns the data together with the error. tooLong is set
// (and line is empty) when the line exceeded maxLine; the rest of it has been
// discarded.
func (d *Decoder) readLine() (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, isPrefix, rerr := d.r.ReadLine()
		if rerr != nil {
			return buf, tooLong, rerr
		}
		if !tooLong {
			if len(buf)+len(chunk) > maxLine {
				tooLong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if !isPrefix {
			return buf, tooLong, nil
		}
	}
}

// probe holds every field the dispatcher looks at. Unmarshalling into it never
// keeps a string that could name a file: "item" and "message" are classified
// and dropped by decodeLine.
type probe struct {
	MessageType string `json:"message_type"`
	StructType  string `json:"struct_type"`

	// error
	Error  *struct{ Message string } `json:"error"`
	During string                    `json:"during"`
	Item   string                    `json:"item"`
	// error (check style) and exit_error
	Message string `json:"message"`
	Code    *int   `json:"code"`

	// kind inference
	FilesRestored *json.RawMessage `json:"files_restored"`
	NumErrors     *json.RawMessage `json:"num_errors"`
	BrokenPacks   *json.RawMessage `json:"broken_packs"`
}

func (d *Decoder) decodeLine(line []byte, tooLong bool) Message {
	if tooLong {
		return Unparsed{Class: ClassUnknown}
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Unparsed{Class: Classify(string(trimmed))}
	}
	var p probe
	if err := json.Unmarshal(trimmed, &p); err != nil {
		// Cut off or malformed: the text may still say what went wrong.
		return Unparsed{Class: Classify(string(trimmed))}
	}
	typ := p.MessageType
	if typ == "" {
		typ = p.StructType // older restic: ls lines carry only struct_type
	}

	switch typ {
	case "status":
		return d.decodeStatus(trimmed, &p)
	case "summary":
		return d.decodeSummary(trimmed, &p)
	case "verbose_status":
		return decodeVerboseStatus(trimmed)
	case "error":
		return d.decodeError(&p)
	case "exit_error":
		code := 0
		if p.Code != nil {
			code = *p.Code
		}
		return ExitError{Code: code, Class: Classify(p.Message)}
	case "snapshot":
		return decodeInto[LsSnapshot](trimmed)
	case "node":
		return decodeInto[Node](trimmed)
	}
	return Unknown{Type: safeWord(typ, 64, "?")}
}

// decodeInto unmarshals into T and returns it as a Message, or Unparsed when the
// fields do not fit.
func decodeInto[T any](line []byte) Message {
	var v T
	if err := json.Unmarshal(line, &v); err != nil {
		return Unparsed{Class: ClassUnknown}
	}
	m, ok := any(v).(Message)
	if !ok {
		return Unparsed{Class: ClassUnknown}
	}
	return m
}

func (d *Decoder) restoreStream(p *probe) bool {
	switch d.kind {
	case KindRestore:
		return true
	case KindBackup, KindCheck:
		return false
	}
	return p.FilesRestored != nil
}

func (d *Decoder) decodeStatus(line []byte, p *probe) Message {
	switch {
	case d.restoreStream(p):
		return decodeInto[RestoreStatus](line)
	case d.kind == KindBackup || d.kind == "":
		return decodeInto[BackupStatus](line)
	}
	return Unknown{Type: "status"}
}

func (d *Decoder) decodeSummary(line []byte, p *probe) Message {
	switch {
	case d.kind == KindCheck || d.kind == "" && (p.NumErrors != nil || p.BrokenPacks != nil):
		return decodeCheckSummary(line)
	case d.restoreStream(p):
		return decodeInto[RestoreSummary](line)
	}
	return decodeBackupSummary(line)
}

// decodeBackupSummary decodes a backup summary and reduces its snapshot id to
// hex (safeSnapshotID); anything else restic put there is dropped.
func decodeBackupSummary(line []byte) Message {
	m := decodeInto[BackupSummary](line)
	if s, ok := m.(BackupSummary); ok {
		s.SnapshotID = safeSnapshotID(s.SnapshotID)
		return s
	}
	return m
}

// decodeVerboseStatus decodes a verbose_status line and reduces its action to
// restic's vocabulary (safeAction). The "item" is not a field and is gone.
func decodeVerboseStatus(line []byte) Message {
	m := decodeInto[VerboseStatus](line)
	if v, ok := m.(VerboseStatus); ok {
		v.Action = safeAction(v.Action)
		return v
	}
	return m
}

func decodeCheckSummary(line []byte) Message {
	var v struct {
		NumErrors          int             `json:"num_errors"`
		BrokenPacks        json.RawMessage `json:"broken_packs"`
		SuggestRepairIndex bool            `json:"suggest_repair_index"`
		SuggestPrune       bool            `json:"suggest_prune"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return Unparsed{Class: ClassUnknown}
	}
	return CheckSummary{
		NumErrors:          v.NumErrors,
		BrokenPacks:        countOf(v.BrokenPacks),
		SuggestRepairIndex: v.SuggestRepairIndex,
		SuggestPrune:       v.SuggestPrune,
	}
}

// countOf reads a JSON array (its length), a JSON number (its value) or null
// (zero). Anything else counts as zero.
func countOf(raw json.RawMessage) int {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	switch raw[0] {
	case '[':
		var a []json.RawMessage
		if json.Unmarshal(raw, &a) == nil {
			return len(a)
		}
	default:
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			return n
		}
	}
	return 0
}

// decodeError turns an error line into an ErrorEvent. backup and restore send
// {"error":{"message":...},"during":...,"item":...}; check sends
// {"message":...} alone. The message and the item are classified and dropped;
// the item's path is taken out of the message first (ClassifyItem).
func (d *Decoder) decodeError(p *probe) Message {
	text := p.Message
	if p.Error != nil && p.Error.Message != "" {
		text = p.Error.Message
	}
	during := safeWord(p.During, 32, "")
	if during == "" && d.kind == KindCheck {
		during = "check"
	}
	return ErrorEvent{During: during, Class: ClassifyItem(text, p.Item), Ext: Ext(p.Item)}
}

// safeWord returns s when it is 1..max bytes of [a-z0-9_-] (after lower-casing
// ASCII), "" for an empty s, and bad for anything else. It keeps restic's small
// vocabulary words (during, message_type) from ever carrying free text.
func safeWord(s string, max int, bad string) string {
	if s == "" {
		return ""
	}
	if len(s) > max {
		return bad
	}
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return bad
		}
	}
	return string(b)
}

// actionWords is restic 0.19.1's vocabulary for the "action" of a verbose_status
// line: backup prints new, unchanged, modified and scan_finished; restore prints
// restored, updated, unchanged and deleted. Anything else is dropped.
var actionWords = map[string]bool{
	"new": true, "unchanged": true, "modified": true, "scan_finished": true,
	"restored": true, "updated": true, "deleted": true,
}

// safeAction keeps s only when it is one of actionWords (exact, lower case, as
// restic prints them), "" for an empty s, and "?" for anything else. The raw
// string is not kept.
func safeAction(s string) string {
	if s == "" || actionWords[s] {
		return s
	}
	return "?"
}

// safeSnapshotID keeps s only when it is 8..64 lower-case hex digits (restic
// prints the full 64-digit id; 8 is the short form) and returns "" otherwise.
func safeSnapshotID(s string) string {
	if len(s) < 8 || len(s) > 64 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}
