// checkin.go implements daily check-in for CN accounts: the manual
// handleManualCheckin endpoint, the 09:00 / 21:00 auto scheduler, and the
// per-account mutex that prevents duplicate check-ins from racing browser
// tabs. Global accounts are excluded — they use one-shot trial claims instead.
package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	schedulerStop chan struct{}
	schedulerMu   sync.Mutex
)

func ensureScheduler() {
	schedulerMu.Lock()
	defer schedulerMu.Unlock()
	if schedulerStop != nil {
		return // already running
	}
	schedulerStop = make(chan struct{})
	go schedulerLoop(schedulerStop)
}

// Note: there is deliberately no stopCheckinScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

// wbSchedulerWake is one planned scheduler wake: the instant plus the task
// families scheduled for exactly that instant.
//
// S2 review fix (reference: workbuddy2api Scheduler.nextWake): a wake carries
// the kinds it was planned for instead of running every family. Before this,
// the loop ran runAutoCheckin() on every wake — including the four travel-only
// slots (01/05/13/17 CST) — so the check-in tick ran up to six times a day
// instead of the scheduled 09:00/21:00.
type wbSchedulerWake struct {
	At        time.Time
	Checkin   bool
	Keepalive bool
	Travel    bool
}

type wbWakeKind int

const (
	wbWakeKindCheckin wbWakeKind = iota
	wbWakeKindKeepalive
	wbWakeKindTravel
)

func (w *wbSchedulerWake) mark(kind wbWakeKind) {
	switch kind {
	case wbWakeKindCheckin:
		w.Checkin = true
	case wbWakeKindKeepalive:
		w.Keepalive = true
	case wbWakeKindTravel:
		w.Travel = true
	}
}

// wbNextLocalHour returns today's h:00 local instant, or tomorrow's when it is
// not strictly after now (same rule as the historical nextCheckinTime).
func wbNextLocalHour(now time.Time, h int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
	}
	return t
}

// wbJitterWindowMinutes bounds the randomized offset applied to a check-in
// slot so the daily action does not land on the exact hour mark (owner
// schedule ruling 2026-09-30: 00:05–00:15 CST window).
const wbJitterWindowMinutes = 10

// wbJitterMinutes draws the check-in slot's random offset (5–14 minutes).
// Tests replace this seam with a deterministic value so wake arithmetic
// stays exact; production keeps the live draw.
var wbJitterMinutes = func() int {
	return 5 + rand.Intn(wbJitterWindowMinutes)
}

// wbNextCheckinInstant returns the next check-in instant strictly after now:
// the slot hour h:00 plus a random 5–15 minute offset. Same day-rollover
// rule as wbNextLocalHour.
func wbNextCheckinInstant(now time.Time, h int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
	t = t.Add(time.Duration(wbJitterMinutes()) * time.Minute)
	if !t.After(now) {
		t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
	}
	return t
}

// wbNextSchedulerWake computes the next wake strictly after now and the task
// families scheduled for that instant (reference nextWake semantics). Local
// hour tables (check-in / keepalive) stay host-local; travel slots are
// CST-anchored instants converted through wbNextTravelSlot, plus a one-second
// margin so the timer lands at or after the slot instant.
//
// A zero At means every family is currently disabled; the loop idles instead
// of spinning.
func wbNextSchedulerWake(now time.Time) wbSchedulerWake {
	var wake wbSchedulerWake
	consider := func(t time.Time, kind wbWakeKind) {
		if t.IsZero() {
			return
		}
		switch {
		case wake.At.IsZero() || t.Before(wake.At):
			wake = wbSchedulerWake{At: t}
			wake.mark(kind)
		case t.Equal(wake.At):
			wake.mark(kind)
		}
	}
	for _, h := range checkinHours {
		consider(wbNextCheckinInstant(now, h), wbWakeKindCheckin)
	}
	for _, h := range keepaliveHours {
		consider(wbNextLocalHour(now, h), wbWakeKindKeepalive)
	}
	// Independent travel slots (I6 parity: default 1,5,9,13,17,21 CST) are
	// CST-anchored instants, not host-local hours: if the host clock is not in
	// UTC+8, converting the hours into local time would drift the wake away
	// from the slot (and out of the catch-up window). +1s guarantees the tick
	// lands at or after the slot instant so wbTakeDueTravelSlot sees it.
	if wbAutoTravelEnabled() {
		if t := wbNextTravelSlot(now, wbTravelHours()); !t.IsZero() {
			consider(t.Add(time.Second), wbWakeKindTravel)
		}
	}
	return wake
}

