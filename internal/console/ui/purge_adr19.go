package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ADR 0019 in the pages: purge a retired device, purge every retired device of a customer, and
// delete a customer that has no devices.
//
// Each dialog is the retire dialog's shape (a <details> popover holding a plain form, so it works
// with script off) and posts to a /ui route that goes through the admin API in process (apiSend),
// the one handler that checks and audits: owner role and session only (auth.RequireSession), fresh
// MFA (auth.RequireFreshMFA), the exact typed name, the reason. The /ui route is itself owner-only,
// so a forged form from another role stops at Require before anything else runs.
//
// Fresh MFA. When the session's last authenticator check is older than auth.FreshMFAWindow the same
// form carries an authenticator code field; the post first calls POST /api/v1/auth/step-up with it
// and then the destructive call, so nothing typed is lost. A refusal (wrong code, lockout, rate
// limit, a check that went stale while the dialog was open, a name that does not match) re-renders
// the page with the dialog open and what was typed kept, except the code. A recovery code is not
// accepted by the step-up, and the dialog says so.
//
// Nothing here logs a host name, a customer name, a reason or a code: device and customer ids,
// statuses and fixed classes only.

const (
	patDevicePurge          = "POST /ui/devices/{id}/purge"
	patCustomerDelete       = "POST /ui/customers/{id}/delete"
	patCustomerPurgeRetired = "POST /ui/customers/{id}/purge-retired"
)

func purgeUIPatterns() []string {
	return []string{patDevicePurge, patCustomerDelete, patCustomerPurgeRetired}
}

// PurgeUIRoles is the minimum role of each pattern, for the role-gating test: all owner (ADR 0019).
func PurgeUIRoles() map[string]string {
	return map[string]string{patDevicePurge: "OW", patCustomerDelete: "OW", patCustomerPurgeRetired: "OW"}
}

func (s *Service) registerPurgeUI(r httpx.Registrar) {
	ow := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOwner, auth.ModeUI, h) }
	r.Handle(patDevicePurge, ow(s.devicePurgePost))
	r.Handle(patCustomerDelete, ow(s.customerDeletePost))
	r.Handle(patCustomerPurgeRetired, ow(s.customerPurgeRetiredPost))
}

// ---- views ----

// Kinds of PurgeForm.
const (
	purgeKindDevice   = "device"   // purge one retired device
	purgeKindRetired  = "retired"  // purge every retired device of a customer
	purgeKindCustomer = "customer" // delete a customer with no devices
)

// PurgeForm is one of the three destructive dialogs of ADR 0019.
type PurgeForm struct {
	Kind   string
	ID     string // element id prefix, unique per kind
	Action string
	CSRF   string
	// Name is what must be typed exactly: the device's host name or the customer's name.
	Name string
	// What names the object in the button's reason ("device", "customer").
	What string
	// Customer is the customer's name (Kind retired); Count the devices that will be purged.
	Customer string
	Count    int
	// CountUnsure: the purging devices could not be read, so Count may include some already purging.
	CountUnsure bool
	Button      string
	BusyLabel   string
	// Open starts the dialog open (after a refusal).
	Open bool
	MFA  MFAStep
	// Kept after a refusal: what was typed (never the code).
	Typed, Reason string
	// Err is the refusal, at the top of the dialog; ConfirmErr, ReasonErr and CodeErr mark their field.
	Err                            *Note
	ConfirmErr, ReasonErr, CodeErr string
	// Busy renders the submitting state (the /design page; the script sets it on a real submit).
	Busy bool
}

// MFAStep is the fresh-MFA part of a dialog.
type MFAStep struct {
	// Need: the session's last authenticator check is older than the window, so a code is asked for.
	Need bool
	// Since and Until bound the current check when it is fresh (display zone, 12-hour clock).
	Since, Until, UntilISO string
	Zone                   string
	Window                 int // minutes
}

// PurgingView is the device page's statement about a purging device.
type PurgingView struct {
	Class string // info while it goes, crit when an attempt failed or nothing finished for too long
	Title string
	Lede  string
	// Requested is when the purge was asked for and By who asked (an account name, never a host name).
	Requested, RequestedISO, By string
	// Attempt is the last attempt's time ("" before the first).
	Attempt, AttemptISO string
	// ErrorText is the last failed attempt's class in words, ErrorClass the class word ("" when none).
	ErrorText, ErrorClass string
	Retry                 string
	Reason                string
}

// CustDeleteView is the "Delete customer" section of the customer page (owner only).
type CustDeleteView struct {
	Name                     string
	Active, Retired, Purging int
	ToPurge                  int
	// PurgeErr: the purging devices could not be read.
	PurgeErr bool
	// State is one of has_active, only_retired, all_purging, empty.
	State string
	Form  *PurgeForm
	// Notice is the answer of the last action on this section.
	Notice *CauseView
}

