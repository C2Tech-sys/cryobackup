package ui

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The restore status page, GET /restores/{id} (B17; plan B.1 "Operator-visible failures"; ticket
// P22-G). It words every status of both kinds of restore: a download (ready, delivered, a
// download that was cut off, expired) and a restore to the device (queued with the device online
// or offline, running or waiting for the device's backup, restored, RESTORED WITH ERRORS with its
// counts by class, each failure class, cancelled, expired). The sentence of a device restore's
// outcome is the service's (restores.DescribeOutcome); this page adds what the operator does next.
// While the restore can still change, the live part polls itself (htmx) and stops at a final status.
// No path appears here: counts, classes, times and the host name only.

// RestoreView is the progress and outcome of one restore.
type RestoreView struct {
	ID       string
	DeviceID string
	Host     string
	// Device: a restore to the device (else a download).
	Device bool
	Status Chip
	// Headline is the outcome sentence; Message what it means and what to do. Class colours the banner.
	Headline string
	Message  string
	Class    string
	Facts    []Fact
	Counts   *RestoreCounts
	Bytes    string
	// CanDownload offers the download button (a POST that mints the single-use token).
	CanDownload bool
	CSRF        string
	// Poll: the live part refreshes itself every PollSeconds until the status is final.
	Poll        bool
	PollSeconds int
	Expires     string
	Created     string
	Steps       []Step
	Failed      bool
	// ZipHint links back to the wizard for a download while a device restore waits.
	ZipHint bool
}

// RestoreCounts are a device restore's numbers (restores.Outcome).
type RestoreCounts struct {
	Restored string
	Skipped  string
	Bytes    string
	Errors   []RestoreErrRow
	ErrTotal string
}

// RestoreErrRow is one error class of a restore with its count.
type RestoreErrRow struct {
	Class string
	Label string
	Count string
}

// RestoreStatusData is GET /restores/{id}.
type RestoreStatusData struct {
	Page
	R RestoreView
}

// restoreErrLabel says a restore error class in words; an unknown class shows as itself.
func restoreErrLabel(class string) string {
	switch class {
	case "access_denied":
		return "Windows refused access"
	case "target_locked":
		return "file held open by another program"
	case "guard_refused":
		return "refused by the safety check"
	case "nothing_matched":
		return "not found in the snapshot"
	case "interrupted":
		return "cut short"
	case "disk":
		return "not enough free space"
	case "other":
		return "other errors"
	}
	return class
}

func failureMessage(class string) string {
	switch class {
	case "network", "storage", "storage_unreachable", "unavailable", "timeout":
		return "The storage or the maintenance worker could not be reached. Nothing was restored. Try again once the conditions above clear."
	case "wrong_password", "auth":
		return "The repository key did not open the repository. Nothing was restored. The escrow copy may need verification."
	case "repo_missing":
		return "The device's repository does not exist yet. Nothing was restored."
	case "not_found":
		return "The file or snapshot was not found in the repository. Nothing was restored."
	case "custody_customer_held":
		return "The device's key is held by the customer. No technician restore is possible."
	case "cancelled":
		return "The restore was cancelled because the console was stopping. Nothing was restored."
	case "disconnected":
		return "The download was interrupted before it finished. Nothing was kept on the server; download again to start over."
	case "artifact_missing":
		return "The prepared zip is no longer on the server. Start the restore again."
	}
	return "The restore failed. Nothing was restored. The request id in the console log identifies it."
}

// deviceNext is what the operator does after a device restore ended with class.
func deviceNext(status, class string) string {
	switch {
	case status == "cancelled":
		return "The restore command was cancelled before the device started it. Nothing was restored."
	case status == "expired":
		return "Nothing was restored. Start a new restore, or download the files as a zip."
	case class == "interrupted":
		return "Files that were being written when it stopped may be incomplete. Run the restore again; files already in place are kept."
	case class == "auth":
		return "Nothing was restored. Check the device's gateway login (the device page shows it), then run the restore again."
	case class == "disk":
		return "Nothing was written. Free space on the device, or restore fewer items, then run the restore again."
	case class == "guard_refused":
		return "Nothing was written. The device's log says which folder; remove it and run the restore again."
	case class == "nothing_matched":
		return "Nothing was written. Pick the items again from the snapshot listing."
	case class == "repo_unreachable":
		return "Nothing was restored. Check the gateway and the device's network, then run the restore again."
	}
	return "Nothing more was restored. The device's log has the details."
}

// closedByConsole is what the operator does after the console closed a device restore whose
// agent stopped reporting it (store.DeviceRestoreProgress.ClosedBy, ADR 0032), and after the
// agent's late report of it.
func closedByConsole(host string, oc restores.Outcome) string {
	var why string
	switch oc.ClosedBy {
	case store.RestoreClosedAgentSilent:
		why = "Nothing came from " + host + " for " + strconv.Itoa(int(store.DeviceRestoreSilenceLimit/time.Hour)) + " hours while the restore was open, so the console closed it."
	case store.RestoreClosedAgentRestarted:
		why = "The agent on " + host + " restarted during the restore and never reported it, so the console closed it."
	default:
		why = host + " sent no result " + strconv.Itoa(int(store.DeviceRestoreRunningBound/time.Hour)) + " hours after it started the restore, so the console closed it."
	}
	if oc.Late != nil {
		return why + " " + host + " reported it afterwards (above). This restore stays closed: start a new one for anything still missing."
	}
	return why + " Files it placed before it stopped stay. Check the files on " + host + ", then run the restore again for anything missing; a late report from " + host + " is shown here."
}

func (s *Service) restoreView(ctx context.Context, p *auth.Principal, rr store.RestoreRequest, host string, live liveFacts) RestoreView {
	c := s.fc(ctx)
	v := RestoreView{CSRF: p.CSRF, ID: rr.ID, DeviceID: rr.DeviceID, Host: host, Created: rr.CreatedAt.In(c.loc()).Format("Mon 3:04:05 PM"),
		Expires: rr.ExpiresAt.In(c.loc()).Format("Mon 2 Jan 3:04 PM"), PollSeconds: 2}
	v.Steps = []Step{{1, "Snapshot", "done"}, {2, "Select", "done"}, {3, "Destination", "done"}, {4, "Review", "done"}}
	oc := restores.DescribeOutcome(rr)
	items := plural(rr.SelectionCount, "item", "items")
	if wm, ok := restores.WireMode(rr.Mode); ok {
		v.Device = true
		s.deviceRestoreView(ctx, &v, rr, oc, live, c)
		dest := "A new folder on " + host
		if string(wm) == restores.ModeDeviceOriginal {
			dest = "Original location, keep existing files"
			if wo, _ := restores.WireOverwrite(rr.Overwrite); wo == api.DeviceOverwriteIfChanged {
				dest = "Original location, replace changed files"
			}
		}
		v.Facts = []Fact{{Key: "Destination", Value: dest}, {Key: "Selected", Value: items}, {Key: "Requested", Value: c.dayClock(rr.CreatedAt), Mono: true}}
		if rr.FinishedAt != nil {
			v.Facts = append(v.Facts, Fact{Key: "Finished", Value: c.dayClock(*rr.FinishedAt), Mono: true})
		} else if rr.Status == "queued" || rr.Status == "running" {
			v.Facts = append(v.Facts, Fact{Key: "Waits until", Value: c.dayClock(rr.ExpiresAt), Mono: true})
		}
		if rr.Status == "running" && live.Offline {
			seen := "Never"
			if live.SeenKnown {
				seen = ageLong(c.Now.Sub(live.LastSeen)) + " (" + c.dayClock(live.LastSeen) + ")"
			}
			v.Facts = append(v.Facts, Fact{Key: "Last seen", Value: seen})
		}
		if oc.Late != nil {
			v.Facts = append(v.Facts, Fact{Key: "Late report", Value: c.dayClock(oc.Late.ReceivedAt), Mono: true})
		}
		return v
	}
	v.Headline = oc.Text
	v.Facts = []Fact{{Key: "Destination", Value: "Download as a zip"}, {Key: "Selected", Value: items}, {Key: "Requested", Value: c.dayClock(rr.CreatedAt), Mono: true}}
	switch rr.Status {
	case "queued":
		v.Status, v.Poll, v.Class = Chip{ID: "q", Label: "QUEUED", Class: ClassInfo, Pulse: true}, true, ClassInfo
		v.Message = "Waiting for the maintenance worker to start. This page refreshes by itself."
	case "running":
		v.Status, v.Poll, v.Class = Chip{ID: "q", Label: "RUNNING", Class: ClassInfo, Pulse: true}, true, ClassInfo
		v.Message = "Preparing the zip from the snapshot. Large files take a while. This page refreshes by itself."
	case "ready", "delivered":
		// ADR 0008: nothing is prepared or stored; the zip is built from the snapshot while it
		// downloads, so its size is known only afterwards.
		live := s.Restores != nil && s.Restores.Downloadable(rr)
		if rr.Bytes != nil {
			v.Bytes = byteSize(uint64(*rr.Bytes))
		}
		until := rr.ExpiresAt.In(c.loc()).Format("Mon 3:04 PM")
		if rr.Status == "ready" {
			v.Status, v.Class = Chip{ID: "q", Label: "READY", Class: ClassOK}, ClassOK
			v.Message = "Ready to download. The zip streams straight from the snapshot: nothing is stored on the server, and a dropped download starts again from the beginning. The link works once; a new one can be made until " + until + "."
		} else {
			v.Status, v.Class = Chip{ID: "q", Label: "DELIVERED", Class: ClassNeutral}, ClassInfo
			v.Message = "The zip was downloaded. Nothing was kept on the server; you can download it again until " + until + ", and it streams from the snapshot again."
			if oc.Skipped != nil && oc.Skipped.Total() > 0 {
				v.Message += " Links and other entries that are not regular files or folders were left out; the zip's CRYOBACKUP-SKIPPED.txt counts them by kind."
			}
		}
		if rr.ErrorClass != "" {
			v.Class = ClassWarn
			v.Message = "The last download did not finish (class: " + rr.ErrorClass + "). No zip was saved: a part-file in the browser's downloads is not a usable zip. " + v.Message
		}
		if !live {
			v.Class = ClassNeutral
			v.Message = "This restore has expired. Start another restore if you need the files again."
		}
		// No token in the page: the button is a POST that mints one and redirects.
		v.CanDownload = live && p.Role >= auth.RoleOperator
	case "failed":
		v.Status, v.Failed, v.Class = Chip{ID: "q", Label: "FAILED", Class: ClassCrit}, true, ClassCrit
		v.Message = failureMessage(rr.ErrorClass)
		if rr.ErrorClass != "" {
			v.Message += " Class: " + rr.ErrorClass + "."
		}
	default:
		v.Status, v.Class = Chip{ID: "q", Label: strings.ToUpper(rr.Status), Class: ClassNeutral}, ClassNeutral
		v.Message = "This restore is no longer available. Start another restore if you need the files again."
	}
	if oc.Skipped != nil && oc.Skipped.Total() > 0 {
		v.Facts = append(v.Facts, Fact{Key: "Left out", Value: plural(oc.Skipped.Total(), "link or special entry", "links or special entries")})
	}
	v.Facts = append(v.Facts, Fact{Key: "Expires", Value: c.dayClock(rr.ExpiresAt), Mono: true})
	return v
}

// deviceRestoreView fills v for a restore to the device.
func (s *Service) deviceRestoreView(ctx context.Context, v *RestoreView, rr store.RestoreRequest, oc restores.Outcome, live liveFacts, c fmtCtx) {
	v.Headline = oc.Text
	v.PollSeconds = 3
	switch rr.Status {
	case "queued":
		v.Status, v.Poll, v.Class, v.ZipHint = Chip{ID: "q", Label: "QUEUED", Class: ClassInfo, Pulse: true}, true, ClassInfo, true
		switch {
		case live.Offline && live.SeenKnown:
			v.Class = ClassWarn
			v.Headline = "Queued. " + v.Host + " is offline."
			v.Message = "Last seen " + ageLong(c.Now.Sub(live.LastSeen)) + " (" + c.dayClock(live.LastSeen) + "). Starts when online, until " + c.dayClock(rr.ExpiresAt) + ". For files now, use Download as zip."
		case live.Offline:
			v.Class = ClassWarn
			v.Headline = "Queued. " + v.Host + " has not been heard from."
			v.Message = "Starts when online, until " + c.dayClock(rr.ExpiresAt) + ". For files now, use Download as zip."
		default:
			v.Headline = "Queued. " + v.Host + " is online."
			v.Message = v.Host + " picks the restore up at its next check-in, normally within a minute. It waits until " + c.dayClock(rr.ExpiresAt) + ". This page refreshes by itself."
		}
	case "running":
		v.Status, v.Poll, v.Class = Chip{ID: "q", Label: "RUNNING", Class: ClassInfo, Pulse: true}, true, ClassInfo
		if live.Offline {
			// The device has gone quiet with the restore open (ticket P22-R2a, A1): never the
			// plain "Files are being restored." The sweep closes it as interrupted once the
			// silence passes store.DeviceRestoreSilenceLimit (ADR 0032).
			v.Status.Class, v.Status.Pulse, v.Class = ClassWarn, false, ClassWarn
			v.Headline = v.Host + " has not been heard from."
			heard := rr.CreatedAt
			if rr.StartedAt != nil {
				heard = *rr.StartedAt
			}
			if live.SeenKnown {
				v.Headline = v.Host + " has not been heard from since " + c.dayClock(live.LastSeen) + "."
				if live.LastSeen.After(heard) {
					heard = live.LastSeen
				}
			}
			closeAt := heard.Add(store.DeviceRestoreSilenceLimit)
			if rr.StartedAt != nil && rr.StartedAt.Add(store.DeviceRestoreRunningBound).Before(closeAt) {
				closeAt = rr.StartedAt.Add(store.DeviceRestoreRunningBound)
			}
			v.Message = "The outcome is unknown until it reports. If it stays silent, the console closes the restore as interrupted at about " +
				c.dayClock(closeAt) + ", and " + v.Host + " can take another restore."
		} else if live.BackupOpen {
			v.Status.Label = "WAITING"
			v.Headline = "Waiting for the running backup."
			v.Message = v.Host + " took the restore and runs one repository task at a time: it starts as soon as the backup finishes. This page refreshes by itself."
		} else {
			v.Headline = "Running on " + v.Host + "."
			v.Message = "Files are being restored. The counts arrive when the restore finishes; this page refreshes by itself."
		}
	case "delivered":
		v.Status, v.Class = Chip{ID: "q", Label: "RESTORED", Class: ClassOK}, ClassOK
		v.Message = "Files that already existed were kept and are counted as skipped."
		if rr.Overwrite == store.RestoreOverwriteIfChanged {
			v.Message = "Files that differed from the snapshot were replaced; files that already matched are counted as skipped."
		}
		if rr.Mode == store.RestoreModeDevicePath {
			v.Message = "The files are in the CryoBackup-Restore folder on the system drive of " + v.Host + ", in the folder named after this restore (" + rr.ID + ")."
		}
		if rr.ErrorClass == store.RestoreClassPartial {
			v.Status, v.Class = Chip{ID: "q", Label: "RESTORED WITH ERRORS", Class: ClassWarn}, ClassWarn
			v.Message = "Some files could not be written; they are counted by class below. The device's log names them. Run the restore again for the rest once the cause is cleared."
		}
	case "failed":
		v.Status, v.Failed, v.Class = Chip{ID: "q", Label: "FAILED", Class: ClassCrit}, true, ClassCrit
		v.Message = deviceNext(rr.Status, rr.ErrorClass)
		if oc.ClosedBy != "" {
			v.Message = closedByConsole(v.Host, oc)
			if oc.Late != nil {
				v.Class = ClassWarn // the device has spoken since; the chip keeps the console's verdict
			}
		}
	case "cancelled":
		v.Status, v.Class = Chip{ID: "q", Label: "CANCELLED", Class: ClassNeutral}, ClassNeutral
		v.Message = deviceNext(rr.Status, rr.ErrorClass)
	case "expired", "purged":
		v.Status, v.Class, v.ZipHint = Chip{ID: "q", Label: "EXPIRED", Class: ClassNeutral}, ClassNeutral, true
		v.Message = deviceNext("expired", "")
	default:
		v.Status, v.Class = Chip{ID: "q", Label: strings.ToUpper(rr.Status), Class: ClassNeutral}, ClassNeutral
	}
	if rr.Status == "delivered" || rr.Status == "failed" || rr.Status == "cancelled" {
		if oc.FilesRestored > 0 || oc.FilesSkipped > 0 || len(oc.Errors) > 0 || rr.Status == "delivered" {
			rc := &RestoreCounts{Restored: commas(uint64(max(oc.FilesRestored, 0))), Skipped: commas(uint64(max(oc.FilesSkipped, 0))), Bytes: byteSize(oc.BytesRestored)}
			var total int64
			for _, e := range oc.Errors {
				rc.Errors = append(rc.Errors, RestoreErrRow{Class: e.Class, Label: restoreErrLabel(e.Class), Count: commas(uint64(max(e.Count, 0)))})
				total += e.Count
			}
			if total > 0 {
				rc.ErrTotal = commas(uint64(total))
			}
			v.Counts = rc
		}
	}
}

func (s *Service) loadRestore(w http.ResponseWriter, r *http.Request, p *auth.Principal) (store.RestoreRequest, string, bool) {
	rr, err := s.Store.GetRestoreRequest(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, p, "This restore")
		return store.RestoreRequest{}, "", false
	}
	host := ""
	if d, err := s.Store.GetDevice(r.Context(), p.Scope, rr.DeviceID); err == nil {
		host = d.Hostname
	}
	return rr, host, true
}

