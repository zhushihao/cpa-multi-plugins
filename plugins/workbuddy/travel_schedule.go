// travel_schedule.go implements the independent six-slot cat-travel schedule
// (parity with workbuddy2api's travel_hours, deployed as [1,5,9,13,17,21] in
// Asia/Shanghai) plus the runtime pass that advances the travel state machine
// once per slot per account.
//
// Why independent: the plugin historically ran travel only as one step of the
// 09:00/21:00 check-in tick (2 passes/day), while the reference schedules
// travel at every CST travel hour with no check-in dependency (4h cadence
// matches the upstream ~3.5h trip cycle). This file owns the schedule; when
// it is enabled, the auto check-in tick no longer advances travel itself
// (tasksDailyBonusMode(..., includeTravel=false)) so there is exactly one
// automatic travel driver. Manual /tasks/run still advances travel as before.
//
// Catch-up semantics: a process that starts shortly after a slot runs that
// slot once (bounded by wbTravelCatchupWindow); older missed slots are not
// replayed — the next scheduled slot covers them. A slot is identified by its
// CST day+hour, so it can never double-fire within one process, and the
// upstream state machine is idempotent across restarts (idle→depart,
// traveling→skip, arrived→claim).
package main

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Travel slots use the shared CST clock from billing.go (cstZone =
// Asia/Shanghai, UTC+8, no DST) — the same zone the upstream reports its
// calendars in, so a fixed zone avoids depending on host tzdata. Matches
// workbuddy2api's cstZone.

// defaultWbTravelHours is the parity schedule: six independent slots per CST
// day.
var defaultWbTravelHours = []int{1, 5, 9, 13, 17, 21}

// wbTravelCatchupWindow bounds restart catch-up: only a slot this recent is
// replayed once; anything older waits for the next slot.
const wbTravelCatchupWindow = 2 * time.Hour

// wbTravelAccountDelay throttles accounts within one travel pass (~800ms,
// reference travelAccountDelay). Tests set it to 0.
var wbTravelAccountDelay = 800 * time.Millisecond

// -----------------------------------------------------------------------------
// Config
// -----------------------------------------------------------------------------

var (
	wbTravelMu       sync.RWMutex
	wbTravelAuto     = true // travel_auto, default on
	wbTravelHoursRaw = ""   // travel_hours, empty → defaultWbTravelHours
)

// setWbTravelConfig installs the parsed travel config (called from configure).
func setWbTravelConfig(auto bool, raw string) {
	wbTravelMu.Lock()
	wbTravelAuto = auto
	wbTravelHoursRaw = strings.TrimSpace(raw)
	wbTravelMu.Unlock()
}

// wbTravelConfig snapshots the raw config (tests restore through it).
func wbTravelConfig() (bool, string) {
	wbTravelMu.RLock()
	defer wbTravelMu.RUnlock()
	return wbTravelAuto, wbTravelHoursRaw
}

// wbTravelHours returns the effective slot list.
func wbTravelHours() []int {
	wbTravelMu.RLock()
	raw := wbTravelHoursRaw
	wbTravelMu.RUnlock()
	return wbParseTravelHours(raw)
}

// wbAutoTravelEnabled reports the travel_auto toggle.
func wbAutoTravelEnabled() bool {
	wbTravelMu.RLock()
	defer wbTravelMu.RUnlock()
	return wbTravelAuto
}

// wbAutoTravelOwnsTravel reports whether the independent auto pass is the
// single automatic travel driver. When true, the check-in tick's task loop
// must not also advance travel.
func wbAutoTravelOwnsTravel() bool {
	wbTravelMu.RLock()
	auto := wbTravelAuto
	raw := wbTravelHoursRaw
	wbTravelMu.RUnlock()
	return auto && len(wbParseTravelHours(raw)) > 0
}

// wbParseTravelHours parses "1,5,9,13,17,21": whitespace, duplicates and
// out-of-range/non-numeric entries are dropped; an empty or fully invalid
// value falls back to the default schedule.
func wbParseTravelHours(raw string) []int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]int(nil), defaultWbTravelHours...)
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(defaultWbTravelHours))
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || n > 23 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return append([]int(nil), defaultWbTravelHours...)
	}
	sort.Ints(out)
	return out
}

// -----------------------------------------------------------------------------
// Pure schedule math
// -----------------------------------------------------------------------------

// wbTravelSlotKey names a slot by CST calendar day + hour.
func wbTravelSlotKey(t time.Time, hour int) string {
	return t.In(cstZone).Format("2006-01-02") + "@" + strconv.Itoa(hour)
}

