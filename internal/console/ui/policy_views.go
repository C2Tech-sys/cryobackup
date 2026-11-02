package ui

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// View types and builders of the policy screens (S24, S25, S17 and the device
// Policy tab, ticket M2-W2A). The controls are described once, in
// policy_fields.go; this file turns a form's values into what the templates draw.

// PolCheck is one checkbox of a group.
type PolCheck struct {
	Name, Value, Label string
	Checked            bool
}

// PolRule is one retention count.
type PolRule struct{ Key, Label, Name, Value string }

// PolPresetOpt is a retention preset card.
type PolPresetOpt struct {
	Value, Label, Detail string
	Selected             bool
}

// PolRetention is the composite retention control.
type PolRetention struct {
	ModeName, Mode string
	PresetName     string
	Presets        []PolPresetOpt
	Rules          []PolRule
	KeepWithinName string
	KeepWithin     string
	KeepTagName    string
	KeepTag        string
}

// WinRow is one weekday of a window grid.
type WinRow struct {
	Day   string
	Hours string
	Cells []bool
}

// WinGrid is the server's reading of a window (components 8.1): the 7x24 grid and
// the normalised text, or the parser's error with its position.
type WinGrid struct {
	Valid     bool
	Blank     bool
	Rows      []WinRow
	Summary   string
	Canonical string
	Err       string
	Echo      string // the text, for the caret line
	Caret     string // spaces up to the failing character, then ^
	// HeadHours labels every sixth column of the grid.
	HeadHours []string
}

var dayShort = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// windowGrid reads a window string with the policy package's parser.
func windowGrid(text string, optional bool) *WinGrid {
	g := &WinGrid{Echo: text}
	if strings.TrimSpace(text) == "" {
		g.Blank = true
		if !optional {
			g.Err = "A window is required. Use */0-24 for always."
		}
		return g
	}
	w, err := policy.ParseWindow(text)
	if err != nil {
		g.Err = windowMessage(err)
		var pe *policy.ParseError
		if errors.As(err, &pe) {
			g.Caret = strings.Repeat(" ", pe.Offset) + "^"
		}
		return g
	}
	g.Valid = true
	for h := 0; h < 24; h++ {
		lbl := ""
		if h%6 == 0 {
			lbl = fmt.Sprintf("%02d", h)
		}
		g.HeadHours = append(g.HeadHours, lbl)
	}
	grid := w.Grid()
	minutes := w.Minutes()
	open := 0
	for d := 0; d < 7; d++ {
		row := WinRow{Day: dayShort[d], Cells: make([]bool, 24)}
		n := 0
		for h := 0; h < 24; h++ {
			row.Cells[h] = grid[d][h]
			if grid[d][h] {
				n++
			}
		}
		mins := 0
		for m := 0; m < 1440; m++ {
			if minutes[d][m] {
				mins++
			}
		}
		open += mins
		row.Hours = fmt.Sprintf("%s h", trimHours(float64(mins)/60))
		g.Rows = append(g.Rows, row)
	}
	g.Canonical = w.String()
	switch {
	case w.IsAlways():
		g.Summary = "*/0-24 (always)"
	case open == 0:
		g.Summary = "No hours are allowed: backups will never start."
	default:
		g.Summary = fmt.Sprintf("%s hours per week allowed", trimHours(float64(open)/60))
	}
	return g
}

func trimHours(h float64) string {
	s := fmt.Sprintf("%.1f", h)
	return strings.TrimSuffix(s, ".0")
}

// PolOverride is the override control of one setting on the device form.
type PolOverride struct {
	Name       string
	On         bool
	Source     string // default | policy | override
	SourceText string
}

// PolFieldView is one control, ready to draw.
type PolFieldView struct {
	Path, Name, ID string
	Label, Help    string
	Unit           string
	Kind           string
	Value          string
	Lines          string
	Checked        bool
	Opts           []Option
	Checks         []PolCheck
	Custom         string
	CustomName     string
	Placeholder    string
	Optional       bool
	Err            string
	Adv            bool
	Mono           bool
	Ret            *PolRetention
	Grid           *WinGrid
	Ov             *PolOverride
	// Flag says the stored value of this control was not usable and the default is shown (repair editor, M2-W3A).
	Flag string
}

// PolSectionView is one section of the editor.
type PolSectionView struct {
	Key, Title, Lead string
	ID               string
	Fields           []PolFieldView
	Adv              []PolFieldView
	Errs             int
	Overrides        int
	// Flags counts the stored parts of this section that were not usable (repair editor);
	// FlagNotes names those that have no control of their own.
	Flags     int
	FlagNotes []string
	// Open expands the section: it has an error, an override, or is the first.
	Open bool
	// Baseline lists the published exclusion baseline (selection section, read-only).
	Baseline []string
}

