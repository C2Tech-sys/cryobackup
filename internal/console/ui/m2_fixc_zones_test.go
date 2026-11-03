package ui

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-FIX-C (F12): the display zone is a select of IANA names, not free text.

// selectOf returns the <select> element with the given name from a page.
func selectOf(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<select[^>]*name="` + regexp.QuoteMeta(name) + `".*?</select>`).FindString(body)
	if m == "" {
		t.Fatalf("no <select name=%q> on the page", name)
	}
	return m
}

func selectedOptions(sel string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<option value="([^"]*)" selected>`).FindAllStringSubmatch(sel, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestFixCZoneListIsValidAndCoversTheStoreList(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range zoneNames() {
		if seen[n] {
			t.Errorf("zone %s is listed twice", n)
		}
		seen[n] = true
		if !store.ValidZoneName(n) {
			t.Errorf("zone %s is offered but the server would refuse it", n)
		}
	}
	for _, n := range store.CommonZones {
		if !seen[n] {
			t.Errorf("zone %s was offered before and is missing from the select", n)
		}
	}
	g := zoneGroups("", false)
	if g[0].Label != "United States" || g[0].Options[0].Value != "America/New_York" {
		t.Errorf("US zones come first: %+v", g[0])
	}
}

func TestFixCZoneGroupsKeepTheCurrentValue(t *testing.T) {
	count := func(gs []ZoneGroup, v string) (n, sel int) {
		for _, g := range gs {
			for _, o := range g.Options {
				if o.Value == v {
					n++
					if o.Selected {
						sel++
					}
				}
			}
		}
		return
	}
	if n, sel := count(zoneGroups("Asia/Tokyo", false), "Asia/Tokyo"); n != 1 || sel != 1 {
		t.Errorf("a listed zone appears once, selected: %d/%d", n, sel)
	}
	if n, sel := count(zoneGroups("Asia/Kathmandu", false), "Asia/Kathmandu"); n != 1 || sel != 1 {
		t.Errorf("an unlisted current zone is added once, selected: %d/%d", n, sel)
	}
	if n, sel := count(zoneGroups("", true), ""); n != 1 || sel != 1 {
		t.Errorf("the empty choice is selected when there is no zone: %d/%d", n, sel)
	}
	if _, sel := count(zoneGroups("Europe/Paris", true), ""); sel != 0 {
		t.Error("the empty choice must not be selected when a zone is set")
	}
}

func TestFixCSettingsZoneIsASelect(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.page("/settings", 200)
	if regexp.MustCompile(`<input[^>]*name="display_timezone"`).MatchString(body) || strings.Contains(body, "<datalist") {
		t.Error("the display zone must not be a free-text input")
	}
	sel := selectOf(t, body, "display_timezone")
	mustContain(t, "select", sel, `<optgroup label="United States">`, `<option value="America/Chicago" selected>`, `<option value="UTC">`)
	if got := selectedOptions(sel); len(got) != 1 || got[0] != "America/Chicago" {
		t.Errorf("selected options: %v", got)
	}
	// A valid zone that is not in the curated list still saves, and is then shown selected.
	res, _ := e.post("/ui/settings", url.Values{"display_timezone": {"Asia/Kathmandu"}, "refresh_interval_s": {"30"}}, false)
	if res.StatusCode != 303 {
		t.Fatalf("save an unlisted zone: %d", res.StatusCode)
	}
	if got := selectedOptions(selectOf(t, e.page("/settings", 200), "display_timezone")); len(got) != 1 || got[0] != "Asia/Kathmandu" {
		t.Errorf("an unlisted saved zone must stay selected: %v", got)
	}
	// Server-side validation is unchanged: a hand-posted bad zone is refused.
	if res, _ := e.post("/ui/settings", url.Values{"display_timezone": {"Mars/Base"}, "refresh_interval_s": {"30"}}, false); res.StatusCode != 400 {
		t.Errorf("a bad zone must still be refused: %d", res.StatusCode)
	}
}

func TestFixCCustomerFormZoneIsASelect(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.page("/ui/customers/new", 200)
	if regexp.MustCompile(`<input[^>]*name="display_tz"`).MatchString(body) {
		t.Error("the customer zone must not be a free-text input")
	}
	sel := selectOf(t, body, "display_tz")
	if got := selectedOptions(sel); len(got) != 1 || got[0] != "" {
		t.Errorf("a new customer starts on the console zone (empty value): %v", got)
	}
	mustContain(t, "empty choice", sel, `<option value="" selected>Use the console&#39;s zone</option>`)
}
