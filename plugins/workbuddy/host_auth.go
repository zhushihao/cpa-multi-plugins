// host_auth.go wraps the host's auth-store RPC (host.auth.list / get /
// get_bundle). These are the only paths the plugin uses to read auth files;
// writes go through hostAuthPersist / hostAuthPersistMigrate in lifecycle.go.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// rpcHostAuthListResponse mirrors the host's host.auth.list envelope result.
type rpcHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

// hostAuthList returns all workbuddy-family credentials known to the host.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list: bad envelope")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	// Fresh slice — resp.Files[:0] would alias the RPC response's backing
	// array (P1-3: fragile pattern, safe today but could break if resp is
	// ever cached/reused).
	//
	// Filter by FILE NAME, NOT by Type/Provider: many existing auth files
	// on disk don't carry a "type"/"provider" field (they were written
	// before that convention), and EqualFold("", providerName) returns
	// false for them — meaning we'd incorrectly exclude files that have
	// a workbuddy-family name but no type field. The filename is the only
	// reliable cross-version discriminator for type-less files.
	//
	// v0.9.43: the accepted-name set is now isOurFamilyFileName — the
	// SAME predicate handleParseAuth uses to claim files. The previous
	// inline prefix list (workbuddy- / codebuddy-cn- / codebuddy-intl-)
	// missed the legacy single-account names (workbuddy.json /
	// codebuddy.json), so a credential saved under a legacy name — the
	// UID-less-login fallback — was claimed by parse yet permanently
	// INVISIBLE here: the panel showed "no credential" while the file
	// existed on disk, and the lifecycle legacy-name migration never ran
	// either (it only sees files this list returns).
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	for _, f := range resp.Files {
		if !isOurFamilyFileName(f.Name) {
			continue
		}
		// Content guard: a file can carry our filename prefix while its body
		// belongs to another plugin (e.g. a qoder auth saved under a
		// workbuddy- name by a third-party tool). Running it through this
		// plugin's endpoints 401s against the wrong upstream with a cryptic
		// APISIX HTML page. Entries WITHOUT a type stay eligible — the
		// filename prefix remains their only discriminator.
		if foreign, owner := foreignAuthOwner(f.Type, f.Provider); foreign {
			log.Printf("workbuddy: auth %s skipped — credential type %q belongs to the %s plugin, not workbuddy", f.Name, owner, owner)
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// foreignAuthOwner reports whether the host-resolved type/provider of an
// auth entry names a plugin outside this one's family. Returns (false, "")
// when no type is declared (legacy files — the filename prefix is then the
// only discriminator) or when the value belongs to the workbuddy/codebuddy
// merged family.
func foreignAuthOwner(entryType, entryProvider string) (bool, string) {
	t := strings.ToLower(strings.TrimSpace(entryType))
	if t == "" {
		t = strings.ToLower(strings.TrimSpace(entryProvider))
	}
	switch t {
	case "", "workbuddy", "workbuddy-cn", "workbuddy-global", "workbuddy-intl",
		"codebuddy", "codebuddy-cn", "codebuddy-intl":
		return false, ""
	default:
		return true, t
	}
}

// hostAuthGet fetches the credential JSON for one auth index.
func hostAuthGet(authIndex string) (*storedAuth, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, err
	}
	return parseStored(phys.JSON)
}

// hostAuthGetFn is the read seam for tests that drive account loops (travel,
// scheduled check-in) without a live host RPC bridge. Production callers use
// it through hostAuthGetFn — never hostAuthGet directly — so a single stub
// covers the whole loop.
var hostAuthGetFn = hostAuthGet

// hostAuthGetBundle is one host.auth.get for both storage and physical metadata
// (avoids the previous double-RPC in dashboard: get + getPhysical).
func hostAuthGetBundle(authIndex string) (*storedAuth, *hostAuthPhysical, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, nil, err
	}
	sa, err := parseStored(phys.JSON)
	if err != nil {
		return nil, phys, err
	}
	return sa, phys, nil
}

// hostAuthGetBundleFn is the read seam for tests that drive the lifecycle
// reconcile loop without a live host RPC bridge.
var hostAuthGetBundleFn = hostAuthGetBundle
