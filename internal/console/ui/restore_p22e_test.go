package ui

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
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
	// A wrong code is refused by the step-up: still nothing written.
	if res, _ := e.post(base, submit("000000"), false); res.StatusCode != 401 || e.restoreCount(id) != 0 {
		t.Fatalf("wrong code: %d, %d restores", res.StatusCode, e.restoreCount(id))
	}
	// A current authenticator code steps up and creates the restore.
	res, _ = e.post(base, submit(e.mfa.code("admin")), false)
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