func init() {
	for k, v := range map[string]string{
		"device.purge.request": "Device purge requested",
		"device.purge.done":    "Device purged",
		"customer.delete":      "Customer deleted",
		auth.ActionStepUp:      "MFA step-up",
	} {
		if _, ok := auditActionText[k]; !ok {
			auditActionText[k] = v
		}
		if _, ok := auditActionLabels[k]; !ok {
			auditActionLabels[k] = v
		}
	}
	for k, v := range map[string]CauseView{
		"purge_requested":    {Class: ClassInfo, Title: "Purge started.", Text: "The console is removing this device's backups and escrowed key from storage. It retries by itself until storage reports the repository gone; then the record is deleted and this page answers not found."},
		"purge_already":      {Class: ClassInfo, Title: "Already purging.", Text: "A purge of this device was requested before. Nothing was written again: the first request's reason and time stand."},
		"err_purge_active":   {Class: ClassCrit, Title: "Not purged.", Text: "Only a retired device can be purged. Retire it first."},
		"err_purge_fail":     {Class: ClassCrit, Title: "Not purged.", Text: "The console could not request the purge. Nothing was changed."},
		"err_cust_devices":   {Class: ClassCrit, Title: "Customer not deleted.", Text: "It still has devices. Retire the active ones, purge the retired ones, then delete the customer."},
		"err_cust_fail":      {Class: ClassCrit, Title: "Customer not deleted.", Text: "The console could not delete the customer. Nothing was changed."},
		"err_cpurge_fail":    {Class: ClassCrit, Title: "Purge not started.", Text: "The console could not request the purges. Any device it had already marked shows as purging; nothing else was changed."},
		"cpurge_none":        {Class: ClassInfo, Title: "Nothing new to purge.", Text: "Every retired device of this customer was already purging."},
		"cpurge_queued":      {Class: ClassInfo, Title: "Purge started.", Text: ""},
		"cust_deleted_ok":    {Class: ClassOK, Title: "Customer deleted.", Text: "Its enrollment tokens and own policies were deleted with it. The audit log keeps the record."},
		"err_purge_notfound": {Class: ClassCrit, Title: "Not found.", Text: "It was removed or never existed. Reload the page."},
	} {
		if _, ok := cmdNotices[k]; !ok {
			cmdNotices[k] = v
		}
	}
}

// mfaStep is the fresh-MFA state of p's session now.
func (s *Service) mfaStep(ctx context.Context, p *auth.Principal) MFAStep {
	c := s.fc(ctx)
	st := MFAStep{Zone: c.Zone, Window: int(auth.FreshMFAWindow / time.Minute)}
	if !s.Auth.FreshMFA(p, s.Auth.Now()) || p.MFAVerifiedAt == nil {
		st.Need = true
		return st
	}
	until := p.MFAVerifiedAt.Add(auth.FreshMFAWindow)
	st.Since, st.Until, st.UntilISO = c.dayTime(*p.MFAVerifiedAt), c.dayTime(until), until.UTC().Format(time.RFC3339)
	return st
}

func (s *Service) purgeForm(ctx context.Context, p *auth.Principal, kind, action, name string) *PurgeForm {
	f := &PurgeForm{Kind: kind, Action: action, CSRF: p.CSRF, Name: name, MFA: s.mfaStep(ctx, p)}
	switch kind {
	case purgeKindDevice:
		f.ID, f.What, f.Button, f.BusyLabel = "purge", "device", "Purge permanently", "Purging…"
	case purgeKindRetired:
		f.ID, f.What, f.Button, f.BusyLabel = "cpur", "customer", "Purge retired devices", "Purging…"
	case purgeKindCustomer:
		f.ID, f.What, f.Button, f.BusyLabel = "cdel", "customer", "Delete customer", "Deleting…"
	}
	return f
}

// ---- the device page ----

// purgeActions puts Purge on the action bar of a retired device for an owner, and takes every
// action away from a purging device. It runs last in actionBar.
func (s *Service) purgeActions(r *http.Request, p *auth.Principal, dev store.Device, bar *ActionBar) {
	if dev.Purging() {
		bar.Items = []ActionItem{{ID: "act-purge", Key: "purge", Label: "Purging",
			Why: "No action is offered while the console removes this device's backups. It retries by itself; when storage reports the repository gone, the record is deleted."}}
		bar.More, bar.Identity = nil, nil
		return
	}
	if dev.RetiredAt == nil || p.Role < auth.RoleOwner {
		return // others do not see the control at all
	}
	f := s.purgeForm(r.Context(), p, purgeKindDevice, "/ui/devices/"+dev.ID+"/purge", dev.Hostname)
	f.Open = r.URL.Query().Get("purge") == "1"
	bar.Items = append(bar.Items, ActionItem{ID: "act-purge", Key: "purge", Label: "Purge permanently…", CSRF: p.CSRF, Enabled: true,
		Hint: "Deletes this device's backups and escrowed key from storage, then its record. One-way.", Purge: f})
}

