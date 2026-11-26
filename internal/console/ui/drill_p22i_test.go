package ui

import (
	"net/url"
	"strings"
	"testing"
)

// P22-I: the DRILL COPY strip is a field of the console's own ui.Service (Service.Drill, set by
// cli.Build), not a switch of the process. These are the ui half of cli's banner tests.

const drillStripText = "DRILL COPY: this console is a restored copy."

func TestP22I_DrillIsAServiceField(t *testing.T) {
	t.Parallel()
	drill, plain := newEnv(t), newEnv(t)
	drill.svc.Drill = true // before the first request, as cli.Build sets it
	// The bare pages (no session yet).
	for _, e := range []*env{drill, plain} {
		want := e == drill
		_, login := e.do("GET", "/login", nil)
		if got := strings.Contains(login, drillStripText); got != want {
			t.Errorf("sign-in: strip=%v, want %v", got, want)
		}
		_, bad := e.do("POST", "/login", url.Values{"username": {"nobody"}, "password": {"wrong"}})
		if got := strings.Contains(bad, drillStripText); got != want {
			t.Errorf("sign-in refusal: strip=%v, want %v", got, want)
		}
		_, reset := e.do("GET", "/login/reset?token=nope", nil)
		if got := strings.Contains(reset, drillStripText); got != want {
			t.Errorf("reset page: strip=%v, want %v", got, want)
		}
	}
	for _, e := range []*env{drill, plain} {
		e.login()
	}
	// /design is not here: it exists in fixtures mode only, and a drill copy is never a fixtures console (cli.Build).
	for _, pg := range []string{"/", "/devices", "/customers", "/system", "/account", "/devices/" + drill.dev["FRONTDESK-01"]} {
		d, p := drill.page(pg, 200), plain.page(strings.ReplaceAll(pg, drill.dev["FRONTDESK-01"], plain.dev["FRONTDESK-01"]), 200)
		if !strings.Contains(d, drillStripText) || strings.Count(d, `id="drill-copy"`) != 1 {
			t.Errorf("%s: the drill console shows no single strip", pg)
		}
		if strings.Contains(p, drillStripText) {
			t.Errorf("%s: a console built beside a drill console shows its strip", pg)
		}
	}
	// The error page, reached through the same page() builder.
	if body := drill.page("/devices/01JNOSUCHDEVICE00000000000", 404); !strings.Contains(body, drillStripText) {
		t.Error("the 404 page of a drill console has no strip")
	}
	// The MFA page: a password-only session.
	for _, e := range []*env{drill, plain} {
		e.loginAsPasswordOnly("admin")
		_, mfa := e.do("GET", "/login/mfa", nil)
		if got, want := strings.Contains(mfa, drillStripText), e == drill; got != want {
			t.Errorf("mfa page: strip=%v, want %v", got, want)
		}
	}
}
