package restoreplace

import "testing"

// TestRelate pins the exact, byte-for-byte relation of a path to the
// selection: no case folding, no prefix match inside a name.
func TestRelate(t *testing.T) {
	sel := []Rel{{"Users", "x", "Documents"}, {"Data", "a.txt"}}
	for _, tc := range []struct {
		p        []string
		rel      relation
		selected bool
	}{
		{[]string{"Users"}, relAncestor, false},
		{[]string{"Users", "x"}, relAncestor, false},
		{[]string{"Users", "x", "Documents"}, relUnder, true},
		{[]string{"Users", "x", "Documents", "f.docx"}, relUnder, false},
		{[]string{"Users", "x", "documents"}, relNone, false}, // case is never folded
		{[]string{"Users", "x", "Documents2"}, relNone, false},
		{[]string{"Users", "y"}, relNone, false},
		{[]string{"Data"}, relAncestor, false},
		{[]string{"Data", "a.txt"}, relUnder, true},
		{[]string{"Data", "b.txt"}, relNone, false},
		{[]string{"Other"}, relNone, false},
	} {
		rel, selected := relate(tc.p, sel)
		if rel != tc.rel || selected != tc.selected {
			t.Errorf("relate(%q) = %v,%v want %v,%v", tc.p, rel, selected, tc.rel, tc.selected)
		}
	}
}

func TestCountsMergeAndClasses(t *testing.T) {
	var a, b Counts
	a.Restored, a.BytesRestored = 2, 10
	a.Add(ClassGuardRefused, 1)
	a.Add(ClassOther, 0) // nothing
	b.Skipped, b.BytesSkipped = 3, 7
	b.Add(ClassGuardRefused, 2)
	b.Add(ClassAccessDenied, 1)
	a.Merge(b)
	if a.Restored != 2 || a.Skipped != 3 || a.BytesRestored != 10 || a.BytesSkipped != 7 || a.ErrorTotal() != 4 {
		t.Fatalf("merged = %+v", a)
	}
	if got := a.Classes(); len(got) != 2 || got[0] != ClassAccessDenied || got[1] != ClassGuardRefused {
		t.Fatalf("classes = %v", got)
	}
}
