package main

// cost_attribution_test.go — S1/I6 review-fix tests (red → green).
//
// Two independent misalignments were found by review:
//
//  1. The routing cost ledger was written under sa.Account.UID while the
//     scheduler can only address host auth record IDs (file-derived). A
//     candidate could therefore never see its own observations.
//  2. The observation used the resolved upstream model id while the lookup
//     used the requested (aliased / realm-prefixed) model id, so the tier
//     filter silently degraded to "unknown".
//
// CPAMP usage attribution (the account UID the monitoring pipeline keys on)
// is a separate concern and must stay unchanged.

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// setModelAliasesForTest installs a host oauth-model-alias table
// (alias → real) and restores the previous table on cleanup.
func setModelAliasesForTest(t *testing.T, byAlias map[string]string) {
	t.Helper()
	modelAliasCache.Lock()
	prev := modelAliasCache.byAlias
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
	t.Cleanup(func() {
		modelAliasCache.Lock()
		modelAliasCache.byAlias = prev
		modelAliasCache.Unlock()
	})
}

// TestCostModelKey_CanonicalKeyResolvesAlias pins the single canonical model
// key: both the observation path and the SchedulerPick lookup derive the key
// through this function, so they can never drift apart.
//
// Bare-only boundary (Issue #6 owner correction): the removed round4 realm
// namespace is NOT normalized away. The ledger key stays total and
// identity-preserving — a prefixed name is denied at the chat ingress and can
// never be observed, so aliasing it into the bare id would only let two
// different providers' names collide in one ledger row.
func TestCostModelKey_CanonicalKeyResolvesAlias(t *testing.T) {
	setModelAliasesForTest(t, map[string]string{
		"point/deepseek-v4-flash": "deepseek-v4-flash",
	})
	cases := map[string]string{
		"deepseek-v4.1-flash":     "deepseek-v4.1-flash",
		"cn:deepseek-v4.1-flash":  "cn:deepseek-v4.1-flash",
		"global:glm-5":            "global:glm-5",
		"intl:fast-model":         "intl:fast-model",
		"  glm-5.3  ":             "glm-5.3",
		"point/deepseek-v4-flash": "deepseek-v4-flash",
		"":                        "",
	}
	for in, want := range cases {
		if got := wbCostModelKey(in); got != want {
			t.Errorf("wbCostModelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestObserveModelCost_KeyedByCanonicalModelKey: the observation side and the
// SchedulerPick lookup side must agree on the canonical key for the
// client-requested name (round4 used a realm-prefixed name here; under the
// bare-only contract the requested name is the bare id — same agreement).
func TestObserveModelCost_KeyedByCanonicalModelKey(t *testing.T) {
	const authID = "wb-cost-auth-id"
	const model = "wb-cost-alias-model"
	wbObserveModelCost(authID, model, map[string]any{"credit": float64(0.5), "total_tokens": float64(1000)})
	tier, _ := wbRouterPool.costLedger.tierFor(authID, wbCostModelKey(model))
	if tier != wbCostTierPaid {
		t.Fatalf("tier for canonical key = %d, want %d (observation landed under a different model key)", tier, wbCostTierPaid)
	}
	// The pool's own lookup must agree: note + pick share one key space.
	cands := wbPoolCandidatesForScheduler([]pluginapi.SchedulerAuthCandidate{
		{ID: authID, Provider: providerName},
	})
	got, ok := wbRouterPool.pick(model, cands)
	if !ok || got != authID {
		t.Fatalf("pick = (%q,%v), want (%q,true)", got, ok, authID)
	}
	if tier, _ := wbRouterPool.costLedger.tierFor(authID, model); tier != wbCostTierPaid {
		t.Fatalf("plain-model lookup = %d, want %d", tier, wbCostTierPaid)
	}
}

// TestPublishUsage_CostLedgerKeyedByHostAuthID is the wiring-level contract:
// the executor-style publish call carries BOTH identities — the host auth
// record ID the scheduler addresses and the account UID CPAMP attributes —
// and the cost observation must land on the auth record ID, under the
// canonical key of the requested (aliased) name.
func TestPublishUsage_CostLedgerKeyedByHostAuthID(t *testing.T) {
	setModelAliasesForTest(t, map[string]string{"wb-pub-alias": "wb-pub-model"})
	const authID = "wb-pub-auth-record"
	const uid = "wb-pub-account-uid"
	const model = "wb-pub-model"
	publishUsageWithUsage("wb-pub-alias", model, uid, authID, time.Now(),
		usageDetailFromMap(map[string]any{"prompt_tokens": float64(500), "completion_tokens": float64(500)}),
		map[string]any{"credit": float64(0.25), "total_tokens": float64(1000)}, false, 0, "")

	if tier, _ := wbRouterPool.costLedger.tierFor(authID, model); tier != wbCostTierPaid {
		t.Fatalf("host-auth-ID tier = %d, want %d (cost record not keyed by the scheduler-addressable ID)", tier, wbCostTierPaid)
	}
	if _, ok := wbRouterPool.costLedger.lookup(uid, model); ok {
		t.Fatal("observation must not be keyed by the account UID (scheduler cannot address it)")
	}
}

// TestSchedulerPick_TierHardFilterUnderAliasedModel: end-to-end through the
// SchedulerPick RPC surface — after one paid and one free observation recorded
// through the publish path for the SAME requested alias, the tier-0 hard
// filter must deterministically route to the free account.
func TestSchedulerPick_TierHardFilterUnderAliasedModel(t *testing.T) {
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)
	setModelAliasesForTest(t, map[string]string{"wb-alias-request": "wb-alias-model"})

	publishUsageWithUsage("wb-alias-request", "wb-alias-model", "uid-paid", "auth-paid", time.Now(),
		usageDetailFromMap(map[string]any{"total_tokens": float64(500)}),
		map[string]any{"credit": float64(2.5), "total_tokens": float64(500)}, false, 0, "")
	publishUsageWithUsage("wb-alias-request", "wb-alias-model", "uid-free", "auth-free", time.Now(),
		usageDetailFromMap(map[string]any{"total_tokens": float64(500)}),
		map[string]any{"credit": float64(0), "total_tokens": float64(500)}, false, 0, "")

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "wb-alias-request",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-paid", Provider: providerName},
			{ID: "auth-free", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "auth-free" {
		t.Fatalf("pick = %+v, want auth-free (tier-0 hard filter must see the aliased observation)", resp)
	}
}

// TestSchedulerPick_UnknownUIDSingleAccountUnaffected: a single candidate with
// no resolvable identity must keep working exactly as before (fail-open).
func TestSchedulerPick_UnknownUIDSingleAccountUnaffected(t *testing.T) {
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "wb-single-model",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-solo", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := parsePickResponse(t, raw); !resp.Handled || resp.AuthID != "wb-solo" {
		t.Fatalf("pick = %+v, want wb-solo handled", resp)
	}
}
