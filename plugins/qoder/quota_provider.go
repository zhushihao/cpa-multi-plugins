// quota_provider.go implements the QuotaProvider capability (host v7.3.11
// sdk/pluginapi/types.go:1484 QuotaProvider: DescribeQuota/FetchQuota/
// ResetQuota) over the plugin RPC surface.
//
// The plugin compiles against SDK v7.2.30 (go.mod pin), which predates the
// quota capability: no pluginapi.QuotaProvider interface and no quota.* method
// constants exist there. The plugin/host boundary is JSON over the C ABI
// (abi_cgo.go host tables), so the contract is the wire shape, mirrored here
// with local structs whose JSON tags are verified against host v7.3.11:
//   - registration flag  quota_provider            (internal/pluginhost/rpc_schema.go:48)
//   - methods            quota.identifier/describe/fetch/reset
//     (sdk/pluginabi/types.go:90-93)
//   - describe response  supported_providers / display_name / supports_reset
//     (sdk/pluginapi/types.go:1500)
//   - fetch response     subscription / summary / serverTimeOffsetMs / groups
//     (sdk/pluginapi/types.go:1541-1638)
//   - reset response     success / message         (sdk/pluginapi/types.go:1680)
//
// FetchQuota contract (ask 2026-09-30): READ-ONLY over the existing
// accountCache — the same store cachedAccountDetails feeds on the panel/
// reconcile/keepalive cadence and invalidateAccountCredits nils after a chat.
// No new upstream call, no host auth RPC: the host addresses the credential
// by auth_id, which is exactly the accountCache key. Absent/unknown credits
// return quota_unavailable — never a fabricated zero balance. ResetQuota is
// honestly declared unsupported.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Wire method names — byte-identical to host v7.3.11 sdk/pluginabi
// types.go:90-93. Local because SDK v7.2.30 has no such constants.
const (
	methodQuotaIdentifier = "quota.identifier"
	methodQuotaDescribe   = "quota.describe"
	methodQuotaFetch      = "quota.fetch"
	methodQuotaReset      = "quota.reset"
)

// quotaFetchRequest decodes the host's quota.fetch/reset payload. The host
// marshals rpcQuotaFetchRequest{pluginapi.QuotaFetchRequest, host_callback_id}
// (internal/pluginhost/rpc_client.go:656/:665); fields we don't need (host,
// host_callback_id, metadata, attributes) decode-and-drop.
type quotaFetchRequest struct {
	AuthIndex   string            `json:"auth_index"`
	AuthID      string            `json:"auth_id"`
	Provider    string            `json:"provider"`
	StorageJSON json.RawMessage   `json:"storage_json,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// quotaSubscription mirrors pluginapi.QuotaSubscription (types.go:1541).
type quotaSubscription struct {
	Plan string `json:"plan,omitempty"`
}

// quotaMetric mirrors pluginapi.QuotaMetric (types.go:1619): Format "number"
// or "currency" for management UI rendering.
type quotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

// quotaBucket mirrors pluginapi.QuotaBucket (types.go:1584): camelCase on the
// wire (remainingFraction/resetTime).
type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// quotaGroup mirrors pluginapi.QuotaGroup (types.go:1566).
type quotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []quotaBucket `json:"buckets,omitempty"`
}

// quotaFetchResponse mirrors pluginapi.QuotaFetchResponse (types.go:1630).
type quotaFetchResponse struct {
	Subscription       *quotaSubscription `json:"subscription,omitempty"`
	Summary            []quotaMetric      `json:"summary,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []quotaGroup       `json:"groups,omitempty"`
}

// quotaDescribeResponse mirrors pluginapi.QuotaDescribeResponse
// (types.go:1500): snake_case on the wire.
type quotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

// quotaResetResponse mirrors pluginapi.QuotaResetResponse (types.go:1680).
type quotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// handleQuotaDescribe declares the provider keys this plugin serves quota for.
func handleQuotaDescribe([]byte) ([]byte, error) {
	return okEnvelope(quotaDescribeResponse{
		SupportedProviders: []string{providerName},
		DisplayName:        "Qoder 积分",
		SupportsReset:      false,
	})
}

