// authfile.go owns every physical auth-file path the plugin touches: the
// workbuddy-<uid>.json naming rule, UID sanitization (path-traversal defense),
// path safety checks, and the read / write / delete helpers that talk to the
// host's auth store via host.auth.* RPC. Callers above (lifecycle reconcile)
// decide when to disable / re-enable / delete; this file decides how.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authFileNameFor matches toAuthData naming: always workbuddy-<uid>.json when UID is known.
// Bare "workbuddy.json" is legacy single-account only (no UID).
var unsafeUIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeUIDForFileName(uid string) string {
	uid = strings.TrimSpace(uid)
	uid = unsafeUIDChars.ReplaceAllString(uid, "_")
	if uid == "" || uid == "." || uid == ".." {
		return ""
	}
	if len(uid) > 64 {
		uid = uid[:64]
	}
	return uid
}

func authFileNameFor(sa *storedAuth) string {
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			// Intl accounts (merged codebuddy-intl) keep a region-qualified name
			// so they never collide with a CN/Global account of the same uid.
			if isIntlDomain(sa.Auth.Domain) {
				return "workbuddy-intl-" + uid + ".json"
			}
			return "workbuddy-" + uid + ".json"
		}
	}
	return authFileName
}

// isLegacyWorkbuddyAuthName reports the historical single-file name that collides
// with multi-account workbuddy-<uid>.json for the same credential.

func isLegacyWorkbuddyAuthName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), authFileName)
}

// resolveAuthFileTarget picks the canonical file name + path for save/delete.
// Prefer workbuddy-<uid>.json; if the host still points at legacy workbuddy.json
// for a UID-bearing account, the caller writes the canonical name and reports
// the legacy path. I6: the legacy file is PRESERVED by
// hostAuthPersistMigrate (non-destructive) — the returned legacyPath is for
// logging/observability only, never a delete target.
//
// Round-2 review hardening: the host-reported name is only reused when it is
// neither a legacy workbuddy name nor a preserved legacy codebuddy-cn / intl
// SOURCE, and when the physical document carries the SAME account identity as
// sa. A drifted index→file mapping therefore can never redirect a write onto
// another account's file or over a preserved source.
func resolveAuthFileTarget(sa *storedAuth, phys *hostAuthPhysical) (name, path string, legacyPath string) {
	name = authFileNameFor(sa)
	if phys == nil {
		return name, "", ""
	}
	path = strings.TrimSpace(phys.Path)
	physName := strings.TrimSpace(phys.Name)
	if physName != "" && physNameSafeToReuse(physName, phys.JSON, sa) {
		// Already on multi-account name — keep host name (should match uid form).
		name = physName
	}
	if isLegacyWorkbuddyAuthName(physName) || isLegacyWorkbuddyAuthName(filepath.Base(path)) {
		if sa != nil && strings.TrimSpace(sa.Account.UID) != "" {
			// Migrate: write the canonical name; hostAuthPersistMigrate
			// keeps the legacy file in place (I6, 旧凭据不回写不删除).
			legacyPath = path
		}
	}
	return name, path, legacyPath
}

// physNameSafeToReuse reports whether the host-reported file name may be used as
// the write target: it must not be a preserved legacy source (workbuddy.json /
// codebuddy-cn-* / codebuddy-intl-*), and the physical credential must parse
// and carry the same identity (uid × realm) as sa.
func physNameSafeToReuse(physName string, physJSON []byte, sa *storedAuth) bool {
	if isLegacyWorkbuddyAuthName(physName) || isLegacyCodebuddyAuthName(physName) || isLegacyCodebuddyIntlAuthName(physName) {
		return false
	}
	physSA, err := parseStored(physJSON)
	if err != nil || physSA == nil {
		return false // unknown physical credential: fall back to the canonical name
	}
	want := wbAuthIdentityOf(sa).key()
	got := wbAuthIdentityOf(physSA).key()
	return want != "" && want == got
}

// hostAuthPersist saves via host API only. Dual-writing the physical path after
// a successful host.auth.save is redundant (host already WriteFile) and can
// re-fire the watcher → extra re-parse / transient dual registration risk.

