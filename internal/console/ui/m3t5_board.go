package ui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T5: the fleet board's filter (B01, B03): a state, a ring segment, a saved
// view; the saved-views control; the state-explain tooltip.

// ageSegLabel and custSegLabel name a ring segment in the filter chips.
func ageSegLabel(seg string, stale time.Duration) string {
	h := int(stale / time.Hour)
	switch seg {
	case "under24":
		return "under 24 h"
	case "mid":
		return fmt.Sprintf("24–%d h", h)
	case "over":
		return fmt.Sprintf("over %d h or never", h)
	case "parked":
		return "parked"
	}
	return seg
}

func custSegLabel(seg string) string {
	switch seg {
	case "ok":
		return "fully within SLA"
	case "warn":
		return "has warnings"
	case "crit":
		return "has critical or missing"
	case "other":
		return "onboarding or empty"
	}
	return seg
}

// ringLinks makes each legend row of a ring a link that filters the board by its
// segment (B03): a second click on the chosen segment clears it. A segment with no
// devices is not a link: nothing would be shown.
func ringLinks(r *Ring, segs []string, bq boardQuery, set func(boardQuery, string) boardQuery, cur string) {
	for i := range r.Legend {
		if i >= len(segs) {
			break
		}
		r.Legend[i].Current = cur == segs[i]
		if r.Legend[i].N > 0 || r.Legend[i].Current {
			r.Legend[i].Href = href(set(bq, segs[i]))
		}
	}
}

// boardData builds the fleet board for the filter bq: the evaluated fleet, the
// ten-day strips, KPIs and rings, with the filter (a state, an age or customer ring
// segment, a saved view) applied through one clause tree (m3t5_filter.go).
func (s *Service) boardData(ctx context.Context, p *auth.Principal, f health.Fleet, bq boardQuery, events string, oob bool) BoardData {
	c := s.fc(ctx)
	strips, err := health.TenDay(ctx, s.Store, p.Scope, f.View, f.Computed, s.loc(ctx))
	if err != nil {
		s.log().Warn("ten day strips unavailable", logx.Err(err))
		strips = nil
	}
	metas := s.deviceMetas(ctx, p)
	pg := s.page(ctx, p, f, "Fleet health", "fleet", events)
	pg.Banners = buildBannersIn(f, oob, c.Loc).forRole(p.Role)
	pg.Stamp.OOB = oob
	b := BoardData{Page: pg, Columns: 5, AsOf: c.clock(f.Computed), QS: bq.QS()}
	stale := state.DefaultThresholds().StaleAfter

	var viewRaw []byte
	if bq.View != "" {
		if v, err := s.Store.GetSavedView(ctx, p.Scope, p.Admin.ID, bq.View); err == nil && v.Entity != "devices" {
			// A view of another table: its clauses name runs or customers, not devices. Applying them here would
			// match nothing and read as "0 of 25" with no word of why.
			b.ViewWarn = &CauseView{Class: ClassWarn, Title: "This view belongs to the " + viewPageName(v.Entity) + " page.",
				Text: "It filters " + v.Entity + ", not devices, so every device is shown here.", Link: &Link{Href: viewPageHref(v.Entity, v.ID), Label: "Open it on the " + viewPageName(v.Entity) + " page"}}
		} else if err == nil {
			b.viewLoaded, viewRaw = &v, v.Filter
		} else {
			b.ViewWarn = &CauseView{Class: ClassWarn, Title: "View not found.", Text: "The view in this link was deleted, or it is not shared with you. Every device is shown."}
		}
	}
	tree, perr := parseVF(bq.filterJSON(viewRaw, stale))
	if perr != nil {
		tree = nil
		b.ViewWarn = &CauseView{Class: ClassCrit, Title: "This view cannot be applied.", Text: "Its filter is not one the board can read, so every device is shown. Open it through the API, or save it again from the board."}
	}

	// the active filters, as removable chips
	if bq.View != "" && b.viewLoaded != nil {
		b.Filter.Chips = append(b.Filter.Chips, FilterChip{Label: "View: " + b.viewLoaded.Name, Href: href(boardQuery{States: bq.States, Age: bq.Age, Cust: bq.Cust})})
	}
	for _, st := range bq.States {
		b.Filter.Chips = append(b.Filter.Chips, FilterChip{Label: "State: " + stateLabel(state.State(st)), Href: href(bq.withoutState(st))})
	}
	if bq.Age != "" {
		b.Filter.Chips = append(b.Filter.Chips, FilterChip{Label: "Last success: " + ageSegLabel(bq.Age, stale), Href: href(bq.withAge(bq.Age))})
	}
	if bq.Cust != "" {
		b.Filter.Chips = append(b.Filter.Chips, FilterChip{Label: "Customers: " + custSegLabel(bq.Cust), Href: href(bq.withCust(bq.Cust))})
	}
	b.Filter.Active = len(b.Filter.Chips) > 0

	deviceFilter := tree != nil // a state, an age segment or a view's clauses: customers with no device cannot match them
	running, total, shown := 0, 0, 0
	rowsByCustomer := map[string][]DeviceRow{}
	for _, cs := range f.View.Customers {
		custOK := bq.Cust == "" || customerSeg(cs.Status) == bq.Cust
		var rows []DeviceRow
		for _, ds := range cs.Devices {
			if ds.Primary == state.StateRetired {
				continue
			}
			total++
			ri := s.runningInfo(ctx, p, ds)
			if ri.Open {
				running++
			}
			m := metas[ds.ID]
			if !custOK || !tree.match(devFacts{DS: ds, OS: m.OS, Class: m.Class}, f.Computed) {
				continue
			}
			shown++
			rows = append(rows, deviceRow(ds, m, strips, ri, c))
		}
		rowsByCustomer[cs.ID] = rows
	}
	b.Filter.Shown, b.Filter.Total = shown, total
	for _, cs := range f.View.Customers {
		rows := rowsByCustomer[cs.ID]
		// The customer ring counts customers, devices or none: a customer of the chosen segment is shown with
		// its rows, or with none (MISSING, onboarding, empty). Only a filter on devices drops a customer with no match.
		if bq.Cust != "" && customerSeg(cs.Status) != bq.Cust {
			continue
		}
		if deviceFilter && len(rows) == 0 {
			continue
		}
		g := groupView(cs, rows, c)
		if b.Filter.Active {
			g.Collapsed = false // every matching group is open, so the rows on screen are the count (fresh.js keeps it so)
		}
		b.Groups = append(b.Groups, g)
	}
	if b.Filter.Active {
		b.Filter.CustText = plural(len(b.Groups), "customer", "customers")
	}
	b.KPIs = kpis(f, c, running)
	b.Rings = []Ring{ageRing(f, c), customerRing(f)}
	ringLinks(&b.Rings[0], ageSegs, bq, boardQuery.withAge, bq.Age)
	ringLinks(&b.Rings[1], custSegs, bq, boardQuery.withCust, bq.Cust)
	switch {
	case f.View.FirstRun:
		b.Empty = &EmptyView{Kind: "neutral", Title: "No customers yet",
			Body: "Create the first customer and an enrollment token with: cryo-console token --create-customer \"Customer name\". Devices appear here as pending their first backup."}
	case total == 0:
		b.Empty = &EmptyView{Kind: "alarm", Title: "No devices are enrolled",
			Body: "Nothing is being backed up. Enroll a customer or check the agent listener."}
	case b.Filter.Active && len(b.Groups) == 0:
		b.Empty = &EmptyView{Kind: "neutral", Title: "No devices match these filters",
			Body: fmt.Sprintf("0 of %d shown.", total), Link: &Link{Href: "/", Label: "Clear filters"}}
	}
	return b
}

