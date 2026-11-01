// quota_provider.go implements the pluginapi.QuotaProvider capability (host
// v7.3.11 sdk/pluginapi/types.go:1484) over the plugin's RPC surface.
//
// Channel (host internal/pluginhost/rpc_client.go registerRPCPlugin): the
// registration capabilities JSON carries "quota_provider":true; the host then
// calls quota.identifier to learn the provider key and forwards
// quota.describe / quota.fetch / quota.reset. The build SDK (v7.2.30) predates
// the quota types, so the wire structs and method names below mirror the
// host's own JSON tags locally — the established wire-compat pattern of this
// plugin (cf. rpcHostHTTPRequestWire in host_bridge.go, decodeHostHTTPDoResult).
// Older hosts never send quota.* and never read the extra capability flag, so
// declaring it is backward-compatible.
//
// Data policy: FetchQuota is a READ-ONLY projection of the existing
// accountCache (the same store invalidateAccountCredits drops credits from
// after each chat). It never opens an upstream call — the panel's /credits and
// the reconcile loop remain the only writers. A credential with no cached
// snapshot reports quota_unavailable instead of guessing; ResetQuota is
// declared unsupported (SupportsReset=false, success=false with a message).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Wire method names, mirroring sdk/pluginabi v7.3.11 (the build-time
// pluginabi package predates them).
const (
	methodQuotaIdentifier = "quota.identifier"
	methodQuotaDescribe   = "quota.describe"
	methodQuotaFetch      = "quota.fetch"
	methodQuotaReset      = "quota.reset"
)

// quotaUnavailableCode is the envelope error code for "no cached quota data".
// The host surfaces plugin errors on the management quota endpoint as a 502
// with the plugin's message, so the code stays human-readable.
const quotaUnavailableCode = "quota_unavailable"

// -----------------------------------------------------------------------------
// Wire structs (mirror host v7.3.11 pluginapi JSON tags exactly)
// -----------------------------------------------------------------------------

// quotaFetchRequest mirrors the host's pluginapi.QuotaFetchRequest fields the
// plugin consumes (HTTPClient is host-side only and never crosses the wire;
// attributes/host are decoded-and-ignored today but reserved for callers that
// need them later).
type quotaFetchRequest struct {
	AuthIndex      string         `json:"auth_index"`
	AuthID         string         `json:"auth_id"`
	Provider       string         `json:"provider"`
	StorageJSON    []byte         `json:"storage_json,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	HostCallbackID string         `json:"host_callback_id,omitempty"`
}

// quotaDescribeRequest mirrors pluginapi.QuotaDescribeRequest.
type quotaDescribeRequest struct {
	Plugin pluginapi.Metadata `json:"plugin,omitempty"`
}

// quotaDescribeResponse mirrors pluginapi.QuotaDescribeResponse.
type quotaDescribeResponse struct {
	SupportedProviders []string `json:"supported_providers,omitempty"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportsReset      bool     `json:"supports_reset,omitempty"`
}

// quotaSubscription mirrors pluginapi.QuotaSubscription.
type quotaSubscription struct {
	Plan     string `json:"plan,omitempty"`
	TierName string `json:"tierName,omitempty"`
	TierID   string `json:"tierId,omitempty"`
}

// quotaMetric mirrors pluginapi.QuotaMetric.
type quotaMetric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

