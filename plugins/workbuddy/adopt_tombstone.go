// adopt_tombstone.go records credentials the operator deliberately removed, so
// startAdoption cannot resurrect them from the preserved legacy source file.
//
// Problem (N1, second audit): adoption copies codebuddy-cn-<uid>.json (or
// codebuddy-intl-<uid>.json) to the canonical workbuddy-<uid>.json and
// preserves the source by policy. If the canonical file is later deleted
// (lifecycle deleteAuth for an exhausted Global account, or an account
// removal), the next adoption run sees "legacy present, canonical missing" and
// re-adopts — resurrecting an account the operator deliberately removed.
//
// Solution: a plugin-owned, schema-versioned tombstone file. The delete path
// records (uid, realm) with an ATOMIC WRITE + READ-BACK VERIFICATION before it
// removes anything, and fails CLOSED when the record cannot be proven durable
// (persistence off, corrupt/unsupported/newer schema, permission/path/write
// failure): the credential is disabled instead of deleted. Adoption skips a
// legacy source whose identity is tombstoned, and refuses to run at all when
// the store is unusable. The legacy credential itself is never modified or
// deleted (I6 non-destructive policy).
//
// Deliberate re-import / re-login writes the canonical file directly (never
// through adoption), so a user action always revives the account; deleting the
// tombstone file clears the protection.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// wbAdoptTombstoneSchemaVersion versions the on-disk document. A different
	// version on disk is never overwritten (fail-closed).
	wbAdoptTombstoneSchemaVersion = 1
	// wbAdoptTombstoneFileName is the default artifact name (plugin-scoped).
	wbAdoptTombstoneFileName = "adopt-tombstones.json"
	// wbAdoptTombstoneEnv overrides the path; "off" disables persistence. Per the
	// architect ruling (2026-09-29), a disabled/unavailable store means the
	// persistent record of removed identities is LOST: routing and automatic
	// tasks then fail CLOSED for every WorkBuddy identity protected by this
	// mechanism (deliberately turning it off can render the plugin unusable for
	// automatic work). Deletion/adoption fail closed in the same states; only a
	// fresh install (no file yet at a usable path) is "no removals known".
	wbAdoptTombstoneEnv = "WORKBUDDY_ADOPT_TOMBSTONE_FILE"
)

// wbAdoptTombstoneRow is one removed credential.
type wbAdoptTombstoneRow struct {
	UID       string `json:"uid"`
	Realm     string `json:"realm"`
	Canonical string `json:"canonical,omitempty"`
	RemovedAt string `json:"removed_at,omitempty"`
}

// wbAdoptTombstoneDoc is the on-disk document.
type wbAdoptTombstoneDoc struct {
	SchemaVersion int                   `json:"schema_version"`
	UpdatedAt     string                `json:"updated_at,omitempty"`
	Tombstones    []wbAdoptTombstoneRow `json:"tombstones"`
}

var (
	// wbAdoptDeleteMu serializes automatic adoption against the delete path's
	// record+delete sequence, so an adoption that already decided to re-create
	// a canonical file cannot land AFTER a concurrent removal deleted it.
	wbAdoptDeleteMu sync.Mutex

	wbAdoptTombMu sync.Mutex
	// wbAdoptTombRows is keyed by uid × realm (wbIdentityKey).
	wbAdoptTombRows map[string]wbAdoptTombstoneRow
	// wbAdoptTombLoaded is the lazy-load marker for the on-disk document.
	wbAdoptTombLoaded bool
	// wbAdoptTombEnvValue records the env value the current state was loaded
	// for. Production never changes it; tests swap the artifact per test, and
	// re-loading on a change keeps one test's unusable store from leaking into
	// the next one.
	wbAdoptTombEnvValue string
	// wbAdoptTombPath is "" when persistence is unavailable.
	wbAdoptTombPath string
	// wbAdoptTombStoreErr, when non-empty, means the store must not be trusted
	// (disabled path, unreadable, corrupt, unsupported/newer schema): neither
	// writes nor automatic adoption may proceed. Fail-closed.
	wbAdoptTombStoreErr string
	// wbAdoptTombUntrusted is set when the store is unusable because its CONTENT
	// cannot be trusted (unreadable/corrupt/unsupported/newer schema) — as
	// opposed to being explicitly disabled or absent. Routing/automatic filters
	// fail CLOSED on an untrusted store (round-3 review: an empty row set from a
	// damaged file must not look like "no removals were ever recorded").
	wbAdoptTombUntrusted bool
	// wbAdoptTombUntrustedLogOnce keeps the untrusted-store warning to one line
	// per process instead of one per routed candidate.
	wbAdoptTombUntrustedLogOnce sync.Once
	// wbAdoptTombstoneAfterWriteHook is a tests-only park point between the
	// atomic write and the read-back verification (proves the read-back gate).
	wbAdoptTombstoneAfterWriteHook func()
)

