package main

// scheduler_wake_test.go — S2 review fix (red → green).
//
// Before the fix the scheduler loop called runAutoCheckin() unconditionally on
// EVERY wake, including the four travel-only slots (01/05/13/17 CST): the
// check-in tick therefore ran up to six times a day instead of the scheduled
// 09:00/21:00, doubling upstream app calls.
//
// The fix adopts the reference's nextWake model (workbuddy2api
// internal/scheduler/scheduler.go: a wake is planned together with the task
// families due at that instant and runs exactly those), while the independent
// travel pass keeps its own slot dedup/catch-up window.

import (
	"testing"
	"time"
)

// withCheckinHours pins the check-in hour table (single value) with zero
// jitter for deterministic slot arithmetic in tests. Keepalive is disabled
// (empty table) so the returned wake is purely the check-in instant.
func withCheckinHours(t *testing.T, h int) {
	t.Helper()
	prevCheckin, prevKeepalive := checkinHours, keepaliveHours
	checkinHours = []int{h}
	keepaliveHours = []int{}
	prevJitter := wbJitterMinutes
	wbJitterMinutes = func() int { return 0 }
	t.Cleanup(func() {
		checkinHours, keepaliveHours = prevCheckin, prevKeepalive
		wbJitterMinutes = prevJitter
	})
}

// withSchedulerHourTables swaps the local-hour schedule tables for the test
// duration (isolates the planner from whatever the defaults are). Check-in
// jitter is pinned to 0 so slot instants are exact; tests that exercise the
// jitter window call wbNextCheckinInstant directly instead.
func withSchedulerHourTables(t *testing.T, checkin, keepalive []int) {
	t.Helper()
	prevCheckin, prevKeepalive := checkinHours, keepaliveHours
	checkinHours, keepaliveHours = checkin, keepalive
	prevJitter := wbJitterMinutes
	wbJitterMinutes = func() int { return 0 }
	t.Cleanup(func() {
		checkinHours, keepaliveHours = prevCheckin, prevKeepalive
		wbJitterMinutes = prevJitter
	})
}

// withSchedulerDispatchSeams replaces the three wake-dispatch sinks and
// returns call counters, so dispatch tests never touch the network.
func withSchedulerDispatchSeams(t *testing.T) (checkin func() int, travel func() int) {
	t.Helper()
	prevCheckin, prevTravel := runAutoCheckinFn, runAutoTravelFn
	prevKeepalive := runTokenKeepaliveFn
	checkinCalls, travelCalls, keepaliveCalls := 0, 0, 0
	runAutoCheckinFn = func() { checkinCalls++ }
	runAutoTravelFn = func() { travelCalls++ }
	runTokenKeepaliveFn = func() *keepaliveSummary { keepaliveCalls++; return nil }
	t.Cleanup(func() {
		runAutoCheckinFn, runAutoTravelFn = prevCheckin, prevTravel
		runTokenKeepaliveFn = prevKeepalive
	})
	return func() int { return checkinCalls },
		func() int { return travelCalls }
}

