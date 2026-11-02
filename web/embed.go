// Package web embeds the console templates and vendored static assets.
// Nothing here is loaded from a CDN at runtime (CLAUDE.md).
package web

import "embed"

// Templates holds the html/template sources.
//
//go:embed templates
var Templates embed.FS

// Static holds CSS, JS, fonts and images served at /static/.
//
//go:embed static
var Static embed.FS
