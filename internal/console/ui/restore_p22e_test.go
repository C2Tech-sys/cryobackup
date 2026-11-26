package ui

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-E fix E4: the web wizard's fresh-MFA gate (ADR 0027 ruling C5). A stale
// session is answered 403 with the step-up reason before any worker call and
// writes no restore; a current authenticator code in the form steps the session
// up and the restore is created; each refusal is its own 4xx page.

func (e *env) restoreCount(id string) int {
	e.t.Helper()
	l, err := e.st.ListRestoreRequests(context.Background(), store.ScopeAll(), id, store.Page{Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(l.Items)
}

func TestRestoreWizardNeedsFreshMFA(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	base := "/ui/devices/" + id + "/restore"
	submit := func(code string) url.Values {
		v := url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "cur": {"/C/Users/Public/Documents"}, "action": {"submit"},
			"path": {"/C/Users/Public/Documents/Quarterly-Report.txt"}}
		if code != "" {
			v.Set("code", code)
		}
		return v
	}

	// The login's check ages past the window: stale.
	e.advance(6 * time.Minute)
	calls := len(e.be.Calls())
	res, body := e.post(base, submit(""), false)
	if res.StatusCode != 403 || !strings.Contains(body, "A recent MFA check is required") {
		t.Fatalf("stale session: %d", res.StatusCode)
	}
	if n := e.restoreCount(id); n != 0 {
		t.Fatalf("a stale session wrote %d restores", n)
	}
	if got := e.be.Calls()[calls:]; len(got) != 0 {
		t.Fatalf("a stale session reached the worker: %+v", got)
	}
	// The 403 page offers the authenticator-code field (fix E7) and keeps the
	// chosen snapshot and file in the form.
	for _, want := range []string{`name="code"`, `autocomplete="one-time-code"`, `inputmode="numeric"`, `<label for="restore-code">Authenticator code</label>`,
		`name="path" value="/C/Users/Public/Documents/Quarterly-Report.txt"`, `value="submit">Continue to the download</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the stale page lacks %s", want)
		}
	}
	// A wrong code is refused by the step-up: still nothing written, the field offered again.
	shown := formOf(t, body)
	shown.Set("code", "000000")
	if res, b := e.post(base, shown, false); res.StatusCode != 401 || e.restoreCount(id) != 0 || !strings.Contains(b, `name="code"`) {
		t.Fatalf("wrong code: %d, %d restores", res.StatusCode, e.restoreCount(id))
	}
	// The shown form, posted as it stands with a current authenticator code,
	// steps up and creates the restore.
	shown.Set("code", e.mfa.code("admin"))
	res, _ = e.post(base, shown, false)
	if res.StatusCode != 303 || !strings.HasPrefix(res.Header.Get("Location"), "/restores/") || e.restoreCount(id) != 1 {
		t.Fatalf("fresh after step-up: %d %s, %d restores", res.StatusCode, res.Header.Get("Location"), e.restoreCount(id))
	}
	if e.be.Count(adminpath.MethodDump) != 0 {
		t.Fatal("creating a restore ran a dump")
	}
}

func TestRestoreWizardRefusalPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["DR-OFFICE-PC"]
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET retired_at = ?, purge_requested_at = ? WHERE id = ?`, e.now.UnixMilli(), e.now.UnixMilli(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, body := e.post("/ui/devices/"+id+"/restore", url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "cur": {"/C/Users/Public/Documents"},
		"action": {"submit"}, "path": {"/C/Users/Public/Documents/Quarterly-Report.txt"}}, false)
	if res.StatusCode != 409 || !strings.Contains(body, "This device is being purged") {
		t.Fatalf("purging: %d", res.StatusCode)
	}
	if e.restoreCount(id) != 0 {
		t.Fatal("a purging device got a restore")
	}
}

var (
	hiddenRE   = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)
	selectedRE = regexp.MustCompile(`<option value="([0-9a-f]{64})" selected>`)
	submitRE   = regexp.MustCompile(`<button class="btn primary" type="submit" name="(action)" value="(submit)">`)
)

// formOf reads the restore_box form a browser would post: its hidden fields,
// the selected snapshot and the Continue button. It fails when the page offers
// no way to continue.
func formOf(t *testing.T, body string) url.Values {
	t.Helper()
	v := url.Values{}
	for _, m := range hiddenRE.FindAllStringSubmatch(body, -1) {
		v.Set(m[1], html.UnescapeString(m[2]))
	}
	if m := selectedRE.FindStringSubmatch(body); m != nil {
		v.Set("snapshot", m[1])
	}
	m := submitRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the page offers no Continue button")
	}
	v.Set(m[1], m[2])
	return v
}

// TestRestoreWizardEveryRefusalPage (fix E8): every rule a restore create can
// refuse by has its own status and sentence, never a 500. The web wizard
// downloads a zip, so the device-mode rules (retired download only, agent too
// old, restore active) and ErrInvalid cannot be reached through it today; they
// are pinned here on the mapping the handler uses.
func TestRestoreWizardEveryRefusalPage(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		title  string
	}{
		{restores.ErrStepUp, 403, "A recent MFA check is required."},
		{restores.ErrCustomerHeld, 409, "Customer-held key"},
		{restores.ErrDevicePurging, 409, "This device is being purged"},
		{restores.ErrRetiredDownloadOnly, 409, "This device is retired: download only"},
		{restores.ErrAgentTooOld, 409, "The agent on this device is too old"},
		{restores.ErrRestoreActive, 409, "A restore is already active for this device"},
		{restores.ErrMasterKey, 503, "Master key unavailable"},
		{restores.ErrInvalid, 400, "That selection is not valid"},
		{store.ErrNotFound, 404, "Not found"},
	} {
		status, b, known := restoreRefusal(c.err)
		if !known || status != c.status || b == nil || b.Title != c.title || b.Text == "" {
			t.Errorf("%v: %d %+v %v, want %d %q", c.err, status, b, known, c.status, c.title)
		}
	}
	if _, _, known := restoreRefusal(store.ErrAudit); known {
		t.Error("an audit failure is a fault, not a refusal page")
	}
}

// TestRestoreWizardReachableRefusals (fix E8): customer-held and a missing
// master key through HTTP: their status and sentence, no restore row.
func TestRestoreWizardReachableRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	post := func(host string) (int, string) {
		res, body := e.post("/ui/devices/"+e.dev[host]+"/restore", url.Values{"snapshot": {fixtures.FixtureSnapshotID}, "cur": {"/C/Users/Public/Documents"},
			"action": {"submit"}, "path": {"/C/Users/Public/Documents/Quarterly-Report.txt"}}, false)
		return res.StatusCode, body
	}
	if code, body := post("LEGAL-PC-01"); code != 409 || !strings.Contains(body, "No technician restore for a customer-held key.") || e.restoreCount(e.dev["LEGAL-PC-01"]) != 0 {
		t.Fatalf("customer-held: %d", code)
	}
	e.svc.Restores.Master = nil // the console lost its master key
	if code, body := post("DR-OFFICE-PC"); code != 503 || !strings.Contains(body, "Restores are disabled until the master key is loaded.") || e.restoreCount(e.dev["DR-OFFICE-PC"]) != 0 {
		t.Fatalf("no master key: %d", code)
	}
}
