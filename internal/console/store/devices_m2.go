package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// This file is the M2 device surface (ticket M2-W1C): the filtered device list
// joined with customer names, the "next due" estimate, the pure merge with the
// state engine's verdicts (sort, state filter, paging), the audited settings
// edit (rename, notes, class, update ring) and the device's audit rows. The M0
// ListDevices / UpdateDevice in devices.go are untouched.

// DeviceRow is a device with its customer's name.
type DeviceRow struct {
	Device
	CustomerName string
}

// DeviceFilter narrows ListDeviceRows at the SQL level. The computed health
// state is not stored, so the state filter lives in BuildDeviceTable.
type DeviceFilter struct {
	CustomerID string
	// Search is a case-insensitive substring of the hostname, friendly name or customer name.
	Search         string
	DeviceClass    string
	UpdateRing     string
	IncludeRetired bool
}

// ListDeviceRows returns every device inside scope that matches f, ordered by
// hostname. A CustomerID outside scope yields no rows (never another
// customer's): the scope clause is ANDed in, not replaced.
func (s *Store) ListDeviceRows(ctx context.Context, scope Scope, f DeviceFilter) ([]DeviceRow, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	clause, args := scope.clause("d.customer_id")
	where := []string{clause}
	if f.CustomerID != "" {
		where, args = append(where, "d.customer_id = ?"), append(args, f.CustomerID)
	}
	if f.Search != "" {
		pat := "%" + escapeLike(strings.TrimSpace(f.Search)) + "%"
		where = append(where, `(d.hostname LIKE ? ESCAPE '\' OR d.friendly_name LIKE ? ESCAPE '\' OR c.name LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat, pat)
	}
	if f.DeviceClass != "" {
		where, args = append(where, "d.device_class = ?"), append(args, f.DeviceClass)
	}
	if f.UpdateRing != "" {
		where, args = append(where, "d.update_ring = ?"), append(args, f.UpdateRing)
	}
	if !f.IncludeRetired {
		where = append(where, "d.retired_at IS NULL")
	}
	query := `SELECT ` + prefixCols(deviceCols, "d.") + `, c.name FROM devices d JOIN customers c ON c.id = d.customer_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY d.hostname COLLATE NOCASE, d.id`
	var out []DeviceRow
	err := s.Read(ctx, func(tx *Tx) error {
		var err error
		out, err = collect(tx, query, args, func(sc scanner) (DeviceRow, error) {
			var r DeviceRow
			d, err := scanDevice(extraScanner{sc, []any{&r.CustomerName}})
			if err != nil {
				return DeviceRow{}, err
			}
			r.Device = d
			return r, nil
		})
		return err
	})
	return out, err
}

// ---------------------------------------------------------------------------
// next due
// ---------------------------------------------------------------------------

// Kinds of NextDue.
const (
	DueScheduled = "scheduled" // At is the earliest next start
	DueNow       = "due"       // a run may start now
	DuePaused    = "paused"    // parked: nothing is due
	DueRetired   = "retired"
	DueUnknown   = "unknown" // no usable schedule (never guessed)
)

// NextDue is when a device's schedule next allows a backup. It is an estimate
// from the effective policy and the last success: the console does not know the
// device's local zone, so a time window is read in loc.
type NextDue struct {
	Kind string
	At   time.Time
}

// DeviceNextDue estimates the next scheduled backup of each device in ids that
// lies inside scope (a device outside scope has no entry). lastSuccess is the
// engine's last success per device id; loc is the zone window times are read in
// (nil = UTC). Nothing is written.
func (s *Store) DeviceNextDue(ctx context.Context, scope Scope, ids []string, lastSuccess map[string]time.Time, now time.Time, loc *time.Location) (map[string]NextDue, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if loc == nil {
		loc = time.UTC
	}
	out := make(map[string]NextDue, len(ids))
	err := s.Read(ctx, func(tx *Tx) error {
		comp, err := newComposer(tx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			var customerID string
			var pol sql.NullString
			var paused, until, retired sql.NullInt64
			err := tx.QueryRow(`SELECT d.customer_id, c.policy_id, d.paused_at, d.paused_until, d.retired_at
				FROM devices d JOIN customers c ON c.id = d.customer_id WHERE d.id = ?`, id).Scan(&customerID, &pol, &paused, &until, &retired)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && !scope.Allows(customerID)) {
				continue
			}
			if err != nil {
				return err
			}
			switch {
			case retired.Valid:
				out[id] = NextDue{Kind: DueRetired}
				continue
			case paused.Valid && (!until.Valid || until.Int64 > now.UnixMilli()):
				out[id] = NextDue{Kind: DuePaused}
				continue
			}
			p, _, _, err := comp.effective(id, pol.String)
			if err != nil {
				out[id] = NextDue{Kind: DueUnknown} // fail loud in the column: no guess
				continue
			}
			dec := policy.Due(policy.DueInput{Now: now.In(loc), LastSuccess: lastSuccess[id], Schedule: p.Schedule, DeviceID: id})
			switch {
			case dec.Due:
				out[id] = NextDue{Kind: DueNow, At: now}
			case !dec.NotBefore.IsZero():
				out[id] = NextDue{Kind: DueScheduled, At: dec.NotBefore.UTC()}
			default:
				out[id] = NextDue{Kind: DueUnknown}
			}
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// the merged table
// ---------------------------------------------------------------------------

// DeviceSorts are the sort keys of the device table.
var DeviceSorts = []string{"state", "host", "customer", "last_success", "next_due", "agent", "os"}

// DeviceTableQuery shapes BuildDeviceTable.
type DeviceTableQuery struct {
	// States keeps devices whose computed primary state is one of these (state names).
	States []string
	// Sort is one of DeviceSorts; "" means state (exceptions first). Desc reverses it.
	Sort string
	Desc bool
	// Page and Size select a numbered page; when Limit > 0 the window is the
	// rows [Offset, Offset+Limit) instead (the JSON API's cursor paging).
	Page, Size    int
	Offset, Limit int
}

// DeviceTableRow is a device joined with the engine's verdict and its next due.
type DeviceTableRow struct {
	DeviceRow
	// State is the engine's verdict; InView is false when the engine has none for
	// this device, which is shown and sorted as UNKNOWN (never as healthy).
	State  state.DeviceState
	InView bool
	Next   NextDue
}

// Primary is the state the row sorts and filters by.
func (r DeviceTableRow) Primary() state.State {
	if !r.InView {
		return state.StateUnknown
	}
	return r.State.Primary
}

// DeviceTable is one page of the device table.
type DeviceTable struct {
	Rows []DeviceTableRow
	Page PageInfo
	// Total is the number of devices before the state filter.
	Total int
}

// ValidDeviceState reports whether s names a state of the precedence table.
func ValidDeviceState(s string) bool {
	for _, st := range state.Precedence() {
		if string(st) == s {
			return true
		}
	}
	return false
}

func stateRank(st state.State) int {
	for i, p := range state.Precedence() {
		if p == st {
			return i
		}
	}
	return len(state.Precedence())
}

// BuildDeviceTable joins rows with the engine's verdicts (view) and next-due
// estimates, applies the state filter, sorts and pages. It is pure.
func BuildDeviceTable(rows []DeviceRow, view []state.DeviceState, due map[string]NextDue, q DeviceTableQuery) (DeviceTable, error) {
	var bad []FieldError
	want := map[string]bool{}
	for _, st := range q.States {
		st = strings.ToUpper(strings.TrimSpace(st))
		if st == "" {
			continue
		}
		if !ValidDeviceState(st) {
			bad = append(bad, fieldf("state", "Unknown device state."))
			break
		}
		want[st] = true
	}
	sortKey := q.Sort
	if sortKey == "" {
		sortKey = "state"
	}
	known := false
	for _, k := range DeviceSorts {
		known = known || k == sortKey
	}
	if !known {
		bad = append(bad, fieldf("sort", "Unknown sort key."))
	}
	if err := validation(bad); err != nil {
		return DeviceTable{}, err
	}
	byID := make(map[string]state.DeviceState, len(view))
	for _, d := range view {
		byID[d.ID] = d
	}
	all := make([]DeviceTableRow, 0, len(rows))
	for _, r := range rows {
		tr := DeviceTableRow{DeviceRow: r, Next: due[r.ID]}
		if tr.Next.Kind == "" {
			tr.Next.Kind = DueUnknown
		}
		if ds, ok := byID[r.ID]; ok {
			tr.State, tr.InView = ds, true
		}
		all = append(all, tr)
	}
	t := DeviceTable{Total: len(all)}
	kept := all[:0:0]
	for _, r := range all {
		if len(want) > 0 && !want[string(r.Primary())] {
			continue
		}
		kept = append(kept, r)
	}
	farFuture := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	dueKey := func(r DeviceTableRow) time.Time {
		switch r.Next.Kind {
		case DueNow, DueScheduled:
			return r.Next.At
		}
		return farFuture
	}
	host := func(a, b DeviceTableRow) bool { return strings.ToLower(a.Hostname) < strings.ToLower(b.Hostname) }
	less := func(a, b DeviceTableRow) bool {
		switch sortKey {
		case "host":
			return host(a, b)
		case "customer":
			if ca, cb := strings.ToLower(a.CustomerName), strings.ToLower(b.CustomerName); ca != cb {
				return ca < cb
			}
		case "last_success":
			if la, lb := a.State.Detail.LastSuccess, b.State.Detail.LastSuccess; !la.Equal(lb) {
				return la.Before(lb)
			}
		case "next_due":
			if da, db := dueKey(a), dueKey(b); !da.Equal(db) {
				return da.Before(db)
			}
		case "agent":
			if va, vb := a.AgentVersion, b.AgentVersion; va != vb {
				if c := state.CompareVersions(va, vb); c != 0 {
					return c < 0
				}
				return va < vb
			}
		case "os":
			if oa, ob := strings.ToLower(a.OSVersion), strings.ToLower(b.OSVersion); oa != ob {
				return oa < ob
			}
		default: // state: loudest first (severity, then the §4.2 row order)
			sa, sb := severityOf(a), severityOf(b)
			if sa != sb {
				return sa > sb
			}
			if ra, rb := stateRank(a.Primary()), stateRank(b.Primary()); ra != rb {
				return ra < rb
			}
		}
		return host(a, b)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if q.Desc {
			return less(kept[j], kept[i])
		}
		return less(kept[i], kept[j])
	})
	off, end, info := windowOf(len(kept), q.Page, q.Size, q.Offset, q.Limit)
	t.Rows, t.Page = kept[off:end], info
	if t.Rows == nil {
		t.Rows = []DeviceTableRow{}
	}
	return t, nil
}

// DeviceTable reads the devices inside scope that match f, joins them with the
// engine's verdicts (view, from health.Evaluator.Fleet for the same scope), and
// returns the sorted, state-filtered page described by q, each row carrying its
// next-due estimate. The estimate is computed for every candidate row only when
// the table is sorted by it, otherwise just for the rows of the page.
func (s *Store) DeviceTable(ctx context.Context, scope Scope, f DeviceFilter, view []state.DeviceState, q DeviceTableQuery, now time.Time, loc *time.Location) (DeviceTable, error) {
	rows, err := s.ListDeviceRows(ctx, scope, f)
	if err != nil {
		return DeviceTable{}, err
	}
	lastSuccess := make(map[string]time.Time, len(view))
	for _, d := range view {
		lastSuccess[d.ID] = d.Detail.LastSuccess
	}
	var due map[string]NextDue
	if q.Sort == "next_due" {
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		if due, err = s.DeviceNextDue(ctx, scope, ids, lastSuccess, now, loc); err != nil {
			return DeviceTable{}, err
		}
	}
	tb, err := BuildDeviceTable(rows, view, due, q)
	if err != nil {
		return DeviceTable{}, err
	}
	if due == nil && len(tb.Rows) > 0 {
		ids := make([]string, len(tb.Rows))
		for i, r := range tb.Rows {
			ids[i] = r.ID
		}
		if due, err = s.DeviceNextDue(ctx, scope, ids, lastSuccess, now, loc); err != nil {
			return DeviceTable{}, err
		}
		for i := range tb.Rows {
			if nd, ok := due[tb.Rows[i].ID]; ok {
				tb.Rows[i].Next = nd
			}
		}
	}
	return tb, nil
}

// severityOf ranks a row for the exception-first order. A device the engine has
// no verdict for counts as critical: a hole in the data is loud.
func severityOf(r DeviceTableRow) state.Severity {
	if !r.InView {
		return state.SeverityCritical
	}
	return r.State.Alarm
}

// ---------------------------------------------------------------------------
// settings edit
// ---------------------------------------------------------------------------

// DeviceSettingsPatch is a set of optional edits to the operator-owned settings
// of a device; nil fields are left alone. An empty FriendlyName or Notes clears
// it.
type DeviceSettingsPatch struct {
	FriendlyName *string
	Notes        *string
	DeviceClass  *string
	UpdateRing   *string
}

// Allowed values of the enumerated settings.
var (
	DeviceClasses = []string{"unknown", "laptop", "desktop"}
	UpdateRings   = []string{"stable", "pilot"}
)

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// UpdateDeviceSettings applies p to a device inside scope and writes a
// device.update audit row in the same transaction, naming the changed fields
// (and the before and after of the two enumerated ones; free text is never
// copied into the audit row). It reports whether anything changed: a patch that
// changes nothing writes no audit row. A retired device is not editable
// (ErrConflict). Rejected input is a *ValidationError.
func (s *Store) UpdateDeviceSettings(ctx context.Context, scope Scope, id string, p DeviceSettingsPatch, actor AuditActor) (Device, bool, error) {
	if err := scope.check(); err != nil {
		return Device{}, false, err
	}
	var errs []FieldError
	if p.FriendlyName == nil && p.Notes == nil && p.DeviceClass == nil && p.UpdateRing == nil {
		errs = append(errs, fieldf("settings", "Send at least one setting to change."))
	}
	var friendly, notes string
	if p.FriendlyName != nil {
		v, ok := cleanLine(*p.FriendlyName, 120)
		if !ok {
			errs = append(errs, fieldf("friendly_name", "The name must be one line of at most 120 characters."))
		}
		friendly = v
	}
	if p.Notes != nil {
		v, ok := cleanText(*p.Notes, 2000)
		if !ok {
			errs = append(errs, fieldf("notes", "Notes are limited to 2000 characters of plain text."))
		}
		notes = v
	}
	if p.DeviceClass != nil && !oneOf(*p.DeviceClass, DeviceClasses) {
		errs = append(errs, fieldf("device_class", "Use unknown, laptop or desktop."))
	}
	if p.UpdateRing != nil && !oneOf(*p.UpdateRing, UpdateRings) {
		errs = append(errs, fieldf("update_ring", "Use stable or pilot."))
	}
	if err := validation(errs); err != nil {
		return Device{}, false, err
	}
	var out Device
	changed := false
	err := s.Write(ctx, func(tx *Tx) error {
		cur, err := getDevice(tx, scope, id)
		if err != nil {
			return err
		}
		if cur.RetiredAt != nil {
			return conflictf("device is retired")
		}
		sets, args := []string{}, []any{}
		var fields []string
		details := map[string]any{}
		if p.FriendlyName != nil && friendly != cur.FriendlyName {
			sets, args, fields = append(sets, "friendly_name = ?"), append(args, nullStr(friendly)), append(fields, "friendly_name")
		}
		if p.Notes != nil && notes != cur.Notes {
			sets, args, fields = append(sets, "notes = ?"), append(args, nullStr(notes)), append(fields, "notes")
		}
		if p.DeviceClass != nil && *p.DeviceClass != cur.DeviceClass {
			sets, args, fields = append(sets, "device_class = ?"), append(args, *p.DeviceClass), append(fields, "device_class")
			details["device_class"] = map[string]string{"from": cur.DeviceClass, "to": *p.DeviceClass}
		}
		if p.UpdateRing != nil && *p.UpdateRing != cur.UpdateRing {
			sets, args, fields = append(sets, "update_ring = ?"), append(args, *p.UpdateRing), append(fields, "update_ring")
			details["update_ring"] = map[string]string{"from": cur.UpdateRing, "to": *p.UpdateRing}
		}
		if len(fields) == 0 {
			out = cur
			return nil
		}
		details["fields"] = fields
		sets, args = append(sets, "updated_at = ?"), append(args, tx.nowMS())
		if _, err := tx.Exec(`UPDATE devices SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...); err != nil {
			return err
		}
		ev, err := actor.event("device.update", "device", id, cur.CustomerID, details)
		if err != nil {
			return err
		}
		if err := audited(tx, ev); err != nil {
			return err
		}
		changed = true
		out, err = getDevice(tx, scope, id)
		return err
	})
	if err != nil {
		return Device{}, false, err
	}
	return out, changed, nil
}