type hostAuthPhysical struct {
	AuthIndex string
	Name      string
	Path      string
	JSON      []byte
	Disabled  bool
}

// hostAuthGetPhysicalFn / hostAuthPersistMigrateFn are indirection points so
// tests can exercise note-writing paths without a live host RPC bridge. Both
// default to the real host calls below.
var (
	hostAuthGetPhysicalFn    = hostAuthGetPhysical
	hostAuthPersistMigrateFn = hostAuthPersistMigrate
	// hostAuthPersistFn is the write seam for the login success path
	// (persistLoginCredential): tests stub it to record the save instead of
	// touching the host bridge. The non-destructive adopt path uses it too.
	// The auth-list seam (hostAuthListFn) lives in models_persist.go and is
	// shared by the adopt path.
	hostAuthPersistFn = hostAuthPersist
)

func hostAuthGetPhysical(authIndex string) (*hostAuthPhysical, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get: bad envelope")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return &hostAuthPhysical{
		AuthIndex: resp.AuthIndex,
		Name:      resp.Name,
		Path:      resp.Path,
		JSON:      resp.JSON,
		Disabled:  parseDisabledFromAuthJSON(resp.JSON),
	}, nil
}

// hostAuthSaveJSON persists credential JSON via host.auth.save.

func hostAuthPersist(name, path string, raw []byte) error {
	_ = path // reserved for callers that still pass physical path for migrate logic
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	return hostAuthSaveJSON(name, raw)
}

// hostAuthPersistMigrate persists the canonical credential document. When the
// account was historically stored under the bare legacy name
// (workbuddy.json), the legacy file is PRESERVED IN PLACE.
//
// I6 non-destructive policy: this function used to delete the legacy
// workbuddy.json after the canonical workbuddy-<uid>.json save. That deletion
// is rollback-hostile — the auth directory can be shared with the
// still-running :3002 gateway, and the design's rollback path depends on the
// pre-change credential files staying intact (旧凭据不回写不删除). Accepted
// bounded cost: the host may register the same credential twice (both files
// carry identical tokens); every state machine on this path is idempotent
// (travel/check-in skip when already done, lifecycle notes de-duplicated), so
// the duplicate is inert. Retiring the legacy file is an operator decision
// after the staged cutover, never an automatic write here.
func hostAuthPersistMigrate(name, path, legacyPath string, raw []byte) error {
	if err := hostAuthPersistFn(name, path, raw); err != nil {
		return err
	}
	if legacyPath != "" && isLegacyWorkbuddyAuthName(filepath.Base(legacyPath)) {
		log.Printf("auth migrate: %s written; legacy credential preserved in place at %s (non-destructive)", name, legacyPath)
	}
	return nil
}

// buildAuthFileJSON produces host-save payload: nested storage + top-level metadata.
// extra merges additional top-level keys (optional).

func hostAuthSaveJSON(name string, raw []byte) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	// Every physical write carries the OAuth attribution and keeps foreign
	// or malformed documents out (write-side twin of handleParseAuth's
	// read-side ownership check).
	doc, err := normalizeWorkbuddyAuthDoc(raw)
	if err != nil {
		return err
	}
	if raw, err = json.Marshal(doc); err != nil {
		return err
	}
	// v0.9.41: typed buildAuthFileJSON rebuilds (lifecycle notes, adopt,
	// import) construct a fresh document and would silently drop the
	// persisted model_cache snapshot (v0.9.38) — every credits-note churn
	// wiped it, and the next discovery outage advertised the static-less
	// nothing. Re-inject whitelisted plugin-stamped keys the caller didn't
	// set but the physical file carries.
	raw = preservePluginDocKeys(name, raw)
	saveReq := pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: raw,
	}
	saveBody, _ := json.Marshal(saveReq)
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return fmt.Errorf("host.auth.save: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		msg := "host.auth.save failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = truncateRedacted(env.Error.Message, 200)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// lifecycleStateUnchanged avoids redundant saves when note/disabled unchanged.

func buildAuthFileJSON(sa *storedAuth, disabled bool, note string, extra map[string]any) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	var nested map[string]any
	if err := json.Unmarshal(storage, &nested); err != nil {
		return nil, err
	}
	out := map[string]any{
		"type":     providerName,
		"provider": providerName,
		"logo":     pluginLogoURL,
		"disabled": disabled,
		"note":     note,
		"auth":     nested["auth"],
		"account":  nested["account"],
	}
	for k, v := range extra {
		out[k] = v
	}
	// extra must not be able to strip the classification.
	out["auth_kind"] = "oauth"
	return json.Marshal(out)
}

