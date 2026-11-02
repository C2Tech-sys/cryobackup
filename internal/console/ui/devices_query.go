package ui

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// This file holds what the M2 list screens (devices, customers, run history,
// audit) share: query-string parsing and encoding, the pager, filter chips and
// the 12-hour time helpers. Every list is deep-linkable: the whole state of a
// list lives in its query string, and the encoders omit defaults so a link is
// as short as it can be.

// Option is one entry of a select.
type Option struct {
	Value    string
	Label    string
	Selected bool
}

// PagerLink is one control of the pager. Partial is the htmx fragment URL for
// the same page (empty when the list has no fragment).
type PagerLink struct {
	Label    string
	Aria     string
	Href     string
	Partial  string
	Current  bool
	Disabled bool
}

// PagerView is the pager of components 5.2: the result text, First, Previous,
// up to five page numbers, Next, Last, and the page-size choices.
type PagerView struct {
	// Text is "Showing 51-100 of 212", always with the total.
	Text  string
	Links []PagerLink
	Sizes []PagerLink
	// Pages > 1 shows the page controls; the size choices show whenever the total exceeds the smallest page size.
	Pages int
	Clamp string // set when the requested page was past the end
}

// FilterChip is one removable active filter (components 3.4).
type FilterChip struct {
	Label   string
	Href    string
	Partial string
}

// listURL joins a base path and a query.
func listURL(base string, v url.Values) string {
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// ---- the device list query ----

// devQuery is the state of a device table.
type devQuery struct {
	Customer string
	States   []string
	Q        string
	Sort     string
	Desc     bool
	Page     int
	Size     int
	Retired  bool
}

// parseDevQuery reads a device table's query string. It is lenient about
// shape (a missing or garbled page is page 1) and strict about meaning: an
// unknown sort or state is reported, never silently ignored.
func parseDevQuery(v url.Values) (devQuery, []store.FieldError) {
	q := devQuery{Customer: strings.TrimSpace(v.Get("customer")), Q: strings.TrimSpace(v.Get("q")), Sort: v.Get("sort"),
		Desc: v.Get("dir") == "desc", Page: atoiDefault(v.Get("page"), 1), Size: store.NormalPageSize(atoiDefault(v.Get("size"), 0)),
		Retired: v.Get("retired") == "1"}
	var bad []store.FieldError
	for _, st := range v["state"] {
		st = strings.ToUpper(strings.TrimSpace(st))
		switch {
		case st == "":
		case !store.ValidDeviceState(st):
			bad = append(bad, store.FieldError{Field: "state", Message: "Unknown device state."})
		default:
			q.States = append(q.States, st)
		}
	}
	if q.Sort != "" {
		known := false
		for _, k := range store.DeviceSorts {
			known = known || k == q.Sort
		}
		if !known {
			bad = append(bad, store.FieldError{Field: "sort", Message: "Unknown sort key."})
			q.Sort = ""
		}
	}
	if q.Sort == "state" && !q.Desc {
		q.Sort = "" // the default
	}
	return q, bad
}

// values encodes q, leaving out every default.
func (q devQuery) values() url.Values {
	v := url.Values{}
	if q.Customer != "" {
		v.Set("customer", q.Customer)
	}
	for _, st := range q.States {
		v.Add("state", st)
	}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if q.Sort != "" {
		v.Set("sort", q.Sort)
	}
	if q.Desc {
		v.Set("dir", "desc")
	}
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	if q.Size != store.DefaultPageSize {
		v.Set("size", strconv.Itoa(q.Size))
	}
	if q.Retired {
		v.Set("retired", "1")
	}
	return v
}

func (q devQuery) filtered() bool { return q.Q != "" || len(q.States) > 0 || q.Retired }

// ---- pager ----

// buildPager renders the pager for info; href and partial map a page number and
// a page size to the URL of that view.
func buildPager(info store.PageInfo, noun string, href func(page, size int) string, partial func(page, size int) string) PagerView {
	pv := PagerView{Pages: info.Pages}
	switch {
	case info.Total == 0:
		pv.Text = "Showing 0 of 0 " + noun
	default:
		pv.Text = "Showing " + strconv.Itoa(info.From) + "-" + strconv.Itoa(info.To) + " of " + strconv.Itoa(info.Total)
	}
	if info.Clamped {
		pv.Clamp = "That page is past the end, so the last page is shown."
	}
	link := func(label, aria string, page int, disabled bool) PagerLink {
		l := PagerLink{Label: label, Aria: aria, Disabled: disabled, Current: page == info.Page && !disabled && aria == "Page "+strconv.Itoa(page)}
		if !disabled {
			l.Href = href(page, info.Size)
			if partial != nil {
				l.Partial = partial(page, info.Size)
			}
		}
		return l
	}
	if info.Pages > 1 {
		pv.Links = append(pv.Links,
			link("First", "First page", 1, info.Page == 1),
			link("Previous", "Previous page", info.Page-1, info.Page == 1))
		lo, hi := info.Page-2, info.Page+2
		if lo < 1 {
			lo, hi = 1, 5
		}
		if hi > info.Pages {
			hi = info.Pages
			if lo = hi - 4; lo < 1 {
				lo = 1
			}
		}
		for p := lo; p <= hi; p++ {
			pv.Links = append(pv.Links, link(strconv.Itoa(p), "Page "+strconv.Itoa(p), p, false))
		}
		pv.Links = append(pv.Links,
			link("Next", "Next page", info.Page+1, info.Page == info.Pages),
			link("Last", "Last page", info.Pages, info.Page == info.Pages))
	}
	if info.Total > store.PageSizes[0] {
		for _, sz := range store.PageSizes {
			l := PagerLink{Label: strconv.Itoa(sz), Aria: strconv.Itoa(sz) + " per page", Href: href(1, sz), Current: sz == info.Size}
			if partial != nil {
				l.Partial = partial(1, sz)
			}
			pv.Sizes = append(pv.Sizes, l)
		}
	}
	return pv
}

// ---- time helpers (12-hour clock, display zone) ----

// dayClock is "Mon 2 Jan, 3:04 PM".
func (c fmtCtx) dayClock(t time.Time) string { return t.In(c.loc()).Format("Mon 2 Jan, 3:04 PM") }

// dateClock is "2 Jan 2026, 3:04 PM".
func (c fmtCtx) dateClock(t time.Time) string { return t.In(c.loc()).Format("2 Jan 2006, 3:04 PM") }

// zoneAbbr is "CDT", "UTC" and so on, for the time t in the display zone.
func (c fmtCtx) zoneAbbr(t time.Time) string {
	name, _ := t.In(c.loc()).Zone()
	return name
}

// ageFuture is "in 40 min", "in 3 h", "in 2 d".
func ageFuture(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return "in " + strconv.Itoa(int(d/time.Minute)) + " min"
	case d < 48*time.Hour:
		return "in " + strconv.Itoa(int(d/time.Hour)) + " h"
	default:
		return "in " + strconv.Itoa(int(d/(24*time.Hour))) + " d"
	}
}

// stateOptions is the state filter select: every state of the precedence
// table except RETIRED (retired devices have their own switch).
func stateOptions(selected string) []Option {
	out := []Option{{Value: "", Label: "Any state", Selected: selected == ""}}
	for _, st := range state.Precedence() {
		if st == state.StateRetired {
			continue
		}
		out = append(out, Option{Value: string(st), Label: stateLabel(st), Selected: string(st) == selected})
	}
	return out
}
