package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// quota_provider_test.go pins the pluginapi.QuotaProvider capability (host
// v7.3.11 sdk/pluginapi/types.go:1484) as spoken over the plugin RPC surface.
//
// Contract under test (host internal/pluginhost/rpc_client.go registerRPCPlugin
// + quota_provider.go callQuota*): the registration capabilities JSON carries
// "quota_provider":true, the host then probes quota.identifier and forwards
// quota.describe / quota.fetch / quota.reset. FetchQuota is READ-ONLY over the
// existing accountCache (the same store invalidateAccountCredits nils) — it
// must never open a new upstream call, and unknown/absent data must be
// reported as unavailable instead of guessed.

// quotaTestUpstreams points every billing base at recording httptest servers
// so a test fails loudly if the quota path attempts an upstream call. Returns
// a counter the caller asserts zero.
func quotaTestUpstreams(t *testing.T) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	t.Cleanup(srv.Close)
	restoreBase := setBillingBase(srv.URL)
	restoreGlobal := setBillingBaseGlobal(srv.URL)
	prevIntl := billingBaseIntl
	billingBaseIntl = srv.URL
	t.Cleanup(restoreBase)
	t.Cleanup(restoreGlobal)
	t.Cleanup(func() { billingBaseIntl = prevIntl })
	return &hits
}

// quotaTestNoHostAuth forbids host auth RPCs on the quota path: FetchQuota is
// a pure in-memory cache read (direct auth-ID key + in-memory identity scan).
func quotaTestNoHostAuth(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	prevList := hostAuthListFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		calls.Add(1)
		return nil, fmt.Errorf("quota path must not read the host auth store")
	}
	t.Cleanup(func() { hostAuthListFn = prevList })
	return &calls
}

// quotaFetchViaWire drives the quota.fetch RPC the way the host does
// (registerRPCPlugin forwards pluginapi.QuotaFetchRequest JSON) and decodes
// the envelope result.
func quotaFetchViaWire(t *testing.T, authID string, storage []byte) (quotaFetchResponse, bool, string) {
	t.Helper()
	req, err := json.Marshal(quotaFetchRequest{AuthIndex: authID, AuthID: authID, Provider: providerName, StorageJSON: storage})
	if err != nil {
		t.Fatalf("marshal quota.fetch request: %v", err)
	}
	out, err := handleMethod(methodQuotaFetch, req)
	if err != nil {
		t.Fatalf("quota.fetch handler error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("quota.fetch envelope decode: %v", err)
	}
	if !env.OK {
		code := ""
		if env.Error != nil {
			code = env.Error.Code
		}
		return quotaFetchResponse{}, false, code
	}
	var resp quotaFetchResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("quota.fetch result decode: %v", err)
	}
	return resp, true, ""
}

// quotaFetchViaWireStorage drives quota.fetch with host-attached credential
// storage (the real request shape when the host fills storage_json).
func quotaFetchViaWireStorage(t *testing.T, authID string, storage []byte) (quotaFetchResponse, bool, string) {
	t.Helper()
	req, err := json.Marshal(quotaFetchRequest{AuthIndex: authID, AuthID: authID, Provider: providerName, StorageJSON: storage})
	if err != nil {
		t.Fatalf("marshal quota.fetch request: %v", err)
	}
	out, err := handleMethod(methodQuotaFetch, req)
	if err != nil {
		t.Fatalf("quota.fetch handler error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("quota.fetch envelope decode: %v", err)
	}
	if !env.OK {
		code := ""
		if env.Error != nil {
			code = env.Error.Code
		}
		return quotaFetchResponse{}, false, code
	}
	var resp quotaFetchResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("quota.fetch result decode: %v", err)
	}
	return resp, true, ""
}

// quotaSampleCredits is the live multi-pack shape: 体验版 exhausted + a
// check-in gift pack inside the expiring window + two full grant packs (one
// without a known cycle end). remain 0+99+100+100=299, used 500+1=501,
// size 500+100+100+100=800.
func quotaSampleCredits() *creditsSummary {
	return &creditsSummary{
		TotalRemain:   299,
		TotalUsed:     501,
		TotalSize:     800,
		PackCount:     4,
		FetchedAt:     "2026-09-30T01:00:00Z",
		TotalExpiring: 99,
		Packages: []packageSummary{
			{Name: "体验版", Remain: 0, Used: 500, Size: 500, CycleStart: "2026-09-01 00:00:00", CycleEnd: "2026-09-30 23:59:59"},
			{Name: "签到奖励包", Remain: 99, Used: 1, Size: 100, CycleStart: "2026-09-15 00:00:00", CycleEnd: "2026-10-31 23:59:59"},
			{Name: "裂变包", Remain: 100, Used: 0, Size: 100, CycleStart: "2026-09-15 00:00:00", CycleEnd: "2026-10-31 23:59:59"},
			{Name: "专家加油包", Remain: 100, Used: 0, Size: 100},
		},
	}
}

