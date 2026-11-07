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
func TestRetiredPageWithASyncInFlightReadsTheNewestCleanRead(t *testing.T) {
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
	page := e.page("/devices/"+id, 200)
	mustContain(t, "busy, gone at the newest clean read", page, "finds no login for it")
	for _, bad := range []string{"may still accept", "could not be read", "sync_busy"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page says %q though the newest clean read found the login gone", bad)
		}
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
