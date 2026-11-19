package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"
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

// maxDisplayName bounds the display name of an address, in characters. Longer is
// an error, for the same reason as maxSubject.
const maxDisplayName = 128

// Limits of a plain envelope address (RFC 5321 4.5.3.1 and RFC 1035).
const (
	maxAddress = 254
	maxLocal   = 64
	maxLabel   = 63
)

// Message is one e-mail before it is rendered. From may be empty when the
// sender has a default (Settings.From). To needs one address or more; each is
// "user@host" or "Name <user@host>", where user@host is a plain address (see
// checkEnvelopeAddr: no quoted local part, no address literal). Text is required (a message with only HTML
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
	if utf8.RuneCountInString(f.Name) > maxDisplayName {
		return bad("the from display name is longer than %d characters", maxDisplayName)
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
		if utf8.RuneCountInString(p.Name) > maxDisplayName {
			return bad("a recipient display name is longer than %d characters", maxDisplayName)
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
	if err := checkEnvelopeAddr(a.Address); err != nil {
		return nil, err
	}
	return a, nil
}

// checkEnvelopeAddr accepts only a plain address, the one form that is safe to
// write between the angle brackets of MAIL FROM and RCPT TO. The local part is a
// dot-atom: RFC 5322 atext characters and single dots, so no quote, space, "<",
// ">", "@", backslash or control character (net/mail accepts a quoted local part
// and hands it back unquoted, which would end the path early on the wire). The
// domain is dot-separated LDH labels (letters, digits, hyphen); no address
// literal. At most 254 octets in all, 64 in the local part and 63 per label. A
// display name is not part of it: the header may carry one, the envelope never.
// This is the one check, used when a message or the settings are validated and
// again immediately before an address is written to the socket. The error is
// fixed words: it never echoes the address.
func checkEnvelopeAddr(addr string) error {
	errPlain := errors.New("not a plain e-mail address")
	at := strings.LastIndexByte(addr, '@')
	if at < 1 || len(addr) > maxAddress {
		return errPlain
	}
	local, domain := addr[:at], addr[at+1:]
	if len(local) > maxLocal || !dotAtom(local) || domain == "" {
		return errPlain
	}
	for _, label := range strings.Split(domain, ".") {
		if !ldhLabel(label) {
			return errPlain
		}
	}
	return nil
}

// dotAtom reports whether s is one or more atext runs separated by single dots.
func dotAtom(s string) bool {
	if s == "" {
		return false
	}
	prevDot := true // a leading dot is as wrong as a doubled one
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			if prevDot {
				return false
			}
			prevDot = true
			continue
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0:
		default:
			return false
		}
		prevDot = false
	}
	return !prevDot
}

