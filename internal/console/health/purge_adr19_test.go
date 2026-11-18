package health

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// ADR 0019: the "Purge not finished" banner.

func TestAdr19PurgeBannersJudgeEveryInput(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	tried := ago(time.Minute)
	cases := []struct {
		name       string
		f          PurgeFacts
		want       bool
		count      int
		subject    string
		mustSay    []string
		mustNotSay []string
	}{
		{name: "nothing purging", f: PurgeFacts{}, want: false},
		{name: "a fresh request, no attempt yet", f: PurgeFacts{Devices: []PurgeDevice{{ID: "D1", RequestedAt: ago(10 * time.Minute)}}}, want: false},
		{name: "a fresh request whose attempt passed", f: PurgeFacts{Devices: []PurgeDevice{{ID: "D1", RequestedAt: ago(10 * time.Minute), LastAttemptAt: &tried}}}, want: false},
		{name: "a failed attempt", f: PurgeFacts{Devices: []PurgeDevice{{ID: "D1", RequestedAt: ago(10 * time.Minute), LastAttemptAt: &tried, LastErrorClass: "storage_unreachable"}}},
			want: true, count: 1, subject: "D1", mustSay: []string{"Purge not finished", "1 retired device", "storage_unreachable"}},
		{name: "purging too long with no failure", f: PurgeFacts{Devices: []PurgeDevice{{ID: "D2", RequestedAt: ago(PurgeStuckAfter + time.Minute)}}},
			want: true, count: 1, subject: "D2", mustSay: []string{"more than 3 hours"}},
		{name: "just under the threshold", f: PurgeFacts{Devices: []PurgeDevice{{ID: "D2", RequestedAt: ago(PurgeStuckAfter - time.Minute)}}}, want: false},
		{name: "two failed, one stuck, one fine: the oldest is linked", f: PurgeFacts{Devices: []PurgeDevice{
			{ID: "NEW", RequestedAt: ago(time.Minute), LastErrorClass: "worker_unreachable"},
			{ID: "OLD", RequestedAt: ago(5 * time.Hour)},
			{ID: "MID", RequestedAt: ago(time.Hour), LastErrorClass: "objects_remaining"},
			{ID: "OK", RequestedAt: ago(2 * time.Minute)},
		}}, want: true, count: 3, subject: "OLD", mustSay: []string{"2 retired devices", "objects_remaining, worker_unreachable", "1 retired device has been purging"}},
		{name: "the list cannot be read", f: PurgeFacts{NotRead: true}, want: true, count: 0, mustSay: []string{"could not be read"}},
		{name: "the job's last run failed", f: PurgeFacts{JobErrorClass: "store"}, want: true, count: 0, mustSay: []string{"device-purge job's last run failed (store)"},
			mustNotSay: []string{"stays purging"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PurgeBanners(tc.f, now)
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("banner raised: %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d banners, want 1: %+v", len(got), got)
			}
			b := got[0]
			if b.ID != BannerPurgeNotFinished || b.Severity != state.SeverityCritical || b.Count != tc.count || b.Subject != tc.subject {
				t.Errorf("banner = %+v; want id %s, critical, count %d, subject %q", b, BannerPurgeNotFinished, tc.count, tc.subject)
			}
			for _, s := range tc.mustSay {
				if !strings.Contains(b.Text, s) {
					t.Errorf("text %q does not say %q", b.Text, s)
				}
			}
			for _, s := range tc.mustNotSay {
				if strings.Contains(b.Text, s) {
					t.Errorf("text %q says %q", b.Text, s)
				}
			}
		})
	}
}

func adr19Evaluator(t *testing.T, on bool) (*Evaluator, *store.Store) {
	t.Helper()
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
	return &Evaluator{Store: st, Clock: func() time.Time { return now }, PurgeBanner: on}, st
}

func purgeBanner(f Fleet) *state.Banner {
	for i := range f.View.Banners {
		if f.View.Banners[i].ID == BannerPurgeNotFinished {
			return &f.View.Banners[i]
		}
	}
	return nil
}

// The fixtures hold SPARE-PC-09: retired, purge requested 40 minutes ago, last attempt failed
// (storage_unreachable). With PurgeBanner the evaluator raises the banner and links that device;
// without it, nothing (the other banner tests read the same fixtures).
func TestAdr19EvaluatorRaisesTheBannerFromTheStore(t *testing.T) {
	ctx := context.Background()
	ev, st := adr19Evaluator(t, true)
	l, err := st.ListDevicesPurging(ctx, store.ScopeAll())
	if err != nil || len(l) != 1 {
		t.Fatalf("fixtures: %d purging devices, %v", len(l), err)
	}
	f, err := ev.Fleet(ctx, store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	b := purgeBanner(f)
	if b == nil || b.Subject != l[0].ID || b.Count != 1 || !strings.Contains(b.Text, "storage_unreachable") {
		t.Fatalf("banner = %+v, want the fixture device %s with its class", b, l[0].ID)
	}
	if strings.Contains(b.Text, l[0].Hostname) {
		t.Fatalf("the banner names the host: %s", b.Text)
	}
	// A scope that does not include the device's customer does not see it.
	cs, _ := st.ListCustomers(ctx, store.ScopeAll())
	var other string
	for _, c := range cs {
		if c.ID != l[0].CustomerID {
			other = c.ID
			break
		}
	}
	fs, err := ev.Fleet(ctx, store.ScopeOf(other))
	if err != nil {
		t.Fatal(err)
	}
	if b := purgeBanner(fs); b != nil {
		t.Fatalf("a scope without the device's customer sees its purge banner: %+v", b)
	}

	off, _ := adr19Evaluator(t, false)
	f, err = off.Fleet(ctx, store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	if b := purgeBanner(f); b != nil {
		t.Fatalf("PurgeBanner false raised %+v", b)
	}
}

// Fail loudly: a purge list the evaluator cannot read is the banner, never an empty list.
func TestAdr19UnreadablePurgeListIsTheBanner(t *testing.T) {
	ctx := context.Background()
	ev, _ := adr19Evaluator(t, true)
	f := ev.purgeFacts(ctx, store.Scope{}, nil, nil) // the zero Scope is refused by the store: an unreadable list
	if !f.NotRead {
		t.Fatal("a refused read was taken as an empty list")
	}
	bs := PurgeBanners(f, time.Now())
	if len(bs) != 1 || !strings.Contains(bs[0].Text, "could not be read") || bs[0].Severity != state.SeverityCritical {
		t.Fatalf("banners = %+v", bs)
	}
}

func TestAdr19JobFailureRowRaisesTheBanner(t *testing.T) {
	ctx := context.Background()
	ev, st := adr19Evaluator(t, true)
	now := ev.Clock()
	if err := st.RecordJobRun(ctx, PurgeJob, store.JobRun{Status: "error", ErrorClass: "not_wired", StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	f, err := ev.Fleet(ctx, store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	if b := purgeBanner(f); b == nil || !strings.Contains(b.Text, "last run failed (not_wired)") {
		t.Fatalf("banner = %+v", b)
	}
}
