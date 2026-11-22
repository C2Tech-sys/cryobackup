package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"strings"
	"testing"
)

// P20-MD: inline images. A message with HTML and inline images is
// multipart/alternative { text/plain, multipart/related { text/html, image... } }.

// tinyPNG is a few bytes that stand for an image: Build does not decode it.
var tinyPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0, 1, 2, 3, 250, 251, 252, 253}, 40)...)

func inlineMessage() Message {
	return Message{
		From:    "Cryo Console <console@example.com>",
		To:      []string{"ops@example.com"},
		Subject: "Backup alert",
		Text:    "Two devices failed.\n",
		HTML:    `<p><img src="cid:mark@console" width="28" height="28" alt=""> Two devices failed.</p>` + "\n",
		Kind:    KindAlert,
		Inline:  []Inline{{ContentID: "mark@console", MediaType: "image/png", Data: tinyPNG}},
	}
}

type mimePart struct {
	header map[string][]string
	body   []byte // decoded
	kids   []mimePart
}

// readTree parses a MIME entity (header + body) into a tree, decoding leaves.
func readTree(t *testing.T, ctype, cte string, body io.Reader) mimePart {
	t.Helper()
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		t.Fatalf("Content-Type %q: %v", ctype, err)
	}
	if strings.HasPrefix(mt, "multipart/") {
		var p mimePart
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextRawPart()
			if err == io.EOF {
				return p
			}
			if err != nil {
				t.Fatal(err)
			}
			kid := readTree(t, part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part)
			kid.header = part.Header
			p.kids = append(p.kids, kid)
		}
	}
	var r io.Reader = body
	switch strings.ToLower(cte) {
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, body)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decoding a %s part: %v", mt, err)
	}
	return mimePart{body: b}
}

func mediaType(t *testing.T, h map[string][]string) (string, map[string]string) {
	t.Helper()
	v := ""
	if len(h["Content-Type"]) > 0 {
		v = h["Content-Type"][0]
	}
	mt, params, err := mime.ParseMediaType(v)
	if err != nil {
		t.Fatalf("Content-Type %q: %v", v, err)
	}
	return mt, params
}

func TestBuildWithInlineImageNestsRelatedInsideAlternative(t *testing.T) {
	msg, raw := mustBuild(t, inlineMessage())
	assertWireLimits(t, raw)
	assertHeaderLines(t, raw, 78)
	if mt, _, _ := mime.ParseMediaType(msg.Header.Get("Content-Type")); mt != "multipart/alternative" {
		t.Fatalf("top Content-Type = %q", msg.Header.Get("Content-Type"))
	}
	root := readTree(t, msg.Header.Get("Content-Type"), "", msg.Body)
	if len(root.kids) != 2 {
		t.Fatalf("alternative has %d parts, want 2", len(root.kids))
	}
	if mt, _ := mediaType(t, root.kids[0].header); mt != "text/plain" || string(root.kids[0].body) != "Two devices failed.\r\n" {
		t.Fatalf("first part %s %q", mt, root.kids[0].body)
	}
	rel := root.kids[1]
	mt, params := mediaType(t, rel.header)
	if mt != "multipart/related" || params["type"] != "text/html" {
		t.Fatalf("second part %s %v, want multipart/related; type=text/html", mt, params)
	}
	if len(rel.kids) != 2 {
		t.Fatalf("related has %d parts, want 2", len(rel.kids))
	}
	html, img := rel.kids[0], rel.kids[1]
	if mt, p := mediaType(t, html.header); mt != "text/html" || !strings.EqualFold(p["charset"], "utf-8") || html.header["Content-Transfer-Encoding"][0] != "quoted-printable" {
		t.Fatalf("html part %s %v", mt, html.header)
	}
	if mt, _ := mediaType(t, img.header); mt != "image/png" {
		t.Fatalf("image part %s", mt)
	}
	id := img.header["Content-Id"]
	if len(id) != 1 || id[0] != "<mark@console>" {
		t.Fatalf("Content-ID = %v", img.header)
	}
	// The id in the header is the one the HTML points at.
	cid := strings.TrimSuffix(strings.TrimPrefix(id[0], "<"), ">")
	if !strings.Contains(string(html.body), `src="cid:`+cid+`"`) {
		t.Fatalf("the HTML does not reference cid:%s:\n%s", cid, html.body)
	}
	disp, dp, err := mime.ParseMediaType(img.header["Content-Disposition"][0])
	if err != nil || disp != "inline" || dp["filename"] != "mark.png" {
		t.Fatalf("Content-Disposition %v (%v %v)", img.header["Content-Disposition"], dp, err)
	}
	if img.header["Content-Transfer-Encoding"][0] != "base64" || !bytes.Equal(img.body, tinyPNG) {
		t.Fatalf("the image does not decode to the embedded bytes (%d bytes, cte %v)", len(img.body), img.header["Content-Transfer-Encoding"])
	}
	// base64 lines are at most 76 characters.
	enc := base64.StdEncoding.EncodeToString(tinyPNG)
	start := bytes.Index(raw, []byte(enc[:40]))
	if start < 0 {
		t.Fatal("the base64 body is not in the message")
	}
	n := 0
	for _, l := range strings.Split(string(raw[start:]), "\r\n") {
		if strings.HasPrefix(l, "--") {
			break
		}
		if len(l) > 76 {
			t.Fatalf("a base64 line of %d characters", len(l))
		}
		n++
	}
	if n < 2 {
		t.Fatalf("the base64 body was not wrapped (%d lines)", n)
	}
}