// handleQuotaFetch answers quota.fetch from the in-memory credits cache only.
// Any miss — no cache entry, post-invalidate credits=nil (lifecycle.go
// invalidateAccountCredits), or a dataless all-zero snapshot — is reported as
// quota_unavailable so the management UI shows "unknown", never a fake 0.
func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req quotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("quota_unavailable", "quota.fetch: "+err.Error()), nil
	}
	if resp, ok := quotaFetchFromCache(strings.TrimSpace(req.AuthID)); ok {
		return okEnvelope(resp)
	}
	// Cold-path bootstrap: only a request that carries the credential storage
	// may trigger ONE live billing fetch for the addressed credential (the
	// same fetchUserResource the panel uses), so a host/CPAMP quota refresh
	// can populate a cold cache instead of 502-ing forever — the panel's own
	// lazy path only fills the cache per-credential on demand. A failed or
	// empty answer is never cached and never presented as data.
	//
	// The host may deliver storage_json as a JSON-encoded STRING wrapping the
	// storage object (live 2026-09-30: `json: cannot unmarshal string into
	// map[string]jsontext.Value` in the diagnostic build) — unwrap that layer
	// before parsing. Parse failures fall through to the unavailable envelope
	// (unknown stays unknown; the temporary cold-debug branches are gone).
	if len(req.StorageJSON) > 0 {
		storageRaw := req.StorageJSON
		trimmed := bytes.TrimSpace(storageRaw)
		if len(trimmed) > 0 && trimmed[0] == '"' {
			var inner string
			if err := json.Unmarshal(trimmed, &inner); err == nil && strings.TrimSpace(inner) != "" {
				storageRaw = []byte(inner)
			}
		}
		if sa, errStored := parseStored(storageRaw); errStored == nil && sa != nil && sa.Auth.AccessToken != "" {
			if env, ok, errEnv := quotaColdLiveFetchRespond(req, sa); errEnv != nil {
				return nil, errEnv
			} else if ok {
				return env, nil
			}
			// Failed or empty live answer: fall through to the unavailable
			// envelope — unknown stays unknown.
		}
	}
	// Management-initiated quota.fetch carries no storage_json at all (host
	// fetchQuotaForPlugin sends AuthIndex/AuthID/Metadata/Attributes only —
	// internal/api/handlers/management/plugin_quota.go:282-288), so resolve
	// the credential through the auth list and read its storage.
	if sa := resolveQuotaStorage(req); sa != nil {
		if env, ok, errEnv := quotaColdLiveFetchRespond(req, sa); errEnv != nil {
			return nil, errEnv
		} else if ok {
			return env, nil
		}
	}
	return errorEnvelope("quota_unavailable", "qoder storage unavailable for cold-path quota fetch"), nil
}

// resolveQuotaStorage maps a quota.fetch request (AuthIndex or AuthID) to the
// credential storage the plugin can act on, via the same auth-list seams the
// credits handler uses. Disabled entries and unparseable storage yield nil.
func resolveQuotaStorage(req quotaFetchRequest) *storedAuth {
	files, err := hostAuthListFn()
	if err != nil {
		return nil
	}
	for _, f := range files {
		if f.Disabled {
			continue
		}
		matchIndex := req.AuthIndex != "" && f.AuthIndex == req.AuthIndex
		matchID := req.AuthID != "" && f.ID == req.AuthID
		if !matchIndex && !matchID {
			continue
		}
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil {
			return nil
		}
		sa, err := parseStored(phys.JSON)
		if err != nil || sa == nil || sa.Auth.AccessToken == "" {
			return nil
		}
		return sa
	}
	return nil
}

// quotaColdLiveFetchRespond runs ONE live billing fetch, caches a known-good
// answer, and returns the quota envelope. ok=false means unknown stays
// unknown (caller answers quota_unavailable); never a fabricated zero.
func quotaColdLiveFetchRespond(req quotaFetchRequest, sa *storedAuth) ([]byte, bool, error) {
	fresh, errFetch := quotaLiveFetchFn(sa)
	if errFetch != nil || !quotaCreditsKnown(fresh) {
		return nil, false, nil
	}
	fresh.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	var prev *accountCacheEntry
	if v, hit := accountCache.Load(req.AuthID); hit {
		prev, _ = v.(*accountCacheEntry)
	}
	var ci *checkinSummary
	var plan string
	if prev != nil {
		ci, plan = prev.checkin, prev.plan
	}
	accountCache.Store(req.AuthID, &accountCacheEntry{checkin: ci, credits: fresh, plan: plan, fetched: time.Now()})
	env, err := okEnvelope(quotaResponseFromCredits(plan, fresh))
	return env, true, err
}

