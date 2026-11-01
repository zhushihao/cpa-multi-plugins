package main

// routing_wiring_test.go — integration-level (still pure Go) tests that the
// new mechanisms are actually reachable through the plugin's RPC surface:
// scheduler_mode=weighted, the request.intercept_before/after method dispatch,
// cost observation from upstream usage objects, and the advertised-model scope
// registry.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSchedulerPick_WeightedMode_TierGuardAndStickyRebind(t *testing.T) {
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)
	const model = "wb-model-tier-guard"
	wbRouterPool.noteCost("t-free", model, 0, 1000)   // tier 0
	wbRouterPool.noteCost("t-paid", model, 2.0, 1000) // tier 2

	h := http.Header{}
	h.Set(wbSessionHeader, wbSignSessionKey("fb:tier-guard-key"))
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    model,
		Options:  pluginapi.SchedulerOptions{Headers: h},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "t-free", Provider: providerName},
			{ID: "t-paid", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "t-free" {
		t.Fatalf("weighted pick = %+v, want t-free (tier-0 hard filter beats sticky paid)", resp)
	}
	// The session must have been re-bound to the account that actually served.
	uid, ok := wbSticky.resolve("fb:tier-guard-key", model, []string{"t-free", "t-paid"})
	if !ok || uid != "t-free" {
		t.Fatalf("session re-bind = (%q,%v), want t-free", uid, ok)
	}
	// A second request with the same header keeps serving t-free.
	raw2, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    model,
		Options:  pluginapi.SchedulerOptions{Headers: h},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "t-free", Provider: providerName},
			{ID: "t-paid", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp2 := parsePickResponse(t, raw2); resp2.AuthID != "t-free" {
		t.Fatalf("sticky repeat pick = %+v, want t-free", resp2)
	}
}

func TestSchedulerPick_WeightedMode_ServesStickyWhenTierAllows(t *testing.T) {
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)
	const model = "wb-model-sticky-ok"
	// t-a has a tier-1 (unknown) cost on this model; both are acceptable.
	h := http.Header{}
	h.Set(wbSessionHeader, wbSignSessionKey("fb:sticky-ok-key"))
	first, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    model,
		Options:  pluginapi.SchedulerOptions{Headers: h},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "s-a", Provider: providerName},
			{ID: "s-b", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	r1 := parsePickResponse(t, first)
	if !r1.Handled {
		t.Fatal("weighted pick should handle workbuddy candidates")
	}
	// Same session header → same account (no tier conflict on a fresh model).
	second, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    model,
		Options:  pluginapi.SchedulerOptions{Headers: h},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "s-a", Provider: providerName},
			{ID: "s-b", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r2 := parsePickResponse(t, second); r2.AuthID != r1.AuthID {
		t.Fatalf("sticky pick changed account: %q -> %q", r1.AuthID, r2.AuthID)
	}
}

func TestSchedulerPick_WeightedMode_DefersNonWorkbuddyAndOffMode(t *testing.T) {
	restore := setSchedulerMode(schedulerModeWeighted)
	defer restore()
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: "other",
		Model:    "gpt-5.4",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "x", Provider: "other"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := parsePickResponse(t, raw); resp.Handled {
		t.Fatalf("non-workbuddy must defer, got %+v", resp)
	}
	restore()
	raw2, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "wb-model-off",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-only", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := parsePickResponse(t, raw2); resp.Handled {
		t.Fatalf("off mode must defer, got %+v", resp)
	}
}

// TestHandleMethodRoutesInterceptors proves the new capabilities are reachable
// through the plugin RPC method switch (the same path the host uses).
func TestHandleMethodRoutesInterceptors(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	restoreMode := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restoreMode)
	req := wbInterceptReq("deepseek-v4.1-flash", "deepseek-v4.1-flash",
		`{"messages":[{"role":"user","content":"hello"}]}`, nil)
	out, err := handleMethod(pluginabi.MethodRequestInterceptBefore, req)
	if err != nil {
		t.Fatalf("intercept_before method: %v", err)
	}
	resp := wbParseInterceptResp(t, out)
	if len(resp.Headers.Values(wbSessionHeader)) != 1 {
		t.Fatalf("intercept_before must set the signed session header, got %+v", resp)
	}
	outAfter, err := handleMethod(pluginabi.MethodRequestInterceptAfter, mustMarshal(t, pluginapi.RequestInterceptRequest{
		Model:   "deepseek-v4.1-flash",
		Headers: http.Header{wbSessionHeader: []string{"whatever"}},
	}))
	if err != nil {
		t.Fatalf("intercept_after method: %v", err)
	}
	if respAfter := wbParseInterceptResp(t, outAfter); len(respAfter.ClearHeaders) != 1 {
		t.Fatalf("intercept_after must clear the private header, got %+v", respAfter)
	}
	// Unknown methods still error out.
	if _, err := handleMethod("nope.nope", nil); err != nil {
		t.Fatalf("unknown method should return an error envelope, not err=%v", err)
	}
}