func (s *Service) restoreStatus(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	rr, host, ok := s.loadRestore(w, r, p)
	if !ok {
		return
	}
	f, err := s.Fleet.Fleet(r.Context(), p.Scope)
	if err != nil {
		s.render(w, r, http.StatusServiceUnavailable, "error", "base", errorData(s.fallbackPage(r.Context(), p), 503, "Restore unavailable", "The fleet could not be loaded. Try again shortly."))
		return
	}
	v := s.restoreView(r.Context(), p, rr, host, s.restoreLiveFacts(r.Context(), p, f, rr.DeviceID))
	s.render(w, r, http.StatusOK, "restore_status", "base", RestoreStatusData{Page: s.page(r.Context(), p, f, "Restore", "fleet", "/ui/events"), R: s.withBusyNotice(r, v)})
}

// noticeBusy is adminapi.BusyNotice: the admin API sends a browser back to the restore page with
// ?notice=busy&at=<unix seconds> when every restore stream is in use.
const noticeBusy = "busy"

// busyMessage is what the restore page says then. The console does not know the worker's stream
// budget (CRYO_MAINTAIN_MAX_STREAMS), so it names no number.
const busyMessage = "Other restores are already downloading. Try again in a moment."

// busyNoticeTTL is how long the notice stays on the page and its live part.
const busyNoticeTTL = time.Minute

