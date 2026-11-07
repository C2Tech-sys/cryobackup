package health

import (
	"context"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// m17: retired devices that cannot be read are not "nothing is retired": C17b, not silence.
func TestC17RetiredDevicesThatCannotBeReadAreNotChecked(t *testing.T) {
	e, _ := newEval(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := e.withRetired(ctx, state.LoginFileFacts{})
	if f.NotRead == "" {
		t.Fatalf("a failed read of the retired devices: %+v", f)
	}
	// The fixture fleet has one retired device, and a real read says so.
	f = e.withRetired(context.Background(), state.LoginFileFacts{})
	if f.NotRead != "" || f.Retired != 1 || f.RetiredGarbled != 0 {
		t.Fatalf("a good read: %+v", f)
	}
}

// The measurement reaches the engine through ProbeFacts, with the retired devices added.
func TestC17TheMeasurementReachesTheBanner(t *testing.T) {
	e, c := newEval(t)
	base := FixtureProbes(c.now)
	var measured state.LoginFileFacts
	e.Probes = func() state.ProbeFacts {
		p := base()
		m := measured
		p.LoginFile = &m
		return p
	}
	ctx := context.Background()
	ids := func() string {
		t.Helper()
		f, err := e.Fleet(ctx, store.ScopeAll())
		if err != nil {
			t.Fatal(err)
		}
		return ids(f.View.Banners)
	}
	// Never measured, with a retired device in the fleet: not checked, not nothing.
	if got := ids(); !strings.Contains(got, "C17b") {
		t.Fatalf("never measured: %q", got)
	}
	measured = *base().LoginFile
	if got := ids(); strings.Contains(got, "C17") {
		t.Fatalf("a clean read: %q", got)
	}
	measured.InFile = 1
	if got := ids(); !strings.Contains(got, "C17") || strings.Contains(got, "C17b") {
		t.Fatalf("a login in the file: %q", got)
	}
}