// TestSchedulerWake_TravelSlot13CSTIsTravelOnly pins the planner for the exact
// scenario of the review: the 13:00 CST travel slot is a travel-only wake.
func TestSchedulerWake_TravelSlot13CSTIsTravelOnly(t *testing.T) {
	withSchedulerHourTables(t, nil, nil)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "13")

	now := time.Date(2026, 9, 29, 10, 30, 0, 0, cstZone) // 10:30 CST
	wake := wbNextSchedulerWake(now)
	want := time.Date(2026, 9, 29, 13, 0, 1, 0, cstZone) // slot + 1s margin
	if !wake.At.Equal(want) {
		t.Fatalf("next wake = %s, want %s", wake.At.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
	if wake.Checkin || wake.Keepalive {
		t.Fatalf("travel-slot wake must not carry other kinds: %+v", wake)
	}
	if !wake.Travel {
		t.Fatalf("13:00 CST wake must be a travel kind: %+v", wake)
	}
}

// TestSchedulerWake_CheckinSlotIsCheckinKind: the 09:00 local slot plans a
// check-in wake (and nothing else when travel is disabled).
func TestSchedulerWake_CheckinSlotIsCheckinKind(t *testing.T) {
	withSchedulerHourTables(t, []int{9}, nil)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(false, "")

	now := time.Date(2026, 9, 29, 8, 30, 0, 0, time.Local)
	wake := wbNextSchedulerWake(now)
	want := time.Date(2026, 9, 29, 9, 0, 0, 0, time.Local)
	if !wake.At.Equal(want) {
		t.Fatalf("next wake = %s, want %s", wake.At, want)
	}
	if !wake.Checkin || wake.Travel || wake.Keepalive {
		t.Fatalf("09:00 local wake kinds = %+v, want checkin only", wake)
	}
}

// TestSchedulerDispatch_TravelWakeRunsTravelAndCheckinRetry is the S2
// regression (travel pass runs exactly once, slot key consumed) updated by
// the owner schedule ruling 2026-09-30: travel wakes now ALSO run the
// check-in retry (upstream is idempotent per day — a no-op after a
// successful midnight attempt, a recovery after a failed one).
func TestSchedulerDispatch_TravelWakeRunsTravelAndCheckinRetry(t *testing.T) {
	resetTravelSlotStateForTest(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "13")

	checkin, travel := withSchedulerDispatchSeams(t)
	at := time.Date(2026, 9, 29, 13, 0, 1, 0, cstZone)
	wbDispatchSchedulerWake(wbSchedulerWake{At: at, Travel: true}, at)
	if got := checkin(); got != 1 {
		t.Fatalf("travel-slot wake ran the check-in retry %d time(s), want 1", got)
	}
	if got := travel(); got != 1 {
		t.Fatalf("travel pass ran %d time(s), want exactly 1", got)
	}
	// A second dispatch at the same slot must not re-fire (slot key consumed).
	wbDispatchSchedulerWake(wbSchedulerWake{At: at, Travel: true}, at)
	if got := travel(); got != 1 {
		t.Fatalf("same slot double-fired: travel ran %d time(s), want 1", got)
	}
}

// TestSchedulerDispatch_CheckinWakeRunsCheckinOnce covers the 09:00/21:00
// path: exactly one check-in call per planned wake.
func TestSchedulerDispatch_CheckinWakeRunsCheckinOnce(t *testing.T) {
	resetTravelSlotStateForTest(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(false, "") // isolate: no travel due

	checkin, travel := withSchedulerDispatchSeams(t)
	at := time.Date(2026, 9, 29, 9, 0, 0, 0, time.Local)
	wbDispatchSchedulerWake(wbSchedulerWake{At: at, Checkin: true}, at)
	if got := checkin(); got != 1 {
		t.Fatalf("check-in wake ran the check-in tick %d time(s), want exactly 1", got)
	}
	if got := travel(); got != 0 {
		t.Fatalf("check-in wake with travel owned elsewhere ran travel %d time(s), want 0", got)
	}
}

// TestSchedulerDispatch_CoincidentCheckinAndTravelRunsEachOnce: when the
// 09:00/21:00 check-in slot coincides with a CST travel slot, both run once
// (reference runBatch semantics) — and neither runs twice.
func TestSchedulerDispatch_CoincidentCheckinAndTravelRunsEachOnce(t *testing.T) {
	resetTravelSlotStateForTest(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "9")

	checkin, travel := withSchedulerDispatchSeams(t)
	at := time.Date(2026, 9, 29, 9, 0, 0, 0, cstZone)
	wbDispatchSchedulerWake(wbSchedulerWake{At: at, Checkin: true, Travel: true}, at)
	if got := checkin(); got != 1 {
		t.Fatalf("coincident wake ran check-in %d time(s), want 1", got)
	}
	if got := travel(); got != 1 {
		t.Fatalf("coincident wake ran travel %d time(s), want 1", got)
	}
}
