// lifecycle.go implements credit-based auth lifecycle for workbuddy:
//   - CN exhausted  → disable auth file (disabled:true), re-enable after check-in when credits return
//   - Global exhausted → delete auth file (one-shot quota)
//   - Unknown credits → no-op (never mis-kill)
//   - Hard credit errors from executor → recheck credits then apply policy
//   - Soft rate limits → do not delete Global
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	lifecycleState   sync.Map // auth_id (auth.ID) -> lifecycleStateEntry
	lifecycleSaveTTL = 30 * time.Second
)

type lifecycleStateEntry struct {
	disabled bool
	note     string
	at       time.Time
}

func lifecycleStateUnchanged(authID string, disabled bool, note string) bool {
	v, ok := lifecycleState.Load(authID)
	if !ok {
		return false
	}
	e := v.(*lifecycleStateEntry)
	if e.disabled != disabled || e.note != note {
		return false
	}
	return time.Since(e.at) < lifecycleSaveTTL
}

func rememberLifecycleState(authID string, disabled bool, note string) {
	lifecycleState.Store(authID, &lifecycleStateEntry{disabled: disabled, note: note, at: time.Now()})
}

// pruneLifecycleState removes entries for auth indices that no longer exist
// or whose TTL has expired. Called from dashboard prune to prevent unbounded growth.
func pruneLifecycleState() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
	}
	lifecycleState.Range(func(key, value any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			lifecycleState.Delete(key)
			return true
		}
		if e, ok := value.(*lifecycleStateEntry); ok && time.Since(e.at) > 10*time.Minute {
			lifecycleState.Delete(key)
		}
		return true
	})
}

// disableAuth writes disabled:true for a CN (or fallback) account.
func disableAuth(authIndex, authID string, sa *storedAuth, cr *creditsSummary, reason string) error {
	mu := wbAccountLockFor(authIndex, sa)
	mu.Lock()
	defer mu.Unlock()
	return disableAuthLocked(authIndex, authID, sa, cr, reason)
}