// wbResolveAdoptTombstonePath resolves the tombstone file path from the env
// override or the plugin-scoped per-user cache path. The same safety checks as
// the cost state apply: absolute, not credential-like, and never inside the
// host auth directory.
func wbResolveAdoptTombstonePath() (path string, enabled bool, reason string) {
	v := strings.TrimSpace(os.Getenv(wbAdoptTombstoneEnv))
	if v != "" {
		switch strings.ToLower(v) {
		case "off", "none", "-", "disabled":
			return "", false, "disabled by configuration"
		}
	} else {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return "", false, "no per-user cache dir available: " + err.Error()
		}
		v = filepath.Join(cacheDir, "workbuddy-plugin", wbAdoptTombstoneFileName)
	}
	if !filepath.IsAbs(v) {
		return "", false, "path must be absolute"
	}
	if isSafeWorkbuddyAuthPath(v) {
		return "", false, "path looks like a credential path"
	}
	if dir := peerAuthDir(); dir != "" && isPathUnder(v, dir) {
		return "", false, "path is inside the host auth directory"
	}
	return v, true, ""
}

// wbEnsureAdoptTombstonesLocked lazily loads the on-disk document, reloading
// when the configured artifact changes (the env value is the identity of the
// chosen store; production keeps it constant). Any problem makes the store
// unusable (fail-closed); a missing file is a fresh, usable store. Caller must
// hold wbAdoptTombMu.
func wbEnsureAdoptTombstonesLocked() {
	envValue := strings.TrimSpace(os.Getenv(wbAdoptTombstoneEnv))
	if wbAdoptTombLoaded && wbAdoptTombEnvValue == envValue {
		return
	}
	wbAdoptTombLoaded = true
	wbAdoptTombEnvValue = envValue
	wbAdoptTombRows = map[string]wbAdoptTombstoneRow{}
	wbAdoptTombPath = ""
	wbAdoptTombStoreErr = ""
	wbAdoptTombUntrusted = false
	path, ok, reason := wbResolveAdoptTombstonePath()
	if !ok {
		// Ruling: an explicitly disabled / unavailable store loses the record of
		// removed identities, so content cannot be trusted — routing and automatic
		// tasks fail closed exactly as for a corrupt file.
		wbAdoptTombStoreErr = reason
		wbAdoptTombUntrusted = true
		log.Printf("workbuddy: adopt tombstone store unavailable (%s) — WorkBuddy automatic routing/tasks fail closed; configure %s with a writable path to restore them", reason, wbAdoptTombstoneEnv)
		return
	}
	wbAdoptTombPath = path
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return // fresh store
		}
		wbAdoptTombStoreErr = "read failed: " + err.Error()
		wbAdoptTombUntrusted = true
		log.Printf("workbuddy: adopt tombstones %s unreadable (%v) — deletion and automatic adoption are disabled until this is repaired", path, err)
		return
	}
	var doc wbAdoptTombstoneDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		wbAdoptTombStoreErr = "corrupt: " + err.Error()
		wbAdoptTombUntrusted = true
		log.Printf("workbuddy: adopt tombstones %s corrupt (%v) — deletion and automatic adoption are disabled until this is repaired", path, err)
		return
	}
	if doc.SchemaVersion != wbAdoptTombstoneSchemaVersion {
		if doc.SchemaVersion > wbAdoptTombstoneSchemaVersion {
			wbAdoptTombStoreErr = fmt.Sprintf("newer schema v%d on disk", doc.SchemaVersion)
		} else {
			wbAdoptTombStoreErr = fmt.Sprintf("unsupported schema v%d", doc.SchemaVersion)
		}
		wbAdoptTombUntrusted = true
		log.Printf("workbuddy: adopt tombstones %s has %s — leaving it untouched", path, wbAdoptTombStoreErr)
		return
	}
	for _, row := range doc.Tombstones {
		uid := strings.TrimSpace(row.UID)
		if uid == "" {
			continue
		}
		row.UID = uid
		row.Realm = wbNormalizeRealm(row.Realm)
		wbAdoptTombRows[uid+"\x1f"+row.Realm] = row
	}
}

