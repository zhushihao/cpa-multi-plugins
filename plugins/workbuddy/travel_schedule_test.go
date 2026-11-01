package main

// travel_schedule_test.go — G07 parity tests for the six-slot, independent
// cat-travel schedule. workbuddy2api schedules travel at travel_hours
// (deployed as [1,5,9,13,17,21] in Asia/Shanghai), decoupled from check-in.
// The plugin-side port adds a bounded catch-up window so a process restart
// does not silently lose a slot, and never runs more than one catch-up pass.

import (
	"testing"
	"time"
)

func wbTravelHoursMustEqual(t *testing.T, got []int, want ...int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("hours = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hours = %v, want %v", got, want)
		}
	}
}

// withAutoTravelDisabled isolates schedule tests that were written before the
// independent travel wake existed.
func withAutoTravelDisabled(t *testing.T) {
	t.Helper()
	auto, raw := wbTravelConfig()
	setWbTravelConfig(false, raw)
	t.Cleanup(func() { setWbTravelConfig(auto, raw) })
}

func TestTravelSlotsExactlySix(t *testing.T) {
	wbTravelHoursMustEqual(t, defaultWbTravelHours, 1, 5, 9, 13, 17, 21)
	for _, hours := range [][]int{defaultWbTravelHours, wbParseTravelHours("1,5,9,13,17,21")} {
		if len(hours) != 6 {
			t.Fatalf("want exactly 6 slots, got %v", hours)
		}
	}
	// Config parsing: whitespace, duplicates and out-of-range values are
	// normalized; an unusable value falls back to the default schedule.
	wbTravelHoursMustEqual(t, wbParseTravelHours(" 21 , 1 ,5,5,99,-1 ,x"), 1, 5, 21)
	wbTravelHoursMustEqual(t, wbParseTravelHours(""), defaultWbTravelHours...)
	wbTravelHoursMustEqual(t, wbParseTravelHours("24,99"), defaultWbTravelHours...)
	wbTravelHoursMustEqual(t, wbParseTravelHours("9"), 9)
}

// TestTravelSlotKeysAreCST pins the timezone discipline: slot identity is the
// CST (UTC+8) calendar day + hour, regardless of the host's local zone.
func TestTravelSlotKeysAreCST(t *testing.T) {
	// 2026-09-29 01:00 CST == 2026-09-28 17:00 UTC.
	slot := time.Date(2026, 9, 29, 1, 0, 0, 0, cstZone)
	utc := slot.UTC()
	if got, want := wbTravelSlotKey(utc, 1), "2026-09-29@1"; got != want {
		t.Fatalf("UTC-carried instant key = %q, want %q", got, want)
	}
	// The same wall-clock in a western zone is the NEXT CST day.
	ny, err := time.LoadLocation("America/New_York")
	if err == nil {
		west := time.Date(2026, 9, 28, 13, 0, 0, 0, ny) // 13:00 EDT == 17:00 UTC
		if got, want := wbTravelSlotKey(west, 1), "2026-09-29@1"; got != want {
			t.Fatalf("western-zone key = %q, want %q (CST day must win)", got, want)
		}
	}
}

// TestTravelSixIndependentTriggers walks one CST day and asserts exactly six
// due triggers at the six slot boundaries (and no trigger in between).
func TestTravelSixIndependentTriggers(t *testing.T) {
	hours := defaultWbTravelHours
	lastKey := ""
	triggers := 0
	base := time.Date(2026, 9, 29, 0, 30, 0, 0, cstZone) // 00:30 CST, before slot 1
	now := base
	for step := 0; step < 24*2; step++ { // 48 half-hour steps = one day
		if _, key, ok := wbDueTravelSlot(now, hours, lastKey); ok {
			triggers++
			lastKey = key
		}
		now = now.Add(30 * time.Minute)
		if now.Sub(base) > 24*time.Hour {
			break
		}
	}
	if triggers != 6 {
		t.Fatalf("one CST day triggered %d travel passes, want exactly 6", triggers)
	}
	// Same slot re-checked immediately is NOT due twice (independence is per
	// slot key, not per hour bucket reuse).
	slot := time.Date(2026, 9, 29, 13, 0, 0, 0, cstZone)
	_, key, ok := wbDueTravelSlot(slot, hours, "")
	if !ok {
		t.Fatal("13:00 slot must be due on a fresh day")
	}
	if _, _, again := wbDueTravelSlot(slot, hours, key); again {
		t.Fatal("same slot must not fire twice")
	}
}

