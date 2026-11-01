// scheduler_night_gate_test.go — night black_cat removal contract (round6).
//
// Owner decision 2026-09-30: the upstream black_cat activity no longer
// exists, so the feature was REMOVED from the plugin (superseding the
// earlier Option A "config gate default off" approach). These tests pin the
// removal: no scheduler configuration can plan a 23:00 night wake, and the
// black_cat action is gone from the growth action table (manual runs
// enumerate the same table).
package main

import (
	"testing"
	"time"
)

// TestSchedulerNightWakeNeverPlanned pins that with the full default hour
// tables and travel disabled, the planner skips straight past the old 23:00
// night slot: from 22:30 the next wake is the 09:00 check-in the next day.
func TestSchedulerNightWakeNeverPlanned(t *testing.T) {
	withSchedulerHourTables(t, []int{9, 21}, []int{22})
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(false, "")

	now := time.Date(2026, 9, 29, 22, 30, 0, 0, cstZone)
	wake := wbNextSchedulerWake(now)
	want := time.Date(2026, 9, 30, 9, 0, 0, 0, cstZone)
	if !wake.At.Equal(want) {
		t.Fatalf("next wake from 22:30 = %s, want %s (no 23:00 night slot may be planned)", wake.At, want)
	}
	if !wake.Checkin {
		t.Fatalf("09:00 wake must be check-in kind: %+v", wake)
	}
}

// TestSchedulerNightWakeNeverPlannedBeforeSlot pins the same contract from
// earlier in the evening: from 20:00 the next wake is the 21:00 check-in,
// not a 23:00 night slot.
func TestSchedulerNightWakeNeverPlannedBeforeSlot(t *testing.T) {
	withSchedulerHourTables(t, []int{9, 21}, []int{22})
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(false, "")

	now := time.Date(2026, 9, 29, 20, 0, 0, 0, cstZone)
	wake := wbNextSchedulerWake(now)
	want := time.Date(2026, 9, 29, 21, 0, 0, 0, cstZone)
	if !wake.At.Equal(want) {
		t.Fatalf("next wake from 20:00 = %s, want %s", wake.At, want)
	}
}

// TestBlackCatRemovedFromGrowthActions pins that black_cat is gone from the
// growth action table — the auto pass and the manual /tasks/run enumeration
// both walk this table, so absence here covers both surfaces.
func TestBlackCatRemovedFromGrowthActions(t *testing.T) {
	for _, action := range growthAutoActions {
		if action.Code == "black_cat" {
			t.Fatalf("black_cat must be removed from growthAutoActions, found entry %+v", action)
		}
	}
}
