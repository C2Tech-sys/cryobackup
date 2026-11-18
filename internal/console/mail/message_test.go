package mail

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 18, 6, 30, 0, 0, time.FixedZone("CST", -6*3600))

func mustBuild(t *testing.T, m Message) (*netmail.Message, []byte) {
	t.Helper()
	raw, err := m.Build(fixedNow)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the built message does not parse as RFC 5322: %v\n%s", err, raw)
	}
	return msg, raw
}

// decodeSinglePart returns the decoded text of a single-part message (what the
// quoted-printable reader gives back: CRLF line ends).
func decodeSinglePart(t *testing.T, raw []byte) string {
	t.Helper()
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("Content-Transfer-Encoding"); got != "quoted-printable" {
		t.Fatalf("Content-Transfer-Encoding = %q", got)
	}
	b, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// parts returns the raw parts of a multipart/alternative message: their media
// type, charset, transfer encoding and decoded text.
type partInfo struct{ typ, charset, cte, text string }

func parts(t *testing.T, msg *netmail.Message) []partInfo {
	t.Helper()
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, %v", msg.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var out []partInfo
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		pt, pp, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, partInfo{pt, pp["charset"], p.Header.Get("Content-Transfer-Encoding"), string(b)})
	}
}

func TestBuildStructure(t *testing.T) {
	m := Message{
		From:    "Cryo Console <console@example.com>",
		To:      []string{"ops@example.com", "Night Desk <night@example.org>"},
		Subject: "Backup alert: 2 devices",
		Text:    "Two devices failed.\nSee the console.\n",
		HTML:    "<p>Two devices failed.</p>\n",
		Kind:    KindAlert,
	}
	msg, raw := mustBuild(t, m)
	h := msg.Header
	if d, err := h.Date(); err != nil || !d.Equal(fixedNow) || !strings.Contains(h.Get("Date"), "+0000") {
		t.Fatalf("Date = %q (%v): must be the build time in UTC", h.Get("Date"), err)
	}
	if h.Get("MIME-Version") != "1.0" || h.Get("Auto-Submitted") != "auto-generated" || h.Get("X-CryoBackup-Kind") != "alert" {
		t.Fatalf("headers = %v", h)
	}
	if id := h.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Fatalf("Message-ID = %q, want one under the From domain", id)
	}
	from, err := h.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Address != "console@example.com" || from[0].Name != "Cryo Console" {
		t.Fatalf("From = %v, %v", from, err)
	}
	to, err := h.AddressList("To")
	if err != nil || len(to) != 2 || to[1].Address != "night@example.org" {
		t.Fatalf("To = %v, %v", to, err)
	}
	if h.Get("Bcc") != "" || h.Get("Cc") != "" {
		t.Fatal("Build must not add Cc or Bcc")
	}
	ps := parts(t, msg)
	if len(ps) != 2 {
		t.Fatalf("%d parts", len(ps))
	}
	if ps[0].typ != "text/plain" || ps[1].typ != "text/html" {
		t.Fatalf("part order = %s, %s: text first, HTML second", ps[0].typ, ps[1].typ)
	}
	for _, p := range ps {
		if !strings.EqualFold(p.charset, "utf-8") || p.cte != "quoted-printable" {
			t.Fatalf("part %+v: want utf-8 quoted-printable", p)
		}
	}
	if ps[0].text != "Two devices failed.\r\nSee the console.\r\n" || ps[1].text != "<p>Two devices failed.</p>\r\n" {
		t.Fatalf("part texts = %q / %q", ps[0].text, ps[1].text)
	}
	assertWireLimits(t, raw)
}

// The wire form: CRLF only, no line over 998, and the body parts wrapped at 76.
func assertWireLimits(t *testing.T, raw []byte) {
	t.Helper()
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) ||
		bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\r")) {
		t.Fatal("the message has a bare LF or CR")
	}
	for _, l := range strings.Split(string(raw), "\r\n") {
		if len(l) > 998 {
			t.Fatalf("a line of %d bytes", len(l))
		}
	}
	if !bytes.HasSuffix(raw, []byte("\r\n")) {
		t.Fatal("the message does not end with a line end")
	}
}

func TestBuildWithoutHTMLIsOneTextPart(t *testing.T) {
	msg, raw := mustBuild(t, Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "hello\n"})
	if mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type")); mt != "text/plain" || !strings.EqualFold(params["charset"], "utf-8") {
		t.Fatalf("Content-Type = %q", msg.Header.Get("Content-Type"))
	}
	if got := decodeSinglePart(t, raw); got != "hello\r\n" {
		t.Fatalf("body = %q", got)
	}
	if msg.Header.Get("X-CryoBackup-Kind") != "" {
		t.Fatal("no kind, no header")
	}
}

func TestBuildAddsNoRemoteContent(t *testing.T) {
	_, raw := mustBuild(t, Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "plain\n", HTML: "<p>plain</p>\n"})
	for _, needle := range []string{"http:", "https:", "src=", "<img", "<link", "url(", "@import", "<script"} {
		if bytes.Contains(bytes.ToLower(raw), []byte(needle)) {
			t.Fatalf("the builder added %q", needle)
		}
	}
}

