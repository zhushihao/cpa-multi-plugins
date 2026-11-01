// usage.go implements the UsagePlugin capability: the host calls handleUsage
// after every request with a canonical record, and we forward to CPAMP. The
// legacy publishUsage path is kept for hosts without UsagePlugin wiring.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleUsage is the UsagePlugin entry point. The host calls this after every
// request with the canonical usage record (it also records to its own
// DefaultManager, so we don't need to). Our job is just to forward to CPAMP.
//
// v0.7.0 compliance: this replaces the old pattern where each executor path
// called publishUsage directly (which both skipped the host audit and forced
// every call site to remember to publish).
func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	// Only forward workbuddy's own records; the host will route other plugins'
	// usage to their own UsagePlugin.
	if record.Provider != "" && record.Provider != providerName {
		return okEnvelope(map[string]any{"forwarded": false})
	}
	detail := usage.Detail{
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		ReasoningTokens:     record.Detail.ReasoningTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
	}
	started := record.RequestedAt
	if started.IsZero() {
		started = time.Now().Add(-record.Latency)
	}
	// v0.9.42: local per-credential ledger feeding the credential-card usage
	// summary in the host's local credential management (note field). Async —
	// the host's usage pump must never block on note-write RPCs.
	usageLedgerObserve(record.AuthIndex, record.AuthID, detail.TotalTokens, record.Failed, started)
	go usageNoteRefreshSoon(record.AuthIndex, record.AuthID)
	forwardUsageToCPAMP(
		record.Alias,
		record.Model,
		record.AuthID,
		started,
		detail,
		record.Failed,
		record.Failure.StatusCode,
		record.Failure.Body,
	)
	return okEnvelope(map[string]any{"forwarded": true})
}

// publishUsage is kept for backward compatibility with existing call sites
// inside the executor. After v0.7.0 the host calls UsagePlugin.HandleUsage
// itself after every request, so executor-side publish is redundant. We
// forward to CPAMP here too so that hosts WITHOUT UsagePlugin wiring (older
// CPA builds) still emit usage; hosts with the wiring will trigger HandleUsage
// separately, but forwardUsageToCPAMP is idempotent at the CPAMP ingestion
// layer (NDJSON import dedups on timestamp+auth+model+total_tokens).
//
// authID is the CPAMP attribution id (the account UID); failed attempts carry
// no usage map, so no routing-cost identity is needed here.
func publishUsage(requestedModel, upstreamModel, authID string, started time.Time, detail usage.Detail, failed bool, statusCode int, errBody string) {
	publishUsageWithUsage(requestedModel, upstreamModel, authID, "", started, detail, nil, failed, statusCode, errBody)
}

// publishUsageWithUsage is publishUsage plus the raw upstream `usage` object,
// which lets the success path record the observed credit cost for the
// cost-tier ledger (usage.credit exists only in the raw object; the
// canonicalized usage.Detail drops it).
//
// cpampAuthID and ledgerAuthID are deliberately DISTINCT identities:
//   - cpampAuthID is the account UID the CPAMP monitoring pipeline attributes
//     usage to (unchanged contract);
//   - ledgerAuthID is the host auth record ID the scheduler can address, which
//     is what the cost ledger must be keyed by (review S1). Passing "" means
//     "no routable identity" — such an observation is skipped rather than
//     misfiled under a UID no SchedulerPick candidate can ever match.
func publishUsageWithUsage(requestedModel, upstreamModel, cpampAuthID, ledgerAuthID string, started time.Time, detail usage.Detail, usageMap map[string]any, failed bool, statusCode int, errBody string) {
	if !failed && usageMap != nil {
		wbObserveModelCost(ledgerAuthID, requestedModel, usageMap)
	}
	model := strings.TrimSpace(upstreamModel)
	if model == "" {
		model = strings.TrimSpace(requestedModel)
	}
	alias := strings.TrimSpace(requestedModel)
	if alias == "" {
		alias = model
	}
	// Fire-and-forget so the executor hot path never blocks on the CPAMP
	// round-trip. handleUsage (the host-driven path) is synchronous because the
	// host already runs it on its own goroutine after the request completes.
	go forwardUsageToCPAMP(alias, model, cpampAuthID, started, normalizeUsageDetail(detail), failed, statusCode, errBody)
}

// wbObserveModelCost feeds one measured (account, model) cost sample into the
// routing pool's ledger. authID is the host auth record ID (the pool's key);
// requestedModel is the client-requested name — the ledger canonicalizes it
// with wbCostModelKey so the schedule-time lookup finds the row. Missing
// identity, missing credit or non-positive token totals are "no observation",
// never a zero-cost sample (otherwise free-tier would be asserted from an
// absence of data).
func wbObserveModelCost(authID, requestedModel string, usageMap map[string]any) {
	if strings.TrimSpace(authID) == "" || strings.TrimSpace(requestedModel) == "" {
		return
	}
	credit, total, ok := wbUsageCreditTotal(usageMap)
	if !ok {
		return
	}
	wbRouterPool.noteCost(authID, requestedModel, credit, total)
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
func usageMapFromCompletion(payload []byte) map[string]any {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return nil
	}
	m, _ := obj["usage"].(map[string]any)
	return m
}

