package buildinfo

import (
	"strings"
	"testing"
)

func TestStringIncludesEveryField(t *testing.T) {
	got := String("cryo-console")
	for _, want := range []string{"cryo-console", Version, Commit, Date} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() = %q, missing %q", got, want)
		}
	}
}
