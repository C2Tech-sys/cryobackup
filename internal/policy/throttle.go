package policy

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Rate is a transfer limit in KiB/s, the only unit restic's --limit-upload and
// --limit-download take. 0 means unlimited.
type Rate int

// maxRate caps a parsed rate at 1 TiB/s; anything larger is a typo.
const maxRate = 1 << 30

// ParseRate parses one rate:
//
//	"0" | "unlimited"   unlimited (0)
//	"<n>" | "<n>k"      n KiB/s
//	"<n>m"              n MiB/s (n*1024 KiB/s)
//
// Case-insensitive. Percent rates ("50%") are not supported - restic cannot
// express them - and return an error that says so; they are never silently
// dropped (architecture §3.6; engine §1.6, recon sweep §5.5 E).
func ParseRate(s string) (Rate, error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "":
		return 0, errors.New("empty rate")
	case strings.Contains(s, "%"):
		return 0, errors.New("percent rates are not supported (restic takes KiB/s only): " + strconv.Quote(orig))
	case s == "unlimited":
		return 0, nil
	}
	mult := 1
	switch s[len(s)-1] {
	case 'k':
		s = s[:len(s)-1]
	case 'm':
		s, mult = s[:len(s)-1], 1024
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return 0, errors.New("invalid rate " + strconv.Quote(orig) + ": want unlimited, <n>, <n>k or <n>m")
		}
	}
	if s == "" {
		return 0, errors.New("invalid rate " + strconv.Quote(orig) + ": missing number")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > maxRate/mult {
		return 0, errors.New("rate " + strconv.Quote(orig) + " is too large")
	}
	return Rate(n * mult), nil
}

// ParseThrottleSpec parses the compact throttle form of the recon notes
// (sweep §5.5 E):
//
//	spec := rate (';' rate '@' window)*
//
// "60;10@Mon-Fri/8-18;5@Sat,Sun/0-24" is 60 KiB/s by default, 10 KiB/s on
// weekdays 08:00-18:00 and 5 KiB/s on weekends. The rate applies to both
// directions. Each window is a single window item (days/hours); it may not
// contain ';'. On overlap the later window wins (see Throttle.Effective).
// Errors are *ParseError with offsets into spec.
func ParseThrottleSpec(s string) (Throttle, error) {
	var t Throttle
	off := 0
	for i, seg := range strings.Split(s, ";") {
		lead := len(seg) - len(strings.TrimLeft(seg, " \t"))
		body := strings.TrimSpace(seg)
		start := off + lead
		off += len(seg) + 1
		if i == 0 {
			r, err := ParseRate(body)
			if err != nil {
				return Throttle{}, &ParseError{Offset: start, Msg: err.Error()}
			}
			t.Upload, t.Download = r, r
			continue
		}
		at := strings.IndexByte(body, '@')
		if at < 0 {
			return Throttle{}, &ParseError{Offset: start, Msg: "expected <rate>@<window>"}
		}
		r, err := ParseRate(body[:at])
		if err != nil {
			return Throttle{}, &ParseError{Offset: start, Msg: err.Error()}
		}
		rest := body[at+1:]
		wstart := start + at + 1 + (len(rest) - len(strings.TrimLeft(rest, " \t")))
		when := strings.TrimSpace(rest)
		if _, err := ParseWindow(when); err != nil {
			var pe *ParseError
			if errors.As(err, &pe) {
				return Throttle{}, &ParseError{Offset: wstart + pe.Offset, Msg: pe.Msg}
			}
			return Throttle{}, err
		}
		t.Windows = append(t.Windows, ThrottleWindow{When: when, Up: r, Down: r})
	}
	return t, nil
}

// parsedWindows returns the parsed window of each ThrottleWindow; entries that
// do not parse (Validate rejects them) are zero Windows, which are never open,
// so a corrupt window can never throttle a backup.
func (t Throttle) parsedWindows() []Window {
	ws := make([]Window, len(t.Windows))
	for i, tw := range t.Windows {
		if w, err := ParseWindow(tw.When); err == nil {
			ws[i] = w
		}
	}
	return ws
}

func (t Throttle) effective(ws []Window, now time.Time) (up, down Rate) {
	up, down = t.Upload, t.Download
	for i, w := range ws {
		if w.Contains(now) {
			up, down = t.Windows[i].Up, t.Windows[i].Down // later windows win
		}
	}
	return up, down
}

// Effective returns the limits in force at now (evaluated on now's wall
// clock, so pass device-local time). The default limits apply unless a window
// is open; when several windows are open the later one in Windows wins, so a
// narrow window listed after a broad one is the "most specific". 0 = unlimited.
func (t Throttle) Effective(now time.Time) (up, down Rate) {
	return t.effective(t.parsedWindows(), now)
}

// NextBoundary returns the next instant after from at which the effective
// limits change, on a minute boundary, searching 8 days ahead. The agent uses
// it to restart the restic child at the boundary, because restic fixes its
// limits for the life of the process (engine §1.6; architecture §6.3). It
// returns (zero, false) when there are no windows or the limits never change.
func (t Throttle) NextBoundary(from time.Time) (time.Time, bool) {
	if len(t.Windows) == 0 {
		return time.Time{}, false
	}
	ws := t.parsedWindows()
	up0, down0 := t.effective(ws, from)
	tm := floorMinute(from)
	for i := 0; i < 8*minutesPerDay; i++ {
		tm = tm.Add(time.Minute)
		if up, down := t.effective(ws, tm); up != up0 || down != down0 {
			return tm, true
		}
	}
	return time.Time{}, false
}

// GatewayDownGrace returns throttle.gateway_down_grace_minutes as a duration:
// 0 (a policy stored before the field existed) is the default of
// GatewayDownGraceDefaultMinutes. Validate keeps any other value inside
// GatewayDownGraceMinMinutes..GatewayDownGraceMaxMinutes; a value outside it
// that reaches here anyway is clamped, never taken as "no watchdog".
func (t Throttle) GatewayDownGrace() time.Duration {
	m := t.GatewayDownGraceMinutes
	switch {
	case m == 0:
		m = GatewayDownGraceDefaultMinutes
	case m < GatewayDownGraceMinMinutes:
		m = GatewayDownGraceMinMinutes
	case m > GatewayDownGraceMaxMinutes:
		m = GatewayDownGraceMaxMinutes
	}
	return time.Duration(m) * time.Minute
}
