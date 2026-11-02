package policy

import (
	"strconv"
	"strings"
	"time"
)

// ParseError reports the first problem found in a mini-language string
// (window, rate, throttle spec). Offset is a byte offset into the string that
// was parsed; for ParseThrottleSpec it is an offset into the whole spec.
type ParseError struct {
	Offset int
	Msg    string
}

// Error renders "at <offset>: <message>".
func (e *ParseError) Error() string { return "at " + strconv.Itoa(e.Offset) + ": " + e.Msg }

const (
	minutesPerDay  = 1440
	minutesPerWeek = 7 * minutesPerDay
	windowWords    = (minutesPerWeek + 63) / 64
)

var dayNames = [7]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// Window is a weekly open/closed pattern at one-minute resolution, Monday
// first. It is an immutable comparable value: two Windows are equal (==) iff
// they open and close at the same minutes. The zero Window is never open and
// is not producible by ParseWindow.
//
// A Window carries no time zone. Contains and NextOpen read the wall clock of
// the time they are given, so the caller passes device-local time (architecture
// §6.1: windows are evaluated in device-local time and gate starts only).
type Window struct {
	bits [windowWords]uint64
}

func (w *Window) set(day, from, to int) {
	for m := from; m < to; m++ {
		i := day*minutesPerDay + m
		w.bits[i>>6] |= 1 << (uint(i) & 63)
	}
}

func (w Window) open(day, minute int) bool {
	i := day*minutesPerDay + minute
	return w.bits[i>>6]&(1<<(uint(i)&63)) != 0
}

