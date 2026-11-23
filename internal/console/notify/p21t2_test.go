package notify

import (
	"regexp"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P21-T2 fix batch 1 R1: the colour of a PAUSED chip depends on the section its line sits in. In the
// Parked section it is accent, as in the console; under Warnings it is the warning colour and under
// Critical the danger colour, because the paused device carries an alarm-level flag and the section's
// colour is what the operator reads first. The two non-parked cases are the states
// internal/state/classify_test.go B13 shows reachable: a paused device keeps its escrow alarm (alarm
// level) and its identity-conflict or failed-maintenance alarm (critical level). The section's other
// chips are unchanged.
func TestP21T2_PausedChipColourFollowsItsSection(t *testing.T) {
	chipOf := regexp.MustCompile(`<td bgcolor="(#[0-9A-F]{6})"[^>]*color:(#[0-9A-F]{6});">PAUSED</td>`)
	cases := []struct {
		name          string
		alarm         state.Severity
		flags         []state.State
		section, next string // the label of the section the line must sit in, and the label that ends it
		soft, ink     string
	}{
		{"parked", state.SeverityNone, nil, "Parked and long-deferred devices", "", tokens["accent-soft"], tokens["accent"]},
		{"warnings", state.SeverityAlarm, []state.State{state.StateEscrowUnverified}, "Warnings", "Parked and long-deferred devices", tokens["warning-soft"], tokens["warning"]},
		{"critical", state.SeverityCritical, []state.State{state.StateIdentityConflict, state.StateMaintFailed}, "Critical", "Warnings", tokens["danger-soft"], tokens["danger"]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, acks := mixedView()
			p := &v.Devices[5] // SPARE-LAPTOP, PAUSED
			p.Alarm, p.AlarmFlags = c.alarm, c.flags
			out, err := RenderDigest(v, acks, Options{})
			if err != nil {
				t.Fatal(err)
			}
			from := strings.Index(out.HTML, `;">`+c.section+`</td>`)
			if from < 0 {
				t.Fatalf("no %q section label", c.section)
			}
			to := len(out.HTML)
			if c.next != "" {
				if to = strings.Index(out.HTML, `;">`+c.next+`</td>`); to < from {
					t.Fatalf("section label %q is not after %q (%d, %d)", c.next, c.section, to, from)
				}
			}
			section := out.HTML[from:to]
			if !strings.Contains(section, "SPARE-LAPTOP") {
				t.Fatalf("the paused device is not listed under %q: the case is not exercised", c.section)
			}
			m := chipOf.FindStringSubmatch(section)
			if m == nil {
				t.Fatalf("no PAUSED chip in the %q section", c.section)
			}
			if m[1] != c.soft || m[2] != c.ink {
				t.Errorf("the PAUSED chip under %q is %s on %s, want %s on %s", c.section, m[2], m[1], c.ink, c.soft)
			}
		})
	}
}
