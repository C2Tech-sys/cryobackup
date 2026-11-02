package restic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		msg  string
		want ErrorClass
	}{
		// rule 1: wrong password
		{"Fatal: wrong password or no key found", ClassWrongPassword},
		{"FATAL: Wrong Password", ClassWrongPassword},
		{"no key found", ClassWrongPassword},
		// rule 2: repository missing (real fixture text, scrubbed)
		{"Fatal: repository does not exist: unable to open config file: GetFileAttributesEx C:\\fixture\\no-such-repo\\config: The system cannot find the path specified.\nIs there a repository at the following location?\nC:\\fixture\\no-such-repo", ClassRepoMissing},
		{"Is there a repository at the following location?", ClassRepoMissing},
		// rule 3: repository lock
		{"unable to create lock in backend: repository is already locked exclusively by PID 4242 on fixture-host by fixture-user (UID 0, GID 0)", ClassLock},
		{"unable to create lock in backend: repository is already locked by PID 18004 on fixture-host by fixture-user (UID 0, GID 0)\nlock was created at 2026-10-22 12:04:57 (990.3762ms ago)\nstorage ID ce6d1d87\nthe `unlock` command can be used to remove stale locks", ClassLock},
		{"failed to refresh lock in time", ClassLock},
		{"cannot take an exclusive lock: lock held", ClassLock},
		// rule 4: VSS (real text from restic 0.19.1 without admin rights)
		{"VSS error: The caller does not have sufficient backup privileges or is not an administrator: E_ACCESSDENIED (0x455f41434345535344454e494544)", ClassVSS},
		{"failed to create volume shadow copy for C:\\", ClassVSS},
		{"VSS_E_BAD_STATE", ClassVSS},
		// rule 5: TLS
		{"Get \"https://gw.example.com/config\": tls: failed to verify certificate: x509: certificate signed by unknown authority", ClassTLS},
		{"remote error: tls: bad certificate", ClassTLS},
		{"x509: certificate has expired or is not yet valid", ClassTLS},
		{"tls handshake timeout", ClassTLS},
		// rule 6: auth
		{"unexpected HTTP response (401): 401 Unauthorized", ClassAuth},
		{"server response unexpected: 403 Forbidden (403)", ClassAuth},
		{"authentication failed", ClassAuth},
		// rule 7: storage
		{"unexpected HTTP response (500): 500 Internal Server Error", ClassStorage},
		{"server returned 503.", ClassStorage},
		{"HTTP 507 Insufficient Storage", ClassStorage},
		{"502 Bad Gateway", ClassStorage},
		{"504 Gateway Timeout", ClassStorage},
		// rule 8: network
		{"Get \"https://gw.example.com/\": dial tcp 192.0.2.1:443: connectex: No connection could be made because the target machine actively refused it.", ClassNetwork},
		{"dial tcp: lookup gw.example.com: no such host", ClassNetwork},
		{"read tcp 10.0.0.2:5000->192.0.2.1:443: wsarecv: An existing connection was forcibly closed by the remote host.", ClassNetwork},
		{"write tcp 10.0.0.2:5000->192.0.2.1:443: wsasend: An established connection was aborted", ClassNetwork},
		{"read: connection reset by peer", ClassNetwork},
		{"unexpected EOF", ClassNetwork},
		{"Get \"https://gw.example.com/\": net/http: request canceled (Client.Timeout exceeded) i/o timeout", ClassNetwork},
		{"proxyconnect tcp: dial tcp 10.0.0.9:3128: connect: connection refused", ClassNetwork},
		// rule 9: disk
		{"write C:\\fixture\\repo\\data\\ab\\abcd: There is not enough space on the disk.", ClassDisk},
		{"write /tmp/x: no space left on device", ClassDisk},
		{"disk quota exceeded", ClassDisk},
		// rule 10: memory
		{"fatal error: runtime: out of memory", ClassMemory},
		{"cannot allocate memory", ClassMemory},
		// rule 11: file in use (real text from the locked-file fixture, scrubbed)
		{"open \\\\?\\C:\\fixture\\tree\\notes\\locked.docx: The process cannot access the file because it is being used by another process.", ClassLocked},
		{`open C:\fixture\x.docx: The process cannot access the file because it is being used by another process.`, ClassLocked},
		{"The process cannot access the file because another process has locked a portion of the file. lock violation", ClassLocked},
		{"sharing violation", ClassLocked},
		// rule 12: permission
		{"Access is denied.", ClassPermission},
		{"failed to restore timestamp of \"C:\\fixture\\restore\\full\\C\": Access is denied.", ClassPermission},
		{"open /root/x: permission denied", ClassPermission},
		{"A required privilege is not held by the client.", ClassPermission},
		{"operation not permitted", ClassPermission},
		// rule 13: missing source
		{"The system cannot find the path specified.", ClassMissingSource},
		{"The system cannot find the file specified.", ClassMissingSource},
		{`C:\fixture\missing does not exist, skipping`, ClassMissingSource},
		{"lstat /data/x: no such file or directory", ClassMissingSource},
		{"The directory name is invalid. is not a directory", ClassMissingSource},
		// rule 14: parse
		{"invalid character '<' looking for beginning of value", ClassParse},
		{"unexpected end of JSON input", ClassParse},
		{"json: cannot unmarshal string into Go value of type int", ClassParse},
		// rule 15: internal
		{"panic: runtime error: index out of range [3] with length 3", ClassInternal},
		{"goroutine 1 [running]:", ClassInternal},
		{"invalid memory address or nil pointer dereference", ClassInternal},
		// rule 16: unsupported file (the two complete messages restic 0.19.1 can print,
		// internal/fs/node.go nodeFillExtendedStat; the first is the lab evidence from
		// AppData\Local\Microsoft\WindowsApps). restic prints them behind the item
		// path ("<item>: unsupported file type ..."); ClassifyItem strips that, so
		// Classify sees the bare text, which must be the whole message.
		{`unsupported file type "irregular"`, ClassUnsupportedFile},
		{`unsupported file type ""`, ClassUnsupportedFile},
		{`Unsupported File Type "IRREGULAR"`, ClassUnsupportedFile},
		{"unsupported file type \"irregular\"\n", ClassUnsupportedFile},
		{"error: unsupported file type \"irregular\"\n", ClassUnknown},
		// unknown
		{"", ClassUnknown},
		{"Fatal: There were 1 errors", ClassUnknown},
		{"Warning: at least one source file could not be read", ClassUnknown},
		{"cmd.Start: exec: \"rclone\": executable file not found in %PATH%", ClassUnknown},
		{"pack c93e9286 contains 2 errors: [blob b45ae7ba: decrypting blob failed: ciphertext verification failed unexpected pack id]", ClassUnknown},
		{"Aucun accès: accès refusé.", ClassUnknown}, // localized Windows text is not classified (doc.go, Locale)
	}
	for _, tt := range tests {
		name := tt.msg
		if len(name) > 50 {
			name = name[:50]
		}
		t.Run(name, func(t *testing.T) {
			if got := Classify(tt.msg); got != tt.want {
				t.Fatalf("Classify(%q) = %s, want %s", tt.msg, got, tt.want)
			}
		})
	}
	if len(tests) < 30 {
		t.Fatalf("only %d classifier cases", len(tests))
	}
}

