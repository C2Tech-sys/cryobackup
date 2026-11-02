package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// FIX20: a --repair-acl quarantine started from "install" or "enroll" writes
// Event Log 1007 exactly as "run" does, the source is made to exist first, and
// an Event Log that cannot be reached never stops the repair and is never
// dropped silently.

type recordedEvent struct {
	level string
	id    uint32
	msg   string
}

// fakeEventLog stands in for service.EnsureEventSource + OpenEventLog + Write.
type fakeEventLog struct {
	mu       sync.Mutex
	opens    int
	closes   int
	events   []recordedEvent
	openErr  error // the source could not be registered or opened
	writeErr error // every write fails
	// openedBefore records len(events) at each open (always 0: the log is open
	// before PrepareState can write to it).
	openedBefore []int
}

func useFakeEventLog(t *testing.T, f *fakeEventLog) {
	t.Helper()
	old := openEventSink
	t.Cleanup(func() { openEventSink = old })
	openEventSink = func() (eventSink, func(), error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.opens++
		f.openedBefore = append(f.openedBefore, len(f.events))
		if f.openErr != nil {
			return nil, nil, f.openErr
		}
		return func(level string, id uint32, msg string) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.writeErr != nil {
					return f.writeErr
				}
				f.events = append(f.events, recordedEvent{level, id, msg})
				return nil
			}, func() {
				f.mu.Lock()
				f.closes++
				f.mu.Unlock()
			}, nil
	}
}

func (f *fakeEventLog) ids() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uint32
	for _, e := range f.events {
		out = append(out, e.id)
	}
	return out
}

func has1007(f *fakeEventLog) bool {
	for _, id := range f.ids() {
		if id == eventStateQuarantined {
			return true
		}
	}
	return false
}

func TestInstallRepairWritesEventLog1007(t *testing.T) {
	stubElevatedACL(t)
	f := &fakeEventLog{}
	useFakeEventLog(t, f)
	l := squat(t)

	rep, err := PrepareInstallState(l.Root, true)
	if err != nil || rep.Quarantined == "" || !rep.Verified {
		t.Fatalf("install --repair-acl: %+v %v", rep, err)
	}
	if !has1007(f) {
		t.Fatalf("events = %v, want Event Log %d", f.ids(), eventStateQuarantined)
	}
	for _, e := range f.events {
		if e.id == eventStateQuarantined && e.level != "warning" {
			t.Errorf("1007 level = %q, want warning (as run writes it)", e.level)
		}
	}
	if f.opens != 1 || f.closes != 1 || f.openedBefore[0] != 0 {
		t.Errorf("opens %d closes %d (events before open %v): the source must be ensured and opened once, before the repair, and closed", f.opens, f.closes, f.openedBefore)
	}
	if rep.EventLogErr != "" {
		t.Errorf("EventLogErr = %q on a healthy Event Log", rep.EventLogErr)
	}
}