// TestTravelNextSlotStrictlyAfter: the next wake is strictly after now, so the
// scheduler never busy-loops on the slot it just handled.
func TestTravelNextSlotStrictlyAfter(t *testing.T) {
	hours := defaultWbTravelHours
	atSlot := time.Date(2026, 9, 29, 1, 0, 0, 0, cstZone)
	next := wbNextTravelSlot(atSlot, hours)
	want := time.Date(2026, 9, 29, 5, 0, 0, 0, cstZone)
	if !next.Equal(want) {
		t.Fatalf("next after 01:00 = %s, want %s", next, want)
	}
	// After the last slot the next wake is tomorrow's first slot.
	last := time.Date(2026, 9, 29, 21, 0, 0, 0, cstZone)
	next2 := wbNextTravelSlot(last, hours)
	want2 := time.Date(2026, 9, 30, 1, 0, 0, 0, cstZone)
	if !next2.Equal(want2) {
		t.Fatalf("next after 21:00 = %s, want %s", next2, want2)
	}
}

// TestTravelCatchupBounded: a restart shortly after a slot catches that slot
// once; a long-missed slot is skipped (the next scheduled slot handles it)
// so a start at noon never replays the whole morning.
func TestTravelCatchupBounded(t *testing.T) {
	hours := defaultWbTravelHours
	// Restart at 01:20 with nothing run today → catch the 01:00 slot.
	now := time.Date(2026, 9, 29, 1, 20, 0, 0, cstZone)
	if _, key, ok := wbDueTravelSlot(now, hours, ""); !ok || key != "2026-09-29@1" {
		t.Fatalf("restart at 01:20 must catch 01:00, got (%q,%v)", key, ok)
	}
	// Restart at 12:00 with the last run at 05:00 (09:00 missed by 3h > 2h
	// window) → skip; the 13:00 slot handles it.
	now2 := time.Date(2026, 9, 29, 12, 0, 0, 0, cstZone)
	if _, key, ok := wbDueTravelSlot(now2, hours, "2026-09-29@5"); ok {
		t.Fatalf("09:00 slot missed by 3h must not be replayed, got %q", key)
	}
	// Exactly at the catch-up window edge (11:00, 09:00 last run at 05:00) it
	// still fires.
	now3 := time.Date(2026, 9, 29, 11, 0, 0, 0, cstZone) // 2h after 09:00
	if _, key, ok := wbDueTravelSlot(now3, hours, "2026-09-29@5"); !ok || key != "2026-09-29@9" {
		t.Fatalf("slot at the catch-up window edge must fire, got (%q,%v)", key, ok)
	}
}

// TestTravelCrossDay: the day boundary is included; yesterday's last slot is
// not replayable once out of window, and the first slot of the new day fires
// with a day-qualified key even though its hour was also used yesterday.
func TestTravelCrossDay(t *testing.T) {
	hours := defaultWbTravelHours
	// 23:30 CST — latest slot 21:00 (2.5h ago) → out of window, nothing due.
	now := time.Date(2026, 9, 29, 23, 30, 0, 0, cstZone)
	if _, key, ok := wbDueTravelSlot(now, hours, "2026-09-29@21"); ok {
		t.Fatalf("23:30 must not replay 21:00, got %q", key)
	}
	// 00:40 next day — nothing run since yesterday 21:00, latest slot is
	// yesterday 21:00 (3h40m ago) → skipped; 01:00 fires next.
	if _, key, ok := wbDueTravelSlot(time.Date(2026, 9, 30, 0, 40, 0, 0, cstZone), hours, "2026-09-29@21"); ok {
		t.Fatalf("00:40 must not replay yesterday 21:00, got %q", key)
	}
	// 01:00 next day: the key is day-qualified, so even though hour 1 fired
	// yesterday, today's 01:00 is a fresh independent slot.
	if _, key, ok := wbDueTravelSlot(time.Date(2026, 9, 30, 1, 0, 0, 0, cstZone), hours, "2026-09-29@1"); !ok || key != "2026-09-30@1" {
		t.Fatalf("new-day 01:00 must fire, got (%q,%v)", key, ok)
	}
}

