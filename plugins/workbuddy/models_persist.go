// models_persist.go — v0.9.38: persist each realm's last successful model
// discovery INTO the credential files themselves (top-level "model_cache"
// key), so the last-known-good catalog survives host restarts and upstream
// outages.
//
// Why: the per-credential fallback chain in fetchDynamicModelsFromStorageInner
// ends at the in-memory stale cache (v0.12.71), which dies with the process.
// After a restart — or before the realm's first successful discovery in a
// process lifetime — discovery failure advertised NOTHING (v0.9.33), the
// host then UnregisterClient'd the credential's models, and the management
// panel's per-credential excluded-models editor read an empty registry
// (GET /v0/management/auth-files/models → GetModelsForClient). Persisting
// the snapshot closes that gap: on the next model.for_auth the plugin serves
// the persisted catalog, the host registers it, and the picker has models.
//
// Scope guard: snapshots are per-REALM and only ever served back to
// credentials of the SAME realm (cn/global/intl) — a CN catalog must never
// satisfy an Intl credential's model query (v0.12.18 boundary; different
// gateways advertise different model ids). The "merged across realms" view
// the panel shows emerges at the host registry, which unions every
// credential's registration.
//
// Write hygiene: every host.auth.save re-fires the watcher → AuthUpdate →
// model.for_auth. persistModelSnapshot therefore writes ONLY when the
// discovered list actually changed (full ModelInfo compare), so the loop
// converges after one save per catalog change.
package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// modelCacheDocKey is the top-level key stamped into credential files. It
// rides along every save because normalizeWorkbuddyAuthDoc merges unknown
// top-level keys as raw JSON (never re-marshaled through a typed struct).
const modelCacheDocKey = "model_cache"

// persistedModelCache is the on-disk snapshot shape.
type persistedModelCache struct {
	// Realm the catalog was discovered under ("cn" | "global" | "intl").
	Realm string `json:"realm"`
	// FetchedAt is the discovery time (RFC3339 UTC), used to pick the
	// freshest peer snapshot when the asking credential has none of its own.
	FetchedAt string `json:"fetched_at"`
	// Models is the raw discovered list (pre alias-name overlay — the
	// runtime learned-name overlay re-applies on every serve branch).
	Models []pluginapi.ModelInfo `json:"models"`
}

// Test seams — the real host RPC bridge is unavailable in unit tests.
var (
	hostAuthListFn     = hostAuthList
	hostAuthSaveJSONFn = hostAuthSaveJSON
)

// readModelCacheDoc extracts the model_cache snapshot from one credential
// document. Returns nil when absent, malformed, or empty — a snapshot with
// no models is treated as "never persisted" so it can never shadow a peer's
// real catalog.
func readModelCacheDoc(doc []byte) *persistedModelCache {
	if len(doc) == 0 {
		return nil
	}
	var probe struct {
		Cache *persistedModelCache `json:"model_cache"`
	}
	if err := json.Unmarshal(doc, &probe); err != nil || probe.Cache == nil {
		return nil
	}
	c := probe.Cache
	if strings.TrimSpace(c.Realm) == "" || len(c.Models) == 0 {
		return nil
	}
	return c
}

// persistedSnapshotForStorage resolves the last persisted snapshot for the
// asking credential's realm: its own document first, then the freshest
// same-realm peer document. Cross-realm snapshots are never served.
func persistedSnapshotForStorage(storageJSON []byte, realm string) ([]pluginapi.ModelInfo, bool) {
	if snap := readModelCacheDoc(storageJSON); snap != nil && snap.Realm == realm {
		return snap.Models, true
	}
	files, err := hostAuthListFn()
	if err != nil || len(files) == 0 {
		return nil, false
	}
	var best *persistedModelCache
	for _, f := range files {
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil || phys == nil || len(phys.JSON) == 0 {
			continue
		}
		c := readModelCacheDoc(phys.JSON)
		if c == nil || c.Realm != realm {
			continue
		}
		if best == nil || persistedFetchedAt(c) > persistedFetchedAt(best) {
			best = c
		}
	}
	if best == nil {
		return nil, false
	}
	return best.Models, true
}

// persistedFetchedAt renders the snapshot's discovery time for comparison;
// unparsable timestamps sort oldest ("").
func persistedFetchedAt(c *persistedModelCache) string {
	if c == nil {
		return ""
	}
	return c.FetchedAt
}