func TestQuotaFetchMapsCachedCreditsSample(t *testing.T) {
	hits := quotaTestUpstreams(t)
	noAuth := quotaTestNoHostAuth(t)
	accountCache.Store("wb-quota-1", &accountCacheEntry{credits: quotaSampleCredits(), plan: "FREE", fetched: time.Now()})
	t.Cleanup(func() { accountCache.Delete("wb-quota-1") })

	resp, ok, _ := quotaFetchViaWire(t, "wb-quota-1", nil)
	if !ok {
		t.Fatalf("cached credits must map to a quota response, got unavailable")
	}
	if hits.Load() != 0 || noAuth.Load() != 0 {
		t.Fatalf("quota.fetch must be read-only (upstream hits=%d host-auth calls=%d)", hits.Load(), noAuth.Load())
	}

	// Subscription: the cached plan is the plan tier.
	if resp.Subscription == nil {
		t.Fatalf("subscription missing for a cached plan")
	}
	if resp.Subscription.Plan != "FREE" {
		t.Errorf("subscription.plan = %q, want FREE", resp.Subscription.Plan)
	}

	// Summary: the three aggregate counters plus the expiring subset.
	want := map[string]float64{
		"credits_remaining": 299,
		"credits_used":      501,
		"credits_size":      800,
		"credits_expiring":  99,
	}
	got := map[string]quotaMetric{}
	for _, m := range resp.Summary {
		if m.Format != "number" || m.Unit != "credits" {
			t.Errorf("metric %s: format/unit = %q/%q, want number/credits", m.Key, m.Format, m.Unit)
		}
		if _, dup := got[m.Key]; dup {
			t.Errorf("duplicate summary metric %s", m.Key)
		}
		got[m.Key] = m
	}
	for key, val := range want {
		m, ok := got[key]
		if !ok {
			t.Errorf("summary metric %s missing", key)
			continue
		}
		if m.Value != val {
			t.Errorf("summary metric %s = %v, want %v", key, m.Value, val)
		}
	}

	// Groups: one per package, in package order; the bucket fraction is
	// remain/size with the cycle end carried through.
	if len(resp.Groups) != 4 {
		t.Fatalf("groups = %d, want one per package (4)", len(resp.Groups))
	}
	type groupWant struct {
		name   string
		frac   float64
		reset  string
		hasRem bool // description carries "remain/size"
	}
	wants := []groupWant{
		{"体验版", 0, "2026-09-30 23:59:59", true},
		{"签到奖励包", 0.99, "2026-10-31 23:59:59", true},
		{"裂变包", 1, "2026-10-31 23:59:59", true},
		{"专家加油包", 1, "", true},
	}
	for i, w := range wants {
		g := resp.Groups[i]
		if g.DisplayName != w.name {
			t.Errorf("group[%d].displayName = %q, want %q", i, g.DisplayName, w.name)
		}
		if len(g.Buckets) != 1 {
			t.Errorf("group[%d] buckets = %d, want 1", i, len(g.Buckets))
			continue
		}
		b := g.Buckets[0]
		if b.Window != "cycle" {
			t.Errorf("group[%d] bucket window = %q, want cycle", i, b.Window)
		}
		if b.RemainingFraction != w.frac {
			t.Errorf("group[%d] remainingFraction = %v, want %v", i, b.RemainingFraction, w.frac)
		}
		if b.ResetTime != w.reset {
			t.Errorf("group[%d] resetTime = %q, want %q", i, b.ResetTime, w.reset)
		}
		if w.hasRem && !strings.Contains(b.Description, "99") && !strings.Contains(b.Description, "100") && !strings.Contains(b.Description, "500") {
			t.Errorf("group[%d] description %q should carry the raw remain/size figures", i, b.Description)
		}
	}
}

func TestQuotaFetchUnknownReportsUnavailable(t *testing.T) {
	hits := quotaTestUpstreams(t)
	noAuth := quotaTestNoHostAuth(t)

	// 1) No cache entry at all.
	_, ok, code := quotaFetchViaWire(t, "wb-quota-none", nil)
	if ok {
		t.Fatalf("unknown credential must not return a quota response")
	}
	if code != "quota_unavailable" {
		t.Errorf("unavailable code = %q, want quota_unavailable", code)
	}

	// 2) invalidateAccountCredits semantics: the entry survives with
	// credits=nil after a chat — still unavailable, never stale numbers.
	accountCache.Store("wb-quota-nil", &accountCacheEntry{credits: nil, plan: "FREE", fetched: time.Now()})
	t.Cleanup(func() { accountCache.Delete("wb-quota-nil") })
	_, ok, code = quotaFetchViaWire(t, "wb-quota-nil", nil)
	if ok {
		t.Fatalf("credits=nil (post-invalidate) must be unavailable")
	}
	if code != "quota_unavailable" {
		t.Errorf("nil-credits code = %q, want quota_unavailable", code)
	}

	if hits.Load() != 0 || noAuth.Load() != 0 {
		t.Fatalf("unavailable path must stay read-only (upstream hits=%d host-auth calls=%d)", hits.Load(), noAuth.Load())
	}
}