func TestInstallWithoutRepairDoesNotTouchTheEventLog(t *testing.T) {
	acl := stubElevatedACL(t)
	f := &fakeEventLog{}
	useFakeEventLog(t, f)
	l := verifiedTree(t, acl)
	if _, err := PrepareInstallState(l.Root, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if f.opens != 0 {
		t.Errorf("the Event Log was opened %d times without --repair-acl", f.opens)
	}
}

// An unelevated --repair-acl is refused by PrepareState; it must not also
// try to register an Event Log source it has no right to.
func TestInstallRepairUnelevatedDoesNotOpenTheEventLog(t *testing.T) {
	stubElevatedACL(t)
	stateElevated = func() bool { return false }
	f := &fakeEventLog{}
	useFakeEventLog(t, f)
	l := squat(t)
	if _, err := PrepareInstallState(l.Root, true); !errors.Is(err, ErrACLRefused) {
		t.Fatalf("unelevated --repair-acl: %v, want ErrACLRefused", err)
	}
	if f.opens != 0 {
		t.Errorf("the Event Log was opened %d times by a process that cannot repair", f.opens)
	}
}

func TestInstallRepairProceedsWhenTheEventLogCannotBeOpened(t *testing.T) {
	stubElevatedACL(t)
	f := &fakeEventLog{openErr: errors.New("access is denied")}
	useFakeEventLog(t, f)
	l := squat(t)

	rep, err := PrepareInstallState(l.Root, true)
	if err != nil || rep.Quarantined == "" || !rep.Verified {
		t.Fatalf("the repair must proceed without an Event Log: %+v %v", rep, err)
	}
	if !strings.Contains(rep.EventLogErr, "access is denied") || !strings.Contains(rep.EventLogErr, "quarantine") {
		t.Errorf("EventLogErr = %q; the failure must be reported for stderr, naming the quarantine", rep.EventLogErr)
	}
	if strings.Contains(rep.EventLogErr, l.Root) {
		t.Errorf("EventLogErr names the state directory: %q", rep.EventLogErr)
	}
}

func TestInstallRepairProceedsWhenAWriteFails(t *testing.T) {
	stubElevatedACL(t)
	f := &fakeEventLog{writeErr: errors.New("the event log is full")}
	useFakeEventLog(t, f)
	l := squat(t)

	rep, err := PrepareInstallState(l.Root, true)
	if err != nil || rep.Quarantined == "" {
		t.Fatalf("the repair must proceed when the write fails: %+v %v", rep, err)
	}
	if !strings.Contains(rep.EventLogErr, "1007") || !strings.Contains(rep.EventLogErr, "the event log is full") {
		t.Errorf("EventLogErr = %q; a dropped 1007 must be reported", rep.EventLogErr)
	}
}

func TestEnrollRepairWritesEventLog1007(t *testing.T) {
	stubElevatedACL(t)
	f := &fakeEventLog{}
	useFakeEventLog(t, f)
	fc := newFakeConsole(t)
	l := squat(t)

	res, err := Enroll(context.Background(), EnrollOptions{
		StateDir: l.Root, ServerURL: fc.URL(), Token: testEnrollToken, InsecureLab: true,
		InsecurePlaintextSecrets: plainSecrets(), AllowUnprotectedState: true, RepairACL: true,
	})
	if err != nil {
		t.Fatalf("enroll --repair-acl: %v", err)
	}
	if res.Quarantined == "" {
		t.Fatal("nothing was quarantined")
	}
	if !has1007(f) {
		t.Fatalf("events = %v, want Event Log %d", f.ids(), eventStateQuarantined)
	}
	if f.opens != 1 || f.closes != 1 {
		t.Errorf("opens %d closes %d, want one of each", f.opens, f.closes)
	}
	if res.EventLogErr != "" {
		t.Errorf("EventLogErr = %q on a healthy Event Log", res.EventLogErr)
	}
}

func TestEnrollRepairReportsAnEventLogThatCannotBeOpened(t *testing.T) {
	stubElevatedACL(t)
	f := &fakeEventLog{openErr: errors.New("access is denied")}
	useFakeEventLog(t, f)
	fc := newFakeConsole(t)
	l := squat(t)

	res, err := Enroll(context.Background(), EnrollOptions{
		StateDir: l.Root, ServerURL: fc.URL(), Token: testEnrollToken, InsecureLab: true,
		InsecurePlaintextSecrets: plainSecrets(), AllowUnprotectedState: true, RepairACL: true,
	})
	if err != nil || res.Quarantined == "" {
		t.Fatalf("the enrollment must go ahead: %+v %v", res, err)
	}
	if !strings.Contains(res.EventLogErr, "access is denied") {
		t.Errorf("EventLogErr = %q; the failure must reach stderr", res.EventLogErr)
	}
}

func TestEnrollWithoutRepairDoesNotTouchTheEventLog(t *testing.T) {
	f := &fakeEventLog{}
	useFakeEventLog(t, f)
	fc := newFakeConsole(t)
	_ = enroll(t, fc)
	if f.opens != 0 {
		t.Errorf("the Event Log was opened %d times by a plain enroll", f.opens)
	}
}
