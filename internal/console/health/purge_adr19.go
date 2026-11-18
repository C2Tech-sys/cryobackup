package health

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The "Purge not finished" banner (ADR 0019, "When it fails").
//
// A purge marks a retired device, then the device-purge job removes its repository through the
// maintenance worker, and only when the worker reports the prefix empty is the row deleted. Until
// then the device is "purging". This banner is raised, critical, whenever that is not going to
// finish by itself:
//
//   - a purging device whose last attempt failed (purge_last_error_class set: storage unreachable,
//     worker unreachable, objects left behind, a refusal);
//   - a purging device purging for longer than PurgeStuckAfter with no failure recorded: the job
//     is not running, or not reaching the device;
//   - the device-purge job's own last run failed (its job_state row): it could not read the list
//     or record what happened;
//   - the purging devices cannot be read here. "Cannot tell" is the banner, never silence.
//
// It is computed from the store on every evaluation the Evaluator makes with PurgeBanner set,
// so it does not depend on the job: a console that never runs the job still raises it once
// PurgeStuckAfter has passed.
// It names the count and carries the first affected device's id in Subject, so a page can link to
// that device. It never carries a host name: device ids and fixed class words only.
//
// Its ID is not a §4.6 C-id: it is about devices an owner chose to destroy, not about a console
// component, so it has no row on the System health page (adminapi healthRowBanners maps C-ids).

// BannerPurgeNotFinished is the banner's ID.
const BannerPurgeNotFinished = "purge_not_finished"

// PurgeJob is the job_state row of the device-purge job (jobs.NameDevicePurge).
const PurgeJob = "device-purge"

// PurgeStuckAfter is how long a device may be purging, with no failure recorded, before the banner
// says so. One purge_repo call may take up to two hours (adminpath's timeout for the method), so
// the threshold leaves room for one full attempt and the job's next pass.
const PurgeStuckAfter = 3 * time.Hour

// PurgeDevice is what the banner needs of one purging device: no host name.
type PurgeDevice struct {
	ID             string
	RequestedAt    time.Time
	LastAttemptAt  *time.Time
	LastErrorClass string
}

// PurgeFacts is everything PurgeBanners judges. The zero value is "nothing purging, nothing wrong".
type PurgeFacts struct {
	// NotRead is set when the purging devices could not be read: the banner then says so.
	NotRead bool
	// Devices are the purging devices inside the evaluation's scope.
	Devices []PurgeDevice
	// JobErrorClass is the class of the device-purge job's last run when that run failed; empty
	// when it passed, has not run, or its row could not be read (the job rows' own banners cover
	// an unreadable job table).
	JobErrorClass string
}

// PurgeBanners turns the facts into at most one banner, critical. It is pure: same facts and
// time, same answer.
func PurgeBanners(f PurgeFacts, now time.Time) []state.Banner {
	var failed, stuck int
	var first string
	classes := map[string]bool{}
	devs := append([]PurgeDevice(nil), f.Devices...)
	sort.SliceStable(devs, func(i, j int) bool { // oldest request first: the link goes to the one waiting longest
		if !devs[i].RequestedAt.Equal(devs[j].RequestedAt) {
			return devs[i].RequestedAt.Before(devs[j].RequestedAt)
		}
		return devs[i].ID < devs[j].ID
	})
	for _, d := range devs {
		switch {
		case d.LastErrorClass != "":
			failed++
			classes[d.LastErrorClass] = true
		case now.Sub(d.RequestedAt) > PurgeStuckAfter:
			stuck++
		default:
			continue
		}
		if first == "" {
			first = d.ID
		}
	}
	var parts []string
	if f.NotRead {
		parts = append(parts, "the purging devices could not be read, so a purge that is not finishing cannot be shown")
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%s still %s backups in storage after a failed attempt (%s)", devices(failed), holdVerb(failed), classList(classes)))
	}
	if stuck > 0 {
		parts = append(parts, fmt.Sprintf("%s %s been purging for more than %d hours with no attempt finished", devices(stuck), haveVerb(stuck), int(PurgeStuckAfter/time.Hour)))
	}
	if f.JobErrorClass != "" {
		parts = append(parts, fmt.Sprintf("the device-purge job's last run failed (%s)", f.JobErrorClass))
	}
	if len(parts) == 0 {
		return nil
	}
	text := "Purge not finished: " + strings.Join(parts, "; ") + "."
	if failed+stuck > 0 {
		text += " The device stays purging and its record stays until storage reports the repository gone; the job retries on its schedule."
	}
	return []state.Banner{{ID: BannerPurgeNotFinished, Subject: first, Severity: state.SeverityCritical, Count: failed + stuck, Text: text}}
}

func devices(n int) string {
	if n == 1 {
		return "1 retired device"
	}
	return fmt.Sprintf("%d retired devices", n)
}

func holdVerb(n int) string {
	if n == 1 {
		return "holds its"
	}
	return "hold their"
}

func haveVerb(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func classList(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// purgeFacts reads the purging devices inside scope and the job's last run. A failed read is
// NotRead, never an empty list.
func (e *Evaluator) purgeFacts(ctx context.Context, scope store.Scope, jobs []store.JobState, jerr error) PurgeFacts {
	var f PurgeFacts
	list, err := e.Store.ListDevicesPurging(ctx, scope)
	if err != nil {
		f.NotRead = true
	}
	for _, d := range list {
		if d.PurgeRequestedAt == nil {
			continue
		}
		f.Devices = append(f.Devices, PurgeDevice{ID: d.ID, RequestedAt: d.PurgeRequestedAt.UTC(), LastAttemptAt: d.PurgeLastAttemptAt, LastErrorClass: d.PurgeLastErrorClass})
	}
	if jerr == nil {
		for _, j := range jobs {
			if j.Name == PurgeJob && j.LastStatus == "error" {
				f.JobErrorClass = j.LastErrorClass
				if f.JobErrorClass == "" {
					f.JobErrorClass = "unknown"
				}
			}
		}
	}
	return f
}