// TestQuotaFetchColdPathLiveFetchesOnce covers the bootstrap: no cache, no
// sibling identity → one fetchUserResource for the addressed credential; a
// real (non-empty) answer is cached under the addressed key and returned.
func TestQuotaFetchColdPathLiveFetchesOnce(t *testing.T) {
	noAuth := quotaTestNoHostAuth(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"Response":{"Data":{"TotalCount":1,"TotalDosage":100,"Accounts":[{"PackageName":"测试包","CapacityRemain":100,"CapacityUsed":0,"CapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0,"CycleCapacitySize":100,"CycleStartTime":"2026-09-01 00:00:00","CycleEndTime":"2026-10-01 00:00:00"}]}}}}`))
	}))
	t.Cleanup(srv.Close)
	restoreBase := setBillingBase(srv.URL)
	restoreGlobal := setBillingBaseGlobal(srv.URL)
	t.Cleanup(restoreBase)
	t.Cleanup(restoreGlobal)

	storage, _ := json.Marshal(storedAuth{
		Auth:    storedTokens{AccessToken: "fixture-not-a-secret", Domain: "codebuddy.cn"},
		Account: storedAccount{UID: "7777"},
	})
	resp, ok, code := quotaFetchViaWire(t, "wb-quota-cold", storage)
	if !ok {
		t.Fatalf("cold path with live data must return a quota response, code=%q", code)
	}
	if len(resp.Summary) < 3 || resp.Summary[0].Value != 100 {
		t.Errorf("cold summary = %+v, want remaining 100", resp.Summary)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("live billing fetches = %d, want exactly 1", got)
	}
	if noAuth.Load() != 0 {
		t.Errorf("quota path must not read the host auth store (calls=%d)", noAuth.Load())
	}
	// cached under the addressed key now
	if _, hit := accountCache.Load("wb-quota-cold"); !hit {
		t.Fatalf("successful live fetch must cache under the addressed auth ID")
	}
	t.Cleanup(func() { accountCache.Delete("wb-quota-cold") })
}

func TestQuotaDescribeDeclaresWorkbuddy(t *testing.T) {
	out, err := handleMethod(methodQuotaDescribe, []byte(`{}`))
	if err != nil {
		t.Fatalf("quota.describe error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("quota.describe envelope: err=%v ok=%v", err, env.OK)
	}
	var resp quotaDescribeResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("quota.describe result decode: %v", err)
	}
	if len(resp.SupportedProviders) != 1 || resp.SupportedProviders[0] != providerName {
		t.Errorf("supported_providers = %v, want [%s]", resp.SupportedProviders, providerName)
	}
	if resp.SupportsReset {
		t.Errorf("supports_reset = true, want false (reset is not supported)")
	}
	if strings.TrimSpace(resp.DisplayName) == "" {
		t.Errorf("display_name must be non-empty")
	}
}

func TestQuotaResetNotSupported(t *testing.T) {
	hits := quotaTestUpstreams(t)
	out, err := handleMethod(methodQuotaReset, []byte(`{"auth_index":"wb-quota-1","auth_id":"wb-quota-1","provider":"workbuddy"}`))
	if err != nil {
		t.Fatalf("quota.reset handler error (unsupported must be a structured response, not an error): %v", err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("quota.reset envelope: err=%v ok=%v", err, env.OK)
	}
	var resp quotaResetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("quota.reset result decode: %v", err)
	}
	if resp.Success {
		t.Errorf("reset must report success=false (declared unsupported)")
	}
	if strings.TrimSpace(resp.Message) == "" {
		t.Errorf("reset message must explain the unsupported state")
	}
	if hits.Load() != 0 {
		t.Errorf("reset must not touch upstream (hits=%d)", hits.Load())
	}
}

func TestQuotaIdentifierMatchesProvider(t *testing.T) {
	// The host derives the quota provider key from quota.identifier and the
	// management layer matches it against auth.Provider (normalizeProviderID
	// lowercases both) — the identifier must be exactly providerName or the
	// host never flips supports_quota for workbuddy credentials.
	out, err := handleMethod(methodQuotaIdentifier, []byte(`{}`))
	if err != nil {
		t.Fatalf("quota.identifier error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("quota.identifier envelope: err=%v ok=%v", err, env.OK)
	}
	var resp identifierResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("quota.identifier result decode: %v", err)
	}
	if resp.Identifier != providerName {
		t.Errorf("quota identifier = %q, want %q", resp.Identifier, providerName)
	}
}

func TestQuotaRegistrationDeclaresQuotaProvider(t *testing.T) {
	// Registration snapshot: quota_provider flips the host's SupportsQuota,
	// and the pre-existing capability flags must survive the addition
	// byte-for-byte on the wire (registerRPCPlugin reads snake_case keys).
	reg := wbRegistration()
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	var snap struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Capabilities struct {
			QuotaProvider        bool   `json:"quota_provider"`
			ExecutorModelScope   string `json:"executor_model_scope"`
			ExecutorInputFormats int    `json:"-"` // not a bool; shape-checked below
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decode registration snapshot: %v", err)
	}
	if snap.Metadata.Name != providerName {
		t.Errorf("metadata.name = %q, want %q", snap.Metadata.Name, providerName)
	}
	if !snap.Capabilities.QuotaProvider {
		t.Errorf("capabilities.quota_provider = false, want true (host supports_quota flips on this flag)")
	}
	rawCap, _ := json.Marshal(reg.Capabilities)
	var caps map[string]any
	if err := json.Unmarshal(rawCap, &caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	for flag, want := range map[string]bool{
		"model_provider":         true,
		"auth_provider":          true,
		"executor":               true,
		"scheduler":              true,
		"management_api":         true,
		"usage_plugin":           true,
		"quota_provider":         true,
		"frontend_auth_provider": false,
	} {
		got, ok := caps[flag].(bool)
		if !ok || got != want {
			t.Errorf("capabilities.%s = %#v (present=%v), want %v", flag, caps[flag], ok, want)
		}
	}
	if snap.Capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeOAuth) {
		t.Errorf("executor_model_scope = %q, want %q", snap.Capabilities.ExecutorModelScope, pluginapi.ExecutorModelScopeOAuth)
	}
}

// TestQuotaFetchColdPathStringEncodedStorage pins the wire reality: the host
// (json v2 build) delivers storage_json as a QUOTED STRING wrapping the auth
// doc — without unwrapping, the cold bootstrap can never fire.
func TestQuotaFetchColdPathStringEncodedStorage(t *testing.T) {
	noAuth := quotaTestNoHostAuth(t)
	// Upstream routes billing calls direct (v0.9.49 codebuddy bypass; the host
	// bridge is unavailable in unit tests), so pointing billingBase at the
	// httptest server is enough — no seam stub needed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"Response":{"Data":{"TotalCount":1,"TotalDosage":100,"Accounts":[{"PackageName":"测试包","CapacityRemain":100,"CapacityUsed":0,"CapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0,"CycleCapacitySize":100,"CycleStartTime":"2026-09-01 00:00:00","CycleEndTime":"2026-10-01 00:00:00"}]}}}}`))
	}))
	t.Cleanup(srv.Close)
	restoreBase := setBillingBase(srv.URL)
	t.Cleanup(restoreBase)

	doc := `{"type":"workbuddy","provider":"workbuddy","auth":{"accessToken":"tok-fixture-not-a-secret"},"account":{"uid":"` + "442e36a5-858a-408d-ba25-9c906c7cad28" + `","nickname":"fixture"}}`
	quoted, err := json.Marshal(doc) // JSON string token wrapping the doc
	if err != nil {
		t.Fatalf("marshal quoted storage: %v", err)
	}
	resp, ok, code := quotaFetchViaWireStorage(t, "wb-quota-str", quoted)
	if !ok {
		t.Fatalf("string-encoded storage must unwrap and live-fetch, code=%q", code)
	}
	t.Logf("wire ok, summary=%+v", resp.Summary)
	if len(resp.Summary) < 3 || resp.Summary[0].Value != 100 {
		t.Errorf("cold summary = %+v, want remaining 100", resp.Summary)
	}
	if noAuth.Load() != 0 {
		t.Errorf("hostAuthCalls=%d, want 0 host-auth RPCs on the cold path", noAuth.Load())
	}
	t.Cleanup(func() { accountCache.Delete("wb-quota-str") })
}

// srvURLForQuotaCold stands up the billing stub and returns its URL.
func srvURLForQuotaCold(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","Response":{"Data":{"TotalCount":1,"TotalDosage":100,"Accounts":[{"PackageName":"测试包","CapacityRemain":100,"CapacityUsed":0,"CapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0,"CycleCapacitySize":100,"CycleStartTime":"2026-09-01 00:00:00","CycleEndTime":"2026-10-01 00:00:00"}]}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
