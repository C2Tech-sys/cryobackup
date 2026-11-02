package adminpath

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// validBodies is one valid request body per method, for raw requests.
var validBodies = map[string]string{
	MethodPing:      `{}`,
	MethodInit:      `{"device_id":"dev1"}`,
	MethodCatConfig: `{"device_id":"dev1"}`,
	MethodSnapshots: `{"device_id":"dev1"}`,
	MethodLs:        `{"device_id":"dev1","snapshot_id":"latest","dir":"/"}`,
	MethodFind:      `{"device_id":"dev1","pattern":"x"}`,
	MethodDump:      `{"device_id":"dev1","snapshot_id":"latest","path":"/a"}`,
	MethodStats:     `{"device_id":"dev1","mode":"raw-data"}`,
	MethodUnlock:    `{"device_id":"dev1"}`,
	MethodForget:    `{"device_id":"dev1","keep_within":"30d","rules":{"keep_daily":7}}`,
	MethodPrune:     `{"device_id":"dev1"}`,
	MethodCheck:     `{"device_id":"dev1"}`,
}

func TestHTTPStatusTable(t *testing.T) {
	want := map[string]int{
		CodeInvalidRequest:      400,
		CodeNotFound:            404,
		CodeCustodyCustomerHeld: 409,
		CodeRepoMissing:         404,
		CodeWrongPassword:       401,
		CodeLocked:              423,
		CodeStorageUnreachable:  503,
		CodeResticFailed:        502,
		CodeTimeout:             504,
		CodeUnavailable:         503,
		CodeBusy:                503,
		CodeInternal:            500,
	}
	if len(Codes()) != len(want) {
		t.Fatalf("Codes() lists %d codes, want %d", len(Codes()), len(want))
	}
	for code, status := range want {
		if got := HTTPStatus(code); got != status {
			t.Errorf("HTTPStatus(%s) = %d, want %d", code, got, status)
		}
		if m := Message(code); m == "" || strings.ContainsAny(m, `\/`) {
			t.Errorf("Message(%s) = %q", code, m)
		}
	}
	if HTTPStatus("nonsense") != 500 || Message("nonsense") != Message(CodeInternal) {
		t.Error("an unknown code must behave as internal")
	}
	if NewError("nonsense", "").Code != CodeInternal {
		t.Error("NewError must fold an unknown code into internal")
	}
}

// TestErrorMapping: each code travels as its HTTP status and arrives at the
// client as an *Error with the same code and class and the fixed message, even
// when the backend put free text in Message.
func TestErrorMapping(t *testing.T) {
	fb := newFake()
	c, path := startServer(t, fb, ClientOptions{})
	classes := map[string]restic.ErrorClass{
		CodeInvalidRequest:      "",
		CodeNotFound:            "",
		CodeCustodyCustomerHeld: "",
		CodeRepoMissing:         restic.ClassRepoMissing,
		CodeWrongPassword:       restic.ClassWrongPassword,
		CodeLocked:              restic.ClassLock,
		CodeStorageUnreachable:  restic.ClassNetwork,
		CodeResticFailed:        restic.ClassUnknown,
		CodeTimeout:             "",
		CodeUnavailable:         "",
		CodeInternal:            restic.ClassInternal,
	}
	for _, code := range Codes() {
		t.Run(code, func(t *testing.T) {
			fb.setHook(func(context.Context, string) error {
				return &Error{Code: code, Class: classes[code], Message: "free text " + sentinel}
			})
			_, err := c.CatConfig(context.Background(), withID("e1"))
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("got %T %v, want *Error", err, err)
			}
			if e.Code != code || e.Class != classes[code] || e.Message != Message(code) {
				t.Errorf("got %+v, want code %s class %q message %q", *e, code, classes[code], Message(code))
			}
			status, body, _ := rawDo(t, path, "POST", "/v1/cat_config", "application/json", []byte(validBodies[MethodCatConfig]))
			if status != HTTPStatus(code) {
				t.Errorf("status %d, want %d", status, HTTPStatus(code))
			}
			if strings.Contains(string(body), "PHI") || strings.Contains(string(body), "free text") {
				t.Errorf("body carries backend text: %s", body)
			}
		})
	}
}

