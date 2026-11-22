package notify

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	"regexp"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
)

// P20-MD (ADR 0022): the brand mark travels inside every message as an inline image under a
// content id, so showing it makes no request (it is not remote content: ADR 0020's decision on
// remote images stands). It keeps its own dark tile, so a mail client that recolours the message
// still shows it on dark. The CRYOBACKUP wordmark beside it is live text; no font is loaded.

// markPNG is the mark: 64x64 RGBA PNG rendered from web/static/favicon.svg, shown at 28x28.
//
//go:embed assets/mark.png
var markPNG []byte

// MarkContentID is the content id of the mark in a message. The HTML part shows the mark as
// <img src="cid:MarkContentID">.
const MarkContentID = "cryobackup-mark@console"

// MarkPNG returns the bytes of the mark (a copy): the one embedded file the messages carry and
// the console serves for its previews.
func MarkPNG() []byte { return bytes.Clone(markPNG) }

// markSrc is the src of the mark in the HTML part. html/template rewrites a URL of a scheme it does
// not know (cid:) to "#ZgotmplZ"; this fixed value is typed as a URL on purpose, and no data ever
// reaches it.
func markSrc() htmltemplate.URL { return htmltemplate.URL("cid:" + MarkContentID) }

// markInline is the mark as the inline image of a message: a copy of the embedded bytes, so a
// caller that changes a message's image cannot change the next message's.
func markInline() []mail.Inline {
	return []mail.Inline{{ContentID: MarkContentID, MediaType: "image/png", Data: MarkPNG()}}
}

// markAttr is the mark's src attribute exactly as the templates write it.
const markAttr = `src="cid:` + MarkContentID + `"`

// styleBlock matches the one <style> block of a message (its phone-width media query).
var styleBlock = regexp.MustCompile(`(?s)<style>.*?</style>\n?`)

// PreviewHTML is the HTML part of a message made fit for the console's sandboxed preview frame: the
// mark's cid: link, which does not resolve in a browser, points at markURL (the console's own route
// to the same embedded bytes), and the <style> block is left out, because the frame inherits the
// console's Content-Security-Policy (style-src 'self'), which refuses an inline <style> element;
// the inline style attributes that carry the look are allowed. Outlook's document settings in the
// head (msoHead, exact bytes) go too: a browser has no use for them. Nothing else changes.
func PreviewHTML(html, markURL string) string {
	html = strings.ReplaceAll(html, markAttr, `src="`+htmltemplate.HTMLEscapeString(markURL)+`"`)
	html = strings.Replace(html, string(msoHead)+"\n", "", 1)
	return styleBlock.ReplaceAllString(html, "")
}
