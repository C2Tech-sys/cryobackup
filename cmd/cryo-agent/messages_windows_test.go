//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/c2tech-sys/cryobackup/internal/agent/service"
)

// formatEvent renders event id from the message table of the module file
// the way Event Viewer does (FormatMessage on the EventMessageFile, loaded
// as data, with the event's strings as the arguments).
func formatEvent(t *testing.T, file string, id uint32, insert string) (string, error) {
	t.Helper()
	h, err := windows.LoadLibraryEx(file, 0, windows.LOAD_LIBRARY_AS_DATAFILE)
	if err != nil {
		t.Fatalf("load %s as data: %v", file, err)
	}
	defer windows.FreeLibrary(h)
	arg, _ := windows.UTF16PtrFromString(insert)
	args := []uintptr{uintptr(unsafe.Pointer(arg))}
	buf := make([]uint16, 1024)
	n, err := windows.FormatMessage(windows.FORMAT_MESSAGE_FROM_HMODULE|windows.FORMAT_MESSAGE_ARGUMENT_ARRAY,
		uintptr(h), id, 0, buf, (*byte)(unsafe.Pointer(&args[0])))
	if err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// L-EVT-1: this package's build links messages_windows_amd64.syso, so the
// test binary carries the same message table as cryo-agent.exe. Every id the
// agent writes renders as exactly the agent's line (EventCreate.exe, the old
// message file, has no text for ids above 1000: Event Viewer showed
// "description cannot be found" for 1003, 1009 and 1010).
func TestEveryEventIDHasItsMessage(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range service.EventIDs {
		line := "probe line for " + strconv.Itoa(int(id))
		got, err := formatEvent(t, exe, id, line)
		if err != nil || got != line {
			t.Errorf("event %d renders as %q, %v; want %q (rerun deploy/windows/eventlog/build-messages.ps1)", id, got, err, line)
		}
	}
}

// L-EVT-1 end to end, elevated (the windows-latest CI runner): the source
// registered by EnsureEventSource names this executable, and an event above
// 1000 written through it comes back from the Event Log with its text, as
// Event Viewer shows it (Get-WinEvent .Message). It only runs where no real
// source exists and removes the source afterwards.
func TestEventAbove1000RendersElevated(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("not elevated: registering an Event Log source needs HKLM write access (the windows-latest CI runner is elevated)")
	}
	key := `SYSTEM\CurrentControlSet\Services\EventLog\Application\` + service.EventSource
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE); err == nil {
		k.Close()
		t.Skipf("the %q source is already registered here; not touching a real installation", service.EventSource)
	}
	t.Cleanup(func() { _ = eventlog.Remove(service.EventSource) })
	if err := service.EnsureEventSource(); err != nil {
		t.Fatalf("EnsureEventSource: %v", err)
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	mf, _, err := k.GetStringValue("EventMessageFile")
	k.Close()
	exe, _ := os.Executable()
	if err != nil || !strings.EqualFold(mf, exe) {
		t.Fatalf("EventMessageFile = %q, %v; want this executable %q", mf, err, exe)
	}

	el, err := service.OpenEventLogFresh()
	if err != nil {
		t.Fatalf("OpenEventLogFresh: %v", err)
	}
	defer el.Close()
	probe := "cryo-fix7-probe-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := el.Write("warning", 1009, probe); err != nil {
		t.Fatalf("Write: %v", err)
	}
	ps := `(Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='` + service.EventSource +
		`'; Id=1009} -MaxEvents 20 | Where-Object { $_.Properties[0].Value -eq '` + probe + `' } | Select-Object -First 1).Message`
	var msg string
	for i := 0; i < 10 && msg == ""; i++ {
		out, _ := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
		msg = strings.TrimSpace(string(out))
		if msg == "" {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if msg != probe {
		t.Fatalf("Event 1009 .Message = %q; want the line written (%q)", msg, probe)
	}
}