// TestErrorTranslation: errors that are not *Error become fixed codes, and a
// panic does not stop the server.
func TestErrorTranslation(t *testing.T) {
	fb := newFake()
	c, path := startServer(t, fb, ClientOptions{})
	cases := []struct {
		name string
		hook func(context.Context, string) error
		code string
	}{
		{"unknown error", func(context.Context, string) error { return errors.New("restic: " + sentinel) }, CodeInternal},
		{"wrapped deadline", func(context.Context, string) error { return fmt.Errorf("prune: %w", context.DeadlineExceeded) }, CodeTimeout},
		{"canceled", func(context.Context, string) error { return context.Canceled }, CodeUnavailable},
		{"validation error from the backend", func(context.Context, string) error { return invalidField("x") }, CodeInvalidRequest},
		{"wrapped *Error", func(context.Context, string) error {
			return fmt.Errorf("while %s: %w", sentinel, NewError(CodeLocked, restic.ClassLock))
		}, CodeLocked},
		{"unknown code", func(context.Context, string) error { return &Error{Code: "made_up", Message: sentinel} }, CodeInternal},
		{"unknown class", func(context.Context, string) error {
			return &Error{Code: CodeResticFailed, Class: "made_up_class"}
		}, CodeResticFailed},
		{"panic with a string", func(context.Context, string) error { panic("boom " + sentinel) }, CodeInternal},
		{"panic with an error", func(context.Context, string) error { panic(errors.New(sentinel)) }, CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb.setHook(tc.hook)
			_, err := c.Snapshots(context.Background(), withID("t1"))
			var e *Error
			if !errors.As(err, &e) || e.Code != tc.code {
				t.Fatalf("got %T %v, want code %s", err, err, tc.code)
			}
			if e.Message != Message(tc.code) {
				t.Errorf("message %q is not the fixed one", e.Message)
			}
			if tc.name == "unknown class" && e.Class != restic.ClassUnknown {
				t.Errorf("an unrecognised class must become unknown, got %q", e.Class)
			}
			if tc.name == "wrapped *Error" && e.Class != restic.ClassLock {
				t.Errorf("class lost: %q", e.Class)
			}
			status, body, _ := rawDo(t, path, "POST", "/v1/snapshots", "application/json", []byte(validBodies[MethodSnapshots]))
			if status != HTTPStatus(tc.code) {
				t.Errorf("status %d, want %d", status, HTTPStatus(tc.code))
			}
			if strings.Contains(string(body), "sentinel") || strings.Contains(string(body), "fixture") || strings.Contains(string(body), "boom") {
				t.Errorf("body leaks backend text: %s", body)
			}
		})
	}
	// Still serving after the panics.
	fb.setHook(nil)
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatalf("server stopped serving after a panic: %v", err)
	}
}

