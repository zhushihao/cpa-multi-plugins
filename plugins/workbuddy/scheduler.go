// scheduler.go implements the CPA scheduler.pick capability for workbuddy.
//
// Two opt-in routing modes exist behind the scheduler_mode config:
//
//   - "credits"  (legacy): panel-selected active account, sticky until that
//     account is exhausted/disabled/missing.
//   - "weighted" (I6 parity mode): the cost-tier aware weighted pool
//     (routing_pool.go) plus first-user-message session stickiness delivered
//     through the request interceptor header bridge (session_stick.go).
//
// Non-workbuddy candidates are always deferred so the built-in scheduler
// handles them; scheduler_mode=off (the default) defers everything.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Legacy config values kept for configure() compatibility; pick always uses
// panel active-auth selection now (not credit-max ranking).
const (
	schedulerModeOff     = "off"
	schedulerModeCredits = "credits"
	// schedulerModeWeighted is the cost-tier aware weighted pool with session
	// stickiness (workbuddy2api parity). Additive value: off/credits behavior
	// is unchanged.
	schedulerModeWeighted = "weighted"
)

var (
	schedulerMode   = schedulerModeOff
	schedulerModeMu sync.RWMutex
)

// wbRouterPool / wbSticky are the process-wide routing state used by the
// weighted mode. Defaults mirror the reference: 30m exploration window, 100ms
// anti-thundering-herd gap, 30m sticky TTL with 5m GC.
var (
	wbRouterPool = newWbAccountPool(wbPoolParams{
		CostExploreInterval: wbDefaultCostExploreInterval,
		MinPickGap:          wbDefaultMinPickGap,
	})
	wbSticky            = newWbStickyRouter(nil)
	wbRouterJanitorOnce sync.Once
)

// ensureWbRouterJanitor starts the sticky-binding GC ticker and the periodic
// cost-state flush (idempotent).
func ensureWbRouterJanitor() {
	wbRouterJanitorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(wbStickyGCInterval)
			defer ticker.Stop()
			for range ticker.C {
				wbSticky.gc()
				// Backstop flush: observations normally save on their own
				// throttle; this bounds what a crash can lose.
				_ = wbRouterPool.maybePersistCosts()
			}
		}()
	})
}

// setSchedulerMode is a test helper that returns a restore func.
func setSchedulerMode(mode string) func() {
	schedulerModeMu.Lock()
	old := schedulerMode
	schedulerMode = mode
	schedulerModeMu.Unlock()
	return func() {
		schedulerModeMu.Lock()
		schedulerMode = old
		schedulerModeMu.Unlock()
	}
}

func loadedSchedulerMode() string {
	schedulerModeMu.RLock()
	defer schedulerModeMu.RUnlock()
	return schedulerMode
}

// handleSchedulerPick selects a workbuddy auth candidate according to
// scheduler_mode. Non-workbuddy candidates are always deferred
// (Handled: false) so the built-in scheduler handles them.
//
// scheduler_mode:
//   - "off"      → plugin does NOT handle routing; defer everything to built-in.
//   - "credits"  → panel-selected active account (sticky, fallback when that
//     account becomes exhausted/disabled).
//   - "weighted" → cost-tier filtered weighted pool + session stickiness.
//
// Default is off (see schedulerMode init). Users opting into the plugin's
// routing should set scheduler_mode in plugin config.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	// v0.6.31: actually honor the scheduler_mode toggle. Previously the config
	// was parsed but never read here, so "off" silently behaved like "credits".
	mode := loadedSchedulerMode()
	if mode != schedulerModeCredits && mode != schedulerModeWeighted {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Collect workbuddy candidates only.
	var wbCandidates []pluginapi.SchedulerAuthCandidate
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		if candidateDisabled(c) {
			continue
		}
		wbCandidates = append(wbCandidates, c)
	}
	if len(wbCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	// Legacy + canonical copies of one credential are ONE account (I5 review +
	// second audit): collapse them to a single stable representative so the
	// pool neither double-weights the account nor splits its cost ledger.
	// In-memory only — auth files are never touched.
	wbCandidates = wbDedupCandidatesByIdentity(wbCandidates)
	// A deliberately removed (tombstoned) identity must never be routed again,
	// even if the host still lists a preserved copy (round-2 enforcement).
	wbCandidates = wbDropTombstonedCandidates(wbCandidates)
	if len(wbCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	if mode == schedulerModeWeighted {
		return handleWeightedSchedulerPick(req, wbCandidates)
	}

	// Build thin view for active-auth picker.
	cands := make([]activeAuthCandidate, 0, len(wbCandidates))
	for _, c := range wbCandidates {
		_, exhausted := cachedCreditsScore(c.ID)
		cands = append(cands, activeAuthCandidate{
			ID:        c.ID,
			Disabled:  false, // already filtered
			Exhausted: exhausted,
		})
	}
	picked := pickActiveAuth(cands)
	if picked == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  picked,
		Handled: true,
	})
}