// TestClassifyPrecedence pins the order of the table: a message that matches
// several rows gets the earliest.
func TestClassifyPrecedence(t *testing.T) {
	tests := []struct {
		msg  string
		want ErrorClass
	}{
		{"Fatal: wrong password; repository does not exist", ClassWrongPassword},
		{"repository does not exist: connection refused", ClassRepoMissing},
		{"repository is already locked: connection refused", ClassLock},
		{"VSS error: tls: bad certificate", ClassVSS},
		{"x509: certificate invalid: 401", ClassTLS},
		{"401 Unauthorized after 503", ClassAuth},
		{"503 Service Unavailable: dial tcp: connection refused", ClassStorage},
		{"dial tcp: no space left on device", ClassNetwork},
		{"no space left on device: cannot allocate memory", ClassDisk},
		{"out of memory: being used by another process", ClassMemory},
		{"being used by another process: access is denied", ClassLocked},
		{"access is denied: no such file or directory", ClassPermission},
		{"no such file or directory: invalid character", ClassMissingSource},
		{"invalid character: panic", ClassParse},
		// the last row loses to every earlier one that also matches
		{`unsupported file type "irregular": access is denied`, ClassPermission},
		{`unsupported file type "irregular" vss`, ClassVSS},
		{`unsupported file type "irregular": connection refused`, ClassNetwork},
	}
	for _, tt := range tests {
		if got := Classify(tt.msg); got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.msg, got, tt.want)
		}
	}
}

// TestClassifyEveryRuleKeyword gives every keyword of every row of the table a
// case of its own, in upper case and embedded in text, so that a typo in the
// table or an accidental shadowing by an earlier row fails here.
func TestClassifyEveryRuleKeyword(t *testing.T) {
	rules := Rules()
	if len(rules) != 16 {
		t.Fatalf("table has %d rows, want 15 from ticket P1-T1 plus unsupported_file (M1-T3); the 17th is the implicit unknown", len(rules))
	}
	seen := map[ErrorClass]bool{}
	for i, r := range rules {
		seen[r.Class] = true
		for _, kw := range append(append([]string(nil), r.Any...), r.Words...) {
			for _, msg := range []string{kw, "xx " + strings.ToUpper(kw) + " yy", "prefix: " + kw + "."} {
				if got := Classify(msg); got != r.Class {
					t.Errorf("rule %d (%s): Classify(%q) = %s", i+1, r.Class, msg, got)
				}
			}
		}
		for _, e := range r.Exact {
			for _, msg := range []string{e, strings.ToUpper(e), " " + e + "\n"} {
				if got := Classify(msg); got != r.Class {
					t.Errorf("rule %d (%s): Exact: Classify(%q) = %s", i+1, r.Class, msg, got)
				}
			}
			// Exact means exact: the text embedded in more text is not this row.
			for _, msg := range []string{"xx " + e, e + " yy", "prefix: " + e} {
				if got := Classify(msg); got == r.Class {
					t.Errorf("rule %d (%s): Exact matched inside %q", i+1, r.Class, msg)
				}
			}
		}
		if len(r.All) > 0 {
			msg := "a " + strings.Join(r.All, " and ") + " b"
			if got := Classify(msg); got != r.Class {
				t.Errorf("rule %d (%s): All %v: Classify(%q) = %s", i+1, r.Class, r.All, msg, got)
			}
			// Missing one of the pair must not match this row.
			if got := Classify("a " + r.All[0] + " b"); got == r.Class && len(r.Any) == 0 {
				t.Errorf("rule %d matched on half of its All list", i+1)
			}
		}
	}
	for _, c := range AllClasses() {
		switch c {
		case ClassUnknown, ClassGuardRefused:
			if seen[c] {
				t.Errorf("%s must not be producible by the classifier", c)
			}
		default:
			if !seen[c] {
				t.Errorf("class %s has no classifier rule", c)
			}
		}
	}
}

