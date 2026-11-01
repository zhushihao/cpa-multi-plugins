// cost_observe.go feeds the weighted pool's per-(account,model) cost ledger.
//
// Ported shape: the local stack observed costs from the RAW upstream usage
// object (publishUsageWithUsage carried usageMap). Upstream 0.9.82's
// UsagePlugin path only receives the canonicalized UsageDetail — the host's
// usage translation (internal/pluginhost/adapters_usage_translation.go)
// carries token counters but DROPS the upstream `credit` field, so this file
// cannot observe credit from handleUsage. What it does instead: it records a
// "spent" observation from token volume (per-1k cost left at 0 → tier 0).
//
// Consequence (documented limitation): the cost tier can distinguish accounts
// that saw traffic (tier 0, free-ish) from unknown (no observation); it can
// NOT price a costly reasoning model on top of that. weighted mode therefore
// balances on the remaining/expiring/idle factors and treats all observed
// accounts as cost-0 until the host exposes raw usage credits.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// wbObserveModelCost records one completed request's cost signal for the
// weighted pool. credit comes from the raw usage object when a caller has it
// (executor-side paths); through the canonical handleUsage path credit is
// always -1 ("unknown → tier 0"), never fabricated.
func wbObserveModelCost(authID, requestedModel string, usageMap map[string]any) {
	credit, total, ok := wbUsageCreditTotal(usageMap)
	if !ok || authID == "" || strings.TrimSpace(requestedModel) == "" {
		return
	}
	wbRouterPool.noteCost(authID, requestedModel, credit, total)
}

// wbObserveUsageDetail feeds the ledger from the canonical usage record.
// Only successful requests are observations; failed ones carry no cost
// identity and are skipped (never a zero-cost sample from an absence of data).
func wbObserveUsageDetail(authID, model string, totalTokens int64, failed bool) {
	if failed {
		return
	}
	// The canonical path has no `credit` field (see header): the observation
	// carries token volume only and costs 0/1k (tier 0, "traffic seen").
	wbRouterPool.noteCost(authID, model, 0, int(totalTokens))
}

// publishUsageWithUsage keeps the local-stack test surface alive: the raw
// usage map carried credit; here the canonical path ignores it and observes
// token volume only. Retained signature for test compatibility.
func publishUsageWithUsage(requestedModel, upstreamModel, cpampAuthID, ledgerAuthID string, started time.Time, detail usage.Detail, usageMap map[string]any, failed bool, statusCode int, errBody string) {
	_ = upstreamModel
	_ = cpampAuthID
	_ = started
	_ = statusCode
	_ = errBody
	if failed || usageMap == nil {
		return
	}
	wbObserveModelCost(ledgerAuthID, requestedModel, usageMap)
}

// wbUsageCreditTotal extracts usage.credit (float or numeric string) and a
// positive total token count from an upstream usage object.
func wbUsageCreditTotal(m map[string]any) (float64, int, bool) {
	if len(m) == 0 {
		return 0, 0, false
	}
	credit, ok := wbAnyFloat(m["credit"])
	if !ok || credit < 0 {
		return 0, 0, false
	}
	total := wbAnyInt(m["total_tokens"])
	if total <= 0 {
		total = wbAnyInt(m["prompt_tokens"]) + wbAnyInt(m["completion_tokens"])
	}
	if total <= 0 {
		return 0, 0, false
	}
	return credit, total, true
}

// wbAnyFloat tolerates the numeric shapes upstream emits (float64 from
// encoding/json, json.Number, int64, or a numeric string).

// wbAnyFloat tolerates the numeric shapes upstream emits (float64 from
// encoding/json, json.Number, int64, or a numeric string).
func wbAnyFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func wbAnyInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i)
		}
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err == nil {
			return i
		}
	}
	return 0
}

// usageMapFromCompletion extracts the raw usage object from an aggregated
// chat.completion payload (nil when absent).