// nextCheckinTime is the historical name kept for callers/tests; it returns
// the next wake instant.
//
// Owner schedule ruling 2026-09-30: the check-in slot is the single midnight
// window 00:05–00:15 (00:00 + random 5–15 min). The travel slots (1/5/9/13/
// 17/21 CST) double as check-in RETRY wakes: wbDispatchSchedulerWake runs the
// check-in pass on travel wakes too — runAutoCheckin's upstream check-in is
// idempotent per day (already-checked-in is a harmless no-op), so a failed
// midnight attempt recovers on the next travel wake. The travel wake is
// deliberately NOT merged into this instant (the travel pass must fire at its
// own slot), so tests assert the window on wbNextCheckinInstant directly.
func nextCheckinTime(now time.Time) time.Time {
	return wbNextSchedulerWake(now).At
}

func schedulerLoop(stop chan struct{}) {
	for {
		wake := wbNextSchedulerWake(time.Now())
		if wake.At.IsZero() {
			// Every family disabled (defensive: the hour tables are fixed and
			// non-empty, so this only happens if a caller empties them). Idle
			// instead of spinning; a stop signal still exits promptly.
			select {
			case <-stop:
				return
			case <-time.After(time.Hour):
				continue
			}
		}
		timer := time.NewTimer(time.Until(wake.At))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			wbDispatchSchedulerWake(wake, time.Now())
		}
	}
}

// Scheduler dispatch sinks — seams so tests can exercise wake dispatch without
// network. Production values are the real implementations.
var (
	runAutoCheckinFn    = runAutoCheckin
	runAutoTravelFn     = runAutoTravel
	runTokenKeepaliveFn = runTokenKeepalive
)

// wbDispatchSchedulerWake runs exactly the task families the wake was planned
// for, plus a due independent travel slot.
//
// Travel is attempted on every wake because wbTakeDueTravelSlot is itself
// slot-deduplicated (slot key + bounded catch-up window), which also covers the
// coincident case where a check-in wake is planned one second before the same
// CST hour's travel instant. Keepalive runs only when planned.
//
// Check-in runs on check-in wakes AND on travel wakes (owner schedule ruling
// 2026-09-30: the six travel slots double as daily check-in retries). The
// upstream check-in is idempotent per day, so a travel-wake retry after a
// successful midnight attempt is a harmless no-op — no duplicate credit, no
// duplicate action. S2's "travel wake must not run check-in" constraint is
// explicitly superseded by this owner ruling.
func wbDispatchSchedulerWake(wake wbSchedulerWake, now time.Time) {
	if wake.Checkin || wake.Travel {
		runAutoCheckinFn()
	}
	// Fire keepalive when the current tick falls within its scheduled window
	// (e.g. 22:00 keepalive fires on the 22:00 tick).
	if wake.Keepalive && shouldRunKeepaliveNow(now) {
		runTokenKeepaliveFn()
	}
	if slot, key, ok := wbTakeDueTravelSlot(now); ok {
		log.Printf("travel: slot %s due (%s) — running travel pass", key, slot.Format(time.RFC3339))
		runAutoTravelFn()
	}
}

// runAutoCheckin is the scheduled lifecycle tick (09:00 / 21:00).
// CN: optional daily check-in, then reconcile (disable exhausted / reenable after credits).
// Global: no auto trial (one-shot claim is manual only); reconcile may delete exhausted auths.
//
// v0.6.31: per-account work runs concurrently (sem=4) — was serial, so N accounts
// meant 3N serial HTTP round-trips on the billing API. Matches the pattern used
// by buildDashboardEx and handleManualCheckin.
func runAutoCheckin() {
	checkinAutoMu.RLock()
	doCheckin := checkinAuto
	checkinAutoMu.RUnlock()
	// Lifecycle may still run when check-in is off (credit gate).
	if !doCheckin && !lifecycleEnabled() {
		return
	}
	files, err := hostAuthListFn()
	if err != nil {
		return
	}
	// One upstream action per account: collapse the legacy + canonical copies
	// before fan-out, canonical copy preferred (round-2 review). The per-pass
	// guard inside processAutoCheckinAccount stays as a safety net.
	if doCheckin {
		files, _ = wbDedupedTargets(files)
	}
	// A-24: per-account work runs concurrently (sem=4).
	seen := &wbAccountPassGuard{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processAutoCheckinAccount(f, doCheckin, seen)
		}()
	}
	wg.Wait()
}

