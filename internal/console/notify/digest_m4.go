package notify

import (
	"bytes"
	_ "embed"
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
	digestHTMLTmpl = mdTemplate("digest-html", digestHTMLSource)
)

// digestLine is one listed device or MISSING customer.
type digestLine struct {
	Host  string // empty for a MISSING customer
	State string // the state word shown
	// Colour is the token of the line's chip: its section's colour, except for a PAUSED device. A
	// PAUSED chip is always accent, as in the console, in whichever section the line sits: a pause is
	// the operator's own decision, not a warning, and the state word is beside the colour. (Accent is
	// the current choice; the operator has not ruled on this colour.)
	Colour string
	Also   string // the other alarming flags, comma separated
	Since  string
	Ack    string // ACKNOWLEDGED or SNOOZED; empty when neither
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
	Word   string // the band's word in the plain-text part
	Colour string // a token name: the colour of the band's chips (but see digestLine.Colour), beside the state word
	Groups []digestGroup
	Count  int
}

// digestBanner is one console condition.
type digestBanner struct {
	Word, Colour, Text string
}

// digestTile is one counter of the HTML part (P20-MD): a number of the totals line, its label, and a
// colour token that is never its only meaning.
type digestTile struct {
	N      int
	Label  string
	Colour string
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
	// Level is the colour token of the headline strip of the HTML part: danger when a critical line,
	// a missing customer or a critical console condition is listed; success only when the headline
	// is an all-clear with every device backed up; warning otherwise.
	Level string
	// Tiles are the four counters of the HTML part: the same numbers the totals line is built from.
	Tiles []digestTile
}

// digestDevices counts the devices a digest speaks for: every device of the view that is neither
// retired nor left out by its policy.
func digestDevices(v state.FleetView) int {
	n := 0
	for _, c := range digestByState(v) {
		n += c
	}
	return n
}

// digestByState counts the devices a digest speaks for by their primary state.
func digestByState(v state.FleetView) map[state.State]int {
	m := map[state.State]int{}
	for i := range v.Devices {
		if d := &v.Devices[i]; d.Primary != state.StateRetired && !d.DigestExcluded {
			m[d.Primary]++
		}
	}
	return m
}

// digestBackedUp splits the OK devices a digest speaks for (primary state OK: in none of the engine's
// exception states, which are every other state of state.Precedence, each counted under its own
// name by digestByState) into those with a current backup (state.DeviceState.CurrentBackup, the
// engine's freshness verdict): backed up; and those without one, notCurrent. The engine does not
// make an OK device without a current backup (no success is PENDING_FIRST or BLANK, an old one
// STALE), but if it ever did the device would be counted and said, never hidden in the backed-up
// number. With digestByState this counts every device exactly once.
func digestBackedUp(v state.FleetView) (backedUp, notCurrent int) {
	for i := range v.Devices {
		if d := &v.Devices[i]; d.Primary == state.StateOK && !d.DigestExcluded {
			if d.CurrentBackup {
				backedUp++
			} else {
				notCurrent++
			}
		}
	}
	return backedUp, notCurrent
}