// disableAuthLocked is disableAuth without taking the per-account lock: the
// caller already holds the unified per-account lock. Used by the delete path's
// fail-closed fallback (architect ruling 2026-09-29: without a verified
// tombstone the credential must be disabled instead of deleted).
func disableAuthLocked(authIndex, authID string, sa *storedAuth, cr *creditsSummary, reason string) error {
	note := displayNote(sa, cr, true)
	if reason != "" && !strings.Contains(note, reason) {
		// keep note short; reason only if room
		if len(note)+len(reason) < 75 {
			note = note + " · " + reason
		}
	}
	if lifecycleStateUnchanged(authID, true, note) {
		return nil
	}
	// Prefer live physical file to preserve any extra fields if present.
	// Round-2 review: the read goes through the test seam (consistency with the
	// delete path) and resolveAuthFileTarget verifies the physical identity
	// before reusing the host-reported file name.
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err == nil && parseDisabledFromAuthJSON(phys.JSON) {
		// already disabled; still refresh note if needed
		if lifecycleStateUnchanged(authID, true, note) {
			return nil
		}
	}
	name := authFileNameFor(sa)
	path := ""
	legacyPath := ""
	if phys != nil {
		name, path, legacyPath = resolveAuthFileTarget(sa, phys)
	}
	raw, err := buildAuthFileJSON(sa, true, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistMigrateFn(name, path, legacyPath, raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, true, note)
	accountCache.Delete(authID)
	return nil
}

// reenableAuth writes disabled:false when CN has credits again.
func reenableAuth(authIndex, authID string, sa *storedAuth, cr *creditsSummary) error {
	mu := wbAccountLockFor(authIndex, sa)
	mu.Lock()
	defer mu.Unlock()

	if !shouldReenableCN(true, cr) {
		return nil
	}
	note := displayNote(sa, cr, false)
	if lifecycleStateUnchanged(authID, false, note) {
		return nil
	}
	phys, err := hostAuthGetPhysicalFn(authIndex)
	name := authFileNameFor(sa)
	path := ""
	legacyPath := ""
	if err == nil {
		name, path, legacyPath = resolveAuthFileTarget(sa, phys)
	}
	raw, err := buildAuthFileJSON(sa, false, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistMigrate(name, path, legacyPath, raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, false, note)
	accountCache.Delete(authID)
	return nil
}

// deleteAuth removes Global exhausted credentials from disk.
//
// Round-2 review guards:
//   - the physical target is read and verified UNDER the shared adopt/delete
//     mutex, so an in-process adoption cannot change the target between the
//     verification read and the delete (external host operations are outside
//     this mutex and remain a documented boundary);
//   - the physical credential must parse and carry the SAME identity (uid ×
//     realm) as the requested sa, otherwise the delete fails closed (never
//     delete another account's credential, never tombstone the wrong account);
//   - a preserved legacy codebuddy-cn / codebuddy-intl SOURCE is never a delete
//     target (I6: old credential files are read-only rollback material);
//   - the tombstone is durably recorded and read back verified before the
//     deletion; any failure disables instead (fail-closed);
//   - the dual-named legacy copy is always preserved (only reported).
func deleteAuth(authIndex, authID string, sa *storedAuth) error {
	mu := wbAccountLockFor(authIndex, sa)
	mu.Lock()
	defer mu.Unlock()

	wbAdoptDeleteMu.Lock()
	defer wbAdoptDeleteMu.Unlock()

	// Seam-based read (hostAuthGetPhysicalFn defaults to hostAuthGetPhysical)
	// so the credential-preservation tests can drive this path offline.
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil {
		return err
	}
	// The physical document is authoritative for what would actually be
	// removed: verify it before touching anything.
	physSA, perr := parseStored(phys.JSON)
	if perr != nil || physSA == nil {
		log.Printf("workbuddy: refusing to delete auth index %s: physical credential unreadable (%v)", authIndex, perr)
		return fmt.Errorf("delete refused: physical credential unreadable: %v", perr)
	}
	wantKey := wbAuthIdentityOf(sa).key()
	targetKey := wbAuthIdentityOf(physSA).key()
	if wantKey == "" || targetKey == "" || wantKey != targetKey {
		log.Printf("workbuddy: refusing to delete auth index %s: requested identity %q != physical identity %q", authIndex, wantKey, targetKey)
		return fmt.Errorf("delete refused: credential identity mismatch (requested %q, physical %q)", wantKey, targetKey)
	}

	path := strings.TrimSpace(phys.Path)
	if path == "" {
		// Try to reconstruct path from peer workbuddy files' directory + canonical name.
		name, _, _ := resolveAuthFileTarget(sa, phys)
		if dir := peerAuthDir(); dir != "" && name != "" {
			candidate := filepath.Join(dir, name)
			if isSafeWorkbuddyAuthPath(candidate) {
				path = candidate
			}
		}
	}
	if path == "" {
		// Last resort: disable instead of silent no-op (never invent a random path).
		note := displayNote(sa, nil, true) + " · 应删除但无 path"
		raw, berr := buildAuthFileJSON(sa, true, note, nil)
		if berr != nil {
			return fmt.Errorf("no path and build failed: %w", berr)
		}
		name, _, _ := resolveAuthFileTarget(sa, phys)
		if err := hostAuthPersistMigrateFn(name, "", "", raw); err != nil {
			return err
		}
		rememberLifecycleState(authID, true, note)
		accountCache.Delete(authID)
		clearActiveAuthIfMatch(authID)
		return nil
	}
	// A preserved legacy source is never deleted (I6 policy). The canonical
	// copy is the only delete target this path may touch.
	if base := strings.ToLower(filepath.Base(path)); isLegacyCodebuddyAuthName(base) || isLegacyCodebuddyIntlAuthName(base) || isLegacyWorkbuddyAuthName(base) {
		log.Printf("workbuddy: refusing to delete preserved legacy source %s (I6 policy)", path)
		return fmt.Errorf("delete refused: %s is a preserved legacy credential source", path)
	}
	// N1 (architect ruling 2026-09-29): the deletion is only allowed after the
	// removal is durably recorded and read back verified. Any failure (tombstone
	// off/corrupt/future schema/path/permission/write/read-back) fails CLOSED:
	// the credential is disabled instead of deleted, so it leaves routing
	// without becoming resurrectable from a preserved legacy source.
	recordErr := wbRecordDeletionTombstone(physSA, filepath.Base(path))
	if recordErr != nil {
		log.Printf("workbuddy: refusing to delete %s: %v — disabling instead (fail-closed)", path, recordErr)
		return disableAuthLocked(authIndex, authID, sa, nil, "删除需 tombstone 但不可持久化")
	}
	if delErr := deleteAuthFileInDir(path, filepath.Dir(path)); delErr != nil {
		return delErr
	}
	preserveLegacyDualName(filepath.Dir(path), sa)
	lifecycleState.Delete(authID)
	accountCache.Delete(authID)
	clearActiveAuthIfMatch(authID)
	return nil
}

// removeLegacyDualName removes <dir>/workbuddy.json ONLY when it is provably
// the same credential as the account being deleted (same non-empty UID).
//
// I6 credential-preservation policy: the bare legacy name carries no UID, so
// the old unconditional cleanup could destroy a DIFFERENT account's credential
// sharing the directory. Under the non-destructive migration both names may
// exist for one UID, so the dual-named copy must still be removed together
// with its account (otherwise an exhausted Global account would reappear on
// the next reload) — but only after reading the file and matching the UID.
// preserveLegacyDualName reports the dual-named legacy copy after a delete.
//
// Round-2 review (I6 preservation-first): this used to DELETE <dir>/workbuddy.json
// when it carried the same UID, which contradicted the non-destructive policy
// stated in authfile.go — the old file is rollback material for :3002 and is
// never rewritten or deleted automatically. The canonical file is the only
// delete target; the legacy copy stays byte-identical. Retiring it is an
// operator decision after the staged cutover.
func preserveLegacyDualName(dir string, sa *storedAuth) {
	if sa == nil || strings.TrimSpace(dir) == "" {
		return
	}
	legacy := filepath.Join(dir, authFileName)
	if !isLegacyWorkbuddyAuthName(filepath.Base(legacy)) {
		return
	}
	if st, err := os.Stat(legacy); err == nil && !st.IsDir() {
		log.Printf("lifecycle: legacy dual-named copy %s preserved in place (I6: old credential files are never rewritten or deleted)", legacy)
	}
}

// peerAuthDir returns the directory of any workbuddy auth file known to the host.
// Uses HostAuthFileEntry.Path from the list response (A-38: was N+1 — list + getPhysical per file).
// Seam-based (hostAuthListFn) so path-safety tests can run offline.
func peerAuthDir() string {
	files, err := hostAuthListFn()
	if err != nil {
		return ""
	}
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p != "" {
			return filepath.Dir(p)
		}
	}
	return ""
}

// applyExhaustedPolicy applies disable (CN) or delete (Global).
func applyExhaustedPolicy(authIndex, authID string, sa *storedAuth, cr *creditsSummary, reason string) error {
	if !lifecycleEnabled() {
		return nil
	}
	action := lifecycleActionFor(accountRegion(sa), cr)
	switch action {
	case lifecycleDelete:
		return deleteAuth(authIndex, authID, sa)
	case lifecycleDisable:
		return disableAuth(authIndex, authID, sa, cr, reason)
	default:
		return nil
	}
}

// existingNoteCredits reads the credit segment already stored on disk so a
// failed credits query can keep the last known value instead of regressing the
// card to "积分未知".
func existingNoteCredits(authIndex string) string {
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil || phys == nil {
		return ""
	}
	return noteCreditsFromJSON(phys.JSON)
}

// noteCreditsFromJSON extracts the credit segment from a raw auth-file body.
func noteCreditsFromJSON(raw []byte) string {
	var pjson struct {
		Note string `json:"note"`
	}
	if json.Unmarshal(raw, &pjson) != nil {
		return ""
	}
	return creditSegmentFromNote(pjson.Note)
}

// syncAuthNote writes note without changing disabled state.
//
// cr == nil means "credits unknown this round" (upstream error or not fetched
// yet). In that case the previously stored credit segment is preserved: a
// transient billing failure must not erase a note the user already sees.
func syncAuthNote(authIndex, authID string, sa *storedAuth, cr *creditsSummary, disabled bool) error {
	if sa == nil {
		return nil
	}
	note := displayNoteWithPrev(sa, cr, disabled, existingNoteCredits(authIndex))
	// v0.9.42: the change-guard compares the lifecycle-OWNED base only — the
	// 【用量】 segment churns on its own writer's schedule and must not echo
	// lifecycle saves.
	base := usageStripNote(note)
	if lifecycleStateUnchanged(authID, disabled, base) {
		return nil
	}
	mu := wbAccountLockFor(authIndex, sa)
	mu.Lock()
	defer mu.Unlock()
	phys, err := hostAuthGetPhysicalFn(authIndex)
	name := authFileNameFor(sa)
	path := ""
	legacyPath := ""
	if err == nil {
		name, path, legacyPath = resolveAuthFileTarget(sa, phys)
		// re-read disabled from disk as source of truth
		disabled = parseDisabledFromAuthJSON(phys.JSON)
		note = displayNoteWithPrev(sa, cr, disabled, noteCreditsFromJSON(phys.JSON))
		base = usageStripNote(note)
	}
	if lifecycleStateUnchanged(authID, disabled, base) {
		return nil
	}
	raw, err := buildAuthFileJSON(sa, disabled, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistMigrateFn(name, path, legacyPath, raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, disabled, base)
	return nil
}

// reconcileOneAccount refreshes credits and applies lifecycle for one auth.
// authIndex is used for host RPC (host.auth.get), authID (auth.ID) is used
// for cache keys (accountCache/lifecycleState) so it matches the scheduler's
// SchedulerAuthCandidate.ID.
// force ignores short-circuit only for credit fetch (uses force on cache via caller).
func reconcileOneAccount(authIndex, authID string, force bool) (action lifecycleAction, err error) {
	if !lifecycleEnabled() {
		return lifecycleNone, nil
	}
	// Single host.auth.get (A-19): previous hostAuthGet + hostAuthGetPhysical
	// doubled RPC on every reconcile tick (21 accounts × 2).
	sa, phys, err := hostAuthGetBundleFn(authIndex)
	if err != nil {
		return lifecycleNone, err
	}
	// A deliberately removed (tombstoned) identity — or an untrusted tombstone
	// store — is not reconciled: retrying its deletion would loop forever and
	// must never touch a preserved copy (round-2/round-3 enforcement,
	// fail-closed). A verified re-import/login clears the record.
	if wbTombstoneBlocksAuth(sa) {
		return lifecycleNone, nil
	}
	disabled := false
	if phys != nil {
		disabled = phys.Disabled
	}

	// Credits: use force path via fetchUserResource always when force,
	// else try cache first.
	var cr *creditsSummary
	if !force {
		if v, ok := accountCache.Load(authID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 && e.credits != nil && time.Since(e.fetched) < accountCacheTTL {
				cr = e.credits
			}
		}
	}
	if cr == nil {
		// Route credits fetch through cachedAccountDetails so singleflight
		// serializes concurrent writers for the same authID (P0-2 fix: the
		// previous Load→Store sequence here had a check-then-act window
		// where a concurrent dashboard cachedAccountDetails write could
		// overwrite our merge with newer plan/checkin values).
		_, _, cr2, _ := cachedAccountDetails(authID, sa, true)
		cr = cr2
		if cr == nil {
			return lifecycleNone, nil
		}
	}

	region := accountRegion(sa)
	if region == "cn" && disabled {
		if shouldReenableCN(true, cr) {
			if err := reenableAuth(authIndex, authID, sa, cr); err != nil {
				return lifecycleReenable, err
			}
			return lifecycleReenable, nil
		}
		// still disabled: refresh note
		_ = syncAuthNote(authIndex, authID, sa, cr, true)
		return lifecycleNone, nil
	}

	act := lifecycleActionFor(region, cr)
	switch act {
	case lifecycleDelete:
		// P1-4: confirm before deleting a Global account — a transient 402
		// from the upstream billing API could otherwise cause an irreversible
		// delete. Re-fetch credits once more; only proceed if still exhausted.
		cr2, err2 := fetchUserResource(sa)
		if err2 != nil || !isCreditsExhausted(cr2) {
			// Credits may have recovered (or fetch failed) — don't delete.
			return lifecycleNone, nil
		}
		return lifecycleDelete, deleteAuth(authIndex, authID, sa)
	case lifecycleDisable:
		return lifecycleDisable, disableAuth(authIndex, authID, sa, cr, "耗尽")
	default:
		// healthy: keep note fresh (throttled)
		_ = syncAuthNote(authIndex, authID, sa, cr, false)
		return lifecycleNone, nil
	}
}

// reconcileAllAccounts walks workbuddy auths and applies lifecycle.
func reconcileAllAccounts(force bool) []map[string]any {
	if !lifecycleEnabled() {
		return nil
	}
	files, err := hostAuthList()
	if err != nil {
		return []map[string]any{{"error": err.Error()}}
	}
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		act, err := reconcileOneAccount(f.AuthIndex, f.ID, force)
		row := map[string]any{"auth_index": f.AuthIndex, "action": act.String()}
		if err != nil {
			row["error"] = err.Error()
		}
		if act != lifecycleNone || err != nil {
			out = append(out, row)
		}
	}
	return out
}

// reconcileAfterExecutorError triggers lifecycle when upstream reports hard credit failure.
// AuthID from the executor may be the credential ID (UID) rather than runtime auth_index;
// we resolve via host.auth.list when direct get fails.
func reconcileAfterExecutorError(authID string, status int, body string) {
	if !lifecycleEnabled() || strings.TrimSpace(authID) == "" {
		return
	}
	// v0.9.15: 输入过大是请求级问题（同一 body 在任何账号上都会被拒），
	// 不触发积分 reconcile——413/过长 body 若恰好携带 "quota exceeded"
	// 等词曾可能经 hardCreditMarkers 词表碰撞误触发生命周期。
	if isPromptTooLong(status, body) {
		return
	}
	if isSoftRateLimit(status, body) && !isHardCreditError(status, body) {
		return
	}
	if !isHardCreditError(status, body) {
		return
	}
	go func() {
		idx, id := resolveAuthIndexAndID(authID)
		if idx == "" {
			return
		}
		_, _ = reconcileOneAccount(idx, id, true)
	}()
}

// resolveAuthIndexAndID maps executor AuthID (index, file id, or account UID)
// to host auth_index AND auth.ID. Returns ("", "") if not found.
func resolveAuthIndexAndID(authID string) (string, string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", ""
	}
	// Fast path: already an auth_index the host understands.
	if _, err := hostAuthGet(authID); err == nil {
		// Find the matching file entry to get auth.ID.
		if files, err := hostAuthList(); err == nil {
			for _, f := range files {
				if f.AuthIndex == authID {
					return authID, f.ID
				}
			}
		}
		return authID, ""
	}
	files, err := hostAuthList()
	if err != nil {
		return "", ""
	}
	// Prefer O(list) name/id match before per-account host.auth.get (A-22).
	// Multi-account files are workbuddy-<uid>.json; list Name/ID usually carry that.
	wantName := "workbuddy-" + authID + ".json"
	for _, f := range files {
		if f.AuthIndex == authID || f.ID == authID || f.Name == authID {
			return f.AuthIndex, f.ID
		}
		if listEntryMatchesUID(f, authID, wantName) {
			return f.AuthIndex, f.ID
		}
	}
	// Slow path: only when list metadata lacks uid (rare legacy shapes).
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		if strings.TrimSpace(sa.Account.UID) == authID {
			return f.AuthIndex, f.ID
		}
	}
	return "", ""
}