// boardViews is the saved-views control of the board page. It is not part of the
// live region (#board), so a live update never replaces it mid-edit.
func (s *Service) boardViews(r *http.Request, p *auth.Principal, bq boardQuery, loaded *store.SavedView) *ViewsControl {
	c := s.viewsControl(r, p, s.boardViewState(bq, loaded))
	return c
}

// ---- the state-explain tooltip ----

// ExplainLine is one state the device is in, with its reason.
type ExplainLine struct {
	Chip  Chip
	Alarm bool
	Text  string
}

// ExplainView is the fragment of the state-explain tooltip: GET /api/v1/devices/{id}/state
// in words, for a hover or focus on a state chip.
type ExplainView struct {
	Lines []ExplainLine
	Facts []Fact
	Note  string
	AsOf  string
	Err   string
}

const patExplain = "GET /ui/devices/{id}/explain"

func (s *Service) explainFragment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idParam(id) {
		http.NotFound(w, r)
		return
	}
	var st adminapi.DeviceStateResponse
	if err := s.apiGet(r, "/api/v1/devices/"+id+"/state", &st); err != nil {
		if apiStatus(err) == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		s.log().Error("ui: state explain failed", logx.Class("render"), logx.Err(err))
		s.renderFragment(w, r, ExplainView{Err: "The live explanation could not be read. Reload the page to try again."})
		return
	}
	s.renderFragment(w, r, s.explainOf(r.Context(), st))
}

func (s *Service) renderFragment(w http.ResponseWriter, r *http.Request, v ExplainView) {
	out, err := s.renderString("board", "state_explain", v)
	if err != nil {
		http.Error(w, "The explanation could not be rendered.", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out))
}

