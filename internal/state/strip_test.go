package state

import (
	"reflect"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zone database for the DST cases, independent of the host
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func hourAt(s string, ok, bad int) StripHour {
	h, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return StripHour{Hour: h, Success: ok, Failure: bad}
}

// cellOn returns the cell whose date is y-m-d in the strip's zone.
func cellOn(t *testing.T, cells [StripDays]StripCell, y int, m time.Month, d int) StripCell {
	t.Helper()
	for _, c := range cells {
		if cy, cm, cd := c.Date.Date(); cy == y && cm == m && cd == d {
			return c
		}
	}
	t.Fatalf("no cell for %d-%02d-%02d in %v", y, m, d, cells)
	return StripCell{}
}

func kinds(cells [StripDays]StripCell) []DayKind {
	out := make([]DayKind, len(cells))
	for i, c := range cells {
		out[i] = c.Kind
	}
	return out
}

func TestStripDST(t *testing.T) {
	chi := zone(t, "America/Chicago")
	t.Run("E01_strip_dst_spring_forward_23h_day", func(t *testing.T) {
		// 2026-03-08 has 23 hours in Chicago: [06:00Z, 05:00Z next day).
		f := StripFacts{DisplayZone: chi, EnrolledAt: time.Date(2026, 2, 1, 0, 0, 0, 0, chi), Hours: []StripHour{
			hourAt("2026-03-08T06:30:00Z", 1, 0), // 00:30 CST, the first hour of the day
			hourAt("2026-03-09T04:30:00Z", 0, 1), // 23:30 CDT, the last hour of the same day
			hourAt("2026-03-09T05:30:00Z", 1, 0), // 00:30 CDT the next day
		}}
		cells := TenDayStrip(f, time.Date(2026, 3, 12, 12, 0, 0, 0, chi))
		d8, d9 := cellOn(t, cells, 2026, 3, 8), cellOn(t, cells, 2026, 3, 9)
		if d8.Kind != DayMixed || d8.Success != 1 || d8.Failed != 1 {
			t.Errorf("Mar 8 = %+v, want one mixed bucket", d8)
		}
		if d9.Kind != DaySuccess || d9.Success != 1 {
			t.Errorf("Mar 9 = %+v", d9)
		}
		if got := d9.Date.Sub(d8.Date); got != 23*time.Hour {
			t.Errorf("Mar 8 spans %v, want 23h", got)
		}
	})
	t.Run("E02_strip_dst_fall_back_25h_day", func(t *testing.T) {
		// 2026-11-01 has 25 hours in Chicago: [05:00Z, 06:00Z next day).
		f := StripFacts{DisplayZone: chi, EnrolledAt: time.Date(2026, 10, 1, 0, 0, 0, 0, chi), Hours: []StripHour{
			hourAt("2026-11-01T05:30:00Z", 1, 0), // 00:30 CDT
			hourAt("2026-11-01T06:30:00Z", 1, 0), // 01:30 CDT
			hourAt("2026-11-01T07:30:00Z", 1, 0), // 01:30 CST, the repeated hour
			hourAt("2026-11-02T05:30:00Z", 1, 0), // 23:30 CST, still Nov 1
			hourAt("2026-11-02T06:30:00Z", 0, 1), // 00:30 CST Nov 2
		}}
		cells := TenDayStrip(f, time.Date(2026, 11, 4, 12, 0, 0, 0, chi))
		d1, d2 := cellOn(t, cells, 2026, 11, 1), cellOn(t, cells, 2026, 11, 2)
		if d1.Kind != DaySuccess || d1.Success != 4 || d1.Failed != 0 {
			t.Errorf("Nov 1 = %+v, want one bucket with 4 successes", d1)
		}
		if d2.Kind != DayFailed || d2.Failed != 1 {
			t.Errorf("Nov 2 = %+v", d2)
		}
		if got := d2.Date.Sub(d1.Date); got != 25*time.Hour {
			t.Errorf("Nov 1 spans %v, want 25h", got)
		}
	})
	t.Run("E01_E02_cells_are_consecutive_local_days_across_both_changes", func(t *testing.T) {
		for _, now := range []time.Time{time.Date(2026, 3, 12, 0, 30, 0, 0, chi), time.Date(2026, 11, 5, 23, 30, 0, 0, chi)} {
			cells := TenDayStrip(StripFacts{DisplayZone: chi}, now)
			for i := 1; i < StripDays; i++ {
				if h, m, s := cells[i].Date.Clock(); h != 0 || m != 0 || s != 0 || !cells[i].Date.Equal(cells[i-1].Date.AddDate(0, 0, 1)) {
					t.Fatalf("cell %d = %v after %v", i, cells[i].Date, cells[i-1].Date)
				}
			}
			if y, m, d := cells[StripDays-1].Date.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
				t.Errorf("last cell %v is not today %v", cells[StripDays-1].Date, now)
			}
		}
	})
}

