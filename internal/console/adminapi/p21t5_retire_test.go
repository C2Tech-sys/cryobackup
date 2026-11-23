package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
)

// P21-T5 (operator ruling 2026-11-23): when a retire's wait ends with a read of the file in flight,
// the answer is the newest read that finished during the wait, whatever it found; check_timed_out
// only when no read finished. These tests end the wait at a chosen read instead of on a timer, so
// no answer depends on how loaded the machine is.

// retireEndable retires the device through the handler with a request context the test ends.
// The retire's wait runs on a child of the request's context, so ending it ends the wait exactly as
// the bound does: the read in flight answers check_timed_out to the loop.
func retireEndable(t *testing.T, e *env, ctx context.Context, deviceID string) DeviceRetireResponse {
	t.Helper()
	owner := e.login("admin", pw)
	raw, _ := json.Marshal(DeviceRetireRequest{Reason: "r"})
	req := httptest.NewRequest("POST", "/api/v1/devices/"+deviceID+"/retire", bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for _, ck := range owner.cookies {
		req.AddCookie(ck)
	}
	req.Header.Set(auth.CSRFHeader, owner.csrf)
	rec := httptest.NewRecorder()
	e.srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("retire: %d %s", rec.Code, rec.Body.Bytes())
	}
	return decode[DeviceRetireResponse](t, rec.Body.Bytes())
}

// readsThenEnd answers the wait's reads from reads, in order. The read after them ends the wait
// (end) and never returns while the test runs, so it is the read in flight when the wait ends.
// n counts the reads that began.
func readsThenEnd(t *testing.T, end context.CancelFunc, n *atomic.Int32, reads ...func(context.Context) jobs.LoginFileCheck) func(context.Context) jobs.LoginFileCheck {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(ctx context.Context) jobs.LoginFileCheck {
		i := int(n.Add(1)) - 1
		if i < len(reads) {
			return reads[i](ctx)
		}
		end()
		<-release
		return jobs.LoginFileCheck{}
	}
}

func answer(c jobs.LoginFileCheck) func(context.Context) jobs.LoginFileCheck {
	return func(context.Context) jobs.LoginFileCheck { return c }
}

func TestRetireWaitThatEndsAnswersTheNewestFinishedRead(t *testing.T) {
	type want struct{ status, reason string }
	cases := []struct {
		name  string
		reads func(id string) []func(context.Context) jobs.LoginFileCheck
		want  want
	}{
		{"(a) a finished read, then the wait ends mid-read: that read's answer", func(id string) []func(context.Context) jobs.LoginFileCheck {
			return []func(context.Context) jobs.LoginFileCheck{answer(jobs.LoginFileCheck{InFile: []string{id}})}
		}, want{GatewayLoginPending, gatewayStillInFile}},
		{"(b) no read finished: check_timed_out", func(string) []func(context.Context) jobs.LoginFileCheck {
			return nil
		}, want{GatewayLoginPending, gatewayCheckTimedOut}},
		{"(c) a busy read, then the wait ends mid-read: sync_busy as before", func(string) []func(context.Context) jobs.LoginFileCheck {
			return []func(context.Context) jobs.LoginFileCheck{answer(jobs.LoginFileCheck{ReadClass: jobs.LoginFileBusy})}
		}, want{GatewayLoginPending, jobs.LoginFileBusy}},
		{"(d) a later finished read replaces a busy one", func(id string) []func(context.Context) jobs.LoginFileCheck {
			return []func(context.Context) jobs.LoginFileCheck{answer(jobs.LoginFileCheck{ReadClass: jobs.LoginFileBusy}), answer(jobs.LoginFileCheck{InFile: []string{id}})}
		}, want{GatewayLoginPending, gatewayStillInFile}},
		{"(d) a later busy read replaces one that found the login", func(id string) []func(context.Context) jobs.LoginFileCheck {
			return []func(context.Context) jobs.LoginFileCheck{answer(jobs.LoginFileCheck{InFile: []string{id}}), answer(jobs.LoginFileCheck{ReadClass: jobs.LoginFileBusy})}
		}, want{GatewayLoginPending, jobs.LoginFileBusy}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx, end := context.WithCancel(context.Background())
			defer end()
			reads := tc.reads(e.dev.ID)
			var n atomic.Int32
			e.svc.SyncGateway, e.svc.GatewaySyncWait = func() {}, time.Minute // only end() ends the wait
			e.svc.CheckGatewayLogins = readsThenEnd(t, end, &n, reads...)
			got := retireEndable(t, e, ctx, e.dev.ID)
			if got.GatewayLogin != tc.want.status || got.GatewayLoginReason != tc.want.reason {
				t.Fatalf("answer %s/%s, want %s/%s", got.GatewayLogin, got.GatewayLoginReason, tc.want.status, tc.want.reason)
			}
			if r := int(n.Load()); r != len(reads)+1 {
				t.Fatalf("%d reads began, want %d: the wait did not end at the held read", r, len(reads)+1)
			}
		})
	}
}