// ldhLabel reports whether s is a host-name label: letters, digits and hyphens,
// not starting or ending with a hyphen, 1 to 63 characters.
func ldhLabel(s string) bool {
	if s == "" || len(s) > maxLabel || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
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
// From, To, Subject (encoded words when it is not ASCII; a non-ASCII display name
// is encoded the same way), Message-ID under the From domain, MIME-Version,
// Content-Type (the boundary on a continuation line), Auto-Submitted: auto-generated and,
// when Kind is set, X-CryoBackup-Kind. There is no Bcc and no Cc: every
// recipient is in To. The body is multipart/alternative with the text part
// first and the HTML part second, both UTF-8 and quoted-printable; without HTML
// it is one text/plain part. Nothing in the output loads remote content unless
// the caller's own HTML does.
//
// Header lines are at most 78 octets wherever there is a fold point: a space in
// an ASCII subject or display name, the gap between encoded words, between
// addresses and in the Content-Type. A token with no fold point (an address of
// 254 octets, a long unbroken ASCII word, a Message-ID under a long domain) stays
// on one line; no line passes 998 octets, which maxSubject and maxDisplayName
// guarantee for the parts the caller supplies. A fold never leaves a line of
// whitespace only.
func (m Message) Build(now time.Time) ([]byte, error) {
	a, err := m.parse()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	h := func(name, value string) { b.WriteString(name + ": " + value + "\r\n") }

	h("Date", now.UTC().Format(time.RFC1123Z))
	b.WriteString(addressHeader("From", []*netmail.Address{a.from}))
	b.WriteString(addressHeader("To", a.to))
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
	h("Content-Type", "multipart/alternative;\r\n boundary="+mw.Boundary())
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

// Header folding. A header line is kept within maxHeaderLine octets (RFC 5322
// 2.1.1 recommends 78; the hard limit is 998) and an encoded word within
// maxEncodedWord characters (RFC 2047 2). foldColumn is where ASCII text is
// folded at a space.
const (
	maxHeaderLine  = 78
	foldColumn     = 76
	maxEncodedWord = 75
	// encodedOverhead is the "=?utf-8?q?" and "?=" around the payload of a word.
	encodedOverhead = len("=?utf-8?q?") + len("?=")
)

// subjectHeader returns the folded Subject header line(s) with their CRLF.
// A non-ASCII subject becomes encoded words (see encodeWords); an ASCII one is
// folded at spaces.
func subjectHeader(s string) string {
	const name = "Subject: "
	if !isASCII(s) {
		return name + encodeWords(s, len(name)) + "\r\n"
	}
	return name + foldSpaces(len(name), s) + "\r\n"
}

// addressHeader returns the From or To header with its CRLF: the addresses
// separated by a comma and a fold, each as "Name <addr>" or "<addr>".
func addressHeader(name string, list []*netmail.Address) string {
	var out strings.Builder
	out.WriteString(name + ": ")
	col := len(name) + 2
	for i, a := range list {
		if i > 0 {
			out.WriteString(",\r\n ")
			col = 1
		}
		if isASCII(a.Name) {
			out.WriteString(foldSpaces(col, a.String()))
			continue
		}
		// The angle address goes on its own folded line: it cannot be split, and
		// after a last word of 75 characters it would pass the line limit.
		out.WriteString(encodeWords(a.Name, col) + "\r\n <" + a.Address + ">")
	}
	out.WriteString("\r\n")
	return out.String()
}

// foldSpaces returns value with a CRLF and a space inserted at spaces so no line
// passes foldColumn columns, given that the first line starts at column col. A
// word longer than that stays whole; the 998-octet limit is enforced by
// maxSubject and maxDisplayName. No trailing CRLF.
func foldSpaces(col int, value string) string {
	var out strings.Builder
	for i, w := range strings.Split(value, " ") {
		if i > 0 {
			// Never fold before an empty word (a trailing space or a run of
			// spaces): the new line would hold whitespace only, which RFC 5322
			// 3.2.2 forbids generating. The spaces stay at the end of the line.
			if w != "" && col+1+len(w) > foldColumn && col > 1 {
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
	return out.String()
}

// encodeWords returns s as RFC 2047 encoded words (UTF-8, Q encoding) joined by
// a CRLF and a space. The first line starts at column col. Each word is at most
// maxEncodedWord characters and no line passes maxHeaderLine octets. A
// multi-byte character is never split between two words (RFC 2047 5(3)); a byte
// that is not valid UTF-8 is encoded on its own. Only letters and digits are
// written as they are and a space is "_", so the result is safe in a phrase as
// well as in an unstructured header. No trailing CRLF.
func encodeWords(s string, col int) string {
	// payloadLimit is what a word may carry on a line that starts at column col.
	payloadLimit := func(col int) int {
		n := maxEncodedWord
		if room := maxHeaderLine - col; room < n {
			n = room
		}
		if n -= encodedOverhead; n < 12 { // one 4-byte character
			n = 12
		}
		return n
	}
	var out, payload strings.Builder
	limit := payloadLimit(col)
	emit := func() {
		out.WriteString("=?utf-8?q?" + payload.String() + "?=")
		payload.Reset()
	}
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		piece := qEncode(s[i : i+size])
		if payload.Len() > 0 && payload.Len()+len(piece) > limit {
			emit()
			out.WriteString("\r\n ")
			limit = payloadLimit(1)
		}
		payload.WriteString(piece)
		i += size
	}
	if payload.Len() > 0 {
		emit()
	}
	return out.String()
}

// qEncode is the Q encoding of one character.
func qEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('_')
		default:
			fmt.Fprintf(&b, "=%02X", c)
		}
	}
	return b.String()
}

// messageID is <random@domain> with the domain of the From address.
func messageID(addr string) string {
	domain := addr[strings.LastIndex(addr, "@")+1:]
	var r [16]byte
	_, _ = rand.Read(r[:])
	return "<" + hex.EncodeToString(r[:]) + "@" + domain + ">"
}
