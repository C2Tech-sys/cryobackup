package ui

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// A notes-only save of the Settings form must audit "notes" and nothing else:
// the form shows onboarding_until to the minute, so a stored time with seconds
// must survive a save that did not touch it.
func TestM2NotesOnlySaveDoesNotRewriteTheOnboardingWindow(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	harbor := e.customerID("Harbor Family Practice")
	until := time.Date(2026, 12, 1, 23, 0, 30, 0, time.UTC) // 5:00:30 PM in Chicago
	yes := true
	if _, _, err := e.st.UpdateCustomerAudited(context.Background(), store.ScopeAll(), harbor, store.CustomerPatch{Expected: &yes, OnboardingUntil: &until}, store.AuditActor{AdminID: "x", Username: "seed", Method: "api"}); err != nil {
		t.Fatal(err)
	}
	form := e.page("/customers/"+harbor+"/settings", 200)
	mustContain(t, "form shows the minute", form, `value="2026-12-01T17:00"`)
	before := len(e.auditRows("customer.update", harbor))
	res, body := e.post("/ui/customers/"+harbor, url.Values{"name": {"Harbor Family Practice"}, "slug": {"harbor-family-practice"}, "expected": {"1"},
		"onboarding_until": {"2026-12-01T17:00"}, "notes": {"only the notes changed"},
		"contact_name": {"Marcus Bell", ""}, "contact_email": {"marcus@harbor-family.example", ""}, "contact_phone": {"", ""}, "contact_role": {"practice administrator", ""}}, false)
	if res.StatusCode != 303 {
		t.Fatalf("save: %d %s", res.StatusCode, body)
	}
	rows := e.auditRows("customer.update", harbor)
	if len(rows) != before+1 {
		t.Fatalf("want one new audit row, got %d", len(rows)-before)
	}
	if d := string(rows[0].Details); !strings.Contains(d, `"fields":["notes"]`) || strings.Contains(d, "onboarding_until") {
		t.Fatalf("the audit row must name only notes: %s", d)
	}
	c, _ := e.st.GetCustomer(context.Background(), store.ScopeAll(), harbor)
	if c.OnboardingUntil == nil || !c.OnboardingUntil.Equal(until) {
		t.Fatalf("onboarding_until was rewritten: %v", c.OnboardingUntil)
	}
	// A different minute is a real edit.
	e.post("/ui/customers/"+harbor, url.Values{"name": {"Harbor Family Practice"}, "slug": {"harbor-family-practice"}, "expected": {"1"},
		"onboarding_until": {"2026-12-02T09:15"}, "notes": {"only the notes changed"},
		"contact_name": {"Marcus Bell", ""}, "contact_email": {"marcus@harbor-family.example", ""}, "contact_phone": {"", ""}, "contact_role": {"practice administrator", ""}}, false)
	if d := string(e.auditRows("customer.update", harbor)[0].Details); !strings.Contains(d, `"fields":["onboarding_until"]`) {
		t.Fatalf("a new minute is a change: %s", d)
	}
}

// A filter that names an unknown or out-of-scope customer reads like any other
// filter that matches nothing: neutral, with the in-scope denominator, no alarm
// and nothing that tells the two apart.
func TestM2UnknownOrForeignCustomerFilterIsNotAnAlarmOrAnOracle(t *testing.T) {
	e := newEnv(t)
	north, summit := e.customerID("Northwind Dental"), e.customerID("Summit Eye Care")
	e.signInAs("scoped", "operator", []string{north})
	text := e.page("/devices?q=no-such-host", 200)
	foreign := e.page("/devices?customer="+summit, 200)
	ghost := e.page("/devices?customer=01J9Y4K3A0B1C2D3E4F5G6H7ZZ", 200)
	for name, b := range map[string]string{"text filter": text, "foreign customer": foreign, "unknown customer": ghost} {
		mustContain(t, name, b, "No devices match these filters", "0 of 3 shown.", `class="panel-state neutral"`)
		if strings.Contains(b, "No devices are enrolled") || strings.Contains(b, `class="panel-state alarm"`) {
			t.Errorf("%s: a filter must not raise the empty-fleet alarm", name)
		}
		if strings.Contains(b, "Summit Eye Care") {
			t.Errorf("%s names a customer outside scope", name)
		}
	}
	e.signInAs("ad", "admin", nil)
	mustContain(t, "admin unknown customer", e.page("/devices?customer=01J9Y4K3A0B1C2D3E4F5G6H7ZZ", 200), "No devices match these filters", "0 of 25 shown.")
}

func TestM2UnchangedDeviceSettingsDoNotDirtyTheFleet(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	id := e.dev["FRONTDESK-01"]
	d, _ := e.st.GetDevice(context.Background(), store.ScopeAll(), id)
	dirty := func() bool { e.hub.mu.Lock(); defer e.hub.mu.Unlock(); return e.hub.dirty }
	same := url.Values{"friendly_name": {d.FriendlyName}, "notes": {d.Notes}, "device_class": {d.DeviceClass}, "update_ring": {d.UpdateRing}}
	e.hub.mu.Lock()
	e.hub.dirty = false
	e.hub.mu.Unlock()
	if res, _ := e.post("/ui/devices/"+id+"/settings", same, false); res.StatusCode != 303 {
		t.Fatalf("unchanged save: %d", res.StatusCode)
	}
	if dirty() {
		t.Fatal("a save that changed nothing must not mark the fleet dirty")
	}
	changed := url.Values{"friendly_name": {"New name"}, "notes": {d.Notes}, "device_class": {d.DeviceClass}, "update_ring": {d.UpdateRing}}
	e.post("/ui/devices/"+id+"/settings", changed, false)
	if !dirty() {
		t.Fatal("a real change must mark the fleet dirty")
	}
}

func TestM2AuditTabPagerShowsTheTotal(t *testing.T) {
	e := newEnv(t)
	e.signInAs("ad", "admin", nil)
	id := e.dev["FRONTDESK-01"]
	for i := 0; i < 30; i++ {
		e.post("/ui/devices/"+id+"/settings", url.Values{"notes": {"edit " + strconv.Itoa(i)}, "device_class": {"desktop"}, "update_ring": {"stable"}}, false)
	}
	total, err := e.st.DeviceAuditTotal(context.Background(), store.ScopeAll(), id)
	if err != nil || total < 30 {
		t.Fatalf("total %d %v", total, err)
	}
	p1 := e.page("/devices/"+id+"/audit?size=25", 200)
	mustContain(t, "page 1", p1, "Showing 1-25 of "+strconv.Itoa(total), `aria-label="Next page"`, "Per page")
	p2 := e.page("/devices/"+id+"/audit?size=25&page=2", 200)
	mustContain(t, "page 2", p2, "Showing 26-"+strconv.Itoa(min(50, total))+" of "+strconv.Itoa(total), `aria-label="Previous page"`)
	mustContain(t, "clamped", e.page("/devices/"+id+"/audit?size=25&page=99", 200), "past the end", " of "+strconv.Itoa(total))
}

// The loading state keeps the header and turns the body cells into skeleton bars (components 5.1).
func TestM2LoadingStateIsSkeletonRows(t *testing.T) {
	e := newEnv(t)
	res, css := e.do("GET", "/static/app.css", nil)
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	mustContain(t, "css", css, "#devresults.htmx-request table.fleet tbody td::after", "shimmer 1.4s")
	e.login()
	mustContain(t, "results region", e.page("/devices", 200), `id="devresults"`, `hx-indicator="#devresults"`, `data-loading-text="Loading devices"`)
}