// TestNextCheckinTimeWakesAtTravelSlot: with auto travel on, the scheduler
// wake must include the CST-anchored travel instants (the checkin hours are
// far away in this scenario), and with it off the travel instants disappear.
func TestNextCheckinTimeWakesAtTravelSlot(t *testing.T) {
	auto, raw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(auto, raw) })

	// 2026-09-29 10:30 CST: next travel slot is 13:00 CST.
	now := time.Date(2026, 9, 29, 10, 30, 0, 0, cstZone)
	setWbTravelConfig(true, "")
	next := nextCheckinTime(now).In(cstZone)
	if next.Hour() != 13 || next.Minute() != 0 {
		t.Fatalf("next wake = %s CST, want 13:00 CST travel slot", next.Format(time.RFC3339))
	}
	// Disabled: the next wake is the next checkin/keepalive/night slot in the
	// host-local zone instead (13:00 CST would otherwise win).
	setWbTravelConfig(false, "")
	nextOff := nextCheckinTime(now)
	if nextOff.In(cstZone).Hour() == 13 {
		t.Fatalf("disabled travel must not wake at a travel slot, got %s", nextOff)
	}
}

// TestTasksTravelStepOwnership: the per-account travel step is gated by the
// caller's includeTravel flag, and the auto path derives that flag from the
// travel_auto ownership predicate.
func TestTasksTravelStepOwnership(t *testing.T) {
	prev := tasksTravelOnceFn
	calls := 0
	tasksTravelOnceFn = func(*storedAuth, func(string, ...any)) { calls++ }
	t.Cleanup(func() { tasksTravelOnceFn = prev })

	sa := &storedAuth{}
	add := func(string, ...any) {}
	tasksTravelStep(sa, false, add)
	if calls != 0 {
		t.Fatalf("includeTravel=false must not advance travel, got %d calls", calls)
	}
	tasksTravelStep(sa, true, add)
	if calls != 1 {
		t.Fatalf("includeTravel=true must advance travel once, got %d calls", calls)
	}

	// Architect ruling 2026-09-29: no automatic entry advances travel. With
	// travel_auto on the independent pass owns it; with travel_auto off the
	// switch closes the check-in loop's travel step as well. Manual runs keep
	// including travel (manualTasksIncludeTravel).
	auto, raw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(auto, raw) })
	setWbTravelConfig(true, "")
	if autoTasksIncludeTravel() {
		t.Fatal("auto travel owns travel → check-in loop must not advance it")
	}
	setWbTravelConfig(false, "")
	if autoTasksIncludeTravel() {
		t.Fatal("travel_auto=false must close the check-in loop's travel step too")
	}
	if !manualTasksIncludeTravel() {
		t.Fatal("manual task runs must keep including travel")
	}
}

// TestTravelHoursFallbackAtRuntime pins the config-facing defaults and the
// gate predicate used by the auto loop.
func TestTravelHoursFallbackAtRuntime(t *testing.T) {
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })

	setWbTravelConfig(true, "")
	wbTravelHoursMustEqual(t, wbTravelHours(), 1, 5, 9, 13, 17, 21)
	setWbTravelConfig(true, "3,15")
	wbTravelHoursMustEqual(t, wbTravelHours(), 3, 15)

	setWbTravelConfig(false, "")
	if wbAutoTravelOwnsTravel() {
		t.Fatal("disabled travel_auto must not own travel")
	}
	setWbTravelConfig(true, "3,15")
	if !wbAutoTravelOwnsTravel() {
		t.Fatal("enabled travel_auto with valid hours must own travel")
	}
	setWbTravelConfig(true, "bad")
	if !wbAutoTravelOwnsTravel() {
		t.Fatal("invalid hours fall back to the default schedule, which still owns travel")
	}
}
