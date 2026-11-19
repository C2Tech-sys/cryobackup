package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Path-like and patient-like strings that must never reach a message. Each is seeded into the free
// text of the fixture world; the markers are what the rendered output is searched for.
var leakSeeds = []struct{ value, marker string }{
	{`C:\Users\jdoe\Documents\Patients\chart-0042.pdf`, `chart-0042`},
	{`/home/clinic/records/xray_2026.dcm`, `xray_2026`},
	{`DOE, JANE DOB 1970-01-01 MRN 445566`, `445566`},
	{`\\fileserver\share\billing\ssn-123-45-6789.xlsx`, `ssn-123`},
	{`patient: Smith, John (Medicare 1EG4-TE5-MK72)`, `1EG4-TE5-MK72`},
}

// seedFreeText writes the seeds into every free-text field the fixture world has: notes, reasons,
// policy names, acknowledgement notes, agent-reported strings and the per-class error counts of runs.
func seedFreeText(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	s := func(i int) string { return leakSeeds[i%len(leakSeeds)].value }
	stmts := []struct {
		q    string
		args []any
	}{
		{`UPDATE customers SET notes = ?`, []any{s(0)}},
		{`UPDATE customers SET contacts = json_array(json_object('name', ?, 'email', 'front@example.test'))`, []any{s(2)}},
		{`UPDATE policies SET name = ? || ' ' || id`, []any{s(1)}},
		{`UPDATE devices SET pause_reason = ? WHERE paused_at IS NOT NULL`, []any{s(4)}},
		{`UPDATE devices SET retired_reason = ? WHERE retired_at IS NOT NULL`, []any{s(3)}},
		{`UPDATE devices SET purge_reason = ? WHERE purge_requested_at IS NOT NULL`, []any{s(0)}},
		{`UPDATE devices SET os_version = ?, agent_version = ?`, []any{s(1), s(2)}},
		{`UPDATE device_policy_overrides SET reason = ?`, []any{s(3)}},
		{`UPDATE alarm_acks SET note = ?`, []any{s(4)}},
		{`UPDATE runs SET error_counts = json_object(?, 3) WHERE error_class IS NOT NULL`, []any{s(0)}},
	}
	if err := st.Write(ctx, func(tx *store.Tx) error {
		for _, x := range stmts {
			if _, err := tx.Exec(x.q, x.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Every rendered subject and body (alert, recovery and the daily digest), over every alarm of the
// fixture world with seeded free text, carries none of the seeds.
func TestRenderedMessagesCarryNoPathOrPatientText(t *testing.T) {
	ctx := context.Background()
	now := fixtures.Now()
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	seedFreeText(t, st)
	ev := &health.Evaluator{Store: st, Clock: func() time.Time { return now }, MasterLoaded: func() bool { return true }}
	f, err := ev.Alarms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Every alarm of every device and every customer, critical or not, as one batch; and every
	// device as a closed episode for the recovery notice.
	var alerts, recoveries []store.Episode
	for _, d := range f.View.Devices {
		closed := now
		for _, s := range append([]state.State{d.Primary}, d.AlarmFlags...) {
			e := store.Episode{ID: d.ID, SubjectType: store.EpisodeDevice, SubjectID: d.ID, CustomerID: d.CustomerID, AlarmType: string(s), StateSince: d.Since}
			alerts = append(alerts, e)
			e.ClosedAt = &closed
			recoveries = append(recoveries, e)
		}
	}
	for _, c := range f.View.Customers {
		alerts = append(alerts, store.Episode{SubjectType: store.EpisodeCustomer, SubjectID: c.ID, CustomerID: c.ID, AlarmType: store.AlarmMissing})
	}
	if len(alerts) < 20 {
		t.Fatalf("the fixture world gave only %d alarms: the test would prove little", len(alerts))
	}
	var out []string
	for _, render := range []func(state.FleetView, []store.Episode, Options) (Content, error){RenderAlert, RenderRecovery} {
		for _, batch := range [][]store.Episode{alerts, recoveries} {
			c, err := render(f.View, batch, Options{ConsoleURL: "https://console.example.test/"})
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, c.Subject, c.Text, c.HTML)
		}
	}
	// The daily digest of the same world, with its acknowledgements (whose notes are seeded).
	acks, err := st.ListActiveAcks(ctx, store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	dg, err := RenderDigest(f.View, acks, Options{ConsoleURL: "https://console.example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) == 0 || !strings.Contains(dg.Text, "[ACKNOWLEDGED]") && !strings.Contains(dg.Text, "[SNOOZED]") {
		t.Fatalf("the digest of the fixture world marks no acknowledgement (%d active): the test would prove little", len(acks))
	}
	out = append(out, dg.Subject, dg.Text, dg.HTML)
	all := strings.Join(out, "\n")
	if len(f.View.Devices) == 0 || !strings.Contains(all, f.View.Devices[0].Hostname) {
		t.Fatal("the rendered output names no host: the test renders nothing")
	}
	for _, s := range leakSeeds {
		if strings.Contains(all, s.marker) {
			t.Errorf("a rendered message carries the seeded text %q", s.marker)
		}
	}
	for _, frag := range []string{`C:\`, `\\fileserver`, `/home/`, `.pdf`, `.dcm`, `.xlsx`, `Patients`, `DOB`, `MRN`} {
		if strings.Contains(all, frag) {
			t.Errorf("a rendered message carries %q", frag)
		}
	}
}
