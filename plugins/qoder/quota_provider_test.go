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

// quota_provider_test.go pins the QuotaProvider capability (host v7.3.11
// sdk/pluginapi/types.go:1484 QuotaProvider: DescribeQuota/FetchQuota/
// ResetQuota) as spoken over the plugin RPC surface.
//
// Contract under test (host internal/pluginhost/rpc_client.go registerRPCPlugin
// + quota_provider.go callQuota*): the registration capabilities JSON carries
// "quota_provider":true (rpc_schema.go:48), the host then probes
// quota.identifier and forwards quota.describe / quota.fetch / quota.reset.
// FetchQuota is READ-ONLY over the existing accountCache (the same store
// cachedAccountDetails feeds and invalidateAccountCredits nils) — it must
// never open a new upstream call, reuse the existing refresh cadence only,
// and unknown/absent data must be reported as quota_unavailable instead of
// guessing a zero balance.
//
// NOTE on the upstream guard: qoder's fetchUserResource/fetchPaymentType call
// upstreamBaseFor(sa) directly (billing.go:191/:259), so the
// billingBaseOverride seam cannot intercept them. The recording server below
// covers the seam-riding sash paths; the stronger no-fetch proof is that the
// mapping test asserts the exact fixture numbers, which no live fetch could
// produce.

// quotaTestUpstreams points billingBaseOverride at a counting httptest server
// so a test fails loudly if the quota path rides the billing seam. Returns a
// counter the caller asserts zero.
func quotaTestUpstreams(t *testing.T) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	prev := billingBaseOverride
	billingBaseOverride = func(string) string { return srv.URL }
	t.Cleanup(func() { billingBaseOverride = prev })
	return &hits
}