// weekdayIndex maps time.Weekday (Sunday = 0) to Monday = 0.
func weekdayIndex(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// ParseWindow parses the backup-window mini-language (matrix r26; origin:
// docs/recon-notes/sweep_acronis_datto_axcient_veeam_urbackup.md §5.5 C).
//
//	window   := item (';' item)*
//	item     := days '/' hours (',' hours)*
//	days     := '*' | dayspec (',' dayspec)*
//	dayspec  := day | day '-' day         (inclusive; Fri-Mon wraps over Sunday)
//	day      := 1..7 (Mon=1) | Mon|Tue|Wed|Thu|Fri|Sat|Sun   (case-insensitive)
//	hours    := time '-' time
//	time     := H | H:MM                  (0 <= H <= 24, MM 00-59, 24 only as 24 or 24:00)
//
// An hours range whose end is less than or equal to its start wraps past
// midnight: 20-6 is 20:00 to 06:00 the next day, and Sun/20-6 spills into
// Monday. A range cannot start at 24:00. Whitespace around tokens is ignored.
// "*/0-24" and "1-7/0-24" are "always". On failure the error is a
// *ParseError whose Offset is the byte offset of the first problem.
func ParseWindow(s string) (Window, error) {
	p := &windowParser{s: s}
	var w Window
	for {
		if err := p.item(&w); err != nil {
			return Window{}, err
		}
		p.skipSpace()
		if p.pos >= len(p.s) {
			return w, nil
		}
		if p.s[p.pos] != ';' {
			return Window{}, p.fail(p.pos, "expected ';' or end of window, found "+strconv.QuoteRune(rune(p.s[p.pos])))
		}
		p.pos++
	}
}

type windowParser struct {
	s   string
	pos int
}

func (p *windowParser) fail(off int, msg string) *ParseError {
	return &ParseError{Offset: off, Msg: msg}
}

func (p *windowParser) skipSpace() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func (p *windowParser) peek() byte {
	if p.pos < len(p.s) {
		return p.s[p.pos]
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func (p *windowParser) item(w *Window) error {
	days, err := p.days()
	if err != nil {
		return err
	}
	p.skipSpace()
	if p.peek() != '/' {
		return p.fail(p.pos, "expected '/' after days")
	}
	p.pos++
	for {
		from, to, err := p.hours()
		if err != nil {
			return err
		}
		for _, d := range days {
			if to > from {
				w.set(d, from, to)
			} else { // wraps past midnight into the next day (and Sunday into Monday)
				w.set(d, from, minutesPerDay)
				w.set((d+1)%7, 0, to)
			}
		}
		p.skipSpace()
		if p.peek() != ',' {
			return nil
		}
		p.pos++
	}
}

func (p *windowParser) days() ([]int, error) {
	p.skipSpace()
	if p.peek() == '*' {
		p.pos++
		return []int{0, 1, 2, 3, 4, 5, 6}, nil
	}
	var out []int
	for {
		a, err := p.day()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.peek() == '-' {
			p.pos++
			b, err := p.day()
			if err != nil {
				return nil, err
			}
			for i := a; ; i = (i + 1) % 7 {
				out = append(out, i)
				if i == b {
					break
				}
			}
		} else {
			out = append(out, a)
		}
		p.skipSpace()
		if p.peek() != ',' {
			return out, nil
		}
		p.pos++
	}
}

// day parses one day token and returns its Monday = 0 index.
func (p *windowParser) day() (int, error) {
	p.skipSpace()
	start := p.pos
	switch c := p.peek(); {
	case isDigit(c):
		for isDigit(p.peek()) {
			p.pos++
		}
		tok := p.s[start:p.pos]
		n, _ := strconv.Atoi(tok)
		if len(tok) > 1 || n < 1 || n > 7 {
			return 0, p.fail(start, "day "+strconv.Quote(tok)+" out of range 1-7")
		}
		return n - 1, nil
	case isAlpha(c):
		for isAlpha(p.peek()) {
			p.pos++
		}
		tok := p.s[start:p.pos]
		for i, n := range dayNames {
			if strings.EqualFold(tok, n) {
				return i, nil
			}
		}
		return 0, p.fail(start, "unknown day "+strconv.Quote(tok)+" (use Mon-Sun or 1-7)")
	default:
		return 0, p.fail(p.pos, "expected day (1-7 or Mon-Sun)")
	}
}

// hours parses "time-time" and returns minutes since midnight.
func (p *windowParser) hours() (from, to int, err error) {
	p.skipSpace()
	start := p.pos
	from, err = p.clock()
	if err != nil {
		return 0, 0, err
	}
	if from == minutesPerDay {
		return 0, 0, p.fail(start, "an hour range cannot start at 24:00")
	}
	p.skipSpace()
	if p.peek() != '-' {
		return 0, 0, p.fail(p.pos, "expected '-' in hour range")
	}
	p.pos++
	to, err = p.clock()
	return from, to, err
}

// clock parses H or H:MM and returns minutes since midnight (24:00 = 1440).
func (p *windowParser) clock() (int, error) {
	p.skipSpace()
	start := p.pos
	for isDigit(p.peek()) {
		p.pos++
	}
	if p.pos == start {
		return 0, p.fail(start, "expected hour")
	}
	tok := p.s[start:p.pos]
	h, _ := strconv.Atoi(tok)
	if len(tok) > 2 || h > 24 {
		return 0, p.fail(start, "hour "+strconv.Quote(tok)+" out of range 0-24")
	}
	m := 0
	if p.peek() == ':' {
		p.pos++
		ms := p.pos
		for isDigit(p.peek()) {
			p.pos++
		}
		mt := p.s[ms:p.pos]
		if len(mt) != 2 {
			return 0, p.fail(ms, "expected two-digit minutes")
		}
		m, _ = strconv.Atoi(mt)
		if m > 59 {
			return 0, p.fail(ms, "minutes "+strconv.Quote(mt)+" out of range 00-59")
		}
	}
	if h == 24 && m != 0 {
		return 0, p.fail(start, "24 is only valid as 24 or 24:00")
	}
	return h*60 + m, nil
}

// Contains reports whether the window is open at t, evaluated on the wall
// clock of t.Location() (seconds are ignored). A 23- or 25-hour day on a DST
// change needs no special handling: the wall clock of the caller's zone
// already skips or repeats the hour.
func (w Window) Contains(t time.Time) bool {
	return w.open(weekdayIndex(t), t.Hour()*60+t.Minute())
}

// IsAlways reports whether the window is open at every minute of the week.
func (w Window) IsAlways() bool {
	for i := 0; i < minutesPerWeek; i++ {
		if w.bits[i>>6]&(1<<(uint(i)&63)) == 0 {
			return false
		}
	}
	return true
}

// Minutes returns the full week at minute resolution, Monday = 0.
func (w Window) Minutes() [7][1440]bool {
	var out [7][1440]bool
	for d := 0; d < 7; d++ {
		for m := 0; m < minutesPerDay; m++ {
			out[d][m] = w.open(d, m)
		}
	}
	return out
}

// Grid returns the week at hour resolution, Monday = 0. A cell is true iff
// every minute of that hour is open, so a partially open hour reads false.
func (w Window) Grid() [7][24]bool {
	var out [7][24]bool
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			all := true
			for m := 0; m < 60 && all; m++ {
				all = w.open(d, h*60+m)
			}
			out[d][h] = all
		}
	}
	return out
}

// NextOpen returns the earliest instant >= from at which the window is open,
// searching 8 days ahead, or (zero, false) if it never opens (including the
// zero Window). If the window is open at from, from itself is returned;
// otherwise the result lies on a minute boundary. The search advances one
// real minute at a time and reads the wall clock of from's location at each
// step, so a skipped or repeated DST hour is handled by the zone, never by
// 24-hour arithmetic.
func (w Window) NextOpen(from time.Time) (time.Time, bool) {
	if w.Contains(from) {
		return from, true
	}
	t := floorMinute(from)
	for i := 0; i < 8*minutesPerDay; i++ {
		t = t.Add(time.Minute)
		if w.Contains(t) {
			return t, true
		}
	}
	return time.Time{}, false
}

// floorMinute drops the seconds and sub-second part of t without leaving its
// instant's minute (and without any wall-clock re-resolution that could jump
// back across a DST repeat).
func floorMinute(t time.Time) time.Time {
	return t.Add(-time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond()))
}