// reconcileByUID finds workbuddy auth by account UID and applies executor-error lifecycle.
func reconcileByUID(uid string, status int, body string) {
	uid = strings.TrimSpace(uid)
	if uid == "" || !lifecycleEnabled() {
		return
	}
	// v0.9.15: 输入过大是请求级问题（同一 body 在任何账号上都会被拒），
	// 不触发积分 reconcile——413/过长 body 若恰好携带 "quota exceeded"
	// 等词曾可能经 hardCreditMarkers 词表碰撞误触发生命周期。
	if isPromptTooLong(status, body) {
		return
	}
	if !isHardCreditError(status, body) {
		return
	}
	idx, id := resolveAuthIndexAndID(uid)
	if idx == "" {
		return
	}
	_, _ = reconcileOneAccount(idx, id, true)
}

// invalidateAccountCredits drops cached credits so the next panel/reconcile
// fetch hits upstream. Call after a successful chat completion — otherwise a
// short TTL cache makes "used" look frozen while the user is burning credits.
func invalidateAccountCredits(authID, authUID string) {
	// Invalidate credits only — keep plan/checkin in cache.
	invalidateCredits := func(id string) {
		if v, ok := accountCache.Load(id); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				fresh := *e
				fresh.credits = nil
				fresh.fetched = time.Now()
				accountCache.Store(id, &fresh)
			}
		}
	}
	if authID != "" {
		invalidateCredits(authID)
	}
	if authUID == "" || authUID == authID {
		return
	}
	// Also drop any cache keyed by auth_index that maps to this UID.
	files, err := hostAuthList()
	if err != nil {
		return
	}
	wantName := "workbuddy-" + authUID + ".json"
	matchedByName := false
	for _, f := range files {
		if f.AuthIndex == authID || f.ID == authID || f.Name == authID {
			invalidateCredits(f.ID)
			continue
		}
		if listEntryMatchesUID(f, authUID, wantName) {
			invalidateCredits(f.ID)
			matchedByName = true
		}
	}
	if matchedByName {
		return
	}
	// Slow path: legacy names without uid in list metadata.
	for _, f := range files {
		if f.AuthIndex == authID {
			continue
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		if strings.TrimSpace(sa.Account.UID) == authUID {
			invalidateCredits(f.ID)
		}
	}
}