// processAutoCheckinAccount handles one account's scheduled tick. Extracted so
// runAutoCheckin can fan out per-account work without duplicating logic.
func processAutoCheckinAccount(f pluginapi.HostAuthFileEntry, doCheckin bool, seen *wbAccountPassGuard) {
	// A-24: only fetch sa when needed (checkin). For lifecycle-only paths,
	// let reconcileOneAccount do the single hostAuthGetBundle internally.
	if doCheckin {
		sa, err := hostAuthGetFn(f.AuthIndex)
		if err != nil {
			return
		}
		// A deliberately removed (tombstoned) identity — or an untrusted
		// tombstone store — is not acted on, even though its preserved legacy
		// copy may still be listed (round-2/round-3 enforcement, fail-closed).
		if wbTombstoneBlocksAuth(sa) {
			return
		}
		// Legacy + canonical copies of one credential are one upstream account:
		// act once per pass (second audit: doubled check-in app calls).
		if !seen.first(sa) {
			return
		}
		if isGlobalDomain(sa.Auth.Domain) {
			// Global: never check-in or auto-claim trial. Lifecycle only.
			// Invalidate cache (copy entry, set credits=nil, keep plan/checkin).
			if v, ok := accountCache.Load(f.ID); ok {
				if e, ok2 := v.(*accountCacheEntry); ok2 {
					fresh := *e
					fresh.credits = nil
					fresh.fetched = time.Now()
					accountCache.Store(f.ID, &fresh)
				}
			}
			if lifecycleEnabled() {
				_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
			}
			return
		}
		// CN: the whole check-in + growth section runs under the unified
		// per-account lock so it cannot race a manual check-in/task run or the
		// travel pass (I3 review fix). The lock is keyed by account identity
		// (uid × realm), so both copies of a duplicated credential serialize on
		// ONE mutex (round-2 review). reconcileOneAccount below takes the same
		// lock in its leaf writers, so it must stay OUTSIDE this section.
		mu := wbAccountLockFor(f.AuthIndex, sa)
		mu.Lock()
		// CN: daily check-in when enabled.
		ci, err := fetchCheckinStatus(sa)
		if err == nil && ci != nil && ci.Active && !ci.TodayCheckedIn {
			if _, callErr := performCheckinCall(sa); callErr == nil {
				// Refresh once after a successful checkin call so cache reflects
				// the post-call state. If the status call fails keep the pre-call
				// snapshot rather than dropping it (v0.6.31: avoid shadowing ci
				// with a second fetch that could race with concurrent readers).
				if ci2, _ := fetchCheckinStatus(sa); ci2 != nil {
					ci = ci2
				}
				// P1-5: checkin grants new credits — refresh the credits cache
				// immediately so the panel shows the updated balance without
				// waiting for the async reconcile pass.
				if cr2, crErr := fetchUserResource(sa); crErr == nil && cr2 != nil {
					if v, ok := accountCache.Load(f.ID); ok {
						if prev, ok2 := v.(*accountCacheEntry); ok2 {
							fresh := *prev
							fresh.credits = cr2
							fresh.fetched = time.Now()
							accountCache.Store(f.ID, &fresh)
						}
					}
				}
			}
		}
		// v0.9.13: growth-center daily bonus loop rides the check-in tick
		// (task center: report / makeup / accept / travel / redeem / lottery /
		// claim). Gated by tasks_auto; failures are absorbed into its own
		// summary and never affect the checkin flow above.
		// Architect ruling 2026-09-29: no automatic loop travels — travel_auto
		// off closes this entry too, and travel_auto on leaves travel to the
		// independent six-slot pass.
		if tasksAutoEnabled() {
			if res := tasksDailyBonusModeFn(sa, autoTasksIncludeTravel()); res != nil {
				for _, line := range res.Lines {
					log.Printf("tasks %s: %s", f.AuthIndex, line)
				}
			}
		}
		// Refresh cache with latest checkin status (merge, don't wipe credits/plan).
		if ci != nil {
			var prev *accountCacheEntry
			if v, ok := accountCache.Load(f.ID); ok {
				prev, _ = v.(*accountCacheEntry)
			}
			entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
			if prev != nil {
				entry.credits = prev.credits
				entry.plan = prev.plan
			}
			accountCache.Store(f.ID, entry)
		}
		mu.Unlock()
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
		}
		return
	}
	// Lifecycle-only (checkin off): reconcile handles its own get.
	if lifecycleEnabled() {
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

// handleManualCheckin serves POST /checkin.
//
// Single-account mode (body.auth_index set): runs checkinOneAccount directly —
// one hostAuthGet + at most two upstream calls (status, then check-in). The
// old three-phase classify/execute/summarize pipeline ran the same framework
// for one account as for thirty (4 RPC + 5 HTTP + lock-held re-reads), which
// routinely blew past the 30s management API timeout: the check-in succeeded
// upstream but the response never reached the panel.
//
// Batch mode (empty auth_index): fans out checkinOneAccount per account
// (sem=4), preserving input order and the {results, summary} response shape
// the panel depends on.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthListFn()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}
	// Batch mode: legacy + canonical copies of one credential are one upstream
	// account — check in once (second audit). Single-account mode is explicit:
	// the caller named one credential, so it is never deduplicated.
	duplicates := 0
	if !single {
		targets, duplicates = wbDedupedTargets(targets)
		if len(targets) == 0 {
			return map[string]any{"error": "no matching account"}
		}
	}

	results := make([]map[string]any, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = checkinOneAccountFn(f)
		}(i, f)
	}
	wg.Wait()

	// Summary counters (field names are a panel contract — panel.html
	// checkinAll reads success/already/fail/skipped_global/eligible).
	successN, failN, alreadyN, globalN, eligibleN := 0, 0, 0, 0, 0
	for _, out := range results {
		if out == nil {
			continue
		}
		if out["error"] != nil {
			failN++
			continue
		}
		reason, _ := out["reason"].(string)
		switch reason {
		case "already":
			alreadyN++
			continue
		case "global":
			globalN++
			continue
		}
		eligibleN++
		if out["success"] == true {
			successN++
		} else {
			failN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":             len(targets),
			"eligible":          eligibleN,
			"success":           successN,
			"already":           alreadyN,
			"skipped_global":    globalN,
			"skipped_duplicate": duplicates,
			"fail":              failN,
			"attempted":         eligibleN,
		},
	}
}

