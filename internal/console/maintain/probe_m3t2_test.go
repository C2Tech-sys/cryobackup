package maintain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

const probeRepoPassword = "probe-repo-password-0123456789abcdef"

// probeKeys adds the probe repository's password to the harness unwrapper.
type probeKeys struct{ *fakeKeys }

func (probeKeys) ProbeRepoPassword() (string, error) { return probeRepoPassword, nil }

func (e *tenv) withProbeKey() { e.w.keys = probeKeys{e.keys} }

// argsOf lists the first two arguments of every repository call, in order.
func (e *tenv) argsOf() []string {
	var out []string
	for _, c := range e.repoCalls() {
		a := c.Args[0]
		if len(c.Args) > 1 && c.Args[0] == "list" {
			a += " " + c.Args[1]
		}
		out = append(out, a)
	}
	return out
}

// ADR 0017 / §4.6 C02: the storage probe lists the probe repository's keys and
// reads its config, at <bucket>/<prefix>/probe with the probe password and no
// lock. On the very first probe, a missing repository is created once (restic
// init) and recorded; a console with no device still gets a passing probe.
func TestC08ProbeRepositoryCreatedOnceThenListedAndRead(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "list", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"}, {Cmd: "init", Stdout: "init.json"},
		{Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	if _, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID}); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if got := strings.Join(e.argsOf(), ","); got != "list keys,init,cat" {
		t.Fatalf("calls %s, want list keys,init,cat", got)
	}
	for _, c := range e.repoCalls() {
		if !strings.HasSuffix(c.Repo, "test-bucket/pre/probe") || c.Password != probeRepoPassword {
			t.Fatalf("call %v on %q (password from the probe key: %v)", c.Args, c.Repo, c.Password == probeRepoPassword)
		}
		if c.Args[0] == "list" && !strings.Contains(strings.Join(c.Args, " "), "--no-lock") {
			t.Fatalf("list without --no-lock writes a lock every probe: %v", c.Args)
		}
	}
	if _, err := e.st.GetSetting(e.ctx, SettingProbeRepo); err != nil {
		t.Fatalf("creation not recorded: %v", err)
	}
	if strings.Contains(e.logs.String(), probeRepoPassword) {
		t.Fatal("the probe repository password reached the log")
	}
}

// M3-T2 H1(a): once created, a missing probe repository (an emptied or
// re-pointed prefix) is repo_missing, never re-created and never a pass.
func TestC08ProbeRepositoryGoneIsRepoMissing(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "list", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"}, {Cmd: "init", Stdout: "init.json"},
		{Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	if err := e.st.PutSetting(e.ctx, SettingProbeRepo, []byte(`{"created_at":1}`), ""); err != nil {
		t.Fatal(err)
	}
	_, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	var ae *adminpath.Error
	if !errors.As(err, &ae) || ae.Code != adminpath.CodeRepoMissing {
		t.Fatalf("gone probe repository: %v, want repo_missing", err)
	}
	if got := strings.Join(e.argsOf(), ","); got != "list keys" {
		t.Fatalf("calls %s: a gone probe repository must not be re-created", got)
	}
}

// A healthy probe: list, then cat config; a storage failure on list is the answer.
func TestC08ProbeRepositoryHealthyAndUnreachable(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "list", Stdout: "inline:0123abcd\n"}, {Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	if _, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID}); err != nil {
		t.Fatalf("healthy: %v", err)
	}
	if got := strings.Join(e.argsOf(), ","); got != "list keys,cat" {
		t.Fatalf("calls %s", got)
	}
	e.scenario([]fakeRule{{Cmd: "list", Exit: 1, Stderr: "inline:Fatal: unable to open repository: dial tcp: connection refused\n"}})
	if _, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID}); err == nil {
		t.Fatal("a failed list passed the probe")
	}
}

// Without a probe key the worker cannot probe: an internal error, never a pass;
// and the reserved id is not a device for any other method.
func TestC08ProbeRepositoryNeedsItsKey(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "list", Stdout: "inline:x\n"}, {Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	_, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	var ae *adminpath.Error
	if !errors.As(err, &ae) || ae.Code != adminpath.CodeInternal {
		t.Fatalf("no probe key: %v", err)
	}
	if _, err := e.w.Snapshots(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID}); !errors.As(err, &ae) || ae.Code != adminpath.CodeNotFound {
		t.Fatalf("snapshots of the probe id: %v, want not_found", err)
	}
	if _, err := e.st.GetSetting(e.ctx, SettingProbeRepo); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("setting written without a probe: %v", err)
	}
}

// M3-T2 delta 2, M-2: with every concurrency slot taken (dumps, a scheduler
// pass, a browse), the probe waits at most ProbeSlotWait and answers busy,
// which the console reads as "not checked"; it never runs into the console's
// deadline as if storage were down.
func TestC08ProbeOnAFullWorkerIsBusy(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "list", Stdout: "inline:x\n"}, {Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	e.w.probeSlotWait = 100 * time.Millisecond
	for i := 0; i < cap(e.w.sem); i++ {
		e.w.sem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(e.w.sem); i++ {
			<-e.w.sem
		}
	}()
	start := time.Now()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second) // stands in for the console's deadline
	defer cancel()
	_, err := e.w.CatConfig(ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	var ae *adminpath.Error
	if !errors.As(err, &ae) || ae.Code != adminpath.CodeBusy {
		t.Fatalf("full worker: %v, want busy", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the probe waited %v for a slot", took)
	}
	if ProbeSlotWait <= 0 || ProbeSlotWait > 10*time.Second {
		t.Fatalf("ProbeSlotWait %v must be a few seconds", ProbeSlotWait)
	}
}

// M3-T2 delta 2, L1: a probe repository that already exists without the
// settings row (a rebuilt database, an init cut short) arms the guard on its
// first clean probe; when it later disappears the answer is repo_missing,
// never a silent re-create.
func TestC08PreExistingProbeRepositoryArmsTheGuard(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "list", Stdout: "inline:0123abcd\n"}, {Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	e.withProbeKey()
	if _, err := e.st.GetSetting(e.ctx, SettingProbeRepo); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row before the first probe: %v", err)
	}
	if _, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID}); err != nil {
		t.Fatalf("pre-existing repository: %v", err)
	}
	if _, err := e.st.GetSetting(e.ctx, SettingProbeRepo); err != nil {
		t.Fatalf("the guard was not armed by a clean probe: %v", err)
	}
	e.scenario([]fakeRule{{Cmd: "list", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"}, {Cmd: "init", Stdout: "init.json"},
		{Cmd: "cat", Stdout: "cat_config.json"}})
	_, err := e.w.CatConfig(e.ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	var ae *adminpath.Error
	if !errors.As(err, &ae) || ae.Code != adminpath.CodeRepoMissing {
		t.Fatalf("removed repository: %v, want repo_missing", err)
	}
	for _, c := range e.repoCalls() {
		if c.Args[0] == "init" {
			t.Fatal("a removed probe repository was re-created")
		}
	}
}
