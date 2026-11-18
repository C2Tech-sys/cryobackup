package maintain

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The tests of purge_repo (ADR 0019) run the worker against the fake program of
// helpers_test.go used as BOTH restic and rclone: rclone's subcommands (size,
// purge, lsjson) are scripted with the same rules, and every invocation is
// recorded in fake.calls. No real rclone, no remote and no rclone.conf of the
// operator are involved (RCLONE_CONFIG points into the test's own harness
// directory, which holds only the fake's script).

// purgeSentinel is an object name the fake lists after a purge; it must never
// reach the log or an error.
const purgeSentinel = "PHI-sentinel-object"

// newPurgeEnv is newTenv with the fake as Config.RcloneProgram and the prefix
// the fixtures record in devices.repo_path ("fleet/<id>/").
func newPurgeEnv(t *testing.T, rules []fakeRule, mut func(*Config)) *tenv {
	t.Helper()
	exe, _ := fakeResticPath(t)
	return newTenv(t, rules, func(c *Config) {
		c.RcloneProgram = filepath.ToSlash(exe)
		c.Prefix = "fleet"
		if mut != nil {
			mut(c)
		}
	})
}

// retiredID returns a retired fixtures device (one with a maintenance row).
func (e *tenv) retiredID() string {
	e.t.Helper()
	var id string
	err := e.st.Read(e.ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT d.id FROM devices d JOIN maintenance_schedule m ON m.device_id = d.id
			WHERE d.retired_at IS NOT NULL ORDER BY d.id LIMIT 1`).Scan(&id)
	})
	if err != nil {
		e.t.Fatalf("no retired device in the fixtures: %v", err)
	}
	return id
}

// rcloneCalls are the recorded invocations of rclone's subcommands.
func (e *tenv) rcloneCalls() []fakeCall {
	var out []fakeCall
	for _, c := range e.calls() {
		if len(c.Args) > 0 && (c.Args[0] == "size" || c.Args[0] == "purge" || c.Args[0] == "lsjson") {
			out = append(out, c)
		}
	}
	return out
}

// lease returns the device's maintenance lease holder ("" when free).
func (e *tenv) lease(id string) string {
	e.t.Helper()
	var h sql.NullString
	if err := e.st.Read(e.ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT lease_holder FROM maintenance_schedule WHERE device_id = ?`, id).Scan(&h)
	}); err != nil {
		e.t.Fatal(err)
	}
	return h.String
}

func purgeReq(id string) adminpath.PurgeRepoRequest {
	return adminpath.PurgeRepoRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: id, RequestID: "purge-test"}}
}

// noLeak fails when the log or an error text names the storage location, the
// command line or an object.
func (e *tenv) noLeak(t *testing.T, err error) {
	t.Helper()
	text := e.logs.String()
	if err != nil {
		text += err.Error()
	}
	for _, bad := range []string{"test-bucket", "fleet/", "admin:", "rclone-test", purgeSentinel, "--json", "lsjson -R", "no such host"} {
		if strings.Contains(text, bad) {
			t.Errorf("log or error names %q:\n%s", bad, text)
		}
	}
}

var (
	ruleSize5   = fakeRule{Cmd: "size", Stdout: `inline:{"count":5,"bytes":1234,"sizeless":0}` + "\n"}
	rulePurgeOK = fakeRule{Cmd: "purge"}
	ruleListNil = fakeRule{Cmd: "lsjson", Stdout: "inline:[\n]\n"}
	ruleLeft2   = fakeRule{Cmd: "lsjson", Stdout: `inline:[` + "\n" +
		`{"Path":"data/aa/` + purgeSentinel + `","Name":"` + purgeSentinel + `","Size":100,"IsDir":false},` + "\n" +
		`{"Path":"index/` + purgeSentinel + `","Name":"x","Size":34,"IsDir":false}` + "\n]\n"}
	notFound = "inline:2026/11/17 10:00:00 ERROR : error listing: directory not found\n"
)

