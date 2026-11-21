package notify

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

var update = flag.Bool("update", false, "rewrite the golden files of the renderers")

var (
	renderNow = time.Date(2026, 11, 17, 21, 4, 0, 0, time.UTC)
	cst       = time.FixedZone("CST", -6*3600)
)

const (
	custSummit = "01CUSTSUMMIT0000000000000A"
	custNorth  = "01CUSTNORTH00000000000000B"
	devFront   = "01DEVFRONT0000000000000001"
	devRecep   = "01DEVRECEP0000000000000002"
	devLab     = "01DEVLAB000000000000000003"
)

// renderView is a small fleet: Summit with three devices, Northwind expected and missing.
func renderView() state.FleetView {
	return state.FleetView{
		ComputedAt: renderNow,
		Devices: []state.DeviceState{
			{ID: devFront, CustomerID: custSummit, Hostname: "FRONTDESK-01", Primary: state.StateFailed, Detail: state.Detail{ErrorClass: "vss"}},
			{ID: devRecep, CustomerID: custSummit, Hostname: "RECEPTION-02", Primary: state.StateOK},
			{ID: devLab, CustomerID: custSummit, Hostname: "LAB-PC", Primary: state.StateStale},
		},
		Customers: []state.CustomerState{
			{ID: custSummit, Name: "Summit Eye Care"},
			{ID: custNorth, Name: "Northwind Dental", Status: state.CustomerMissing},
		},
	}
}

func ep(id, subject, subjectID, customer, alarm string, since time.Time) store.Episode {
	return store.Episode{ID: id, SubjectType: subject, SubjectID: subjectID, CustomerID: customer, AlarmType: alarm, OpenedAt: since, StateSince: since}
}

func alertBatch() []store.Episode {
	return []store.Episode{
		ep("e1", store.EpisodeDevice, devLab, custSummit, "STALE", renderNow.Add(-50*time.Hour)),
		ep("e2", store.EpisodeDevice, devFront, custSummit, "FAILED", renderNow.Add(-19*time.Hour)),
		ep("e3", store.EpisodeCustomer, custNorth, custNorth, store.AlarmMissing, renderNow.Add(-72*time.Hour)),
	}
}

func recoveryBatch() []store.Episode {
	closed := renderNow.Add(-2 * time.Minute)
	a := ep("e4", store.EpisodeDevice, devRecep, custSummit, "FAILED", renderNow.Add(-26*time.Hour))
	a.ClosedAt = &closed
	b := ep("e5", store.EpisodeCustomer, custNorth, custNorth, store.AlarmMissing, renderNow.Add(-72*time.Hour))
	b.ClosedAt = &closed
	return []store.Episode{a, b}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run Golden -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file (rerun with -update after checking the change):\n--- got ---\n%s", name, got)
	}
}

