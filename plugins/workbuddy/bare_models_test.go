package main

// bare_models_test.go — Issue #6 owner correction (I6-BARE-MODELS-IMPL):
// this candidate exposes and accepts BARE upstream model names ONLY.
//
// The round4 cn:/global:/intl: realm namespace (model_realm.go) is removed
// outright — not retained as a dead compatibility layer, and no static alias
// table takes its place:
//   - model.for_auth advertises the raw discovery/pin/alias-resolved ids, with
//     no prefixed variants;
//   - the chat ingress denies a prefixed name at the candidate — a clear 400
//     ("invalid_request_error"), never stripped, never forwarded upstream;
//   - the interceptor scope fence treats a prefixed name as not this plugin's
//     (the incumbent :3002 serves those names; touching them would leak this
//     plugin's private header across providers).
//
// Bare names keep the pre-round4 behavior byte-for-byte (oauth-model-alias
// resolution included).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---------------------------------------------------------------------------
// Ingress: a prefixed request is denied at the candidate
// ---------------------------------------------------------------------------

// TestIngressUpstreamModelDeniesRemovedRealmPrefix: every shape of the removed
// namespace is rejected before any upstream work — nothing is stripped, nothing
// is resolved through the alias table, nothing is forwarded.
func TestIngressUpstreamModelDeniesRemovedRealmPrefix(t *testing.T) {
	// If the implementation wrongly stripped the prefix (round4 behavior) this
	// alias would resolve; the deny must happen before alias resolution.
	setModelAliasesForTest(t, map[string]string{"glm-5.3": "must-not-apply"})
	cases := []struct{ in, prefix string }{
		{"cn:glm-5.3", "cn:"},
		{"CN:Glm-5.3", "cn:"}, // prefix match is case-insensitive
		{"  cn:  glm-5.3  ", "cn:"},
		{"cn:", "cn:"}, // no id after the prefix
		{"global:hy4-preview", "global:"},
		{"global:   ", "global:"},
		{"intl:fast-model", "intl:"},
		{"intl:", "intl:"},
	}
	for _, c := range cases {
		got, err := wbIngressUpstreamModel(c.in, nil)
		if err == nil {
			t.Fatalf("wbIngressUpstreamModel(%q) must deny a removed realm prefix, got %q", c.in, got)
		}
		if got != "" {
			t.Errorf("wbIngressUpstreamModel(%q) must not resolve a model, got %q", c.in, got)
		}
		if !strings.Contains(err.Error(), "invalid_request_error") {
			t.Errorf("wbIngressUpstreamModel(%q) error must carry invalid_request_error so the host treats it as a client failure: %v", c.in, err)
		}
		if !strings.Contains(err.Error(), c.prefix) {
			t.Errorf("wbIngressUpstreamModel(%q) error must name the removed prefix %q: %v", c.in, c.prefix, err)
		}
		var sc interface{ StatusCode() int }
		if !errors.As(err, &sc) || sc.StatusCode() != http.StatusBadRequest {
			t.Errorf("wbIngressUpstreamModel(%q) must carry HTTP 400, got %v", c.in, err)
		}
	}
}

