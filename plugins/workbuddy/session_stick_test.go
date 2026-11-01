package main

// session_stick_test.go — G07/G08 tests for first-user-message session
// stickiness across the plugin-only dual-hook bridge:
//
//	request.intercept_before (Body -> signed private header)
//	  -> Scheduler.Pick (Options.Headers -> sticky account)
//
// and for the hostile cases the design requires: non-WorkBuddy requests must
// not be touched, forged private headers must be rejected (and stripped), and
// the private header must never survive toward an upstream. All pure Go.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func wbTestBody(msgs ...map[string]any) []byte {
	raw, _ := json.Marshal(map[string]any{"model": "m", "messages": msgs})
	return raw
}

func wbUserMsg(text string) map[string]any {
	return map[string]any{"role": "user", "content": text}
}

// withAdvertisedModels installs the advertised-model scope set for a test and
// restores the previous set afterwards.
func withAdvertisedModels(t *testing.T, ids ...string) {
	t.Helper()
	prev := wbAdvertisedModelSnapshot()
	models := make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, pluginapi.ModelInfo{ID: id, Name: id})
	}
	wbRecordAdvertisedModels(models)
	t.Cleanup(func() { wbRestoreAdvertisedModels(prev) })
}

// withWeightedMode enables the only routing mode that consumes the session
// header (weighted), restoring the previous mode afterwards.
func withWeightedMode(t *testing.T) {
	t.Helper()
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)
}

// ---------------------------------------------------------------------------
// Key extraction
// ---------------------------------------------------------------------------

func TestWBSessionKeyConversationPriority(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"metadata snake wins", map[string]any{
			"metadata":        map[string]any{"conversation_id": "c-snake", "conversationId": "c-camel"},
			"conversation_id": "top-snake",
		}, "c-snake"},
		{"metadata camel over top-level", map[string]any{
			"metadata":        map[string]any{"conversationId": "m-camel"},
			"conversation_id": "top-snake",
		}, "m-camel"},
		{"top-level snake", map[string]any{"conversation_id": "top-snake"}, "top-snake"},
		{"top-level camel", map[string]any{"conversationId": "top-camel"}, "top-camel"},
		{"prompt_cache_key last", map[string]any{
			"prompt_cache_key": "pck",
			"messages":         []map[string]any{wbUserMsg("hello")},
		}, "pck"},
	}
	for _, tc := range cases {
		raw, _ := json.Marshal(tc.body)
		if got := wbConversationKey(raw); got != tc.want {
			t.Errorf("%s: wbConversationKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestWBSessionKey_ConversationBeatsFallback: a conversation id is returned
// even when a user_id is present (user_id only suppresses the fallback).
func TestWBSessionKey_ConversationBeatsFallback(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"conversation_id": "conv-1", "user_id": "u1"},
		"messages": []map[string]any{wbUserMsg("hello")},
		"user_id":  "u2",
	})
	if got := wbSessionKeyForBody(raw); got != "conv-1" {
		t.Fatalf("wbSessionKeyForBody = %q, want conv-1", got)
	}
}

func TestWBSessionKeyFallbackFirstUserText(t *testing.T) {
	body := wbTestBody(
		map[string]any{"role": "system", "content": "sys"},
		wbUserMsg("Build a queue"),
		wbUserMsg("second turn"),
	)
	key := wbSessionKeyForBody(body)
	if !strings.HasPrefix(key, "fb:") {
		t.Fatalf("fallback key = %q, want fb: prefix", key)
	}
	sum := sha256.Sum256([]byte("Build a queue"))
	want := "fb:" + hex.EncodeToString(sum[:16])
	if key != want {
		t.Fatalf("fallback key = %q, want %q (sha256 of FIRST user text, first 16 bytes)", key, want)
	}
	// Same body → same key; different first message → different key.
	if again := wbSessionKeyForBody(body); again != key {
		t.Fatalf("fallback key not stable: %q vs %q", again, key)
	}
	other := wbSessionKeyForBody(wbTestBody(wbUserMsg("Other")))
	if other == key {
		t.Fatal("different first user text must yield a different key")
	}
	// Content as an array of typed parts: only text parts participate.
	parts := wbTestBody(map[string]any{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "Build"},
		{"type": "text", "text": " a queue"},
	}})
	if got, want := wbSessionKeyForBody(parts), key; got != want {
		t.Fatalf("array-parts key = %q, want %q", got, want)
	}
}