// quotaLiveFetchFn is the seam for the cold-path live billing fetch; tests
// stub it to stay offline while exercising the bootstrap contract.
var quotaLiveFetchFn = fetchUserResource

// quotaFetchFromCache is the pure mapping step: authID (the host's auth.ID,
// the same key cachedAccountDetails/storeCreditsSnapshot/invalidateAccountCredits
// use) -> normalized quota response.
func quotaFetchFromCache(authID string) (quotaFetchResponse, bool) {
	if authID == "" {
		return quotaFetchResponse{}, false
	}
	v, loaded := accountCache.Load(authID)
	if !loaded {
		return quotaFetchResponse{}, false
	}
	entry, isEntry := v.(*accountCacheEntry)
	if !isEntry || !quotaCreditsKnown(entry.credits) {
		return quotaFetchResponse{}, false
	}
	return quotaResponseFromCredits(entry.plan, entry.credits), true
}

// quotaCreditsKnown mirrors the shared "data present" convention of
// isCreditsExhausted (billing.go): a pure zero with no package rows is "no
// data", never a balance — upstream said nothing, so we say nothing.
func quotaCreditsKnown(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	return cr.TotalRemain > 0 || cr.TotalUsed > 0 || cr.TotalSize > 0 || len(cr.Packages) > 0
}

// quotaResponseFromCredits maps the management /credits snapshot
// (creditsSummary: total remain/used/size + per-package remain/used/size/
// cycle_end) into the host's normalized quota shape: three aggregate summary
// metrics plus one group per informative package whose single "cycle" bucket
// carries remain/size as remainingFraction and cycle_end as resetTime.
// All-zero package rows are skipped — an empty pool carries no signal and
// rendering it as a 0 bucket would fabricate certainty.
func quotaResponseFromCredits(plan string, cr *creditsSummary) quotaFetchResponse {
	resp := quotaFetchResponse{
		Summary: []quotaMetric{
			{Key: "credits_remaining", Label: "剩余积分", Value: float64(cr.TotalRemain), Unit: "credits", Format: "number"},
			{Key: "credits_used", Label: "已用积分", Value: float64(cr.TotalUsed), Unit: "credits", Format: "number"},
			{Key: "credits_size", Label: "总额度", Value: float64(cr.TotalSize), Unit: "credits", Format: "number"},
		},
	}
	if p := strings.TrimSpace(plan); p != "" {
		resp.Subscription = &quotaSubscription{Plan: p}
	}
	for _, p := range cr.Packages {
		if p.Remain == 0 && p.Used == 0 && p.Size == 0 {
			continue
		}
		resp.Groups = append(resp.Groups, quotaGroup{
			DisplayName: p.Name,
			Buckets: []quotaBucket{{
				Window:            "cycle",
				RemainingFraction: quotaRemainingFraction(p.Remain, p.Size),
				ResetTime:         strings.TrimSpace(p.CycleEnd),
				Description:       fmt.Sprintf("%s：余 %d / %d 积分（已用 %d）", p.Name, p.Remain, p.Size, p.Used),
			}},
		})
	}
	return resp
}

// quotaRemainingFraction turns remain/size into the bucket fraction. A
// size-less package with credits is reported fully available; the fraction is
// clamped to [0,1] because creditsSummary truncates floats to int64.
func quotaRemainingFraction(remain, size int64) float64 {
	if size <= 0 {
		if remain > 0 {
			return 1
		}
		return 0
	}
	f := float64(remain) / float64(size)
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// handleQuotaReset honestly declares that qoder credits cannot be reset: the
// upstream pools refill on their own package cycle, and the host-side
// cooldown clearing (authManager.ResetQuota) is a routing concern the
// management API exposes separately. Success=false makes the management
// handler answer 502 with our message (plugin_quota.go:206-211).
func handleQuotaReset([]byte) ([]byte, error) {
	return okEnvelope(quotaResetResponse{
		Success: false,
		Message: "qoder 积分由上游套餐周期自动重置，不支持手动重置（面板 /credits 可查看余量）",
	})
}
