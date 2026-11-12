package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T6: device retire (B07, architecture §5.3 POST /ui/devices/{id}/retire, ADR
// 0016). "Retire…" opens a dialog: a reason, an optional replacement, an optional
// final backup, and the device name typed to confirm. It posts to POST
// /ui/devices/{id}/retire, which goes through POST /api/v1/devices/{id}/retire: the
// one handler that revokes, audits and signals the gateway sync.
//
// Retired devices collapse under "Retired (n)" on the lists that show devices
// (§4.2 row 1); RetiredFold is that control. They raise no alarm and count in no
// denominator: the engine decides that (§4.2 row 1, §4.3), and nothing here does.

const patRetire = "POST /ui/devices/{id}/retire"

func m3t6Patterns() []string { return []string{patRetire} }

// M3T6Roles is the minimum role of each pattern, for the role-gating test.
func M3T6Roles() map[string]string { return map[string]string{patRetire: "AD"} }

func (s *Service) registerM3T6(r httpx.Registrar) {
	r.Handle(patRetire, s.Auth.Require(auth.RoleAdmin, auth.ModeUI, http.HandlerFunc(s.deviceRetirePost)))
}

// RetireForm is the dialog of the Retire action.
type RetireForm struct {
	Action string
	CSRF   string
	Host   string
	// Open starts the dialog open: the operator came from a list's Retire link.
	Open bool
	// Return is where the answer goes back to ("" = this device's page).
	Return string
	// Replacements are the customer's other active devices.
	Replacements []Option
	// ReplacementsErr: the list could not be read, so the choice is not offered.
	ReplacementsErr bool
	// Parked: the final backup is not available until the device is resumed.
	Parked bool
}

// retireItem is the Retire entry of the action bar for a device.
func (s *Service) retireItem(r *http.Request, p *auth.Principal, dev store.Device, retired, parked bool) ActionItem {
	it := ActionItem{ID: "act-retire", Key: "retire", Label: "Retire…", CSRF: p.CSRF, Enabled: true,
		Hint: "Revokes this device's token, removes its gateway login and ends its backups for good. Its data stays."}
	switch {
	case retired:
		it.Enabled, it.Why, it.Hint = false, "Already retired.", ""
	case p.Role < auth.RoleAdmin:
		it.Enabled, it.Why, it.Hint = false, "Needs the admin role.", ""
	default:
		q := r.URL.Query()
		f := &RetireForm{Action: "/ui/devices/" + dev.ID + "/retire", CSRF: p.CSRF, Host: dev.Hostname, Open: q.Get("retire") == "1", Parked: parked}
		if q.Get("from") == "unseen" {
			f.Return = "unseen"
		}
		f.Replacements, f.ReplacementsErr = s.replacementOptions(r, p, dev)
		it.Retire = f
	}
	return it
}

// replacementOptions lists the customer's other active devices for the dialog.
func (s *Service) replacementOptions(r *http.Request, p *auth.Principal, dev store.Device) ([]Option, bool) {
	rows, err := s.Store.ListDeviceRows(r.Context(), p.Scope, store.DeviceFilter{CustomerID: dev.CustomerID})
	if err != nil {
		s.log().Error("ui: replacement devices unreadable", logx.Err(err))
		return nil, true
	}
	opts := []Option{{Value: "", Label: "None", Selected: true}}
	for _, d := range rows {
		if d.ID != dev.ID && d.RetiredAt == nil {
			opts = append(opts, Option{Value: d.ID, Label: d.Hostname})
		}
	}
	return opts, false
}

// the notices of the retire answer, and the audit text of what M3-T6 writes
func init() {
	auditActionText["device.retire"] = "Device retired"
	auditActionLabels["device.retire"] = "Device retired"
	auditActionLabels["enrollment_token.delete"] = "Enrollment token deleted"
	auditActionText["device.retire_sync"] = "Gateway login after the retire"
	auditActionLabels["device.retire_sync"] = "Gateway login after a retire"
	for k, v := range map[string]CauseView{
		"retired_ok":          {Class: ClassOK, Title: "Device retired.", Text: "Its device token is revoked and its gateway login is gone from the gateway's file. Nothing was deleted: its repository stays in storage."},
		"retired_pending":     {Class: ClassCrit, Title: "Device retired, but its gateway login is not removed.", Text: "The device token is revoked, but the gateway may still accept this device's login until the sync succeeds. A banner stays up while a read of the gateway's file finds the login or cannot read the file."},
		"final_backup_queued": {Class: ClassInfo, Title: "Final backup queued. The device is not retired yet.", Text: "A backup needs the credentials a retire takes away. When this one has finished, retire the device."},
		"err_retire_confirm":  {Class: ClassCrit, Title: "Not retired.", Text: "Type the device name exactly to confirm."},
		"err_retire_reason":   {Class: ClassCrit, Title: "Not retired.", Text: "A reason is required: one line, no file paths."},
		"err_retire_repl":     {Class: ClassCrit, Title: "Not retired.", Text: "The replacement must be an active device of the same customer."},
		"err_retire_fail":     {Class: ClassCrit, Title: "Not retired.", Text: "The console could not retire the device. Nothing was changed."},
	} {
		if _, ok := cmdNotices[k]; !ok {
			cmdNotices[k] = v
		}
	}
}

func (s *Service) deviceRetirePost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	dev, err := s.Store.GetDevice(r.Context(), p.Scope, id)
	switch {
	case errors.Is(err, store.ErrNotFound): // 404 for another customer's device, before anything else
		s.notFound(w, r, p, "This device")
		return
	case err != nil:
		s.log().Error("ui: device unreadable for a retire", logx.Err(err))
		s.errorPage(w, r, p, http.StatusServiceUnavailable, "Not retired", "The device could not be read, so nothing was changed. Try again shortly.")
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if dev.RetiredAt == nil { // a retired device answers its state: there is nothing left to confirm
		if strings.TrimSpace(r.PostFormValue("confirm")) != dev.Hostname {
			back(w, r, id, "", "err_retire_confirm", retireBackExtra(r)...)
			return
		}
		if reason == "" {
			back(w, r, id, "", "err_retire_reason", retireBackExtra(r)...)
			return
		}
	}
	body := adminapi.DeviceRetireRequest{Reason: reason, ReplacedBy: r.PostFormValue("replaced_by"), FinalBackup: r.PostFormValue("final_backup") == "1"}
	status, resp, err := s.apiSend(r, http.MethodPost, "/api/v1/devices/"+id+"/retire", body)
	if s.writeFailed(w, r, p, status, err) {
		return
	}
	switch status {
	case http.StatusOK:
		s.retireAnswered(w, r, id, resp)
	case http.StatusAccepted:
		back(w, r, id, "", "final_backup_queued")
	case http.StatusConflict: // only the final backup can conflict: the device is parked
		back(w, r, id, "", "err_parked")
	case http.StatusBadRequest:
		code := "err_retire_reason"
		var pr struct {
			Errors []struct {
				Field string `json:"field"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(resp, &pr)
		for _, f := range pr.Errors {
			if f.Field == "replaced_by" {
				code = "err_retire_repl"
			}
		}
		back(w, r, id, "", code, retireBackExtra(r)...)
	default:
		s.log().Error("ui: retire failed", logx.Class("render"), logx.Int("status", status))
		back(w, r, id, "", "err_retire_fail")
	}
}

// retireAnswered sends the operator back with what the answer says about the
// gateway. Only an answer that says removed reads as removed: anything else,
// including one this cannot read, is pending.
func (s *Service) retireAnswered(w http.ResponseWriter, r *http.Request, id string, resp []byte) {
	var out adminapi.DeviceRetireResponse
	_ = json.Unmarshal(resp, &out)
	code, extra := "retired_ok", []string(nil)
	if out.GatewayLogin != adminapi.GatewayLoginRemoved {
		code, extra = "retired_pending", []string{"why", out.GatewayLoginReason}
	}
	if r.PostFormValue("return") == "unseen" {
		v := url.Values{"cmd": {code}}
		if extra != nil {
			v.Set(extra[0], extra[1])
		}
		http.Redirect(w, r, listURL("/fleet/unseen", v), http.StatusSeeOther)
		return
	}
	back(w, r, id, "", code, extra...)
}

// retireBackExtra reopens the dialog after a refused retire, keeping where it came from.
func retireBackExtra(r *http.Request) []string {
	extra := []string{"retire", "1"}
	if r.PostFormValue("return") == "unseen" {
		extra = append(extra, "from", "unseen")
	}
	return extra
}

// retiredCause is the device page's plain statement about a retired device.
func (s *Service) retiredCause(ctx context.Context, p *auth.Principal, dev store.Device, c fmtCtx) *CauseView {
	if dev.RetiredAt == nil {
		return nil
	}
	var text []string
	if dev.RetiredReason != "" {
		text = append(text, "Reason: "+dev.RetiredReason+".")
	}
	if dev.ReplacedByDeviceID != "" {
		if repl, err := s.Store.GetDevice(ctx, p.Scope, dev.ReplacedByDeviceID); err == nil {
			text = append(text, "Replaced by "+repl.Hostname+".")
		}
	}
	title := "Retired " + c.dateClock(*dev.RetiredAt)
	if dev.RetiredAt.UnixMilli() < store.MinPlausibleTime {
		title = "Retired, at a time that cannot be read"
	}
	class := ClassNeutral
	rctx, cancel := context.WithTimeout(ctx, retiredPageCheck)
	defer cancel()
	switch v := s.m3.api.GatewayLoginNow(rctx, dev.ID, *dev.RetiredAt); {
	case v.Status == adminapi.GatewayLoginRemoved:
		text = append(text, "Its device token is revoked and a read of the gateway's file finds no login for it, so no backups can reach the gateway.")
	case v.InFile:
		class, title = ClassCrit, title+", gateway login not removed"
		text = append(text, "Its device token is revoked, but the gateway may still accept this device's login until the sync succeeds (reason: "+v.Reason+").")
	default:
		class = ClassWarn
		text = append(text, "Its device token is revoked. The gateway's file could not be read (reason: "+v.Reason+"), so the gateway may still accept this device's login until a read finds it gone.")
	}
	text = append(text, retiredAdvice)
	return &CauseView{Class: class, Title: title, Text: strings.Join(text, " ")}
}

// retiredPageCheck bounds the device page's read of the gateway's file.
const retiredPageCheck = time.Second

// retiredAdvice is what the operator still has to do and cannot get from the console.
const retiredAdvice = "Its repository is kept; restoring from a retired device is not available in the console yet. " +
	"Uninstall the agent from the machine. An agent 0.3.0 or later goes quiet by itself (about one request an hour); an older one keeps trying every few minutes. To bring the machine back, uninstall, move the state folder aside and install with a new token. While an enrollment token is active, a recurring RMM script enrolls the same machine again as a new device, so stop the script or revoke the token."

// ---- the "Retired (n)" control ----

// RetiredRow is one retired device under a "Retired (n)" control.
type RetiredRow struct {
	ID         string
	Host       string
	CustomerID string
	Customer   string // "" when the list is one customer's
	When       string
	ISO        string
}

// RetiredFold is "Retired (n)", collapsed: the retired devices a list leaves out of its rows.
type RetiredFold struct {
	N    int
	Rows []RetiredRow
	Chip Chip
}

func retiredChip() Chip {
	return Chip{Label: catalogue[state.StateRetired].Label, Class: catalogue[state.StateRetired].Class, Small: true}
}

// foldOf builds the control from the engine's view of customers, or nil when no
// device is retired. Keep decides which of them this list speaks for. With
// showCustomer each row names its customer.
func foldOf(cs []*state.CustomerState, c fmtCtx, showCustomer bool, keep func(state.DeviceState) bool) *RetiredFold {
	var rows []RetiredRow
	for _, cu := range cs {
		for _, d := range cu.Devices {
			if d.Primary != state.StateRetired || (keep != nil && !keep(d)) {
				continue
			}
			row := RetiredRow{ID: d.ID, Host: d.Hostname, CustomerID: d.CustomerID}
			if showCustomer {
				row.Customer = cu.Name
			}
			if !d.Since.IsZero() {
				row.When, row.ISO = c.dateClock(d.Since), d.Since.UTC().Format(time.RFC3339)
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Host < rows[j].Host })
	return &RetiredFold{N: len(rows), Rows: rows, Chip: retiredChip()}
}

// retiredFold is the control of a list over the whole fleet view, or one customer.
func retiredFold(f health.Fleet, c fmtCtx, customerID string) *RetiredFold {
	return foldOf(customerPtrs(f), c, customerID == "", func(d state.DeviceState) bool { return customerID == "" || d.CustomerID == customerID })
}

func customerPtrs(f health.Fleet) []*state.CustomerState {
	out := make([]*state.CustomerState, 0, len(f.View.Customers))
	for i := range f.View.Customers {
		out = append(out, &f.View.Customers[i])
	}
	return out
}

// customerFold is the control of one customer group on the board.
func customerFold(cs state.CustomerState, c fmtCtx) *RetiredFold {
	return foldOf([]*state.CustomerState{&cs}, c, false, nil)
}
