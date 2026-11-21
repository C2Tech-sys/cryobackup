package notify

import (
	_ "embed"
	htmltemplate "html/template"
)

// P20-MD (ADR 0022): the HTML parts of the four messages share one shell (templates/shell.html.tmpl):
// the console's dark look as inline-styled tables, the mark as an inline image, one button.

//go:embed templates/shell.html.tmpl
var shellSource string

// softOf names the background a colour token sits on in a chip or a strip.
var softOf = map[string]string{
	"danger": "danger-soft", "warning": "warning-soft", "success": "success-soft", "accent": "accent-soft",
}

// soft is the soft background of a colour token; the raised surface for a colour without one (the
// muted notice band).
func soft(colour string) string {
	if s, ok := softOf[colour]; ok {
		return s
	}
	return "surface-raised"
}

// stripData is the headline strip: its level colour (a token name) and its text.
type stripData struct{ Colour, Text string }

// chipData is a state chip: its colour (a token name) and its word.
type chipData struct{ Colour, Word string }

// ctaData is the button: the console URL, and whether the no-link sentence stands in when there is none.
type ctaData struct {
	URL      string
	Sentence bool
}

// The ghost table that holds classic Outlook (which ignores max-width) to 640 px. Fixed markup,
// never data: html/template drops comments from a template, so it is written as a typed value.
const (
	msoOpen  = htmltemplate.HTML(`<!--[if mso]><table role="presentation" width="640" align="center" cellpadding="0" cellspacing="0" border="0"><tr><td><![endif]-->`)
	msoClose = htmltemplate.HTML(`<!--[if mso]></td></tr></table><![endif]-->`)
)

var mdFuncs = htmltemplate.FuncMap{
	"tok":      tok,
	"soft":     soft,
	"markSrc":  markSrc,
	"msoOpen":  func() htmltemplate.HTML { return msoOpen },
	"msoClose": func() htmltemplate.HTML { return msoClose },
	"bar":      func(colour, text string) stripData { return stripData{colour, text} },
	"chip":     func(colour, word string) chipData { return chipData{colour, word} },
	"cta":      func(url string, sentence bool) ctaData { return ctaData{url, sentence} },
}

// mdTemplate parses an HTML part's template with the shared shell.
func mdTemplate(name, source string) *htmltemplate.Template {
	t := htmltemplate.Must(htmltemplate.New(name).Funcs(mdFuncs).Parse(shellSource))
	return htmltemplate.Must(t.Parse(source))
}
