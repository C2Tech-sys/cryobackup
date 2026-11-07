package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
)

// M3-HARDEN (CI red on windows-latest): a sync in flight is the normal state right after a
// fault is repaired, and the device page's short read then meets a busy slot. It said the file
// "could not be read" and the gateway "may still accept" the login, though the newest clean read,
// taken after the retire, had found it gone. The page reads that read.
func TestRetiredPageWithASyncInFlightSaysPending(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.dev["FRONTDESK-01"]
	r := e.postForm("/ui/devices/"+id+"/retire", vals("reason", "gone", "confirm", "FRONTDESK-01"))
	if !strings.Contains(e.location(r), "cmd=retired") {
		t.Fatalf("retire went to %q", e.location(r))
	}
	after := e.now.Add(time.Minute)
	e.now = after.Add(time.Minute)

	// Gone at a clean read after the retire, and a run holds the slot now.
	e.loginFile = func() jobs.LoginFileCheck {
		return jobs.LoginFileCheck{At: e.now, ReadClass: jobs.LoginFileBusy, CleanAt: after}
	}
	// An old copy of the file may have been put back since that clean read: only a read now proves
	// the login is gone, so a busy read is pending.
	page := e.page("/devices/"+id, 200)
	mustContain(t, "busy, gone at an older clean read", page, "may still accept")
	if strings.Contains(page, "finds no login for it") {
		t.Errorf("a busy read answered removed from an older clean read")
	}

	// The newest clean read still holds the login: that is what the page says.
	e.loginFile = func() jobs.LoginFileCheck {
		return jobs.LoginFileCheck{At: e.now, ReadClass: jobs.LoginFileBusy, CleanAt: after, InFile: []string{id}}
	}
	mustContain(t, "busy, in the file at the newest clean read", e.page("/devices/"+id, 200), "gateway login not removed")

	// A clean read from before the retire proves nothing about it.
	e.loginFile = func() jobs.LoginFileCheck {
		return jobs.LoginFileCheck{At: e.now, ReadClass: jobs.LoginFileBusy, CleanAt: after.Add(-time.Hour)}
	}
	mustContain(t, "busy, no clean read since the retire", e.page("/devices/"+id, 200), "may still accept")

	// No clean read at all: the same.
	e.loginFile = func() jobs.LoginFileCheck { return jobs.LoginFileCheck{At: e.now, ReadClass: jobs.LoginFileBusy} }
	mustContain(t, "busy, no clean read", e.page("/devices/"+id, 200), "may still accept")

	// A file that cannot be read now is not answered from an old read, however clean.
	e.loginFile = func() jobs.LoginFileCheck {
		return jobs.LoginFileCheck{At: e.now, ReadClass: jobs.LoginFileUnreadable, CleanAt: after}
	}
	mustContain(t, "unreadable", e.page("/devices/"+id, 200), "may still accept")
}
