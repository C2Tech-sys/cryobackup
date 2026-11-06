package fixtures

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-T5 (C): the fixtures banners cite their times on a 12-hour clock in the display zone
// ("Last OK 1:33 PM"), never as 24-hour UTC ("Last OK 18:33", "due by 07:30").
var clock24 = regexp.MustCompile(`\b\d{1,2}:\d{2}\b( ?[AP]M)?`)

func texts(b []state.Banner) []string {
	out := make([]string, len(b))
	for i, x := range b {
		out[i] = x.Text
	}
	return out
}

func TestM3T5C_FixturesBannerTimesAreTwelveHour(t *testing.T) {
	for _, set := range [][]string{texts(Banners(Now())), texts(AllBanners(Now()))} {
		for _, text := range set {
			for _, m := range clock24.FindAllStringSubmatch(text, -1) {
				if m[1] == "" {
					t.Errorf("a 24-hour time in %q: %s", text, m[0])
				}
			}
		}
	}
	all := strings.Join(texts(Banners(Now())), "\n")
	if !strings.Contains(all, "Last OK 1:33 PM.") {
		t.Errorf("C01 should read Last OK 1:33 PM (America/Chicago): %s", all)
	}
	if !strings.Contains(all, "It was due by 7:30 AM.") {
		t.Errorf("C14: %s", all)
	}
}

func TestM3T5C_FixturesBannerTimesFollowTheZoneGiven(t *testing.T) {
	utc := strings.Join(texts(BannersIn(Now(), time.UTC)), "\n")
	if !strings.Contains(utc, "Last OK 6:33 PM.") {
		t.Errorf("in UTC the same instant is 6:33 PM: %s", utc)
	}
	// a time from another day names the day, so a bare clock time never reads as today
	if !strings.Contains(utc, "Last send Mon 19 Oct 4:42 PM.") {
		t.Errorf("the C03 time is yesterday and must say so: %s", utc)
	}
	chi := strings.Join(texts(Banners(Now())), "\n")
	if !strings.Contains(chi, "Last send Mon 19 Oct 11:42 AM.") {
		t.Errorf("C03 in America/Chicago: %s", chi)
	}
}