// purgeView fills the device page for a purging device: the statement replaces the retired one,
// a PURGING chip joins the head, and Restore says why it is not offered.
func (s *Service) purgeView(ctx context.Context, p *auth.Principal, dev store.Device, c fmtCtx, v *DeviceView) {
	if !dev.Purging() {
		return
	}
	pv := s.purgingView(ctx, dev, c)
	v.Purge, v.Cause = &pv, nil
	v.Chips = append(v.Chips, Chip{ID: "purge-" + dev.ID, Label: "PURGING", Class: pv.Class, Title: "PURGING", Reason: pv.Lede})
	v.CanRestore, v.RestoreNote = false, "This device is being purged."
}

// purgingView builds the statement from the device row. By is resolved to an account name.
func (s *Service) purgingView(ctx context.Context, dev store.Device, c fmtCtx) PurgingView {
	pv := PurgingView{Class: ClassInfo, Title: "Purging", Reason: dev.PurgeReason,
		Retry: fmt.Sprintf("It retries by itself, about every %d minutes, until storage reports the repository gone; then the record is deleted and this page answers not found. There is no cancel.", int(jobs.DefaultDevicePurgeBackoff/time.Minute))}
	pv.Lede = "The console is removing this device's backups and escrowed key from storage."
	at := *dev.PurgeRequestedAt
	pv.Requested, pv.RequestedISO = c.dateClock(at), at.UTC().Format(time.RFC3339)
	pv.By = s.accountName(ctx, dev.PurgeRequestedBy)
	if dev.PurgeLastAttemptAt != nil {
		pv.Attempt, pv.AttemptISO = c.dateClock(*dev.PurgeLastAttemptAt), dev.PurgeLastAttemptAt.UTC().Format(time.RFC3339)
	}
	switch {
	case dev.PurgeLastErrorClass != "":
		pv.Class, pv.Title = ClassCrit, "Purge not finished"
		pv.ErrorClass = safeJobClass(dev.PurgeLastErrorClass)
		pv.ErrorText = purgeClassText(dev.PurgeLastErrorClass)
		pv.Lede = "The last attempt to remove this device's backups failed. They are still in storage, and the record stays until they are gone."
		pv.Retry = fmt.Sprintf("It retries by itself, about every %d minutes. Clear the condition named here and the next attempt picks it up; there is no way to drop the record while its backups remain.", int(jobs.DefaultDevicePurgeBackoff/time.Minute))
	case c.Now.Sub(at) > health.PurgeStuckAfter:
		pv.Class, pv.Title = ClassCrit, "Purge not finished"
		pv.Lede = fmt.Sprintf("No attempt has finished in more than %d hours: the purge job is not running, or not reaching this device.", int(health.PurgeStuckAfter/time.Hour))
	}
	return pv
}

// accountName is who an admin id is, as the pages name accounts; never empty.
func (s *Service) accountName(ctx context.Context, id string) string {
	if id == "" {
		return "an account since deleted"
	}
	a, err := s.Store.GetAdmin(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "an account since deleted"
	case err != nil:
		return "an account that could not be read"
	case a.DisplayName != "":
		return a.DisplayName + " (" + a.Username + ")"
	}
	return a.Username
}

// purgeClassText is a purge error class in words: the admin path's own sentence for its codes, a
// fixed sentence for the job's classes, and the class itself for anything else.
func purgeClassText(class string) string {
	switch class {
	case jobs.PurgeClassWorkerUnreachable:
		return adminpath.Message(adminpath.CodeUnavailable)
	case jobs.PurgeClassObjectsRemaining:
		return "The removal reported success, but storage still lists objects under this device's prefix."
	case jobs.PurgeClassBadResult:
		return "The maintenance worker answered counts that cannot be true, so nothing was taken as removed."
	case jobs.PurgeClassArtifactCleanup:
		return "A restore archive of this device staged on the console could not be deleted."
	case jobs.PurgeClassComplete:
		return "Storage reports the repository gone, but the record could not be deleted from the console database."
	case jobs.PurgeClassNotWired:
		return "This console has no connection to the maintenance worker configured."
	case jobs.PurgeClassNotRecorded:
		return "An attempt failed and could not be recorded."
	case "network":
		return adminpath.Message(adminpath.CodeStorageUnreachable)
	case jobs.ClassTimeout:
		return adminpath.Message(adminpath.CodeTimeout)
	}
	for _, code := range adminpath.Codes() {
		if code == class {
			return adminpath.Message(code)
		}
	}
	return "The attempt failed."
}