func TestRulesReturnsCopy(t *testing.T) {
	a := Rules()
	a[0].Class = ClassNetwork
	a[0].Any[0] = "zzz"
	if b := Rules(); b[0].Class != ClassWrongPassword || b[0].Any[0] != "wrong password" {
		t.Fatal("Rules exposes the internal table")
	}
}

// TestClassifyTokenBoundaries covers the heuristic that keeps short words (HTTP
// status codes, eof) from matching inside names and numbers.
func TestClassifyTokenBoundaries(t *testing.T) {
	tests := []struct {
		msg  string
		want ErrorClass
	}{
		{`open C:\fixture\500\a.docx: Access is denied.`, ClassPermission},
		{`open C:\fixture\503.txt: Access is denied.`, ClassPermission},
		{`open /srv/401/x: permission denied`, ClassPermission},
		{"open file500.txt: Access is denied.", ClassPermission},
		{"open invoice-403.pdf: permission denied", ClassPermission},
		{"open 1500: Access is denied.", ClassPermission},
		{"read thereof.docx: Access is denied.", ClassPermission},
		{"status=503", ClassStorage},
		{"HTTP/1.1 503 Service", ClassStorage},
		{"(500)", ClassStorage},
		{"got 504.", ClassStorage},
		{"returned 403\n", ClassAuth},
		{"unexpected EOF.", ClassNetwork},
		{"EOF", ClassNetwork},
		{"v1.500 released", ClassUnknown},
	}
	for _, tt := range tests {
		if got := Classify(tt.msg); got != tt.want {
			t.Errorf("Classify(%q) = %s, want %s", tt.msg, got, tt.want)
		}
	}
}

func TestClassifyDoesNotRetainInput(t *testing.T) {
	// Classify is a pure function of its argument: calling it again with other
	// input must not be influenced by earlier calls.
	a := Classify("wrong password")
	_ = Classify("connection refused")
	if b := Classify("wrong password"); a != b || a != ClassWrongPassword {
		t.Fatal("Classify is not pure")
	}
}