func TestStripZone(t *testing.T) {
	chi, tyo := zone(t, "America/Chicago"), zone(t, "Asia/Tokyo")
	// 16:00Z on Oct 21 is 01:00 Oct 22 in Tokyo and 11:00 Oct 21 in Chicago.
	run := []StripHour{hourAt("2026-10-21T16:00:00Z", 1, 0)}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	enrolled := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	t.Run("E03_strip_uses_customer_tz_not_display_tz", func(t *testing.T) {
		cells := TenDayStrip(StripFacts{CustomerZone: tyo, DisplayZone: chi, EnrolledAt: enrolled, Hours: run}, now)
		if cells[StripDays-1].Date.Location() != tyo {
			t.Fatalf("strip drawn in %v, want the customer's zone", cells[StripDays-1].Date.Location())
		}
		if c := cellOn(t, cells, 2026, 10, 22); c.Kind != DaySuccess {
			t.Errorf("Oct 22 Tokyo = %+v, want the run there", c)
		}
		if c := cellOn(t, cells, 2026, 10, 21); c.Kind != DayMissed {
			t.Errorf("Oct 21 Tokyo = %+v, want missed", c)
		}
	})
	t.Run("E03_strip_falls_back_to_display_tz", func(t *testing.T) {
		cells := TenDayStrip(StripFacts{DisplayZone: chi, EnrolledAt: enrolled, Hours: run}, now)
		if c := cellOn(t, cells, 2026, 10, 21); c.Kind != DaySuccess {
			t.Errorf("Oct 21 Chicago = %+v, want the run there", c)
		}
		if c := cellOn(t, cells, 2026, 10, 22); c.Kind != DayNone {
			t.Errorf("Oct 22 Chicago (today, no run) = %+v, want none", c)
		}
	})
	t.Run("E03_strip_zone_order", func(t *testing.T) {
		if StripZone(tyo, chi) != tyo || StripZone(nil, chi) != chi || StripZone(nil, nil) != time.UTC {
			t.Error("StripZone: customer, then display, then UTC")
		}
	})
}

func TestStripCellClassification(t *testing.T) {
	utc := time.UTC
	day := func(d, h int) string { return time.Date(2026, 10, d, h, 0, 0, 0, utc).Format(time.RFC3339) }
	// t0 is 2026-10-22 12:00 UTC: the strip is Oct 13 .. Oct 22.
	hours := []StripHour{
		hourAt(day(13, 9), 1, 0), // before enrollment (Oct 14 10:00): none, even with a run
		hourAt(day(14, 20), 1, 0),
		hourAt(day(15, 2), 1, 0), hourAt(day(15, 22), 0, 1),
		hourAt(day(16, 3), 0, 2),
		// Oct 17: nothing, a run was expected: missed
		hourAt(day(18, 3), 2, 0),
		hourAt(day(19, 3), 1, 0),
		hourAt(day(20, 3), 1, 0), // paused from Oct 20 on: none even with a run
	}
	base := StripFacts{DisplayZone: utc, EnrolledAt: time.Date(2026, 10, 14, 10, 0, 0, 0, utc), Hours: hours}
	cases := []struct {
		name string
		mod  func(f *StripFacts)
		want []DayKind
	}{
		{"E04_strip_cell_classification", func(f *StripFacts) { f.PausedAt = time.Date(2026, 10, 20, 1, 0, 0, 0, utc) },
			[]DayKind{DayNone, DaySuccess, DayMixed, DayFailed, DayMissed, DaySuccess, DaySuccess, DayNone, DayNone, DayNone}},
		{"E04_strip_unpaused_shows_missed_days_and_today_none", nil,
			[]DayKind{DayNone, DaySuccess, DayMixed, DayFailed, DayMissed, DaySuccess, DaySuccess, DaySuccess, DayMissed, DayNone}},
		{"E04_strip_today_with_a_run_is_success", func(f *StripFacts) { f.Hours = append(f.Hours, hourAt(day(22, 8), 1, 0)) },
			[]DayKind{DayNone, DaySuccess, DayMixed, DayFailed, DayMissed, DaySuccess, DaySuccess, DaySuccess, DayMissed, DaySuccess}},
		{"E04_strip_pending_first_backup_days_are_none", func(f *StripFacts) { f.PendingFirst, f.Hours = true, nil },
			[]DayKind{DayNone, DayNone, DayNone, DayNone, DayNone, DayNone, DayNone, DayNone, DayNone, DayNone}},
		{"E04_strip_no_schedule_expects_no_run", func(f *StripFacts) { f.NoSchedule = true },
			[]DayKind{DayNone, DaySuccess, DayMixed, DayFailed, DayNone, DaySuccess, DaySuccess, DaySuccess, DayNone, DayNone}},
		{"E04_strip_paused_before_the_window_is_all_none", func(f *StripFacts) { f.PausedAt = time.Unix(0, 0) },
			[]DayKind{DayNone, DayNone, DayNone, DayNone, DayNone, DayNone, DayNone, DayNone, DayNone, DayNone}},
		{"E04_strip_hours_outside_the_window_are_ignored", func(f *StripFacts) {
			f.Hours = []StripHour{hourAt("2026-10-01T03:00:00Z", 5, 5), hourAt("2026-10-23T03:00:00Z", 5, 5)}
			f.EnrolledAt = time.Date(2026, 9, 1, 0, 0, 0, 0, utc)
		}, []DayKind{DayMissed, DayMissed, DayMissed, DayMissed, DayMissed, DayMissed, DayMissed, DayMissed, DayMissed, DayNone}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := base
			f.Hours = append([]StripHour(nil), base.Hours...)
			if c.mod != nil {
				c.mod(&f)
			}
			cells := TenDayStrip(f, t0)
			if got := kinds(cells); !reflect.DeepEqual(got, c.want) {
				t.Errorf("kinds = %v\n want %v", got, c.want)
			}
		})
	}
	t.Run("E04_strip_counts_add_up_per_day", func(t *testing.T) {
		cells := TenDayStrip(base, t0)
		if c := cellOn(t, cells, 2026, 10, 15); c.Success != 1 || c.Failed != 1 {
			t.Errorf("Oct 15 = %+v", c)
		}
		if c := cellOn(t, cells, 2026, 10, 16); c.Failed != 2 {
			t.Errorf("Oct 16 = %+v", c)
		}
	})
}

