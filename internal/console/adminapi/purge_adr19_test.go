package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// ADR 0019: the step-up route, the device purge, the customer delete and the purge of a
// customer's retired devices.

// lockedBuf is a log sink safe for the job's goroutine.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// okPurger is a worker whose purge_repo always removes everything.
type okPurger struct {
	mu    sync.Mutex
	calls []string
}

func (p *okPurger) PurgeRepo(_ context.Context, req adminpath.PurgeRepoRequest) (adminpath.PurgeRepoResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req.DeviceID)
	return adminpath.PurgeRepoResult{ObjectsRemoved: 7, BytesRemoved: 4096}, nil
}

type purgeEnv struct {
	*env
	logs  *lockedBuf
	owner *session
	hosts []string
}

func newPurgeEnv(t *testing.T) *purgeEnv {
	t.Helper()
	e := newEnv(t)
	pe := &purgeEnv{env: e, logs: &lockedBuf{}}
	e.svc.Log = logx.New(pe.logs, logx.LevelDebug)
	pe.owner = e.login("admin", pw)
	l, err := e.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{IncludeRetired: true, Page: store.Page{Limit: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range l.Items {
		pe.hosts = append(pe.hosts, d.Hostname)
	}
	return pe
}

// noHostNames fails when a device host name appears in the captured logs, in any purge or
// customer audit row, or in body.
func (pe *purgeEnv) noHostNames(t *testing.T, bodies ...[]byte) {
	t.Helper()
	var rows strings.Builder
	_ = pe.st.Read(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT COALESCE(action,'') || '|' || COALESCE(object_id,'') || '|' || COALESCE(details,'') FROM audit_events
			WHERE action LIKE 'device.purge.%' OR action = 'customer.delete' OR action = 'auth.stepup'`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var s string
			_ = r.Scan(&s)
			rows.WriteString(s + "\n")
		}
		return r.Err()
	})
	logs := pe.logs.String()
	for _, h := range pe.hosts {
		if strings.Contains(logs, h) {
			t.Errorf("host name %q in a log line:\n%s", h, logs)
		}
		if strings.Contains(rows.String(), h) {
			t.Errorf("host name %q in an audit row:\n%s", h, rows.String())
		}
		for _, b := range bodies {
			if strings.Contains(string(b), h) {
				t.Errorf("host name %q in an answer: %s", h, b)
			}
		}
	}
}

func (pe *purgeEnv) actions(t *testing.T, since int) []string {
	t.Helper()
	return pe.auditActionsSince(since)
}

func purgeBody(host string) DevicePurgeRequest {
	return DevicePurgeRequest{ConfirmHostName: host, Reason: "decommissioned"}
}

// ---- the problem code ----

// The api table entry must be byte-compatible with the document auth.RequireFreshMFA writes by
// hand, so a client reads one shape whichever path answered.
func TestAdr19StepUpProblemMatchesTheAuthGate(t *testing.T) {
	if api.CodeMFAStepUpRequired != auth.CodeStepUpRequired {
		t.Fatalf("api %q != auth %q", api.CodeMFAStepUpRequired, auth.CodeStepUpRequired)
	}
	e := newEnv(t)
	ctx := httpx.WithRequestID(context.Background(), "req-adr19")
	stale := e.clk.Now().Add(-auth.FreshMFAWindow - time.Second)
	ctx = auth.WithPrincipal(ctx, &auth.Principal{Admin: store.Admin{ID: "x", Username: "x"}, Role: auth.RoleOwner, SessionID: "s1", Scope: store.ScopeAll(), MFAVerifiedAt: &stale})
	req := httptest.NewRequest("DELETE", "/api/v1/devices/x", nil).WithContext(ctx)

	gate := httptest.NewRecorder()
	e.svc.Auth.RequireFreshMFA(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("a stale session reached the handler") })).ServeHTTP(gate, req)
	table := httptest.NewRecorder()
	httpx.Problem(table, req, api.CodeMFAStepUpRequired, 0)

	if gate.Code != 403 || table.Code != 403 {
		t.Fatalf("status gate %d table %d, want 403", gate.Code, table.Code)
	}
	if g, w := gate.Body.String(), table.Body.String(); g != w {
		t.Fatalf("bodies differ:\nauth gate: %s\napi table: %s", g, w)
	}
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if gate.Header().Get(h) != table.Header().Get(h) {
			t.Errorf("%s: gate %q table %q", h, gate.Header().Get(h), table.Header().Get(h))
		}
	}
}

// ---- device purge ----

func TestAdr19DevicePurgeEndToEnd(t *testing.T) {
	pe := newPurgeEnv(t)
	d := pe.deviceNamed("OLD-RECEPTION-PC")
	purger := &okPurger{}
	runner := jobs.NewRunner(pe.st, "adr19-test", logx.New(pe.logs, logx.LevelDebug))
	runner.Add((&jobs.DevicePurge{Store: pe.st, Purger: purger, Log: logx.New(pe.logs, logx.LevelDebug)}).Job())
	pe.svc.Jobs = runner
	n0 := pe.auditRowCount()

	res, b := pe.req("DELETE", "/api/v1/devices/"+d.ID, pe.owner, purgeBody(d.Hostname))
	if res.StatusCode != 202 {
		t.Fatalf("purge: %d %s", res.StatusCode, b)
	}
	var out DevicePurgeResponse
	if err := json.Unmarshal(b, &out); err != nil || out.ID != d.ID || !out.Changed || out.State != "RETIRED" || out.Purge.RequestedAt.IsZero() {
		t.Fatalf("answer %s (%v)", b, err)
	}
	// The request woke the job: it ran at once, removed the repository and deleted the row.
	jobBackground.Wait()
	if _, err := pe.st.GetDevice(context.Background(), store.ScopeAll(), d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after the woken job the device is still there: %v", err)
	}
	if len(purger.calls) != 1 || purger.calls[0] != d.ID {
		t.Fatalf("purge_repo calls = %v", purger.calls)
	}
	got := strings.Join(pe.actions(t, n0), ",")
	if got != "device.purge.request,device.purge.done" {
		t.Fatalf("audit actions = %s", got)
	}
	pe.noHostNames(t, b)
}

func TestAdr19DevicePurgeShowsInTheDeviceJSON(t *testing.T) {
	pe := newPurgeEnv(t)
	d := pe.deviceNamed("OLD-RECEPTION-PC")
	if _, b := pe.req("GET", "/api/v1/devices/"+d.ID, pe.owner, nil); strings.Contains(string(b), `"purge"`) {
		t.Fatalf("a device that is not purging shows purge: %s", b)
	}
	pe.mustReq("DELETE", "/api/v1/devices/"+d.ID, pe.owner, purgeBody(d.Hostname), 202)
	_, b := pe.req("GET", "/api/v1/devices/"+d.ID, pe.owner, nil)
	var dj DeviceDetailJSON
	if err := json.Unmarshal(b, &dj); err != nil || dj.Purge == nil || dj.Purge.RequestedAt.IsZero() || dj.Purge.LastAttemptAt != nil || dj.Purge.LastErrorClass != "" {
		t.Fatalf("device detail purge = %+v (%v): %s", dj.Purge, err, b)
	}
	// The fixture SPARE-PC-09 has a failed attempt: the list shows its class and time.
	spare := pe.deviceNamed("SPARE-PC-09")
	_, lb := pe.req("GET", "/api/v1/devices?include_retired=true", pe.owner, nil)
	var l DeviceListResponse
	if err := json.Unmarshal(lb, &l); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range l.Devices {
		if row.ID == spare.ID {
			found = true
			if row.Purge == nil || row.Purge.LastErrorClass != "storage_unreachable" || row.Purge.LastAttemptAt == nil {
				t.Errorf("SPARE-PC-09 purge = %+v", row.Purge)
			}
		}
	}
	if !found {
		t.Fatal("the purging device is not in the list with include_retired")
	}
}

func TestAdr19TypedNameMustMatchExactly(t *testing.T) {
	pe := newPurgeEnv(t)
	d := pe.deviceNamed("OLD-RECEPTION-PC")
	before := pe.digest()
	for _, typed := range []string{"", "old-reception-pc", "OLD-RECEPTION-PC ", "OLD-RECEPTION", "SPARE-PC-09"} {
		res, b := pe.req("DELETE", "/api/v1/devices/"+d.ID, pe.owner, purgeBody(typed))
		if res.StatusCode != 400 || problemCode(t, res, b) != api.CodeInvalidRequest || !strings.Contains(string(b), `"field":"confirm_host_name"`) {
			t.Errorf("typed %q: %d %s", typed, res.StatusCode, b)
		}
		if typed != "" && strings.Contains(string(b), typed) {
			t.Errorf("the answer echoes what was typed: %s", b)
		}
		pe.noHostNames(t, b)
	}
	if pe.digest() != before {
		t.Fatal("a mismatched confirmation changed stored data")
	}
	// The same for a customer.
	c := pe.customerNamed("Cedar Valley Veterinary")
	for _, p := range []string{"DELETE /api/v1/customers/" + c.ID, "POST /api/v1/customers/" + c.ID + "/purge-retired-devices"} {
		m, path, _ := strings.Cut(p, " ")
		res, b := pe.req(m, path, pe.owner, CustomerConfirmRequest{ConfirmName: "cedar valley veterinary", Reason: "closed"})
		if res.StatusCode != 400 || !strings.Contains(string(b), `"field":"confirm_name"`) || strings.Contains(string(b), "Cedar") || strings.Contains(string(b), "cedar") {
			t.Errorf("%s with a wrong name: %d %s", p, res.StatusCode, b)
		}
	}
	if pe.digest() != before {
		t.Fatal("a mismatched customer confirmation changed stored data")
	}
}

func TestAdr19StaleMFANeedsAStepUpThenTheSameRequestPasses(t *testing.T) {
	pe := newPurgeEnv(t)
	d := pe.deviceNamed("OLD-RECEPTION-PC")
	pe.clk.Advance(auth.FreshMFAWindow + time.Minute)
	res, b := pe.req("DELETE", "/api/v1/devices/"+d.ID, pe.owner, purgeBody(d.Hostname))
	if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeMFAStepUpRequired {
		t.Fatalf("stale session: %d %s, want 403 %s", res.StatusCode, b, api.CodeMFAStepUpRequired)
	}
	if dv := pe.deviceNamed("OLD-RECEPTION-PC"); dv.Purging() {
		t.Fatal("a stale session marked the device")
	}
	// A wrong code is refused with the existing code and does not renew the mark.
	if res, b := pe.req("POST", "/api/v1/auth/step-up", pe.owner, StepUpRequest{Code: "000000"}); res.StatusCode != 401 || problemCode(t, res, b) != api.CodeUnauthorized {
		t.Fatalf("wrong code: %d %s", res.StatusCode, b)
	}
	if res, _ := pe.req("DELETE", "/api/v1/devices/"+d.ID, pe.owner, purgeBody(d.Hostname)); res.StatusCode != 403 {
		t.Fatalf("after a wrong code the request passed: %d", res.StatusCode)
	}
	if res, b := pe.req("POST", "/api/v1/auth/step-up", pe.owner, StepUpRequest{Code: ""}); res.StatusCode != 400 {
		t.Fatalf("empty code: %d %s", res.StatusCode, b)
	}
	if res, b := pe.req("POST", "/api/v1/auth/step-up", pe.owner, StepUpRequest{Code: pe.mfa.code("admin")}); res.StatusCode != 204 {
		t.Fatalf("step-up: %d %s", res.StatusCode, b)
	}
	if res, b := pe.req("DELETE", "/api/v1/devices/"+d.ID, pe.owner, purgeBody(d.Hostname)); res.StatusCode != 202 {
		t.Fatalf("after the step-up: %d %s", res.StatusCode, b)
	}
	pe.noHostNames(t)
}

func TestAdr19APITokensAreForbiddenNeverPromptedForAStepUp(t *testing.T) {
	pe := newPurgeEnv(t)
	d := pe.deviceNamed("OLD-RECEPTION-PC")
	c := pe.customerNamed("Cedar Valley Veterinary")
	summit := pe.customerNamed("Summit Eye Care")
	r, b := pe.req("POST", "/api/v1/me/api-tokens", pe.owner, TokenCreateRequest{Name: "full", Scope: "full", Days: 30})
	var tok TokenCreateResponse
	if r.StatusCode != 201 || json.Unmarshal(b, &tok) != nil {
		t.Fatalf("token: %d %s", r.StatusCode, b)
	}
	before := pe.digest()
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"DELETE", "/api/v1/devices/" + d.ID, purgeBody(d.Hostname)},
		{"DELETE", "/api/v1/customers/" + c.ID, CustomerConfirmRequest{ConfirmName: c.Name, Reason: "closed"}},
		{"POST", "/api/v1/customers/" + summit.ID + "/purge-retired-devices", CustomerConfirmRequest{ConfirmName: summit.Name, Reason: "closed"}},
		{"POST", "/api/v1/auth/step-up", StepUpRequest{Code: pe.mfa.code("admin")}},
	} {
		res, b := pe.req(tc.method, tc.path, nil, tc.body, "Authorization", "Bearer "+tok.Token)
		if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
			t.Errorf("%s %s with an owner's full token: %d %s, want 403 forbidden", tc.method, tc.path, res.StatusCode, b)
		}
	}
	if pe.digest() != before {
		t.Fatal("a token changed stored data")
	}
}

func TestAdr19OnlyTheOwnerRolePasses(t *testing.T) {
	pe := newPurgeEnv(t)
	d := pe.deviceNamed("OLD-RECEPTION-PC")
	c := pe.customerNamed("Cedar Valley Veterinary")
	summit := pe.customerNamed("Summit Eye Care")
	n0 := pe.auditRowCount()
	for _, role := range []string{"read_only", "operator", "admin"} {
		pe.addAdmin("u-"+role, role, nil)
		s := pe.login("u-"+role, pw)
		for _, tc := range []struct {
			method, path string
			body         any
		}{
			{"DELETE", "/api/v1/devices/" + d.ID, purgeBody(d.Hostname)},
			{"DELETE", "/api/v1/customers/" + c.ID, CustomerConfirmRequest{ConfirmName: c.Name, Reason: "closed"}},
			{"POST", "/api/v1/customers/" + summit.ID + "/purge-retired-devices", CustomerConfirmRequest{ConfirmName: summit.Name, Reason: "closed"}},
		} {
			res, b := pe.req(tc.method, tc.path, s, tc.body)
			if res.StatusCode != 403 || problemCode(t, res, b) != api.CodeForbidden {
				t.Errorf("%s %s as %s: %d %s, want 403 forbidden", tc.method, tc.path, role, res.StatusCode, b)
			}
		}
		// Any signed-in role may renew its own session's mark.
		if res, b := pe.req("POST", "/api/v1/auth/step-up", s, StepUpRequest{Code: pe.mfa.code("u-" + role)}); res.StatusCode != 204 {
			t.Errorf("step-up as %s: %d %s", role, res.StatusCode, b)
		}
	}
	// Logins, MFA enrollments and step-ups write their own rows; no purge or delete may have happened.
	for _, a := range pe.actions(t, n0) {
		if strings.HasPrefix(a, "device.purge.") || a == "customer.delete" {
			t.Errorf("a non-owner caused %s", a)
		}
	}
	if dv := pe.deviceNamed("OLD-RECEPTION-PC"); dv.Purging() {
		t.Error("a non-owner marked the device")
	}
	if _, err := pe.st.GetCustomer(context.Background(), store.ScopeAll(), c.ID); err != nil {
		t.Errorf("a non-owner deleted the customer: %v", err)
	}
}

func TestAdr19ActiveDeviceIs409AndARepeatIsIdempotent(t *testing.T) {
	pe := newPurgeEnv(t)
	active := pe.deviceNamed("DR-OFFICE-PC")
	res, b := pe.req("DELETE", "/api/v1/devices/"+active.ID, pe.owner, purgeBody(active.Hostname))
	if res.StatusCode != 409 || problemCode(t, res, b) != api.CodeConflict || !strings.Contains(string(b), `"reason":"`+ReasonDeviceNotRetired+`"`) {
		t.Fatalf("active device: %d %s", res.StatusCode, b)
	}
	spare := pe.deviceNamed("SPARE-PC-09") // purging in the fixtures
	n0 := pe.auditRowCount()
	res, b = pe.req("DELETE", "/api/v1/devices/"+spare.ID, pe.owner, DevicePurgeRequest{ConfirmHostName: spare.Hostname})
	var out DevicePurgeResponse
	if res.StatusCode != 202 || json.Unmarshal(b, &out) != nil || out.Changed || out.Purge.LastErrorClass != "storage_unreachable" {
		t.Fatalf("repeat on a purging device: %d %s", res.StatusCode, b)
	}
	if pe.auditRowCount() != n0 {
		t.Fatal("a repeat wrote an audit row")
	}
	if res, b := pe.req("DELETE", "/api/v1/devices/01JZZZZZZZZZZZZZZZZZZZZZZZ", pe.owner, purgeBody("x")); res.StatusCode != 404 {
		t.Fatalf("unknown device: %d %s", res.StatusCode, b)
	}
	pe.noHostNames(t, b)
}

// ---- customers ----

func TestAdr19CustomerWithDevicesIs409WithTheCounts(t *testing.T) {
	pe := newPurgeEnv(t)
	c := pe.customerNamed("Summit Eye Care")
	l, err := pe.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{CustomerID: c.ID, IncludeRetired: true, Page: store.Page{Limit: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	var active, retired int
	for _, d := range l.Items {
		if d.RetiredAt == nil {
			active++
		} else {
			retired++
		}
	}
	if active == 0 || retired == 0 {
		t.Fatalf("fixture changed: %d active, %d retired", active, retired)
	}
	before := pe.digest()
	res, b := pe.req("DELETE", "/api/v1/customers/"+c.ID, pe.owner, CustomerConfirmRequest{ConfirmName: c.Name, Reason: "contract ended"})
	var p struct {
		Code            string
		Reason          string
		Active, Retired int
	}
	if res.StatusCode != 409 || json.Unmarshal(b, &p) != nil || p.Code != api.CodeConflict || p.Reason != ReasonCustomerHasDevices || p.Active != active || p.Retired != retired {
		t.Fatalf("customer with devices: %d %s; want 409 with active %d retired %d", res.StatusCode, b, active, retired)
	}
	if pe.digest() != before {
		t.Fatal("a refused delete changed stored data")
	}
}

func TestAdr19CustomerDeleteHappyPath(t *testing.T) {
	pe := newPurgeEnv(t)
	c := pe.customerNamed("Cedar Valley Veterinary")
	n0 := pe.auditRowCount()
	res, b := pe.req("DELETE", "/api/v1/customers/"+c.ID, pe.owner, CustomerConfirmRequest{ConfirmName: c.Name, Reason: "never onboarded"})
	if res.StatusCode != 204 {
		t.Fatalf("delete: %d %s", res.StatusCode, b)
	}
	if _, err := pe.st.GetCustomer(context.Background(), store.ScopeAll(), c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the customer is still there: %v", err)
	}
	if got := strings.Join(pe.actions(t, n0), ","); got != "customer.delete" {
		t.Fatalf("audit = %s", got)
	}
	if res, _ := pe.req("DELETE", "/api/v1/customers/"+c.ID, pe.owner, CustomerConfirmRequest{ConfirmName: c.Name, Reason: "again"}); res.StatusCode != 404 {
		t.Fatalf("second delete: %d", res.StatusCode)
	}
}

func TestAdr19PurgeRetiredDevicesOfACustomer(t *testing.T) {
	pe := newPurgeEnv(t)
	c := pe.customerNamed("Summit Eye Care")
	old := pe.deviceNamed("OLD-RECEPTION-PC")
	l, _ := pe.st.ListDevices(context.Background(), store.ScopeAll(), store.ListDevicesOptions{CustomerID: c.ID, IncludeRetired: true, Page: store.Page{Limit: 1000}})
	active := 0
	for _, d := range l.Items {
		if d.RetiredAt == nil {
			active++
		}
	}
	// A bad reason is refused before anything happens.
	if res, b := pe.req("POST", "/api/v1/customers/"+c.ID+"/purge-retired-devices", pe.owner, CustomerConfirmRequest{ConfirmName: c.Name}); res.StatusCode != 400 {
		t.Fatalf("no reason: %d %s", res.StatusCode, b)
	}
	n0 := pe.auditRowCount()
	res, b := pe.req("POST", "/api/v1/customers/"+c.ID+"/purge-retired-devices", pe.owner, CustomerConfirmRequest{ConfirmName: c.Name, Reason: "clear old hardware"})
	var out CustomerPurgeRetiredResponse
	if res.StatusCode != 202 || json.Unmarshal(b, &out) != nil || out.Requested != 1 || len(out.RequestedIDs) != 1 || out.RequestedIDs[0] != old.ID ||
		out.AlreadyPurging != 0 || out.ActiveUntouched != active {
		t.Fatalf("bulk: %d %s; want 1 requested (%s), %d active untouched", res.StatusCode, b, old.ID, active)
	}
	if got := strings.Join(pe.actions(t, n0), ","); got != "device.purge.request" {
		t.Fatalf("audit = %s", got)
	}
	for _, d := range l.Items {
		now, err := pe.st.GetDevice(context.Background(), store.ScopeAll(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d.RetiredAt == nil && now.Purging() {
			t.Errorf("an active device was marked: %s", d.ID)
		}
	}
	res, b = pe.req("POST", "/api/v1/customers/"+c.ID+"/purge-retired-devices", pe.owner, CustomerConfirmRequest{ConfirmName: c.Name, Reason: "again"})
	out = CustomerPurgeRetiredResponse{}
	if res.StatusCode != 202 || json.Unmarshal(b, &out) != nil || out.Requested != 0 || out.AlreadyPurging != 1 || len(out.RequestedIDs) != 0 {
		t.Fatalf("repeat: %d %s", res.StatusCode, b)
	}
	pe.noHostNames(t, b)
}