// devicePurgePost is POST /ui/devices/{id}/purge (owner).
func (s *Service) devicePurgePost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r, p, "This device")
		return
	case err != nil:
		s.log().Error("ui: device unreadable for a purge", logx.ID("device_id", id), logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Not purged", "The device could not be read, so nothing was changed. Try again shortly.")
		return
	}
	if dev.Purging() { // nothing left to confirm: answer the state
		back(w, r, id, "", "purge_already")
		return
	}
	in := readPurgeInput(r)
	again := func(status int, fill func(*PurgeForm)) {
		s.devicePurgeAgain(w, r, p, id, status, func(f *PurgeForm) { f.Typed, f.Reason = in.Typed, in.Reason; fill(f) })
	}
	if refusal, done := s.stepUp(w, r, p, in.Code); done {
		return
	} else if refusal != nil {
		again(refusal.status, func(f *PurgeForm) { f.Err, f.CodeErr, f.MFA.Need = &refusal.Note, refusal.Note.Title, true })
		return
	}
	status, resp, err := s.apiSend(r, http.MethodDelete, "/api/v1/devices/"+id, adminapi.DevicePurgeRequest{ConfirmHostName: in.Typed, Reason: in.Reason})
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch status {
	case http.StatusAccepted:
		var out adminapi.DevicePurgeResponse
		_ = json.Unmarshal(resp, &out)
		code := "purge_requested"
		if !out.Changed {
			code = "purge_already"
		}
		back(w, r, id, "", code)
	case http.StatusBadRequest:
		again(http.StatusBadRequest, func(f *PurgeForm) { fieldRefusal(f, resp, "confirm_host_name", "Not purged.") })
	case http.StatusForbidden:
		if !stepUpRequired(resp) {
			s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "Purging a device needs the owner role, on a signed-in session.")
			return
		}
		again(http.StatusForbidden, staleMFA)
	case http.StatusNotFound:
		s.notFound(w, r, p, "This device")
	case http.StatusConflict:
		back(w, r, id, "", "err_purge_active")
	default:
		s.log().Error("ui: purge request failed", logx.ID("device_id", id), logx.Class("render"), logx.Int("status", status))
		back(w, r, id, "", "err_purge_fail")
	}
}

// devicePurgeAgain re-renders the device page with the purge dialog open, filled by fill.
func (s *Service) devicePurgeAgain(w http.ResponseWriter, r *http.Request, p *auth.Principal, id string, status int, fill func(*PurgeForm)) {
	fleet, ok := s.fleetFor(w, r, p, "Device")
	if !ok {
		return
	}
	v, ok := s.deviceView(r.Context(), p, fleet, id)
	if !ok {
		s.notFound(w, r, p, "This device")
		return
	}
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id)
	if err != nil {
		s.notFound(w, r, p, "This device")
		return
	}
	pg := DevPanelPage{Page: s.page(r.Context(), p, fleet, "Device detail", "devices", "/ui/events?device="+id), Dev: v, Tab: "summary", Tabs: s.panelTabs(id, "summary", p, v.CanRestore)}
	if ds, ok := deviceState(fleet, id); ok {
		bar := s.actionBar(r, p, fleet, dev, ds)
		for i := range bar.Items {
			if f := bar.Items[i].Purge; f != nil {
				f.Open = true
				fill(f)
			}
		}
		pg.Actions = &bar
	}
	s.render(w, r, status, "device", "base", pg)
}

// ---- the customer page ----

// custDeleteView builds the "Delete customer" section, or nil below the owner role.
func (s *Service) custDeleteView(r *http.Request, p *auth.Principal, row store.CustomerRow) *CustDeleteView {
	if p.Role < auth.RoleOwner {
		return nil
	}
	v := &CustDeleteView{Name: row.Name, Active: row.Devices, Retired: row.Retired, Notice: custNotice(r)}
	if row.Retired > 0 {
		purging, err := s.Store.ListDevicesPurging(r.Context(), p.Scope)
		if err != nil {
			s.log().Error("ui: purging devices unreadable", logx.ID("customer_id", row.ID), logx.Err(err))
			v.PurgeErr = true
		}
		for _, d := range purging {
			if d.CustomerID == row.ID {
				v.Purging++
			}
		}
		v.ToPurge = max(row.Retired-v.Purging, 0)
	}
	switch {
	case row.Devices > 0:
		v.State = "has_active"
	case row.Retired > 0 && v.ToPurge > 0:
		v.State = "only_retired"
		f := s.purgeForm(r.Context(), p, purgeKindRetired, "/ui/customers/"+row.ID+"/purge-retired", row.Name)
		f.Customer, f.Count, f.CountUnsure = row.Name, v.ToPurge, v.PurgeErr
		f.Open = r.URL.Query().Get("purge") == "1"
		v.Form = f
	case row.Retired > 0:
		v.State = "all_purging"
	default:
		v.State = "empty"
		f := s.purgeForm(r.Context(), p, purgeKindCustomer, "/ui/customers/"+row.ID+"/delete", row.Name)
		f.Open = r.URL.Query().Get("delete") == "1"
		v.Form = f
	}
	return v
}

// custNotice is the answer of the last delete or purge action, from the query.
func custNotice(r *http.Request) *CauseView {
	q := r.URL.Query()
	code := q.Get("cdel")
	if !cmdErrCode.MatchString(code) {
		return nil
	}
	n, ok := cmdNotices[code]
	if !ok || (!strings.HasPrefix(code, "cpurge_") && !strings.HasPrefix(code, "err_c")) {
		return nil
	}
	if code == "cpurge_queued" {
		req, _ := strconv.Atoi(q.Get("n"))
		already, _ := strconv.Atoi(q.Get("already"))
		if req < 1 || req > 100000 || already < 0 || already > 100000 {
			return nil
		}
		n.Text = plural(req, "retired device is", "retired devices are") + " being purged, each on its own with its own audit rows. The customer can be deleted once they are gone."
		if already > 0 {
			n.Text += " " + plural(already, "other was", "others were") + " already purging."
		}
	}
	return &n
}