// forwardUsageToCPAMP POSTs one NDJSON line to CPAMP usage/import.
// Silent on misconfig / network errors — never blocks chat.
//
// v0.7.0: routed via host.http.do so the call is captured by request-log and
// uses host transport policy (proxy, timeout). Was: raw http.Client.
func forwardUsageToCPAMP(alias, model, authID string, started time.Time, detail usage.Detail, failed bool, statusCode int, errBody string) {
	usageReportMu.RLock()
	url := strings.TrimSpace(usageReportURL)
	key := strings.TrimSpace(usageReportKey)
	usageReportMu.RUnlock()
	if url == "" || key == "" {
		return
	}
	ts := started
	if ts.IsZero() {
		ts = time.Now()
	}
	latencyMs := int64(0)
	if !started.IsZero() {
		latencyMs = time.Since(started).Milliseconds()
		if latencyMs < 0 {
			latencyMs = 0
		}
	}
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	failBody := ""
	failCode := 200
	if failed {
		failCode = statusCode
		if failCode <= 0 {
			failCode = 502
		}
		failBody = truncate(redactSecrets(errBody), 512)
	}
	payload := map[string]any{
		"timestamp":     ts.UTC().Format(time.RFC3339Nano),
		"latency_ms":    latencyMs,
		"source":        "workbuddy",
		"auth_index":    strings.TrimSpace(authID),
		"provider":      providerName,
		"model":         model,
		"alias":         alias,
		"endpoint":      "POST /v1/chat/completions",
		"auth_type":     "oauth",
		"executor_type": "workbuddy",
		"generate":      true,
		"failed":        failed,
		"tokens": map[string]any{
			"input_tokens":          detail.InputTokens,
			"output_tokens":         detail.OutputTokens,
			"reasoning_tokens":      detail.ReasoningTokens,
			"cached_tokens":         detail.CachedTokens,
			"cache_read_tokens":     detail.CacheReadTokens,
			"cache_creation_tokens": detail.CacheCreationTokens,
			"total_tokens":          total,
		},
		"fail": map[string]any{
			"status_code": failCode,
			"body":        failBody,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	body = append(body, '\n')
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := hostHTTPDo(req)
	if err != nil {
		return
	}
	_ = resp.Body
}

func normalizeUsageDetail(d usage.Detail) usage.Detail {
	if d.TotalTokens == 0 {
		if total := d.InputTokens + d.OutputTokens + d.ReasoningTokens; total > 0 {
			d.TotalTokens = total
		}
	}
	return d
}

// usageDetailFromMap converts an OpenAI-style "usage" JSON object into a
// usage.Detail, tolerating both snake_case naming and numeric jitter.
func usageDetailFromMap(m map[string]any) usage.Detail {
	if len(m) == 0 {
		return usage.Detail{}
	}
	num := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				switch n := v.(type) {
				case float64:
					return int64(n)
				case int64:
					return n
				case json.Number:
					i, _ := n.Int64()
					return i
				}
			}
		}
		return 0
	}
	d := usage.Detail{
		InputTokens:     num("prompt_tokens", "input_tokens"),
		OutputTokens:    num("completion_tokens", "output_tokens"),
		TotalTokens:     num("total_tokens"),
		CachedTokens:    num("cached_tokens"),
		CacheReadTokens: num("cache_read_input_tokens"),
	}
	if ct, ok := m["completion_tokens_details"].(map[string]any); ok {
		if v, ok2 := ct["reasoning_tokens"].(float64); ok2 {
			d.ReasoningTokens = int64(v)
		}
	}
	return d
}

// usageDetailFromCompletion extracts the usage block from an aggregated
// non-streaming chat.completion payload.
func usageDetailFromCompletion(payload []byte) usage.Detail {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return usage.Detail{}
	}
	m, _ := obj["usage"].(map[string]any)
	return usageDetailFromMap(m)
}

// modelFromCompletionPayload extracts the echoed "model" field from a folded
// chat.completion payload (empty when absent). Feeds noteLearnedRealModel —
// the concrete upstream model that served a tier-alias request.
func modelFromCompletionPayload(payload []byte) string {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return ""
	}
	s, _ := obj["model"].(string)
	return s
}

// sseUsageCollector scans upstream SSE chunks and keeps the last "usage"
// object seen (CodeBuddy emits it on the terminal chunk).
// v0.9.25: also keeps the last non-empty "model" echo — the concrete upstream
// model that served the request — so tier aliases can learn their real
// backing model (noteLearnedRealModel, see models.go).
type sseUsageCollector struct {
	last      map[string]any
	respModel string
}

func (c *sseUsageCollector) feed(rawJSON string) {
	var chunk map[string]any
	if json.Unmarshal([]byte(rawJSON), &chunk) != nil {
		return
	}
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		c.last = u
	}
	if v, ok := chunk["model"].(string); ok && v != "" {
		c.respModel = v
	}
}

func (c *sseUsageCollector) detail() usage.Detail {
	return usageDetailFromMap(c.last)
}