func TestDigestOncePerLocalDay(t *testing.T) {
	chi := zone(t, "America/Chicago")
	t.Run("E05_digest_once_per_local_day_across_dst", func(t *testing.T) {
		for _, c := range []struct {
			name      string
			localTime string
			triggers  []string // two instants on one local day
			wantKey   string
			wantDue   string
		}{
			{"spring_forward", "07:00", []string{"2026-03-08T13:00:00Z", "2026-03-09T04:59:00Z"}, "digest:2026-03-08", "2026-03-08T12:00:00Z"},
			{"fall_back", "07:00", []string{"2026-11-01T14:00:00Z", "2026-11-02T05:59:00Z"}, "digest:2026-11-01", "2026-11-01T13:00:00Z"},
			{"fall_back_repeated_hour", "01:30", []string{"2026-11-01T06:45:00Z", "2026-11-01T07:45:00Z"}, "digest:2026-11-01", "2026-11-01T06:30:00Z"},
			{"spring_forward_skipped_hour", "02:30", []string{"2026-03-08T08:45:00Z", "2026-03-08T20:00:00Z"}, "digest:2026-03-08", "2026-03-08T08:30:00Z"},
		} {
			t.Run(c.name, func(t *testing.T) {
				keys := map[string]bool{}
				for _, s := range c.triggers {
					now, _ := time.Parse(time.RFC3339, s)
					due, ok := DigestDue(c.localTime, chi, now)
					if !ok || due.Format(time.RFC3339) != c.wantDue {
						t.Fatalf("DigestDue at %s = %v ok=%v, want %s", s, due, ok, c.wantDue)
					}
					keys[DigestKey(due, chi)] = true
				}
				if len(keys) != 1 || !keys[c.wantKey] {
					t.Errorf("two triggers on one local day name %v, want only %s (one send)", keys, c.wantKey)
				}
			})
		}
	})
	t.Run("E05_digest_key_is_the_local_date", func(t *testing.T) {
		at := time.Date(2026, 3, 9, 4, 30, 0, 0, time.UTC) // 23:30 CDT on Mar 8
		if got := DigestKey(at, chi); got != "digest:2026-03-08" {
			t.Errorf("key = %s", got)
		}
		if got := DigestKey(at, nil); got != "digest:2026-03-09" {
			t.Errorf("nil zone key = %s, want the UTC date", got)
		}
		if _, ok := DigestDue("", chi, at); ok {
			t.Error("an unset digest time has no due time")
		}
	})
	t.Run("E05_c14_judged_once_per_local_day_on_a_dst_day", func(t *testing.T) {
		w := healthyFleet(1, 1)
		w.DisplayZone = chi
		sent := ms(time.Date(2026, 11, 1, 13, 1, 0, 0, time.UTC)) // 07:01 CST on the fall-back day
		w.Probes = ProbeFacts{Digest: &DigestFacts{LocalTime: "07:00", LastSentAt: sent}}
		for _, s := range []string{"2026-11-01T14:00:00Z", "2026-11-02T05:59:00Z", "2026-11-02T13:29:00Z"} {
			now, _ := time.Parse(time.RFC3339, s)
			if _, ok := findBanner(Evaluate(w, now), "C14"); ok {
				t.Errorf("C14 at %s after the day's digest was sent", s)
			}
		}
		now, _ := time.Parse(time.RFC3339, "2026-11-02T13:31:00Z") // Nov 2 07:31 CST, nothing sent that day
		b, ok := findBanner(Evaluate(w, now), "C14")
		if !ok || !strings.Contains(b.Text, "due by 7:30 AM") {
			t.Errorf("C14 = %+v ok=%v", b, ok)
		}
	})
}
