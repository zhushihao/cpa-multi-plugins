package main

// tombstone_revival_test.go — post-freeze P0 (round 3):
//
// persistLoginCredential cleared the removal record BEFORE its own save, and
// the clear itself was a best-effort in-memory delete: a failed save (or a
// failed clear write) left the identity unblocked in this process while a
// restart re-read the stale tombstone. The revival path must be symmetric with
// the removal path:
//
//   - clear ONLY after a confirmed canonical write whose read-back on disk
//     carries the same identity;
//   - the clear itself is atomic + read-back verified; on ANY failure the
//     identity stays BLOCKED (in memory and on disk stay consistent);
//   - the host's own post-poll primary save is not observable here, so a fresh
//     login whose plugin-side save fails is NOT treated as a reliable revival
//     (G07 boundary).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// revivalEnv: tombstone recorded for one CN account, host seams installed, and
// an auth dir where the canonical file can be written/verified.
type revivalEnv struct {
	*tombFailEnv
	authDir string
}

func newRevivalEnv(t *testing.T) *revivalEnv {
	t.Helper()
	e := &revivalEnv{tombFailEnv: newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json")), authDir: t.TempDir()}
	if err := wbRecordDeletionTombstone(e.sa, e.canonicalName); err != nil {
		t.Fatalf("record tombstone: %v", err)
	}
	// peerAuthDir resolves via the host list's Path.
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID: "id-fc", AuthIndex: "idx-fc", Name: e.canonicalName, Path: filepath.Join(e.authDir, e.canonicalName), Type: "workbuddy",
		}}, nil
	}
	return e
}

func (e *revivalEnv) writeCanonical(t *testing.T, raw []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.authDir, e.canonicalName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoginPersistFailureKeepsTombstone: a failed plugin-side save must NOT
// clear the removal record (the host's primary save is not observable here).
func TestLoginPersistFailureKeepsTombstone(t *testing.T) {
	e := newRevivalEnv(t)
	prev := hostAuthPersistFn
	hostAuthPersistFn = func(string, string, []byte) error { return os.ErrPermission }
	t.Cleanup(func() { hostAuthPersistFn = prev })

	persistLoginCredential(e.sa)
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("failed login persist cleared the tombstone (fail-open revival)")
	}
	// Restart: still blocked (on-disk record untouched).
	resetAdoptTombstonesForTest(t)
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("tombstone must survive a restart after a failed login persist")
	}
}

// TestLoginPersistWithoutVerifiedFileKeepsTombstone: even a "successful" save
// RPC with no verifiable canonical file on disk must not revive the identity.
func TestLoginPersistWithoutVerifiedFileKeepsTombstone(t *testing.T) {
	e := newRevivalEnv(t)
	prev := hostAuthPersistFn
	hostAuthPersistFn = func(string, string, []byte) error { return nil } // no file written
	t.Cleanup(func() { hostAuthPersistFn = prev })

	persistLoginCredential(e.sa)
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("tombstone cleared without a verified canonical file on disk")
	}
}

// TestLoginPersistClearsAfterVerifiedWrite: the revival clears only after the
// canonical file is on disk with the same identity, and the clear is durable.
func TestLoginPersistClearsAfterVerifiedWrite(t *testing.T) {
	e := newRevivalEnv(t)
	raw, _ := json.Marshal(e.sa)
	prev := hostAuthPersistFn
	hostAuthPersistFn = func(name, path string, r []byte) error {
		return os.WriteFile(filepath.Join(e.authDir, e.canonicalName), r, 0o600)
	}
	t.Cleanup(func() { hostAuthPersistFn = prev })
	_ = raw

	persistLoginCredential(e.sa)
	if wbIsAdoptTombstoned(e.sa) {
		t.Fatal("revival after a verified canonical write must clear the tombstone")
	}
	resetAdoptTombstonesForTest(t)
	if wbIsAdoptTombstoned(e.sa) {
		t.Fatal("cleared tombstone must be durable across a restart")
	}
}

// TestTombstoneClearWriteFailureKeepsBlocked: a failed clear write must keep the
// identity blocked in memory AND on disk (no silent unblock).
func TestTombstoneClearWriteFailureKeepsBlocked(t *testing.T) {
	e := newRevivalEnv(t)
	tombPath := e.tombPath()
	// Make the tombstone file read-only so the atomic replace fails on Windows.
	if err := os.Chmod(tombPath, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tombPath, 0o600) })

	if err := wbClearAdoptTombstone(e.sa); err == nil {
		t.Fatal("clear must report a write failure")
	}
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("in-memory row must be restored when the clear write fails")
	}
	resetAdoptTombstonesForTest(t)
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("on-disk row must still block after a failed clear")
	}
}

// TestTombstoneClearReadBackMismatchKeepsBlocked: a clear whose read-back still
// sees the row must fail closed, roll the row back on disk, and survive a
// restart (this hook restores a valid document containing the row).
func TestTombstoneClearReadBackMismatchKeepsBlocked(t *testing.T) {
	e := newRevivalEnv(t)
	prevHook := wbAdoptTombstoneAfterWriteHook
	wbAdoptTombstoneAfterWriteHook = func() {
		doc := wbAdoptTombstoneDoc{
			SchemaVersion: wbAdoptTombstoneSchemaVersion,
			Tombstones:    []wbAdoptTombstoneRow{{UID: e.uid, Realm: "cn"}},
		}
		raw, _ := json.Marshal(doc)
		_ = os.WriteFile(e.tombPath(), raw, 0o600)
	}
	t.Cleanup(func() { wbAdoptTombstoneAfterWriteHook = prevHook })

	if err := wbClearAdoptTombstone(e.sa); err == nil {
		t.Fatal("clear must report a read-back mismatch")
	}
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("identity must stay blocked when the clear cannot be verified")
	}
	// The rollback rewrite is verified: a restart must still see the row.
	resetAdoptTombstonesForTest(t)
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("rollback must be durable: restart must still block the identity")
	}
}