// ---------------------------------------------------------------------------
// the device's audit rows
// ---------------------------------------------------------------------------

// ListDeviceAudit returns the audit events about one device, newest first. The
// device must lie inside scope (ErrNotFound otherwise); the events are further
// limited to the scope the way ListAudit does.
func (s *Store) ListDeviceAudit(ctx context.Context, scope Scope, deviceID string, page Page) (Listing[AuditEvent], error) {
	if _, err := s.GetDevice(ctx, scope, deviceID); err != nil {
		return Listing[AuditEvent]{}, err
	}
	return s.ListAudit(ctx, scope, AuditFilter{ObjectType: "device", ObjectID: deviceID}, page)
}

// DeviceAuditTotal counts the audit events about one device that ListDeviceAudit
// would return, so the audit tab can say "Showing 1-50 of N". The device must
// lie inside scope (ErrNotFound otherwise).
func (s *Store) DeviceAuditTotal(ctx context.Context, scope Scope, deviceID string) (int, error) {
	if _, err := s.GetDevice(ctx, scope, deviceID); err != nil {
		return 0, err
	}
	where := `object_type = 'device' AND object_id = ?`
	args := []any{deviceID}
	if !scope.All {
		clause, a := scope.clause("customer_id")
		where, args = where+" AND "+clause, append(args, a...)
	}
	var n int
	err := s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE `+where, args...).Scan(&n)
	})
	return n, err
}
