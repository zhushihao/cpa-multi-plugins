package main

import (
	"encoding/json"
	"testing"
)

// --- normalizeQoderAuthDoc: attribution stamps, everything else survives ---

func TestNormalizeQoderAuthDocClassifiesAndPreserves(t *testing.T) {
	for _, raw := range []string{
		// Nested plugin shape with panel-managed extras.
		`{"auth":{"accessToken":"old","personalToken":"pt"},"account":{"uid":"u","enterpriseId":"e"},"disabled":true,"priority":4,"prefix":"team","custom":{"id":9007199254740993}}`,
		// Legacy flat shape with pre-merge family type declarations.
		`{"type":"qoder-intl","provider":"qoderwork","accessToken":"old","disabled":true,"priority":4,"prefix":"team","custom":{"id":9007199254740993}}`,
	} {
		doc, err := normalizeQoderAuthDoc([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		var before map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &before)
		for _, key := range []string{"disabled", "priority", "prefix", "custom", "account", "auth"} {
			if want, ok := before[key]; ok && string(doc[key]) != string(want) {
				t.Fatalf("changed %s: %s -> %s", key, want, doc[key])
			}
		}
		if string(doc["auth_kind"]) != `"oauth"` || string(doc["type"]) != `"qoder"` || string(doc["provider"]) != `"qoder"` {
			t.Fatalf("classification missing: %v/%v/%v", doc["type"], doc["provider"], doc["auth_kind"])
		}
	}
	for _, raw := range []string{`null`, `[]`, `not-json{`, `{"type":"workbuddy"}`, `{"provider":"codex"}`, `{"type":42}`} {
		if _, err := normalizeQoderAuthDoc([]byte(raw)); err == nil {
			t.Fatalf("unsafe object accepted: %s", raw)
		}
	}
}

// --- mergeQoderRefreshedTokens: only the rotating pair + expiry move ---

func TestMergeQoderRefreshedTokensTouchesOnlyTokenKeys(t *testing.T) {
	for _, nested := range []bool{false, true} {
		doc := map[string]any{
			"type": "qoder", "disabled": true, "note": "operator note", "priority": 8, "prefix": "private", "custom": "keep",
			"account": map[string]any{"uid": "u", "enterpriseId": "team", "custom": "keep"},
		}
		authDoc := map[string]any{"accessToken": "old", "refreshToken": "old-refresh", "personalToken": "keep-pat", "expiresAt": 1, "domain": "qoder.com", "region": "intl", "extension": "keep"}
		if nested {
			doc["auth"] = authDoc
		} else {
			for k, v := range authDoc {
				doc[k] = v
			}
		}
		original, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		updated, err := mergeQoderRefreshedTokens(original, storedTokens{AccessToken: "new", RefreshToken: "new-refresh", ExpiresAt: 99})
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]json.RawMessage
		_ = json.Unmarshal(original, &before)
		_ = json.Unmarshal(updated, &after)
		for _, key := range []string{"disabled", "note", "priority", "prefix", "custom", "account"} {
			if string(before[key]) != string(after[key]) {
				t.Fatalf("nested=%v refresh changed %s: %s -> %s", nested, key, before[key], after[key])
			}
		}
		tokens := after
		if nested {
			tokens = nil
			_ = json.Unmarshal(after["auth"], &tokens)
		}
		if string(tokens["accessToken"]) != `"new"` || string(tokens["refreshToken"]) != `"new-refresh"` || string(tokens["expiresAt"]) != "99" {
			t.Fatalf("tokens not updated: %v/%v/%v", tokens["accessToken"], tokens["refreshToken"], tokens["expiresAt"])
		}
		for _, key := range []string{"personalToken", "domain", "region", "extension"} {
			want, _ := json.Marshal(authDoc[key])
			if string(tokens[key]) != string(want) {
				t.Fatalf("refresh dropped auth metadata %s", key)
			}
		}
		if string(after["auth_kind"]) != `"oauth"` {
			t.Fatal("refresh lost OAuth classification")
		}
	}
}

func TestMergeQoderRefreshedTokensEmptyAndUnreadableDocuments(t *testing.T) {
	fresh, err := mergeQoderRefreshedTokens(nil, storedTokens{AccessToken: "dt-a", ExpiresAt: 5})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(fresh, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["accessToken"] != "dt-a" || doc["auth_kind"] != "oauth" {
		t.Fatalf("fresh document incomplete: %v", doc)
	}
	if _, err := mergeQoderRefreshedTokens([]byte("not-json{"), storedTokens{}); err == nil {
		t.Fatal("unreadable document accepted")
	}
}

// --- runtime metadata chain carries the same classification ---

func TestQoderRuntimeMetadataClassifiesOAuth(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "dt-t", RefreshToken: "drt-t", PersonalToken: "pt-t", Domain: "qoder.com", Region: "intl"},
		Account: storedAccount{UID: "u-1"},
	}
	if md := toAuthData(sa).Metadata; md["auth_kind"] != "oauth" {
		t.Fatalf("runtime OAuth classification missing: %v", md["auth_kind"])
	}
	raw, err := buildAuthFileJSON(sa, true, "keep note", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// fork fix (世豪 2026-10-10 授权): unknown top-level fields survive rewrites.
	preserve := []byte(`{"priority":10,"weight":3,"type":"qoder","note":"old","auth":{},"model_cache":{"stale":true}}`)
	rawKeep, errKeep := buildAuthFileJSON(sa, true, "keep note", nil, preserve)
	if errKeep != nil {
		t.Fatal(errKeep)
	}
	var kept map[string]any
	if err := json.Unmarshal(rawKeep, &kept); err != nil {
		t.Fatal(err)
	}
	if kept["priority"] != float64(10) || kept["weight"] != float64(3) {
		t.Fatalf("preserve lost custom keys: priority=%v weight=%v", kept["priority"], kept["weight"])
	}
	// model_cache is maintained by models_persist outside this rebuild; the
	// rewrite must carry it forward from the physical file instead of dropping it.
	if kept["model_cache"] == nil {
		t.Fatal("preserve must carry forward model_cache")
	}
	if kept["note"] != "keep note" || kept["disabled"] != true || kept["auth_kind"] != "oauth" {
		t.Fatalf("managed keys must not come from preserve: note=%v disabled=%v auth_kind=%v", kept["note"], kept["disabled"], kept["auth_kind"])
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["auth_kind"] != "oauth" || doc["type"] != "qoder" {
		t.Fatalf("lifecycle save classification = %v/%v", doc["auth_kind"], doc["type"])
	}
	if doc["disabled"] != true || doc["note"] != "keep note" {
		t.Fatal("credential settings changed")
	}
}