// wbLatestTravelSlot returns the most recent slot at or before now. Before the
// first slot of a CST day it returns yesterday's last slot (so the catch-up
// window still applies across midnight).
func wbLatestTravelSlot(now time.Time, hours []int) (time.Time, string, bool) {
	if len(hours) == 0 {
		return time.Time{}, "", false
	}
	nowCST := now.In(cstZone)
	var best time.Time
	for _, h := range hours {
		cand := time.Date(nowCST.Year(), nowCST.Month(), nowCST.Day(), h, 0, 0, 0, cstZone)
		if !cand.After(nowCST) && (best.IsZero() || cand.After(best)) {
			best = cand
		}
	}
	if best.IsZero() {
		y := nowCST.AddDate(0, 0, -1)
		h := hours[len(hours)-1]
		best = time.Date(y.Year(), y.Month(), y.Day(), h, 0, 0, 0, cstZone)
	}
	return best, wbTravelSlotKey(best, best.In(cstZone).Hour()), true
}

// wbNextTravelSlot returns the next slot strictly after now (CST-anchored).
func wbNextTravelSlot(now time.Time, hours []int) time.Time {
	if len(hours) == 0 {
		return time.Time{}
	}
	nowCST := now.In(cstZone)
	for _, h := range hours {
		cand := time.Date(nowCST.Year(), nowCST.Month(), nowCST.Day(), h, 0, 0, 0, cstZone)
		if cand.After(nowCST) {
			return cand
		}
	}
	tomorrow := nowCST.AddDate(0, 0, 1)
	return time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), hours[0], 0, 0, 0, cstZone)
}

// wbDueTravelSlot reports the slot the caller should run now: the latest slot
// that (a) has not been recorded as run and (b) is within the catch-up window.
func wbDueTravelSlot(now time.Time, hours []int, lastKey string) (time.Time, string, bool) {
	slot, key, ok := wbLatestTravelSlot(now, hours)
	if !ok || key == lastKey {
		return time.Time{}, "", false
	}
	if now.Sub(slot) > wbTravelCatchupWindow {
		return time.Time{}, "", false
	}
	return slot, key, true
}

// -----------------------------------------------------------------------------
// Runtime pass
// -----------------------------------------------------------------------------

var (
	wbTravelRunMu   sync.Mutex
	wbTravelLastKey string // in-memory: a restart re-runs at most one catch-up
)

// wbTakeDueTravelSlot consumes the currently due slot (if any) and returns it.
// Marking before running prevents a second scheduler tick from double-firing.
func wbTakeDueTravelSlot(now time.Time) (time.Time, string, bool) {
	if !wbAutoTravelEnabled() {
		return time.Time{}, "", false
	}
	hours := wbTravelHours()
	wbTravelRunMu.Lock()
	defer wbTravelRunMu.Unlock()
	slot, key, ok := wbDueTravelSlot(now, hours, wbTravelLastKey)
	if !ok {
		return time.Time{}, "", false
	}
	wbTravelLastKey = key
	return slot, key, true
}

// runAutoTravel advances the travel state machine once for every CN account.
// Accounts are visited serially with a small inter-account delay (upstream
// friendliness), each under the UNIFIED per-account lock (identity-keyed) so a
// manual check-in/task run, the scheduled tick and this pass cannot race the
// same account — including when the host lists both the canonical and the
// preserved legacy copy of one credential (round-2 review). The canonical copy
// is the representative the pass acts on.
func runAutoTravel() {
	if !wbAutoTravelEnabled() {
		return
	}
	files, err := hostAuthListFn()
	if err != nil {
		log.Printf("travel: host auth list failed: %v", err)
		return
	}
	// One entry per account identity, canonical copy preferred.
	targets, _ := wbDedupedTargets(files)
	first := true
	for _, f := range targets {
		f := f
		// Resolve the identity for the unified lock before taking it.
		lockSA, err := hostAuthGetFn(f.AuthIndex)
		if err != nil || !tasksSupportsGrowth(lockSA) {
			continue // CN-only: global/intl have no travel system
		}
		mu := wbAccountLockFor(f.AuthIndex, lockSA)
		mu.Lock()
		// Fresh read under the lock: the token may have been refreshed while we
		// waited, and the credential is what the travel step actually uses.
		sa, err := hostAuthGetFn(f.AuthIndex)
		if err != nil || !tasksSupportsGrowth(sa) {
			mu.Unlock()
			continue
		}
		if !first && wbTravelAccountDelay > 0 {
			time.Sleep(wbTravelAccountDelay)
		}
		first = false
		res := &tasksBonusResult{}
		add := func(format string, args ...any) {
			res.Lines = append(res.Lines, fmt.Sprintf(format, args...))
		}
		tasksTravelOnceFn(sa, add)
		mu.Unlock()
		for _, line := range res.Lines {
			log.Printf("travel %s: %s", f.AuthIndex, line)
		}
	}
}
