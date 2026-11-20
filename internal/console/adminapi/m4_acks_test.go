package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P20-W3A: acknowledgements and snoozes through the admin API, and the leak test of the M4 read routes.

var episodeSeq atomic.Int64

// openEpisode opens an alarm episode of alarm (a state name such as FAILED) for a device, as the
// evaluator would: an acknowledgement answers an open alarm only.
func (e *env) openEpisode(deviceID, alarm string) {
	e.t.Helper()
	now := e.clk.Now().UnixMilli()
	id := fmt.Sprintf("01JW3AEPISODE%013d", episodeSeq.Add(1)) // 26 characters
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO alarm_episodes (id, subject_type, device_id, alarm_type, opened_at, state_since, created_at, updated_at)
			VALUES (?, 'device', ?, ?, ?, ?, ?, ?)`, id, deviceID, alarm, now-3600_000, now-3600_000, now, now)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func TestM4AcknowledgeSnoozeListAndClear(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	e.openEpisode(e.dev.ID, "FAILED")
	op := e.as("w3a-op", "operator", nil)

	b := e.wantStatus("POST", "/api/v1/acks", op, CreateAckRequest{SubjectType: "device", SubjectID: e.dev.ID, AlarmType: "failed", Kind: AckKindAcknowledge, Note: "vendor ticket open"}, 201)
	a := decode[AckResponse](t, b)
	if a.ID == "" || a.Kind != AckKindAcknowledge || a.Note != "vendor ticket open" || a.AcknowledgedByName != "w3a-op" || a.CustomerID != e.dev.CustomerID || a.ComputedAt.IsZero() {
		t.Fatalf("ack %s", b)
	}
	ro := e.as("w3a-ro", "read_only", nil)
	l := decode[AcksResponse](t, e.wantStatus("GET", "/api/v1/acks", ro, nil, 200))
	found := false
	for _, x := range l.Acks {
		found = found || x.ID == a.ID
	}
	if !found {
		t.Fatalf("the acknowledgement is listed for every signed-in role: %+v", l)
	}
	only := decode[AcksResponse](t, e.wantStatus("GET", "/api/v1/acks?subject_id="+e.dev.ID, ro, nil, 200))
	if len(only.Acks) != 1 || only.Acks[0].ID != a.ID {
		t.Fatalf("subject filter %+v", only)
	}

	// A snooze replaces it.
	until := e.clk.Now().Add(48 * time.Hour).UTC().Truncate(time.Millisecond)
	s := decode[AckResponse](t, e.wantStatus("POST", "/api/v1/acks", op, CreateAckRequest{SubjectType: "device", SubjectID: e.dev.ID, AlarmType: "failed", Kind: AckKindSnooze, SnoozeUntil: &until}, 201))
	if s.Kind != AckKindSnooze || s.SnoozeUntil == nil || !s.SnoozeUntil.Equal(until) {
		t.Fatalf("snooze %+v", s)
	}
	n := decode[AckResponse](t, e.wantStatus("POST", "/api/v1/acks", op, CreateAckRequest{SubjectType: "device", SubjectID: e.dev.ID, AlarmType: "failed", Kind: AckKindSnooze, UntilNextSuccess: true}, 201))
	if n.Kind != AckKindSnooze || !n.UntilNextSuccess {
		t.Fatalf("until next success %+v", n)
	}

	e.wantStatus("DELETE", "/api/v1/acks/"+n.ID, op, nil, 204)
	e.wantStatus("DELETE", "/api/v1/acks/"+n.ID, op, nil, 204) // already cleared: nothing changes
	if c := e.auditCount("alarm_ack.clear", n.ID); c != 1 {
		t.Fatalf("%d clear rows, want 1", c)
	}
	l = decode[AcksResponse](t, e.wantStatus("GET", "/api/v1/acks?subject_id="+e.dev.ID, owner, nil, 200))
	if len(l.Acks) != 0 {
		t.Fatalf("after clear %+v", l)
	}
	// The note is never in the audit trail.
	evs, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "alarm_ack.create"}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs.Items {
		if strings.Contains(string(ev.Details), "vendor ticket") {
			t.Fatalf("the note reached the audit trail: %s", ev.Details)
		}
	}
}

func TestM4AckRefusals(t *testing.T) {
	e := newEnv(t)
	op := e.as("w3a-op", "operator", nil)
	ack := func(req CreateAckRequest, want int) []byte {
		t.Helper()
		return e.wantStatus("POST", "/api/v1/acks", op, req, want)
	}
	dev := e.dev.ID

	b := ack(CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindAcknowledge}, 409)
	if code, reason, detail := m4Reason(t, b); code != api.CodeConflict || reason != "alarm_not_open" || detail != detailAlarmNotOpen {
		t.Fatalf("not open: %s", b)
	}
	e.openEpisode(dev, "FAILED")
	past := e.clk.Now().Add(-time.Minute)
	far := e.clk.Now().Add(store.MaxSnooze + time.Hour)
	soon := e.clk.Now().Add(time.Hour)
	for name, tc := range map[string]struct {
		req   CreateAckRequest
		field string
	}{
		"snooze in the past":        {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindSnooze, SnoozeUntil: &past}, "snooze_until"},
		"snooze past the limit":     {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindSnooze, SnoozeUntil: &far}, "snooze_until"},
		"snooze without an end":     {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindSnooze}, "snooze_until"},
		"snooze with both ends":     {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindSnooze, SnoozeUntil: &soon, UntilNextSuccess: true}, "snooze_until"},
		"acknowledge with an end":   {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindAcknowledge, SnoozeUntil: &soon}, "kind"},
		"no kind":                   {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed"}, "kind"},
		"a customer alarm type":     {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "missing_customer", Kind: AckKindAcknowledge}, "alarm_type"},
		"an unknown subject type":   {CreateAckRequest{SubjectType: "site", SubjectID: dev, AlarmType: "failed", Kind: AckKindAcknowledge}, "subject_type"},
		"a note that is too long":   {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindAcknowledge, Note: strings.Repeat("x", 501)}, "note"},
		"a note with control bytes": {CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindAcknowledge, Note: "a\x00b"}, "note"},
	} {
		b := ack(tc.req, 400)
		if fe := fieldOf(t, b); len(fe) < 1 || fe[0].Field != tc.field {
			t.Errorf("%s: %s", name, b)
		}
	}
	e.wantStatus("POST", "/api/v1/acks", op, `{"subject_type":"device","subject_id":"`+dev+`","alarm_type":"failed","kind":"acknowledge","extra":1}`, 400)
	ack(CreateAckRequest{SubjectType: "device", SubjectID: "01HNOSUCHDEVICE00000000000", AlarmType: "failed", Kind: AckKindAcknowledge}, 404)
	e.wantStatus("DELETE", "/api/v1/acks/01HNOSUCHACK00000000000000", op, nil, 404)

	// Roles and scope: read-only cannot acknowledge; an operator limited to another customer cannot
	// see or acknowledge this device.
	e.wantStatus("POST", "/api/v1/acks", e.as("w3a-ro", "read_only", nil), CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindAcknowledge}, 403)
	cs, err := e.st.ListCustomers(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	other := ""
	for _, c := range cs {
		if c.ID != e.dev.CustomerID {
			other = c.ID
		}
	}
	scoped := e.as("w3a-scoped", "operator", []string{other})
	e.wantStatus("POST", "/api/v1/acks", scoped, CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindAcknowledge}, 404)
	a := decode[AckResponse](t, ack(CreateAckRequest{SubjectType: "device", SubjectID: dev, AlarmType: "failed", Kind: AckKindAcknowledge}, 201))
	for _, x := range decode[AcksResponse](t, e.wantStatus("GET", "/api/v1/acks", scoped, nil, 200)).Acks {
		if x.ID == a.ID {
			t.Fatal("a scoped caller lists another customer's acknowledgement")
		}
	}
	e.wantStatus("DELETE", "/api/v1/acks/"+a.ID, scoped, nil, 404)
}

// ---- the leak test ----

// m4LeakSeeds are path-like and patient-like strings seeded into the free text of the console;
// the markers are what every response body of the new read routes is searched for.
var m4LeakSeeds = []struct{ value, marker string }{
	{`C:\Users\jdoe\Documents\Patients\chart-0042.pdf`, `chart-0042`},
	{`/home/clinic/records/xray_2026.dcm`, `xray_2026`},
	{`DOE, JANE DOB 1970-01-01 MRN 445566`, `445566`},
	{`\\fileserver\share\billing\ssn-123-45-6789.xlsx`, `ssn-123`},
	{`patient: Smith, John (Medicare 1EG4-TE5-MK72)`, `1EG4-TE5-MK72`},
}

func TestM4ReadRoutesCarryNoPathOrPatientText(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	fake := mail.NewFakeSender()
	e.withMail(fake)
	e.openEpisode(e.dev.ID, "FAILED")
	notes := ""
	for _, s := range m4LeakSeeds {
		notes += s.value + " "
	}
	e.wantStatus("POST", "/api/v1/acks", owner, CreateAckRequest{SubjectType: "device", SubjectID: e.dev.ID, AlarmType: "failed", Kind: AckKindAcknowledge, Note: notes}, 201)
	seed := func(i int) string { return m4LeakSeeds[i%len(m4LeakSeeds)].value }
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		for _, x := range []struct {
			q    string
			args []any
		}{
			{`UPDATE customers SET notes = ?`, []any{seed(0)}},
			{`UPDATE devices SET notes = ?, pause_reason = ?`, []any{seed(1), seed(2)}},
			{`UPDATE devices SET os_version = ?, agent_version = ?`, []any{seed(3), seed(4)}},
			{`UPDATE device_policy_overrides SET reason = ?`, []any{seed(3)}},
			{`UPDATE runs SET error_counts = json_object(?, 3) WHERE error_class IS NOT NULL`, []any{seed(0)}},
			{`UPDATE alarm_acks SET note = ? WHERE note IS NOT NULL AND note NOT LIKE '%chart-0042%'`, []any{seed(4)}},
		} {
			if _, err := tx.Exec(x.q, x.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Some mail in the log: a recipient, a failed test, a digest.
	e.wantStatus("POST", "/api/v1/notification-rules", owner, map[string]any{"name": "Night desk", "address": "ops@example.com"}, 201)
	fake.FailNext(1, &mail.SendError{Stage: mail.StageAuth, Class: mail.ClassAuthRejected})
	e.wantStatus("POST", "/api/v1/settings/smtp/test", owner, SMTPTestRequest{To: "ops@example.com"}, 200)
	e.wantStatus("POST", "/api/v1/digest/send", owner, nil, 200)

	check := func(route string, body []byte) {
		t.Helper()
		for _, s := range m4LeakSeeds {
			if strings.Contains(string(body), s.marker) {
				t.Errorf("%s carries %q: %s", route, s.marker, body)
			}
		}
	}
	for _, route := range []string{"/api/v1/notification-rules", "/api/v1/notification-log", "/api/v1/settings/smtp", "/api/v1/digest/preview"} {
		check(route, e.wantStatus("GET", route, owner, nil, 200))
	}
	// The acknowledgement list carries the note (that is where it is shown), and nothing else may.
	b := e.wantStatus("GET", "/api/v1/acks", owner, nil, 200)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	shown := false
	for _, a := range raw["acks"].([]any) {
		m := a.(map[string]any)
		if n, _ := m["note"].(string); strings.Contains(n, "chart-0042") {
			shown = true
		}
		delete(m, "note")
	}
	if !shown {
		t.Fatal("the note is shown in the acknowledgement list")
	}
	rest, _ := json.Marshal(raw)
	check("/api/v1/acks (without the note)", rest)
}