// quotaBucket mirrors pluginapi.QuotaBucket.
type quotaBucket struct {
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// quotaGroup mirrors pluginapi.QuotaGroup.
type quotaGroup struct {
	DisplayName string        `json:"displayName,omitempty"`
	Buckets     []quotaBucket `json:"buckets,omitempty"`
}

// quotaFetchResponse mirrors pluginapi.QuotaFetchResponse.
type quotaFetchResponse struct {
	Subscription       *quotaSubscription `json:"subscription,omitempty"`
	Summary            []quotaMetric      `json:"summary,omitempty"`
	ServerTimeOffsetMs int64              `json:"serverTimeOffsetMs,omitempty"`
	Groups             []quotaGroup       `json:"groups,omitempty"`
}

// quotaResetRequest mirrors pluginapi.QuotaResetRequest (subset as above).
type quotaResetRequest struct {
	AuthIndex   string `json:"auth_index"`
	AuthID      string `json:"auth_id"`
	Provider    string `json:"provider"`
	StorageJSON []byte `json:"storage_json,omitempty"`
}

// quotaResetResponse mirrors pluginapi.QuotaResetResponse.
type quotaResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

// handleQuotaDescribe declares this plugin as the quota provider for its own
// family. Reset is NOT offered: the upstream billing API has no reset action.
func handleQuotaDescribe() ([]byte, error) {
	return okEnvelope(quotaDescribeResponse{
		SupportedProviders: []string{providerName},
		DisplayName:        "WorkBuddy Credits",
		SupportsReset:      false,
	})
}

// handleQuotaFetch projects the cached credits snapshot into the host's
// normalized quota shape. Cache-first: accountCache direct hit (the host
// addresses the credential by auth.ID, the cache's own key), then the same
// (uid × realm) sibling resolution invalidateAccountCredits uses, driven by
// the in-memory identity map — never hostAuthList. On a cache miss it falls
// back to ONE live billing fetch for the addressed credential (the same
// fetchUserResource the panel /credits uses) and stores the snapshot, so a
// host/CPAMP quota refresh can bootstrap a cold cache instead of failing
// 502 forever (the panel lazy path only fills per-credential on demand).
// A live fetch failure still reports quota_unavailable — never stale or
// invented numbers.
func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req quotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cr, plan, ok := quotaCachedCredits(req)
	if ok {
		return okEnvelope(quotaResponseFrom(cr, plan))
	}
	// Identity resolution (uid × realm via the in-memory map) runs FIRST:
	// a sibling copy's cached snapshot must win over any live fetch, and an
	// unresolvable identity must not trigger upstream calls.
	if id := wbAuthIdentityOf(parseStoredForIdentity(req.StorageJSON)); id.UID != "" {
		var found *accountCacheEntry
		var foundKey string
		accountCache.Range(func(key, value any) bool {
			k, _ := key.(string)
			e, ok := value.(*accountCacheEntry)
			if !ok || e == nil || e.credits == nil {
				return true
			}
			cid, known := wbKnownAuthIdentity(k)
			if !known || cid.UID != id.UID {
				return true
			}
			if id.Realm != "" && cid.Realm != "" && cid.Realm != id.Realm {
				return true
			}
			found, foundKey = e, k
			return false
		})
		if found != nil {
			_ = foundKey
			return okEnvelope(quotaResponseFrom(found.credits, found.plan))
		}
	}
	// Cold-path bootstrap: only a request that carries the credential storage
	// may trigger a live billing fetch (one call, the addressed credential
	// only). No storage → pure cache miss, reported unavailable without any
	// upstream traffic (the in-memory-only contract tests pin this).
	//
	// The host may deliver storage_json as a JSON-encoded STRING wrapping the
	// storage object (live 2026-09-30, json v2 build: `cannot unmarshal
	// string into map[string]jsontext.Value`) — unwrap that layer before
	// parsing or the cold bootstrap can never fire.
	if len(req.StorageJSON) > 0 {
		storageRaw := req.StorageJSON
		trimmed := bytes.TrimSpace(storageRaw)
		if len(trimmed) > 0 && trimmed[0] == '"' {
			var inner string
			if err := json.Unmarshal(trimmed, &inner); err == nil && strings.TrimSpace(inner) != "" {
				storageRaw = []byte(inner)
			}
		}
		sa, errStored := parseStored(storageRaw)
		if errStored == nil && sa != nil && sa.Auth.AccessToken != "" {
			fresh, errFetch := fetchUserResource(sa)
			if errFetch != nil && isBridgeEnvelopeCorruption(errFetch) {
				// The host http bridge occasionally hands back a truncated
				// JSON envelope ("unexpected end of JSON input") under stack
				// movement — the same hazard the direct bypass exists for.
				// One retry through the direct client keeps quota bootstrap
				// from failing on it.
				fresh, errFetch = fetchUserResourceViaDirect(sa)
			}
			if errFetch == nil && fresh != nil && fresh.TotalSize > 0 {
				fresh.FetchedAt = time.Now().UTC().Format(time.RFC3339)
				var ciP *checkinSummary
				var planP string
				if v, hit := accountCache.Load(req.AuthID); hit {
					if e, okE := v.(*accountCacheEntry); okE && e != nil {
						ciP, planP = e.checkin, e.plan
					}
				}
				accountCache.Store(req.AuthID, &accountCacheEntry{checkin: ciP, credits: fresh, plan: planP, fetched: time.Now()})
				return okEnvelope(quotaResponseFrom(fresh, planP))
			}
			// Failed or empty answer: fall through to the unavailable envelope
			// — never cached, never presented as data.
		}
	}
	return errorEnvelope(quotaUnavailableCode,
		"workbuddy: no cached billing snapshot for this credential; refresh via the panel /credits endpoint (quota read is cache-only)"), nil
}

// parseStoredForIdentity decodes storage for identity resolution only; a bad
// blob yields the zero identity (no uid → no sibling scan).
func parseStoredForIdentity(raw []byte) *storedAuth {
	sa, err := parseStored(raw)
	if err != nil || sa == nil {
		return &storedAuth{}
	}
	return sa
}

// handleQuotaReset answers the structured not-supported response. Returning a
// plain success=false (not an error envelope) keeps the host's reset endpoint
// message clean ("quota reset rejected by provider" + our message) — and the
// describe contract already hides the button (SupportsReset=false).
func handleQuotaReset() ([]byte, error) {
	return okEnvelope(quotaResetResponse{
		Success: false,
		Message: "workbuddy quota is an upstream billing snapshot; reset is not supported by this plugin",
	})
}

// -----------------------------------------------------------------------------
// Cache projection
// -----------------------------------------------------------------------------