func TestWBSessionKeySuppressedByUserID(t *testing.T) {
	// metadata.user_id present → no fallback key.
	metaUser := wbTestBody(wbUserMsg("hello"))
	var m map[string]any
	_ = json.Unmarshal(metaUser, &m)
	m["metadata"] = map[string]any{"user_id": "u1"}
	raw, _ := json.Marshal(m)
	if got := wbSessionKeyForBody(raw); got != "" {
		t.Fatalf("metadata.user_id must suppress fallback stickiness, got %q", got)
	}
	// top-level user_id present → no fallback key.
	topUser := wbTestBody(wbUserMsg("hello"))
	_ = json.Unmarshal(topUser, &m)
	m["user_id"] = "u2"
	raw2, _ := json.Marshal(m)
	if got := wbSessionKeyForBody(raw2); got != "" {
		t.Fatalf("top-level user_id must suppress fallback stickiness, got %q", got)
	}
}

func TestWBFirstUserTextShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain", `{"messages":[{"role":"user","content":"hi"}]}`, "hi"},
		{"skips non-user", `{"messages":[{"role":"system","content":"s"},{"role":"assistant","content":"a"},{"role":"user","content":"target"}]}`, "target"},
		{"parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{}},{"type":"text","text":"b"}]}]}`, "ab"},
		{"no messages", `{"input":"x"}`, ""},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`, ""},
		{"bad json", `{`, ""},
		{"empty body", ``, ""},
		{"non-string role skip", `{"messages":[{"role":7,"content":"x"},{"role":"user","content":"y"}]}`, "y"},
	}
	for _, tc := range cases {
		if got := wbFirstUserText([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: wbFirstUserText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Signed private header (forgery resistance)
// ---------------------------------------------------------------------------

func TestWBSessionHeaderSigning(t *testing.T) {
	signed := wbSignSessionKey("fb:abc")
	if signed == "" {
		t.Fatal("signing must be available (crypto/rand secret)")
	}
	if key, ok := wbParseSignedSessionKey(signed); !ok || key != "fb:abc" {
		t.Fatalf("parse signed = (%q,%v), want (fb:abc,true)", key, ok)
	}
	// A client-supplied raw key (no signature) must not verify.
	if _, ok := wbParseSignedSessionKey("fb:abc"); ok {
		t.Fatal("unsigned value must not verify")
	}
	// Tampered key must not verify.
	tampered := strings.Replace(signed, "fb:abc", "fb:abd", 1)
	if _, ok := wbParseSignedSessionKey(tampered); ok {
		t.Fatal("tampered key must not verify")
	}
	// Wrong version prefix must not verify.
	if _, ok := wbParseSignedSessionKey("v9" + strings.TrimPrefix(signed, "v1")); ok {
		t.Fatal("unknown version must not verify")
	}
	// Truncated MAC must not verify.
	parts := strings.SplitN(signed, ".", 3)
	if _, ok := wbParseSignedSessionKey("v1." + parts[1][:16] + "." + parts[2]); ok {
		t.Fatal("truncated MAC must not verify")
	}
	if _, ok := wbParseSignedSessionKey(""); ok {
		t.Fatal("empty value must not verify")
	}
}

// ---------------------------------------------------------------------------
// Interceptor before auth — scope safety
// ---------------------------------------------------------------------------

func wbInterceptReq(model, requested, body string, headers http.Header) []byte {
	req := pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		Model:          model,
		RequestedModel: requested,
		Stream:         false,
		Headers:        headers,
		Body:           []byte(body),
	}
	raw, _ := json.Marshal(req)
	return raw
}

func mustInterceptBefore(t *testing.T, raw []byte) []byte {
	t.Helper()
	out, err := handleRequestInterceptBefore(raw)
	if err != nil {
		t.Fatalf("handleRequestInterceptBefore: %v", err)
	}
	return out
}

func mustInterceptAfter(t *testing.T, raw []byte) []byte {
	t.Helper()
	out, err := handleRequestInterceptAfter(raw)
	if err != nil {
		t.Fatalf("handleRequestInterceptAfter: %v", err)
	}
	return out
}

func wbParseInterceptResp(t *testing.T, raw []byte) pluginapi.RequestInterceptResponse {
	t.Helper()
	var env struct {
		OK     bool                               `json:"ok"`
		Result pluginapi.RequestInterceptResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, raw)
	}
	if !env.OK {
		t.Fatal("envelope not ok")
	}
	return env.Result
}

// TestInterceptBefore_NonWorkbuddyUnchanged: a non-WorkBuddy request without
// the private header must come back byte-identical (empty response).
func TestInterceptBefore_NonWorkbuddyUnchanged(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	resp := wbParseInterceptResp(t, mustInterceptBefore(t,
		wbInterceptReq("gpt-5.4", "gpt-5.4", `{"messages":[{"role":"user","content":"hi"}]}`, nil)))
	if len(resp.Headers) != 0 || len(resp.Body) != 0 || len(resp.ClearHeaders) != 0 {
		t.Fatalf("non-WorkBuddy request must be untouched, got %+v", resp)
	}
}

// TestInterceptBefore_NonWorkbuddyStripsForgedHeader: if the client injects
// the private header on a non-WorkBuddy request it is stripped before it can
// reach another provider's upstream; body/other headers stay untouched.
func TestInterceptBefore_NonWorkbuddyStripsForgedHeader(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	h := http.Header{}
	h.Set(wbSessionHeader, "forged")
	h.Set("Authorization", "Bearer x")
	resp := wbParseInterceptResp(t, mustInterceptBefore(t,
		wbInterceptReq("gpt-5.4", "gpt-5.4", `{"messages":[]}`, h)))
	if len(resp.ClearHeaders) != 1 || !strings.EqualFold(resp.ClearHeaders[0], wbSessionHeader) {
		t.Fatalf("forged private header must be cleared, got %+v", resp.ClearHeaders)
	}
	if len(resp.Headers) != 0 {
		t.Fatalf("no replacement header expected, got %+v", resp.Headers)
	}
	if len(resp.Body) != 0 {
		t.Fatalf("body must not change, got %q", resp.Body)
	}
}

// TestInterceptBefore_WorkbuddySetsSignedKey: a scoped request with a user
// message gets exactly one signed private header and an untouched body, while
// a removed-prefix name (served by the incumbent :3002, not by this candidate)
// stays byte-identical.
func TestInterceptBefore_WorkbuddySetsSignedKey(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	withWeightedMode(t)
	body := `{"messages":[{"role":"user","content":"hello"}]}`
	resp := wbParseInterceptResp(t, mustInterceptBefore(t, wbInterceptReq("deepseek-v4.1-flash", "deepseek-v4.1-flash", body, nil)))
	vals := resp.Headers.Values(wbSessionHeader)
	if len(vals) != 1 {
		t.Fatalf("want exactly one session header, got %+v", resp.Headers)
	}
	key, ok := wbParseSignedSessionKey(vals[0])
	if !ok {
		t.Fatalf("header value must verify, got %q", vals[0])
	}
	if key != wbSessionKeyForBody([]byte(body)) {
		t.Fatalf("header key = %q, want %q", key, wbSessionKeyForBody([]byte(body)))
	}
	if len(resp.Body) != 0 {
		t.Fatalf("interceptor must not rewrite the body, got %q", resp.Body)
	}
	// A prefixed name is NOT this plugin's (Issue #6 bare-only contract): the
	// incumbent serves cn: names, so this request must not be touched.
	prefixed := wbParseInterceptResp(t, mustInterceptBefore(t, wbInterceptReq("cn:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", body, nil)))
	if len(prefixed.Headers) != 0 || len(prefixed.Body) != 0 || len(prefixed.ClearHeaders) != 0 {
		t.Fatalf("removed-prefix request must stay byte-identical, got %+v", prefixed)
	}
}

// TestInterceptBefore_WorkbuddyOverwritesForged: a forged value is replaced
// by the freshly computed signed key (never trusted).
func TestInterceptBefore_WorkbuddyOverwritesForged(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	withWeightedMode(t)
	h := http.Header{}
	h.Set(wbSessionHeader, "forged-value")
	resp := wbParseInterceptResp(t, mustInterceptBefore(t, wbInterceptReq(
		"deepseek-v4.1-flash", "deepseek-v4.1-flash",
		`{"messages":[{"role":"user","content":"hello"}]}`, h)))
	vals := resp.Headers.Values(wbSessionHeader)
	if len(vals) != 1 {
		t.Fatalf("want overwritten header, got %+v", resp.Headers)
	}
	if vals[0] == "forged-value" {
		t.Fatal("forged value must not survive")
	}
	if _, ok := wbParseSignedSessionKey(vals[0]); !ok {
		t.Fatalf("replacement must verify, got %q", vals[0])
	}
}

// TestInterceptBefore_WorkbuddyNoKeyClears: a scoped request whose body has no
// derivable key must have the forged header cleared (no stickiness signal).
func TestInterceptBefore_WorkbuddyNoKeyClears(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	withWeightedMode(t)
	h := http.Header{}
	h.Set(wbSessionHeader, "forged-value")
	resp := wbParseInterceptResp(t, mustInterceptBefore(t, wbInterceptReq(
		"deepseek-v4.1-flash", "deepseek-v4.1-flash", `{"messages":[]}`, h)))
	if len(resp.Headers) != 0 {
		t.Fatalf("no header should be set, got %+v", resp.Headers)
	}
	if len(resp.ClearHeaders) != 1 || !strings.EqualFold(resp.ClearHeaders[0], wbSessionHeader) {
		t.Fatalf("forged header must be cleared, got %+v", resp.ClearHeaders)
	}
}

// TestInterceptBefore_RemovedRealmPrefixNeverScoped: the round4 cn:/global:/
// intl: namespace is gone — a prefixed name is never treated as WorkBuddy,
// even when its base id is advertised (the incumbent :3002 serves those names;
// touching them would leak this plugin's private header across providers).
func TestInterceptBefore_RemovedRealmPrefixNeverScoped(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	withWeightedMode(t)
	for _, model := range []string{
		"cn:deepseek-v4.1-flash",
		"global:deepseek-v4.1-flash",
		"intl:deepseek-v4.1-flash",
		"cn:someone-elses-model",
	} {
		resp := wbParseInterceptResp(t, mustInterceptBefore(t, wbInterceptReq(
			model, model, `{"messages":[{"role":"user","content":"hello"}]}`, nil)))
		if len(resp.Headers) != 0 || len(resp.Body) != 0 || len(resp.ClearHeaders) != 0 {
			t.Fatalf("prefixed model %q must not be touched, got %+v", model, resp)
		}
	}
}

// TestInterceptBefore_ScopedButNoWeightedConsumer: outside weighted mode a
// scoped request must not gain a header (no consumer), while a forged value is
// still stripped.
func TestInterceptBefore_ScopedButNoWeightedConsumer(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	restore := setSchedulerMode(schedulerModeOff)
	t.Cleanup(restore)
	body := `{"messages":[{"role":"user","content":"hello"}]}`
	resp := wbParseInterceptResp(t, mustInterceptBefore(t,
		wbInterceptReq("deepseek-v4.1-flash", "deepseek-v4.1-flash", body, nil)))
	if len(resp.Headers) != 0 {
		t.Fatalf("off mode must not set the bridge header, got %+v", resp.Headers)
	}
	h := http.Header{}
	h.Set(wbSessionHeader, "forged")
	resp2 := wbParseInterceptResp(t, mustInterceptBefore(t,
		wbInterceptReq("deepseek-v4.1-flash", "deepseek-v4.1-flash", body, h)))
	if len(resp2.Headers) != 0 || len(resp2.ClearHeaders) != 1 {
		t.Fatalf("off mode must only strip a forged header, got %+v", resp2)
	}
}

// TestInterceptAfter_ClearsPrivateHeaderForScopedRequest: the private header
// must not travel toward any upstream — after-auth clears it.
func TestInterceptAfter_ClearsPrivateHeaderForScopedRequest(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	h := http.Header{}
	h.Set(wbSessionHeader, wbSignSessionKey("fb:x"))
	resp := wbParseInterceptResp(t, mustInterceptAfter(t, wbInterceptReq(
		"deepseek-v4.1-flash", "deepseek-v4.1-flash", `{}`, h)))
	if len(resp.ClearHeaders) != 1 || !strings.EqualFold(resp.ClearHeaders[0], wbSessionHeader) {
		t.Fatalf("after-auth must clear the private header, got %+v", resp.ClearHeaders)
	}
	if len(resp.Headers) != 0 {
		t.Fatalf("after-auth must not set headers, got %+v", resp.Headers)
	}
}

// ---------------------------------------------------------------------------
// Sticky router
// ---------------------------------------------------------------------------

func TestStickyRouterBindResolveTTLGC(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	r := newWbStickyRouter(func() time.Time { return clk.now() })
	avail := []string{"b", "a"} // unordered on purpose
	uid, ok := r.resolve("k1", "m1", avail)
	if !ok || (uid != "a" && uid != "b") {
		t.Fatalf("resolve = (%q,%v), want one of a/b", uid, ok)
	}
	// Second resolve returns the same binding (sticky).
	uid2, _ := r.resolve("k1", "m1", avail)
	if uid2 != uid {
		t.Fatalf("resolve changed binding: %q -> %q", uid, uid2)
	}
	// Rolling TTL: keep touching for > TTL total; entry must survive.
	clk.advance(29 * time.Minute)
	if _, ok := r.resolve("k1", "m1", avail); !ok {
		t.Fatal("entry must survive with rolling TTL")
	}
	clk.advance(29 * time.Minute)
	if got, _ := r.resolve("k1", "m1", avail); got != uid {
		t.Fatalf("rolling touch failed: %q -> %q", uid, got)
	}
	// GC beyond TTL drops the entry.
	clk.advance(31 * time.Minute)
	if n := r.gc(); n != 1 {
		t.Fatalf("gc dropped %d entries, want 1", n)
	}
	if r.count() != 0 {
		t.Fatalf("entries = %d after gc, want 0", r.count())
	}
	// And a new resolve re-assigns.
	if _, ok := r.resolve("k1", "m1", avail); !ok {
		t.Fatal("resolve after gc should reassign")
	}
}

func TestStickyRouterUnavailableReassigns(t *testing.T) {
	r := newWbStickyRouter(nil)
	uid, ok := r.resolve("k1", "m1", []string{"a", "b"})
	if !ok {
		t.Fatal("first resolve failed")
	}
	other := "a"
	if uid == "a" {
		other = "b"
	}
	// Bound account gone (exhausted/disabled) → reassign to remaining.
	got, ok := r.resolve("k1", "m1", []string{other})
	if !ok || got != other {
		t.Fatalf("resolve with bound account unavailable = (%q,%v), want %q", got, ok, other)
	}
	// No candidates → no assignment.
	if _, ok := r.resolve("k1", "m1", nil); ok {
		t.Fatal("empty candidate list must not resolve")
	}
}

func TestStickyRouterIdleFirstAndSpread(t *testing.T) {
	r := newWbStickyRouter(nil)
	avail := []string{"a", "b", "c", "d"}
	// Four distinct keys must spread over idle accounts first (no key gets
	// doubled up while an unbound account is available).
	used := map[string]int{}
	keys := []string{"k1", "k2", "k3", "k4"}
	for _, k := range keys {
		uid, ok := r.resolve(k, "m1", avail)
		if !ok {
			t.Fatalf("resolve %s failed", k)
		}
		used[uid]++
	}
	for _, uid := range avail {
		if used[uid] != 1 {
			t.Fatalf("idle-first assignment violated: %v", used)
		}
	}
	// The fifth key must go to an already-bound account (pool exhausted).
	uid, ok := r.resolve("k5", "m1", avail)
	if !ok || used[uid] != 1 {
		t.Fatalf("fifth key binding = (%q,%v), want an existing bound account", uid, ok)
	}
	// Same key always resolves to the same account.
	again, _ := r.resolve("k5", "m1", avail)
	if again != uid {
		t.Fatalf("sticky changed for k5: %q -> %q", uid, again)
	}
}

func TestStickyRouterUnbind(t *testing.T) {
	r := newWbStickyRouter(nil)
	if _, ok := r.resolve("k1", "m1", []string{"a"}); !ok {
		t.Fatal("resolve failed")
	}
	if !r.unbind("k1") {
		t.Fatal("unbind must report an existing binding")
	}
	if r.unbind("k1") {
		t.Fatal("second unbind must report no binding")
	}
}

// ---------------------------------------------------------------------------
// Header -> pick bridge (this is what handleSchedulerPick consumes)
// ---------------------------------------------------------------------------

func TestStickyPickFromHeaders(t *testing.T) {
	withAdvertisedModels(t, "deepseek-v4.1-flash")
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	router := newWbStickyRouter(func() time.Time { return clk.now() })
	h := http.Header{}
	h.Set(wbSessionHeader, wbSignSessionKey("fb:key"))
	uid, key, ok := wbStickyPickFromHeaders(h, "m1", router, []string{"a", "b"})
	if !ok || key == "" {
		t.Fatal("valid signed header must produce a sticky pick")
	}
	// Same header → same account (sticky).
	uid2, _, _ := wbStickyPickFromHeaders(h, "m1", router, []string{"a", "b"})
	if uid2 != uid {
		t.Fatalf("sticky pick changed: %q -> %q", uid, uid2)
	}
	// Forged (unsigned) header must be ignored entirely.
	forged := http.Header{}
	forged.Set(wbSessionHeader, "fb:key")
	if _, _, ok := wbStickyPickFromHeaders(forged, "m1", router, []string{"a", "b"}); ok {
		t.Fatal("unsigned header must not produce a pick")
	}
	// No header → no sticky pick.
	if _, _, ok := wbStickyPickFromHeaders(http.Header{}, "m1", router, []string{"a", "b"}); ok {
		t.Fatal("missing header must not produce a pick")
	}
	// Bound account unavailable (host removed it) → the router safely
	// re-assigns to the only remaining candidate.
	uid3, key3, ok := wbStickyPickFromHeaders(h, "m1", router, []string{"c"})
	if !ok || uid3 != "c" {
		t.Fatalf("re-selection = (%q,%v), want c", uid3, ok)
	}
	if key3 == "" {
		t.Fatal("re-selection must return the session key for re-binding")
	}
	_ = uid
}

// TestPickWithStickyTierGuard: session affinity must never bypass the cost
// tier hard filter — a session bound to a paid account is refused while a
// free-tier account exists (the caller then re-binds).
func TestPickWithStickyTierGuard(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(21))
	pool.noteCost("free", "m1", 0, 1000)
	pool.noteCost("paid", "m1", 2.0, 1000)
	cands := []wbPoolCandidate{wbCand("free", 100, 0), wbCand("paid", 100, 0)}
	for i := 0; i < 20; i++ {
		got, ok := pool.pickWithSticky("m1", cands, "paid")
		if !ok || got != "free" {
			t.Fatalf("sticky paid must be refused under tier-0 filter, got (%q,%v)", got, ok)
		}
		clk.advance(time.Minute)
	}
	// Without a tier conflict the sticky account is honored.
	if got, _ := pool.pickWithSticky("m1", cands, "free"); got != "free" {
		t.Fatalf("sticky free = %q, want free", got)
	}
	// Unknown-tier sticky account is honored while no tier-0 exists.
	if got, _ := pool.pickWithSticky("m2", cands, "paid"); got != "paid" {
		t.Fatalf("sticky paid on m2 = %q, want paid (no observations)", got)
	}
}