// wbAdoptTombstonesUsable reports whether the tombstone store can be trusted
// for this process (false = fail-closed: no deletion, no automatic adoption).
func wbAdoptTombstonesUsable() (bool, string) {
	wbAdoptTombMu.Lock()
	defer wbAdoptTombMu.Unlock()
	wbEnsureAdoptTombstonesLocked()
	return wbAdoptTombStoreErr == "", wbAdoptTombStoreErr
}

// wbNormalizeRealm maps a realm label onto the cn/global/intl vocabulary.
func wbNormalizeRealm(realm string) string {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "global":
		return "global"
	case "intl":
		return "intl"
	default:
		return "cn"
	}
}

// wbTombstoneBlocksIdentity reports whether an account identity must be treated
// as removed: it is tombstoned, OR the store is untrusted (damaged content, or
// an explicitly disabled/unavailable store — both lose the record of removed
// identities; architect ruling 2026-09-29). Only a FRESH install (usable path,
// no file yet) counts as "no removals known" and does not block.
func wbTombstoneBlocksIdentity(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	wbAdoptTombMu.Lock()
	defer wbAdoptTombMu.Unlock()
	wbEnsureAdoptTombstonesLocked()
	if wbAdoptTombUntrusted {
		wbAdoptTombUntrustedLogOnce.Do(func() {
			log.Printf("workbuddy: tombstone store untrusted (%s) — routing/automatic filters fail closed until it is repaired", wbAdoptTombStoreErr)
		})
		return true
	}
	_, ok := wbAdoptTombRows[key]
	return ok
}

// wbTombstoneBlocksAuth is wbTombstoneBlocksIdentity for a parsed credential.
func wbTombstoneBlocksAuth(sa *storedAuth) bool {
	return wbTombstoneBlocksIdentity(wbAuthIdentityOf(sa).key())
}

// wbIsAdoptTombstoned reports whether this credential's identity was
// deliberately removed. Callers that guard destructive or recreating actions
// must ALSO check wbAdoptTombstonesUsable: an unusable store yields no rows
// and must be treated as fail-closed, not as "nothing was removed".
func wbIsAdoptTombstoned(sa *storedAuth) bool {
	if sa == nil {
		return false
	}
	key := wbAuthIdentityOf(sa).key()
	if key == "" {
		return false
	}
	wbAdoptTombMu.Lock()
	defer wbAdoptTombMu.Unlock()
	wbEnsureAdoptTombstonesLocked()
	_, ok := wbAdoptTombRows[key]
	return ok
}

// wbIsTombstonedIdentity reports whether an identity key (wbIdentityKey) was
// deliberately removed. Used by the enforcement filters (routing, automatic
// tasks, lifecycle) that must treat a preserved copy as removed.
func wbIsTombstonedIdentity(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	wbAdoptTombMu.Lock()
	defer wbAdoptTombMu.Unlock()
	wbEnsureAdoptTombstonesLocked()
	_, ok := wbAdoptTombRows[key]
	return ok
}