// String returns the canonical form: days with identical hours are grouped
// ("Mon-Fri/8-18;Sat,Sun/0-24"), runs of three or more days are ranges, a
// window open all week is "*/0-24", and a range that crosses midnight is
// written as two ranges on the two days. ParseWindow(w.String()) == w. The
// zero Window renders as "".
func (w Window) String() string {
	var perDay [7]string
	for d := 0; d < 7; d++ {
		perDay[d] = w.dayHours(d)
	}
	var groups []string
	done := [7]bool{}
	for d := 0; d < 7; d++ {
		if done[d] || perDay[d] == "" {
			continue
		}
		var days []int
		for e := d; e < 7; e++ {
			if !done[e] && perDay[e] == perDay[d] {
				done[e] = true
				days = append(days, e)
			}
		}
		groups = append(groups, dayList(days)+"/"+perDay[d])
	}
	return strings.Join(groups, ";")
}

// dayHours renders the open intervals of one day, e.g. "8-12,13:30-18".
func (w Window) dayHours(d int) string {
	var parts []string
	m := 0
	for m < minutesPerDay {
		if !w.open(d, m) {
			m++
			continue
		}
		s := m
		for m < minutesPerDay && w.open(d, m) {
			m++
		}
		parts = append(parts, clockString(s)+"-"+clockString(m))
	}
	return strings.Join(parts, ",")
}

func clockString(min int) string {
	h, m := min/60, min%60
	if m == 0 {
		return strconv.Itoa(h)
	}
	return strconv.Itoa(h) + ":" + twoDigits(m)
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// dayList renders ascending day indexes: "*" for all seven, "Mon-Fri" for runs
// of three or more, otherwise individual names.
func dayList(days []int) string {
	if len(days) == 7 {
		return "*"
	}
	var parts []string
	for i := 0; i < len(days); {
		j := i
		for j+1 < len(days) && days[j+1] == days[j]+1 {
			j++
		}
		if j-i >= 2 {
			parts = append(parts, dayNames[days[i]]+"-"+dayNames[days[j]])
		} else {
			for k := i; k <= j; k++ {
				parts = append(parts, dayNames[days[k]])
			}
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