func TestPurgeRepoHappyPathADR19(t *testing.T) {
	e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleListNil}, nil)
	id := e.retiredID()
	res, err := e.w.PurgeRepo(e.ctx, purgeReq(id))
	if err != nil {
		t.Fatalf("PurgeRepo: %v", err)
	}
	if res.ObjectsRemoved != 5 || res.BytesRemoved != 1234 || res.RemainingObjects != 0 || res.ErrorClass != "" {
		t.Fatalf("result = %+v, want 5 objects, 1234 bytes removed, none left", res)
	}
	calls := e.rcloneCalls()
	want := [][]string{
		{"size", "--json", "--", "admin:test-bucket/fleet/" + id},
		{"purge", "--", "admin:test-bucket/fleet/" + id},
		{"lsjson", "-R", "--files-only", "--no-modtime", "--no-mimetype", "--", "admin:test-bucket/fleet/" + id},
	}
	if len(calls) != len(want) {
		t.Fatalf("rclone calls = %d, want %d: %+v", len(calls), len(want), calls)
	}
	for i, c := range calls {
		if strings.Join(c.Args, " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %q, want %q", i, c.Args, want[i])
		}
		// The same minimal environment as restic, minus everything of restic's:
		// rclone needs no repository password and gets none.
		if c.RClone != e.cfg.RcloneConfig || c.Password != "" || c.Repo != "" || c.TmpDir != filepath.Join(e.scratch, "tmp") ||
			c.RCache != filepath.Join(e.scratch, "rclone") {
			t.Errorf("call %d environment = %+v", i, c)
		}
		for _, n := range c.EnvNames {
			if strings.HasPrefix(n, "RESTIC_") {
				t.Errorf("call %d got %s", i, n)
			}
		}
	}
	if got := e.keys.called(); len(got) != 0 {
		t.Errorf("purge asked for a repository key: %v", got)
	}
	if h := e.lease(id); h != "" {
		t.Errorf("lease left held by %q", h)
	}
	if !strings.Contains(e.logs.String(), "repository purge finished") {
		t.Errorf("no finish line in the log:\n%s", e.logs.String())
	}
	e.noLeak(t, nil)
}

func TestPurgeRepoAbsentPathIsSuccessADR19(t *testing.T) {
	for name, size := range map[string]fakeRule{
		"directory not found": {Cmd: "size", Exit: 3, Stderr: notFound},
		"empty":               {Cmd: "size", Stdout: `inline:{"count":0,"bytes":0,"sizeless":0}`},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPurgeEnv(t, []fakeRule{size, rulePurgeOK, {Cmd: "lsjson", Exit: 3, Stdout: "inline:[\n", Stderr: notFound}}, nil)
			id := e.retiredID()
			for i := 0; i < 2; i++ { // idempotent: the second call is the same
				res, err := e.w.PurgeRepo(e.ctx, purgeReq(id))
				if err != nil || res.ObjectsRemoved != 0 || res.BytesRemoved != 0 || res.RemainingObjects != 0 || res.ErrorClass != "" {
					t.Fatalf("call %d = %+v, %v; want success with zeros", i, res, err)
				}
			}
			for _, c := range e.rcloneCalls() {
				if c.Args[0] == "purge" {
					t.Fatal("rclone purge ran on a prefix that holds nothing")
				}
			}
			if n := len(e.rcloneCalls()); n != 4 {
				t.Errorf("rclone calls = %d, want size and lsjson twice", n)
			}
		})
	}
}

