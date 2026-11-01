// oauth.go implements the AuthProvider login flow: browser-driven OAuth via
// CodeBuddy's login endpoints (CN and Global), login state polling, and token
// refresh. Each login flow gets an isolated cookie jar so multi-account flows
// never cross-contaminate session state.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// newLoginClient builds an isolated client with its own cookie jar so that the
// browser login for one state can never leak into another.
func newLoginClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: sharedHTTPClient().Transport,
		Jar:       jar,
	}
}

// doJSON sends method to fullURL with the given headers, parses the {code,msg,data}
// envelope, and returns the inner data payload. httpStatus is the upstream code.
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		loginHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		// Redirects: Go's client follows them for GET, but a 3xx that lands
		// here (e.g. POST 307/308 not re-sent, or a new upstream gateway) would
		// otherwise surface as a misleading JSON "parse failed".
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d (location: %s)", resp.StatusCode, resp.Header.Get("Location"))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, resp.StatusCode, nil
}

// loginHeaders applies the base headers plus the X-IDE-* client headers when
// the current login platform is "ide" (CodeBuddy IDE login variant).
func loginHeaders(req *http.Request) {
	commonHeaders(req)
	applyPlatformHeaders(req, currentLoginPlatform())
}

// loginHeadersFor is the realm-aware login header set: Intl (codebuddy.ai)
// logins additionally drop X-Requested-With and use the Intl IDE client
// header values (merged codebuddy-intl plugin behavior).
func loginHeadersFor(req *http.Request, region string) {
	loginHeaders(req)
	if region == regionIntl {
		// No stored auth yet at login time, so apply the Intl client header
		// set directly.
		req.Header.Del("X-Requested-With")
		req.Header.Set("X-IDE-Type", "IDE")
		req.Header.Set("X-IDE-Name", "CodeBuddy")
		req.Header.Set("X-IDE-Version", "1.100.0")
		req.Header.Set("X-Product-Version", "1.100.0")
	}
}

// authStateHeaders returns the header func for the auth/state request
// (nil keeps the historical default: doJSON falls back to loginHeaders).
func authStateHeaders(region string) func(*http.Request) {
	if region != regionIntl {
		return nil
	}
	return func(r *http.Request) { loginHeadersFor(r, region) }
}

func handleStartLogin(raw []byte) ([]byte, error) {
	return startLoginWithRegion(raw, loadedLoginRegion())
}

