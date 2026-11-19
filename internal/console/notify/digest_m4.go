package notify

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	"sort"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The daily digest (ADR 0020 decision 8; replica/design/components.md 10.2): the console's banner
// state first, then the exceptions of state.DigestOf, critical before alarm level before the
// parked and long-deferred devices, each section grouped by customer, then one line of totals.
// It is sent every day, also when everything is green: a digest that only arrives on bad days
// cannot be told apart from a console that has stopped.
//
// Who is listed is state.DigestOf's decision (a suspended customer is listed and labelled muted);
// this file only lays it out. An acknowledged or snoozed alarm stays listed and is marked as such:
// an acknowledgement mutes mail, it never hides a state (DH8). The content rules are the alert's:
// customer names, host names, state words and times only, never a path, a file name, file
// contents, a note or a policy name, and no remote content.

//go:embed templates/digest.txt.tmpl
var digestTextSource string

//go:embed templates/digest.html.tmpl
var digestHTMLSource string

var (
	digestTextTmpl = texttemplate.Must(texttemplate.New("digest-text").Parse(digestTextSource))
	digestHTMLTmpl = htmltemplate.Must(htmltemplate.New("digest-html").Funcs(htmltemplate.FuncMap{"tok": tok}).Parse(digestHTMLSource))
)

// digestLine is one listed device or MISSING customer.
type digestLine struct {
	Host  string // empty for a MISSING customer
	State string // the state word shown
	Also  string // the other alarming flags, comma separated
	Since string
	Ack   string // ACKNOWLEDGED or SNOOZED; empty when neither
	// Indefinite flags a pause with no end date.
	Indefinite bool
}

// digestGroup is one customer's lines inside a section.
type digestGroup struct {
	Customer string
	Muted    bool
	Lines    []digestLine
	missing  bool // the customer is MISSING: its group leads its section, as in the alert
}

// digestSection is one severity band of the exceptions.
type digestSection struct {
	Title  string
	Glyph  htmltemplate.HTML // the band's glyph: a fixed character entity, never data
	Word   string            // the band's word in the plain-text part
	Colour string            // a token name
	Groups []digestGroup
	Count  int
}

// digestBanner is one console condition.
type digestBanner struct {
	Word, Colour, Text string
	Glyph              htmltemplate.HTML // a fixed character entity, never data
}

// digestPage is what both digest templates render.
type digestPage struct {
	Title      string // the first line: what the day amounts to
	Date       string
	Polled     string
	Checked    int    // devices the digest speaks for (not retired)
	CheckedAs  string // the same, worded: "212 devices"
	Banners    []digestBanner
	Sections   []digestSection
	Totals     string
	ConsoleURL string
}

// digestDevices counts the devices a digest speaks for: every device of the view that is not retired.
func digestDevices(v state.FleetView) int {
	n := 0
	for i := range v.Devices {
		if v.Devices[i].Primary != state.StateRetired {
			n++
		}
	}
	return n
}

// ackMarks maps "subject type/subject id/ack alarm type" to ACKNOWLEDGED or SNOOZED for the active
// acknowledgements.
func ackMarks(acks []store.Ack) map[string]string {
	m := make(map[string]string, len(acks))
	for _, a := range acks {
		w := "ACKNOWLEDGED"
		if a.SnoozeUntil != nil || a.UntilNextSuccess {
			w = "SNOOZED"
		}
		m[a.SubjectType+"/"+a.SubjectID+"/"+a.AlarmType] = w
	}
	return m
}

// ackOf is the acknowledgement mark of a digest line: the first of its alarms that is covered.
func ackOf(l state.DigestLine, marks map[string]string) string {
	subject, id := store.EpisodeDevice, l.DeviceID
	alarms := append([]string{string(l.State)}, statesToStrings(l.Alarms)...)
	if l.Kind == state.DigestMissing {
		subject, id, alarms = store.EpisodeCustomer, l.CustomerID, []string{store.AlarmMissing}
	}
	for _, a := range alarms {
		if t, ok := store.AckAlarmType(a); ok {
			if w := marks[subject+"/"+id+"/"+t]; w != "" {
				return w
			}
		}
	}
	return ""
}

func statesToStrings(ss []state.State) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// Sections of the exceptions, worst first.
const (
	sectionCritical = iota
	sectionWarning
	sectionOther
)

// sectionOf is the band of a line: a MISSING customer or a critical device; an alarm-level device;
// or a listed device that does not alarm (parked, deferred for more than a day).
func sectionOf(l state.DigestLine) int {
	switch {
	case l.Kind == state.DigestMissing || l.Alarm >= state.SeverityCritical:
		return sectionCritical
	case l.Alarm >= state.SeverityAlarm:
		return sectionWarning
	}
	return sectionOther
}