func TestPurgeRepoRcloneFailsADR19(t *testing.T) {
	const dns = "inline:2026/11/17 10:00:00 ERROR : dial tcp: lookup s3.example: no such host\n"
	const denied = "inline:2026/11/17 10:00:00 ERROR : AccessDenied: Access Denied\n\tstatus code: 403, forbidden\n"

	t.Run("measure fails: nothing purged", func(t *testing.T) {
		e := newPurgeEnv(t, []fakeRule{{Cmd: "size", Exit: 1, Stderr: dns}, rulePurgeOK, ruleListNil}, nil)
		id := e.retiredID()
		_, err := e.w.PurgeRepo(e.ctx, purgeReq(id))
		wantCode(t, err, adminpath.CodeStorageUnreachable, restic.ClassNetwork)
		if c := e.rcloneCalls(); len(c) != 1 || c[0].Args[0] != "size" {
			t.Fatalf("calls after a failed measurement = %+v", c)
		}
		if e.lease(id) != "" {
			t.Error("lease not released")
		}
		e.noLeak(t, err)
	})
	t.Run("purge fails, objects left: result with the class", func(t *testing.T) {
		e := newPurgeEnv(t, []fakeRule{ruleSize5, {Cmd: "purge", Exit: 1, Stderr: denied}, ruleLeft2}, nil)
		res, err := e.w.PurgeRepo(e.ctx, purgeReq(e.retiredID()))
		if err != nil {
			t.Fatalf("err = %v, want a result that reports what is left", err)
		}
		if res.RemainingObjects != 2 || res.ObjectsRemoved != 3 || res.BytesRemoved != 1100 || res.ErrorClass != restic.ClassAuth {
			t.Fatalf("result = %+v, want 3 removed, 1100 bytes, 2 left, class auth", res)
		}
		e.noLeak(t, err)
	})
	t.Run("purge fails, nothing left: success", func(t *testing.T) {
		e := newPurgeEnv(t, []fakeRule{ruleSize5, {Cmd: "purge", Exit: 1, Stderr: denied}, ruleListNil}, nil)
		res, err := e.w.PurgeRepo(e.ctx, purgeReq(e.retiredID()))
		if err != nil || res.RemainingObjects != 0 || res.ObjectsRemoved != 5 {
			t.Fatalf("= %+v, %v; the listing proves the prefix empty", res, err)
		}
	})
	t.Run("purge and listing fail: the purge's error", func(t *testing.T) {
		e := newPurgeEnv(t, []fakeRule{ruleSize5, {Cmd: "purge", Exit: 1, Stderr: denied}, {Cmd: "lsjson", Exit: 1, Stderr: dns}}, nil)
		_, err := e.w.PurgeRepo(e.ctx, purgeReq(e.retiredID()))
		wantCode(t, err, adminpath.CodeResticFailed, restic.ClassAuth)
		e.noLeak(t, err)
	})
	t.Run("unreadable size output", func(t *testing.T) {
		e := newPurgeEnv(t, []fakeRule{{Cmd: "size", Stdout: "inline:not json"}, rulePurgeOK, ruleListNil}, nil)
		_, err := e.w.PurgeRepo(e.ctx, purgeReq(e.retiredID()))
		wantCode(t, err, adminpath.CodeResticFailed, restic.ClassParse)
		if c := e.rcloneCalls(); len(c) != 1 {
			t.Fatalf("calls = %+v, want the measurement only", c)
		}
	})
	t.Run("unreadable listing after the purge", func(t *testing.T) {
		e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, {Cmd: "lsjson", Stdout: "inline:{}"}}, nil)
		_, err := e.w.PurgeRepo(e.ctx, purgeReq(e.retiredID()))
		wantCode(t, err, adminpath.CodeResticFailed, restic.ClassParse)
	})
}

func TestPurgeRepoObjectsLeftADR19(t *testing.T) {
	e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleLeft2}, nil)
	id := e.retiredID()
	res, err := e.w.PurgeRepo(e.ctx, purgeReq(id))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.RemainingObjects != 2 || res.ObjectsRemoved != 3 || res.BytesRemoved != 1100 || res.ErrorClass != restic.ClassStorage {
		t.Fatalf("result = %+v, want 2 left (class storage), 3 removed, 1100 bytes", res)
	}
	if !strings.Contains(e.logs.String(), "repository purge incomplete") {
		t.Errorf("objects left after a purge must be logged loudly:\n%s", e.logs.String())
	}
	e.noLeak(t, nil)
}

