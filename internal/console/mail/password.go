package mail

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"runtime"
)

// maxPasswordFile bounds what is read from the password file. A password is a
// line; a file this large is not one.
const maxPasswordFile = 4096

// ReadPasswordFile reads the relay password from the file smtp.password_file
// names: one line, the trailing newline (LF or CRLF) trimmed and nothing else.
// It is called at every send, so a rotated password needs no restart, and by
// the doctor check.
//
// On Unix a file readable by group or others is refused (class
// password_file_mode); on Windows there is no mode to check and the ACL is the
// operator's. The file must be a regular file, non-empty and one line. The
// error is a *SendError at StageConfig: the path and the contents never appear
// in it.
func ReadPasswordFile(path string) (string, error) {
	if path == "" {
		return "", newErr(StageConfig, ClassPasswordFile, false)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", newErr(StageConfig, ClassPasswordFile, false)
	}
	defer f.Close()
	// Stat the open handle, not the path: the file checked is the file read.
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", newErr(StageConfig, ClassPasswordFile, false)
	}
	if runtime.GOOS != "windows" && modeTooOpen(st.Mode()) {
		return "", newErr(StageConfig, ClassPasswordFileMode, false)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPasswordFile+1))
	if err != nil || len(raw) > maxPasswordFile {
		return "", newErr(StageConfig, ClassPasswordFile, false)
	}
	if bytes.HasSuffix(raw, []byte("\r\n")) {
		raw = raw[:len(raw)-2]
	} else {
		raw = bytes.TrimSuffix(raw, []byte("\n"))
	}
	if len(raw) == 0 || bytes.ContainsAny(raw, "\r\n\x00") {
		return "", newErr(StageConfig, ClassPasswordFile, false)
	}
	return string(raw), nil
}

// modeTooOpen reports a file mode that lets group or others read, write or run
// the file. Only owner bits are acceptable for a secret.
func modeTooOpen(m fs.FileMode) bool { return m.Perm()&0o077 != 0 }
