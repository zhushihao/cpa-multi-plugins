// session_stick.go implements first-user-message session stickiness with a
// plugin-only dual-hook bridge, and the safety fences around it.
//
// Why a bridge at all: the host's SchedulerPickRequest carries no request body
// (sdk/pluginapi/types.go SchedulerPickRequest: Plugin/Provider/Providers/
// Model/Stream/Options/Candidates only; sdk/cliproxy/auth/conductor_selection.go
// schedulerOptions() copies just Headers+Metadata), so the scheduler hook alone
// cannot see the conversation. The host does offer a request interceptor that
// runs before credential selection with the full Body and lets it rewrite
// Headers (sdk/api/handlers/handlers_interceptors.go applyRequestInterceptorsBeforeAuth
// sets opts.Headers from the interceptor response, and those same opts feed
// pickViaPluginScheduler). This file turns that into:
//
//	intercept_before: first user text -> sha256[:16] -> signed private header
//	Scheduler.Pick:   verify signature -> sticky account (30m rolling, 5m GC)
//
// Safety fences (design §WorkBuddy 路由/账本):
//   - scope: only models this plugin advertises (bare ID; Issue #6 owner
//     correction — the round4 realm namespaces cn:/global:/intl: are removed,
//     so a prefixed name is never this plugin's) are touched; everything else
//     returns an empty response so non-WorkBuddy requests stay byte-identical;
//   - forgery: the wire value is never trusted — it must carry an
//     HMAC-SHA256 signature made with a per-process random secret; unsigned or
//     tampered values are ignored by the scheduler;
//   - no leak: a forged private header is cleared on non-scoped requests and in
//     the after-auth hook, so it cannot reach another provider's upstream.
package main