// TestPathFreeErrorBodies: whatever a backend returns or panics with, for every
// method, the response body and the client's error text contain no part of the
// sentinel path.
func TestPathFreeErrorBodies(t *testing.T) {
	fb := newFake()
	c, path := startServer(t, fb, ClientOptions{})
	failures := map[string]func(context.Context, string) error{
		"plain": func(context.Context, string) error {
			return errors.New("open " + sentinel + `\secret.docx: access is denied`)
		},
		"typed": func(context.Context, string) error {
			return &Error{Code: CodeResticFailed, Class: restic.ClassPermission, Message: sentinel}
		},
		"wrapped":    func(context.Context, string) error { return fmt.Errorf("%s: %w", sentinel, NewError(CodeNotFound, "")) },
		"panic":      func(context.Context, string) error { panic(sentinel) },
		"deadline":   func(context.Context, string) error { return fmt.Errorf("%s: %w", sentinel, context.DeadlineExceeded) },
		"validation": func(context.Context, string) error { return &ValidationError{Field: "x"} },
	}
	for name, hook := range failures {
		fb.setHook(hook)
		for _, m := range Methods() {
			status, body, hdr := rawDo(t, path, "POST", "/v1/"+m, "application/json", []byte(validBodies[m]))
			if status == http.StatusOK {
				t.Errorf("%s/%s: status 200", name, m)
			}
			for _, bad := range []string{"PHI", "sentinel", "fixture", "secret.docx", "denied", `\`} {
				if strings.Contains(string(body), bad) || strings.Contains(fmt.Sprint(hdr), bad) {
					t.Errorf("%s/%s: response contains %q: %s", name, m, bad, body)
				}
			}
			e := wireError(t, body)
			if e.Message != Message(e.Code) {
				t.Errorf("%s/%s: message %q is not the fixed one for %s", name, m, e.Message, e.Code)
			}
		}
	}
	// And through the client.
	fb.setHook(failures["plain"])
	_, err := c.Check(context.Background(), CheckRequest{DeviceRequest: withID("p1")})
	if err == nil || strings.Contains(err.Error(), "PHI") || strings.Contains(err.Error(), "fixture") {
		t.Errorf("client error leaks: %v", err)
	}
}

// TestUnserialisableResultIsInternal: a result JSON cannot encode (NaN) is an
// internal error, not a broken response.
func TestUnserialisableResultIsInternal(t *testing.T) {
	b := &nanBackend{fakeBackend: newFake()}
	c, _ := startServer(t, b, ClientOptions{})
	_, err := c.Stats(context.Background(), StatsRequest{DeviceRequest: withID("n1"), Mode: restic.StatsRawData})
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeInternal {
		t.Fatalf("got %v, want internal", err)
	}
}

type nanBackend struct{ *fakeBackend }

func (n *nanBackend) Stats(context.Context, StatsRequest) (restic.StatsResult, error) {
	return restic.StatsResult{CompressionRatio: math.NaN()}, nil
}

// TestClientNormalisesWireErrors: a server that answers with free text, an
// unknown code or garbage does not get its text through the client.
func TestClientNormalisesWireErrors(t *testing.T) {
	for name, body := range map[string]string{
		"free text":    `{"error":{"code":"locked","class":"lock","message":"` + `C:\\fixture\\PHI-sentinel` + `"}}`,
		"unknown code": `{"error":{"code":"made_up","message":"PHI-sentinel"}}`,
		"not json":     `<html>PHI-sentinel</html>`,
		"no envelope":  `{"oops":"PHI-sentinel"}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := decodeError([]byte(body))
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("got %T", err)
			}
			if e.Message != Message(e.Code) || strings.Contains(e.Error(), "PHI") {
				t.Errorf("not normalised: %+v", *e)
			}
		})
	}
	if e := decodeError([]byte(`{"error":{"code":"locked","class":"lock"}}`)).(*Error); e.Code != CodeLocked || e.Class != restic.ClassLock {
		t.Errorf("good body mangled: %+v", *e)
	}
}