func TestBuildWithoutInlineImagesIsUnchanged(t *testing.T) {
	m := inlineMessage()
	m.Inline = nil
	msg, _ := mustBuild(t, m)
	ps := parts(t, msg)
	if len(ps) != 2 || ps[1].typ != "text/html" {
		t.Fatalf("without inline images the message is text + html: %+v", ps)
	}
}

func TestInlineImagesAreValidated(t *testing.T) {
	big := make([]byte, 64<<10+1)
	cases := map[string]func(*Message){
		"no html":          func(m *Message) { m.HTML = "" },
		"blank html":       func(m *Message) { m.HTML = "  \n" },
		"angle id":         func(m *Message) { m.Inline[0].ContentID = "<mark@console>" },
		"crlf id":          func(m *Message) { m.Inline[0].ContentID = "mark\r\nBcc: x@example.test" },
		"space id":         func(m *Message) { m.Inline[0].ContentID = "mark console" },
		"quote id":         func(m *Message) { m.Inline[0].ContentID = `mark"@console` },
		"empty id":         func(m *Message) { m.Inline[0].ContentID = "" },
		"long id":          func(m *Message) { m.Inline[0].ContentID = strings.Repeat("a", 101) },
		"jpeg":             func(m *Message) { m.Inline[0].MediaType = "image/jpeg" },
		"svg":              func(m *Message) { m.Inline[0].MediaType = "image/svg+xml" },
		"type with params": func(m *Message) { m.Inline[0].MediaType = "image/png; name=x" },
		"empty data":       func(m *Message) { m.Inline[0].Data = nil },
		"too big":          func(m *Message) { m.Inline[0].Data = big },
		"duplicate id": func(m *Message) {
			m.Inline = append(m.Inline, Inline{ContentID: "mark@console", MediaType: "image/png", Data: tinyPNG})
		},
	}
	for name, mod := range cases {
		m := inlineMessage()
		mod(&m)
		_, err := m.Build(fixedNow)
		if !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: err = %v, want ErrInvalidMessage", name, err)
			continue
		}
		for _, leak := range []string{"mark", "console", "jpeg", "svg", "Bcc"} {
			if strings.Contains(strings.TrimPrefix(err.Error(), ErrInvalidMessage.Error()), leak) {
				t.Errorf("%s: the error names a value (%q): %v", name, leak, err)
			}
		}
	}
	if err := inlineMessage().Validate(); err != nil {
		t.Fatalf("the valid inline message is refused: %v", err)
	}
	m := inlineMessage()
	m.Inline[0].Data = make([]byte, 64<<10)
	if err := m.Validate(); err != nil {
		t.Fatalf("an image of exactly 64 KiB is refused: %v", err)
	}
}

// Two images keep their order and their ids.
func TestBuildWithTwoInlineImages(t *testing.T) {
	m := inlineMessage()
	m.Inline = append(m.Inline, Inline{ContentID: "second_image-2@console", MediaType: "image/png", Data: []byte("other")})
	msg, raw := mustBuild(t, m)
	assertWireLimits(t, raw)
	root := readTree(t, msg.Header.Get("Content-Type"), "", msg.Body)
	rel := root.kids[1]
	if len(rel.kids) != 3 || rel.kids[2].header["Content-Id"][0] != "<second_image-2@console>" || string(rel.kids[2].body) != "other" {
		t.Fatalf("related parts: %d", len(rel.kids))
	}
	if _, raw2 := mustBuild(t, m); !bytes.Contains(raw2, []byte(`filename="second_image-2.png"`)) {
		t.Fatal("the second image's file name is not derived from its id")
	}
}