// parseDisabledFromAuthJSON reads top-level disabled from physical auth JSON.

func parseDisabledFromAuthJSON(raw []byte) bool {
	var m struct {
		Disabled bool `json:"disabled"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Disabled
}

// priorityDocKey is the top-level routing-tier key external tools write into
// auth files (CPA panel, CPA-Manager-Plus batch priority, hand edits). The
// plugin never authors it — it must only survive plugin rewrites and reach
// the host record (see preservePluginDocKeys / handleParseAuth).
const priorityDocKey = "priority"

// priorityFromJSON extracts a top-level "priority" from a credential payload.
// The host accepts both JSON numbers and numeric strings for the key (see the
// management auth-files writer / file synthesizer), so both are accepted
// here; anything else reports "absent" rather than guessing a value.
func priorityFromJSON(raw []byte) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var probe struct {
		Priority any `json:"priority"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return 0, false
	}
	switch v := probe.Priority.(type) {
	case float64:
		return int(v), true
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// isSafeWorkbuddyAuthPath rejects non-workbuddy filenames, empty paths, and
// traversal attempts. It validates both the basename pattern AND that the path
// does not escape via ".." segments. Callers that need to confine deletes to
// a specific directory should additionally check isPathUnder(path, dir).

func isSafeWorkbuddyAuthPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	// Reject any path containing ".." — prevents traversal regardless of basename.
	if strings.Contains(filepath.ToSlash(path), "../") || strings.Contains(filepath.ToSlash(path), "/..") {
		return false
	}
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	isCodebuddy := strings.HasPrefix(lower, "codebuddy-cn-") || lower == "codebuddy-cn.json"
	if !strings.HasPrefix(lower, "workbuddy-") && lower != "workbuddy.json" && !isCodebuddy {
		return false
	}
	if !strings.HasSuffix(lower, ".json") {
		return false
	}
	// Path traversal / absolute weirdness: base must equal cleaned base.
	if base != filepath.Base(filepath.Clean(path)) {
		return false
	}
	return true
}

// isPathUnder reports whether path is inside dir (after cleaning both).
// Empty dir means "no constraint" (returns true for any safe path).

func isPathUnder(path, dir string) bool {
	path = strings.TrimSpace(path)
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return true
	}
	cleanPath := filepath.Clean(path)
	cleanDir := filepath.Clean(dir)
	if cleanPath == cleanDir {
		return false // path is the dir itself, not under it
	}
	rel, err := filepath.Rel(cleanDir, cleanPath)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator)+"..")
}

// deleteAuthFileAt removes a workbuddy auth file. Missing file is success.
// Deprecated: use deleteAuthFileInDir instead (adds directory + absolute path
// confinement). Retained for test coverage of the base safe-delete path.

func deleteAuthFileAt(path string) error {
	if !isSafeWorkbuddyAuthPath(path) {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// deleteAuthFileInDir is like deleteAuthFileAt but additionally requires the
// path to be under dir. Use for lifecycle deletes where the auth directory is
// known — prevents a malicious/buggy host path from deleting arbitrary files.
// The path MUST be absolute (defense against relative-path CWD deletion).

func deleteAuthFileInDir(path, dir string) error {
	if !isSafeWorkbuddyAuthPath(path) {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("refusing to delete relative path: %s", path)
	}
	if dir != "" && !isPathUnder(path, dir) {
		return fmt.Errorf("refusing to delete path outside auth dir: %s (dir=%s)", path, dir)
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// hostAuthGetFull returns physical JSON, path, and name for an auth index.