// startLoginWithRegion starts a login flow pinned to region. The host RPC
// entry (handleStartLogin) passes the configured login_region — the OAuth
// entry point stays single per plugin; which realm it targets is chosen in
// the plugin config (login_region dropdown) and is STICKY (v0.12.10).
func startLoginWithRegion(raw []byte, region string) ([]byte, error) {
	client := newLoginClient()
	platform := currentLoginPlatform()
	headers := authStateHeaders(region)
	stateBase := endpointAuthStateBase
	if region == regionIntl {
		// The codebuddy.ai login entry is the IDE client only (merged
		// codebuddy-intl plugin behavior).
		platform = "ide"
		stateBase = upstreamBaseForRegion(region) + "/v2/plugin/auth/state?platform="
	}
	data, _, err := doJSON(client, http.MethodPost, stateBase+platform, headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl — please restart the login flow")
	}
	loginStates.Store(st.State, &loginCtx{client: client, region: region, expires: time.Now().Add(loginTTL)})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       st.AuthURL,
		State:     st.State,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
		Metadata:  map[string]any{"logo": pluginLogoURL},
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login) — the login session was lost; please re-initiate login")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired (5 min timeout) — please re-initiate login and complete within 5 minutes")
	}

	// Single-shot poll per RPC: the host drives the polling cadence.
	// auth/token is the authoritative login-status endpoint: the application
	// layer returns a non-zero code ("login ing") while pending, and code 0
	// with the token bundle once complete. login/account sits behind the
	// openresty gateway and is rejected (401) until login finishes, so probe
	// token first and only fetch account once we hold a bearer.
	tokenBase := endpointAuthToken
	var tokenHeaders func(*http.Request)
	if lc.region == regionIntl {
		tokenBase = upstreamBaseForRegion(lc.region) + "/v2/plugin/auth/token?state="
		tokenHeaders = func(r *http.Request) { loginHeadersFor(r, lc.region) }
	}
	tokRaw, status, errTok := doJSON(lc.client, http.MethodGet, tokenBase+state, tokenHeaders, nil)
	if errTok != nil {
		// Transport-level failures and 5xx are real errors, not "still waiting":
		// surface them so the user sees a failure instead of polling until TTL.
		if status == 0 || status >= 500 {
			loginStates.Delete(state)
			return nil, fmt.Errorf("poll: token endpoint error: %w — upstream may be temporarily unavailable; retry in a few minutes", errTok)
		}
		// 4xx / business-code responses mean the login is still pending.
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}

	acct, acctErr := fetchLoginAccount(lc.client, lc.region, state, tok.AccessToken)
	if strings.TrimSpace(acct.UID) == "" {
		// Identity fallback: pull the uid out of the access token's JWT
		// claims when the account endpoint stayed empty after the retry
		// ladder (upstream outage or response-shape change).
		acct.UID = uidFromTokenClaims(tok.AccessToken)
	}
	if strings.TrimSpace(acct.UID) == "" {
		// v0.9.43: never mint a UID-less credential. The old behavior
		// silently continued with an empty account, which made
		// toAuthDataOptsWithNote fall back to the legacy single-account
		// filename workbuddy.json — a name the panel's family filter
		// does not list. The login looked successful on both the
		// upstream page AND the CPA UI (the host had saved the file),
		// yet no credential card ever appeared. Failing the poll
		// surfaces the real cause in the CPA UI instead.
		loginStates.Delete(state)
		if acctErr == nil {
			acctErr = fmt.Errorf("account payload carried no uid")
		}
		return nil, fmt.Errorf("login: upstream account endpoint returned no uid after retries (%v) — credential withheld; please retry login", acctErr)
	}

	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			Domain:       tok.Domain,
		},
		Account: storedAccount{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}
	// Pin the realm for Intl logins whose token response omitted the domain
	// (gateway routing depends on it).
	if lc.region == regionIntl && strings.TrimSpace(sa.Auth.Domain) == "" {
		sa.Auth.Domain = "codebuddy.ai"
	}
	// Pin the region explicitly (v0.12.15): credential-manager notes and
	// labels resolve via accountRegion; domain sniffing is only a legacy
	// fallback. Global accounts never go through login (panel import).
	sa.Auth.Region = lc.region
	// v0.9.44: persist the credential plugin-side BEFORE reporting success.
	// The host saves the poll's Auth payload itself, but that save rides the
	// login dialog's lifecycle — mimo hit the same failure class (v0.2.10:
	// "login complete" shown while the credential never landed). Persisting
	// here makes "poll success" imply "credential file exists under the
	// canonical name" regardless of what the host does next; the host's own
	// save converges on the same FileName (same record key, no duplicate).
	// Non-fatal: the host save stays primary; a bridge hiccup here only
	// costs the redundancy, never the login.
	persistLoginCredential(sa)
	loginStates.Delete(state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

// persistLoginCredential writes the just-minted login credential to the host
// auth store under the canonical file-layer name (authFileNameFor — intl
// accounts region-qualified). Best-effort: failures are logged and swallowed
// because the host's post-poll save remains the primary persistence path.
func persistLoginCredential(sa *storedAuth) {
	raw, err := buildAuthFileJSON(sa, false, displayNoteWithPrev(sa, nil, false, ""), nil)
	if err != nil {
		log.Printf("workbuddy: login persist (plugin-side) marshal failed: %v", err)
		return
	}
	// Fail-closed revival (round-3 P0): the plugin-side save runs FIRST; the
	// removal record is cleared only afterwards and only with a verified
	// canonical read-back. The host's own post-poll primary save is not
	// observable here, so a failed plugin-side save is NOT treated as a reliable
	// revival — the identity stays blocked until a verified write (import or a
	// later successful login). G07 boundary: not claimed as reliable.
	if err := hostAuthPersistFn(authFileNameFor(sa), "", raw); err != nil {
		log.Printf("workbuddy: login persist (plugin-side) failed — host save remains primary; removal record NOT cleared (fail-closed): %v", err)
		return
	}
	if cleared, reason := wbClearTombstoneAfterVerifiedCanonicalWrite(sa); cleared {
		log.Printf("workbuddy: login revival verified (%s) — removal record cleared", authFileNameFor(sa))
	} else if reason != "" {
		log.Printf("workbuddy: login persist wrote %s but %s", authFileNameFor(sa), reason)
	}
}

// loginAcctRetryDelays bounds the in-poll retry ladder for the account
// fetch. Documented upstream race (see handlePollLogin): login/account sits
// behind the openresty gateway and is rejected (401) for a short moment
// right after auth/token succeeds. Var so tests can collapse the wait.
var loginAcctRetryDelays = []time.Duration{400 * time.Millisecond, 800 * time.Millisecond}

// fetchLoginAccount fetches the login/account payload for a completed token
// exchange, retrying transient failures. An empty-UID payload counts as a
// failure — the caller decides whether to fall back or fail the login.
func fetchLoginAccount(client *http.Client, region, state, accessToken string) (accountData, error) {
	acctBase := endpointLoginAcct
	if region == regionIntl {
		acctBase = upstreamBaseForRegion(region) + "/v2/plugin/login/account?state="
	}
	return fetchLoginAccountAt(client, acctBase, region, state, accessToken)
}

// fetchLoginAccountAt is fetchLoginAccount against an explicit base URL
// (test seam: endpointLoginAcct is a package const).
func fetchLoginAccountAt(client *http.Client, acctBase, region, state, accessToken string) (accountData, error) {
	headers := func(r *http.Request) {
		loginHeaders(r)
		r.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if region == regionIntl {
		headers = func(r *http.Request) {
			loginHeadersFor(r, region)
			r.Header.Set("Authorization", "Bearer "+accessToken)
		}
	}
	var acct accountData
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if attempt > len(loginAcctRetryDelays) {
				return acct, lastErr
			}
			time.Sleep(loginAcctRetryDelays[attempt-1])
		}
		acctRaw, status, err := doJSON(client, http.MethodGet, acctBase+state, headers, nil)
		if err == nil {
			if parsed, ok := parseLoginAccount(acctRaw); ok {
				return parsed, nil
			}
			lastErr = fmt.Errorf("http %d: payload carried no uid", status)
			continue
		}
		lastErr = err
	}
}

