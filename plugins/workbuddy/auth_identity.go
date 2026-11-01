// auth_identity.go resolves host auth records to a stable account identity
// (uid × realm) and deduplicates the legacy + canonical copies of one
// credential.
//
// Background (I5 review + second audit): the non-destructive adoption policy
// deliberately preserves the legacy codebuddy-cn-<uid>.json / workbuddy.json
// files next to the canonical workbuddy-<uid>.json copy, so one account can be
// listed twice. Both copies then act on the same upstream account:
//   - the weighted scheduler candidate pool would double-count the account
//     (one draw unit per file, split cost ledger rows);
//   - each account pass (travel / scheduled check-in / task runs) would issue
//     its upstream calls twice.
//
// Dedup is IN-MEMORY ONLY: no auth file is ever deleted, rewritten or moved
// (I6 non-destructive credential policy). Identity is (uid, realm), not uid
// alone — a human account may legitimately hold both a CN and an Intl
// credential with the same uid, and merging those would route or act on the
// wrong realm. Credentials whose uid cannot be resolved are never merged
// (fail-open: never merge two accounts on a guess).
package main

import (
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// hostAuthGetFn reads one auth record's parsed storage by host auth index.
// (Local-stack seam name; implemented over the upstream physical-read + parse
// pair so the identity map stays addressable by host auth ID.)
var hostAuthGetFn = func(authIndex string) (*storedAuth, error) {
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil {
		return nil, err
	}
	return parseStored(phys.JSON)
}

// wbTombstoneBlocksIdentity is the adopt-tombstone gate (adopt_tombstone.go in
// the local stack). The tombstone store is NOT ported to this build, so no
// identity can be tombstoned and the gate is inert (always false). Porting
// tombstones is a follow-up if adopt-then-delete semantics are ever needed.
func wbTombstoneBlocksIdentity(key string) bool { return false }


// -----------------------------------------------------------------------------
// Identity
// -----------------------------------------------------------------------------

// wbAuthIdentity is one resolved account identity.
type wbAuthIdentity struct {
	UID   string
	Realm string // cn | global | intl
	// Name is the host auth file name when known; used only to prefer the
	// canonical uid-named copy as the stable representative.
	Name string
}

// wbIdentityKey is the dedup key ("" for an unusable identity).
func (e wbAuthIdentity) key() string {
	uid := strings.TrimSpace(e.UID)
	if uid == "" {
		return ""
	}
	realm := strings.TrimSpace(e.Realm)
	if realm == "" {
		realm = "cn"
	}
	return uid + "\x1f" + realm
}

// wbAuthIdentityOf derives the identity from a parsed credential.
func wbAuthIdentityOf(sa *storedAuth) wbAuthIdentity {
	if sa == nil {
		return wbAuthIdentity{}
	}
	return wbAuthIdentity{UID: strings.TrimSpace(sa.Account.UID), Realm: accountRegion(sa)}
}

// wbIdentityCanonicalName is the file name the plugin itself writes for an
// identity (mirrors authFileNameFor).
func wbIdentityCanonicalName(e wbAuthIdentity) string {
	uid := sanitizeUIDForFileName(e.UID)
	if uid == "" {
		return authFileName
	}
	if strings.EqualFold(strings.TrimSpace(e.Realm), "intl") {
		return "workbuddy-intl-" + uid + ".json"
	}
	return "workbuddy-" + uid + ".json"
}

// -----------------------------------------------------------------------------
// Per-pass guard (travel / check-in / task loops)
// -----------------------------------------------------------------------------

// wbAccountPassGuard remembers which account identities one pass has already
// handled, so legacy + canonical copies of a credential act once per pass.
type wbAccountPassGuard struct {
	seen sync.Map // identity key -> struct{}
}

// first reports whether this credential's identity is new for the pass. A nil
// guard or a credential without a uid always counts as new (fail-open).
func (g *wbAccountPassGuard) first(sa *storedAuth) bool {
	if g == nil {
		return true
	}
	key := wbAuthIdentityOf(sa).key()
	if key == "" {
		return true
	}
	_, loaded := g.seen.LoadOrStore(key, struct{}{})
	return !loaded
}

// -----------------------------------------------------------------------------
// Scheduler-candidate identity resolution
// -----------------------------------------------------------------------------

// wbAuthIdentityScanInterval throttles the host.auth.list fallback scan: the
// scheduler runs on the request hot path, so unknown candidate IDs may trigger
// at most one credential read per window.
const wbAuthIdentityScanInterval = 30 * time.Second

var (
	wbAuthIdentityMu     sync.RWMutex
	wbAuthIdentitiesByID = map[string]wbAuthIdentity{}
	wbAuthIdentityScanAt time.Time
)

// wbRememberAuthIdentity records one auth-record-ID -> identity observation.
// Fed by executor paths (which know both the selected AuthID and the parsed
// credential) and by the throttled host.auth.list scan.
func wbRememberAuthIdentity(authID string, sa *storedAuth) {
	id := strings.TrimSpace(authID)
	e := wbAuthIdentityOf(sa)
	if id == "" || e.UID == "" {
		return
	}
	wbAuthIdentityMu.Lock()
	wbAuthIdentitiesByID[id] = e
	wbAuthIdentityMu.Unlock()
}

// wbKnownAuthIdentity returns the cached identity for one auth record ID.
func wbKnownAuthIdentity(authID string) (wbAuthIdentity, bool) {
	wbAuthIdentityMu.RLock()
	defer wbAuthIdentityMu.RUnlock()
	e, ok := wbAuthIdentitiesByID[strings.TrimSpace(authID)]
	return e, ok
}

// wbAuthIdentitiesForCandidates resolves identities for scheduler candidate
// IDs. Unknown IDs trigger at most one throttled host.auth.list scan; RPC
// failures are fail-open (the candidate simply is not deduplicated).
func wbAuthIdentitiesForCandidates(ids []string) map[string]wbAuthIdentity {
	out := make(map[string]wbAuthIdentity, len(ids))
	var missing []string
	wbAuthIdentityMu.RLock()
	for _, id := range ids {
		if e, ok := wbAuthIdentitiesByID[strings.TrimSpace(id)]; ok {
			out[strings.TrimSpace(id)] = e
		} else {
			missing = append(missing, strings.TrimSpace(id))
		}
	}
	wbAuthIdentityMu.RUnlock()
	if len(missing) == 0 {
		return out
	}
	wbScanAuthIdentities()
	wbAuthIdentityMu.RLock()
	for _, id := range missing {
		if e, ok := wbAuthIdentitiesByID[id]; ok {
			out[id] = e
		}
	}
	wbAuthIdentityMu.RUnlock()
	return out
}

// wbScanAuthIdentities reads the host auth list once (throttled) and fills in
// missing identities. Concurrent callers are allowed to proceed without
// waiting — the scan is an optimization, never a correctness dependency.
func wbScanAuthIdentities() {
	wbAuthIdentityMu.Lock()
	if !wbAuthIdentityScanAt.IsZero() && time.Since(wbAuthIdentityScanAt) < wbAuthIdentityScanInterval {
		wbAuthIdentityMu.Unlock()
		return
	}
	wbAuthIdentityScanAt = time.Now()
	wbAuthIdentityMu.Unlock()

	files, err := hostAuthListFn()
	if err != nil {
		return
	}
	for _, f := range files {
		if _, ok := wbKnownAuthIdentity(f.ID); ok {
			continue
		}
		sa, err := hostAuthGetFn(f.AuthIndex)
		if err != nil {
			continue
		}
		id := strings.TrimSpace(f.ID)
		e := wbAuthIdentityOf(sa)
		if id == "" || e.UID == "" {
			continue
		}
		e.Name = strings.TrimSpace(f.Name)
		wbAuthIdentityMu.Lock()
		wbAuthIdentitiesByID[id] = e
		wbAuthIdentityMu.Unlock()
	}
}

// wbDedupCandidatesByIdentity collapses scheduler candidates that resolve to
// the same (uid, realm) into ONE stable representative. Non-destructive: only
// the in-memory candidate list is filtered. Unresolvable candidates are kept.
//
// Representative preference (stable across host list order): the canonical
// uid-named file wins; ties break on the lexicographically smaller ID.
func wbDedupCandidatesByIdentity(cands []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	if len(cands) < 2 {
		return cands
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	identities := wbAuthIdentitiesForCandidates(ids)
	if len(identities) == 0 {
		return cands
	}
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(cands))
	positions := make(map[string]int, len(cands))
	for _, c := range cands {
		cur, ok := identities[strings.TrimSpace(c.ID)]
		key := ""
		if ok {
			key = cur.key()
		}
		if key == "" {
			out = append(out, c) // unknown identity: never merged
			continue
		}
		at, dup := positions[key]
		if !dup {
			positions[key] = len(out)
			out = append(out, c)
			continue
		}
		prev := out[at]
		prevIdentity := identities[strings.TrimSpace(prev.ID)]
		if wbCandidateBetterRepresentative(c.ID, cur, prev.ID, prevIdentity) {
			out[at] = c
		}
	}
	return out
}

// wbDropTombstonedCandidates removes scheduler candidates whose identity was
// deliberately removed (round-2 enforcement: a preserved legacy copy of a
// deleted account must not be routed again). Unknown identities are kept
// (fail-open: the store only ever contains explicit removals).
func wbDropTombstonedCandidates(cands []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	if len(cands) == 0 {
		return cands
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	identities := wbAuthIdentitiesForCandidates(ids)
	if len(identities) == 0 {
		return cands
	}
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(cands))
	for _, c := range cands {
		e, ok := identities[strings.TrimSpace(c.ID)]
		if ok && wbTombstoneBlocksIdentity(e.key()) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// wbCandidateBetterRepresentative reports whether cand is a better stable
// representative than cur for the same identity.
func wbCandidateBetterRepresentative(cand string, candIdentity wbAuthIdentity, cur string, curIdentity wbAuthIdentity) bool {
	canonical := strings.ToLower(wbIdentityCanonicalName(candIdentity))
	candCanonical := strings.ToLower(strings.TrimSpace(candIdentity.Name)) == canonical
	curCanonical := strings.ToLower(strings.TrimSpace(curIdentity.Name)) == canonical
	if candCanonical != curCanonical {
		return candCanonical
	}
	return strings.TrimSpace(cand) < strings.TrimSpace(cur)
}

// -----------------------------------------------------------------------------
// Batch target dedup (manual check-in / task runs / night tick)
// -----------------------------------------------------------------------------

// wbAccountLockKey is the unified per-account lock key: the account identity
// (uid × realm) when it can be resolved, falling back to the auth index. Legacy
// and canonical copies of one credential have DIFFERENT auth indexes, so an
// index-keyed lock would let two passes (manual check-in vs auto travel/tick)
// act on the same upstream account concurrently (round-2 review).
func wbAccountLockKey(authIndex string, sa *storedAuth) string {
	if key := wbAuthIdentityOf(sa).key(); key != "" {
		return "id\x1f" + key
	}
	return "idx\x1f" + strings.TrimSpace(authIndex)
}

// wbAccountLockFor returns the unified mutex for an account. Callers must pass
// the credential whenever they have it; the index fallback exists only for
// paths that cannot resolve one, and mixing the two keys for one account is a
// bug (the fallback cannot see identity-keyed holders).
func wbAccountLockFor(authIndex string, sa *storedAuth) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(wbAccountLockKey(authIndex, sa), &sync.Mutex{})
	return v.(*sync.Mutex)
}

// wbDedupedTargets returns at most one host file entry per account identity,
// preferring the canonical uid-named copy regardless of host list order (the
// preserved legacy source must not be the target of batch/lifecycle entries).
// It reads each target's credential (hostAuthGetFn) and never writes or deletes
// anything. Entries whose credential cannot be read or has no uid are kept
// as-is (fail-open). Callers use the returned duplicate count for summaries.
func wbDedupedTargets(targets []pluginapi.HostAuthFileEntry) (kept []pluginapi.HostAuthFileEntry, duplicates int) {
	if len(targets) == 0 {
		return targets, 0
	}
	type entryIdentity struct {
		key string
		can bool
	}
	kept = make([]pluginapi.HostAuthFileEntry, 0, len(targets))
	positions := make(map[string]int, len(targets))
	identities := make([]entryIdentity, 0, len(targets))
	for _, f := range targets {
		sa, err := hostAuthGetFn(f.AuthIndex)
		if err != nil {
			// Unreadable: keep, the per-account path reports the error.
			kept = append(kept, f)
			identities = append(identities, entryIdentity{})
			continue
		}
		e := wbAuthIdentityOf(sa)
		key := e.key()
		if key == "" {
			kept = append(kept, f)
			identities = append(identities, entryIdentity{})
			continue
		}
		// A tombstoned identity — or an untrusted tombstone store — must not let
		// the preserved copy act (round-2/round-3 enforcement, fail-closed).
		if wbTombstoneBlocksIdentity(key) {
			duplicates++
			continue
		}
		canonical := strings.EqualFold(strings.TrimSpace(f.Name), wbIdentityCanonicalName(e))
		if at, dup := positions[key]; dup {
			duplicates++
			// Replace the representative when this entry is the canonical copy
			// and the current one is not (stable: lexicographically smaller ID
			// breaks ties).
			if canonical && !identities[at].can {
				kept[at] = f
				identities[at] = entryIdentity{key: key, can: canonical}
			} else if canonical == identities[at].can && strings.TrimSpace(f.ID) < strings.TrimSpace(kept[at].ID) {
				kept[at] = f
			}
			continue
		}
		positions[key] = len(kept)
		kept = append(kept, f)
		identities = append(identities, entryIdentity{key: key, can: canonical})
	}
	return kept, duplicates
}
