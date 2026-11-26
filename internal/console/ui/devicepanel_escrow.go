package ui

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The repository-key panel of the device Summary tab (ADR 0030, plan B.3): who holds the key,
// when it was last proven to open the repository, what failed, when the next check is due and,
// for an administrator, "Verify now". Retired devices get the same panel: their key is rechecked
// weekly like every other.
//
// What the panel never claims: a verified key opens the repository's config. It is not a check of
// the backup data (that is `restic check`, which the pilot runs without; ADR 0026), and the panel
// says so in the same place as the verdict.
//
// "Verify now" posts to POST /ui/devices/{id}/escrow/verify (admin; the same CSRF and origin gate
// as every page action). The handler asks the admin API's own handler, in process, to run the
// check (POST /api/v1/devices/{id}/escrow/verify): the API decides, runs the worker, records the
// proof and writes the one audit row (device.escrow.verify, path-free). This page writes nothing
// and audits nothing of its own, so no action is audited twice.
//
// Answers. A verdict (verified, failed, not verified) is a 303 back to the page with a signed
// flash, so a reload does not run the check again and a URL cannot fake "verified". A refusal is
// the device page itself with the refusal's real status (403, 409, 503, 500) and its sentence. An
// htmx post gets the panel alone with 200 whatever the outcome, because htmx 2 swaps no 4xx
// response (the P22-G wizard's rule): every outcome is visible with JavaScript on and off.

// escrowProvesText is what a pass proves, in the panel and beside every verdict. It is the sentence
// of the API's answer (adminapi.EscrowVerifyResponse.Proves); a test pins the two together.
const escrowProvesText = "A verified key opens the repository's config. This does not prove that the backup data is intact."

// Kinds of key the panel reports (EscrowPanel.State, the data-escrow attribute).
const (
	escVerified    = "verified"      // proven within the weekly cadence and a day
	escOverdue     = "overdue"       // proven, but longer ago than the cadence and a day: the verifier is not keeping up
	escFailed      = "failed"        // the last check reached a verdict on the key or the repository
	escNever       = "never"         // escrowed, never proven
	escHeld        = "customer_held" // CryoBackup never holds the key: nothing to verify
	escPurging     = "purging"       // the device is being purged: its repository is going away
	escUnconfirmed = "unconfirmed"   // the enrollment is not confirmed: no repository yet
	escNone        = "none"          // no active key record at all
	escUnreadable  = "unreadable"    // the key record could not be read
)

// EscrowPanel is the repository-key panel of the device Summary tab.
type EscrowPanel struct {
	DeviceID string
	CSRF     string
	// Action is where Verify now posts.
	Action  string
	State   string
	Retired bool
	Chip    Chip
	// Lead is the sentence of a key that needs attention (failed, overdue, never proven, unreadable).
	Lead *CauseView
	// Result is the answer of a Verify now that was just run, or a refusal.
	Result *CauseView
	Rows   []Fact
	// Note is a plain line under the facts (what retirement means for the key).
	Note   string
	Proves string
	// CanVerify shows the Verify now button; Why says, when it is not shown, why not.
	CanVerify bool
	Why       string
}

// ShowLead says whether the attention sentence is shown: not beside a "verification failed" answer,
// which says the same thing about the same check.
func (p *EscrowPanel) ShowLead() bool {
	return p.Lead != nil && (p.Result == nil || p.Result.Title != "Verification failed.")
}

// escrowAssess says what the panel reports about an escrowed key at now, with the chip class: the
// engine's own rule (state.EscrowKeyFailing) decides how loud a key that is not green is.
func escrowAssess(esc store.Escrow, now time.Time) (kind, class string) {
	facts := state.EscrowKeyFacts{Escrow: state.EscrowFacts{Custody: "escrowed", CreatedAt: state.Millis(esc.CreatedAt.UnixMilli()), LastVerifyErrorClass: esc.LastVerifyErrorClass}}
	if esc.VerifiedAt != nil {
		facts.Escrow.VerifiedAt = state.Millis(esc.VerifiedAt.UnixMilli())
	}
	_, bad := state.EscrowKeyFailing(facts, now)
	loud := ClassWarn
	if bad {
		loud = ClassCrit
	}
	switch {
	case esc.LastVerifyErrorClass != "":
		return escFailed, ClassCrit
	case esc.VerifiedAt == nil:
		return escNever, loud
	case now.Sub(*esc.VerifiedAt) > state.EscrowVerifyMaxAge:
		return escOverdue, loud
	}
	return escVerified, ClassOK
}

