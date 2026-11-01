package main

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// models_union_test.go — round6 slice 3: the realm model catalog is the UNION
// of every credential's discovery, not a single slot. Before this change the
// realm cache held ONE answer; whichever credential discovered first after a
// restart owned the realm's catalog, so two accounts with different catalogs
// (live: one missing kimi-k3-2 / glm-5.3-flashx / minimax-m3-pay) made models
// flicker across restarts, papered over with a models_cn pin (now deprecated —
// parsed, ignored; safe to delete from config).
//
// Contract under test:
//   - A has X, B has Y → the realm advertises X∪Y (same id deduped,
//     case-insensitive; display name taken from any consistent contributor).
//   - One credential's discovery failure never swallows the others' catalog.
//   - A credential whose contribution is already in the fresh union does not
//     re-discover (TTL short-circuit preserved); a credential never heard
//     from discovers exactly once (its models may be missing from the union).
//   - Per-realm exclusion filtering is untouched (filterExcludedModelsForRealm
//     runs downstream of this layer — see handleModelForAuth).

// unionTestEnv swaps the discovery seam for a per-token dispatcher and
// isolates the cache + pins for the test. dispatch maps accessToken →
// (models, error).
func unionTestEnv(t *testing.T, dispatch func(token, realm string) ([]pluginapi.ModelInfo, error)) *int {
	t.Helper()
	resetDynamicModelsCache()
	resetPinnedModels()
	t.Cleanup(func() { resetDynamicModelsCache(); resetPinnedModels() })
	var calls int
	var mu sync.Mutex
	orig := discoverModelsFn
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return dispatch(accessToken, realm)
	}
	t.Cleanup(func() { discoverModelsFn = orig })
	return &calls
}

// unionCredStorage builds a CN credential blob with a stable uid so sibling
// and repeat asks map to the same contribution slot.
func unionCredStorage(token, uid string) []byte {
	return []byte(`{"auth":{"accessToken":"` + token + `","region":"cn"},"account":{"uid":"` + uid + `"}}`)
}

// TestModelCatalogUnionsAcrossAccounts is the core fixture: account A
// discovers X, account B discovers Y (each missing the other's model) — the
// realm must advertise X∪Y, with a shared id appearing once.
func TestModelCatalogUnionsAcrossAccounts(t *testing.T) {
	calls := unionTestEnv(t, func(token, realm string) ([]pluginapi.ModelInfo, error) {
		switch token {
		case "tok-a":
			// A: shared model (upper-case variant of B's id) + X; no Y.
			return realmTestModels("Shared", "model-x"), nil
		case "tok-b":
			// B: Y + the shared id (lower-case); no X.
			return realmTestModels("model-y", "shared"), nil
		}
		return nil, errors.New("unexpected token " + token)
	})

	// A asks first: it is unknown to the union, so it discovers.
	gotA := fetchDynamicModelsFromStorage(unionCredStorage("tok-a", "ua"))
	if ids := discoveryIDs(gotA); len(ids) != 2 || ids[0] != "Shared" || ids[1] != "model-x" {
		t.Fatalf("first account must be served its own discovery, got %v", ids)
	}

	// B asks next: the union is fresh but B was never heard from — it must
	// discover too, and the served catalog must grow to X∪Y (shared once).
	gotB := fetchDynamicModelsFromStorage(unionCredStorage("tok-b", "ub"))
	if ids := discoveryIDs(gotB); len(ids) != 3 {
		t.Fatalf("union fixture broken: got %v, want [Shared model-x model-y] (A∪B, shared id once)", ids)
	}
	byID := map[string]bool{}
	for _, m := range gotB {
		byID[m.ID] = true
	}
	if !byID["model-x"] {
		t.Errorf("union missing A-only model-x: %v", discoveryIDs(gotB))
	}
	if !byID["model-y"] {
		t.Errorf("union missing B-only model-y: %v", discoveryIDs(gotB))
	}
	shared := 0
	for _, m := range gotB {
		if strings.EqualFold(m.ID, "shared") {
			shared++
		}
	}
	if shared != 1 {
		t.Errorf("shared id must appear exactly once (case-insensitive dedupe), got %d: %v", shared, discoveryIDs(gotB))
	}

	// Repeat asks within the TTL serve the union without re-discovering
	// (both contributions recorded — the single-slot stampede stays dead).
	before := *calls
	if ids := discoveryIDs(fetchDynamicModelsFromStorage(unionCredStorage("tok-a", "ua"))); len(ids) != 3 {
		t.Fatalf("repeat ask must serve the full union, got %v", ids)
	}
	if ids := discoveryIDs(fetchDynamicModelsFromStorage(unionCredStorage("tok-b", "ub"))); len(ids) != 3 {
		t.Fatalf("repeat ask must serve the full union, got %v", ids)
	}
	if *calls != before {
		t.Fatalf("contributing credentials must not re-discover within the TTL: calls %d → %d", before, *calls)
	}

	// Diagnostics name the multi-account answer.
	st := realmModelStateFor("cn")
	if st == nil || st.Source != "discovery union (2 credentials)" {
		t.Fatalf("union state source = %+v, want 'discovery union (2 credentials)'", st)
	}
	if st.Count != 3 {
		t.Fatalf("union state count = %d, want 3", st.Count)
	}
}

