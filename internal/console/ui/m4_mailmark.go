package ui

import (
	"bytes"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/notify"
)

// P20-MD (ADR 0022): the brand mark of the e-mail messages, for the console's previews of them.
//
// A message carries the mark inside itself and shows it as <img src="cid:...">, which a browser
// cannot resolve. The sandboxed preview frames (the digest preview of /settings/email and the
// samples of /design/mail) point it at this route instead, which serves the same embedded file
// (notify.MarkPNG): same origin, so the console's Content-Security-Policy (img-src 'self') allows it
// unchanged. It is public like /static: a sandboxed frame has an opaque origin and sends no session
// cookie, and the file is the public logo (rendered from web/static/favicon.svg, itself public).
//
// It is also the one response of the admin listener that says Cross-Origin-Resource-Policy:
// cross-origin, overriding the listener's same-origin. The frames are sandboxed without
// allow-same-origin, so their requests come from an opaque origin, and a same-origin (or same-site)
// CORP refuses the image to them: proven in Chrome, where img-src 'self' of the inherited CSP allows
// the load and CORP alone blocks it. The file is public and the same for everyone, so letting any
// origin embed it discloses nothing. The CSP is unchanged.

const (
	patMailMark  = "GET /mail/mark"
	mailMarkPath = "/mail/mark"
)

// mailMarkPNG is read once: the bytes never change while the console runs.
var mailMarkPNG = notify.MarkPNG()

func (s *Service) mailMark(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin") // the sandboxed preview frames have an opaque origin
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(mailMarkPNG))
}

// mailPreviewHTML is a message's HTML part as a preview frame shows it (notify.PreviewHTML): the mark
// from mailMarkPath, the inline <style> block left out (the frame inherits style-src 'self').
func mailPreviewHTML(html string) string { return notify.PreviewHTML(html, mailMarkPath) }
