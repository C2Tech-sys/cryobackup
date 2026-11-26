package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-I: the repository-key panel of the device Summary tab and its Verify now button (ADR 0030).

func (e *env) seedEscrow(t *testing.T) {
	t.Helper()
	if err := fixtures.SeedP22Escrow(context.Background(), e.st, e.now); err != nil {
		t.Fatal(err)
	}
}

func (e *env) escrowRow(t *testing.T, id string) store.Escrow {
	t.Helper()
	esc, err := e.st.ActiveEscrow(context.Background(), store.ScopeAll(), id)
	if err != nil {
		t.Fatal(err)
	}
	return esc
}

func (e *env) escrowDev(t *testing.T, key string) string {
	t.Helper()
	s, ok := fixtures.P22EscrowStateByKey(key)
	if !ok {
		t.Fatalf("no escrow state %q", key)
	}
	return e.dev[s.Host]
}

const verifyButton = `>Verify now</button>`

func verifyAction(id string) string { return `action="/ui/devices/` + id + `/escrow/verify"` }

func TestP22I_EscrowPanelStates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	type want struct {
		state   string
		has     []string
		button  bool
		hasNot  []string
		whyText string
	}
	cases := map[string]want{
		"verified":    {state: "verified", button: true, has: []string{"VERIFIED", "Last verified", "2 d ago", "Next check", "By ", "weekly, up to a day sooner", "Escrowed: the console holds the key", escrowProvesText}},
		"stale":       {state: "overdue", button: true, has: []string{"OVERDUE", "The weekly check has not run.", "10 days old", "Last attempt", "reached no verdict, so the earlier proof stands and keeps ageing", "Due now: the next maintenance pass runs it"}},
		"failed":      {state: "failed", button: true, has: []string{"FAILED", "The last check failed.", "wrong password", "Last failure", "wrong_password", "It raises ESCROW UNVERIFIED on this device."}},
		"failed-repo": {state: "failed", button: true, has: []string{"No repository was found at this device's storage location.", "repo_missing"}},
		"never":       {state: "never", button: true, has: []string{"NEVER VERIFIED", "This key has never been verified.", "Never", "At the next maintenance pass, then every hour until it passes"}},
		"held":        {state: "customer_held", has: []string{"CUSTOMER-HELD", "Customer-held. CryoBackup never holds this key."}, whyText: "Nothing to verify: the console cannot open this repository"},
		"unconfirmed": {state: "unconfirmed", has: []string{"NOT ENROLLED YET"}, whyText: "The device has not confirmed its enrollment"},
		"purging":     {state: "purging", has: []string{"PURGING"}, whyText: "This device is being purged: its repository is going away, so its key is no longer checked."},
		"retired":     {state: "verified", button: true, has: []string{"VERIFIED", "This device is retired. Its key is still checked every week, so a download from its backups stays possible (ADR 0030)."}},
	}
	for _, st := range fixtures.P22EscrowStates() {
		w, ok := cases[st.Key]
		if !ok {
			t.Errorf("state %q has no case", st.Key)
			continue
		}
		id := e.dev[st.Host]
		body := e.page("/devices/"+id, 200)
		i := strings.Index(body, `<section id="escrow-panel"`)
		if i < 0 {
			t.Errorf("%s: no escrow panel", st.Key)
			continue
		}
		panel := body[i : i+strings.Index(body[i:], "</section>")]
		mustContainText(t, st.Key, panel, append(w.has, `data-escrow="`+w.state+`"`, escrowProvesText)...)
		if got := strings.Contains(panel, verifyAction(id)) && strings.Contains(panel, verifyButton); got != w.button {
			t.Errorf("%s: Verify now button present=%v, want %v", st.Key, got, w.button)
		}
		if !w.button && w.whyText != "" {
			mustContainText(t, st.Key+" why", panel, w.whyText)
		}
		if pathLike.MatchString(panel) {
			t.Errorf("%s: a path in the panel", st.Key)
		}
	}
	// The one-line fact of the Facts list agrees with the panel: a stale proof is not green there either.
	stale := e.page("/devices/"+e.escrowDev(t, "stale"), 200)
	mustContainText(t, "stale fact", stale, "Key custody", "verified 10 d ago, overdue")
	if strings.Contains(stale, `st ok sm"><i aria-hidden="true"></i>ESCROWED`) {
		t.Error("a 10-day-old proof reads green in the Facts list")
	}
}