// TestTombstoneClearReadBackMismatchRepairsDiskAndSurvivesRestart: a transient
// damage (the file is deleted between the clear write and its read-back) must be
// repaired by the verified rollback, so a restart is still blocked.
func TestTombstoneClearReadBackMismatchRepairsDiskAndSurvivesRestart(t *testing.T) {
	e := newRevivalEnv(t)
	prevHook := wbAdoptTombstoneAfterWriteHook
	wbAdoptTombstoneAfterWriteHook = func() { _ = os.Remove(e.tombPath()) }
	t.Cleanup(func() { wbAdoptTombstoneAfterWriteHook = prevHook })

	if err := wbClearAdoptTombstone(e.sa); err == nil {
		t.Fatal("clear must report the damaged read-back")
	}
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("in-memory state must stay blocked")
	}
	resetAdoptTombstonesForTest(t)
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("verified rollback must repair the on-disk document: restart must still block")
	}
}

// TestTombstoneClearTamperDamageKeepsFailClosedAcrossRestart: an external actor
// keeps the store damaged (read-only corrupt document) so neither the clear nor
// its rollback can be verified. The process must fail closed AND a restart must
// load the store as unusable (adoption skipped, deletion refused).
//
// Boundary (G07): an actor with write access to the plugin state file can also
// delete a healthy tombstone file outright; cross-restart guarantees therefore
// depend on file-system permissions/integrity, not on plugin-local code.
func TestTombstoneClearTamperDamageKeepsFailClosedAcrossRestart(t *testing.T) {
	e := newRevivalEnv(t)
	tombPath := e.tombPath()
	prevHook := wbAdoptTombstoneAfterWriteHook
	wbAdoptTombstoneAfterWriteHook = func() {
		_ = os.Remove(tombPath)
		_ = os.WriteFile(tombPath, []byte("{corrupt"), 0o400)
	}
	t.Cleanup(func() {
		wbAdoptTombstoneAfterWriteHook = prevHook
		_ = os.Chmod(tombPath, 0o600)
	})

	if err := wbClearAdoptTombstone(e.sa); err == nil {
		t.Fatal("clear must report that the store cannot be trusted")
	}
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("in-memory state must stay blocked")
	}
	// In-process fail-closed: the store is unusable.
	if usable, _ := wbAdoptTombstonesUsable(); usable {
		t.Fatal("store must be unusable after an unverifiable clear")
	}
	// Restart: the corrupt document must load as unusable, not as "no removals".
	resetAdoptTombstonesForTest(t)
	if usable, reason := wbAdoptTombstonesUsable(); usable {
		t.Fatalf("restart must load the damaged store as unusable (reason=%q)", reason)
	}
	// And adoption must refuse to run on it.
	legacyName := "codebuddy-cn-" + e.uid + ".json"
	legacyPath := filepath.Join(e.authDir, legacyName)
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID: "id-legacy-copy", AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, Type: "codebuddy-cn",
		}}, nil
	}
	hostAuthGetFn = func(string) (*storedAuth, error) { return e.sa, nil }
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, JSON: legacyJSON}, nil
	}
	var saves int32
	prevPersist := hostAuthPersistFn
	hostAuthPersistFn = func(string, string, []byte) error { atomic.AddInt32(&saves, 1); return nil }
	t.Cleanup(func() { hostAuthPersistFn = prevPersist })
	adoptForeignAuths()
	if got := atomic.LoadInt32(&saves); got != 0 {
		t.Fatalf("adoption must be skipped on an unusable store (%d save(s))", got)
	}
}

// TestFailedRevivalKeepsIdentityFiltered is the end-to-end consequence: after a
// failed login persist the preserved legacy copy must still be unroutable.
func TestFailedRevivalKeepsIdentityFiltered(t *testing.T) {
	e := newRevivalEnv(t)
	prev := hostAuthPersistFn
	hostAuthPersistFn = func(string, string, []byte) error { return os.ErrPermission }
	t.Cleanup(func() { hostAuthPersistFn = prev })
	persistLoginCredential(e.sa)

	legacyName := "codebuddy-cn-" + e.uid + ".json"
	legacyPath := filepath.Join(e.authDir, legacyName)
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID: "id-legacy-copy", AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, Type: "codebuddy-cn",
		}}, nil
	}
	hostAuthGetFn = func(string) (*storedAuth, error) { return e.sa, nil }
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, JSON: legacyJSON}, nil
	}
	resetAuthUIDCacheForTest(t)
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider:   providerName,
		Model:      "wb-revival-filter",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "id-legacy-copy", Provider: providerName}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := parsePickResponse(t, raw); resp.Handled {
		t.Fatalf("a failed revival must not make the preserved copy routable, got %+v", resp)
	}
}
