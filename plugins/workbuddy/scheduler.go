// scheduler.go implements the CPA scheduler.pick capability for workbuddy.
//
// Routing uses the panel-selected active account (region from that card's
// domain). When the selection is exhausted/disabled/missing, randomly switch
// to another non-exhausted workbuddy candidate. Non-workbuddy candidates are
// always deferred so the built-in scheduler handles them.
package main

import (
	"encoding/json"
	"log"
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

// handleSchedulerPick selects a workbuddy auth candidate based on the
// panel-selected active account. Non-workbuddy candidates are always deferred
// (Handled: false) so the built-in scheduler handles them.
//
// scheduler_mode:
//   - "off"     → plugin does NOT handle routing; defer everything to built-in.
//   - "credits" → plugin picks via panel-selected active account (sticky, with
//     fallback when that account becomes exhausted/disabled).
//
// Default is off (see schedulerMode init). Users opting into the plugin's
// routing should set scheduler_mode: credits in plugin config.
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
	// Issue #35 (v0.9.57): a candidate whose (account, request-model) pair is
	// cooling after an upstream failure is skipped so the plugin-side picker
	// agrees with the catalog filter (filterCoolingModels) the host-side
	// routing uses. The request model is resolved through the same alias
	// table the executors use so the key matches the recorded pair.
	reqModel := resolveUpstreamModel(req.Model, nil)
	sawWorkbuddy := false
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		sawWorkbuddy = true
		if candidateDisabled(c) {
			continue
		}
		if reqModel != "" && modelIsCooling(c.ID, reqModel) {
			continue
		}
		// v0.9.73: account-restriction quarantine (403+11140) — a restricted
		// credential is not offered as a candidate either, same invisibility
		// contract as the guardrails: the host just sees fewer candidates.
		if accountRestrictionCooling(c.ID) {
			continue
		}
		// #61 guardrails: a guarded credential is simply not offered as a
		// candidate — invisible to the host, no retry budget consumed. Only
		// evaluated here (pick time): admission (#60) owns runtime capacity,
		// guardrails own balance. When every candidate is guarded the pick
		// defers to the host below like any empty-candidate case.
		if guardSkipCredential(c.ID) {
			continue
		}
		wbCandidates = append(wbCandidates, c)
	}
	if len(wbCandidates) == 0 {
		if sawWorkbuddy && (loadedReserveCredits() > 0 || loadedDailyCreditsCap() > 0) {
			log.Printf("workbuddy: scheduler pick deferred — all %s candidates withheld by credit guardrails (reserve/daily-cap)", providerName)
		}
		if sawWorkbuddy && restrictionQuarantineCount() > 0 {
			log.Printf("workbuddy: scheduler pick deferred — all %s candidates quarantined by upstream account restriction (403+11140)", providerName)
		}
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
	ensureWbRouterJanitor()
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

// cachedCreditsDetail returns (remain, expiring, exhausted) from the credits
// cache; remain -1 = unknown. Feeds the weighted pool's three-factor weight.
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
	v, ok := accountCache.Load(authID)
	if !ok {
		return -1, false
	}
	entry, ok := v.(*accountCacheEntry)
	if !ok || entry.credits == nil {
		return -1, false
	}
	return entry.credits.TotalRemain, isCreditsExhausted(entry.credits)
}