func TestP22I_EscrowPanelReadFailureIsAnAlarm(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error { // a key record with a class the console does not word
		_, err := tx.Exec(`UPDATE repo_key_escrow SET last_verify_error_class = 'x', last_verify_attempt_at = ? WHERE device_id = ?`, e.now.UnixMilli(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body := e.page("/devices/"+id, 200)
	mustContainText(t, "unknown class", body, "The last check failed with a class this console version does not recognise.", `data-escrow="failed"`)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM repo_key_escrow WHERE device_id = ?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body = e.page("/devices/"+id, 200)
	mustContainText(t, "no key", body, `data-escrow="none"`, "This device has no active key record.", "NO KEY")
	if strings.Contains(body, verifyAction(id)) {
		t.Error("a device with no key offers Verify now")
	}
}

func (e *env) verifyPost(id string, hx bool) (*http.Response, string) {
	e.t.Helper()
	return e.post("/ui/devices/"+id+"/escrow/verify", url.Values{}, hx)
}

func TestP22I_VerifyNowRecordsAVerdictAndShowsIt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	id := e.escrowDev(t, "stale")
	before := e.escrowRow(t, id)
	res, _ := e.verifyPost(id, false)
	loc := e.location(res)
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/devices/"+id+"?escrow=") || !strings.HasSuffix(loc, "#escrow-panel") {
		t.Fatalf("verify: %d %q (want a 303 back to the device page with a flash)", res.StatusCode, loc)
	}
	if strings.Contains(loc, "escrow=verified") {
		t.Errorf("the verdict is a bare word in the URL: %q", loc)
	}
	if n := e.be.Count(adminpath.MethodCatConfig); n != 1 {
		t.Fatalf("%d worker checks, want 1", n)
	}
	after := e.escrowRow(t, id)
	if before.VerifiedAt == nil || after.VerifiedAt == nil || !after.VerifiedAt.After(*before.VerifiedAt) || after.LastVerifyErrorClass != "" {
		t.Fatalf("the proof was not recorded: before %v after %v", before.VerifiedAt, after.VerifiedAt)
	}
	rows := e.auditRows("device.escrow.verify", id)
	if len(rows) != 1 { // the service audits; the page writes none of its own
		t.Fatalf("%d audit rows, want exactly 1 (not doubled)", len(rows))
	}
	// P22-R2b K2: a click on the page reaches the API in process, so the audit row's method is "web";
	// a direct API call says "api" (adminapi's TestP22R2bEscrowVerifyAuditMethodIsAPIForAPICalls).
	if rows[0].Method != "web" {
		t.Errorf("audit method = %q, want web for a page click", rows[0].Method)
	}
	if strings.Contains(string(rows[0].Details), "/") || pathLike.MatchString(string(rows[0].Details)) {
		t.Errorf("the audit row is not path-free: %s", rows[0].Details)
	}
	page := e.page(strings.SplitN(loc, "#", 2)[0], 200)
	mustContainText(t, "after verify", page, "Verified.", "The key opened this device's repository config just now.", `data-escrow="verified"`, "This does not prove that the backup data is intact.")
	if strings.Contains(page, "The weekly check has not run.") {
		t.Error("the panel still says the check is overdue after a pass")
	}
	// Reloading the bare page shows the recorded state, not the banner (it was the answer of one click).
	again := e.page("/devices/"+id, 200)
	if strings.Contains(again, "Verified.") && strings.Contains(again, "just now. It was recorded") {
		t.Error("the verdict banner outlives its click")
	}
	// A hand-made flash proves nothing: no banner for a token the console did not issue.
	for _, forged := range []string{"escrow:verified~" + "4102444800" + "~deadbeef", "escrow:verified", "verified", "escrow:verified.x~1~2~3"} {
		body := e.page("/devices/"+id+"?escrow="+url.QueryEscape(forged), 200)
		if strings.Contains(body, "just now. It was recorded") {
			t.Errorf("a forged flash %q shows a verdict", forged)
		}
	}
	// A token made for one verdict does not show another: the verdict word is under the MAC.
	swapped := strings.Replace(loc, "%3Averified", "%3Afailed.wrong_password", 1)
	if swapped == loc {
		t.Fatalf("the flash is not in the expected form: %q", loc)
	}
	if body := e.page(strings.SplitN(swapped, "#", 2)[0], 200); strings.Contains(body, "Verification failed.") || strings.Contains(body, "just now. It was recorded") {
		t.Error("a token for one verdict shows another")
	}
}

func TestP22I_VerifyNowFailedVerdictsAreLoud(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		err   error
		check func(t *testing.T, e *env, id string, before store.Escrow, after store.Escrow)
		words []string
	}{
		{"wrong password", adminpath.NewError(adminpath.CodeWrongPassword, ""), func(t *testing.T, e *env, id string, before, after store.Escrow) {
			if after.LastVerifyErrorClass != "wrong_password" {
				t.Errorf("class %q, want wrong_password recorded", after.LastVerifyErrorClass)
			}
		}, []string{"Verification failed.", "does not open this device's repository (wrong password)", `data-escrow="failed"`}},
		{"repo missing", adminpath.NewError(adminpath.CodeRepoMissing, ""), func(t *testing.T, e *env, id string, before, after store.Escrow) {
			if after.LastVerifyErrorClass != "repo_missing" {
				t.Errorf("class %q, want repo_missing recorded", after.LastVerifyErrorClass)
			}
		}, []string{"Verification failed.", "No repository was found at this device's storage location."}},
		{"no verdict", adminpath.NewError(adminpath.CodeStorageUnreachable, ""), func(t *testing.T, e *env, id string, before, after store.Escrow) {
			if after.LastVerifyErrorClass != "" || after.VerifiedAt == nil || !after.VerifiedAt.Equal(*before.VerifiedAt) {
				t.Errorf("an attempt that reached no verdict changed the proof: %v -> %v class %q", before.VerifiedAt, after.VerifiedAt, after.LastVerifyErrorClass)
			}
			if after.LastVerifyAttemptAt == nil || (before.LastVerifyAttemptAt != nil && !after.LastVerifyAttemptAt.After(*before.LastVerifyAttemptAt)) {
				t.Errorf("the attempt was not recorded")
			}
		}, []string{"No verdict.", "Storage did not answer.", "the earlier proof stands and keeps ageing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.login()
			e.seedEscrow(t)
			e.be.Errors = map[string]error{adminpath.MethodCatConfig: tc.err}
			id := e.escrowDev(t, "stale")
			before := e.escrowRow(t, id)
			res, _ := e.verifyPost(id, false)
			if res.StatusCode != http.StatusSeeOther {
				t.Fatalf("%d, want 303", res.StatusCode)
			}
			page := e.page(strings.SplitN(e.location(res), "#", 2)[0], 200)
			mustContainText(t, tc.name, page, tc.words...)
			tc.check(t, e, id, before, e.escrowRow(t, id))
			if n := len(e.auditRows("device.escrow.verify", id)); n != 1 {
				t.Errorf("%d audit rows, want 1", n)
			}
		})
	}
}

