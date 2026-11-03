package ui

// Time zone choices (M2-FIX-C, F12). The display zone is picked from an
// embedded, grouped list of IANA names instead of typed. The standard library
// cannot list the zone database, so the list is curated here: United States
// first, then UTC, then the common world zones. The form still posts a plain
// zone name and the server validates it exactly as before (store.ValidZoneName
// -> time.LoadLocation); the list is a convenience, never the rule. A stored
// value that is not in the list is added as its own first group, selected, so
// opening a form and saving it never changes a zone silently.

// ZoneOption is one <option>.
type ZoneOption struct {
	Value    string
	Label    string
	Selected bool
}

// ZoneGroup is one <optgroup>; a group with an empty Label renders its options bare.
type ZoneGroup struct {
	Label   string
	Options []ZoneOption
}

type zoneDef struct{ name, note string }

var zoneCatalogue = []struct {
	group string
	zones []zoneDef
}{
	{"United States", []zoneDef{
		{"America/New_York", "Eastern"}, {"America/Chicago", "Central"}, {"America/Denver", "Mountain"},
		{"America/Phoenix", "Arizona, no daylight saving"}, {"America/Los_Angeles", "Pacific"},
		{"America/Anchorage", "Alaska"}, {"Pacific/Honolulu", "Hawaii"},
	}},
	{"Universal time", []zoneDef{{"UTC", ""}}},
	{"Canada and Latin America", []zoneDef{
		{"America/Halifax", "Atlantic"}, {"America/St_Johns", "Newfoundland"}, {"America/Toronto", ""}, {"America/Vancouver", ""},
		{"America/Mexico_City", ""}, {"America/Bogota", ""}, {"America/Lima", ""}, {"America/Santiago", ""},
		{"America/Argentina/Buenos_Aires", ""}, {"America/Sao_Paulo", ""},
	}},
	{"Europe", []zoneDef{
		{"Atlantic/Reykjavik", ""}, {"Europe/London", ""}, {"Europe/Dublin", ""}, {"Europe/Lisbon", ""}, {"Europe/Paris", ""},
		{"Europe/Berlin", ""}, {"Europe/Madrid", ""}, {"Europe/Rome", ""}, {"Europe/Amsterdam", ""}, {"Europe/Zurich", ""},
		{"Europe/Stockholm", ""}, {"Europe/Warsaw", ""}, {"Europe/Athens", ""}, {"Europe/Helsinki", ""}, {"Europe/Istanbul", ""},
		{"Europe/Moscow", ""},
	}},
	{"Africa and Middle East", []zoneDef{
		{"Africa/Cairo", ""}, {"Africa/Johannesburg", ""}, {"Africa/Lagos", ""}, {"Africa/Nairobi", ""},
		{"Asia/Dubai", ""}, {"Asia/Tehran", ""},
	}},
	{"Asia", []zoneDef{
		{"Asia/Karachi", ""}, {"Asia/Kolkata", ""}, {"Asia/Dhaka", ""}, {"Asia/Bangkok", ""}, {"Asia/Jakarta", ""},
		{"Asia/Singapore", ""}, {"Asia/Hong_Kong", ""}, {"Asia/Shanghai", ""}, {"Asia/Manila", ""}, {"Asia/Seoul", ""}, {"Asia/Tokyo", ""},
	}},
	{"Australia and Pacific", []zoneDef{
		{"Australia/Perth", ""}, {"Australia/Adelaide", ""}, {"Australia/Sydney", ""}, {"Pacific/Auckland", ""},
	}},
}

// zoneNames is every name the curated list offers, in display order.
func zoneNames() []string {
	var out []string
	for _, g := range zoneCatalogue {
		for _, z := range g.zones {
			out = append(out, z.name)
		}
	}
	return out
}

// zoneGroups builds the select for the current value. With allowEmpty the first
// choice is the explicit "use the console's zone" (value ""), selected when
// current is empty. A current value missing from the list is shown first,
// selected, labelled as the saved value.
func zoneGroups(current string, allowEmpty bool) []ZoneGroup {
	var out []ZoneGroup
	if allowEmpty {
		out = append(out, ZoneGroup{Options: []ZoneOption{{Value: "", Label: "Use the console's zone", Selected: current == ""}}})
	}
	known := false
	for _, n := range zoneNames() {
		if n == current {
			known = true
			break
		}
	}
	if current != "" && !known {
		out = append(out, ZoneGroup{Label: "Saved value", Options: []ZoneOption{{Value: current, Label: current, Selected: true}}})
	}
	for _, g := range zoneCatalogue {
		grp := ZoneGroup{Label: g.group}
		for _, z := range g.zones {
			label := z.name
			if z.note != "" {
				label += " (" + z.note + ")"
			}
			grp.Options = append(grp.Options, ZoneOption{Value: z.name, Label: label, Selected: z.name == current})
		}
		out = append(out, grp)
	}
	return out
}

// ZoneGroups is the settings page's zone select.
func (d SettingsPage) ZoneGroups() []ZoneGroup { return zoneGroups(d.ZoneIn, false) }

// ZoneGroups is the customer form's optional zone select.
func (f CustForm) ZoneGroups() []ZoneGroup { return zoneGroups(f.TZ, true) }