// escrowFailureText is the sentence of a recorded failure class. Only the two classes the verifier
// records are worded; anything else is said not to be recognised, never echoed.
func escrowFailureText(class string) string {
	switch class {
	case adminpath.CodeWrongPassword:
		return "The escrowed key does not open this device's repository (wrong password). A restore from it would fail."
	case adminpath.CodeRepoMissing:
		return "No repository was found at this device's storage location. A restore would find nothing."
	}
	return "The last check failed with a class this console version does not recognise."
}

// escrowNoVerdictText is why a check reached no verdict: the fixed sentence of the class.
func escrowNoVerdictText(class string) string {
	switch class {
	case adminpath.CodeStorageUnreachable:
		return "Storage did not answer."
	case adminpath.CodeTimeout:
		return "The repository did not answer in time."
	case adminpath.CodeBusy:
		return "The maintenance worker was busy."
	case adminpath.CodeLocked:
		return "The repository was locked."
	case adminpath.CodeUnavailable:
		return "The maintenance worker was not reachable."
	}
	return "The check could not finish."
}

// escrowPanel builds the panel of device id. A key record that cannot be read is an alarm on the
// panel, never an empty or a green one.
func (s *Service) escrowPanel(ctx context.Context, p *auth.Principal, id string, c fmtCtx) *EscrowPanel {
	pn := &EscrowPanel{DeviceID: id, CSRF: p.CSRF, Action: "/ui/devices/" + id + "/escrow/verify", Proves: escrowProvesText}
	chip := func(label, qual, class string) Chip {
		return Chip{ID: "escrow-" + id, Label: label, Qual: qual, Joiner: " · ", Class: class}
	}
	t, err := s.Store.GetEscrowVerifyTarget(ctx, p.Scope, id)
	if err != nil {
		s.log().Error("ui: escrow target unreadable", logx.Class("store"), logx.Err(err))
		pn.State, pn.Chip = escUnreadable, chip("NOT READ", "", ClassCrit)
		pn.Lead = &CauseView{Class: ClassCrit, Title: "The key record could not be read.", Text: "This is an alarm, not an empty panel. Reload the page; if it persists, check the console log and the database."}
		pn.Why = "Nothing can be verified until the key record can be read."
		return pn
	}
	pn.Retired = t.Retired
	if t.Retired {
		pn.Note = "This device is retired. Its key is still checked every week, so a download from its backups stays possible (ADR 0030)."
	}
	switch t.Custody {
	case "customer_held":
		pn.State, pn.Chip = escHeld, chip("CUSTOMER-HELD", "", ClassNeutral)
		pn.Rows = []Fact{{Key: "Custody", Value: "Customer-held. CryoBackup never holds this key."}}
		pn.Why = "Nothing to verify: the console cannot open this repository, so it cannot check the key, and there is no technician restore."
		return pn
	case "escrowed":
	default:
		pn.State, pn.Chip = escNone, chip("NO KEY", "", ClassCrit)
		pn.Lead = &CauseView{Class: ClassCrit, Title: "This device has no active key record.", Text: "Nothing can be restored from it and nothing can be verified. The device must enroll again."}
		pn.Why = "There is no key to verify."
		return pn
	}
	pn.Rows = []Fact{{Key: "Custody", Value: "Escrowed: the console holds the key, wrapped by its master key."}}
	esc, err := s.Store.ActiveEscrow(ctx, p.Scope, id)
	if err != nil {
		s.log().Error("ui: escrow row unreadable", logx.Class("store"), logx.Err(err))
		pn.State, pn.Chip = escUnreadable, chip("NOT READ", "", ClassCrit)
		pn.Lead = &CauseView{Class: ClassCrit, Title: "The key record could not be read.", Text: "This is an alarm, not an empty panel. Reload the page; if it persists, check the console log and the database."}
		pn.Why = "Nothing can be verified until the key record can be read."
		return pn
	}
	kind, class := escrowAssess(esc, c.Now)
	pn.State = kind
	// The sentences below offer Verify now only when the button is there (the gate at the end of this function).
	askNow := ""
	if !t.Purging && t.Confirmed && p.Role >= auth.RoleAdmin && s.MasterLoaded != nil && s.MasterLoaded() && s.Backend != nil {
		askNow = " Or press Verify now."
	}
	stamp := func(at time.Time) string {
		return c.dayClock(at) + " " + c.zoneAbbr(at) + " · " + ageLong(c.Now.Sub(at))
	}
	switch kind {
	case escVerified:
		pn.Chip = chip("VERIFIED", ageShort(c.Now.Sub(*esc.VerifiedAt)), class)
	case escOverdue:
		pn.Chip = chip("OVERDUE", ageShort(c.Now.Sub(*esc.VerifiedAt)), class)
		pn.Lead = &CauseView{Class: class, Title: "The weekly check has not run.",
			Text: "The newest proof is " + strconv.Itoa(int(c.Now.Sub(*esc.VerifiedAt)/(24*time.Hour))) + " days old. Check that the maintenance worker is running (banner C07)." + askNow}
	case escNever:
		pn.Chip = chip("NEVER VERIFIED", "", class)
		pn.Lead = &CauseView{Class: class, Title: "This key has never been verified.",
			Text: "Until it is, nobody knows that a restore from it would work. The next maintenance pass checks it." + askNow}
	case escFailed:
		pn.Chip = chip("FAILED", "", class)
		lead := escrowFailureText(esc.LastVerifyErrorClass)
		if t.Retired {
			lead += " This device is retired, so it raises no alarm of its own: it counts in the fleet banner for retired devices (C22)."
		} else {
			lead += " It raises ESCROW UNVERIFIED on this device."
		}
		pn.Lead = &CauseView{Class: class, Title: "The last check failed.", Text: lead}
	}
	if esc.VerifiedAt != nil {
		pn.Rows = append(pn.Rows, Fact{Key: "Last verified", Value: stamp(*esc.VerifiedAt)})
	} else {
		pn.Rows = append(pn.Rows, Fact{Key: "Last verified", Value: "Never"})
	}
	if esc.LastVerifyErrorClass != "" {
		cls := esc.LastVerifyErrorClass
		if !classWordRE.MatchString(cls) {
			cls = "not recognised"
		}
		pn.Rows = append(pn.Rows, Fact{Key: "Last failure", Value: cls, Mono: true})
	}
	if at := esc.LastVerifyAttemptAt; at != nil && (esc.VerifiedAt == nil || at.Sub(*esc.VerifiedAt) > time.Minute) {
		v := stamp(*at)
		if esc.LastVerifyErrorClass == "" {
			v += " · reached no verdict, so the earlier proof stands and keeps ageing"
		}
		pn.Rows = append(pn.Rows, Fact{Key: "Last attempt", Value: v})
	}
	pn.Rows = append(pn.Rows, Fact{Key: "Next check", Value: escrowNextCheck(esc.VerifiedAt, esc.LastVerifyAttemptAt, c)})

	switch {
	case t.Purging:
		pn.State, pn.Chip = escPurging, chip("PURGING", "", ClassNeutral)
		pn.Why = "This device is being purged: its repository is going away, so its key is no longer checked."
	case !t.Confirmed:
		pn.State, pn.Chip = escUnconfirmed, chip("NOT ENROLLED YET", "", ClassNeutral)
		pn.Why = "The device has not confirmed its enrollment, so there is no repository to open yet."
	case p.Role < auth.RoleAdmin:
		pn.Why = "Verify now needs the admin role."
	case s.MasterLoaded == nil || !s.MasterLoaded():
		pn.Why = "The master key is unavailable (banner C12): the console cannot unwrap the key to check it."
	case s.Backend == nil:
		pn.Why = "The console has no maintenance worker to run the check with (banner C07)."
	default:
		pn.CanVerify = true
	}
	if t.Purging || !t.Confirmed { // no repository to open, or one going away: the reason below says it, a lead would only mislead
		pn.Lead = nil
	}
	return pn
}