// stateWords is how a count of devices in a state reads ("1 paused", "3 awaiting first backup").
func stateWords(s state.State) string {
	switch s {
	case state.StatePaused:
		return "paused"
	case state.StatePendingFirst:
		return "awaiting first backup"
	case state.StateMissed:
		return "missed a scheduled run"
	case state.StateDeferred:
		return "deferred"
	case state.StateOutdated:
		return "on an outdated agent"
	case state.StateIdentityConflict:
		return "in identity conflict"
	case state.StateEscrowUnverified:
		return "with an unverified escrow"
	case state.StateMaintFailed:
		return "with failed maintenance"
	}
	return strings.ToLower(string(s))
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
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
	date := v.ComputedAt.In(loc)
	if t, err := time.ParseInLocation("2006-01-02", o.DigestDate, loc); err == nil {
		date = t
	}
	p := digestPage{Date: date.Format("Mon 2 Jan"), Polled: when(v.ComputedAt, loc), Checked: digestDevices(v),
		ConsoleURL: consoleURL(o.ConsoleURL)}
	for _, b := range d.Banners {
		db := digestBanner{Word: "WARNING", Colour: "warning", Text: b.Text}
		if b.Severity >= state.SeverityCritical {
			db = digestBanner{Word: "CRITICAL", Colour: "danger", Text: b.Text}
		}
		p.Banners = append(p.Banners, db)
	}
	p.Sections = []digestSection{
		{Title: "Critical", Word: "CRITICAL", Colour: "danger"},
		{Title: "Warnings", Word: "WARNING", Colour: "warning"},
		{Title: "Parked and long-deferred devices", Word: "NOTICE", Colour: "text-muted"},
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
		dl := digestLine{Host: l.Hostname, State: string(l.State), Since: when(l.Since, loc), Ack: ackOf(l, marks), Colour: p.Sections[s].Colour}
		if l.State == state.StatePaused { // accent in every section: see digestLine.Colour
			dl.Colour = "accent"
		}
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
	critDevices, warn, other := p.Sections[sectionCritical].Count-missing, p.Sections[sectionWarning].Count, p.Sections[sectionOther].Count
	byState := digestByState(v)
	// The totals partition the devices: backed up (OK with a current backup), then every exception
	// state of the engine's list under its own name, whatever the age of the device's last success,
	// then an OK device without a current backup. Each device is counted once, so the numbers sum to
	// p.Checked, and "backed up" in the headline is this same number.
	backedUp, notCurrent := digestBackedUp(v)
	var rest []string // the devices that are not backed up: never inside the backed-up number
	for _, st := range state.Precedence() {
		if n := byState[st]; n > 0 && st != state.StateOK {
			rest = append(rest, strconv.Itoa(n)+" "+stateWords(st))
		}
	}
	if notCurrent > 0 {
		rest = append(rest, strconv.Itoa(notCurrent)+" without a current backup")
	}
	// anyCurrent is the engine's freshness verdict over the devices the digest speaks for, whatever
	// their state. The headline says "No devices backed up" only when it is false (P20-W2BF D7): a
	// device in an exception with fresh backups (an outdated agent) never reads as unprotected.
	anyCurrent := false
	for i := range v.Devices {
		dv := &v.Devices[i]
		anyCurrent = anyCurrent || dv.Primary != state.StateRetired && !dv.DigestExcluded && dv.CurrentBackup
	}
	var left []string // what the policy leaves out: always said, so it cannot hide in an all-clear
	if d.ExcludedCustomers > 0 {
		left = append(left, count(d.ExcludedCustomers, "customer", "customers")+" not included by policy")
	}
	if d.ExcludedDevices > 0 {
		left = append(left, count(d.ExcludedDevices, "device", "devices")+" not included by policy")
	}
	// The day is all clear on the INCLUDED devices: no console condition, no critical or warning
	// line (excluded devices make none), and complete data, unless what made it incomplete is an
	// excluded device the engine could not read. Excluded devices are named only in left.
	excludedUnknown := false
	for i := range v.Devices {
		excludedUnknown = excludedUnknown || v.Devices[i].DigestExcluded && v.Devices[i].Primary == state.StateUnknown
	}
	complete := v.Complete || excludedUnknown && len(d.Banners) == 0
	allClear := len(d.Banners) == 0 && complete && critDevices+missing == 0 && warn == 0
	var summary string
	leftSaid := false
	p.Level = "warning"
	switch {
	case !allClear:
		if critDevices+missing > 0 {
			p.Level = "danger"
		}
		for _, b := range p.Banners {
			if b.Colour == "danger" {
				p.Level = "danger"
			}
		}
		parts := []string{strconv.Itoa(critDevices) + " critical", count(warn, "warning", "warnings")}
		if missing > 0 {
			parts = append(parts, count(missing, "customer missing", "customers missing"))
		}
		if len(p.Banners) > 0 {
			parts = append(parts, count(len(p.Banners), "console condition", "console conditions"))
		}
		if !complete {
			parts = append(parts, "data incomplete")
		}
		summary = strings.Join(parts, ", ")
		p.Title = "Not all clear: " + summary + "."
	case p.Checked == 0 && len(left) == 0:
		summary, p.Title = "no devices enrolled yet", "No devices enrolled yet: nothing is being backed up."
	case p.Checked == 0:
		summary = "no devices included, " + strings.Join(left, ", ")
		p.Title, leftSaid = "No devices included in this digest: "+strings.Join(left, ", ")+".", true
	case backedUp == 0 && !anyCurrent: // the engine says no device has a current backup: never "all clear"
		summary, p.Title = "no devices backed up", "No devices backed up."
		if len(rest) > 0 {
			summary += ", " + strings.Join(rest, ", ")
			p.Title = "No devices backed up: " + strings.Join(rest, ", ") + "."
		}
	case backedUp == 0: // every device is in an exception, some with a current backup: lead with the exceptions
		need := count(p.Checked, "device needs attention", "devices need attention")
		summary = need + ", " + strings.Join(rest, ", ")
		p.Title = need + ": " + strings.Join(rest, ", ") + "."
	case len(rest) > 0: // an exception (a device in another state than OK): never "all clear"
		summary = strconv.Itoa(backedUp) + " of " + strconv.Itoa(p.Checked) + " devices backed up, " + strings.Join(rest, ", ")
		p.Title = strconv.Itoa(backedUp) + " of " + strconv.Itoa(p.Checked) + " devices backed up. " + upperFirst(strings.Join(rest, ", ")) + "."
	case len(left) == 0 && backedUp == p.Checked:
		p.Level = "success"
		summary = "all clear, " + count(p.Checked, "device", "devices") + " backed up"
		p.Title = "All " + strconv.Itoa(p.Checked) + " devices backed up."
		if p.Checked == 1 {
			p.Title = "The 1 device is backed up."
		}
	default:
		p.Level = "success"
		summary = "all clear, " + strconv.Itoa(backedUp) + " of " + strconv.Itoa(p.Checked) + " devices backed up"
		p.Title = "All clear: " + strconv.Itoa(backedUp) + " of " + strconv.Itoa(p.Checked) + " devices backed up."
	}
	if len(left) > 0 && !leftSaid {
		summary += ", " + strings.Join(left, ", ")
		p.Title += " " + upperFirst(strings.Join(left, ", ")) + "."
	}
	parts := []string{strconv.Itoa(backedUp) + " backed up"}
	parts = append(parts, rest...)
	p.Totals = count(p.Checked, "device", "devices") + ": " + strings.Join(parts, ", ") + ". Exceptions: " + strconv.Itoa(critDevices) + " critical, " +
		count(warn, "warning", "warnings") + ", " + strconv.Itoa(other) + " parked or deferred. " +
		count(len(v.Customers), "customer", "customers") + ", " + strconv.Itoa(missing) + " missing."
	if len(left) > 0 {
		p.Totals += " " + upperFirst(strings.Join(left, ", ")) + "."
	}
	// The tiles are the totals line again, from the same variables: backed up and devices are its
	// first two numbers, attention is its critical and warning device counts together (the device
	// lines of the Critical and Warnings sections; a missing customer is not a device), paused is
	// its "paused" count (zero when it lists none).
	p.Tiles = []digestTile{
		{N: backedUp, Label: "Backed up", Colour: "success"},
		{N: critDevices + warn, Label: "Attention", Colour: attentionColour(critDevices, warn)},
		{N: byState[state.StatePaused], Label: "Paused", Colour: "accent"},
		{N: p.Checked, Label: "Devices", Colour: "text"},
	}
	for _, i := range []int{0, 2} {
		if p.Tiles[i].N == 0 {
			p.Tiles[i].Colour = "text" // a zero is not a state: no colour
		}
	}
	var text, html bytes.Buffer
	if err := digestTextTmpl.Execute(&text, p); err != nil {
		return Content{}, err
	}
	if err := digestHTMLTmpl.Execute(&html, p); err != nil {
		return Content{}, err
	}
	return Content{Kind: mail.KindDigest, Subject: "[CryoBackup] Backup digest " + p.Date + ": " + summary, Text: text.String(), HTML: html.String(),
		Inline: markInline()}, nil
}

// attentionColour is the colour of the Attention tile's number: danger when a critical device is
// counted, warning when only warnings are, the neutral text colour at zero.
func attentionColour(critical, warnings int) string {
	switch {
	case critical > 0:
		return "danger"
	case warnings > 0:
		return "warning"
	}
	return "text"
}