// A read that finds the login gone, or the file unreadable, still answers at once: a kept read never
// holds the wait open, and the answer is that read's, not an earlier one's.
func TestRetireReadThatSettlesItStillAnswersAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name           string
		last           jobs.LoginFileCheck
		status, reason string
	}{
		{"removed", jobs.LoginFileCheck{}, GatewayLoginRemoved, ""},
		{"unreadable", jobs.LoginFileCheck{ReadClass: jobs.LoginFileUnreadable}, GatewayLoginPending, jobs.LoginFileUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx, end := context.WithCancel(context.Background())
			defer end()
			var n atomic.Int32
			e.svc.SyncGateway, e.svc.GatewaySyncWait = func() {}, time.Minute
			e.svc.CheckGatewayLogins = readsThenEnd(t, end, &n, answer(jobs.LoginFileCheck{InFile: []string{e.dev.ID}}), answer(tc.last))
			began := time.Now()
			got := retireEndable(t, e, ctx, e.dev.ID)
			if got.GatewayLogin != tc.status || got.GatewayLoginReason != tc.reason {
				t.Fatalf("answer %s/%s, want %s/%s", got.GatewayLogin, got.GatewayLoginReason, tc.status, tc.reason)
			}
			if r := n.Load(); r != 2 {
				t.Fatalf("%d reads began, want 2: the settling read must answer at once", r)
			}
			if took := time.Since(began); took > 30*time.Second {
				t.Errorf("it waited %v after a read that settled it", took)
			}
		})
	}
}

// The same rule when the wait's own timer ends it. Whether the first read comes back before the
// timer is up to the machine, so the test holds the answer to the rule in each case it can see: a
// second read begins only after the loop has taken the first read's answer, so then the answer
// must be that read's. Otherwise the first read may or may not have come back in time.
func TestRetireWaitOnItsOwnTimerKeepsTheFinishedRead(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var n atomic.Int32
	e.svc.SyncGateway, e.svc.GatewaySyncWait = func() {}, 300*time.Millisecond
	e.svc.CheckGatewayLogins = func(context.Context) jobs.LoginFileCheck {
		if n.Add(1) == 1 {
			return jobs.LoginFileCheck{InFile: []string{e.dev.ID}}
		}
		<-release // in flight when the timer ends
		return jobs.LoginFileCheck{}
	}
	res, b := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/retire", e.login("admin", pw), DeviceRetireRequest{Reason: "r"})
	if res.StatusCode != 200 {
		t.Fatalf("retire: %d %s", res.StatusCode, b)
	}
	got := decode[DeviceRetireResponse](t, b)
	if got.GatewayLogin != GatewayLoginPending {
		t.Fatalf("%+v", got)
	}
	switch {
	case n.Load() >= 2:
		if got.GatewayLoginReason != gatewayStillInFile {
			t.Fatalf("the first read found the login and the timer ended the second: answer %q, want %q", got.GatewayLoginReason, gatewayStillInFile)
		}
	case got.GatewayLoginReason != gatewayStillInFile && got.GatewayLoginReason != gatewayCheckTimedOut:
		t.Fatalf("one read, then the timer: answer %q", got.GatewayLoginReason)
	default:
		t.Logf("the timer ended the wait before a second read began (answer %q): the machine is loaded", got.GatewayLoginReason)
	}
}
