package main

// travel_independence_test.go — solution-architect ruling (2026-09-29):
//
//   - tasks_auto and travel_auto stay INDEPENDENT: disabling the growth-task
//     loop must not silently keep travel running, and enabling tasks must not
//     re-enable a disabled travel switch (the previous round's DEVIATION_REQUEST
//     is resolved this way).
//   - travel_auto=false now closes ALL AUTOMATIC travel entry points: the
//     independent six-slot pass AND the check-in tick's growth loop (the
//     reverse gap). Manual POST /tasks/run keeps the original semantics and
//     still advances travel.
//
// The four switch combinations, the manual entry and the auto-tick call path
// are all pinned here.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// withTravelTasksConfig pins travel_auto/travel_hours/tasks_auto for the test.
func withTravelTasksConfig(t *testing.T, travelAuto bool, travelHours string, tasksAutoWant bool) {
	t.Helper()
	prevAuto, prevRaw := wbTravelConfig()
	setWbTravelConfig(travelAuto, travelHours)
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	tasksAutoMu.Lock()
	prevTasks := tasksAuto
	tasksAuto = tasksAutoWant
	tasksAutoMu.Unlock()
	t.Cleanup(func() {
		tasksAutoMu.Lock()
		tasksAuto = prevTasks
		tasksAutoMu.Unlock()
	})
}

// TestTravelSwitchCoversEveryAutomaticEntryPoint walks the four switch
// combinations. The automatic task loop must NEVER travel in any combination;
// the independent pass runs only when travel_auto is on.
func TestTravelSwitchCoversEveryAutomaticEntryPoint(t *testing.T) {
	cases := []struct {
		name       string
		travelAuto bool
		tasksAuto  bool
	}{
		{"travel_on_tasks_on", true, true},
		{"travel_on_tasks_off", true, false},
		{"travel_off_tasks_on", false, true},
		{"travel_off_tasks_off", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTravelTasksConfig(t, tc.travelAuto, "", tc.tasksAuto)

			// The check-in tick's growth loop never advances travel: when
			// travel_auto is on the independent pass owns it; when off, the
			// switch closes the automatic path entirely.
			if autoTasksIncludeTravel() {
				t.Fatal("automatic task loop must never advance travel")
			}
			if wbAutoTravelEnabled() != tc.travelAuto {
				t.Fatalf("wbAutoTravelEnabled()=%v, want %v", wbAutoTravelEnabled(), tc.travelAuto)
			}
			if wbAutoTravelOwnsTravel() != tc.travelAuto {
				t.Fatalf("wbAutoTravelOwnsTravel()=%v, want %v", wbAutoTravelOwnsTravel(), tc.travelAuto)
			}

			// The independent pass only consumes a due slot when the switch is
			// on (travel_off_tasks_on must not slip through this entry either).
			resetTravelSlotStateForTest(t)
			at := time.Date(2026, 9, 29, 13, 0, 1, 0, cstZone)
			_, _, due := wbTakeDueTravelSlot(at)
			if due != tc.travelAuto {
				t.Fatalf("wbTakeDueTravelSlot due=%v, want %v", due, tc.travelAuto)
			}
		})
	}
}

// TestManualTasksRunKeepsTravelSemantics: the manual entry is explicitly
// excluded from the travel-switch closure — an operator asking for a manual
// task run still gets the travel step.
func TestManualTasksRunKeepsTravelSemantics(t *testing.T) {
	withTravelTasksConfig(t, false, "", true) // travel switch OFF

	if !manualTasksIncludeTravel() {
		t.Fatal("manual /tasks/run must keep including travel regardless of travel_auto")
	}
	prev := tasksTravelOnceFn
	calls := 0
	tasksTravelOnceFn = func(*storedAuth, func(string, ...any)) { calls++ }
	t.Cleanup(func() { tasksTravelOnceFn = prev })
	tasksTravelStep(&storedAuth{}, manualTasksIncludeTravel(), func(string, ...any) {})
	if calls != 1 {
		t.Fatalf("manual travel step called %d time(s), want 1", calls)
	}
}