func TestExt(t *testing.T) {
	tests := []struct {
		item string
		want string
	}{
		{`C:\fixture\PHI-sentinel\patient-record.docx`, "docx"},
		{`C:\fixture\PHI-sentinel\patient-record.DOCX`, "docx"},
		{"/C/fixture/tree/notes/readme.txt", "txt"},
		{`C:\fixture/mixed\separators/Report.PDF`, "pdf"},
		{"archive.tar.gz", ""},
		{"backup.7z", "7z"},
		{"mail.PST", "pst"},
		{"data.qbw", "qbw"},
		{"drawing.dwg", "dwg"},
		{"logo.ai", "ai"},
		{"photo.heic", "heic"},
		{"song.mp3", "mp3"},
		{"movie.MOV", "mov"},
		{"key.pem", ""},
		{"secret.exe", ""},
		{"noext", ""},
		{"", ""},
		{".docx", ""},
		{`C:\dir.docx\file`, ""},
		{"trailingdot.", ""},
		{"trailing.docx ", "docx"},
		{`C:\fixture\notes\`, ""},
		{"/", ""},
		{"x.docx.exe", ""},
		{"patient-record.docx.bak", "bak"},
	}
	for _, tt := range tests {
		if got := Ext(tt.item); got != tt.want {
			t.Errorf("Ext(%q) = %q, want %q", tt.item, got, tt.want)
		}
	}
	for _, e := range strings.Fields("docx xlsx pptx doc xls ppt pdf txt csv rtf odt ods jpg jpeg png gif heic tif tiff mp4 mov avi mp3 wav zip 7z rar pst ost msg eml html htm xml json log db sqlite bak qbw qbb dwg psd ai") {
		if got := Ext("f." + e); got != e {
			t.Errorf("allow-listed extension %q gave %q", e, got)
		}
	}
	if got := len(allowedExt); got != 44 {
		t.Errorf("allow-list has %d entries, want 44", got)
	}
}

func TestErrorCounts(t *testing.T) {
	c := make(ErrorCounts)
	add := func(during string, class ErrorClass, ext string, n int) {
		for i := 0; i < n; i++ {
			c.Add(ErrorEvent{During: during, Class: class, Ext: ext})
		}
	}
	if got := c.Dominant(); got != ClassUnknown {
		t.Fatalf("empty Dominant = %s, want unknown", got)
	}
	if len(c.Projection()) != 0 || len(c.ByClass()) != 0 {
		t.Fatal("empty counts must project to nothing")
	}
	add("archival", ClassLocked, "docx", 3)
	add("archival", ClassLocked, "pst", 2)
	add("archival", ClassLocked, "", 1)
	add("scan", ClassPermission, "", 4)
	add("archival", ClassMissingSource, "txt", 1)

	if got, want := c.ByClass(), map[ErrorClass]int{ClassLocked: 6, ClassPermission: 4, ClassMissingSource: 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ByClass = %v, want %v", got, want)
	}
	if got := c.Dominant(); got != ClassLocked {
		t.Fatalf("Dominant = %s, want locked", got)
	}
	want := []ErrorProjection{
		{"archival", ClassLocked, "", 1},
		{"archival", ClassLocked, "docx", 3},
		{"archival", ClassLocked, "pst", 2},
		{"archival", ClassMissingSource, "txt", 1},
		{"scan", ClassPermission, "", 4},
	}
	if got := c.Projection(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Projection =\n%v\nwant\n%v", got, want)
	}
	raw, err := json.Marshal(c.Projection()[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"during":"archival","class":"locked","ext":"docx","count":3}` {
		t.Fatalf("wire shape = %s", raw)
	}
}

func TestDominantTieBreaks(t *testing.T) {
	tests := []struct {
		name   string
		counts map[ErrorClass]int
		want   ErrorClass
	}{
		{"hard beats soft on a tie", map[ErrorClass]int{ClassNetwork: 2, ClassDisk: 2}, ClassDisk},
		{"alphabetical among hard", map[ErrorClass]int{ClassPermission: 1, ClassDisk: 1, ClassParse: 1}, ClassDisk},
		{"alphabetical among soft", map[ErrorClass]int{ClassTLS: 1, ClassAuth: 1, ClassNetwork: 1}, ClassAuth},
		{"count beats hardness", map[ErrorClass]int{ClassNetwork: 3, ClassDisk: 2}, ClassNetwork},
		{"single", map[ErrorClass]int{ClassVSS: 1}, ClassVSS},
		{"hard first regardless of map order", map[ErrorClass]int{ClassAuth: 5, ClassInternal: 5, ClassLock: 5, ClassRepoMissing: 5}, ClassInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 20; i++ { // map iteration order is random: repeat
				c := make(ErrorCounts)
				for class, n := range tt.counts {
					for j := 0; j < n; j++ {
						c.Add(ErrorEvent{During: "x", Class: class})
					}
				}
				if got := c.Dominant(); got != tt.want {
					t.Fatalf("Dominant = %s, want %s", got, tt.want)
				}
			}
		})
	}
}

func TestStderrClass(t *testing.T) {
	counts := make(ErrorCounts)
	counts.Add(ErrorEvent{During: "archival", Class: ClassLocked})
	tests := []struct {
		name string
		exit *ExitError
		cs   ErrorCounts
		want ErrorClass
	}{
		{"exit error says something", &ExitError{Code: 12, Class: ClassWrongPassword}, counts, ClassWrongPassword},
		{"exit error unknown falls to counts", &ExitError{Code: 1, Class: ClassUnknown}, counts, ClassLocked},
		{"no exit error", nil, counts, ClassLocked},
		{"nothing at all", nil, nil, ClassUnknown},
	}
	for _, tt := range tests {
		if got := StderrClass(tt.exit, tt.cs); got != tt.want {
			t.Errorf("%s: got %s want %s", tt.name, got, tt.want)
		}
	}
}

// ---- VSS per-volume fallback (matrix r211) ---------------------------------

// TestVSSFallbackVolume reads restic 0.19.1's per-volume fallback message
// (internal/fs/fs_local_vss.go: "failed to create snapshot for [%s]: %s" with
// item = the lower-cased volume plus a separator) and returns the drive letter
// alone, never anything deeper.
func TestVSSFallbackVolume(t *testing.T) {
	tests := []struct {
		name, message, item string
		vol                 string
		ok                  bool
	}{
		{"restic's text and item", `failed to create snapshot for [c:\]: VSS error: GatherWriterMetadata() deadline exceeded`, `c:\`, "C", true},
		{"another volume", `failed to create snapshot for [e:\]: VSS error: Snapshots are not supported for volume e:\`, `e:\`, "E", true},
		{"upper case and a slash", `Failed to create snapshot for [D:/]: x`, `D:/`, "D", true},
		{"no item: the bracket names it", `failed to create snapshot for [f:\]: x`, "", "F", true},
		{"a GUID volume is reported without a letter", `failed to create snapshot for [\\?\Volume{0b2f}\]: x`, `\\?\Volume{0b2f}\`, "", true},
		{"a path item never leaks", `failed to create snapshot for [c:\Users\PHI-sentinel\]: x`, `c:\Users\PHI-sentinel\`, "", true},
		{"a file error is not a fallback", `open c:\Users\x\a.docx: Access is denied.`, `c:\Users\x\a.docx`, "", false},
		{"snapshot deletion is not a fallback", `failed to delete VSS snapshot: x`, `c:\`, "", false},
		{"the privilege failure is fatal, not a fallback", "VSS error: The caller does not have sufficient backup privileges or is not an administrator: E_ACCESSDENIED (0x80070005)", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tt := range tests {
		vol, ok := VSSFallbackVolume(tt.message, tt.item)
		if vol != tt.vol || ok != tt.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tt.name, vol, ok, tt.vol, tt.ok)
		}
		if len(vol) > 1 {
			t.Errorf("%s: %q is more than a drive letter", tt.name, vol)
		}
	}
	// The fallback text classifies as vss through the ordinary table.
	if c := Classify(`failed to create snapshot for [c:\]: VSS error: GatherWriterMetadata() deadline exceeded`); c != ClassVSS {
		t.Errorf("Classify(fallback) = %s, want vss", c)
	}
}

// TestClassifyItem: the item's path is cut out of the message before the table
// runs; a short item (a drive root, a POSIX root) is only stripped as a prefix
// so it cannot remove ordinary words, and the VSS fallback line keeps its class.
func TestClassifyItem(t *testing.T) {
	tests := []struct {
		message, item string
		want          ErrorClass
	}{
		{`C:\proxy\x.exe: unsupported file type "irregular"`, `C:\proxy\x.exe`, ClassUnsupportedFile},
		{`open C:\vss\a.txt: Access is denied.`, `C:\vss\a.txt`, ClassPermission},
		// No item: the path is still taken out by its shape (M1-FIX2); before,
		// "proxy" in the path decided network here.
		{`C:\proxy\x.exe: unsupported file type "irregular"`, "", ClassUnsupportedFile},
		{`failed to create snapshot for [c:\]: VSS error: deadline exceeded`, `c:\`, ClassVSS},
		{`/: read /: i/o timeout`, `/`, ClassNetwork},
		{`c:\: i/o timeout`, `c:\`, ClassNetwork},
	}
	for _, tt := range tests {
		if got := ClassifyItem(tt.message, tt.item); got != tt.want {
			t.Errorf("ClassifyItem(%q, %q) = %s, want %s", tt.message, tt.item, got, tt.want)
		}
	}
}

// TestDocListsExactMessages keeps doc.go in step with the Exact rows of the
// table (doc_test.go checks Any, Words and All).
func TestDocListsExactMessages(t *testing.T) {
	raw, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ToLower(string(raw))
	n := 0
	for i, r := range Rules() {
		for _, e := range r.Exact {
			n++
			if !strings.Contains(doc, strings.ToLower(e)) {
				t.Errorf("rule %d (%s): doc.go does not list the exact message %q", i+1, r.Class, e)
			}
		}
	}
	if n == 0 {
		t.Fatal("no Exact rows: the unsupported_file row lost its anchor")
	}
}

// ---- no path text decides the class (M1-FIX2) ------------------------------

// TestClassifyItemVSSAndRestorePaths pins the shapes restic 0.19.1 prints under
// --use-fs-snapshot, where an OS error names the shadow-copy path rather than
// the item, and for restore, where messages name the restore target. Each path
// is full of words an earlier row reacts to (proxy, vss, certificate, tls,
// 500, shadow copy), and none of them may decide the class.
func TestClassifyItemVSSAndRestorePaths(t *testing.T) {
	const shadow = `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy3`
	tests := []struct {
		name, message, item string
		want                ErrorClass
	}{
		// The verifier's probe: this was network.
		{"verifier probe", `C:\Data\proxy\x: open ` + shadow + `\Data\proxy\x: Access is denied.`, `C:\Data\proxy\x`, ClassPermission},
		{"vss in the shadow path", `C:\Data\vss\x: open ` + shadow + `\Data\vss\x: Access is denied.`, `C:\Data\vss\x`, ClassPermission},
		{"no item given", `C:\Data\proxy\x: open ` + shadow + `\Data\proxy\x: Access is denied.`, ``, ClassPermission},
		{"item is not in the text", `open ` + shadow + `\Data\proxy\x: Access is denied.`, `C:\Data\proxy\x`, ClassPermission},
		{"spaces in the shadow path", `open ` + shadow + `\Data\Program Files\proxy dir\vss file.docx: Access is denied.`, ``, ClassPermission},
		{"locked file", `open ` + shadow + `\Data\certificate\a.xlsx: The process cannot access the file because it is being used by another process.`, ``, ClassLocked},
		{"missing path", `open ` + shadow + `\Data\proxy\500: The system cannot find the path specified.`, ``, ClassMissingSource},
		{"incomplete metadata", `incomplete metadata for C:\Data\vss\x.docx: open ` + shadow + `\Data\vss\x.docx: Access is denied.`, `C:\Data\vss\x.docx`, ClassPermission},
		{"readdirnames", `readdirnames C:\Data\proxy failed: open ` + shadow + `\Data\proxy: Access is denied.`, `C:\Data\proxy`, ClassPermission},
		{"device path", `open \\.\C:\proxy\a: Access is denied.`, ``, ClassPermission},
		{"unc path", `open \\proxy-host\vss$\certificate\a.txt: Access is denied.`, ``, ClassPermission},
		{"extended path", `open \\?\C:\proxy\a.txt: Access is denied.`, ``, ClassPermission},
		{"unc extended path", `open \\?\UNC\proxy\vss\a.txt: Access is denied.`, ``, ClassPermission},
		{"forward slashes", `open C:/proxy/vss/a.txt: Access is denied.`, ``, ClassPermission},
		{"posix path", `open /srv/proxy/vss/a.txt: permission denied`, ``, ClassPermission},
		{"two paths", `rename C:\proxy\a C:\vss\b: Access is denied.`, ``, ClassPermission},
		{"path last", `path is not a directory: C:\proxy\vss`, ``, ClassMissingSource},
		{"after a bracket", `lstat [C:\proxy\a]: Access is denied.`, ``, ClassPermission},
		// restore: the message names the target, the item is the snapshot path.
		{"restore timestamp", `failed to restore timestamp of "C:\\Restore\\proxy\\C\\Data\\x": Access is denied.`, `\C\Data\x`, ClassPermission},
		{"restore timestamp, spaces", `failed to restore timestamp of "D:\\restore vss\\certificate\\a.docx": Access is denied.`, `\a.docx`, ClassPermission},
		{"restore write", `open \\?\C:\Restore\proxy\a.docx: There is not enough space on the disk.`, `\a.docx`, ClassDisk},
		{"restore into a tls directory", `open \\?\C:\Restore\tls x509\a.docx: The process cannot access the file because it is being used by another process.`, `\a.docx`, ClassLocked},
		{"restore target on a share", `failed to restore timestamp of "\\\\proxy\\vss$\\a": Access is denied.`, `\a`, ClassPermission},
		// A path is not a verdict, and removing it does not invent one.
		{"path only", `open C:\proxy\vss: `, ``, ClassUnknown},
		{"directory changed type", `directory "C:\\Data\\proxy" changed type, refusing to archive`, `C:\Data\proxy`, ClassUnknown},
		// What a path cannot do, the message still can: real words outside the path count.
		{"real vss error beside a path", `C:\Data\x: VSS error: snapshot failed`, `C:\Data\x`, ClassVSS},
		{"real network error beside a path", `Get "https://gw.example.com/": dial tcp: lookup gw.example.com: no such host`, ``, ClassNetwork},
		{"url is not a path", `Get "https://gw.example.com/x": 503 Service Unavailable`, ``, ClassStorage},
		{"i/o is not a path", `read C:\proxy\x: i/o timeout`, ``, ClassNetwork},
	}
	for _, tt := range tests {
		if got := ClassifyItem(tt.message, tt.item); got != tt.want {
			t.Errorf("%s: ClassifyItem(%q, %q) = %s, want %s", tt.name, tt.message, tt.item, got, tt.want)
		}
	}
}

// TestClassifyItemPathWordsNeverDecide puts every keyword of every row of the
// table into a path, in every path shape the classifier knows, in front of an
// error whose class is known, and requires that class. A path word that
// reaches the table fails here as the class of the keyword's row.
func TestClassifyItemPathWordsNeverDecide(t *testing.T) {
	osErrors := []struct {
		text string
		want ErrorClass
	}{
		{"Access is denied.", ClassPermission},
		{"The process cannot access the file because it is being used by another process.", ClassLocked},
		{"The system cannot find the path specified.", ClassMissingSource},
		{"There is not enough space on the disk.", ClassDisk},
	}
	shapes := []string{
		`open C:\Data\%s\x.txt: `,
		`open c:/data/%s/x.txt: `,
		`open \\?\C:\Data\%s\x.txt: `,
		`open \\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy7\Data\%s\x.txt: `,
		`open \\.\C:\%s: `,
		`open \\host\share\%s\x: `,
		`open /srv/%s/x.txt: `,
		`failed to restore timestamp of "C:\\Restore\\%s\\x.txt": `,
		`C:\Data\%s\x.txt: open \\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy7\Data\%s\x.txt: `,
		`incomplete metadata for D:\Data\%s dir\x.txt: open \\?\D:\Data\%s dir\x.txt: `,
	}
	var words []string
	for _, r := range Rules() {
		words = append(words, r.Any...)
		words = append(words, r.Words...)
		words = append(words, r.All...)
		for _, e := range r.Exact {
			words = append(words, strings.ReplaceAll(e, `"`, ""))
		}
	}
	n := 0
	for _, w := range words {
		if strings.Contains(w, ": ") {
			// "runtime: out of memory" cannot be a Windows file name (a colon is
			// reserved) and ": " is where a path token ends; the limit is
			// documented on ClassifyItem. Its other form, "out of memory", runs.
			continue
		}
		for _, shape := range shapes {
			path := strings.ReplaceAll(shape, "%s", w)
			for _, e := range osErrors {
				n++
				if got := ClassifyItem(path+e.text, ""); got != e.want {
					t.Errorf("ClassifyItem(%q, \"\") = %s, want %s", path+e.text, got, e.want)
				}
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d cases ran", n)
	}
}

// TestClassifyItemKeepsRealFixtureClasses: removing paths changes no class that
// restic 0.19.1 itself produced. Every error line of every captured fixture
// classifies the same with and without withoutPaths.
func TestClassifyItemKeepsRealFixtureClasses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixtureDir, "*.stderr.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no stderr fixtures: %v", err)
	}
	lines := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			var p struct {
				Type  string `json:"message_type"`
				Msg   string `json:"message"`
				Item  string `json:"item"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(line), &p) != nil || p.Type != "error" {
				continue
			}
			text := p.Msg
			if p.Error.Message != "" {
				text = p.Error.Message
			}
			lines++
			before := Classify(withoutItem(text, p.Item))
			if got := ClassifyItem(text, p.Item); got != before {
				t.Errorf("%s: %q classifies %s, was %s before paths were removed", filepath.Base(f), text, got, before)
			}
		}
	}
	if lines < 3 {
		t.Fatalf("only %d real error lines compared", lines)
	}
}

// TestDecodeVSSAndRestorePathFixtures decodes the hand-written lines (shapes
// read from restic 0.19.1's source, see withoutPaths) through the Decoder.
func TestDecodeVSSAndRestorePathFixtures(t *testing.T) {
	tests := []struct {
		file string
		kind Kind
		want []ErrorEvent
	}{
		{"vss_paths.stderr.jsonl", KindBackup, []ErrorEvent{
			{During: "archival", Class: ClassPermission},
			{During: "archival", Class: ClassLocked, Ext: "xlsx"},
			{During: "archival", Class: ClassPermission, Ext: "docx"},
			{During: "archival", Class: ClassMissingSource},
			{During: "archival", Class: ClassUnsupportedFile},
			{During: "archival", Class: ClassUnknown},
		}},
		{"restore_paths.stderr.jsonl", KindRestore, []ErrorEvent{
			{During: "restore", Class: ClassPermission},
			{During: "restore", Class: ClassPermission, Ext: "docx"},
			{During: "restore", Class: ClassDisk, Ext: "docx"},
			{During: "restore", Class: ClassLocked, Ext: "docx"},
		}},
	}
	for _, tt := range tests {
		raw, err := os.ReadFile(filepath.Join("testdata", "handwritten", tt.file))
		if err != nil {
			t.Fatal(err)
		}
		msgs := decodeAll(t, raw, tt.kind)
		requireNone[Unparsed](t, msgs)
		requireNone[Unknown](t, msgs)
		if got := only[ErrorEvent](msgs); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: events = %+v, want %+v", tt.file, got, tt.want)
		}
	}
}

// TestWithoutPathsRemovesNothingElse: text without a path is returned as it is,
// and a path is replaced by one space (its closing ": " goes with it).
func TestWithoutPathsRemovesNothingElse(t *testing.T) {
	for _, s := range []string{
		"", "Access is denied.", "i/o timeout", "HTTP/1.1 503 Service", "a / b", "/", "tcp:/x", "a:b", "x:",
		`Get "https://gw.example.com/": EOF`, "read tcp 10.0.0.2:5000->192.0.2.1:443: wsarecv: reset", "Aucun accès: accès refusé.",
	} {
		if got := withoutPaths(s); got != s {
			t.Errorf("withoutPaths(%q) = %q", s, got)
		}
	}
	for in, want := range map[string]string{
		`open C:\a b\c: denied`:    "open  denied",
		`x "C:\\a" y`:              `x " " y`,
		"p C:\\a\nnext line":       "p  \nnext line",
		`\\?\C:\a: unsupported`:    " unsupported",
		`ok \\?\C:\a`:              "ok  ",
		`z: C:\a: C:\b: end`:       "z:   end",
		`d:/x/y: Access is denied`: " Access is denied",
	} {
		if got := withoutPaths(in); got != want {
			t.Errorf("withoutPaths(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- no bare name decides the class (M1-FIX3) -------------------------------

// TestClassifyItemQuotedNames: restic check and the restorer name a file by its
// bare name, %q-quoted or after "invalid child node name ", not by an absolute
// path (restic 0.19.1 internal/checker/checker.go, internal/restorer/
// restorer.go). Before M1-FIX3 the name decided: the first three cases were
// network, tls and vss.
func TestClassifyItemQuotedNames(t *testing.T) {
	tests := []struct {
		name, message, item string
		want                ErrorClass
	}{
		{"blob not in index", `file "proxy notes.docx" blob 0 not found in index`, ``, ClassUnknown},
		{"no subtree", `dir node "certificate" has no subtree`, ``, ClassUnknown},
		{"nil blob list", `file "vss.log" has nil blob list`, ``, ClassUnknown},
		{"escaped quote in the name", `file ` + strconv.Quote(`say "proxy" twice`) + ` blob 1 not found in index`, ``, ClassUnknown},
		{"colon space in the name", `file "x509 tls: 503.xlsx" blob 2 not found in index`, ``, ClassUnknown},
		{"wrapped by the tree error", "error for tree 5f3a1b2c:\n  file \"proxy\" blob 0 not found in index\n", ``, ClassUnknown},
		{"unclosed quote", `file "proxy notes.docx blob 0 not found in index`, ``, ClassUnknown},
		{"restorer bare name", `invalid child node name ../proxy`, `/data`, ClassUnknown},
		{"restorer bare name with separators", `invalid child node name vss\certificate.docx`, `/data`, ClassUnknown},
		// The words outside the quotes still decide.
		{"real error after a name", `file "proxy" blob 0: Access is denied.`, ``, ClassPermission},
		{"quoted url, network", `Get "https://gw.example.com/certificate/vss": dial tcp: lookup gw.example.com: no such host`, ``, ClassNetwork},
		{"quoted url, proxy", `Post "https://gw.example.com/": proxyconnect tcp: dial tcp 192.0.2.1:8080: connectex: connection refused`, ``, ClassNetwork},
		{"quoted url, tls", `Get "https://gw.example.com/proxy": tls: failed to verify certificate: x509: certificate signed by unknown authority`, ``, ClassTLS},
		{"quoted url, storage", `Get "https://gw.example.com/proxy/": 503 Service Unavailable`, ``, ClassStorage},
		{"quoted url, eof", `Get "https://gw.example.com/vss": EOF`, ``, ClassNetwork},
		{"single-quoted rune is not a name", `invalid character '"' after object key:value pair`, ``, ClassParse},
		// The Exact row still sees restic's quoted node type.
		{"unsupported irregular", `C:\Data\proxy\alias.exe: unsupported file type "irregular"`, `C:\Data\proxy\alias.exe`, ClassUnsupportedFile},
		{"unsupported empty", `unsupported file type ""`, ``, ClassUnsupportedFile},
		{"a name is not a node type", `unsupported file type "proxy"`, ``, ClassUnknown},
	}
	for _, tt := range tests {
		if got := ClassifyItem(tt.message, tt.item); got != tt.want {
			t.Errorf("%s: ClassifyItem(%q, %q) = %s, want %s", tt.name, tt.message, tt.item, got, tt.want)
		}
	}
}