func TestPurgeRepoContextCancelledADR19(t *testing.T) {
	e := newPurgeEnv(t, []fakeRule{ruleSize5, {Cmd: "purge", SleepMS: 20000}, ruleListNil}, nil)
	id := e.retiredID()
	ctx, cancel := context.WithCancel(e.ctx)
	go func() {
		for i := 0; i < 400; i++ { // cancel once the purge is running
			for _, c := range e.rcloneCalls() {
				if c.Args[0] == "purge" {
					cancel()
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	_, err := e.w.PurgeRepo(ctx, purgeReq(id))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("cancel took %v: the child was not stopped", d)
	}
	for _, c := range e.rcloneCalls() {
		if c.Args[0] == "lsjson" {
			t.Error("the listing ran after the call was cancelled")
		}
	}
	if h := e.lease(id); h != "" {
		t.Errorf("lease left held by %q after a cancel", h)
	}
}

// TestPurgeRepoRefusalsRunNothingADR19 is the table ADR 0019 asks for: every
// hostile or malformed device id and configuration is refused with a typed
// error, and no rclone process is started at all.
func TestPurgeRepoRefusalsRunNothingADR19(t *testing.T) {
	const ulid = "01HZY5X8M3K7Q2W9RTV4BN6CDE"
	type tc struct {
		id     string // "" = the retired fixtures device
		mut    func(*Config)
		reason string
		code   string
	}
	cfg := func(f func(*Config)) func(*Config) { return f }
	cases := map[string]tc{
		// device ids
		"empty id":           {id: "-", reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"probe repository":   {id: adminpath.ProbeRepoID, reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"lower-case ulid":    {id: strings.ToLower(ulid), reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"short id":           {id: ulid[:25], reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"long id":            {id: ulid + "0", reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"id over 7":          {id: "8" + ulid[1:], reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"id with U":          {id: ulid[:25] + "U", reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"dotdot id":          {id: "..", reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"traversal id":       {id: "01HZY5X8M3K7Q2W9RTV4/../..", reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"slash id":           {id: "01HZY5X8M3K7Q2W9RTV4BN6CD/", reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"wildcard id":        {id: "01HZY5X8M3K7Q2W9RTV4BN6CD*", reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"flag id":            {id: "--config=/etc/x", reason: PurgeRefusedDeviceID, code: adminpath.CodeInvalidRequest},
		"unknown device":     {id: ulid, reason: PurgeRefusedDeviceUnknown, code: adminpath.CodeNotFound},
		"active device":      {id: "active", reason: PurgeRefusedNotRetired, code: adminpath.CodeInvalidRequest},
		"repo_path mismatch": {mut: cfg(func(c *Config) { c.Prefix = "other" }), reason: PurgeRefusedRepoPath, code: adminpath.CodeUnavailable},
		// configuration
		"empty prefix":       {mut: cfg(func(c *Config) { c.Prefix = "" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"slash prefix":       {mut: cfg(func(c *Config) { c.Prefix = "//" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"dot prefix":         {mut: cfg(func(c *Config) { c.Prefix = "." }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"dotdot prefix":      {mut: cfg(func(c *Config) { c.Prefix = "fleet/.." }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"traversal prefix":   {mut: cfg(func(c *Config) { c.Prefix = "x/../fleet" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"empty segment":      {mut: cfg(func(c *Config) { c.Prefix = "a//fleet" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"wildcard prefix":    {mut: cfg(func(c *Config) { c.Prefix = "fl*" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"question prefix":    {mut: cfg(func(c *Config) { c.Prefix = "fleet?" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"brace prefix":       {mut: cfg(func(c *Config) { c.Prefix = "{fleet,x}" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"bracket prefix":     {mut: cfg(func(c *Config) { c.Prefix = "[f]leet" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"backslash prefix":   {mut: cfg(func(c *Config) { c.Prefix = `a\fleet` }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"colon prefix":       {mut: cfg(func(c *Config) { c.Prefix = "C:" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"control prefix":     {mut: cfg(func(c *Config) { c.Prefix = "fleet\x00" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"padded prefix":      {mut: cfg(func(c *Config) { c.Prefix = " fleet" }), reason: PurgeRefusedPrefix, code: adminpath.CodeUnavailable},
		"empty bucket":       {mut: cfg(func(c *Config) { c.Bucket = "" }), reason: PurgeRefusedBucket, code: adminpath.CodeUnavailable},
		"slash bucket":       {mut: cfg(func(c *Config) { c.Bucket = "/" }), reason: PurgeRefusedBucket, code: adminpath.CodeUnavailable},
		"dotdot bucket":      {mut: cfg(func(c *Config) { c.Bucket = "../etc" }), reason: PurgeRefusedBucket, code: adminpath.CodeUnavailable},
		"wildcard bucket":    {mut: cfg(func(c *Config) { c.Bucket = "*" }), reason: PurgeRefusedBucket, code: adminpath.CodeUnavailable},
		"drive bucket":       {mut: cfg(func(c *Config) { c.Bucket = "C:/data" }), reason: PurgeRefusedBucket, code: adminpath.CodeUnavailable},
		"empty remote":       {mut: cfg(func(c *Config) { c.Remote = "" }), reason: PurgeRefusedRemote, code: adminpath.CodeUnavailable},
		"one-letter remote":  {mut: cfg(func(c *Config) { c.Remote = "C" }), reason: PurgeRefusedRemote, code: adminpath.CodeUnavailable},
		"flag remote":        {mut: cfg(func(c *Config) { c.Remote = "-x" }), reason: PurgeRefusedRemote, code: adminpath.CodeUnavailable},
		"connection string":  {mut: cfg(func(c *Config) { c.Remote = "admin,provider=x" }), reason: PurgeRefusedRemote, code: adminpath.CodeUnavailable},
		"colon remote":       {mut: cfg(func(c *Config) { c.Remote = ":local" }), reason: PurgeRefusedRemote, code: adminpath.CodeUnavailable},
		"slash remote":       {mut: cfg(func(c *Config) { c.Remote = "a/b" }), reason: PurgeRefusedRemote, code: adminpath.CodeUnavailable},
		"spaced remote":      {mut: cfg(func(c *Config) { c.Remote = "ad min" }), reason: PurgeRefusedRemote, code: adminpath.CodeUnavailable},
		"missing rclone":     {mut: cfg(func(c *Config) { c.RcloneProgram = "/no/such/dir/rclone-missing" }), reason: PurgeRefusedRcloneProgram, code: adminpath.CodeUnavailable},
		"bad request id":     {id: "reqid", reason: PurgeRefusedRequestID, code: adminpath.CodeInvalidRequest},
		"dot segment bucket": {mut: cfg(func(c *Config) { c.Bucket = "a/./b" }), reason: PurgeRefusedBucket, code: adminpath.CodeUnavailable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleListNil}, nil)
			// Configuration is mutated after New on purpose: PurgeRepo must not
			// rely on Config.withDefaults (which already refuses some of these).
			if c.mut != nil {
				c.mut(&e.w.cfg)
			}
			req := purgeReq(e.retiredID())
			switch c.id {
			case "":
			case "-":
				req.DeviceID = ""
			case "active":
				req.DeviceID = e.esc
			case "reqid":
				req.RequestID = "../bad id"
			default:
				req.DeviceID = c.id
			}
			res, err := e.w.PurgeRepo(e.ctx, req)
			var r *PurgeRefusedError
			if !errors.As(err, &r) || r.Reason != c.reason {
				t.Fatalf("err = %v (result %+v), want refusal %q", err, res, c.reason)
			}
			wantCode(t, err, c.code, "")
			if calls := e.rcloneCalls(); len(calls) != 0 {
				t.Fatalf("a refused purge started rclone: %+v", calls)
			}
			if res != (adminpath.PurgeRepoResult{}) {
				t.Errorf("a refusal carries a result: %+v", res)
			}
			if e.lease(e.retiredID()) != "" {
				t.Error("a refusal took the lease")
			}
			if strings.Contains(r.Error(), "fleet") || strings.Contains(r.Error(), "test-bucket") {
				t.Errorf("refusal text names a value: %q", r.Error())
			}
		})
	}
}

// TestPurgeLocationADR19 pins the one location a purge may address.
func TestPurgeLocationADR19(t *testing.T) {
	const id = "01HZY5X8M3K7Q2W9RTV4BN6CDE"
	for _, c := range []struct{ bucket, prefix, want string }{
		{"b", "p", "admin:b/p/" + id},
		{"/b/", "/p/", "admin:b/p/" + id},
		{"b", "a/b/c", "admin:b/a/b/c/" + id},
		{"bucket-1.x", "cryo backups", "admin:bucket-1.x/cryo backups/" + id},
	} {
		loc, _, r := purgeLocation("admin", c.bucket, c.prefix, id)
		if r != nil || loc != c.want {
			t.Errorf("purgeLocation(%q, %q) = %q, %v; want %q", c.bucket, c.prefix, loc, r, c.want)
		}
		repo, err := restic.AdminRepository("admin", c.bucket, c.prefix, id)
		if err != nil || "rclone:"+loc != repo {
			t.Errorf("purge location %q is not restic's repository %q", loc, repo)
		}
	}
}

// TestPurgeRepoLeaseADR19: the purge takes the per-device maintenance lease;
// while it holds it the scheduler cannot lease the device and a second purge is
// refused as locked, and a lease held by someone else defers the purge without
// running rclone.
func TestPurgeRepoLeaseADR19(t *testing.T) {
	e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleListNil}, nil)
	id := e.retiredID()

	release, err := e.w.leasePurge(e.ctx, id)
	if err != nil {
		t.Fatalf("leasePurge: %v", err)
	}
	if h := e.lease(id); !strings.HasPrefix(h, e.w.holder+"-purge-") {
		t.Fatalf("lease holder = %q", h)
	}
	if got, err := e.w.leaseDevice(e.ctx, id, e.clk.Now()); err != nil || got {
		t.Fatalf("the scheduler leased a device under purge: %v, %v", got, err)
	}
	_, err = e.w.PurgeRepo(e.ctx, purgeReq(id))
	wantCode(t, err, adminpath.CodeLocked, restic.ClassLock)
	if c := e.rcloneCalls(); len(c) != 0 {
		t.Fatalf("a purge ran under another purge's lease: %+v", c)
	}
	release()
	if h := e.lease(id); h != "" {
		t.Fatalf("lease not released: %q", h)
	}

	// The scheduler holds it: the purge waits for it.
	if got, err := e.w.leaseDevice(e.ctx, id, e.clk.Now()); err != nil || !got {
		t.Fatalf("scheduler lease: %v, %v", got, err)
	}
	_, err = e.w.PurgeRepo(e.ctx, purgeReq(id))
	wantCode(t, err, adminpath.CodeLocked, restic.ClassLock)
	e.w.releaseDevice(e.ctx, id)

	// An expired lease of anyone is taken over.
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE maintenance_schedule SET lease_holder = 'gone', lease_until = ? WHERE device_id = ?`,
			e.clk.Now().Add(-time.Minute).UnixMilli(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.w.PurgeRepo(e.ctx, purgeReq(id)); err != nil {
		t.Fatalf("purge over an expired lease: %v", err)
	}
	if e.lease(id) != "" {
		t.Error("lease not released after the purge")
	}
}

// TestPurgeRepoWithoutMaintenanceRowADR19: a device that has no
// maintenance_schedule row has no lease to take; the purge still runs.
func TestPurgeRepoWithoutMaintenanceRowADR19(t *testing.T) {
	e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleListNil}, nil)
	id := e.retiredID()
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM maintenance_schedule WHERE device_id = ?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, err := e.w.PurgeRepo(e.ctx, purgeReq(id))
	if err != nil || res.ObjectsRemoved != 5 || res.RemainingObjects != 0 {
		t.Fatalf("= %+v, %v", res, err)
	}
}

// TestPurgeRepoCustomerHeldADR19: rclone needs no repository key, so a
// customer-held device is purged like any other and no key is asked for.
func TestPurgeRepoCustomerHeldADR19(t *testing.T) {
	e := newPurgeEnv(t, []fakeRule{ruleSize5, rulePurgeOK, ruleListNil}, nil)
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET retired_at = ?, retired_reason = 'test', paused_at = NULL, paused_until = NULL,
			pause_reason = NULL, paused_by = NULL WHERE id = ?`, e.clk.Now().UnixMilli(), e.held)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, err := e.w.PurgeRepo(e.ctx, purgeReq(e.held))
	if err != nil || res.ObjectsRemoved != 5 {
		t.Fatalf("= %+v, %v", res, err)
	}
	if got := e.keys.called(); len(got) != 0 {
		t.Errorf("purge asked for a key: %v", got)
	}
}