// TestValidation: invalid requests are refused by the client before anything is
// sent, and by the server when sent raw. The backend is never reached.
func TestValidation(t *testing.T) {
	fb := newFake()
	c, path := startServer(t, fb, ClientOptions{})
	ctx := context.Background()
	d := func(id string) DeviceRequest { return DeviceRequest{DeviceID: id, RequestID: "v1"} }
	good := d("dev1")
	cases := []struct {
		name   string
		method string
		req    any
		call   func() error
	}{
		{"device id empty", MethodCatConfig, DeviceRequest{RequestID: "v1"}, func() error { _, err := c.CatConfig(ctx, d("")); return err }},
		{"device id with a slash", MethodSnapshots, d("a/b"), func() error { _, err := c.Snapshots(ctx, d("a/b")); return err }},
		{"device id with a leading dash", MethodInit, InitRequest{d("-rf")}, func() error { _, err := c.Init(ctx, InitRequest{d("-rf")}); return err }},
		{"device id too long", MethodCatConfig, d(strings.Repeat("a", 65)), func() error { _, err := c.CatConfig(ctx, d(strings.Repeat("a", 65))); return err }},
		{"device id with a dot dot", MethodCatConfig, d("../x"), func() error { _, err := c.CatConfig(ctx, d("../x")); return err }},
		{"request id with a space", MethodCatConfig, DeviceRequest{DeviceID: "dev1", RequestID: "a b"}, func() error {
			_, err := c.CatConfig(ctx, DeviceRequest{DeviceID: "dev1", RequestID: "a b"})
			return err
		}},
		{"ls snapshot with a leading dash", MethodLs, LsRequest{DeviceRequest: good, SnapshotID: "-abcdef12", Dir: "/"}, func() error {
			_, err := c.Ls(ctx, LsRequest{DeviceRequest: good, SnapshotID: "-abcdef12", Dir: "/"})
			return err
		}},
		{"ls snapshot too short", MethodLs, LsRequest{DeviceRequest: good, SnapshotID: "abc", Dir: "/"}, func() error {
			_, err := c.Ls(ctx, LsRequest{DeviceRequest: good, SnapshotID: "abc", Dir: "/"})
			return err
		}},
		{"ls snapshot with a subfolder", MethodLs, LsRequest{DeviceRequest: good, SnapshotID: "latest:sub", Dir: "/"}, func() error {
			_, err := c.Ls(ctx, LsRequest{DeviceRequest: good, SnapshotID: "latest:sub", Dir: "/"})
			return err
		}},
		{"ls dir without a slash", MethodLs, LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "C/Users"}, func() error {
			_, err := c.Ls(ctx, LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "C/Users"})
			return err
		}},
		{"ls dir empty", MethodLs, LsRequest{DeviceRequest: good, SnapshotID: "latest"}, func() error { _, err := c.Ls(ctx, LsRequest{DeviceRequest: good, SnapshotID: "latest"}); return err }},
		{"ls dir with a control character", MethodLs, LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/a\nb"}, func() error {
			_, err := c.Ls(ctx, LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/a\nb"})
			return err
		}},
		{"ls limit negative", MethodLs, LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/", Limit: -1}, func() error {
			_, err := c.Ls(ctx, LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/", Limit: -1})
			return err
		}},
		{"ls limit too large", MethodLs, LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/", Limit: MaxLsLimit + 1}, func() error {
			_, err := c.Ls(ctx, LsRequest{DeviceRequest: good, SnapshotID: "latest", Dir: "/", Limit: MaxLsLimit + 1})
			return err
		}},
		{"find pattern empty", MethodFind, FindRequest{DeviceRequest: good}, func() error { _, err := c.Find(ctx, FindRequest{DeviceRequest: good}); return err }},
		{"find pattern with a leading dash", MethodFind, FindRequest{DeviceRequest: good, Pattern: "-x"}, func() error { _, err := c.Find(ctx, FindRequest{DeviceRequest: good, Pattern: "-x"}); return err }},
		{"find snapshot with a leading dash", MethodFind, FindRequest{DeviceRequest: good, Pattern: "x", SnapshotIDs: []string{"-aaaaaaaa"}}, func() error {
			_, err := c.Find(ctx, FindRequest{DeviceRequest: good, Pattern: "x", SnapshotIDs: []string{"-aaaaaaaa"}})
			return err
		}},
		{"find too many snapshots", MethodFind, FindRequest{DeviceRequest: good, Pattern: "x", SnapshotIDs: make([]string, 65)}, func() error {
			_, err := c.Find(ctx, FindRequest{DeviceRequest: good, Pattern: "x", SnapshotIDs: make([]string, 65)})
			return err
		}},
		{"find limit too large", MethodFind, FindRequest{DeviceRequest: good, Pattern: "x", Limit: MaxFindLimit + 1}, func() error {
			_, err := c.Find(ctx, FindRequest{DeviceRequest: good, Pattern: "x", Limit: MaxFindLimit + 1})
			return err
		}},
		{"dump snapshot with a leading dash", MethodDump, DumpRequest{DeviceRequest: good, SnapshotID: "-aaaaaaaa", Path: "/a"}, func() error {
			_, err := c.Dump(ctx, DumpRequest{DeviceRequest: good, SnapshotID: "-aaaaaaaa", Path: "/a"}, io.Discard)
			return err
		}},
		{"dump path empty", MethodDump, DumpRequest{DeviceRequest: good, SnapshotID: "latest"}, func() error {
			_, err := c.Dump(ctx, DumpRequest{DeviceRequest: good, SnapshotID: "latest"}, io.Discard)
			return err
		}},
		{"dump path relative", MethodDump, DumpRequest{DeviceRequest: good, SnapshotID: "latest", Path: "a/b"}, func() error {
			_, err := c.Dump(ctx, DumpRequest{DeviceRequest: good, SnapshotID: "latest", Path: "a/b"}, io.Discard)
			return err
		}},
		{"stats mode debug", MethodStats, StatsRequest{DeviceRequest: good, Mode: "debug"}, func() error { _, err := c.Stats(ctx, StatsRequest{DeviceRequest: good, Mode: "debug"}); return err }},
		{"stats mode empty", MethodStats, StatsRequest{DeviceRequest: good}, func() error { _, err := c.Stats(ctx, StatsRequest{DeviceRequest: good}); return err }},
		{"unlock remove-all without quiesced", MethodUnlock, UnlockRequest{DeviceRequest: good, RemoveAll: true}, func() error {
			_, err := c.Unlock(ctx, UnlockRequest{DeviceRequest: good, RemoveAll: true, Quiesced: false})
			return err
		}},
		{"forget without keep-within", MethodForget, ForgetRequest{DeviceRequest: good, Rules: KeepRules{KeepDaily: 7}}, func() error {
			_, err := c.Forget(ctx, ForgetRequest{DeviceRequest: good, Rules: KeepRules{KeepDaily: 7}})
			return err
		}},
		{"forget keep-within malformed", MethodForget, ForgetRequest{DeviceRequest: good, KeepWithin: "soon"}, func() error {
			_, err := c.Forget(ctx, ForgetRequest{DeviceRequest: good, KeepWithin: "soon"})
			return err
		}},
		{"forget keep count below -1", MethodForget, ForgetRequest{DeviceRequest: good, KeepWithin: "1d", Rules: KeepRules{KeepWeekly: -2}}, func() error {
			_, err := c.Forget(ctx, ForgetRequest{DeviceRequest: good, KeepWithin: "1d", Rules: KeepRules{KeepWeekly: -2}})
			return err
		}},
		{"forget tag with a comma", MethodForget, ForgetRequest{DeviceRequest: good, KeepWithin: "1d", KeepTags: []string{"a,b"}}, func() error {
			_, err := c.Forget(ctx, ForgetRequest{DeviceRequest: good, KeepWithin: "1d", KeepTags: []string{"a,b"}})
			return err
		}},
		{"prune max-unused malformed", MethodPrune, PruneRequest{DeviceRequest: good, MaxUnused: "lots"}, func() error { _, err := c.Prune(ctx, PruneRequest{DeviceRequest: good, MaxUnused: "lots"}); return err }},
		{"prune max-repack-size malformed", MethodPrune, PruneRequest{DeviceRequest: good, MaxRepackSize: "5%"}, func() error {
			_, err := c.Prune(ctx, PruneRequest{DeviceRequest: good, MaxRepackSize: "5%"})
			return err
		}},
		{"check subset malformed", MethodCheck, CheckRequest{DeviceRequest: good, ReadDataSubset: "bogus"}, func() error {
			_, err := c.Check(ctx, CheckRequest{DeviceRequest: good, ReadDataSubset: "bogus"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := fb.total()
			err := tc.call()
			var e *Error
			if !errors.As(err, &e) || e.Code != CodeInvalidRequest {
				t.Fatalf("client: got %v, want invalid_request", err)
			}
			body := mustJSON(t, tc.req)
			if tc.name == "unlock remove-all without quiesced" {
				body = []byte(`{"device_id":"dev1","remove_all":true}`)
			}
			status, resp, _ := rawDo(t, path, "POST", "/v1/"+tc.method, "application/json", body)
			if status != 400 {
				t.Errorf("server: status %d, want 400 (%s)", status, resp)
			}
			if we := wireError(t, resp); we.Code != CodeInvalidRequest {
				t.Errorf("server: code %q", we.Code)
			}
			if fb.total() != before {
				t.Error("the backend was reached")
			}
		})
	}
}