// RenderDigest renders the daily digest of v, the fleet as the evaluator sees it now, with the
// active acknowledgements acks (they mark lines, they remove none). One rendering serves every
// recipient. The subject is fixed words, the date and counts: short and ASCII at any fleet size,
// and no customer or host name rides in a header.
func RenderDigest(v state.FleetView, acks []store.Ack, o Options) (Content, error) {
	loc := o.Loc
	if loc == nil {
		loc = time.UTC
	}
	d := state.DigestOf(v, v.ComputedAt)
	marks := ackMarks(acks)
	p := digestPage{Date: v.ComputedAt.In(loc).Format("Mon 2 Jan"), Polled: when(v.ComputedAt, loc), Checked: digestDevices(v),
		ConsoleURL: consoleURL(o.ConsoleURL)}
	for _, b := range d.Banners {
		db := digestBanner{Word: "WARNING", Glyph: "&#9650;", Colour: "warning", Text: b.Text}
		if b.Severity >= state.SeverityCritical {
			db = digestBanner{Word: "CRITICAL", Glyph: "&#9670;", Colour: "danger", Text: b.Text}
		}
		p.Banners = append(p.Banners, db)
	}
	p.Sections = []digestSection{
		{Title: "Critical", Glyph: "&#9670;", Word: "CRITICAL", Colour: "danger"},
		{Title: "Warnings", Glyph: "&#9650;", Word: "WARNING", Colour: "warning"},
		{Title: "Parked and long-deferred devices", Glyph: "&#9675;", Word: "NOTICE", Colour: "text-muted"},
	}
	groups := [3]map[string]*digestGroup{{}, {}, {}}
	var order [3][]*digestGroup
	missing, deviceLines := 0, 0
	for _, l := range d.Lines {
		s := sectionOf(l)
		key := l.CustomerID
		g := groups[s][key]
		if g == nil {
			name := l.CustomerName
			if name == "" {
				name = "Customer " + l.CustomerID
			}
			g = &digestGroup{Customer: name, Muted: l.Muted}
			groups[s][key] = g
			order[s] = append(order[s], g)
		}
		dl := digestLine{Host: l.Hostname, State: string(l.State), Since: when(l.Since, loc), Ack: ackOf(l, marks)}
		if l.Kind == state.DigestMissing {
			missing++
			g.missing = true
			dl.State, dl.Since = store.AlarmMissing, ""
		} else {
			deviceLines++
			if dl.Host == "" {
				dl.Host = "device " + l.DeviceID
			}
			var also []string
			for _, a := range l.Alarms {
				if a != l.State {
					also = append(also, string(a))
				}
			}
			dl.Also = strings.Join(also, ", ")
			for _, a := range l.Attrs {
				dl.Indefinite = dl.Indefinite || a == state.AttrPausedIndefinite
			}
		}
		g.Lines = append(g.Lines, dl)
		p.Sections[s].Count++
	}
	for s := range p.Sections {
		sort.SliceStable(order[s], func(i, j int) bool {
			if order[s][i].missing != order[s][j].missing {
				return order[s][i].missing // silence first (components.md 10.2)
			}
			return strings.ToLower(order[s][i].Customer) < strings.ToLower(order[s][j].Customer)
		})
		for _, g := range order[s] {
			p.Sections[s].Groups = append(p.Sections[s].Groups, *g)
		}
	}
	p.CheckedAs = count(p.Checked, "device", "devices")
	crit, warn, other := p.Sections[sectionCritical].Count, p.Sections[sectionWarning].Count, p.Sections[sectionOther].Count
	var summary string
	switch {
	case d.AllClear && p.Checked == 0:
		summary, p.Title = "all clear, no devices enrolled yet", "No devices enrolled yet: nothing is being backed up."
	case d.AllClear:
		summary = "all clear, " + count(p.Checked, "device", "devices")
		p.Title = "All " + strconv.Itoa(p.Checked) + " devices backed up."
		if p.Checked == 1 {
			p.Title = "The 1 device is backed up."
		}
	default:
		parts := []string{strconv.Itoa(crit) + " critical", count(warn, "warning", "warnings")}
		if missing > 0 {
			parts = append(parts, count(missing, "customer missing", "customers missing"))
		}
		if len(p.Banners) > 0 {
			parts = append(parts, count(len(p.Banners), "console condition", "console conditions"))
		}
		if !v.Complete {
			parts = append(parts, "data incomplete")
		}
		summary = strings.Join(parts, ", ")
		p.Title = "Not all clear: " + summary + "."
	}
	fine := p.Checked - deviceLines
	if fine < 0 {
		fine = 0
	}
	p.Totals = count(p.Checked, "device", "devices") + ": " + strconv.Itoa(crit-missing) + " critical, " + count(warn, "warning", "warnings") +
		", " + strconv.Itoa(other) + " parked or deferred, " + strconv.Itoa(fine) + " fine. " +
		count(len(v.Customers), "customer", "customers") + ", " + strconv.Itoa(missing) + " missing."
	var text, html bytes.Buffer
	if err := digestTextTmpl.Execute(&text, p); err != nil {
		return Content{}, err
	}
	if err := digestHTMLTmpl.Execute(&html, p); err != nil {
		return Content{}, err
	}
	return Content{Kind: mail.KindDigest, Subject: "[CryoBackup] Backup digest " + p.Date + ": " + summary, Text: text.String(), HTML: html.String()}, nil
}