// persistModelSnapshot locates the asking credential's file and merges the
// fresh snapshot into it. Best-effort: failures are logged, never propagated
// — a persist miss only means the next restart re-discovers from scratch.
func persistModelSnapshot(storageJSON []byte, realm string, models []pluginapi.ModelInfo) {
	name, doc, ok := locateCredentialFile(storageJSON)
	if !ok {
		log.Printf("models: realm=%s snapshot persist skipped — credential file not located", realm)
		return
	}
	if existing := readModelCacheDoc(doc); existing != nil && existing.Realm == realm && samePersistedModels(existing.Models, models) {
		// Unchanged — a save here would re-fire the watcher → model.for_auth
		// for zero information. This guard is what makes the write loop
		// converge.
		return
	}
	merged, err := mergeModelCacheIntoDoc(doc, &persistedModelCache{
		Realm:     realm,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Models:    models,
	})
	if err != nil {
		log.Printf("models: realm=%s snapshot merge failed: %v", realm, err)
		return
	}
	if err := hostAuthSaveJSONFn(name, merged); err != nil {
		log.Printf("models: realm=%s snapshot persist failed: %v", realm, err)
		return
	}
	log.Printf("models: realm=%s snapshot persisted into %s (%d model(s))", realm, name, len(models))
}

// locateCredentialFile matches the asking credential to its file by access
// token — the only stable identifier that survives both storage shapes
// (nested plugin OAuth docs and flat CPA-Manager-Plus imports) and naming
// schemes (canonical uid files and legacy codebuddy-cn- names).
func locateCredentialFile(storageJSON []byte) (name string, doc []byte, ok bool) {
	tok, hasTok := extractAccessToken(storageJSON)
	if !hasTok || strings.TrimSpace(tok) == "" {
		return "", nil, false
	}
	files, err := hostAuthListFn()
	if err != nil || len(files) == 0 {
		return "", nil, false
	}
	for _, f := range files {
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil || phys == nil || len(phys.JSON) == 0 {
			continue
		}
		if t, ok := extractAccessToken(phys.JSON); ok && t == tok {
			return f.Name, phys.JSON, true
		}
	}
	return "", nil, false
}

// mergeModelCacheIntoDoc stamps the snapshot into a credential document,
// preserving every other top-level key byte-for-byte (note, proxy_url,
// panel-managed fields — same ownership model as normalizeWorkbuddyAuthDoc).
func mergeModelCacheIntoDoc(doc []byte, cache *persistedModelCache) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		return nil, err
	}
	m[modelCacheDocKey] = raw
	return json.Marshal(m)
}

// samePersistedModels compares two snapshots field-completely (not just by
// model id): a renamed display name or changed context limit is a real
// catalog change worth one save.
func samePersistedModels(a, b []pluginapi.ModelInfo) bool {
	if len(a) != len(b) {
		return false
	}
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

// pluginPreservedDocKeys are the top-level keys the save funnel re-injects
// when an incoming document is missing them but the current physical file
// carries them. Keys the caller explicitly set always win over the persisted
// copy.
//
//   - model_cache: plugin-stamped discovery snapshot (v0.9.41) — a typed
//     buildAuthFileJSON rebuild (lifecycle notes, adopt, import) constructs a
//     fresh document and would otherwise silently drop it, and the next
//     discovery outage then advertised nothing.
//   - priority: externally managed routing tier (CPA panel, CPA-Manager-Plus
//     batch priority, hand edits). The plugin owns neither the value nor its
//     semantics — it only has to keep it alive across its own rewrites. The
//     host's plugin-auth branch does not read this key from the file (only
//     the non-plugin synthesizer path does), so losing it silently resets the
//     credential to the default routing tier (round6: shawn-primary /
//     anyu-fallback reverted to balanced pooling).
var pluginPreservedDocKeys = []string{modelCacheDocKey, priorityDocKey}

// preservePluginDocKeys re-injects the plugin-preserved keys that the
// incoming document is missing but the current physical file carries (called
// from the hostAuthSaveJSON funnel; read before overwrite). The typed
// buildAuthFileJSON rebuilds — lifecycle notes, adopt, import — construct a
// fresh document from a struct and otherwise silently drop both the v0.9.38
// model_cache snapshot and the externally managed priority.
func preservePluginDocKeys(name string, doc []byte) []byte {
	if len(doc) == 0 || name == "" {
		return doc
	}
	var have map[string]json.RawMessage
	if err := json.Unmarshal(doc, &have); err != nil {
		return doc
	}
	var missing []string
	for _, key := range pluginPreservedDocKeys {
		if _, ok := have[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return doc
	}
	physical := physicalDocByName(name)
	if len(physical) == 0 {
		return doc
	}
	var prev map[string]json.RawMessage
	if err := json.Unmarshal(physical, &prev); err != nil {
		return doc
	}
	changed := false
	for _, key := range missing {
		if raw, ok := prev[key]; ok && len(raw) > 0 {
			have[key] = raw
			changed = true
		}
	}
	if !changed {
		return doc
	}
	merged, err := json.Marshal(have)
	if err != nil {
		return doc
	}
	return merged
}

// physicalDocByName resolves one credential document by exact file name
// (best-effort; empty when the name is unknown — e.g. a first save).
func physicalDocByName(name string) []byte {
	files, err := hostAuthListFn()
	if err != nil {
		return nil
	}
	for _, f := range files {
		if !strings.EqualFold(strings.TrimSpace(f.Name), name) {
			continue
		}
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil || phys == nil {
			return nil
		}
		return phys.JSON
	}
	return nil
}
