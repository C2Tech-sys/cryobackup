package notify

import (
	"regexp"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P21-T2 D: a PAUSED chip is accent (the console's colour for a pause), never the warning colour,
// also when the paused device carries an alarm-level flag and its line sits in the Warnings
// section; the section's other chips keep the warning colour, and a paused line in the parked
// section is accent as before (TestDigestPausedChipIsAccent).
func TestP21T2_D_PausedChipIsAccentInTheWarningsSection(t *testing.T) {
	v, acks := mixedView()
	p := &v.Devices[5] // SPARE-LAPTOP, PAUSED
	p.Alarm, p.AlarmFlags = state.SeverityAlarm, []state.State{state.StateEscrowUnverified}
	c, err := RenderDigest(v, acks, Options{})
	if err != nil {
		t.Fatal(err)
	}
	warn := strings.Index(c.HTML, `;">Warnings</td>`)
	parked := strings.Index(c.HTML, `;">Parked and long-deferred devices</td>`)
	if warn < 0 || parked < warn {
		t.Fatalf("the Warnings and parked section labels are not where expected (%d, %d)", warn, parked)
	}
	section := c.HTML[warn:parked]
	if !strings.Contains(section, "SPARE-LAPTOP") {
		t.Fatal("the paused device with an alarm-level flag is not listed under Warnings: the case is not exercised")
	}
	chip := regexp.MustCompile(`<td bgcolor="(#[0-9A-F]{6})"[^>]*color:(#[0-9A-F]{6});">PAUSED</td>`).FindStringSubmatch(section)
	if chip == nil {
		t.Fatal("no PAUSED chip in the Warnings section")
	}
	if chip[1] != tokens["accent-soft"] || chip[2] != tokens["accent"] {
		t.Errorf("the PAUSED chip under Warnings is %s on %s, not accent (%s on %s)", chip[2], chip[1], tokens["accent"], tokens["accent-soft"])
	}
	if chip[2] == tokens["warning"] {
		t.Error("the PAUSED chip under Warnings is in the warning colour")
	}
	if n := regexp.MustCompile(`color:(#[0-9A-F]{6});">PARTIAL</td>`).FindStringSubmatch(section); n == nil || n[1] != tokens["warning"] {
		t.Errorf("the other chips of the Warnings section are no longer the warning colour: %v", n)
	}
}