// ovCtx carries what the override form knows per setting.
type ovCtx struct {
	On         map[string]bool   // unit path -> checked
	Src        map[string]string // document path -> source
	PolicyText string
}

func (o *ovCtx) unitSource(unit string) string {
	best := "default"
	for p, s := range o.Src {
		if p == unit || strings.HasPrefix(p, unit+".") {
			if s == store.SourceOverride {
				return s
			}
			if s == store.SourcePolicy {
				best = s
			}
		}
	}
	return best
}

func (o *ovCtx) sourceText(src string) string {
	switch src {
	case store.SourceOverride:
		return "this device's override"
	case store.SourcePolicy:
		return o.PolicyText
	}
	return "the default policy"
}

func fieldID(path string) string { return "pf-" + strings.ReplaceAll(path, ".", "-") }

func buildField(f pfield, vals url.Values, errs map[string]string, ov *ovCtx) PolFieldView {
	v := PolFieldView{Path: f.Path, Name: f.name(), ID: fieldID(f.Path), Label: f.Label, Help: f.Help, Unit: f.Unit, Kind: string(f.Kind),
		Placeholder: f.Placeholder, Optional: f.Optional, Err: errs[f.Path], Adv: f.Adv}
	name := f.name()
	switch f.Kind {
	case pInt, pFloat, pRate:
		v.Value = vals.Get(name)
		v.Mono = true
	case pText:
		v.Value = vals.Get(name)
		v.Mono = true
	case pBool:
		v.Checked = vals.Get(name) != ""
	case pSelect:
		for _, o := range f.Opts {
			v.Opts = append(v.Opts, Option{Value: o.Value, Label: o.Label, Selected: o.Value == vals.Get(name)})
		}
	case pTri:
		for _, o := range []pOpt{{"inherit", "Inherit"}, {"yes", "Yes"}, {"no", "No"}} {
			v.Opts = append(v.Opts, Option{Value: o.Value, Label: o.Label, Selected: o.Value == vals.Get(name)})
		}
	case pWindow:
		v.Value = vals.Get(name)
		v.Mono = true
		v.Grid = windowGrid(v.Value, f.Optional)
	case pLines, pTW:
		v.Lines = vals.Get(name)
		v.Mono = true
	case pPresets:
		have := map[string]bool{}
		for _, p := range vals[name] {
			have[p] = true
		}
		for _, o := range presetLabels {
			v.Checks = append(v.Checks, PolCheck{Name: name, Value: o.Value, Label: o.Label, Checked: have[o.Value]})
		}
	case pExcl:
		have := map[string]bool{}
		for _, p := range vals[name+".presets"] {
			have[p] = true
		}
		for _, o := range exclusionLabels {
			v.Checks = append(v.Checks, PolCheck{Name: name + ".presets", Value: o.Value, Label: o.Label, Checked: have[o.Value]})
		}
		v.CustomName, v.Custom = name+".custom", vals.Get(name+".custom")
	case pRet:
		r := &PolRetention{ModeName: "f.retention.mode", Mode: vals.Get("f.retention.mode"), PresetName: "f.retention.preset",
			KeepWithinName: "f.retention.keep_within", KeepWithin: vals.Get("f.retention.keep_within"),
			KeepTagName: "f.retention.keep_tag", KeepTag: vals.Get("f.retention.keep_tag")}
		if r.Mode != "rules" {
			r.Mode = "preset"
		}
		names := policy.PresetNames()
		sort.Strings(names)
		for _, n := range names {
			rr := policy.RetentionPresets[n]
			r.Presets = append(r.Presets, PolPresetOpt{Value: n, Label: presetTitle(n), Detail: presetDetail(rr), Selected: vals.Get("f.retention.preset") == n})
		}
		for _, k := range retentionKeep {
			r.Rules = append(r.Rules, PolRule{Key: k.Key, Label: k.Label, Name: "f.retention.rules." + k.Key, Value: vals.Get("f.retention.rules." + k.Key)})
		}
		v.Ret = r
	}
	if ov != nil {
		src := ov.unitSource(f.Path)
		v.Ov = &PolOverride{Name: "o." + f.Path, On: ov.On[f.Path], Source: src, SourceText: ov.sourceText(src)}
	}
	return v
}

func presetTitle(n string) string {
	switch n {
	case "30_days":
		return "30 days"
	case "90_days":
		return "90 days"
	case "1_year_gfs":
		return "1 year (grandfather-father-son)"
	case "7_years":
		return "7 years"
	}
	return n
}