// withBusyNotice puts the busy notice in front of the message while it is current: the download
// was refused before it began, nothing was recorded or spent. The page reads it from its own URL;
// the live partial from the URL of the page it sits on (htmx's HX-Current-URL), so a refresh keeps
// it until the user acts (a new page) or busyNoticeTTL has passed.
func (s *Service) withBusyNotice(r *http.Request, v RestoreView) RestoreView {
	if !v.CanDownload {
		return v
	}
	q := r.URL.Query()
	if q.Get("notice") == "" {
		if cur := r.Header.Get("HX-Current-URL"); cur != "" {
			if u, err := url.Parse(cur); err == nil && u.Path == "/restores/"+v.ID {
				q = u.Query()
			}
		}
	}
	if q.Get("notice") != noticeBusy {
		return v
	}
	at, err := strconv.ParseInt(q.Get("at"), 10, 64)
	if err != nil {
		return v
	}
	if age := s.now().Sub(time.Unix(at, 0)); age < -5*time.Second || age > busyNoticeTTL {
		return v
	}
	v.Message = busyMessage + " " + v.Message
	return v
}

func (s *Service) restorePartial(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	rr, err := s.Store.GetRestoreRequest(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	host := ""
	if d, err := s.Store.GetDevice(r.Context(), p.Scope, rr.DeviceID); err == nil {
		host = d.Hostname
	}
	var f health.Fleet
	if fl, err := s.Fleet.Fleet(r.Context(), p.Scope); err == nil {
		f = fl
	}
	s.render(w, r, http.StatusOK, "restore_status", "restore_live", s.withBusyNotice(r, s.restoreView(r.Context(), p, rr, host, s.restoreLiveFacts(r.Context(), p, f, rr.DeviceID))))
}

// restoreDownload is POST /ui/restores/{id}/download: it mints the single-use token for this
// session and redirects to the GET URL, so a token never sits in page HTML waiting to be copied.
// The redirect is the only place it appears.
func (s *Service) restoreDownload(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	rr, err := s.Store.GetRestoreRequest(r.Context(), p.Scope, r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, p, "This restore")
		return
	}
	tok, _, err := s.Restores.MintToken(rr, p.SessionID)
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	if err != nil {
		// Not downloadable (expired, failed): back to the status page, which says why.
		http.Redirect(w, r, "/restores/"+rr.ID, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/api/v1/restores/"+rr.ID+"/download?token="+tok, http.StatusSeeOther)
}