// TestModelCatalogUnionSurvivesSingleAccountFailure: B's discovery failing
// must not swallow A's catalog — B's asks serve A's union, and B does not
// hammer upstream while the union is fresh (one retry attempt per union TTL,
// the same cadence the single-slot cache had).
func TestModelCatalogUnionSurvivesSingleAccountFailure(t *testing.T) {
	calls := unionTestEnv(t, func(token, realm string) ([]pluginapi.ModelInfo, error) {
		switch token {
		case "tok-a":
			return realmTestModels("model-x"), nil
		case "tok-b":
			return nil, errors.New("models API status 503 (account B)")
		}
		return nil, errors.New("unexpected token " + token)
	})

	// A seeds the union.
	if got := fetchDynamicModelsFromStorage(unionCredStorage("tok-a", "ua")); len(got) != 1 {
		t.Fatalf("A must seed its catalog, got %v", discoveryIDs(got))
	}

	// B fails: its ask must still serve A's complete catalog.
	if got := fetchDynamicModelsFromStorage(unionCredStorage("tok-b", "ub")); len(got) != 1 || got[0].ID != "model-x" {
		t.Fatalf("B's failure must not swallow A's catalog, got %v", discoveryIDs(got))
	}

	// B asks again within the union TTL: served from cache, no new upstream
	// attempt (failures retry at the TTL gate, exactly like the old cache).
	before := *calls
	if got := fetchDynamicModelsFromStorage(unionCredStorage("tok-b", "ub")); len(got) != 1 || got[0].ID != "model-x" {
		t.Fatalf("repeat ask after failure must serve the union, got %v", discoveryIDs(got))
	}
	if *calls != before {
		t.Fatalf("failed credential must not re-discover within the union TTL: calls %d → %d", before, *calls)
	}

	// The failure stays visible in the realm diagnostics.
	st := realmModelStateFor("cn")
	if st == nil || st.Source != "last discovery (transient failure)" {
		t.Fatalf("state source = %+v, want 'last discovery (transient failure)'", st)
	}
}

// TestModelCatalogUnionBothAccountsFailThenRecover: the first-asked account
// failing with nothing cached still advertises nothing (v0.9.33 honesty);
// the other account's later success then serves its full catalog.
func TestModelCatalogUnionBothAccountsFailThenRecover(t *testing.T) {
	unionTestEnv(t, func(token, realm string) ([]pluginapi.ModelInfo, error) {
		switch token {
		case "tok-a":
			return nil, errors.New("down")
		case "tok-b":
			return realmTestModels("model-y"), nil
		}
		return nil, errors.New("unexpected token " + token)
	})

	if got := fetchDynamicModelsFromStorage(unionCredStorage("tok-a", "ua")); len(got) != 0 {
		t.Fatalf("no-cache failure must advertise nothing (v0.9.33), got %v", discoveryIDs(got))
	}
	if got := fetchDynamicModelsFromStorage(unionCredStorage("tok-b", "ub")); len(got) != 1 || got[0].ID != "model-y" {
		t.Fatalf("the healthy account's catalog must come through complete, got %v", discoveryIDs(got))
	}
}

// TestModelsCnPinDeprecatedIgnored: models_cn is parsed for tolerance but no
// longer generates or restricts the CN catalog — the union of every CN
// credential's discovery owns it. The line is safe to delete from config.
func TestModelsCnPinDeprecatedIgnored(t *testing.T) {
	calls := unionTestEnv(t, func(token, realm string) ([]pluginapi.ModelInfo, error) {
		if realm != "cn" {
			return nil, errors.New("wrong realm: " + realm)
		}
		return realmTestModels("kimi-k3-2", "glm-5.3-flashx", "minimax-m3-pay"), nil
	})

	// The deprecated pin is still PARSED (tolerated in config)...
	configure(configYAMLEnvelope("enabled: true\nmodels_cn: \"pinned-a, pinned-b\"\n"))
	defer configure(configYAMLEnvelope("enabled: true\n"))
	if ids := pinnedModelIDsForRealm("cn"); len(ids) != 2 {
		t.Fatalf("models_cn must stay parseable (tolerated), got %v", ids)
	}

	// ...but the served CN catalog is the discovery union, not the pin.
	got := fetchDynamicModelsFromStorage(unionCredStorage("tok-a", "ua"))
	if ids := discoveryIDs(got); len(ids) != 3 || ids[0] != "kimi-k3-2" {
		t.Fatalf("models_cn must not generate the CN catalog anymore, got %v", ids)
	}
	if *calls == 0 {
		t.Fatal("models_cn must not suppress CN discovery anymore")
	}
	st := realmModelStateFor("cn")
	if st == nil || st.Source == "pin" {
		t.Fatalf("CN source must never report a pin anymore, got %+v", st)
	}
}