// escrowRetryHold is how long the verifier leaves a key alone after any attempt, whatever its
// outcome. It is maintain.escrowVerifyRetry (unexported there, and maintain is not this ticket's to
// change): keep the two equal.
const escrowRetryHold = time.Hour

// escrowNextCheck words when the verifier checks the key next: weekly, up to a day sooner (its
// stagger), at the next pass once it is due, and never within escrowRetryHold of an attempt. A key
// never proven is retried every hour.
func escrowNextCheck(verified, attempt *time.Time, c fmtCtx) string {
	hold := time.Time{}
	if attempt != nil && c.Now.Before(attempt.Add(escrowRetryHold)) {
		hold = attempt.Add(escrowRetryHold)
	}
	heldUntil := func(what string) string {
		return what + ", not before " + c.dayClock(hold) + " " + c.zoneAbbr(hold) + " (an hour after the last attempt)"
	}
	if verified == nil {
		if !hold.IsZero() {
			return heldUntil("At the next maintenance pass") + ", then every hour until it passes"
		}
		return "At the next maintenance pass, then every hour until it passes"
	}
	due := verified.Add(state.EscrowVerifyEvery)
	if !c.Now.Before(due.Add(-24 * time.Hour)) {
		if !hold.IsZero() {
			return heldUntil("Due: the next maintenance pass runs it")
		}
		return "Due now: the next maintenance pass runs it"
	}
	return "By " + c.dayClock(due) + " " + c.zoneAbbr(due) + " (weekly, up to a day sooner)"
}

