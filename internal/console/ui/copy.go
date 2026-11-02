package ui

import "html/template"

// Copy buttons (M2-W3A, components 9.6 "Code block"). A one-time secret or a
// generated snippet is a <pre> (or a list) with an id; copybtn draws the
// button that copies its text. The button is hidden until js/copy.js runs
// (theme.js marks the page with the js class), and without script the text is
// still one click to select (user-select: all) and the hint under the button
// says how to copy it. The page needs no network request to copy.

// copyButton returns the markup of a copy button for the element with id
// target, plus the no-script hint. label names what is copied for assistive
// technology ("Copy the enrollment token"). Both values are escaped.
func copyButton(target, label string) template.HTML {
	t, l := template.HTMLEscapeString(target), template.HTMLEscapeString(label)
	return template.HTML(`<button type="button" class="btn sm copybtn" data-copy-from="` + t + `" aria-label="` + l + `">` +
		`<svg width="12" height="12" aria-hidden="true"><use href="#i-copy"/></svg><span class="copylabel">Copy</span></button>` +
		`<span class="copyhint meta-mono">Select the text and press Ctrl+C to copy it.</span>`)
}