// wbClearAdoptTombstone durably removes the removal record for this identity:
// atomic write, then a read-back proving the row is GONE. Symmetric with
// wbRecordDeletionTombstone — any failure (store unusable, write error, or a
// read-back that still sees the row) restores the in-memory row and returns an
// error, so the identity stays BLOCKED in memory and on disk consistently.
func wbClearAdoptTombstone(sa *storedAuth) error {
	key := wbAuthIdentityOf(sa).key()
	if key == "" {
		return fmt.Errorf("cannot clear adopt tombstone: credential has no uid")
	}
	wbAdoptTombMu.Lock()
	defer wbAdoptTombMu.Unlock()
	wbEnsureAdoptTombstonesLocked()
	prev, ok := wbAdoptTombRows[key]
	if !ok {
		return nil // nothing recorded
	}
	if wbAdoptTombStoreErr != "" {
		return fmt.Errorf("adopt tombstone store unusable (%s) — identity stays blocked", wbAdoptTombStoreErr)
	}
	if wbAdoptTombPath == "" {
		return fmt.Errorf("adopt tombstone path unavailable — identity stays blocked")
	}
	delete(wbAdoptTombRows, key)
	if err := wbWriteAdoptTombstonesLocked(); err != nil {
		wbAdoptTombRows[key] = prev
		return fmt.Errorf("adopt tombstone clear write failed: %w — identity stays blocked", err)
	}
	if hook := wbAdoptTombstoneAfterWriteHook; hook != nil {
		hook()
	}
	if !wbVerifyAdoptTombstoneAbsentOnDiskLocked(key) {
		// The clear left a document we cannot trust. Restore the row in memory
		// AND on disk, then re-verify: only a verified rollback may be reported
		// as "memory and disk consistently blocked". If even the rollback cannot
		// be verified the store is marked unusable for this process (adoption
		// skipped, deletion refused).
		wbAdoptTombRows[key] = prev
		if werr := wbWriteAdoptTombstonesLocked(); werr != nil {
			wbAdoptTombStoreErr = "clear read-back failed; rollback write failed: " + werr.Error()
			return fmt.Errorf("adopt tombstone clear read-back failed and rollback write failed: %w — identity stays blocked in this process (cross-restart state unverified)", werr)
		}
		if !wbVerifyAdoptTombstoneOnDiskLocked(key) {
			wbAdoptTombStoreErr = "clear read-back failed; rollback could not be verified"
			return fmt.Errorf("adopt tombstone clear read-back failed and rollback could not be verified — identity stays blocked in this process (cross-restart state unverified)")
		}
		return fmt.Errorf("adopt tombstone clear read-back failed; the removal record was restored and verified on disk — identity stays blocked")
	}
	return nil
}

// wbVerifyAdoptTombstoneAbsentOnDiskLocked re-reads the store and proves the
// document is intact at the expected schema AND no row carries this key.
func wbVerifyAdoptTombstoneAbsentOnDiskLocked(key string) bool {
	raw, err := os.ReadFile(wbAdoptTombPath)
	if err != nil {
		return false
	}
	var doc wbAdoptTombstoneDoc
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	if doc.SchemaVersion != wbAdoptTombstoneSchemaVersion {
		return false
	}
	for _, row := range doc.Tombstones {
		if strings.TrimSpace(row.UID)+"\x1f"+wbNormalizeRealm(row.Realm) == key {
			return false
		}
	}
	return true
}