// checkinOneAccountFn is the per-account dispatch seam used by the batch
// endpoint (tests count exactly one call per account).
var checkinOneAccountFn = checkinOneAccount

// checkinOneAccount performs the daily check-in for one account with the
// minimum number of round-trips:
//
//	hostAuthGet ×1 (RPC) → domain gate → fetchCheckinStatus ×1 (failure
//	tolerated, upstream is idempotent) → performCheckinCall ×1.
//
// No lock-held re-reads, no lifecycle reconcile (that is the scheduled
// runAutoCheckin path's job), no redundant status refetches. Cache updates
// merge into the previous entry so credits/plan survive.
func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	out := map[string]any{"auth_index": f.AuthIndex}

	sa, err := hostAuthGet(f.AuthIndex)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["nickname"] = sa.Account.Nickname

	if isGlobalDomain(sa.Auth.Domain) {
		out["success"] = false
		out["skipped"] = true
		out["reason"] = "global"
		out["message"] = "国际版账号不支持签到，请使用领取专家加油包"
		return out
	}

	mu := wbAccountLockFor(f.AuthIndex, sa)
	mu.Lock()
	defer mu.Unlock()

	// Status probe: a failure here is NOT fatal — the check-in call below is
	// idempotent upstream and its business message tells us "already" anyway.
	ci, ciErr := fetchCheckinStatus(sa)
	if ciErr == nil && ci != nil && ci.TodayCheckedIn {
		mergeCheckinCache(f.ID, ci)
		out["success"] = true
		out["skipped"] = true
		out["reason"] = "already"
		out["message"] = "already checked in today"
		return out
	}
	if ciErr == nil && ci != nil && !ci.Active {
		// No check-in plan upstream (intl/trial accounts etc.). Calling
		// daily-checkin anyway would surface the raw business error
		// "没有签到计划" to the panel, which reads like a failure
		// (2026-09-02 user report). Surface a clean skip instead — mirrors
		// cockpit-tools' "签到活动未开启或不适用" handling.
		mergeCheckinCache(f.ID, ci)
		out["success"] = false
		out["skipped"] = true
		out["reason"] = "inactive"
		out["message"] = "该账号暂无签到计划（签到活动未开启或不适用）"
		return out
	}

	res, err := performCheckinCall(sa)
	if err != nil {
		out["error"] = err.Error()
		out["success"] = false
		return out
	}
	for k, v := range res {
		out[k] = v
	}
	// Business soft-fail ("already checked in" family) → done, not failure.
	if msg, _ := out["message"].(string); msg != "" && out["success"] == false {
		low := strings.ToLower(msg)
		if strings.Contains(low, "already") || strings.Contains(msg, "已签") || strings.Contains(msg, "今日") {
			out["success"] = true
			out["skipped"] = true
			out["reason"] = "already"
		}
		if strings.Contains(msg, "签到计划") || strings.Contains(low, "no checkin plan") ||
			strings.Contains(low, "no check-in plan") {
			// Upstream explicitly says there is no check-in plan for this
			// account (intl/trial) — same presentation as the Active gate
			// above so the panel never shows a raw business error.
			out["skipped"] = true
			out["reason"] = "inactive"
			out["message"] = "该账号暂无签到计划（签到活动未开启或不适用）"
		}
	}
	if _, ok := out["success"]; !ok {
		out["success"] = true
	}

	// Post-call cache refresh: one extra status fetch at most; on failure
	// write a TodayCheckedIn placeholder rather than leaving the panel stale.
	if ci2, err2 := fetchCheckinStatus(sa); err2 == nil && ci2 != nil {
		mergeCheckinCache(f.ID, ci2)
	} else {
		mergeCheckinCache(f.ID, &checkinSummary{TodayCheckedIn: true})
	}
	return out
}