func TestGoldenAlert(t *testing.T) {
	c, err := RenderAlert(renderView(), alertBatch(), Options{Loc: cst, ConsoleURL: "https://console.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != mail.KindAlert || c.Subject != "[CryoBackup] 3 new alarms at 2 customers" {
		t.Fatalf("kind %q subject %q", c.Kind, c.Subject)
	}
	golden(t, "alert.txt", c.Text)
	golden(t, "alert.html", c.HTML)
	if err := c.Message("ops@example.com").Validate(); err == nil {
		t.Fatal("a message without From validated: the dispatcher must fill it in")
	}
	m := c.Message("ops@example.com")
	m.From = "CryoBackup <console@example.com>"
	if err := m.Validate(); err != nil {
		t.Fatalf("rendered message does not validate: %v", err)
	}
}

func TestGoldenRecovery(t *testing.T) {
	c, err := RenderRecovery(renderView(), recoveryBatch(), Options{Loc: cst, ConsoleURL: "https://console.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != mail.KindRecovery || c.Subject != "[CryoBackup] 2 alarms resolved at 2 customers" {
		t.Fatalf("kind %q subject %q", c.Kind, c.Subject)
	}
	golden(t, "recovery.txt", c.Text)
	golden(t, "recovery.html", c.HTML)
}

// The order of a message: customers with a MISSING alarm first, then by name; inside a customer,
// precedence then host name. Times are in the display zone with the zone named.
func TestRenderOrderAndZone(t *testing.T) {
	c, err := RenderAlert(renderView(), alertBatch(), Options{Loc: cst})
	if err != nil {
		t.Fatal(err)
	}
	north, summit := strings.Index(c.Text, "Northwind Dental"), strings.Index(c.Text, "Summit Eye Care")
	failed, stale := strings.Index(c.Text, "[FAILED] FRONTDESK-01"), strings.Index(c.Text, "[STALE] LAB-PC")
	if north < 0 || summit < north || failed < summit || stale < failed {
		t.Fatalf("order wrong:\n%s", c.Text)
	}
	if !strings.Contains(c.Text, "since Mon 16 Nov 2026, 8:04 PM CST") || !strings.Contains(c.Text, "error class: vss") {
		t.Fatalf("since or class missing:\n%s", c.Text)
	}
	if strings.Contains(c.Text, "http") || strings.Contains(c.HTML, "href") {
		t.Fatalf("no console URL configured, yet a link was rendered")
	}
}

// Only an http(s) console URL becomes a link; anything else is left out.
func TestRenderConsoleURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://console.example.com/": "https://console.example.com/",
		"http://10.0.0.5:8443":         "http://10.0.0.5:8443",
		"javascript:alert(1)":          "",
		"console.example.com":          "",
		"https://":                     "",
		"":                             "",
	} {
		if got := consoleURL(raw); got != want {
			t.Errorf("consoleURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// HTML escapes what the text part carries as it is; no remote content, no image, inline styles only.
func TestRenderHTMLIsSafe(t *testing.T) {
	v := renderView()
	v.Customers[0].Name = `Summit <script>alert(1)</script> & Co`
	v.Devices[0].Hostname = `<img src=x>`
	c, err := RenderAlert(v, alertBatch(), Options{Loc: cst, ConsoleURL: "https://console.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	// P20-MD: the mark is the one image (inline, by content id) and the phone media query the one
	// style block; both are fixed markup, so they are taken out before the search.
	h := styleBlock.ReplaceAllString(strings.Replace(c.HTML, `<img `+markAttr, "", 1), "")
	for _, bad := range []string{"<script", "<img", "<style", " src=\"", "url(", "@import", "<link"} {
		if strings.Contains(strings.ToLower(h), bad) {
			t.Errorf("HTML contains %q", bad)
		}
	}
	if !strings.Contains(c.HTML, "&lt;script&gt;") || !strings.Contains(c.Text, "<script>") {
		t.Fatalf("names not escaped in HTML or not kept in text")
	}
	if strings.Count(c.HTML, "href=") != 1 {
		t.Fatalf("want exactly one link (the console), HTML:\n%s", c.HTML)
	}
}

// An error class that is not a class word never reaches a message.
func TestRenderDropsAnythingThatIsNotAClassWord(t *testing.T) {
	v := renderView()
	v.Devices[0].Detail.ErrorClass = `C:\Users\jdoe\record.pdf`
	c, err := RenderAlert(v, alertBatch(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.Text, "record.pdf") || strings.Contains(c.Text, "error class") {
		t.Fatalf("a non-class error text was rendered:\n%s", c.Text)
	}
}

// The subject is counts only and fits the sender's limit at any fleet size.
func TestRenderSubjectFitsAtAnyFleetSize(t *testing.T) {
	v := state.FleetView{ComputedAt: renderNow}
	var eps []store.Episode
	for i := 0; i < 5000; i++ {
		cust := "cust" + strings.Repeat("x", 3) + string(rune('A'+i%26)) + string(rune('A'+i/26%26))
		v.Customers = append(v.Customers, state.CustomerState{ID: cust, Name: strings.Repeat("Ü", 200)})
		eps = append(eps, ep("e", store.EpisodeCustomer, cust, cust, store.AlarmMissing, renderNow))
	}
	c, err := RenderAlert(v, eps, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Subject) > 120 || strings.Contains(c.Subject, "Ü") {
		t.Fatalf("subject %q (%d bytes)", c.Subject, len(c.Subject))
	}
	if _, err := RenderAlert(v, nil, Options{}); err == nil {
		t.Fatal("an empty batch rendered")
	}
}

// IsCritical names exactly the critical alarm types of ADR 0020 decision 4; the alarm-level ones
// the store accepts are not critical.
func TestIsCriticalMatchesTheADR(t *testing.T) {
	want := map[string]bool{"FAILED": true, "STALE": true, "UNSEEN": true, "BLANK": true, "STUCK": true,
		"IDENTITY_CONFLICT": true, "UNKNOWN": true, "MISSING": true}
	var all []string
	all = append(all, store.EpisodeAlarmTypes(store.EpisodeDevice)...)
	all = append(all, store.EpisodeAlarmTypes(store.EpisodeCustomer)...)
	for _, a := range all {
		if IsCritical(a) != want[a] {
			t.Errorf("IsCritical(%s) = %v", a, IsCritical(a))
		}
	}
	if IsCritical("OK") || IsCritical("PARTIAL") {
		t.Fatal("OK or PARTIAL is critical")
	}
}