// quotaTestNoHostAuth forbids host auth RPCs on the quota path: FetchQuota is
// a pure in-memory accountCache read keyed by the host-provided auth_id.
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
func quotaFetchViaWire(t *testing.T, authID string) (quotaFetchResponse, bool, string) {
	t.Helper()
	req, err := json.Marshal(quotaFetchRequest{AuthIndex: authID, AuthID: authID, Provider: providerName})
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

// quotaSampleCredits mirrors summarizeQuotaResponse's live four-pool shape:
// base 500/1000 + add-on 99/100 + a full dedicated pack + a half-used org
// pool, plus one all-zero row that carries no balance signal.
// remain 500+99+100+100=800, used 500+1+0+100=601, size 1000+100+100+200=1400.
func quotaSampleCredits() *creditsSummary {
	return &creditsSummary{
		TotalRemain: 800,
		TotalUsed:   601,
		TotalSize:   1400,
		PackCount:   5,
		FetchedAt:   "2026-09-30T01:00:00Z",
		Packages: []packageSummary{
			{Name: "基础额度", Remain: 500, Used: 500, Size: 1000, CycleStart: "2026-09-01T00:00:00Z", CycleEnd: "2026-09-30T23:59:59Z"},
			{Name: "赠送/签到额度", Remain: 99, Used: 1, Size: 100, CycleStart: "2026-09-15T00:00:00Z", CycleEnd: "2026-10-31T23:59:59Z"},
			{Name: "专用资源包 1", Remain: 100, Used: 0, Size: 100, CycleEnd: "2026-10-31T23:59:59Z"},
			{Name: "组织共享池", Remain: 100, Used: 100, Size: 200},
			{Name: "空资源包", Remain: 0, Used: 0, Size: 0},
		},
	}
}

func TestQuotaFetchMapsCachedCreditsSample(t *testing.T) {
	hits := quotaTestUpstreams(t)
	noAuth := quotaTestNoHostAuth(t)
	entry := &accountCacheEntry{credits: quotaSampleCredits(), plan: "Pro Trial", fetched: time.Now()}
	accountCache.Store("qoder-quota-1", entry)
	t.Cleanup(func() { accountCache.Delete("qoder-quota-1") })

	resp, ok, _ := quotaFetchViaWire(t, "qoder-quota-1")
	if !ok {
		t.Fatalf("cached credits must map to a quota response, got quota_unavailable")
	}
	if hits.Load() != 0 || noAuth.Load() != 0 {
		t.Fatalf("quota.fetch must be read-only (upstream hits=%d host-auth calls=%d)", hits.Load(), noAuth.Load())
	}
	// Read-only: the cache entry itself is untouched by the fetch.
	if cur, _ := accountCache.Load("qoder-quota-1"); cur != any(entry) {
		t.Fatalf("quota.fetch must not replace the cached entry")
	}
	if entry.credits.FetchedAt != "2026-09-30T01:00:00Z" {
		t.Fatalf("quota.fetch must not mutate the cached snapshot (FetchedAt=%q)", entry.credits.FetchedAt)
	}

	// Subscription: the cached plan is the plan tier.
	if resp.Subscription == nil {
		t.Fatalf("subscription missing for a cached plan")
	}
	if resp.Subscription.Plan != "Pro Trial" {
		t.Errorf("subscription.plan = %q, want Pro Trial", resp.Subscription.Plan)
	}

	// Summary: the three aggregate counters with real numbers (never zeroed).
	want := map[string]float64{
		"credits_remaining": 800,
		"credits_used":      601,
		"credits_size":      1400,
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

	// Groups: one per informative package, in package order; the bucket
	// fraction is remain/size with the cycle end carried through. The
	// all-zero 空资源包 row must be skipped, never rendered as a 0 bucket.
	if len(resp.Groups) != 4 {
		t.Fatalf("groups = %d, want one per informative package (4)", len(resp.Groups))
	}
	type groupWant struct {
		name  string
		frac  float64
		reset string
	}
	wants := []groupWant{
		{"基础额度", 0.5, "2026-09-30T23:59:59Z"},
		{"赠送/签到额度", 0.99, "2026-10-31T23:59:59Z"},
		{"专用资源包 1", 1, "2026-10-31T23:59:59Z"},
		{"组织共享池", 0.5, ""},
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
		if !strings.Contains(b.Description, w.name) {
			t.Errorf("group[%d] description %q should name the package", i, b.Description)
		}
		if !strings.Contains(b.Description, "积分") {
			t.Errorf("group[%d] description %q should carry the raw remain/size figures", i, b.Description)
		}
	}
}

func TestQuotaFetchUnknownReportsUnavailable(t *testing.T) {
	hits := quotaTestUpstreams(t)

	// 1) No cache entry at all.
	_, ok, code := quotaFetchViaWire(t, "qoder-quota-none")
	if ok {
		t.Fatalf("unknown credential must not return a quota response")
	}
	if code != "quota_unavailable" {
		t.Errorf("unavailable code = %q, want quota_unavailable", code)
	}

	// 2) invalidateAccountCredits semantics: the entry survives with
	// credits=nil after a chat — still unavailable, never stale numbers.
	accountCache.Store("qoder-quota-nil", &accountCacheEntry{credits: nil, plan: "Pro Trial", fetched: time.Now()})
	t.Cleanup(func() { accountCache.Delete("qoder-quota-nil") })
	_, ok, code = quotaFetchViaWire(t, "qoder-quota-nil")
	if ok {
		t.Fatalf("credits=nil (post-invalidate) must be unavailable")
	}
	if code != "quota_unavailable" {
		t.Errorf("nil-credits code = %q, want quota_unavailable", code)
	}

	// 3) A pure zero with no informative package is "no data" per the shared
	// isCreditsExhausted convention (billing.go) — reporting it as a balance
	// would fabricate a 0 where upstream said nothing.
	accountCache.Store("qoder-quota-zero", &accountCacheEntry{credits: &creditsSummary{}, plan: "Free", fetched: time.Now()})
	t.Cleanup(func() { accountCache.Delete("qoder-quota-zero") })
	_, ok, code = quotaFetchViaWire(t, "qoder-quota-zero")
	if ok {
		t.Fatalf("all-zero dataless snapshot must be unavailable, not a 0 balance")
	}
	if code != "quota_unavailable" {
		t.Errorf("zero-snapshot code = %q, want quota_unavailable", code)
	}

	// 4) No auth_id at all — nothing to look up.
	req, _ := json.Marshal(quotaFetchRequest{Provider: providerName})
	out, err := handleMethod(methodQuotaFetch, req)
	if err != nil {
		t.Fatalf("quota.fetch handler error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("quota.fetch envelope decode: %v", err)
	}
	if env.OK {
		t.Fatalf("missing auth_id must be unavailable")
	}

	// round7e contract: the unknown path may READ the host auth list (cold-path
	// resolution is read-only), but must never hit upstream or fabricate data.
	if hits.Load() != 0 {
		t.Fatalf("unavailable path must not hit upstream (upstream hits=%d)", hits.Load())
	}
}

func TestQuotaDescribeDeclaresQoder(t *testing.T) {
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
	out, err := handleMethod(methodQuotaReset, []byte(`{"auth_index":"qoder-quota-1","auth_id":"qoder-quota-1","provider":"qoder"}`))
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
	// host never flips supports_quota for qoder credentials.
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
	// Registration snapshot: quota_provider flips the host's SupportsQuota
	// (internal/pluginhost/snapshot.go:98 via rpc_client.go:146), and the
	// pre-existing capability flags must survive the addition byte-for-byte
	// on the wire (registerRPCPlugin reads snake_case keys).
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
			QuotaProvider      bool   `json:"quota_provider"`
			ExecutorModelScope string `json:"executor_model_scope"`
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
	if fmt.Sprintf("%v", caps["executor_input_formats"]) != "[chat-completions]" {
		t.Errorf("executor_input_formats = %v, want [chat-completions]", caps["executor_input_formats"])
	}
	if fmt.Sprintf("%v", caps["executor_output_formats"]) != "[chat-completions]" {
		t.Errorf("executor_output_formats = %v, want [chat-completions]", caps["executor_output_formats"])
	}
}

// TestQuotaFetchColdPathLiveFetchesOnce covers the cold-cache bootstrap: no
// cached snapshot, but the request carries the credential storage → exactly
// ONE live billing fetch for the addressed credential, result cached under
// the addressed auth ID and returned. A failed/empty live answer stays
// quota_unavailable and is never cached (no invented numbers, no borrow).
func TestQuotaFetchColdPathLiveFetchesOnce(t *testing.T) {
	hits := quotaTestUpstreams(t)
	noAuth := quotaTestNoHostAuth(t)

	fetchCalls := 0
	prevFetch := quotaLiveFetchFn
	quotaLiveFetchFn = func(sa *storedAuth) (*creditsSummary, error) {
		fetchCalls++
		if sa.Auth.AccessToken != "dt-fixture-not-a-secret" {
			t.Errorf("live fetch token = %q, want the fixture dt token", sa.Auth.AccessToken)
		}
		return &creditsSummary{
			TotalRemain: 100, TotalUsed: 0, TotalSize: 100, PackCount: 1,
			FetchedAt: "2026-09-30T09:00:00Z",
			Packages:  []packageSummary{{Name: "测试包", Remain: 100, Used: 0, Size: 100}},
		}, nil
	}
	t.Cleanup(func() { quotaLiveFetchFn = prevFetch })

	storage := json.RawMessage(`{"auth":{"accessToken":"dt-fixture-not-a-secret","refreshToken":"drt-fixture-not-a-secret"},"account":{"uid":"quota-cold-uid"}}`)
	resp, ok, code := quotaFetchViaWireStorage(t, "qoder-quota-cold", storage)
	if !ok {
		t.Fatalf("cold path with live data must return a quota response, code=%q", code)
	}
	if len(resp.Summary) == 0 || resp.Summary[0].Value != 100 {
		t.Errorf("cold summary = %+v, want remaining 100", resp.Summary)
	}
	if fetchCalls != 1 {
		t.Errorf("live billing fetches = %d, want exactly 1", fetchCalls)
	}
	if noAuth.Load() != 0 {
		t.Errorf("quota path must not read the host auth store (calls=%d)", noAuth.Load())
	}
	if hits.Load() != 0 {
		t.Errorf("quota path must not hit raw upstream bases in tests (hits=%d)", hits.Load())
	}
	if _, hit := accountCache.Load("qoder-quota-cold"); !hit {
		t.Fatalf("successful live fetch must cache under the addressed auth ID")
	}
	t.Cleanup(func() { accountCache.Delete("qoder-quota-cold") })

	// Failure path: live fetch error → unavailable, never cached.
	quotaLiveFetchFn = func(sa *storedAuth) (*creditsSummary, error) {
		fetchCalls++
		return nil, fmt.Errorf("boom")
	}
	storage2 := json.RawMessage(`{"auth":{"accessToken":"dt-fixture-not-a-secret","refreshToken":"drt-fixture-not-a-secret"},"account":{"uid":"quota-cold-uid2"}}`)
	_, ok, code = quotaFetchViaWireStorage(t, "qoder-quota-cold2", storage2)
	if ok {
		t.Fatalf("failed live fetch must stay unavailable")
	}
	if code != "quota_unavailable" {
		t.Errorf("failed-fetch code = %q, want quota_unavailable", code)
	}
	if _, hit := accountCache.Load("qoder-quota-cold2"); hit {
		t.Fatalf("failed live fetch must not cache an entry")
	}
}

// quotaFetchViaWireStorage is quotaFetchViaWire with the host-attached
// credential storage the real quota.fetch request carries.
func quotaFetchViaWireStorage(t *testing.T, authID string, storage json.RawMessage) (quotaFetchResponse, bool, string) {
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


// TestQuotaFetchColdPathStringEncodedStorage pins the wire reality: the host
// (json v2 build) sends storage_json as a QUOTED STRING containing the auth
// doc, not as a raw object. The cold path must unwrap it and still live-fetch
// exactly once.
func TestQuotaFetchColdPathStringEncodedStorage(t *testing.T) {
	noAuth := quotaTestNoHostAuth(t)
	fetchCalls := 0
	prevFetch := quotaLiveFetchFn
	quotaLiveFetchFn = func(sa *storedAuth) (*creditsSummary, error) {
		fetchCalls++
		return &creditsSummary{
			TotalRemain: 100, TotalUsed: 5, TotalSize: 100, PackCount: 1,
			Packages: []packageSummary{{Name: "测试包", Remain: 100, Used: 0, Size: 100}},
		}, nil
	}
	t.Cleanup(func() { quotaLiveFetchFn = prevFetch })

	doc := `{"auth":{"accessToken":"dt-fixture-not-a-secret","refreshToken":"drt-fixture-not-a-secret"},"account":{"uid":"quota-str-uid"}}`
	quoted, err := json.Marshal(doc) // JSON string token wrapping the doc
	if err != nil {
		t.Fatalf("marshal quoted storage: %v", err)
	}
	resp, ok, code := quotaFetchViaWireStorage(t, "qoder-quota-str", quoted)
	if !ok {
		t.Fatalf("string-encoded storage must unwrap and live-fetch, code=%q", code)
	}
	if len(resp.Summary) == 0 || resp.Summary[0].Value != 100 {
		t.Errorf("cold summary = %+v, want remaining 100", resp.Summary)
	}
	if fetchCalls != 1 || noAuth.Load() != 0 {
		t.Errorf("fetchCalls=%d hostAuthCalls=%d, want exactly 1 live fetch and 0 host-auth RPCs", fetchCalls, noAuth.Load())
	}
	t.Cleanup(func() { accountCache.Delete("qoder-quota-str") })
}