func TestP22I_VerifyNowOperatorIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	e.addAccount("ops", "operator")
	e.loginAs("ops", testPassword)
	id := e.escrowDev(t, "stale")
	body := e.page("/devices/"+id, 200)
	mustContainText(t, "operator panel", body, `data-escrow="overdue"`, "Verify now needs the admin role.")
	if strings.Contains(body, verifyAction(id)) || strings.Contains(body, verifyButton) {
		t.Error("an operator is offered Verify now")
	}
	for _, hx := range []bool{false, true} {
		res, _ := e.verifyPost(id, hx)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("operator post (hx=%v): %d, want 403", hx, res.StatusCode)
		}
	}
	if n := e.be.Count(adminpath.MethodCatConfig); n != 0 {
		t.Errorf("the worker was asked %d times for an operator", n)
	}
	if n := len(e.auditRows("device.escrow.verify", id)); n != 0 {
		t.Errorf("%d audit rows for a refused request", n)
	}
}

func TestP22I_VerifyNowEachRefusalHasItsSentence(t *testing.T) {
	t.Parallel()
	type refusal struct {
		name   string
		dev    string
		setup  func(e *env, id string)
		status int
		title  string
		text   string
		checks int // worker checks expected
	}
	unavailable := func(e *env, id string) {
		e.be.Errors = map[string]error{adminpath.MethodPing: adminpath.ErrUnavailable}
	}
	for _, tc := range []refusal{
		{name: "customer-held", dev: "held", status: 409, title: "Customer-held key: nothing to verify.", text: "CryoBackup never holds this key"},
		{name: "purging", dev: "purging", status: 409, title: "This device is being purged.", text: "Its repository is going away"},
		{name: "no key or enrollment", dev: "unconfirmed", status: 409, title: "Nothing to verify yet.", text: "no active escrowed key, or its enrollment is not confirmed"},
		{name: "worker down", dev: "stale", setup: unavailable, status: 503, title: "The maintenance worker did not answer.", text: "banner C07"},
		{name: "master key", dev: "stale", status: 503, title: "Master key unavailable.", text: "banner C12",
			setup: func(e *env, id string) {
				e.be.PingStatus = &adminpath.PingResult{Version: "fixtures", MasterKeyLoaded: false}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.login()
			e.seedEscrow(t)
			id := e.escrowDev(t, tc.dev)
			if tc.setup != nil {
				tc.setup(e, id)
			}
			// A browser post: the page with the refusal's real status and its sentence in the panel.
			res, body := e.verifyPost(id, false)
			if res.StatusCode != tc.status {
				t.Fatalf("browser post: %d, want %d", res.StatusCode, tc.status)
			}
			mustContainText(t, tc.name+" browser", body, "<title>", `id="escrow-panel"`, tc.title, tc.text)
			// An htmx post: the panel alone, 200, the same sentence (htmx 2 swaps no 4xx).
			res, frag := e.verifyPost(id, true)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("htmx post: %d, want 200", res.StatusCode)
			}
			if strings.Contains(frag, "<html") || !strings.HasPrefix(strings.TrimSpace(frag), `<section id="escrow-panel"`) {
				t.Errorf("an htmx answer is not the panel alone: %.120s", frag)
			}
			mustContainText(t, tc.name+" htmx", frag, tc.title, tc.text, `role="alert"`)
			if n := e.be.Count(adminpath.MethodCatConfig); n != 0 {
				t.Errorf("%d worker checks for a refusal, want 0", n)
			}
			for _, w := range []string{"Verified.", "Verification failed."} {
				if strings.Contains(body, w) || strings.Contains(frag, w) {
					t.Errorf("a refusal reads like a verdict: %q", w)
				}
			}
			if rec := e.escrowRowOrNil(id); rec != nil && tc.dev == "stale" {
				if rec.LastVerifyErrorClass != "" {
					t.Errorf("a refusal recorded a failure class %q", rec.LastVerifyErrorClass)
				}
			}
		})
	}
}