// TestClassifyItemNameWordsNeverDecide puts every keyword of every row into a
// bare name, in every shape restic 0.19.1 prints one, and requires unknown: no
// rule matches the rest of these messages, so any other class came from the
// name.
func TestClassifyItemNameWordsNeverDecide(t *testing.T) {
	shapes := []func(name string) string{
		func(n string) string { return "file " + strconv.Quote(n) + " blob 0 not found in index" },
		func(n string) string { return "dir node " + strconv.Quote(n) + " has no subtree" },
		func(n string) string { return "file " + strconv.Quote(n) + " has nil blob list" },
		func(n string) string {
			return "error for tree 5f3a1b2c:\n  file " + strconv.Quote(n) + " has nil blob list\n"
		},
		func(n string) string { return "invalid child node name " + n },
	}
	n := 0
	for _, r := range Rules() {
		words := append(append(append([]string(nil), r.Any...), r.Words...), r.All...)
		words = append(words, r.Exact...)
		for _, w := range words {
			for _, shape := range shapes {
				for _, name := range []string{w, "a " + w + " b.docx", strings.ToUpper(w)} {
					n++
					msg := shape(name)
					if got := ClassifyItem(msg, ""); got != ClassUnknown {
						t.Errorf("ClassifyItem(%q, \"\") = %s, want unknown", msg, got)
					}
				}
			}
		}
	}
	if n < 500 {
		t.Fatalf("only %d cases ran", n)
	}
}