// handleWeightedSchedulerPick is the scheduler_mode=weighted path: it bridges
// the verified session header (when present) into the cost-tier pool and
// re-binds the session whenever the pool serves a different account.
func handleWeightedSchedulerPick(req pluginapi.SchedulerPickRequest, wbCandidates []pluginapi.SchedulerAuthCandidate) ([]byte, error) {
	cands := wbPoolCandidatesForScheduler(wbCandidates)
	available := make([]string, 0, len(cands))
	for _, c := range cands {
		if !c.Exhausted {
			available = append(available, c.ID)
		}
	}
	var stickyUID, stickyKey string
	var haveSticky bool
	if len(available) > 0 {
		stickyUID, stickyKey, haveSticky = wbStickyPickFromHeaders(http.Header(req.Options.Headers), req.Model, wbSticky, available)
	}
	picked, ok := wbRouterPool.pickWithSticky(req.Model, cands, stickyUID)
	if !ok {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	if haveSticky && picked != stickyUID && stickyKey != "" {
		// The pool's tier guard (or full-candidate exclusion) overrode the
		// binding — converge the session onto the account that actually served.
		wbSticky.bind(stickyKey, picked)
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  picked,
		Handled: true,
	})
}

// candidateDisabled reports host-disabled auth from Status/metadata.
func candidateDisabled(c pluginapi.SchedulerAuthCandidate) bool {
	st := strings.ToLower(strings.TrimSpace(c.Status))
	if st == "disabled" {
		return true
	}
	if c.Metadata != nil {
		if v, ok := c.Metadata["disabled"]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return strings.EqualFold(strings.TrimSpace(t), "true")
			}
		}
	}
	return false
}

// cachedCreditsScore returns (remain, exhausted) from accountCache.
// remain is -1 when unknown; exhausted uses isCreditsExhausted.
// Key is auth.ID (same as SchedulerAuthCandidate.ID and activeAuthID).
func cachedCreditsScore(authID string) (int64, bool) {
	remain, _, exhausted := cachedCreditsDetail(authID)
	return remain, exhausted
}

// cachedCreditsDetail is cachedCreditsScore plus the soon-expiring subset
// (0 when unknown or the bucket is disabled) for the weighted pool.
func cachedCreditsDetail(authID string) (remain, expiring int64, exhausted bool) {
	v, ok := accountCache.Load(authID)
	if !ok {
		return -1, 0, false
	}
	entry, ok := v.(*accountCacheEntry)
	if !ok || entry.credits == nil {
		return -1, 0, false
	}
	return entry.credits.TotalRemain, entry.credits.TotalExpiring, isCreditsExhausted(entry.credits)
}

// wbPoolCandidatesForScheduler maps host scheduler candidates to pool
// candidates, carrying the cached balance and the soon-expiring subset so the
// three-factor weight (credits / expiring / idle) is fully fed.
func wbPoolCandidatesForScheduler(wbCandidates []pluginapi.SchedulerAuthCandidate) []wbPoolCandidate {
	out := make([]wbPoolCandidate, 0, len(wbCandidates))
	for _, c := range wbCandidates {
		remain, expiring, exhausted := cachedCreditsDetail(c.ID)
		if remain < 0 {
			remain = 0 // unknown balance: no credits-factor contribution
		}
		out = append(out, wbPoolCandidate{
			ID:              c.ID,
			Exhausted:       exhausted,
			Credits:         remain,
			CreditsExpiring: expiring,
		})
	}
	return out
}