// TestIngressUpstreamModelBareNamesUnchanged: a name without a removed
// namespace is handled exactly as before the namespace existed — alias
// resolution and trimming included; a colon that is not one of the removed
// namespaces stays upstream's business.
func TestIngressUpstreamModelBareNamesUnchanged(t *testing.T) {
	setModelAliasesForTest(t, map[string]string{"wb-alias": "glm-5.3"})
	cases := []struct{ in, want string }{
		{"wb-alias", "glm-5.3"},          // oauth-model-alias resolution unchanged
		{"glm-5.3", "glm-5.3"},           // bare id byte-identical
		{"  glm-5.3  ", "glm-5.3"},       // trimmed, as before
		{"glm-5.3:beta", "glm-5.3:beta"}, // colon ids are not namespaces
		{"us:glm-5.3", "us:glm-5.3"},     // only the REMOVED namespaces are denied
		{"", ""},
	}
	for _, c := range cases {
		got, err := wbIngressUpstreamModel(c.in, nil)
		if err != nil {
			t.Errorf("wbIngressUpstreamModel(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("wbIngressUpstreamModel(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

// TestIngressHandlersDenyPrefixedModelBeforeAnyUpstreamWork is the handler-level
// contract: both chat entry points deny a removed-prefix model at the top of
// the candidate, before any credential/host work (no host bridge is installed
// in this test — reaching it would surface a different failure).
func TestIngressHandlersDenyPrefixedModelBeforeAnyUpstreamWork(t *testing.T) {
	const storage = `{"type":"workbuddy","auth":{"accessToken":"tok","refreshToken":"ref"},"account":{"uid":"u-deny","nickname":"n"}}`
	exec := pluginapi.ExecutorRequest{
		AuthID:      "auth-denied",
		Model:       "cn:glm-5.3",
		StorageJSON: []byte(storage),
		Payload:     []byte(`{"model":"cn:glm-5.3","messages":[{"role":"user","content":"hi"}]}`),
	}
	if _, err := handleExecExecute(mustMarshal(t, exec)); err == nil {
		t.Fatal("handleExecExecute must deny a removed-prefix model at the candidate")
	} else if !strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("execute deny must be a client request error, got %v", err)
	}

	streamReq := executorStreamRequest{ExecutorRequest: exec}
	if _, err := handleExecStream(mustMarshal(t, streamReq)); err == nil {
		t.Fatal("handleExecStream must deny a removed-prefix model at the candidate")
	} else if !strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("stream deny must be a client request error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Advertisement: bare ids only
// ---------------------------------------------------------------------------

func modelIDs(ms []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustModelForAuth(t *testing.T, req pluginapi.AuthModelRequest) pluginapi.ModelResponse {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal AuthModelRequest: %v", err)
	}
	out, err := handleModelForAuth(raw)
	if err != nil {
		t.Fatalf("handleModelForAuth: %v", err)
	}
	var env struct {
		OK     bool                    `json:"ok"`
		Result pluginapi.ModelResponse `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, out)
	}
	if !env.OK {
		t.Fatalf("handleModelForAuth failed: %s", out)
	}
	return env.Result
}

// TestHandleModelForAuthAdvertisesBareNamesOnly: a CN credential and an Intl
// credential both advertise exactly the discovered ids — no cn:/global:/intl:
// variant, in neither ID nor display name — and the scope fence accepts the
// bare id while rejecting the removed prefix even though its base is advertised
// (the incumbent :3002 serves the prefixed form; this plugin must not touch it).
func TestHandleModelForAuthAdvertisesBareNamesOnly(t *testing.T) {
	resetDynamicModelsCache()
	t.Cleanup(resetDynamicModelsCache)
	origDiscover := discoverModelsFn
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return realmTestModels("glm-5.3", "auto"), nil
	}
	t.Cleanup(func() { discoverModelsFn = origDiscover })

	prevAdvertised := wbAdvertisedModelSnapshot()
	t.Cleanup(func() { wbRestoreAdvertisedModels(prevAdvertised) })

	want := []string{"glm-5.3", "auto"}
	for _, credential := range []struct {
		name    string
		storage string
	}{
		{"CN", `{"accessToken":"tok-cn","region":"cn"}`},
		{"Intl", `{"accessToken":"tok-intl","region":"intl"}`},
	} {
		resp := mustModelForAuth(t, pluginapi.AuthModelRequest{
			AuthID:       "auth-" + credential.name,
			AuthProvider: providerName,
			StorageJSON:  []byte(credential.storage),
		})
		if resp.Provider != providerName {
			t.Fatalf("%s provider = %q", credential.name, resp.Provider)
		}
		if got := modelIDs(resp.Models); !equalIDs(got, want) {
			t.Fatalf("%s advertisement = %v want %v (bare ids only, no prefixed variants)", credential.name, got, want)
		}
		for _, m := range resp.Models {
			for _, p := range []string{"cn:", "global:", "intl:"} {
				if strings.HasPrefix(strings.ToLower(m.ID), p) || strings.HasPrefix(strings.ToLower(m.Name), p) {
					t.Errorf("%s advertisement carries the removed realm namespace: %+v", credential.name, m)
				}
			}
		}
	}

	if !wbScopedModel("glm-5.3") {
		t.Error("a bare advertised id must still be scoped to this plugin")
	}
	if wbScopedModel("cn:glm-5.3") {
		t.Error("a removed-prefix name must never be scoped to this plugin, even when its base is advertised")
	}
}