// TestAutoCheckinTickDoesNotTravelWhenSwitchOff is the reverse-gap regression:
// travel_auto=false + tasks_auto=true used to run travel inside the check-in
// tick's growth loop. The tick must pass includeTravel=false.
func TestAutoCheckinTickDoesNotTravelWhenSwitchOff(t *testing.T) {
	if got := autoTickTravelFlag(t, false); got {
		t.Fatal("check-in tick asked the growth loop to travel while travel_auto=false")
	}
}

// TestAutoCheckinTickDoesNotDoubleDriveTravelWhenSwitchOn: with travel_auto on,
// the independent six-slot pass owns travel; the check-in tick must pass
// includeTravel=false as well.
func TestAutoCheckinTickDoesNotDoubleDriveTravelWhenSwitchOn(t *testing.T) {
	if got := autoTickTravelFlag(t, true); got {
		t.Fatal("check-in tick asked the growth loop to travel while travel_auto owns it")
	}
}

// TestManualTasksRunPassesIncludeTravel: the manual batch endpoint keeps the
// original semantics (travel included) in every switch combination.
func TestManualTasksRunPassesIncludeTravel(t *testing.T) {
	withTravelTasksConfig(t, false, "", true) // travel switch OFF
	prevBonus, prevMode := tasksDailyBonusFn, tasksDailyBonusModeFn
	var flag bool
	called := 0
	tasksDailyBonusFn = func(sa *storedAuth) *tasksBonusResult { return tasksDailyBonusModeFn(sa, true) }
	tasksDailyBonusModeFn = func(*storedAuth, bool) *tasksBonusResult {
		called++
		flag = manualTasksIncludeTravel()
		return &tasksBonusResult{Success: true}
	}
	t.Cleanup(func() { tasksDailyBonusFn, tasksDailyBonusModeFn = prevBonus, prevMode })
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)

	_ = handleTasksRun(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if called != 1 {
		t.Fatalf("manual task loop ran %d time(s), want 1", called)
	}
	if !flag {
		t.Fatal("manual /tasks/run must include travel regardless of travel_auto")
	}
}

// autoTickTravelFlag drives one account through the scheduled check-in tick
// with tasks_auto=true and reports the includeTravel flag it handed to the
// growth loop.
func autoTickTravelFlag(t *testing.T, travelAuto bool) bool {
	t.Helper()
	const authIndex = "idx-travel-switch"
	stubHostAuthList(t, "id-travel-switch", authIndex, "workbuddy-uid-travel-switch.json", cnLockTestAuth("uid-travel-switch"))
	withCheckinTasksConfig(t, true, true, false)
	withTravelTasksConfig(t, travelAuto, "", true)
	stubBillingEndpoint(t)

	prevMode := tasksDailyBonusModeFn
	got := true
	calls := 0
	tasksDailyBonusModeFn = func(*storedAuth, bool) *tasksBonusResult {
		calls++
		got = autoTasksIncludeTravel()
		return &tasksBonusResult{}
	}
	t.Cleanup(func() { tasksDailyBonusModeFn = prevMode })

	f := pluginapi.HostAuthFileEntry{ID: "id-travel-switch", AuthIndex: authIndex, Name: "workbuddy-uid-travel-switch.json"}
	processAutoCheckinAccount(f, true, &wbAccountPassGuard{})
	if calls != 1 {
		t.Fatalf("check-in tick ran the growth loop %d time(s), want 1", calls)
	}
	return got
}

// stubBillingEndpoint points the CN billing base at an in-process stub so the
// tick's check-in status probe stays hermetic (no real network).
func stubBillingEndpoint(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	t.Cleanup(srv.Close)
	restore := setBillingBase(srv.URL)
	t.Cleanup(restore)
}