// TestRegistrationAdvertisesNewCapabilities pins the wire contract: the host
// only wires request.intercept_before when the registration says
// request_interceptor=true.
func TestRegistrationAdvertisesNewCapabilities(t *testing.T) {
	raw, err := json.Marshal(wbRegistration())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Capabilities map[string]any `json:"capabilities"`
		Metadata     struct {
			// pluginapi.Metadata has no JSON tags → Go field names on the wire.
			ConfigFields []struct {
				Name string `json:"Name"`
			} `json:"ConfigFields"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if v, _ := got.Capabilities["request_interceptor"].(bool); !v {
		t.Fatalf("registration must declare request_interceptor, got %s", raw)
	}
	names := map[string]bool{}
	for _, f := range got.Metadata.ConfigFields {
		names[f.Name] = true
	}
	for _, want := range []string{"scheduler_mode", "expiring_soon", "state_file"} {
		if !names[want] {
			t.Fatalf("registration must expose config field %q", want)
		}
	}
}

// TestCostObservationFromUsageMap: usage.credit + tokens from a real upstream
// usage object feeds the cost ledger; absent/invalid observations never create
// a fake free-tier sample.
func TestCostObservationFromUsageMap(t *testing.T) {
	const model = "wb-model-cost-observe"
	m := map[string]any{"credit": float64(0.25), "total_tokens": float64(500)}
	credit, total, ok := wbUsageCreditTotal(m)
	if !ok || total != 500 {
		t.Fatalf("wbUsageCreditTotal = (%v,%d,%v)", credit, total, ok)
	}
	if diff := credit - 0.25; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("credit = %v, want 0.25", credit)
	}
	// Numeric-string credit (some upstream shapes) is accepted.
	if _, _, ok := wbUsageCreditTotal(map[string]any{"credit": "0.5", "prompt_tokens": float64(10), "completion_tokens": float64(10)}); !ok {
		t.Fatal("numeric-string credit must be accepted")
	}
	// Missing credit / zero tokens = no observation.
	if _, _, ok := wbUsageCreditTotal(map[string]any{"total_tokens": float64(10)}); ok {
		t.Fatal("missing credit must not be an observation")
	}
	if _, _, ok := wbUsageCreditTotal(map[string]any{"credit": float64(1)}); ok {
		t.Fatal("zero tokens must not be an observation")
	}
	// End-to-end into the pool ledger.
	wbObserveModelCost("obs-uid", model, m) // per1k = 0.5 → tier 2
	tier, _ := wbRouterPool.costLedger.tierFor("obs-uid", model)
	if tier != wbCostTierPaid {
		t.Fatalf("tier = %d, want %d after paid observation", tier, wbCostTierPaid)
	}
	// A free observation flips it back to tier 0 (limit-window graduation).
	wbObserveModelCost("obs-uid", model, map[string]any{"credit": float64(0), "total_tokens": float64(500)})
	entry, ok := wbRouterPool.costLedger.lookup("obs-uid", model)
	if !ok || entry.CostPer1k <= 0 {
		t.Fatalf("EMA after free sample = %+v, want > 0 (single free sample does not zero it)", entry)
	}
}

// TestAdvertisedModelRegistry: IDs and Names are recorded, and the scope
// predicate is bare-only — a removed realm namespace (cn:/global:/intl:) is
// never this plugin's, even when its base id is advertised (Issue #6 owner
// correction: the incumbent :3002 serves the prefixed names).
func TestAdvertisedModelRegistry(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	wbAdvertised.Lock()
	wbAdvertised.models["glm-5.2-by-name"] = struct{}{}
	wbAdvertised.Unlock()
	cases := map[string]bool{
		"deepseek-v4.1-flash":        true,
		"cn:deepseek-v4.1-flash":     false,
		"global:deepseek-v4.1-flash": false,
		"intl:deepseek-v4.1-flash":   false,
		"cn:not-ours":                false,
		"glm-5.2-by-name":            true,
		"deepseek-v4.1-flash-x":      false,
		"":                           false,
	}
	for model, want := range cases {
		if got := wbScopedModel(model); got != want {
			t.Errorf("wbScopedModel(%q) = %v, want %v", model, got, want)
		}
	}
}