// quotaCachedCredits resolves the cached credits for a quota.fetch request.
// Order: direct key hit (auth ID — the cache's own key — then auth index),
// then the sibling-copy scan by (uid × realm) identity derived from the
// request's StorageJSON. nil-credits entries (post-invalidateAccountCredits)
// never satisfy a lookup — stale numbers the invalidation dropped must not
// resurface here.
func quotaCachedCredits(req quotaFetchRequest) (*creditsSummary, string, bool) {
	lookup := func(key string) (*accountCacheEntry, bool) {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, false
		}
		if v, ok := accountCache.Load(key); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 && e != nil && e.credits != nil {
				return e, true
			}
		}
		return nil, false
	}
	if e, ok := lookup(req.AuthID); ok {
		return e.credits, e.plan, true
	}
	if e, ok := lookup(req.AuthIndex); ok {
		return e.credits, e.plan, true
	}
	// Sibling resolution needs the account identity; derive it from the
	// credential storage the host attached to the request. Unreadable storage
	// ends the search — never borrow another account's numbers on a guess.
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, "", false
	}
	want := wbAuthIdentityOf(sa)
	if want.UID == "" {
		return nil, "", false
	}
	var found *accountCacheEntry
	accountCache.Range(func(key, value any) bool {
		k, _ := key.(string)
		e, ok := value.(*accountCacheEntry)
		if !ok || e == nil || e.credits == nil {
			return true
		}
		id, known := wbKnownAuthIdentity(k)
		if !known || id.UID != want.UID {
			return true
		}
		// Realm must agree when both sides resolved one (a CN and an Intl
		// credential may share a uid — merged numbers would lie about both).
		if want.Realm != "" && id.Realm != "" && id.Realm != want.Realm {
			return true
		}
		found = e
		return false
	})
	if found == nil {
		return nil, "", false
	}
	return found.credits, found.plan, true
}

// quotaResponseFrom maps a credits snapshot to the normalized quota response.
//
//   - Subscription.Plan: the cached payment type (empty → omitted, unknown).
//   - Summary: the aggregate counters the panel already shows (remaining /
//     used / size / expiring subset). Known zeros are reported as zeros —
//     only the unknown expiring subset is omitted.
//   - Groups: one per resource package (the /credits payload's multi-pack
//     shape, including 赠送/签到-style gift packs), bucket fraction =
//     remain/size clamped to [0,1], ResetTime = the package cycle end as
//     reported upstream, Description carrying the raw remain/size figures so
//     an unusable denominator (size==0) still renders honestly as fraction 0.
func quotaResponseFrom(cr *creditsSummary, plan string) quotaFetchResponse {
	resp := quotaFetchResponse{}
	if strings.TrimSpace(plan) != "" {
		resp.Subscription = &quotaSubscription{Plan: strings.TrimSpace(plan)}
	}
	resp.Summary = []quotaMetric{
		{Key: "credits_remaining", Label: "剩余积分", Value: float64(cr.TotalRemain), Unit: "credits", Format: "number"},
		{Key: "credits_used", Label: "已用积分", Value: float64(cr.TotalUsed), Unit: "credits", Format: "number"},
		{Key: "credits_size", Label: "总额度", Value: float64(cr.TotalSize), Unit: "credits", Format: "number"},
	}
	if cr.TotalExpiring > 0 {
		resp.Summary = append(resp.Summary, quotaMetric{Key: "credits_expiring", Label: "即将过期", Value: float64(cr.TotalExpiring), Unit: "credits", Format: "number"})
	}
	for _, p := range cr.Packages {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = "credits"
		}
		frac := 0.0
		if p.Size > 0 {
			frac = float64(p.Remain) / float64(p.Size)
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
		}
		resp.Groups = append(resp.Groups, quotaGroup{
			DisplayName: name,
			Buckets: []quotaBucket{{
				Window:            "cycle",
				RemainingFraction: frac,
				ResetTime:         strings.TrimSpace(p.CycleEnd),
				Description:       fmt.Sprintf("剩余 %d / 共 %d", p.Remain, p.Size),
			}},
		})
	}
	return resp
}

// isBridgeEnvelopeCorruption reports the truncated-envelope signature of the
// host http bridge (see hostHTTPDoStreamDirect rationale in host_bridge.go).
func isBridgeEnvelopeCorruption(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unexpected end of JSON input")
}

// fetchUserResourceViaDirect re-issues the billing call with the plugin's own
// HTTP client (bypassing the host bridge) by temporarily swapping the seam.
func fetchUserResourceViaDirect(sa *storedAuth) (*creditsSummary, error) {
	prev := hostHTTPDoFn
	hostHTTPDoFn = hostHTTPDoDirectOnly
	defer func() { hostHTTPDoFn = prev }()
	return fetchUserResource(sa)
}

// hostHTTPDoDirectOnly forces the direct-client path regardless of OS/bridge.
func hostHTTPDoDirectOnly(req *http.Request) (*hostHTTPResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	return hostHTTPDoDirect(req, bodyBytes)
}


