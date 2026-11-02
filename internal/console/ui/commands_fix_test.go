package ui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The actions fragment is operator-only (architecture §5 S07), like the actions it offers.
func TestActionsFragmentNeedsTheOperatorRole(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.addAccount("viewer2", "read_only")
	dev := e.dev["FRONTDESK-01"]
	e.page("/ui/devices/"+dev+"/actions", 200)
	e.loginAs("viewer2", testPassword)
	if res, _ := e.do("GET", "/ui/devices/"+dev+"/actions", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only GET actions = %d, want 403", res.StatusCode)
	}
	// The device page still renders for them, with the actions shown as unavailable.
	mustContain(t, "read-only panel", e.page("/devices/"+dev, 200), "Needs the operator role.")
}

// While a command is in flight the Commands tab refreshes itself through the console's
// fast-poll hook (fresh.js: visible tab only, 5 s) and asks for the fragment, not the page.
func TestCommandsTabRefreshesAsAFragmentOnlyWhileInFlight(t *testing.T) {
	e := newEnv(t)
	e.login()
	dev := e.dev["FRONTDESK-01"]
	idle := e.page("/devices/"+dev+"/commands?status=expired", 200)
	if strings.Contains(idle, "data-fast-poll") {
		t.Error("an idle list polls")
	}
	if strings.Contains(idle, "every 5s") {
		t.Error("the list uses an unconditional htmx timer")
	}
	e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"frag"}}, http.StatusSeeOther)
	page := e.page("/devices/"+dev+"/commands", 200)
	mustContain(t, "in flight", page, `data-fast-poll`, `hx-trigger="cryo:poll"`, `hx-get="/devices/`+dev+`/commands"`)
	// The refresh request (htmx sends HX-Request) gets the list and the action menu out of band, no shell.
	res, body := e.do("GET", "/devices/"+dev+"/commands", nil, "HX-Request", "true")
	if res.StatusCode != 200 {
		t.Fatalf("fragment: %d", res.StatusCode)
	}
	mustContain(t, "fragment", body, `id="device-commands"`, `id="dev-actions"`, `hx-swap-oob="true"`, "QUEUED")
	for _, shell := range []string{"<html", "<nav class=\"tabs\"", "id=\"freshness\""} {
		if strings.Contains(body, shell) {
			t.Errorf("the refresh returned page chrome (%s)", shell)
		}
	}
	// Once nothing is in flight the fragment no longer carries the hook, so the polling stops.
	cmds := e.commandsOfType(dev, "backup_now")
	var id string
	for _, c := range cmds {
		if c.IdempotencyKey == "frag" {
			id = c.ID
		}
	}
	delivered, _ := e.st.AgentNextCommands(context.Background(), dev, 10)
	for _, c := range delivered {
		if c.ID == id {
			if _, err := e.st.AgentAckCommand(context.Background(), dev, id, store.CommandAck{Status: "succeeded", Result: []byte(`{"started":true}`)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, body = e.do("GET", "/devices/"+dev+"/commands", nil, "HX-Request", "true")
	if strings.Contains(body, "data-fast-poll") {
		t.Error("a finished command keeps the list polling")
	}
}

// The polling hook itself: visible tab only.
func TestFreshJSFastPollSkipsHiddenTabs(t *testing.T) {
	e := newEnv(t)
	_, js := e.do("GET", "/static/js/fresh.js", nil)
	if !strings.Contains(js, "data-fast-poll") || !strings.Contains(js, "document.hidden") {
		t.Fatal("fresh.js lacks the visibility-guarded fast poll")
	}
}

// A result that arrives after the deadline is shown as such, not as "never received".
func TestLateResultShowsOnTheCommandsTab(t *testing.T) {
	e := newEnv(t)
	e.login()
	dev := e.dev["NEW-EXAM-6"]
	e.postTo("/ui/devices/"+dev+"/backup-now", url.Values{"key": {"late"}}, http.StatusSeeOther)
	delivered, err := e.st.AgentNextCommands(context.Background(), dev, 10)
	if err != nil || len(delivered) != 1 {
		t.Fatalf("delivery: %v %v", delivered, err)
	}
	e.now = e.now.Add(store.DefaultOperatorTTL("backup_now") + time.Minute)
	if _, err := e.st.ExpireCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "expired after delivery", e.page("/devices/"+dev+"/commands", 200), "EXPIRED", "received it but had not answered")
	if _, err := e.st.AgentAckCommand(context.Background(), dev, delivered[0].ID, store.CommandAck{Status: "succeeded", Result: []byte(`{"started":true}`)}); err != nil {
		t.Fatal(err)
	}
	body := e.page("/devices/"+dev+"/commands", 200)
	mustContain(t, "late result", body, "EXPIRED", "answered after the deadline: it reported the command done.")
}

// finding 4: resolving an identity conflict by retiring tells the gateway-login sync at once.
func TestRetireByIdentityResolveSyncsTheGatewayLogins(t *testing.T) {
	e := newEnv(t)
	e.login()
	var syncs atomic.Int32
	e.svc.SyncGateway = func() { syncs.Add(1) }
	dev := e.dev["FRONTDESK-02"]
	e.postTo("/ui/devices/"+dev+"/identity/resolve", url.Values{"action": {"keep"}, "reason": {"was a swap"}}, http.StatusSeeOther)
	if syncs.Load() != 0 {
		t.Fatalf("keeping an identity changed no login, but sync ran %d times", syncs.Load())
	}
	// A refused retire (no confirmation) syncs nothing either; a good one syncs once.
	other := e.dev["LT-OPTOM-04"]
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET identity_conflict_at = ? WHERE id = ?`, e.now.UnixMilli(), other)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.postTo("/ui/devices/"+other+"/identity/resolve", url.Values{"action": {"retire"}, "reason": {"clone"}}, http.StatusSeeOther)
	if syncs.Load() != 0 {
		t.Fatal("a refused retire synced")
	}
	e.postTo("/ui/devices/"+other+"/identity/resolve", url.Values{"action": {"retire"}, "reason": {"clone"}, "confirm": {"LT-OPTOM-04"}}, http.StatusSeeOther)
	if syncs.Load() != 1 {
		t.Fatalf("retire synced %d times, want 1", syncs.Load())
	}
}