// TestValidRequestsPass guards the other direction: the boundary cases that are
// valid are accepted.
func TestValidRequestsPass(t *testing.T) {
	d := DeviceRequest{DeviceID: "01JABCDEFGHJKMNPQRSTVWXYZ0", RequestID: "req_1-x"}
	for name, v := range map[string]validator{
		"device":          d,
		"ls roots":        LsRequest{DeviceRequest: d, SnapshotID: "latest", Dir: "/"},
		"ls hex 64":       LsRequest{DeviceRequest: d, SnapshotID: strings.Repeat("a", 64), Dir: "/C/Users/x y", Limit: MaxLsLimit},
		"find defaults":   FindRequest{DeviceRequest: d, Pattern: "*.docx"},
		"find ids":        FindRequest{DeviceRequest: d, Pattern: "x", SnapshotIDs: []string{"latest", "ABCDEF12"}, Limit: MaxFindLimit},
		"dump subfolder":  DumpRequest{DeviceRequest: d, SnapshotID: "abcdef12:/C/x", Path: "/"},
		"stats":           StatsRequest{DeviceRequest: d, Mode: restic.StatsRawData},
		"unlock stale":    UnlockRequest{DeviceRequest: d},
		"unlock all":      UnlockRequest{DeviceRequest: d, RemoveAll: true, Quiesced: true},
		"forget":          ForgetRequest{DeviceRequest: d, KeepWithin: "1y2m3d4h", Rules: KeepRules{KeepLast: -1, KeepDaily: 7}, KeepTags: []string{"legal"}},
		"prune empty":     PruneRequest{DeviceRequest: d},
		"prune full":      PruneRequest{DeviceRequest: d, MaxUnused: "unlimited", MaxRepackSize: "500M", RepackCacheableOnly: true},
		"check structure": CheckRequest{DeviceRequest: d},
		"check subset":    CheckRequest{DeviceRequest: d, ReadDataSubset: "1/10"},
		"init":            InitRequest{d},
	} {
		if err := v.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestValidationErrorIsPathFree: the error names a field, never a value.
func TestValidationErrorIsPathFree(t *testing.T) {
	err := LsRequest{DeviceRequest: DeviceRequest{DeviceID: "dev1"}, SnapshotID: "latest", Dir: sentinel}.Validate()
	if err == nil || strings.Contains(err.Error(), "PHI") || strings.Contains(err.Error(), "fixture") {
		t.Fatalf("validation error: %v", err)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "dir" {
		t.Errorf("got %#v", err)
	}
}

// TestProtocol covers the HTTP edges: routes, verbs, media type, JSON strictness.
func TestProtocol(t *testing.T) {
	fb := newFake()
	_, path := startServer(t, fb, ClientOptions{})
	json := "application/json"
	for _, tc := range []struct {
		name, verb, route, ctype, body string
		status                         int
		code                           string
	}{
		{"unknown route", "POST", "/v1/nope", json, `{}`, 404, CodeNotFound},
		{"no version prefix", "POST", "/ls", json, `{}`, 404, CodeNotFound},
		{"GET on a POST route", "GET", "/v1/ls", "", ``, 405, CodeInvalidRequest},
		{"DELETE on ping", "DELETE", "/v1/ping", "", ``, 405, CodeInvalidRequest},
		{"wrong media type", "POST", "/v1/init", "text/plain", `{"device_id":"dev1"}`, 415, CodeInvalidRequest},
		{"missing media type", "POST", "/v1/init", "", `{"device_id":"dev1"}`, 415, CodeInvalidRequest},
		{"not json", "POST", "/v1/init", json, `device_id=dev1`, 400, CodeInvalidRequest},
		{"empty body", "POST", "/v1/init", json, ``, 400, CodeInvalidRequest},
		{"unknown field", "POST", "/v1/init", json, `{"device_id":"dev1","password":"x"}`, 400, CodeInvalidRequest},
		{"trailing data", "POST", "/v1/init", json, `{"device_id":"dev1"} {}`, 400, CodeInvalidRequest},
		{"wrong type", "POST", "/v1/init", json, `{"device_id":7}`, 400, CodeInvalidRequest},
		{"media type with charset", "POST", "/v1/init", "application/json; charset=utf-8", `{"device_id":"dev1"}`, 200, ""},
		{"GET ping", "GET", "/v1/ping", "", ``, 200, ""},
		{"POST ping", "POST", "/v1/ping", json, `{}`, 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, hdr := rawDo(t, path, tc.verb, tc.route, tc.ctype, []byte(tc.body))
			if status != tc.status {
				t.Fatalf("status %d, want %d (%s)", status, tc.status, body)
			}
			if got := hdr.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("content type %q", got)
			}
			if tc.code != "" {
				if e := wireError(t, body); e.Code != tc.code {
					t.Errorf("code %q, want %q", e.Code, tc.code)
				}
			}
		})
	}
	if fb.count(MethodPing) != 2 || fb.count(MethodInit) != 1 {
		t.Errorf("backend calls: %v", fb.calls)
	}
}

func TestHandlerNilBackendPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Handler(nil) must panic")
		}
	}()
	Handler(nil)
}

func TestClientImplementsBackend(t *testing.T) {
	var b Backend = NewClient("unused", ClientOptions{})
	if reflect.TypeOf(b).String() != "*adminpath.Client" {
		t.Error("unexpected type")
	}
}