func TestBuildNonASCIIRoundTrip(t *testing.T) {
	subjects := []string{
		"Sauvegarde échouée – 備份 ✓",
		// long enough to need several encoded words
		strings.Repeat("Überprüfung der Sicherung ", 8),
		"\U0001F6A8 alert",
	}
	text := "Grüße aus Köln\n.a line that starts with a dot\n" + strings.Repeat("long ascii line ", 20) + "\ntrailing space \n備份失敗\n"
	html := "<p>Grüße &amp; café =3D literal</p>\n"
	for _, subj := range subjects {
		msg, raw := mustBuild(t, Message{
			From: "Café Console <console@example.com>", To: []string{"Réception <ops@example.com>"},
			Subject: subj, Text: text, HTML: html,
		})
		got, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
		if err != nil || got != subj {
			t.Fatalf("subject round trip = %q, %v; want %q", got, err, subj)
		}
		// The header block itself is pure ASCII.
		hdr := raw[:bytes.Index(raw, []byte("\r\n\r\n"))]
		if !isASCII(string(hdr)) {
			t.Fatalf("a header carries raw non-ASCII: %q", hdr)
		}
		from, err := msg.Header.AddressList("From")
		if err != nil || from[0].Name != "Café Console" {
			t.Fatalf("From name = %v, %v", from, err)
		}
		ps := parts(t, msg)
		if ps[0].text != strings.ReplaceAll(text, "\n", "\r\n") || ps[1].text != strings.ReplaceAll(html, "\n", "\r\n") {
			t.Fatalf("body round trip failed:\n%q\n%q", ps[0].text, ps[1].text)
		}
		assertWireLimits(t, raw)
		for _, l := range strings.Split(string(raw), "\r\n") {
			if len(l) > 78 && !strings.HasPrefix(l, "Subject:") && !strings.HasPrefix(l, "From:") && !strings.HasPrefix(l, "To:") && !strings.HasPrefix(l, "Content-Type:") && !strings.HasPrefix(l, " ") {
				t.Fatalf("a body line of %d bytes: quoted-printable must wrap at 76", len(l))
			}
		}
	}
}

func TestBuildFoldsALongASCIISubject(t *testing.T) {
	subj := strings.Repeat("backup status for the fleet ", 8) + "end"
	msg, raw := mustBuild(t, Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: subj, Text: "t\n"})
	if got := msg.Header.Get("Subject"); got != subj {
		t.Fatalf("unfolded subject = %q", got)
	}
	for _, l := range strings.Split(string(raw), "\r\n") {
		if len(l) > 78 {
			t.Fatalf("header line of %d bytes: %q", len(l), l)
		}
	}
}

func TestBuildRejectsHeaderInjection(t *testing.T) {
	inj := []string{"\r\nBcc: evil@example.net", "\nBcc: evil@example.net", "\rBcc: evil@example.net", "\x00", "a\x1bb", "\x7f"}
	for _, bad := range inj {
		base := func() Message {
			return Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "t", Kind: KindAlert}
		}
		cases := map[string]func(*Message){
			"subject":      func(m *Message) { m.Subject = "hello" + bad },
			"from":         func(m *Message) { m.From = "a@example.com" + bad },
			"from name":    func(m *Message) { m.From = "\"Name" + bad + "\" <a@example.com>" },
			"to":           func(m *Message) { m.To = []string{"b@example.com" + bad} },
			"second to":    func(m *Message) { m.To = []string{"b@example.com", "c@example.com" + bad} },
			"to name":      func(m *Message) { m.To = []string{"\"N" + bad + "\" <b@example.com>"} },
			"kind":         func(m *Message) { m.Kind = Kind("alert" + bad) },
			"subject only": func(m *Message) { m.Subject = bad },
		}
		for name, mod := range cases {
			m := base()
			mod(&m)
			raw, err := m.Build(fixedNow)
			if err == nil {
				t.Fatalf("%s with %q: accepted:\n%s", name, bad, raw)
			}
			if !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("%s: error does not wrap ErrInvalidMessage: %v", name, err)
			}
			if strings.Contains(err.Error(), "evil") {
				t.Fatalf("%s: the error echoes the value: %v", name, err)
			}
			if m.Validate() == nil {
				t.Fatalf("%s: Validate accepted it", name)
			}
		}
	}
	// A tab is a legal header character.
	if _, err := (Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "a\tb", Text: "t"}).Build(fixedNow); err != nil {
		t.Fatalf("a tab in the subject: %v", err)
	}
}

func TestBuildRejectsMalformedMessages(t *testing.T) {
	ok := Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "t"}
	cases := map[string]func(*Message){
		"no from":           func(m *Message) { m.From = "" },
		"bad from":          func(m *Message) { m.From = "not an address" },
		"no recipient":      func(m *Message) { m.To = nil },
		"bad recipient":     func(m *Message) { m.To = []string{"nobody"} },
		"non-ASCII address": func(m *Message) { m.To = []string{"jürgen@example.com"} },
		"empty text":        func(m *Message) { m.Text = " \n" },
		"long subject":      func(m *Message) { m.Subject = strings.Repeat("x", maxSubject+1) },
		"bad kind":          func(m *Message) { m.Kind = "Alert Now" },
	}
	for name, mod := range cases {
		m := ok
		mod(&m)
		if _, err := m.Build(fixedNow); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: Build = %v", name, err)
		}
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a valid message: %v", err)
	}
	// An address with a non-ASCII display name is fine.
	m := ok
	m.To = []string{"Jürgen <j@example.com>"}
	if err := m.Validate(); err != nil {
		t.Fatalf("non-ASCII display name: %v", err)
	}
}

func TestMessageIDsAreUniqueAndDateIsUTC(t *testing.T) {
	m := Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Text: "t"}
	a, _ := mustBuild(t, m)
	b, _ := mustBuild(t, m)
	if a.Header.Get("Message-ID") == b.Header.Get("Message-ID") {
		t.Fatal("two builds share a Message-ID")
	}
	if got := a.Header.Get("Date"); got != "Wed, 18 Nov 2026 12:30:00 +0000" {
		t.Fatalf("Date = %q", got)
	}
}