// customerDeletedNotice is the customer list's answer after a delete.
func customerDeletedNotice(r *http.Request) *CauseView {
	if r.URL.Query().Get("deleted") != "1" {
		return nil
	}
	n := cmdNotices["cust_deleted_ok"]
	return &n
}

func custBack(w http.ResponseWriter, r *http.Request, id string, v url.Values) {
	http.Redirect(w, r, listURL("/customers/"+id, v), http.StatusSeeOther)
}

// customerDeletePost is POST /ui/customers/{id}/delete (owner).
func (s *Service) customerDeletePost(w http.ResponseWriter, r *http.Request) {
	s.customerConfirmPost(w, r, purgeKindCustomer)
}

// customerPurgeRetiredPost is POST /ui/customers/{id}/purge-retired (owner).
func (s *Service) customerPurgeRetiredPost(w http.ResponseWriter, r *http.Request) {
	s.customerConfirmPost(w, r, purgeKindRetired)
}

func (s *Service) customerConfirmPost(w http.ResponseWriter, r *http.Request, kind string) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	if _, err := s.Store.GetCustomer(r.Context(), p.Scope, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r, p, "This customer")
			return
		}
		s.log().Error("ui: customer unreadable for a delete", logx.ID("customer_id", id), logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Nothing changed", "The customer could not be read, so nothing was changed. Try again shortly.")
		return
	}
	in := readPurgeInput(r)
	again := func(status int, fill func(*PurgeForm)) {
		s.customerConfirmAgain(w, r, p, kind, status, func(f *PurgeForm) { f.Typed, f.Reason = in.Typed, in.Reason; fill(f) })
	}
	if refusal, done := s.stepUp(w, r, p, in.Code); done {
		return
	} else if refusal != nil {
		again(refusal.status, func(f *PurgeForm) { f.Err, f.CodeErr, f.MFA.Need = &refusal.Note, refusal.Note.Title, true })
		return
	}
	body := adminapi.CustomerConfirmRequest{ConfirmName: in.Typed, Reason: in.Reason}
	method, target, failCode, title := http.MethodDelete, "/api/v1/customers/"+id, "err_cust_fail", "Customer not deleted."
	if kind == purgeKindRetired {
		method, target, failCode, title = http.MethodPost, "/api/v1/customers/"+id+"/purge-retired-devices", "err_cpurge_fail", "Purge not started."
	}
	status, resp, err := s.apiSend(r, method, target, body)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch status {
	case http.StatusNoContent:
		http.Redirect(w, r, "/customers?deleted=1", http.StatusSeeOther)
	case http.StatusAccepted:
		var out adminapi.CustomerPurgeRetiredResponse
		_ = json.Unmarshal(resp, &out)
		if out.Requested == 0 {
			custBack(w, r, id, url.Values{"cdel": {"cpurge_none"}})
			return
		}
		custBack(w, r, id, url.Values{"cdel": {"cpurge_queued"}, "n": {strconv.Itoa(out.Requested)}, "already": {strconv.Itoa(out.AlreadyPurging)}})
	case http.StatusBadRequest:
		again(http.StatusBadRequest, func(f *PurgeForm) { fieldRefusal(f, resp, "confirm_name", title) })
	case http.StatusForbidden:
		if !stepUpRequired(resp) {
			s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "This needs the owner role, on a signed-in session.")
			return
		}
		again(http.StatusForbidden, staleMFA)
	case http.StatusNotFound:
		s.notFound(w, r, p, "This customer")
	case http.StatusConflict:
		custBack(w, r, id, url.Values{"cdel": {"err_cust_devices"}})
	default:
		s.log().Error("ui: customer delete or purge failed", logx.ID("customer_id", id), logx.Class("render"), logx.Int("status", status))
		custBack(w, r, id, url.Values{"cdel": {failCode}})
	}
}

// customerConfirmAgain re-renders the customer page (Overview) with the dialog of kind open.
func (s *Service) customerConfirmAgain(w http.ResponseWriter, r *http.Request, p *auth.Principal, kind string, status int, fill func(*PurgeForm)) {
	fleet, ok := s.fleetFor(w, r, p, "Customer")
	if !ok {
		return
	}
	row, ok := s.customerRow(w, r, p)
	if !ok {
		return
	}
	c := s.fcAt(r.Context(), fleet.Computed)
	cs := rollupOf(fleet, row.ID)
	pg := CustomerPage{Page: s.page(r.Context(), p, fleet, row.Name, "customers", "/ui/events"), H: s.custHead(p, row, cs, c, "overview"), Tab: "overview"}
	pg.Overview = s.overview(r.Context(), p, fleet, row, cs, c)
	pg.Delete = s.custDeleteView(r, p, row)
	if pg.Delete != nil && pg.Delete.Form != nil && pg.Delete.Form.Kind == kind {
		pg.Delete.Form.Open = true
		fill(pg.Delete.Form)
	}
	s.render(w, r, status, "customer", "base", pg)
}