func (e *env) escrowRowOrNil(id string) *store.Escrow {
	esc, err := e.st.ActiveEscrow(context.Background(), store.ScopeAll(), id)
	if err != nil {
		return nil
	}
	return &esc
}

func TestP22I_VerifyNowAuditRefusedIsNotRecorded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	id := e.escrowDev(t, "stale")
	before := e.escrowRow(t, id)
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER no_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit down'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, body := e.verifyPost(id, false)
	if res.StatusCode < 400 { // the API refuses (the trigger reads as a constraint, 400; a down database is 500): never a verdict
		t.Fatalf("%d, want a refusal when the audit row cannot be written", res.StatusCode)
	}
	mustContainText(t, "not audited", body, "Not verified.", "Nothing was recorded.", `id="escrow-panel"`)
	for _, w := range []string{"Verified.", "Verification failed."} {
		if strings.Contains(body, w) {
			t.Errorf("a refused check reads like a verdict: %q", w)
		}
	}
	// A 500 from the API (the audit row or the record could not be written) has its own sentence.
	r500 := escrowAnswer(http.StatusInternalServerError, nil, nil)
	if r500.Status != 500 || r500.Verdict != "" || !strings.Contains(r500.View.Title, "NOT AUDITED") || !strings.Contains(r500.View.Text, "does not count") {
		t.Errorf("a 500 answer: %+v", r500)
	}
	if after := e.escrowRow(t, id); after.VerifiedAt == nil || !after.VerifiedAt.Equal(*before.VerifiedAt) {
		t.Error("a check whose audit row failed was recorded")
	}
}