// explainOf words the API's answer for the tooltip. The API's own sentence is
// used except where it prints an agent-supplied version label or a UTC time: those
// are worded here, through safeVersion and the display zone.
func (s *Service) explainOf(ctx context.Context, st adminapi.DeviceStateResponse) ExplainView {
	c := s.fcAt(ctx, st.ComputedAt)
	v := ExplainView{AsOf: "Computed " + c.clock(st.ComputedAt) + " (" + c.Zone + ")"}
	for _, e := range st.Explain {
		line := ExplainLine{Alarm: e.Alarming, Text: e.Text}
		stt := state.State(e.State)
		spec, ok := catalogue[stt]
		if !ok {
			spec = chipSpec{"UNKNOWN", ClassCrit}
		}
		line.Chip = Chip{ID: "x-" + e.State, Label: spec.Label, Class: spec.Class, Small: true, Dashed: !ok}
		switch stt {
		case state.StateOutdated:
			line.Text = "The agent runs " + safeVersion(st.Inputs.AgentVersion) + "; " + st.Inputs.ExpectedVersion + " is expected."
		case state.StatePaused:
			line.Text = "Parked by an operator"
			if st.Inputs.PausedUntil != nil {
				line.Text += " until " + c.dayClock(*st.Inputs.PausedUntil)
			} else {
				line.Text += " with no end date"
			}
			if st.Inputs.PauseReason != "" {
				line.Text += ": " + st.Inputs.PauseReason
			}
			line.Text += "."
		case state.StatePendingFirst:
			line.Text = "No successful backup yet"
			if st.Inputs.GraceDeadline != nil {
				line.Text += "; the first one is due by " + c.dayClock(*st.Inputs.GraceDeadline)
			}
			line.Text += "."
		}
		v.Lines = append(v.Lines, line)
	}
	if len(v.Lines) == 0 {
		v.Err = "The console returned no explanation for this device. That is an alarm, not a clean bill of health."
	}
	at := func(t *time.Time) string {
		if t == nil {
			return "never"
		}
		return c.dayClock(*t) + " (" + ageLong(st.ComputedAt.Sub(*t)) + ")"
	}
	v.Facts = append(v.Facts, Fact{Key: "Last success", Value: at(st.Inputs.LastSuccessAt)}, Fact{Key: "Last heard from", Value: at(st.Inputs.LastSeenAt)})
	if st.Inputs.AgentVersion != "" {
		v.Facts = append(v.Facts, Fact{Key: "Agent", Value: safeVersion(st.Inputs.AgentVersion), Mono: true})
	}
	if st.Acknowledged {
		v.Note = "Acknowledged: the alarm stays visible until the cause clears."
		if st.SnoozedUntil != nil {
			v.Note = "Snoozed until " + c.dayClock(*st.SnoozedUntil) + ". The alarm stays visible until the cause clears."
		}
	}
	return v
}

// ---- routes ----

const patM3T5Pause = "POST /ui/devices/{id}/pause"
const patM3T5Resume = "POST /ui/devices/{id}/resume"

func m3t5Patterns() []string {
	return []string{patExplain, patViewCreate, patViewUpdate, patViewRename, patViewPrivate, patViewDelete, patM3T5Pause, patM3T5Resume}
}

// registerM3T5 adds the M3-T5 routes: the explain fragment and the saved-view
// writes need read_only (the admin API behind them decides who may change a
// view); parking and resuming a device need the operator role, like the other
// device actions. Every write is CSRF-checked by Require and audited by the
// admin API handler it goes through.
func (s *Service) registerM3T5(r httpx.Registrar) {
	ro := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleReadOnly, auth.ModeUI, h) }
	op := func(h http.HandlerFunc) http.Handler { return s.Auth.Require(auth.RoleOperator, auth.ModeUI, h) }
	r.Handle(patExplain, ro(s.explainFragment))
	r.Handle(patViewCreate, ro(s.viewCreate))
	r.Handle(patViewUpdate, ro(s.viewUpdate))
	r.Handle(patViewRename, ro(s.viewRename))
	r.Handle(patViewPrivate, ro(s.viewPrivate))
	r.Handle(patViewDelete, ro(s.viewDelete))
	r.Handle(patM3T5Pause, op(s.devicePausePost))
	r.Handle(patM3T5Resume, op(s.deviceResumePost))
}

// viewPageName and viewPageHref say where a saved view of another table lives.
func viewPageName(entity string) string {
	switch entity {
	case "runs":
		return "runs"
	case "customers":
		return "customers"
	}
	return "board"
}

func viewPageHref(entity, id string) string {
	switch entity {
	case "runs":
		return "/runs?view=" + id
	case "customers":
		return "/customers"
	}
	return "/?view=" + id
}
