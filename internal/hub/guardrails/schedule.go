package guardrails

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Window is one "off" window in the resource's local wall-clock schedule.
// On each listed day the resource is off from From; if To <= From the window
// ends at To on the next day. "24:00" is accepted as To only, meaning end of
// day.
type Window struct {
	Days []int  `json:"days"` // ISO: 1 = Monday … 7 = Sunday
	From string `json:"from"` // "HH:MM"
	To   string `json:"to"`   // "HH:MM"; To <= From means the window crosses midnight
}

// EveningsAndWeekends is the common default: off outside business hours on
// weekdays, off all weekend.
var EveningsAndWeekends = []Window{
	{Days: []int{1, 2, 3, 4, 5}, From: "20:00", To: "07:00"},
	{Days: []int{6, 7}, From: "00:00", To: "24:00"},
}

// ParseWindows validates and sorts the window list encoded in raw. "" or
// "[]" decode to an empty list. Merging windows is not needed for
// correctness — Off and LastBoundary work on absolute intervals — and a
// window list a person wrote should read back as written.
func ParseWindows(raw string) ([]Window, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var ws []Window
	if err := json.Unmarshal([]byte(raw), &ws); err != nil {
		return nil, fmt.Errorf("windows must be a JSON list: %w", err)
	}
	for i, w := range ws {
		if len(w.Days) == 0 {
			return nil, fmt.Errorf("window %d has no day", i+1)
		}
		for _, d := range w.Days {
			if d < 1 || d > 7 {
				return nil, fmt.Errorf("window %d: day %d is not between 1 (Monday) and 7 (Sunday)", i+1, d)
			}
		}
		if _, err := minutes(w.From, false); err != nil {
			return nil, fmt.Errorf("window %d from: %w", i+1, err)
		}
		if _, err := minutes(w.To, true); err != nil {
			return nil, fmt.Errorf("window %d to: %w", i+1, err)
		}
		sort.Ints(ws[i].Days)
	}
	return ws, nil
}

// EncodeWindows is the inverse of ParseWindows.
func EncodeWindows(ws []Window) string {
	if len(ws) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ws)
	return string(b)
}

// minutes parses "HH:MM" into minutes since midnight. "24:00" is allowed as
// an end only. Parsed by hand rather than with fmt.Sscanf: Sscanf silently
// ignores trailing unconsumed input and accepts a single digit where two are
// expected, so "2:305" or "12:3a" read back as valid two-digit times — a
// five-character string, but not the HH:MM one it claims to be.
func minutes(s string, end bool) (int, error) {
	if len(s) != 5 || s[2] != ':' || !isDigit(s[0]) || !isDigit(s[1]) || !isDigit(s[3]) || !isDigit(s[4]) {
		return 0, errors.New("expected HH:MM")
	}
	hh := int(s[0]-'0')*10 + int(s[1]-'0')
	mm := int(s[3]-'0')*10 + int(s[4]-'0')
	if mm > 59 || hh > 24 || (hh == 24 && (mm != 0 || !end)) {
		return 0, fmt.Errorf("%q is not a time of day", s)
	}
	return hh*60 + mm, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

type interval struct{ start, end time.Time }

// intervals lists every off interval that starts on a calendar day within
// [t-2d, t+1d] in loc. Two days back covers a window that started the day
// before and crosses midnight; the DST rules live in clock.
func intervals(ws []Window, loc *time.Location, t time.Time) []interval {
	var out []interval
	tl := t.In(loc)
	for dayOff := -2; dayOff <= 1; dayOff++ {
		day := time.Date(tl.Year(), tl.Month(), tl.Day()+dayOff, 0, 0, 0, 0, loc)
		iso := int(day.Weekday())
		if iso == 0 {
			iso = 7
		}
		for _, w := range ws {
			if !contains(w.Days, iso) {
				continue
			}
			from, _ := minutes(w.From, false)
			to, _ := minutes(w.To, true)
			start := clock(day, from)
			var end time.Time
			if to <= from {
				end = clock(time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc), to)
			} else {
				end = clock(day, to)
			}
			out = append(out, interval{start, end})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return merge(out)
}

// clock is a wall-clock minute of a given day, in that day's zone.
//
// Two daylight-saving cases, both handled against what time.Date actually
// does (checked against a scratch program against this build's tzdata, not
// assumed):
//
// Spring: the wall clock 02:30 does not exist. time.Date does not error on
// that — it normalises by shifting the whole missing hour forward by the gap
// width (02:00..02:59 all land on 03:00..03:59 CEST, a fixed +60m shift), not
// by walking the instant back across the transition. We detect the mismatch
// (the minute asked for is not the minute we got back) and snap forward to
// the start of the next hour in this zone, which is always the first instant
// of the new offset.
//
// Autumn: 02:30 happens twice. time.Date returns the *second* (post-
// transition, standard-time) occurrence here — not the first, despite that
// being the intuitive reading; a one-line check confirmed it. The spec wants
// the boundary to fire once, at the earlier of the two absolute instants, so
// that a check made during either repetition of the hour sees the same
// already-crossed boundary. We do not assume the shift is a whole hour — a
// fall-back can be 30 minutes (Australia/Lord_Howe) — so the shift width is
// read from the zone itself: the offset a few hours earlier (comfortably
// before any real-world DST transition, all of which are a few hours or
// less) compared with the offset at t. If it's larger, t is using the
// post-transition offset and the earlier occurrence is exactly that
// difference before it in absolute time; we keep it only if its wall clock
// still reads the same hh:mm, confirming it is genuinely the other
// repetition and not some unrelated transition further back.
func clock(day time.Time, mins int) time.Time {
	if mins >= 24*60 { // "24:00" is the end of the day, i.e. midnight next day
		return time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, day.Location())
	}
	hh, mm := mins/60, mins%60
	t := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, day.Location())
	if t.Hour()*60+t.Minute() != mins {
		return time.Date(day.Year(), day.Month(), day.Day(), hh+1, 0, 0, 0, day.Location())
	}
	_, offAtT := t.Zone()
	_, offBefore := t.Add(-6 * time.Hour).Zone()
	if delta := offBefore - offAtT; delta > 0 {
		if earlier := t.Add(-time.Duration(delta) * time.Second); earlier.Hour() == hh && earlier.Minute() == mm {
			return earlier
		}
	}
	return t
}

func merge(in []interval) []interval {
	var out []interval
	for _, iv := range in {
		if n := len(out); n > 0 && !iv.start.After(out[n-1].end) {
			if iv.end.After(out[n-1].end) {
				out[n-1].end = iv.end
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Off reports whether t (any zone) falls inside an off window, evaluated in
// loc.
func Off(ws []Window, loc *time.Location, t time.Time) bool {
	for _, iv := range intervals(ws, loc, t) {
		if !t.Before(iv.start) && t.Before(iv.end) {
			return true
		}
	}
	return false
}

// LastBoundary returns the most recent boundary at or before t, and whether
// the resource should be off after it. ok=false when there is no window.
func LastBoundary(ws []Window, loc *time.Location, t time.Time) (time.Time, bool, bool) {
	var best time.Time
	off, ok := false, false
	for _, iv := range intervals(ws, loc, t) {
		if !iv.start.After(t) && iv.start.After(best) {
			best, off, ok = iv.start, true, true
		}
		if !iv.end.After(t) && iv.end.After(best) {
			best, off, ok = iv.end, false, true
		}
	}
	return best, off, ok
}