// ---- shared by the three posts ----

type purgeInput struct{ Typed, Reason, Code string }

func readPurgeInput(r *http.Request) purgeInput {
	return purgeInput{Typed: r.PostFormValue("confirm"), Reason: strings.TrimSpace(r.PostFormValue("reason")),
		Code: strings.ReplaceAll(strings.TrimSpace(r.PostFormValue("code")), " ", "")}
}

type stepUpRefusal struct {
	Note   Note
	status int
}

// stepUp renews the session's fresh-MFA mark through POST /api/v1/auth/step-up when the session
// is not fresh and a code was typed. It returns the refusal to show in the dialog, or done when it
// already answered with an error page. A fresh session, or no code, goes on: the destructive call
// is the guard and answers mfa_step_up_required when it must.
func (s *Service) stepUp(w http.ResponseWriter, r *http.Request, p *auth.Principal, code string) (*stepUpRefusal, bool) {
	if code == "" || s.Auth.FreshMFA(p, s.Auth.Now()) {
		return nil, false
	}
	status, _, err := s.apiSend(r, http.MethodPost, "/api/v1/auth/step-up", adminapi.StepUpRequest{Code: code})
	if err == nil {
		switch status {
		case http.StatusNoContent:
			now := s.Auth.Now()
			p.MFAVerifiedAt = &now // this request's copy, so a re-render does not ask again
			return nil, false
		case http.StatusUnauthorized, http.StatusBadRequest: // wrong, expired or replayed code, or a locked account
			return &stepUpRefusal{authNote(auth.ErrBadCode), http.StatusUnauthorized}, false
		case http.StatusTooManyRequests:
			return &stepUpRefusal{authNote(&auth.RateLimitedError{}), http.StatusTooManyRequests}, false
		case http.StatusConflict:
			return &stepUpRefusal{Note{"crit", "No authenticator is enrolled.", "Enroll one in My account first: a recovery code is not accepted here."}, http.StatusConflict}, false
		case http.StatusServiceUnavailable:
			return &stepUpRefusal{Note{"crit", "The master key is not loaded.", "Authenticator codes are checked under the console master key, so none can be checked until an operator loads it (banner C12). Nothing was changed."}, http.StatusServiceUnavailable}, false
		case http.StatusForbidden:
			s.errorPage(w, r, p, http.StatusForbidden, "Not permitted", "This needs the owner role, on a signed-in session.")
			return nil, true
		}
	}
	if s.writeFailed(w, r, p, status, err) {
		return nil, true
	}
	return &stepUpRefusal{Note{"crit", "The code was not checked.", "The console could not check it. Nothing was changed. Try again shortly."}, http.StatusBadRequest}, false
}

// stepUpRequired reports whether a 403 answer is mfa_step_up_required (not a plain forbidden).
func stepUpRequired(resp []byte) bool {
	var pr struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(resp, &pr)
	return pr.Code == auth.CodeStepUpRequired
}

// staleMFA marks a dialog whose session check went stale before the post arrived.
func staleMFA(f *PurgeForm) {
	f.MFA.Need = true
	f.Err = &Note{"warn", "A recent MFA check is required.",
		fmt.Sprintf("Your last authenticator check on this session is older than %d minutes. Enter a current authenticator code below; what you typed is kept.", int(auth.FreshMFAWindow/time.Minute))}
}