// ---- the answer of Verify now ----

// escrowResult is what a Verify now came to: the sentence to show and the status a browser post is
// answered with. Verdict is set when the check ran (verified, failed, not_verified).
type escrowResult struct {
	Status  int
	Verdict string
	Class   string
	View    CauseView
}

// escrowAnswer words the in-process admin API's answer. Every refusal has its own sentence; none is
// dressed as a verdict, and a verdict is never worded from anything but the API's fixed fields.
func escrowAnswer(status int, body []byte, err error) escrowResult {
	crit := func(code int, title, text string) escrowResult {
		return escrowResult{Status: code, View: CauseView{Class: ClassCrit, Title: title, Text: text}}
	}
	if err != nil || status == 0 {
		return crit(http.StatusBadGateway, "Not verified.", "The console could not ask its own admin API, so nothing was checked and nothing was recorded. Try again shortly.")
	}
	switch status {
	case http.StatusOK:
		var out adminapi.EscrowVerifyResponse
		if json.Unmarshal(body, &out) != nil {
			return crit(http.StatusBadGateway, "Not verified.", "The answer could not be read, so the check is not counted. Reload the page to see what was recorded.")
		}
		return escrowVerdict(out.Verdict, out.ErrorClass)
	case http.StatusForbidden:
		return crit(http.StatusForbidden, "Not allowed.", "Only administrators can verify a key.")
	case http.StatusConflict:
		switch problemCode(body) {
		case api.CodeCustodyCustomerHeld:
			return crit(http.StatusConflict, "Customer-held key: nothing to verify.", "CryoBackup never holds this key, so the console cannot open this repository. Nothing was checked.")
		case api.CodeDevicePurging:
			return crit(http.StatusConflict, "This device is being purged.", "Its repository is going away, so its key is not checked. Nothing was checked.")
		}
		return crit(http.StatusConflict, "Nothing to verify yet.", "The device has no active escrowed key, or its enrollment is not confirmed, so there is no repository to open. Nothing was checked.")
	case http.StatusServiceUnavailable:
		if problemCode(body) == api.CodeMasterKeyUnavailable {
			return crit(http.StatusServiceUnavailable, "Master key unavailable.", "The console cannot unwrap the escrowed key without its master key (banner C12). Nothing was checked and nothing was recorded.")
		}
		return crit(http.StatusServiceUnavailable, "The maintenance worker did not answer.", "The check needs the worker (banner C07). Nothing was checked and nothing was recorded as verified. Try again shortly.")
	case http.StatusInternalServerError:
		return crit(http.StatusInternalServerError, "Not recorded (NOT AUDITED).", "The check could not be recorded, or its audit entry could not be written, so it does not count. Nothing was changed. Check the console database and try again.")
	}
	return crit(http.StatusBadGateway, "Not verified.", "The console's admin API answered with status "+strconv.Itoa(status)+". Nothing was recorded. Try again shortly.")
}

// escrowVerdict words a verdict the API reached. An unknown verdict is an alarm, not a pass.
func escrowVerdict(verdict, class string) escrowResult {
	switch verdict {
	case adminapi.EscrowVerified:
		return escrowResult{Status: http.StatusOK, Verdict: verdict,
			View: CauseView{Class: ClassOK, Title: "Verified.", Text: "The key opened this device's repository config just now. It was recorded and audited. This does not prove that the backup data is intact."}}
	case adminapi.EscrowFailed:
		return escrowResult{Status: http.StatusOK, Verdict: verdict, Class: class,
			View: CauseView{Class: ClassCrit, Title: "Verification failed.", Text: escrowFailureText(class) + " It was recorded and audited."}}
	case adminapi.EscrowNotVerified:
		return escrowResult{Status: http.StatusOK, Verdict: verdict, Class: class,
			View: CauseView{Class: ClassWarn, Title: "No verdict.", Text: escrowNoVerdictText(class) + " Nothing was proven: the earlier proof stands and keeps ageing. Try again shortly."}}
	}
	return escrowResult{Status: http.StatusBadGateway,
		View: CauseView{Class: ClassCrit, Title: "Not verified.", Text: "The admin API answered with a verdict this page does not know, so the check is not counted."}}
}

