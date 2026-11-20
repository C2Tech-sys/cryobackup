package mail

import (
	"bufio"
	"errors"
	"net/smtp"
)

// RFC 5321 4.2.1 makes the text of a reply optional: "250" alone is a complete
// reply. net/textproto, which net/smtp reads replies with, requires a fourth
// character (a space or a hyphen) and handles a bare code in two ways, both
// wrong for SMTP:
//
//   - As a whole reply it is a ProtocolError ("short response"), whose text
//     differs between Go versions. bareCode reads the code back from it; that is
//     the fallback for the two replies the filter below cannot reach.
//   - As the last line of a multi-line reply ("250-queued" then "250") it is
//     taken for a line of free text, and the read waits for a final line that
//     never comes: the send hangs until its deadline and ends as a temporary
//     timeout, so the console retries a message the relay had already accepted.
//
// replyLines removes both at the source: it sits under net/textproto's reader and
// gives a line that is nothing but three digits the space textproto requires
// ("250" CRLF becomes "250 " CRLF). Nothing else is changed, and no reply is
// parsed here: net/smtp remains the only SMTP implementation.
//
// It is installed on the client's own reader (smtp.Client.Text.R, an exported
// field), after the greeting and again after STARTTLS, which replaces the reader.
// It cannot be put under the connection: a STARTTLS layer would sit above it and
// it would see cipher text. So the greeting (read inside smtp.NewClient) and the
// EHLO reply that net/smtp reads inside StartTLS are not filtered; a bare code
// there still reaches bareCode, and a multi-line one with a bare last line still
// waits for the deadline.

// filterReplies installs replyLines on c's reader. Call it whenever net/smtp has
// made a new reader: after smtp.NewClient and after StartTLS.
func filterReplies(c *smtp.Client) {
	c.Text.R = bufio.NewReader(newReplyLines(c.Text.R))
}

// replyLines passes the lines of r through, padding a bare three-digit code.
type replyLines struct {
	r       *bufio.Reader
	pending []byte // the rest of the current line, not yet read by the caller
	err     error  // the error that ended the current line, returned once pending is empty
	midLine bool   // the last piece read did not end a line (it filled r's buffer)
}

func newReplyLines(r *bufio.Reader) *replyLines { return &replyLines{r: r} }

func (l *replyLines) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(l.pending) == 0 {
		if l.err != nil {
			err := l.err
			l.err = nil
			return 0, err
		}
		line, err := l.r.ReadSlice('\n')
		startOfLine := !l.midLine
		l.midLine = errors.Is(err, bufio.ErrBufferFull)
		if l.midLine {
			err = nil
		}
		if startOfLine {
			line = padBareCode(line)
		}
		// ReadSlice's slice is only valid until the next read of r, and the
		// caller may take it in several reads: keep a copy.
		l.pending = append(l.pending[:0], line...)
		if len(l.pending) == 0 {
			return 0, err
		}
		l.err = err
	}
	n := copy(p, l.pending)
	l.pending = l.pending[n:]
	return n, nil
}

// padBareCode returns line with a space after the code when line is exactly
// three ASCII digits and an end of line (CRLF or LF); otherwise line itself.
func padBareCode(line []byte) []byte {
	eolOK := len(line) == 5 && line[3] == '\r' && line[4] == '\n' ||
		len(line) == 4 && line[3] == '\n'
	if !eolOK || !isDigit(line[0]) || !isDigit(line[1]) || !isDigit(line[2]) {
		return line
	}
	out := make([]byte, 0, len(line)+1)
	out = append(out, line[:3]...)
	out = append(out, ' ')
	return append(out, line[3:]...)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