// listEntryMatchesUID reports whether host list metadata already encodes the UID
// (workbuddy-<uid>.json naming). Pure helper for O(list) cache invalidation.
func listEntryMatchesUID(f pluginapi.HostAuthFileEntry, uid, wantName string) bool {
	if uid == "" {
		return false
	}
	if strings.EqualFold(f.Name, wantName) || strings.EqualFold(f.ID, wantName) {
		return true
	}
	base := strings.TrimSuffix(f.Name, ".json")
	return strings.EqualFold(base, "workbuddy-"+uid)
}

// enrichAuthMetadata builds Metadata map for AuthData (type/logo/note/disabled).
func enrichAuthMetadata(sa *storedAuth, cr *creditsSummary, disabled bool) map[string]any {
	return enrichAuthMetadataWithPrev(sa, cr, disabled, "")
}

// enrichAuthMetadataWithPrev is enrichAuthMetadata plus a previously known
// credit segment, so callers that can see the on-disk note never degrade a
// populated card back to the "积分未知" placeholder.
func enrichAuthMetadataWithPrev(sa *storedAuth, cr *creditsSummary, disabled bool, prevCredits string) map[string]any {
	note := displayNoteWithPrev(sa, cr, disabled, prevCredits)
	return map[string]any{
		// Same classification the physical writes stamp — the panel and
		// the host's auth listing must agree with the file on disk.
		"auth_kind": "oauth",
		"type":      providerName,
		"provider":  providerName,
		"logo":      pluginLogoURL,
		"note":      note,
		"disabled":  disabled,
	}
}