func TestP22I_VerifyNowHTMXRedrawsTheWholePageWithTheVerdict(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	id := e.escrowDev(t, "stale") // overdue, so the Facts chip and the panel must both change
	if before := e.page("/devices/"+id, 200); !strings.Contains(before, "verified 10 d ago, overdue") {
		t.Fatal("setup: the Facts chip is not overdue")
	}
	res, frag := e.verifyPost(id, true)
	loc := res.Header.Get("HX-Redirect")
	if res.StatusCode != 200 || !strings.HasPrefix(loc, "/devices/"+id+"?escrow=") || !strings.HasSuffix(loc, "#escrow-panel") || strings.TrimSpace(frag) != "" {
		t.Fatalf("htmx verdict: %d HX-Redirect %q body %.80q, want 200 with the redirect a browser post gets", res.StatusCode, loc, frag)
	}
	page := e.page(strings.SplitN(loc, "#", 2)[0], 200)
	mustContainText(t, "after htmx verify", page, "Verified.", `data-escrow="verified"`, "Key custody", "verified 0 s ago")
	if strings.Contains(page, ", overdue") || strings.Contains(page, "OVERDUE") || strings.Contains(page, "The weekly check has not run.") {
		t.Error("the page still says the key is overdue after a pass: it contradicts itself")
	}
	if n := len(e.auditRows("device.escrow.verify", id)); n != 1 {
		t.Errorf("%d audit rows, want 1", n)
	}
	// A retired device is verified like any other, htmx or not.
	rid := e.escrowDev(t, "retired")
	if res, _ := e.verifyPost(rid, true); res.Header.Get("HX-Redirect") == "" {
		t.Errorf("retired htmx verify: no redirect (%d)", res.StatusCode)
	}
	// The htmx form still posts to its own action, targets the panel and works without JS.
	panel := e.page("/devices/"+e.escrowDev(t, "never"), 200)
	mustContainText(t, "form", panel, `hx-post="/ui/devices/`+e.escrowDev(t, "never")+`/escrow/verify"`, `hx-target="#escrow-panel"`, `method="post"`)
}

func TestP22I_VerdictFlashIsBoundToItsDevice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	a, b := e.escrowDev(t, "stale"), e.escrowDev(t, "never")
	res, _ := e.verifyPost(a, false)
	loc := e.location(res)
	_, tok, ok := strings.Cut(strings.SplitN(loc, "#", 2)[0], "?escrow=")
	if !ok {
		t.Fatalf("no flash in %q", loc)
	}
	const shown = "just now. It was recorded"
	if body := e.page("/devices/"+a+"?escrow="+tok, 200); !strings.Contains(body, shown) {
		t.Fatal("device A does not show its own verdict")
	}
	// Device B, same admin session, inside the TTL: A's verdict is not B's.
	body := e.page("/devices/"+b+"?escrow="+tok, 200)
	if strings.Contains(body, shown) || strings.Contains(body, "Verified.") {
		t.Error("A's verdict shows on never-verified device B")
	}
	mustContainText(t, "device B", body, "NEVER VERIFIED")
	// P22-R2b K1: the device id is covered by the MAC. A token for A with the device id edited to B's
	// names B and carries a MAC made over A's name, so B shows nothing. (Mutant: the id dropped from the
	// MAC, at mint and at verify, accepts this token.)
	raw, err := url.QueryUnescape(tok)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(raw, "escrow:"+a+":", "escrow:"+b+":", 1)
	if edited == raw {
		t.Fatalf("the token has another form: %q", raw)
	}
	body = e.page("/devices/"+b+"?escrow="+url.QueryEscape(edited), 200)
	if strings.Contains(body, shown) || strings.Contains(body, "Verified.") {
		t.Error("device B shows A's verdict from a token whose device id was edited")
	}
	mustContainText(t, "device B, edited token", body, "NEVER VERIFIED")
	// A still shows it; after the TTL nobody does.
	if body := e.page("/devices/"+a+"?escrow="+tok, 200); !strings.Contains(body, shown) {
		t.Error("A lost its verdict")
	}
	e.advance(flashTTL + time.Second)
	if body := e.page("/devices/"+a+"?escrow="+tok, 200); strings.Contains(body, shown) {
		t.Error("an expired flash still shows")
	}
}