// The verdict travels in the redirect as a flash token (the "Run now" pattern, m3t4_system.go): the
// verdict word, its class, an expiry and an HMAC over them and the caller's session secret, keyed
// with the process's random key. A URL typed or forged by hand verifies as nothing.
var classWordRE = regexp.MustCompile(`^[a-z_]{1,32}$`)

var escrowFlashRE = regexp.MustCompile(`^(verified|failed|not_verified)(\.[a-z_]{1,32})?$`)

func (s *Service) escrowFlashToken(p *auth.Principal, deviceID string, res escrowResult) string {
	name := "escrow:" + deviceID + ":" + res.Verdict
	if res.Class != "" {
		name += "." + res.Class
	}
	exp := s.now().Add(flashTTL).Unix()
	return url.QueryEscape(name + "~" + strconv.FormatInt(exp, 10) + "~" + s.flashMAC(p, name, exp))
}

// escrowFlash is the verdict banner of a verified token made for device deviceID, else nil: the
// device id is in the name the MAC covers, and the page shows only a token that names its own
// device, so one device's verdict never reads as another's.
func (s *Service) escrowFlash(p *auth.Principal, deviceID, token string) *CauseView {
	parts := strings.Split(token, "~")
	if len(parts) != 3 || deviceID == "" || !strings.HasPrefix(parts[0], "escrow:"+deviceID+":") {
		return nil
	}
	word, ok := strings.CutPrefix(parts[0], "escrow:"+deviceID+":")
	if !ok || !escrowFlashRE.MatchString(word) {
		return nil
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || s.now().Unix() > exp || !hmac.Equal([]byte(parts[2]), []byte(s.flashMAC(p, parts[0], exp))) {
		return nil
	}
	verdict, class, _ := strings.Cut(word, ".")
	res := escrowVerdict(verdict, class)
	if res.Verdict == "" {
		return nil
	}
	return &res.View
}

// escrowVerifyPost is POST /ui/devices/{id}/escrow/verify (admin).
func (s *Service) escrowVerifyPost(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, p, http.StatusBadRequest, "Form not read", "The form could not be read.")
		return
	}
	id := r.PathValue("id")
	if _, err := s.Store.GetDevice(r.Context(), p.Scope, id); err != nil { // 404 for another customer's device, before anything else
		if !errors.Is(err, store.ErrNotFound) {
			s.log().Error("ui: device unreadable for an escrow check", logx.Err(err))
		}
		s.storeFailed(w, r, p, err)
		return
	}
	status, body, err := s.apiSend(r, http.MethodPost, "/api/v1/devices/"+id+"/escrow/verify", nil)
	if err != nil {
		s.log().Error("ui: escrow check failed", logx.Class("render"), logx.Err(err))
	}
	res := escrowAnswer(status, body, err)
	if status == http.StatusNotFound {
		s.notFound(w, r, p, "This device")
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		// A verdict changed what the rest of the page shows (the Facts chip, the alarms, the header chips):
		// htmx follows the same redirect a browser post gets, so the whole page is redrawn with the verdict
		// banner and nothing on it contradicts the panel.
		if res.Verdict != "" && res.Status == http.StatusOK {
			w.Header().Set("HX-Redirect", "/devices/"+id+"?escrow="+s.escrowFlashToken(p, id, res)+"#escrow-panel")
			w.WriteHeader(http.StatusOK)
			return
		}
		// htmx swaps no 4xx: a refusal changed nothing, so the panel, with the sentence in it, answers 200.
		pn := s.escrowPanel(r.Context(), p, id, s.fc(r.Context()))
		pn.Result = &res.View
		s.render(w, r, http.StatusOK, "device", "escrow_panel", pn)
		return
	}
	if res.Verdict != "" && res.Status == http.StatusOK {
		http.Redirect(w, r, "/devices/"+id+"?escrow="+s.escrowFlashToken(p, id, res)+"#escrow-panel", http.StatusSeeOther)
		return
	}
	s.devicePanelAt(w, r, "", res.Status, &res.View)
}