func presetDetail(r policy.Rules) string {
	var parts []string
	add := func(n int, what string) {
		switch {
		case n == -1:
			parts = append(parts, "every "+what)
		case n > 0:
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(r.KeepLast, "latest")
	add(r.KeepHourly, "hourly")
	add(r.KeepDaily, "daily")
	add(r.KeepWeekly, "weekly")
	add(r.KeepMonthly, "monthly")
	add(r.KeepYearly, "yearly")
	out := "everything from the last " + strings.NewReplacer("d", " days", "y", " years", "m", " months", "h", " hours").Replace(r.KeepWithin)
	if len(parts) > 0 {
		out += ", then " + strings.Join(parts, ", ")
	}
	return out
}

// buildSections draws every section from the form values. errs is keyed by
// catalogue path. ov is non-nil on the device override form.
func buildSections(vals url.Values, errs map[string]string, ov *ovCtx) []PolSectionView {
	out := make([]PolSectionView, 0, len(policySections))
	for i, s := range policySections {
		sv := PolSectionView{Key: s.Key, Title: s.Title, Lead: s.Lead, ID: "sec-" + s.Key}
		for _, f := range s.Fields {
			fv := buildField(f, vals, errs, ov)
			if fv.Err != "" {
				sv.Errs++
			}
			if fv.Ov != nil && fv.Ov.On {
				sv.Overrides++
			}
			if f.Adv {
				sv.Adv = append(sv.Adv, fv)
			} else {
				sv.Fields = append(sv.Fields, fv)
			}
		}
		if s.Key == "selection" && ov == nil {
			sv.Baseline = baselineText()
		}
		sv.Open = sv.Errs > 0 || sv.Overrides > 0 || (i == 0 && ov == nil)
		out = append(out, sv)
	}
	return out
}

// baselineText is the published exclusion baseline for display: the tokens that
// the exclude file expands are written the way an operator reads them.
func baselineText() []string {
	out := make([]string, 0, len(policy.Baseline))
	r := strings.NewReplacer(policy.TokenProfile, `<profile>`, policy.TokenSystemDrive, `<system drive>`)
	for _, e := range policy.Baseline {
		out = append(out, r.Replace(e))
	}
	return out
}

// PolErr is one line of an error summary.
type PolErr struct{ Anchor, Label, Msg string }

// polSummary lists every error of a form, in catalogue order, for the top of the page.
func polSummary(errs map[string]string, extra []PolErr) []PolErr {
	var out []PolErr
	seen := map[string]bool{}
	for _, s := range policySections {
		for _, f := range s.Fields {
			if m := errs[f.Path]; m != "" {
				out = append(out, PolErr{Anchor: fieldID(f.Path), Label: f.Label, Msg: m})
				seen[f.Path] = true
			}
		}
	}
	keys := make([]string, 0)
	for k := range errs {
		if !seen[k] && errs[k] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, PolErr{Anchor: "pol-identity", Label: labelOf(k), Msg: errs[k]})
	}
	return append(out, extra...)
}

// ---- review (blast radius) ----

// PolChangeRow is one changed field of a review.
type PolChangeRow struct {
	Section, Label, Old, New string
}

// PolImpactCust is a customer line of a review.
type PolImpactCust struct {
	Name, Href string
	Devices    int
	Changing   int
}

// PolReview is the blast-radius step before a save, an assignment or a delete.
type PolReview struct {
	Kind       string // update | assign | delete
	Headline   string
	Lead       string
	Population int
	Devices    int
	Held       int
	Changes    []PolChangeRow
	Customers  []PolImpactCust
	Blockers   []PolImpactCust
	Unchanged  bool
	Token      string
	Expires    string
	Confirm    string // button label
	Name       *PolChangeRow
	// Dropped lists the stored parts a repair does not carry (M2-W3A).
	Dropped []PolDropped
}

func changeRows(ch []store.FieldChange) []PolChangeRow {
	out := make([]PolChangeRow, 0, len(ch))
	for _, c := range ch {
		sec, _, _ := strings.Cut(c.Path, ".")
		out = append(out, PolChangeRow{Section: sectionTitle(sec), Label: labelOf(c.Path), Old: fmtValue(c.Path, c.Old), New: fmtValue(c.Path, c.New)})
	}
	return out
}

func impactCusts(in []store.ImpactCustomer) []PolImpactCust {
	out := make([]PolImpactCust, len(in))
	for i, c := range in {
		out[i] = PolImpactCust{Name: c.Name, Href: "/customers/" + c.ID + "/policy", Devices: c.Devices, Changing: c.Changing}
	}
	return out
}