func TestP22I_LeadsOfferVerifyNowOnlyWhenTheButtonIsThere(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	never := e.page("/devices/"+e.escrowDev(t, "never"), 200)
	mustContainText(t, "admin, never", never, "This key has never been verified.", "Or press Verify now.", verifyButton)
	// A device whose enrollment was never confirmed has no repository: no lead, no button, no promise of one.
	// (The fixture KIOSK-WAIT-01 is unconfirmed; give it a never-verified key.)
	unc := e.page("/devices/"+e.escrowDev(t, "unconfirmed"), 200)
	if strings.Contains(unc, "press Verify now") || strings.Contains(unc, verifyButton) || strings.Contains(unc, "This key has never been verified.") {
		t.Error("an unconfirmed device is offered a check")
	}
	mustContainText(t, "unconfirmed", unc, "The device has not confirmed its enrollment")
	e.addAccount("ops2", "operator")
	e.loginAs("ops2", testPassword)
	op := e.page("/devices/"+e.escrowDev(t, "stale"), 200)
	mustContainText(t, "operator, overdue", op, "The weekly check has not run.", "Verify now needs the admin role.")
	if strings.Contains(op, "press Verify now") {
		t.Error("an operator is told to press a button it does not have")
	}
}

func TestP22I_NextCheckHonoursTheRetryHold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	// "failed": proven 12 d ago, attempted 40 min ago: due, but held until an hour after the attempt.
	body := e.page("/devices/"+e.escrowDev(t, "failed"), 200)
	mustContainText(t, "held", body, "Next check", "Due: the next maintenance pass runs it, not before", "(an hour after the last attempt)")
	// "stale": attempted 3 h ago: no hold.
	body = e.page("/devices/"+e.escrowDev(t, "stale"), 200)
	mustContainText(t, "not held", body, "Due now: the next maintenance pass runs it")
	if strings.Contains(body, "an hour after the last attempt") {
		t.Error("a hold shows for an attempt made 3 hours ago")
	}
	// After the hour has passed the hold is gone.
	e.advance(30 * time.Minute)
	body = e.page("/devices/"+e.escrowDev(t, "failed"), 200)
	mustContainText(t, "hold over", body, "Due now: the next maintenance pass runs it")
}

func TestP22I_VerifyNowNeedsTheCSRFTokenAndThePageOrigin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	id := e.escrowDev(t, "stale")
	res, _ := e.do("POST", "/ui/devices/"+id+"/escrow/verify", url.Values{}, "Origin", e.srv.URL) // no csrf_token
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token: %d, want 403", res.StatusCode)
	}
	res, _ = e.do("POST", "/ui/devices/"+id+"/escrow/verify", url.Values{"csrf_token": {e.csrf}}, "Origin", "https://evil.example")
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("another origin: %d, want 403", res.StatusCode)
	}
	if n := e.be.Count(adminpath.MethodCatConfig); n != 0 {
		t.Errorf("the worker was asked %d times", n)
	}
}

func TestP22I_VerifyNowOtherCustomersDeviceIs404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	res, _ := e.verifyPost("01JNOSUCHDEVICE00000000000", false)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown device: %d, want 404", res.StatusCode)
	}
}

