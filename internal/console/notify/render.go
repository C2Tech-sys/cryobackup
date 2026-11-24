package notify

import (
	"bytes"
	_ "embed"
	"errors"
	htmltemplate "html/template"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Options are what a message needs besides the view and the episodes.
type Options struct {
	// Loc is the operator's display zone; nil means UTC. Every time in a message is shown in it,
	// with the zone named.
	Loc *time.Location
	// ConsoleURL is the address an operator types to open the console (config admin_url, never public_url: that is the agent API). A message links to it when it is
	// an http or https URL; when empty, the message carries no link. A value that is not one fails the alert and
	// recovery renderers with ErrConsoleLink (P21-T7); the digest and the test message still leave it out.
	ConsoleURL string
	// DigestDate is the local date ("2006-01-02") a digest is for, shown in its subject and header;
	// empty means the date of the view. A retried digest keeps the date of its key. The alert and
	// recovery renderers do not read it.
	DigestDate string
}

// Content is one rendered message: the same for every recipient of a batch. Inline is the images
// the HTML part shows by content id (the mark, P20-MD); they travel inside the message.
type Content struct {
	Kind    mail.Kind
	Subject string
	Text    string
	HTML    string
	Inline  []mail.Inline
}

// Message is c addressed to one recipient. From is left empty: the dispatcher fills it in.
func (c Content) Message(to string) mail.Message {
	return mail.Message{To: []string{to}, Subject: c.Subject, Text: c.Text, HTML: c.HTML, Kind: c.Kind, Inline: c.Inline}
}

// errNothingToRender is what a renderer returns for an empty batch: a message about nothing is a bug.
var errNothingToRender = errors.New("notify: no episode to render")

// ErrConsoleLink is what the alert and recovery renderers return when they are given a console link
// (Options.ConsoleURL, from config admin_url) that they cannot render. config.ParseAdminURL accepts only
// values these renderers render (a test holds them together), so this is a bug, and it fails the send
// loudly instead of mailing a message without its button. The text never carries the link.
var ErrConsoleLink = errors.New("notify: console_link: the configured console link cannot be rendered")

// criticalAlarms are the alarm types that mail at once (ADR 0020 decision 4), in the order a
// message lists them inside a customer: the §4.2 precedence of internal/state, MISSING first.
var criticalAlarms = []string{
	store.AlarmMissing, string(state.StateUnknown), string(state.StateIdentityConflict), string(state.StateBlank),
	string(state.StateStuck), string(state.StateFailed), string(state.StateUnseen), string(state.StateStale),
}

// IsCritical reports whether an episode's alarm type mails at once (ADR 0020 decision 4); the
// alarm-level types do not.
func IsCritical(alarmType string) bool { return rankOf(alarmType) < len(criticalAlarms) }

// rankOf orders alarm types inside a customer: the critical ones in criticalAlarms order, then the
// rest in the engine's precedence.
func rankOf(alarmType string) int {
	for i, a := range criticalAlarms {
		if a == alarmType {
			return i
		}
	}
	for i, s := range state.Precedence() {
		if string(s) == alarmType {
			return len(criticalAlarms) + i
		}
	}
	return len(criticalAlarms) + len(state.Precedence())
}

// classWord is the shape of an error class a message may show; anything else is left out.
var classWord = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// The dark colour tokens of web/static/tokens.css (ADR 0022: the messages take the console's dark
// look). Each value equals the dark token of that name; a soft token, which tokens.css gives as an
// rgba colour, is that colour composited over "surface", because a mail client takes only a 6-digit
// hex value in a bgcolor attribute. TestDarkTokensMatchTokensCSS fails when they drift. Every colour
// is paired with a word: a client that recolours the message loses no meaning.
var tokens = map[string]string{
	"bg": "#0B1220", "surface": "#121C30", "surface-raised": "#182642", "border": "#243656",
	"text": "#E8F0FA", "text-muted": "#93A6C6", "accent": "#5CC9F5", "on-accent": "#06111F",
	"danger": "#FF7A7A", "warning": "#F5B547", "success": "#48D597",
	"accent-soft": "#1C344C", "danger-soft": "#33293A", "warning-soft": "#363434", "success-soft": "#1A363E",
}

func tok(name string) htmltemplate.CSS {
	if v, ok := tokens[name]; ok {
		return htmltemplate.CSS(v)
	}
	return htmltemplate.CSS(tokens["text"])
}

//go:embed templates/message.txt.tmpl
var textSource string

//go:embed templates/message.html.tmpl
var htmlSource string

var (
	textTmpl = texttemplate.Must(texttemplate.New("text").Parse(textSource))
	htmlTmpl = mdTemplate("html", htmlSource)
)

// line is one alarm of a message.
type line struct {
	Host  string // the device's host name; empty for a customer line (MISSING)
	State string // the alarm: FAILED, MISSING, ...
	Now   string // recovery only: the subject's state now
	Since string // when the alarm began, in the display zone
	Ended string // recovery only: when it ended
	Class string // the newest run's error class word, when there is one
	rank  int
}

// group is one customer's lines.
type group struct {
	Customer string
	Lines    []line
	missing  bool
}

// Muted is false: a suspended customer is never alerted. The shell's customer heading (shared with
// the digest, whose groups carry the flag) asks for it.
func (group) Muted() bool { return false }

// page is what both templates render.
type page struct {
	Recovery   bool
	Title      string
	Polled     string
	Groups     []group
	ConsoleURL string
}

// RenderAlert renders the alert for a batch of critical episodes, as the state is in v. One
// message covers the whole batch, grouped by customer (customers with a MISSING alarm first, then
// by name), each customer's lines in precedence order then by host name.
func RenderAlert(v state.FleetView, eps []store.Episode, o Options) (Content, error) {
	return render(v, eps, o, false)
}

// RenderRecovery renders the recovery notice for a batch of closed episodes: what each alarm was,
// when it began and ended, and the subject's state now.
func RenderRecovery(v state.FleetView, eps []store.Episode, o Options) (Content, error) {
	return render(v, eps, o, true)
}

func render(v state.FleetView, eps []store.Episode, o Options, recovery bool) (Content, error) {
	if len(eps) == 0 {
		return Content{}, errNothingToRender
	}
	loc := o.Loc
	if loc == nil {
		loc = time.UTC
	}
	devices := make(map[string]*state.DeviceState, len(v.Devices))
	for i := range v.Devices {
		devices[v.Devices[i].ID] = &v.Devices[i]
	}
	customers := make(map[string]string, len(v.Customers))
	for _, c := range v.Customers {
		customers[c.ID] = c.Name
	}
	byCustomer := map[string]*group{}
	var order []*group
	for _, e := range eps {
		l := line{State: e.AlarmType, Since: when(e.StateSince, loc), rank: rankOf(e.AlarmType)}
		custID := e.CustomerID
		if e.SubjectType == store.EpisodeDevice {
			l.Host = "device " + e.SubjectID // a device the view no longer holds keeps a name
			if d := devices[e.SubjectID]; d != nil {
				if d.Hostname != "" {
					l.Host = d.Hostname
				}
				if custID == "" {
					custID = d.CustomerID
				}
				switch {
				case recovery:
					l.Now = string(d.Primary) // the class of a run that is no longer the cause would mislead
				case classWord.MatchString(d.Detail.ErrorClass):
					l.Class = d.Detail.ErrorClass
				}
			}
		}
		if recovery {
			l.Ended = "an unknown time"
			if e.ClosedAt != nil {
				l.Ended = when(*e.ClosedAt, loc)
			}
		}
		g := byCustomer[custID]
		if g == nil {
			name := customers[custID]
			if name == "" {
				name = "Customer " + custID
			}
			g = &group{Customer: name}
			byCustomer[custID] = g
			order = append(order, g)
		}
		g.missing = g.missing || e.AlarmType == store.AlarmMissing
		g.Lines = append(g.Lines, l)
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].missing != order[j].missing {
			return order[i].missing
		}
		return strings.ToLower(order[i].Customer) < strings.ToLower(order[j].Customer)
	})
	p := page{Recovery: recovery, Polled: when(v.ComputedAt, loc), ConsoleURL: consoleURL(o.ConsoleURL)}
	if p.ConsoleURL == "" && strings.TrimSpace(o.ConsoleURL) != "" {
		return Content{}, ErrConsoleLink // a link was given and would be dropped: never silently
	}
	for _, g := range order {
		sort.SliceStable(g.Lines, func(i, j int) bool {
			if g.Lines[i].rank != g.Lines[j].rank {
				return g.Lines[i].rank < g.Lines[j].rank
			}
			return strings.ToLower(g.Lines[i].Host) < strings.ToLower(g.Lines[j].Host)
		})
		p.Groups = append(p.Groups, *g)
	}
	n, c := len(eps), len(order)
	kind := mail.KindAlert
	if recovery {
		kind = mail.KindRecovery
		p.Title = count(n, "alarm", "alarms") + " resolved at " + count(c, "customer", "customers")
	} else {
		p.Title = count(n, "new alarm", "new alarms") + " at " + count(c, "customer", "customers")
	}
	var text, html bytes.Buffer
	if err := textTmpl.Execute(&text, p); err != nil {
		return Content{}, err
	}
	if err := htmlTmpl.Execute(&html, p); err != nil {
		return Content{}, err
	}
	// The subject is counts only: fixed words and two numbers fit the sender's limit at any fleet size,
	// and no customer or host name rides in a header.
	return Content{Kind: kind, Subject: "[CryoBackup] " + p.Title, Text: text.String(), HTML: html.String(), Inline: markInline()}, nil
}

// when is t in the display zone with the zone named; "unknown" for a zero time.
func when(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.In(loc).Format("Mon 2 Jan 2006, 3:04 PM MST")
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// consoleURL returns raw when it is an absolute http or https URL with a host, else "". Every value
// config.ParseAdminURL accepts passes (TestEveryValidatedAdminURLRendersALink); the alert and recovery
// renderers turn a given link that does not pass into ErrConsoleLink.
func consoleURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(raw, " \r\n\t") {
		return ""
	}
	return u.String()
}