import (
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbSessionHeader is the plugin-private bridge header. It is not a public
// contract: it never leaves the host process with a plugin-generated value
// unverified, and it is cleared before any upstream request.
const wbSessionHeader = "X-Workbuddy-Session-Key"

const (
	// wbStickyTTL is the rolling idle TTL of a session→account binding.
	wbStickyTTL = 30 * time.Minute
	// wbStickyGCInterval is the background sweep cadence.
	wbStickyGCInterval = 5 * time.Minute
	// wbSessionKeyPrefix marks fallback keys derived from the first user text.
	wbSessionKeyPrefix = "fb:"
)

// -----------------------------------------------------------------------------
// Advertised-model scope registry
// -----------------------------------------------------------------------------

var wbAdvertised = struct {
	sync.RWMutex
	models map[string]struct{}
}{models: map[string]struct{}{}}

// wbAdvertisedCap bounds the registry; model catalogs are small (tens of IDs)
// and this only guards against pathological churn.
const wbAdvertisedCap = 512

// wbRecordAdvertisedModels remembers every model ID the plugin advertises via
// model.for_auth, so the interceptor can prove "this model routes to us".
func wbRecordAdvertisedModels(models []pluginapi.ModelInfo) {
	if len(models) == 0 {
		return
	}
	wbAdvertised.Lock()
	defer wbAdvertised.Unlock()
	for _, m := range models {
		for _, id := range []string{m.ID, m.Name} {
			id = strings.ToLower(strings.TrimSpace(id))
			if id != "" {
				wbAdvertised.models[id] = struct{}{}
			}
		}
	}
	if len(wbAdvertised.models) > wbAdvertisedCap {
		// Keep the registry bounded: drop into a deterministic trimmed set.
		ids := make([]string, 0, len(wbAdvertised.models))
		for id := range wbAdvertised.models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		trimmed := map[string]struct{}{}
		for _, id := range ids[len(ids)-wbAdvertisedCap:] {
			trimmed[id] = struct{}{}
		}
		wbAdvertised.models = trimmed
	}
}

// wbAdvertisedModelSnapshot / wbRestoreAdvertisedModels exist for tests.
func wbAdvertisedModelSnapshot() map[string]struct{} {
	wbAdvertised.RLock()
	defer wbAdvertised.RUnlock()
	out := make(map[string]struct{}, len(wbAdvertised.models))
	for id := range wbAdvertised.models {
		out[id] = struct{}{}
	}
	return out
}

func wbRestoreAdvertisedModels(prev map[string]struct{}) {
	wbAdvertised.Lock()
	defer wbAdvertised.Unlock()
	wbAdvertised.models = map[string]struct{}{}
	for id := range prev {
		wbAdvertised.models[id] = struct{}{}
	}
}

func wbAdvertisedContains(id string) bool {
	wbAdvertised.RLock()
	defer wbAdvertised.RUnlock()
	_, ok := wbAdvertised.models[strings.ToLower(strings.TrimSpace(id))]
	return ok
}

// wbScopedModel reports whether a model name is provably routed to this
// plugin: an advertised bare ID. Bare-only contract (Issue #6 owner
// correction): the round4 realm namespaces (cn: / global: / intl:) are removed
// — the incumbent :3002 serves those names, so claiming one here would touch
// another provider's request and leak this plugin's private header across
// providers. An advertised id that itself contains a colon stays ours (the
// advertised set is the authority).
func wbScopedModel(model string) bool {
	return wbAdvertisedContains(model)
}

// wbScopeForRequest is true when either the normalized or the client-requested
// model name is provably WorkBuddy scoped.
func wbScopeForRequest(models ...string) bool {
	for _, m := range models {
		if wbScopedModel(m) {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Session key extraction (reference: workbuddy2api internal/session)
// -----------------------------------------------------------------------------

// wbConversationKey extracts a conversation-level identifier:
// metadata.conversation_id → metadata.conversationId → conversation_id →
// conversationId → prompt_cache_key (snake preferred over camel at each level).
func wbConversationKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := wbStrOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := wbStrOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := wbStrOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := wbStrOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	return wbStrOrEmpty(obj["prompt_cache_key"])
}

func wbStrOrEmpty(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// wbHasUserID reports whether the body carries a user-level identifier
// (metadata.user_id or top-level user_id). Such clients must fall back to
// weighted rotation: the user dimension is far coarser than the upstream
// conversation cache boundary (reference P1-anti-monopoly contract).
func wbHasUserID(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if wbStrOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}
	return wbStrOrEmpty(obj["user_id"]) != ""
}

// wbFirstUserText returns the text of the first role=="user" message. Content
// may be a plain string or an array of typed parts (only "text" parts count).
// Message shapes that do not parse are skipped (a single malformed message
// must not void stickiness for the whole request). Returns "" when the body
// has no derivable text (never fabricates a key).
func wbFirstUserText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	for _, rawMsg := range obj.Messages {
		var m struct {
			Role    any             `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(rawMsg, &m); err != nil {
			continue
		}
		if !strings.EqualFold(wbStrOrEmpty(m.Role), "user") {
			continue
		}
		return wbContentText(m.Content)
	}
	return ""
}

// wbContentText flattens an OpenAI message content value into text.
func wbContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if strings.EqualFold(strings.TrimSpace(p.Type), "text") {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// wbStickyFallbackKey derives a session-level key from the first user text:
// sha256(text) first 16 bytes, hex, "fb:"-prefixed. Suppressed entirely when
// the body carries user_id.
func wbStickyFallbackKey(body []byte) string {
	if wbHasUserID(body) {
		return ""
	}
	text := wbFirstUserText(body)
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return wbSessionKeyPrefix + hex.EncodeToString(sum[:16])
}

// wbSessionKeyForBody applies the full precedence: conversation identifier
// first (user_id does not suppress it), else the first-user-text fallback
// (user_id suppresses it). Empty means "no stickiness" — the caller then uses
// the weighted pool.
func wbSessionKeyForBody(body []byte) string {
	if key := wbConversationKey(body); key != "" {
		return key
	}
	return wbStickyFallbackKey(body)
}

// -----------------------------------------------------------------------------
// Header signing (forgery resistance)
// -----------------------------------------------------------------------------

// wbHeaderSecret is a per-process random secret. Per provider process, not
// persisted: a value signed by another process (or by a client) cannot verify.
var wbHeaderSecret = func() []byte {
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		return nil // signing disabled → no stickiness rather than trusting wire data
	}
	return b
}()

// wbSignSessionKey produces "v1.<hex mac>.<key>"; "" when no secret is
// available (fail closed: the scheduler then ignores the header). The key is
// the tail so it may itself contain ':' (fallback keys do) — only the first
// two separators are structural.
func wbSignSessionKey(key string) string {
	if len(wbHeaderSecret) == 0 || key == "" {
		return ""
	}
	mac := hmac.New(sha256.New, wbHeaderSecret)
	mac.Write([]byte("wb-session|" + key))
	return "v1." + hex.EncodeToString(mac.Sum(nil)[:16]) + "." + key
}

// wbParseSignedSessionKey verifies and extracts the key. Unsigned, tampered,
// truncated or wrong-version values all fail.
func wbParseSignedSessionKey(value string) (string, bool) {
	if len(wbHeaderSecret) == 0 {
		return "", false
	}
	parts := strings.SplitN(strings.TrimSpace(value), ".", 3)
	if len(parts) != 3 || parts[0] != "v1" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	mac, key := strings.ToLower(parts[1]), parts[2]
	want := hmac.New(sha256.New, wbHeaderSecret)
	want.Write([]byte("wb-session|" + key))
	wantHex := hex.EncodeToString(want.Sum(nil)[:16])
	if len(mac) != len(wantHex) || subtle.ConstantTimeCompare([]byte(mac), []byte(wantHex)) != 1 {
		return "", false
	}
	return key, true
}

// wbHeaderValues returns every value stored under the private header,
// case-insensitively (http.Header lookups canonicalize; JSON-decoded maps do
// not, so both are covered).
func wbHeaderValues(h http.Header, name string) []string {
	if h == nil {
		return nil
	}
	var out []string
	for k, vals := range h {
		if strings.EqualFold(k, name) {
			out = append(out, vals...)
		}
	}
	return out
}

// wbPrivateHeaderVariants returns the concrete key spellings present, so
// ClearHeaders removes exactly what is there (canonical form included).
func wbPrivateHeaderVariants(h http.Header) []string {
	if h == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for k := range h {
		if strings.EqualFold(k, wbSessionHeader) && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if len(out) > 0 && !seen[wbSessionHeader] {
		out = append(out, wbSessionHeader)
	}
	return out
}

// -----------------------------------------------------------------------------
// Interceptor handlers
// -----------------------------------------------------------------------------

// wbInterceptBeforeAuth is the request.intercept_before logic. It never
// mutates the body; header changes are limited to the private header, which is
// only ever SET in the weighted routing mode (the only consumer). Forged
// private headers are stripped regardless of mode so they can never reach
// another provider's upstream.
func wbInterceptBeforeAuth(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	var resp pluginapi.RequestInterceptResponse
	present := wbPrivateHeaderVariants(req.Headers)
	scoped := wbScopeForRequest(req.Model, req.RequestedModel)
	if !scoped {
		// Non-WorkBuddy request: untouched unless a private header is present,
		// in which case it is stripped (no cross-provider leak).
		if len(present) > 0 {
			resp.ClearHeaders = present
		}
		return resp
	}
	key := wbSessionKeyForBody(req.Body)
	if key == "" || loadedSchedulerMode() != schedulerModeWeighted {
		// No derivable key, or no consumer for it (off/credits modes): drop
		// any client-supplied value and add nothing.
		if len(present) > 0 {
			resp.ClearHeaders = present
		}
		return resp
	}
	signed := wbSignSessionKey(key)
	if signed == "" {
		// Fail closed: cannot prove provenance → no stickiness signal.
		if len(present) > 0 {
			resp.ClearHeaders = present
		}
		return resp
	}
	h := http.Header{}
	h.Set(wbSessionHeader, signed)
	resp.Headers = h
	if len(present) > 0 {
		// Drop any client-supplied variants; the signed value is applied after
		// the clear, so exactly one correct value survives.
		resp.ClearHeaders = present
	}
	return resp
}

// wbInterceptAfterAuth strips the private header so it cannot travel to any
// upstream (the plugin executor builds its own headers, but other executors
// and future host paths must not inherit it either).
func wbInterceptAfterAuth(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	var resp pluginapi.RequestInterceptResponse
	if present := wbPrivateHeaderVariants(req.Headers); len(present) > 0 {
		resp.ClearHeaders = present
	}
	return resp
}

// handleRequestInterceptBefore is the RPC entry point for
// request.intercept_before.
func handleRequestInterceptBefore(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(wbInterceptBeforeAuth(req))
}

// handleRequestInterceptAfter is the RPC entry point for request.intercept_after.
func handleRequestInterceptAfter(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(wbInterceptAfterAuth(req))
}

// -----------------------------------------------------------------------------
// Sticky router (reference: workbuddy2api internal/session Router)
// -----------------------------------------------------------------------------

type wbStickyEntry struct {
	uid        string
	lastActive time.Time
}

// wbStickyRouter binds session keys to account IDs with a rolling TTL. It is
// pure in-memory (a plugin restart legitimately re-assigns; the upstream
// conversation cache is the only thing at stake, and re-assignment is safe).
type wbStickyRouter struct {
	mu         sync.Mutex
	entries    map[string]wbStickyEntry
	ttl        time.Duration
	gcInterval time.Duration
	now        func() time.Time
}

func newWbStickyRouter(now func() time.Time) *wbStickyRouter {
	if now == nil {
		now = time.Now
	}
	return &wbStickyRouter{
		entries:    map[string]wbStickyEntry{},
		ttl:        wbStickyTTL,
		gcInterval: wbStickyGCInterval,
		now:        now,
	}
}

// resolve returns the account for key, binding one if needed. available is the
// current routable account list (already filtered for disabled/exhausted by
// the caller). A binding whose account is not available is dropped and
// re-assigned (safe re-selection when an account is exhausted/disabled).
func (r *wbStickyRouter) resolve(key, model string, available []string) (string, bool) {
	_ = model // model is part of the reference signature; the caller pre-filters availability
	key = strings.TrimSpace(key)
	if r == nil || key == "" || len(available) == 0 {
		return "", false
	}
	sorted := make([]string, 0, len(available))
	seen := map[string]bool{}
	for _, id := range available {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			sorted = append(sorted, id)
		}
	}
	if len(sorted) == 0 {
		return "", false
	}
	sort.Strings(sorted) // deterministic hash targets across call sites
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[key]; ok {
		if now.Sub(e.lastActive) <= r.ttl && seen[e.uid] {
			r.entries[key] = wbStickyEntry{uid: e.uid, lastActive: now}
			return e.uid, true
		}
		delete(r.entries, key)
	}
	bound := map[string]bool{}
	for _, e := range r.entries {
		bound[e.uid] = true
	}
	idle := make([]string, 0, len(sorted))
	for _, id := range sorted {
		if !bound[id] {
			idle = append(idle, id)
		}
	}
	pool := sorted
	if len(idle) > 0 {
		pool = idle // idle-first assignment keeps parallel sessions apart
	}
	uid := pool[wbHashIndex(key, len(pool))]
	r.entries[key] = wbStickyEntry{uid: uid, lastActive: now}
	r.dropExpiredLocked(now)
	return uid, true
}

// bind force-binds key→uid (used after a weighted pick so the next turn
// converges on the account that actually served this session).
func (r *wbStickyRouter) bind(key, uid string) {
	key, uid = strings.TrimSpace(key), strings.TrimSpace(uid)
	if r == nil || key == "" || uid == "" {
		return
	}
	now := r.now()
	r.mu.Lock()
	r.entries[key] = wbStickyEntry{uid: uid, lastActive: now}
	r.mu.Unlock()
}

// unbind drops a binding (returns whether one existed).
func (r *wbStickyRouter) unbind(key string) bool {
	key = strings.TrimSpace(key)
	if r == nil || key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[key]; !ok {
		return false
	}
	delete(r.entries, key)
	return true
}

// gc sweeps expired bindings and returns how many were dropped.
func (r *wbStickyRouter) gc() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropExpiredLocked(r.now())
}

func (r *wbStickyRouter) dropExpiredLocked(now time.Time) int {
	dropped := 0
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.ttl {
			delete(r.entries, key)
			dropped++
		}
	}
	return dropped
}

// count returns the number of live bindings.
func (r *wbStickyRouter) count() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// wbHashIndex maps a key to [0,n) via sha256 (stable across processes).
func wbHashIndex(key string, n int) int {
	if n <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(key))
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(n))
}

// wbStickyPickFromHeaders reads and verifies the private header and resolves
// it against the available candidates. Unsigned/tampered values are ignored.
// A bound account that is no longer available is re-assigned by the router
// (safe re-selection); the returned key lets the caller re-bind the session
// when the cost-tier-aware pool ends up serving a different account.
func wbStickyPickFromHeaders(headers http.Header, model string, router *wbStickyRouter, available []string) (uid, key string, ok bool) {
	if router == nil || len(available) == 0 {
		return "", "", false
	}
	for _, v := range wbHeaderValues(headers, wbSessionHeader) {
		k, verified := wbParseSignedSessionKey(v)
		if !verified {
			continue
		}
		u, resolved := router.resolve(k, model, available)
		if !resolved {
			return "", "", false
		}
		return u, k, true
	}
	return "", "", false
}