// parseLoginAccount tolerates the account payload shapes seen across realms
// and gateway versions: flat {"uid":...}, numeric uid, and nested
// {"user":{...}} / {"account":{...}} wrappers. Previously a shape change
// silently produced an empty account (and via the bare-filename fallback, an
// invisible credential).
func parseLoginAccount(raw json.RawMessage) (accountData, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return accountData{}, false
	}
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flat); err != nil {
		return accountData{}, false
	}
	if acct, ok := accountDataFromMap(flat); ok {
		return acct, true
	}
	for _, key := range []string{"user", "account", "info", "profile"} {
		nested, exists := flat[key]
		if !exists {
			continue
		}
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(nested, &inner); err != nil {
			continue
		}
		if acct, ok := accountDataFromMap(inner); ok {
			return acct, true
		}
	}
	return accountData{}, false
}

// accountDataFromMap extracts accountData tolerating string or numeric uid.
func accountDataFromMap(m map[string]json.RawMessage) (accountData, bool) {
	var acct accountData
	uid, ok := jsonStringish(m["uid"])
	if !ok || strings.TrimSpace(uid) == "" {
		return accountData{}, false
	}
	acct.UID = uid
	if v, ok := jsonStringish(m["enterpriseId"]); ok {
		acct.EnterpriseID = v
	}
	if v, ok := jsonStringish(m["nickname"]); ok {
		acct.Nickname = v
	}
	return acct, true
}

// jsonStringish decodes a JSON value that may be a string or a number into
// a string. Upstream gateways have been observed flipping uid between both.
func jsonStringish(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), true
	}
	return "", false
}

// uidFromTokenClaims is the last-resort identity fallback: decode the access
// token's JWT payload and return the first claim that looks like an account
// id. Returns "" when the token is not a JWT carrying any recognizable
// claim. Standard `sub` values that are URIs or emails are skipped — they
// are subject identifiers, not CodeBuddy account ids.
func uidFromTokenClaims(accessToken string) string {
	parts := strings.Split(strings.TrimSpace(accessToken), ".")
	if len(parts) < 2 {
		return ""
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	for _, key := range []string{"uid", "user_id", "userId", "sub"} {
		v, ok := jsonStringish(claims[key])
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" || strings.Contains(v, "://") || strings.Contains(v, "@") || strings.Contains(v, " ") {
			continue
		}
		return v
	}
	return ""
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	// Route via host.http.do so request-log captures the refresh call (H2
	// compliance: was doJSON(sharedHTTPClient()) — bypassed host transport
	// policy + logging for the X-Refresh-Token endpoint).
	data, raw2, status, err := refreshCall(sa)
	if err != nil {
		if status >= 400 {
			return nil, fmt.Errorf("refresh rejected (HTTP %d)", status)
		}
		return nil, fmt.Errorf("refresh: %w", err)
	}
	_ = raw2
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken in response — the refresh token may be expired; re-login required")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = preserveExpiry(
		time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second).Unix(),
		sa.Auth.ExpiresAt,
	)
	// No explicit host.auth.save here: the host's auth Manager persists the
	// refreshed credential itself after Refresh returns (conductor.go
	// refreshAuth → m.Update → persist). Writing from the plugin too would
	// double-write the file.
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthDataForRefresh(sa)})
}

// preserveExpiry reuses the previous token's expiresAt when the refresh
// response omits expiresIn (some CodeBuddy deployments return only the token
// pair). Zero would tell the host the credential is permanently expired and
// trigger a refresh storm on every request.
func preserveExpiry(newExpiry, oldExpiry int64) int64 {
	if newExpiry > 0 {
		return newExpiry
	}
	return oldExpiry
}

// toAuthDataForRefresh returns AuthData with FileName left EMPTY so the CPA
// host backfills the original auth.FileName (auth_provider.go:371).
//
// CPA uses FileName (relative to auth dir) as auth ID. If we set it to
// "workbuddy-<uid>.json" while the original file was "workbuddy.json"
// (legacy single-account name), the host treats it as a rename, writes a
// NEW file, and the old one stays → duplicate auth records.
//
// Returning empty FileName = "keep what you had" → no rename, no dup.
func toAuthDataForRefresh(sa *storedAuth) pluginapi.AuthData {
	ad := toAuthDataOpts(sa, nil, false)
	ad.FileName = "" // let host backfill original
	ad.ID = ""       // let host compute from path (prevents ID mismatch dupes)
	return ad
}