func TestP22I_EscrowProvesSentenceMatchesTheAPI(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	id := e.escrowDev(t, "stale")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/devices/"+id+"/escrow/verify", bytes.NewBufferString(""))
	r.Host = strings.TrimPrefix(e.srv.URL, "http://")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Origin", e.srv.URL)
	r.Header.Set(auth.CSRFHeader, e.csrf)
	for _, c := range e.sess {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.svc.pageAPI().ServeHTTP(w, r)
	var out adminapi.EscrowVerifyResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("the API answered %d %s", w.Code, w.Body.String())
	}
	if out.Proves != escrowProvesText {
		t.Errorf("the panel says %q, the API says %q", escrowProvesText, out.Proves)
	}
	if !strings.Contains(escrowProvesText, "opens the repository's config") || !strings.Contains(escrowProvesText, "does not prove that the backup data is intact") {
		t.Fatal("the proves sentence lost its meaning")
	}
}

func TestP22I_RefreshGateBlockedDeviceAsksNoWorker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedP22(t)
	for _, host := range []string{"SPARE-PC-09", "LEGAL-PC-01"} { // purging, customer-held
		id := e.dev[host]
		before := e.be.Count(adminpath.MethodSnapshots)
		_, body := e.post("/ui/devices/"+id+"/restore", url.Values{"action": {"refresh"}}, false)
		if n := e.be.Count(adminpath.MethodSnapshots) - before; n != 0 {
			t.Errorf("%s: a refresh asked the worker %d times", host, n)
		}
		want := "This device is being purged"
		if host == "LEGAL-PC-01" {
			want = "Customer-held key: no technician restore"
		}
		mustContainText(t, host, body, want)
		if strings.Contains(body, "Snapshot list refreshed from the repository.") || strings.Contains(body, "The snapshot list was not refreshed") {
			t.Errorf("%s: the page reports a refresh", host)
		}
		// refreshSnapshots itself refuses for every caller, not only the post handler.
		f, err := e.svc.Fleet.Fleet(context.Background(), store.ScopeAll())
		if err != nil {
			t.Fatal(err)
		}
		pr := &auth.Principal{Role: auth.RoleOwner, Scope: store.ScopeAll()}
		d, ok := e.svc.restoreBase(context.Background(), pr, f, id)
		if !ok || d.Blocked == nil {
			t.Fatalf("%s: the wizard is not blocked on it", host)
		}
		b := e.svc.refreshSnapshots(context.Background(), &d)
		if b.Class == ClassInfo || b.Title != d.Blocked.Title {
			t.Errorf("%s: refreshSnapshots answered %+v, want the blocked sentence", host, b)
		}
		d.Blocked = nil // even with the wizard's flag cleared, a device being purged is not read
		if host == "SPARE-PC-09" {
			if b := e.svc.refreshSnapshots(context.Background(), &d); b.Class == ClassInfo {
				t.Errorf("%s: a purging device was refreshed", host)
			}
		}
		if n := e.be.Count(adminpath.MethodSnapshots) - before; n != 0 {
			t.Errorf("%s: refreshSnapshots asked the worker %d times", host, n)
		}
	}
	// A device the wizard is not blocked on still refreshes.
	ok := e.dev["FRONTDESK-01"]
	before := e.be.Count(adminpath.MethodSnapshots)
	_, body := e.post("/ui/devices/"+ok+"/restore", url.Values{"action": {"refresh"}}, false)
	if e.be.Count(adminpath.MethodSnapshots) != before+1 {
		t.Error("a normal device's refresh no longer reads the repository")
	}
	mustContainText(t, "normal refresh", body, "Snapshot list refreshed from the repository.")
}

func TestP22I_EscrowPanelRetiredFailureNamesTheFleetBanner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	e.seedEscrow(t)
	id := e.escrowDev(t, "retired")
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET last_verify_error_class = 'wrong_password', last_verify_attempt_at = ? WHERE device_id = ? AND retired_at IS NULL`, e.now.Add(-time.Hour).UnixMilli(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body := e.page("/devices/"+id, 200)
	mustContainText(t, "retired failing", body, `data-escrow="failed"`, "it counts in the fleet banner for retired devices (C22)", "This device is retired.")
	if strings.Contains(body, "It raises ESCROW UNVERIFIED on this device.") {
		t.Error("a retired device claims an alarm of its own")
	}
}

// mustContainText is mustContain on the text of the page: the template escapes an apostrophe.
func mustContainText(t *testing.T, name, body string, wants ...string) {
	t.Helper()
	mustContain(t, name, html.UnescapeString(body), wants...)
}
