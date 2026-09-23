package guardrails

import (
	"testing"
	"time"
)

func paris(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("no zone database")
	}
	return loc
}

func at(loc *time.Location, y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, loc)
}

func TestParseWindowsValidates(t *testing.T) {
	ws, err := ParseWindows(`[{"days":[1,2,3,4,5],"from":"20:00","to":"07:00"}]`)
	if err != nil || len(ws) != 1 {
		t.Fatalf("valid: %v %v", ws, err)
	}
	for _, bad := range []string{
		`[{"days":[0],"from":"20:00","to":"07:00"}]`,
		`[{"days":[1],"from":"25:00","to":"07:00"}]`,
		`[{"days":[1],"from":"20:00","to":"7"}]`,
		`[{"days":[],"from":"20:00","to":"07:00"}]`,
		`{"days":[1]}`,
		`[{"days":[1],"from":"2:305","to":"07:00"}]`, // Sscanf would read this as "02:30" and drop the trailing "5"
		`[{"days":[1],"from":"12:3a","to":"07:00"}]`, // Sscanf would read this as "12:03" and drop the "a"
	} {
		if _, err := ParseWindows(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if ws, err := ParseWindows(""); err != nil || len(ws) != 0 {
		t.Fatalf("empty: %v %v", ws, err)
	}
	// EncodeWindows is the inverse of ParseWindows: what a person wrote reads
	// back as written.
	roundTripped, err := ParseWindows(EncodeWindows(ws))
	if err != nil || len(roundTripped) != 1 {
		t.Fatalf("round trip: %v %v", roundTripped, err)
	}
	if got := roundTripped[0]; len(got.Days) != 5 || got.From != "20:00" || got.To != "07:00" {
		t.Fatalf("round trip changed the window: %+v", got)
	}
}

func TestOffAcrossMidnightAndWeekend(t *testing.T) {
	loc := paris(t)
	ws := EveningsAndWeekends
	// Wednesday 2026-09-23.
	if Off(ws, loc, at(loc, 2026, 9, 23, 12, 0)) {
		t.Fatal("Wednesday noon is on")
	}
	if !Off(ws, loc, at(loc, 2026, 9, 23, 21, 0)) {
		t.Fatal("Wednesday 21:00 is off")
	}
	if !Off(ws, loc, at(loc, 2026, 9, 24, 6, 30)) {
		t.Fatal("Thursday 06:30 is still off (window crossed midnight)")
	}
	if Off(ws, loc, at(loc, 2026, 9, 24, 7, 0)) {
		t.Fatal("Thursday 07:00 is on")
	}
	if !Off(ws, loc, at(loc, 2026, 9, 26, 12, 0)) || !Off(ws, loc, at(loc, 2026, 9, 27, 23, 59)) {
		t.Fatal("weekend is off")
	}
	// Sunday's window is 00:00–24:00 and ends exactly at Monday 00:00: no
	// weekday window starts on Sunday, so Monday is on right away.
	if Off(ws, loc, at(loc, 2026, 9, 28, 0, 30)) {
		t.Fatal("Monday 00:30 is on: no weekday window started on Sunday")
	}
	// Given in UTC, evaluated in Paris: 19:00 UTC on a Wednesday in September = 21:00 Paris → off.
	if !Off(ws, loc, time.Date(2026, 9, 23, 19, 0, 0, 0, time.UTC)) {
		t.Fatal("zone must be applied")
	}
}

func TestLastBoundary(t *testing.T) {
	loc := paris(t)
	ws := EveningsAndWeekends
	b, off, ok := LastBoundary(ws, loc, at(loc, 2026, 9, 23, 21, 30))
	if !ok || !off || !b.Equal(at(loc, 2026, 9, 23, 20, 0)) {
		t.Fatalf("Wed 21:30 → last boundary Wed 20:00 (off): %v %v %v", b, off, ok)
	}
	b, off, _ = LastBoundary(ws, loc, at(loc, 2026, 9, 24, 8, 0))
	if off || !b.Equal(at(loc, 2026, 9, 24, 7, 0)) {
		t.Fatalf("Thu 08:00 → Thu 07:00 (on): %v %v", b, off)
	}
	b, off, _ = LastBoundary(ws, loc, at(loc, 2026, 9, 28, 3, 0))
	if off || !b.Equal(at(loc, 2026, 9, 28, 0, 0)) {
		t.Fatalf("Mon 03:00 → Mon 00:00 (on, weekend ended): %v %v", b, off)
	}
	if _, _, ok := LastBoundary(nil, loc, at(loc, 2026, 9, 28, 3, 0)); ok {
		t.Fatal("no window, no boundary")
	}
}

func TestDaylightSaving(t *testing.T) {
	loc := paris(t)
	// Spring 2026: clocks jump from 02:00 to 03:00 on Sunday 2026-03-29.
	ws := []Window{{Days: []int{7}, From: "02:30", To: "04:00"}}
	b, off, ok := LastBoundary(ws, loc, at(loc, 2026, 3, 29, 3, 15))
	if !ok || !off || b.Hour() != 3 || b.Minute() != 0 {
		t.Fatalf("a boundary in the skipped hour moves to 03:00: %v %v", b, off)
	}
	// Autumn 2026: 03:00 falls back to 02:00 on Sunday 2026-10-25; 02:30 happens twice.
	ws = []Window{{Days: []int{7}, From: "02:30", To: "05:00"}}
	first := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC).In(loc)  // 02:30 CEST, first occurrence
	second := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC).In(loc) // 02:30 CET, second occurrence
	b1, _, _ := LastBoundary(ws, loc, first.Add(5*time.Minute))
	b2, _, _ := LastBoundary(ws, loc, second.Add(5*time.Minute))
	if !b1.Equal(b2) {
		t.Fatalf("the repeated hour must yield one boundary, got %v and %v", b1, b2)
	}
}

// TestDaylightSavingHalfHourZone guards against generalizing the Paris fix
// by assuming every fall-back is a whole hour: Lord Howe Island winds its
// clocks back 30 minutes, so 01:30-02:00 repeats there each April.
func TestDaylightSavingHalfHourZone(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Lord_Howe")
	if err != nil {
		t.Skip("no zone database")
	}
	// Autumn 2026-04-05: clocks fall back 30 minutes at local 02:00 (+11:00)
	// to 01:30 (+10:30); 01:45 happens twice.
	ws := []Window{{Days: []int{7}, From: "01:45", To: "03:00"}}
	lordHoweFirst := time.Date(2026, 4, 4, 14, 45, 0, 0, time.UTC).In(loc)  // 01:45 +11, first occurrence
	lordHoweSecond := time.Date(2026, 4, 4, 15, 15, 0, 0, time.UTC).In(loc) // 01:45 +10:30, second occurrence
	b1, _, _ := LastBoundary(ws, loc, lordHoweFirst.Add(2*time.Minute))
	b2, _, _ := LastBoundary(ws, loc, lordHoweSecond.Add(2*time.Minute))
	if !b1.Equal(b2) {
		t.Fatalf("the repeated half-hour must yield one boundary, got %v and %v", b1, b2)
	}
	if !b1.Equal(lordHoweFirst) {
		t.Fatalf("the boundary must be the first (pre-transition) occurrence: got %v, want %v", b1, lordHoweFirst)
	}
}