// TestDecodeNameFixtures decodes the hand-written check and restore lines whose
// names are made of classifier keywords (shapes from restic 0.19.1's source).
func TestDecodeNameFixtures(t *testing.T) {
	tests := []struct {
		file string
		kind Kind
		want []ErrorEvent
	}{
		{"check_names.stderr.jsonl", KindCheck, []ErrorEvent{
			{During: "check", Class: ClassUnknown},
			{During: "check", Class: ClassUnknown},
			{During: "check", Class: ClassUnknown},
			{During: "check", Class: ClassUnknown},
			{During: "check", Class: ClassUnknown},
		}},
		{"restore_names.stderr.jsonl", KindRestore, []ErrorEvent{
			{During: "restore", Class: ClassUnknown},
			{During: "restore", Class: ClassUnknown},
		}},
	}
	for _, tt := range tests {
		raw, err := os.ReadFile(filepath.Join("testdata", "handwritten", tt.file))
		if err != nil {
			t.Fatal(err)
		}
		msgs := decodeAll(t, raw, tt.kind)
		requireNone[Unparsed](t, msgs)
		requireNone[Unknown](t, msgs)
		if got := only[ErrorEvent](msgs); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: events = %+v, want %+v", tt.file, got, tt.want)
		}
	}
}

// TestWithoutNames pins what the name step removes and what it leaves.
func TestWithoutNames(t *testing.T) {
	for _, s := range []string{"", "access is denied.", "i/o timeout", "it's 5", `invalid character '"' after`, "invalid child node name"} {
		if got := withoutNames(s); got != s {
			t.Errorf("withoutNames(%q) = %q", s, got)
		}
	}
	for in, want := range map[string]string{
		`file "a b" blob 0`:                  "file   blob 0",
		`file "a\"b" blob`:                   "file   blob",
		`x "a\\" y "b" z`:                    "x   y   z",
		`""`:                                 " ",
		"open \"a\nnext":                     "open  \nnext",
		"invalid child node name ../a\nnext": "invalid child node name  \nnext",
		`get "https://h/": eof`:              "get  : eof",
		"invalid child node name a; invalid child node name b": "invalid child node name  ",
	} {
		if got := withoutNames(in); got != want {
			t.Errorf("withoutNames(%q) = %q, want %q", in, got, want)
		}
	}
}