// wbVerifyCanonicalCredentialOnDisk reports whether the canonical file for this
// identity exists in the host auth directory and parses to the SAME identity.
// It is the evidence gate for a deliberate revival: the plugin cannot observe
// the host's own post-poll save, so only a canonical file it can read back
// counts.
func wbVerifyCanonicalCredentialOnDisk(sa *storedAuth) bool {
	if sa == nil {
		return false
	}
	dir := peerAuthDir()
	if dir == "" {
		return false
	}
	path := filepath.Join(dir, authFileNameFor(sa))
	if !isSafeWorkbuddyAuthPath(path) {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	diskSA, perr := parseStored(raw)
	if perr != nil || diskSA == nil {
		return false
	}
	want := wbAuthIdentityOf(sa).key()
	got := wbAuthIdentityOf(diskSA).key()
	return want != "" && want == got
}

// wbClearTombstoneAfterVerifiedCanonicalWrite is the ONLY revival entry point
// for the login/import paths. It clears the removal record only when the
// canonical credential is verifiably on disk with the same identity; otherwise
// the identity stays blocked and the reason is returned for explicit reporting
// (never a silent success).
func wbClearTombstoneAfterVerifiedCanonicalWrite(sa *storedAuth) (bool, string) {
	if sa == nil {
		return false, "no credential"
	}
	if !wbIsAdoptTombstoned(sa) {
		return false, "" // nothing recorded
	}
	if !wbVerifyCanonicalCredentialOnDisk(sa) {
		return false, "canonical credential not verified on disk (fail-closed: identity stays blocked)"
	}
	if err := wbClearAdoptTombstone(sa); err != nil {
		return false, err.Error() + " (fail-closed: identity stays blocked)"
	}
	return true, ""
}

// wbRecordDeletionTombstone durably records one removal: atomic write, then a
// READ-BACK that proves the row is on disk. Any failure returns an error and
// restores the previous in-memory state; the caller must then fail closed (do
// NOT delete). target is the concrete file being removed (diagnostics).
func wbRecordDeletionTombstone(sa *storedAuth, target string) error {
	e := wbAuthIdentityOf(sa)
	key := e.key()
	if key == "" {
		return fmt.Errorf("cannot record adopt tombstone: credential has no uid")
	}
	wbAdoptTombMu.Lock()
	defer wbAdoptTombMu.Unlock()
	wbEnsureAdoptTombstonesLocked()
	if wbAdoptTombStoreErr != "" {
		return fmt.Errorf("adopt tombstone store unusable (%s)", wbAdoptTombStoreErr)
	}
	row := wbAdoptTombstoneRow{
		UID:       e.UID,
		Realm:     wbNormalizeRealm(e.Realm),
		Canonical: strings.TrimSpace(target),
		RemovedAt: time.Now().UTC().Format(time.RFC3339),
	}
	prev, hadPrev := wbAdoptTombRows[key]
	wbAdoptTombRows[key] = row
	revert := func() {
		if hadPrev {
			wbAdoptTombRows[key] = prev
		} else {
			delete(wbAdoptTombRows, key)
		}
	}
	if err := wbWriteAdoptTombstonesLocked(); err != nil {
		revert()
		return fmt.Errorf("adopt tombstone write failed: %w", err)
	}
	if hook := wbAdoptTombstoneAfterWriteHook; hook != nil {
		hook()
	}
	if !wbVerifyAdoptTombstoneOnDiskLocked(key) {
		revert()
		return fmt.Errorf("adopt tombstone read-back verification failed for %s", wbAdoptTombPath)
	}
	return nil
}

// wbVerifyAdoptTombstoneOnDiskLocked re-reads the store and proves the row is
// present and the document is at the expected schema version.
func wbVerifyAdoptTombstoneOnDiskLocked(key string) bool {
	if wbAdoptTombPath == "" {
		return false
	}
	raw, err := os.ReadFile(wbAdoptTombPath)
	if err != nil {
		return false
	}
	var doc wbAdoptTombstoneDoc
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	if doc.SchemaVersion != wbAdoptTombstoneSchemaVersion {
		return false
	}
	for _, row := range doc.Tombstones {
		if strings.TrimSpace(row.UID)+"\x1f"+wbNormalizeRealm(row.Realm) == key {
			return true
		}
	}
	return false
}

// wbWriteAdoptTombstonesLocked serializes and atomically writes the document.
// Caller must hold wbAdoptTombMu.
func wbWriteAdoptTombstonesLocked() error {
	rows := make([]wbAdoptTombstoneRow, 0, len(wbAdoptTombRows))
	for _, row := range wbAdoptTombRows {
		rows = append(rows, row)
	}
	// Deterministic file content (map iteration is random): sort by uid+realm.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].UID != rows[j].UID {
			return rows[i].UID < rows[j].UID
		}
		return rows[i].Realm < rows[j].Realm
	})
	doc := wbAdoptTombstoneDoc{
		SchemaVersion: wbAdoptTombstoneSchemaVersion,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
		Tombstones:    rows,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return wbAtomicWriteFile(wbAdoptTombPath, raw)
}