// mergeCheckinCache stores the latest check-in snapshot while preserving the
// credits/plan fields of any previous cache entry (merge, not replace).
func mergeCheckinCache(authID string, ci *checkinSummary) {
	var prev *accountCacheEntry
	if v, ok := accountCache.Load(authID); ok {
		prev, _ = v.(*accountCacheEntry)
	}
	entry := &accountCacheEntry{checkin: ci, fetched: time.Now()}
	if prev != nil {
		entry.credits = prev.credits
		entry.plan = prev.plan
	}
	accountCache.Store(authID, entry)
}

// wbAccountLockSoftCap only surfaces pathological lock-table growth (the table
// is expected to hold a handful to tens of entries per process).
const wbAccountLockSoftCap = 512

// pruneCheckinLocks is intentionally a NO-OP this round.
//
// Round-2 review: every reclamation scheme here is racy — between
// TryLock/Unlock (or LoadAndDelete) and the actual removal another goroutine can
// acquire the old mutex, and the next pass would then create a PARALLEL mutex
// for the same account, defeating the unified per-account serialization. Safe
// reclamation needs refcounts/generation management (out of scope this round).
//
// The unified lock table therefore keeps one mutex per account identity or auth
// record seen for the PROCESS lifetime. Growth is bounded by the auth set
// (tens of entries), never by request volume; a new account adds one entry.
// The function is kept (and called from the dashboard prune path) so the
// behavior is explicit and the soft-cap warning remains.
func pruneCheckinLocks() {
	n := 0
	checkinLocks.Range(func(_, _ any) bool {
		n++
		return true
	})
	if n > wbAccountLockSoftCap {
		log.Printf("workbuddy: account lock table holds %d entries (soft cap %d) — pruning is disabled (racy reclamation), investigate account churn", n, wbAccountLockSoftCap)
	}
}
