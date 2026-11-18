package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"time"
)

// Kind is the one-word purpose of a message. It rides in the X-CryoBackup-Kind
// header (so a mail rule can sort the console's mail) and in log lines.
type Kind string

// The kinds M4 sends.
const (
	KindAlert    Kind = "alert"
	KindRecovery Kind = "recovery"
	KindDigest   Kind = "digest"
	KindTest     Kind = "test"
)

// ErrInvalidMessage is wrapped by every error Build and Validate return. The
// text after it names the field, never the value: values are addresses and
// customer names.
var ErrInvalidMessage = errors.New("mail: invalid message")

// maxSubject bounds a subject in bytes. A longer one is an error, not a silent
// cut: a template that builds a thousand-byte subject is wrong.
const maxSubject = 500

// Message is one e-mail before it is rendered. From may be empty when the
// sender has a default (Settings.From). To needs one address or more; each is
// "user@host" or "Name <user@host>". Text is required (a message with only HTML
// is unreadable in a plain client and scores badly with filters); HTML is
// optional, and without it the message is a single text/plain part.
type Message struct {
	From    string
	To      []string
	Subject string
	Text    string
	HTML    string
	Kind    Kind
}

// addrs is a message's parsed addresses.
type addrs struct {
	from *netmail.Address
	to   []*netmail.Address
}

// Validate reports whether the message can be built. It checks every header
// value for a line break or other control character, parses the addresses
// (ASCII only: the sender does not negotiate SMTPUTF8) and requires a
// recipient and a text body.
func (m Message) Validate() error {
	_, err := m.parse()
	return err
}

func (m Message) parse() (addrs, error) {
	var a addrs
	bad := func(format string, v ...any) (addrs, error) {
		return addrs{}, fmt.Errorf("%w: "+format, append([]any{ErrInvalidMessage}, v...)...)
	}
	if err := headerSafe("from", m.From); err != nil {
		return bad("%v", err)
	}
	if err := headerSafe("subject", m.Subject); err != nil {
		return bad("%v", err)
	}
	if err := headerSafe("kind", string(m.Kind)); err != nil {
		return bad("%v", err)
	}
	for _, t := range m.To {
		if err := headerSafe("to", t); err != nil {
			return bad("%v", err)
		}
	}
	if len(m.Subject) > maxSubject {
		return bad("subject is longer than %d bytes", maxSubject)
	}
	if m.Kind != "" && !kindOK(string(m.Kind)) {
		return bad("kind must be lower-case letters, digits, hyphen or underscore")
	}
	if strings.TrimSpace(m.From) == "" {
		return bad("from is empty")
	}
	f, err := parseAddr(m.From)
	if err != nil {
		return bad("from is not an e-mail address")
	}
	a.from = f
	if len(m.To) == 0 {
		return bad("no recipient")
	}
	for _, t := range m.To {
		p, err := parseAddr(t)
		if err != nil {
			return bad("a recipient is not an e-mail address")
		}
		a.to = append(a.to, p)
	}
	if strings.TrimSpace(m.Text) == "" {
		return bad("text body is empty")
	}
	return a, nil
}

func kindOK(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// headerSafe refuses a CR, LF, NUL or any other control character (a tab is
// fine) in a header value: the one way to inject a header or a recipient.
func headerSafe(field, s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("%s contains a line break or control character", field)
		}
	}
	return nil
}

func parseAddr(s string) (*netmail.Address, error) {
	a, err := netmail.ParseAddress(s)
	if err != nil {
		return nil, err
	}
	if !isASCII(a.Address) {
		return nil, errors.New("non-ASCII address")
	}
	return a, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Build renders the message as RFC 5322 text with CRLF line ends, ready for the
// DATA command. now becomes the Date header (always in UTC). Headers: Date,
// From, To, Subject (an encoded word when it is not ASCII), Message-ID under the
// From domain, MIME-Version, Content-Type, Auto-Submitted: auto-generated and,
// when Kind is set, X-CryoBackup-Kind. There is no Bcc and no Cc: every
// recipient is in To. The body is multipart/alternative with the text part
// first and the HTML part second, both UTF-8 and quoted-printable; without HTML
// it is one text/plain part. Nothing in the output loads remote content unless
// the caller's own HTML does.
func (m Message) Build(now time.Time) ([]byte, error) {
	a, err := m.parse()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	h := func(name, value string) { b.WriteString(name + ": " + value + "\r\n") }

	h("Date", now.UTC().Format(time.RFC1123Z))
	h("From", a.from.String())
	var to []string
	for _, t := range a.to {
		to = append(to, t.String())
	}
	h("To", strings.Join(to, ",\r\n "))
	b.WriteString(subjectHeader(m.Subject))
	h("Message-ID", messageID(a.from.Address))
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")
	if m.Kind != "" {
		h("X-CryoBackup-Kind", string(m.Kind))
	}

	if strings.TrimSpace(m.HTML) == "" {
		h("Content-Type", `text/plain; charset="utf-8"`)
		h("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := writeQP(&b, m.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct{ typ, text string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.typ + `; charset="utf-8"`},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQP(w, part.text); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	h("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.Bytes(), nil
}

// writeQP writes s quoted-printable with CRLF line ends. The output always ends
// with a line end (one is added when s has none).
func writeQP(w io.Writer, s string) error {
	var buf bytes.Buffer
	qw := quotedprintable.NewWriter(&buf)
	if _, err := qw.Write([]byte(s)); err != nil {
		return err
	}
	if err := qw.Close(); err != nil {
		return err
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\r\n")) {
		buf.WriteString("\r\n")
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// subjectHeader returns the folded Subject header line(s) with their CRLF.
// A non-ASCII subject becomes encoded words (RFC 2047, Q encoding, UTF-8).
func subjectHeader(s string) string {
	if !isASCII(s) {
		return "Subject: " + mime.QEncoding.Encode("utf-8", s) + "\r\n"
	}
	return foldASCII("Subject: ", s)
}

// foldASCII writes name+value folding at spaces so no line passes 76 columns
// (a word longer than that stays whole; the 998-octet limit is enforced by
// maxSubject).
func foldASCII(name, value string) string {
	var out strings.Builder
	out.WriteString(name)
	col := len(name)
	for i, w := range strings.Split(value, " ") {
		if i > 0 {
			if col+1+len(w) > 76 && col > 1 {
				out.WriteString("\r\n ")
				col = 1
			} else {
				out.WriteString(" ")
				col++
			}
		}
		out.WriteString(w)
		col += len(w)
	}
	out.WriteString("\r\n")
	return out.String()
}

// messageID is <random@domain> with the domain of the From address.
func messageID(addr string) string {
	domain := addr[strings.LastIndex(addr, "@")+1:]
	var r [16]byte
	_, _ = rand.Read(r[:])
	return "<" + hex.EncodeToString(r[:]) + "@" + domain + ">"
}
