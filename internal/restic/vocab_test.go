package restic

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// schemaList extracts the quoted values of "<column> IN ('a','b',...)" from
// replica/schema.sql. pick selects among several matches (kind appears in more
// than one table).
func schemaList(t *testing.T, column string, pick func([]string) bool) []string {
	t.Helper()
	raw, err := os.ReadFile("../../replica/schema.sql")
	if err != nil {
		t.Fatalf("cannot read replica/schema.sql: %v", err)
	}
	re := regexp.MustCompile(`CHECK \(` + column + ` IN \(([^)]*)\)\)`)
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		var vals []string
		for _, v := range strings.Split(m[1], ",") {
			vals = append(vals, strings.Trim(strings.TrimSpace(v), "'"))
		}
		if pick == nil || pick(vals) {
			return vals
		}
	}
	t.Fatalf("schema.sql has no CHECK (%s IN (...)) that matches", column)
	return nil
}

func sorted[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func TestVocabularyMatchesSchema(t *testing.T) {
	runsKind := func(v []string) bool { return strings.Contains(strings.Join(v, ","), "maintenance") }
	tests := []struct {
		name   string
		column string
		pick   func([]string) bool
		ours   []string
	}{
		{"outcome", "outcome", nil, sorted([]Outcome{OutcomeSuccess, OutcomePartial, OutcomeFailed, OutcomeCancelled, OutcomeSkipped, OutcomeDeferred, OutcomeInterrupted, OutcomeAbandoned})},
		{"deferred_reason", "deferred_reason", nil, sorted([]DeferredReason{DeferredLock, DeferredGateway, DeferredBattery, DeferredMetered, DeferredWindow, DeferredConditions, DeferredFleetCap})},
		{"error_class", "error_class", nil, sorted(AllClasses())},
		{"kind", "kind", runsKind, sorted([]Kind{KindBackup, KindRestore, KindForget, KindPrune, KindCheck, KindUnlock, KindMaintenance})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := schemaList(t, tt.column, tt.pick)
			if !equalSets(append([]string(nil), tt.ours...), want) {
				t.Fatalf("package has %v, schema.sql has %v", tt.ours, want)
			}
		})
	}
}

func TestRecordedKinds(t *testing.T) {
	recorded := []Kind{KindBackup, KindRestore, KindForget, KindPrune, KindCheck, KindUnlock, KindMaintenance}
	readOnly := []Kind{KindSnapshots, KindLs, KindFind, KindDump, KindStats, KindCatConfig, KindVersion, KindInit, Kind("bogus")}
	for _, k := range recorded {
		if !k.Recorded() {
			t.Errorf("%s should be recorded", k)
		}
	}
	for _, k := range readOnly {
		if k.Recorded() {
			t.Errorf("%s should not be recorded", k)
		}
	}
}

func TestErrorClassHardAndValid(t *testing.T) {
	hard := map[ErrorClass]bool{
		ClassRepoMissing: true, ClassWrongPassword: true, ClassPermission: true,
		ClassDisk: true, ClassParse: true, ClassInternal: true,
	}
	if got := len(AllClasses()); got != 18 {
		t.Fatalf("AllClasses has %d entries, want 18", got)
	}
	for _, c := range AllClasses() {
		if !c.Valid() {
			t.Errorf("%s should be valid", c)
		}
		if c.Hard() != hard[c] {
			t.Errorf("%s.Hard() = %v, want %v", c, c.Hard(), hard[c])
		}
	}
	for _, c := range []ErrorClass{"", "Network", "nope", "lock "} {
		if c.Valid() {
			t.Errorf("%q should not be valid", c)
		}
		if c.Hard() {
			t.Errorf("%q should not be hard", c)
		}
	}
}
