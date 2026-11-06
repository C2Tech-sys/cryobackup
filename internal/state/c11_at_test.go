package state

import (
	"strings"
	"testing"
	"time"
)

// C11 names the time the worker's database failures began, so it carries that time
// in At and a page can re-render it in the operator's zone.
func TestC17_db_worker_store_since_is_in_At(t *testing.T) {
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	d := &DatabaseFacts{WorkerStoreFailures: 2, WorkerStoreFailingSince: ms(since), SelfBackup: &SelfBackupFacts{NotInBuild: true},
		IntegrityCheckedAt: ms(now), IntegrityOK: true, MeasuredAt: ms(now), FreeBytes: 900, TotalBytes: 1000}
	got := databaseBanners(d, bannerCtx{now: now, loc: time.UTC})
	if len(got) != 1 || got[0].ID != "C11" || !got[0].At.Equal(since) {
		t.Fatalf("banners %+v, want C11 with At = %v", got, since)
	}
	if !strings.Contains(got[0].Text, "(since ") {
		t.Fatalf("text %q names no time", got[0].Text)
	}
}