// fieldRefusal maps a 400's field errors onto the dialog. The answer names fields only; no name
// is echoed back from it.
func fieldRefusal(f *PurgeForm, resp []byte, confirmField, title string) {
	var pr struct {
		Errors []struct {
			Field string `json:"field"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(resp, &pr)
	for _, e := range pr.Errors {
		switch e.Field {
		case confirmField:
			f.ConfirmErr = "Type the " + f.What + "'s name exactly as shown, including its case."
		case "reason":
			f.ReasonErr = "A reason is required: one line of up to 500 characters, no file paths."
		}
	}
	text := "Fix the field marked below."
	if f.ConfirmErr == "" && f.ReasonErr == "" {
		text = "The request was not valid. Nothing was changed."
	}
	f.Err = &Note{"crit", title, text}
}

// ---- the board, the lists, the banner ----

// purgingSet is the purging devices inside p's scope, by id; ok is false when they could not be read.
func (s *Service) purgingSet(ctx context.Context, p *auth.Principal) (map[string]store.Device, bool) {
	l, err := s.Store.ListDevicesPurging(ctx, p.Scope)
	if err != nil {
		s.log().Error("ui: purging devices unreadable", logx.Err(err))
		return nil, false
	}
	out := make(map[string]store.Device, len(l))
	for _, d := range l {
		out[d.ID] = d
	}
	return out, true
}

// markPurging puts a PURGING chip on the rows of a "Retired (n)" control whose device is purging
// (crit when its last attempt failed), or says the purge state could not be read.
func markPurging(f *RetiredFold, set map[string]store.Device, ok bool) *RetiredFold {
	if f == nil {
		return nil
	}
	if !ok {
		f.PurgeErr = true
		return f
	}
	for i := range f.Rows {
		d, purging := set[f.Rows[i].ID]
		if !purging {
			continue
		}
		ch := Chip{Label: "PURGING", Class: ClassInfo, Small: true}
		f.Purging++
		if d.PurgeLastErrorClass != "" {
			ch.Label, ch.Class = "PURGE FAILING", ClassCrit
			f.Failing++
		}
		f.Rows[i].Purge = &ch
	}
	return f
}

// withPurging is markPurging with the set read now.
func (s *Service) withPurging(ctx context.Context, p *auth.Principal, f *RetiredFold) *RetiredFold {
	if f == nil {
		return nil
	}
	set, ok := s.purgingSet(ctx, p)
	return markPurging(f, set, ok)
}

// bannerSubjectRE is what a banner subject must look like to become a link: a device id.
var bannerSubjectRE = regexp.MustCompile(`^[0-9A-Za-z]{1,64}$`)

// purgeBannerAction links "Purge not finished" to the device it names, when it names one.
func purgeBannerAction(b state.Banner) *Link {
	if b.ID != health.BannerPurgeNotFinished || !bannerSubjectRE.MatchString(b.Subject) {
		return nil
	}
	return &Link{Href: "/devices/" + b.Subject, Label: "Open the device"}
}

func init() { bannerLabels[health.BannerPurgeNotFinished] = "PURGE NOT FINISHED" }

// ---- /design ----

// DesignPurge is the ADR 0019 section of /design: every state of the purge and delete dialogs.
type DesignPurge struct {
	Dialogs  []designPurgeForm
	Purging  []designPurging
	Notices  []designNotice
	Customer []designCustDelete
	Banners  []BannerView
	Fold     *RetiredFold
	FoldErr  *RetiredFold
	Bar      designBar
}

type designPurgeForm struct {
	Name string
	Form *PurgeForm
}

type designPurging struct {
	Name string
	View PurgingView
}

type designNotice struct {
	Name   string
	Notice CauseView
}

type designCustDelete struct {
	Name string
	View *CustDeleteView
}

type designBar struct {
	Purging ActionItem
	Idle    ActionItem
}

func (s *Service) designPurge(ctx context.Context, now time.Time) *DesignPurge {
	c := s.fc(ctx)
	c.Now = now
	host := "SPARE-PC-09"
	fresh := MFAStep{Zone: c.Zone, Window: 5, Since: c.dayTime(now.Add(-time.Minute)), Until: c.dayTime(now.Add(4 * time.Minute)), UntilISO: now.Add(4 * time.Minute).Format(time.RFC3339)}
	need := MFAStep{Need: true, Zone: c.Zone, Window: 5}
	n := 0
	dev := func(name string, mfa MFAStep, set func(*PurgeForm)) designPurgeForm {
		n++
		f := &PurgeForm{Kind: purgeKindDevice, ID: "dpurge" + strconv.Itoa(n), Action: "/design", Name: host, What: "device", Button: "Purge permanently", BusyLabel: "Purging…", MFA: mfa}
		if set != nil {
			set(f)
		}
		return designPurgeForm{name, f}
	}
	d := &DesignPurge{}
	d.Dialogs = []designPurgeForm{
		dev("Idle: MFA fresh, the button waits for the name", fresh, nil),
		dev("MFA needed: a code is asked for first", need, nil),
		dev("Submitting", fresh, func(f *PurgeForm) { f.Typed, f.Reason, f.Busy = host, "lab machine decommissioned", true }),
		dev("Name does not match", fresh, func(f *PurgeForm) {
			f.Typed, f.Reason = "spare-pc-09", "lab machine decommissioned"
			fieldRefusal(f, []byte(`{"errors":[{"field":"confirm_host_name"}]}`), "confirm_host_name", "Not purged.")
		}),
		dev("Reason missing", fresh, func(f *PurgeForm) {
			f.Typed = host
			fieldRefusal(f, []byte(`{"errors":[{"field":"reason"}]}`), "confirm_host_name", "Not purged.")
		}),
		dev("Wrong or expired code (also a locked account)", need, func(f *PurgeForm) {
			n := authNote(auth.ErrBadCode)
			f.Typed, f.Reason, f.Err, f.CodeErr = host, "lab machine decommissioned", &n, n.Title
		}),
		dev("Too many attempts", need, func(f *PurgeForm) {
			n := authNote(&auth.RateLimitedError{})
			f.Typed, f.Reason, f.Err, f.CodeErr = host, "lab machine decommissioned", &n, n.Title
		}),
		dev("MFA went stale while the dialog was open", fresh, func(f *PurgeForm) { f.Typed, f.Reason = host, "lab machine decommissioned"; staleMFA(f) }),
		{"Purge every retired device of a customer", &PurgeForm{Kind: purgeKindRetired, ID: "dcpur", Action: "/design", Name: "Northwind Dental", What: "customer", Customer: "Northwind Dental", Count: 3,
			Button: "Purge retired devices", BusyLabel: "Purging…", MFA: need}},
		{"Delete a customer with no devices", &PurgeForm{Kind: purgeKindCustomer, ID: "dcdel", Action: "/design", Name: "Example Empty", What: "customer",
			Button: "Delete customer", BusyLabel: "Deleting…", MFA: fresh}},
	}
	req := now.Add(-40 * time.Minute)
	tried := now.Add(-5 * time.Minute)
	base := store.Device{ID: "design", PurgeRequestedAt: &req, PurgeReason: "lab machine decommissioned"}
	going := base
	failing := base
	failing.PurgeLastAttemptAt, failing.PurgeLastErrorClass = &tried, adminpath.CodeStorageUnreachable
	old := now.Add(-4 * time.Hour)
	stuck := base
	stuck.PurgeRequestedAt = &old
	for _, x := range []struct {
		name string
		d    store.Device
	}{{"Purging (no attempt finished yet)", going}, {"Purge failing (last attempt failed)", failing}, {"Purge not finished (nothing finished for hours)", stuck}} {
		pv := s.purgingView(ctx, x.d, c)
		pv.By = "Owner Example (owner)"
		d.Purging = append(d.Purging, designPurging{x.name, pv})
	}
	for _, k := range []string{"purge_requested", "purge_already", "err_purge_active", "err_purge_fail", "cust_deleted_ok", "err_cust_devices", "cpurge_none"} {
		d.Notices = append(d.Notices, designNotice{k, cmdNotices[k]})
	}
	q := cmdNotices["cpurge_queued"]
	q.Text = "3 retired devices are being purged, each on its own with its own audit rows. The customer can be deleted once they are gone. 1 other was already purging."
	d.Notices = append(d.Notices, designNotice{"cpurge_queued", q})
	d.Customer = []designCustDelete{
		{"Active devices: not available, with the counts", &CustDeleteView{Name: "Summit Eye Care", Active: 12, Retired: 2, State: "has_active"}},
		{"Only retired devices: purge them all", &CustDeleteView{Name: "Northwind Dental", Retired: 4, Purging: 1, ToPurge: 3, State: "only_retired", Form: formCopy(d.Dialogs[8].Form, "dcust1")}},
		{"Every retired device purging", &CustDeleteView{Name: "Northwind Dental", Retired: 4, Purging: 4, State: "all_purging"}},
		{"Purge state unreadable: the count may include devices already purging", &CustDeleteView{Name: "Northwind Dental", Retired: 4, ToPurge: 4, PurgeErr: true, State: "only_retired",
			Form: &PurgeForm{Kind: purgeKindRetired, ID: "dcpur2", Action: "/design", Name: "Northwind Dental", What: "customer", Customer: "Northwind Dental", Count: 4, CountUnsure: true,
				Button: "Purge retired devices", BusyLabel: "Purging…", MFA: fresh}}},
		{"No devices: delete", &CustDeleteView{Name: "Example Empty", State: "empty", Form: formCopy(d.Dialogs[9].Form, "dcust2")}},
	}
	pb := health.PurgeBanners(health.PurgeFacts{Devices: []health.PurgeDevice{{ID: "01JDESIGNPURGE0000000000AA", RequestedAt: req, LastAttemptAt: &tried, LastErrorClass: adminpath.CodeStorageUnreachable}}}, now)
	for _, b := range pb {
		d.Banners = append(d.Banners, bannerView(b))
	}
	d.Fold = &RetiredFold{N: 3, Chip: retiredChip(), Rows: []RetiredRow{
		{ID: "dr1", Host: "OLD-RECEPTION", When: c.dateClock(now.AddDate(0, 0, -30))},
		{ID: "dr2", Host: "SPARE-PC-09", When: c.dateClock(now.AddDate(0, 0, -30))},
		{ID: "dr3", Host: "XRAY-2019", When: c.dateClock(now.AddDate(0, 0, -60))},
	}}
	markPurging(d.Fold, map[string]store.Device{"dr2": failing, "dr3": going}, true)
	d.FoldErr = markPurging(&RetiredFold{N: 1, Chip: retiredChip(), Rows: []RetiredRow{{ID: "dr4", Host: "LAB-PC-14", When: c.dateClock(now.AddDate(0, 0, -3))}}}, nil, false)
	d.Bar = designBar{
		Idle:    ActionItem{ID: "dact-purge", Key: "purge", Label: "Purge permanently…", Enabled: true, Hint: "Deletes this device's backups and escrowed key from storage, then its record. One-way.", Purge: formCopy(d.Dialogs[0].Form, "dbar")},
		Purging: ActionItem{ID: "dact-purging", Key: "purge", Label: "Purging", Why: "No action is offered while the console removes this device's backups. It retries by itself; when storage reports the repository gone, the record is deleted."},
	}
	return d
}

// formCopy is f under another element id, so one page can show the same dialog twice.
func formCopy(f *PurgeForm, id string) *PurgeForm {
	c := *f
	c.ID = id
	return &c
}
