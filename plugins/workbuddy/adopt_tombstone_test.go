package main

// adopt_tombstone_test.go — N1 second-audit fix (red → green).
//
// Adoption keeps the legacy source file (non-destructive policy), so after the
// canonical file is deliberately deleted (lifecycle deleteAuth), a later
// adoption run would re-create it — resurrecting a removed account. The
// plugin-owned tombstone must block that, across restarts, without ever
// modifying the legacy credential.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestAdoptTombstone_DeleteThenReAdoptSkipped walks the full chain:
// adopt → deleteAuth → re-adopt → restart → re-adopt.
func TestAdoptTombstone_DeleteReAdoptAndRestart(t *testing.T) {
	const uid = "uid-tomb-1"
	// The plugin-owned state lives OUTSIDE the (simulated) host auth dir.
	stateDir := t.TempDir()
	dir := t.TempDir()
	t.Setenv(wbAdoptTombstoneEnv, filepath.Join(stateDir, "adopt-tombstones.json"))
	resetAdoptTombstonesForTest(t)

	legacyName := "codebuddy-cn-" + uid + ".json"
	canonicalName := "workbuddy-" + uid + ".json"
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok-legacy", RefreshToken: "ref-legacy", Domain: "codebuddy.cn"},
		Account: storedAccount{UID: uid, Nickname: "tomb"},
	}
	legacyJSON, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, legacyName)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalPath := filepath.Join(dir, canonicalName)

	// Host stubs: the list + physical view follow the on-disk reality.
	listName := legacyName
	physName, physPath := legacyName, legacyPath
	prevList, prevGetPhys, prevPersist := hostAuthListFn, hostAuthGetPhysicalFn, hostAuthPersistFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{ID: "id-tomb", AuthIndex: "idx-tomb", Name: listName, Path: filepath.Join(dir, listName), Type: "workbuddy"},
		}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-tomb", Name: physName, Path: physPath, JSON: legacyJSON}, nil
	}
	var saves int32
	hostAuthPersistFn = func(string, string, []byte) error { atomic.AddInt32(&saves, 1); return nil }
	t.Cleanup(func() {
		hostAuthListFn, hostAuthGetPhysicalFn, hostAuthPersistFn = prevList, prevGetPhys, prevPersist
	})

	// 1. First adoption: legacy → canonical.
	adoptForeignAuths()
	if got := atomic.LoadInt32(&saves); got != 1 {
		t.Fatalf("first adoption saved %d time(s), want 1", got)
	}

	// 2. Lifecycle delete of the canonical credential (exhausted Global-style
	//    removal, simplified to the shared delete path).
	physName, physPath = canonicalName, canonicalPath
	if err := os.WriteFile(canonicalPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteAuth("idx-tomb", "id-tomb", sa); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	if _, err := os.Stat(canonicalPath); !os.IsNotExist(err) {
		t.Fatalf("canonical file still present after delete (err=%v)", err)
	}
	rawTomb, err := os.ReadFile(filepath.Join(stateDir, "adopt-tombstones.json"))
	if err != nil {
		t.Fatalf("tombstone file not written: %v", err)
	}
	if !containsAll(string(rawTomb), uid, "cn") {
		t.Fatalf("tombstone file does not record the removed identity: %s", rawTomb)
	}

	// 3. The canonical is gone, the legacy source is still there: adoption must
	//    NOT re-create it.
	listName, physName, physPath = legacyName, legacyName, legacyPath
	adoptForeignAuths()
	if got := atomic.LoadInt32(&saves); got != 1 {
		t.Fatalf("re-adoption saved %d time(s) after deletion, want 1 (resurrection)", got)
	}

	// 4. Restart: drop the in-memory set, reload from disk, and try again.
	resetAdoptTombstonesForTest(t)
	adoptForeignAuths()
	if got := atomic.LoadInt32(&saves); got != 1 {
		t.Fatalf("post-restart re-adoption saved %d time(s), want 1 (tombstone must survive restart)", got)
	}

	// The legacy credential is untouched byte-for-byte (non-destructive policy).
	gotLegacy, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("legacy file unreadable: %v", err)
	}
	if string(gotLegacy) != string(legacyJSON) {
		t.Fatalf("legacy credential was modified: %s", gotLegacy)
	}
}

// TestAdoptTombstone_ExplicitReimportNotBlocked: the tombstone only guards the
// automatic adoption path; a direct canonical write (login/import) is a
// deliberate user action and must still work. Here the canonical simply exists
// again, which adoption treats as "already present" — the point is that the
// tombstone check never deletes or blocks the live file.
func TestAdoptTombstone_ExplicitReimportNotBlocked(t *testing.T) {
	const uid = "uid-tomb-2"
	stateDir := t.TempDir()
	dir := t.TempDir()
	t.Setenv(wbAdoptTombstoneEnv, filepath.Join(stateDir, "adopt-tombstones.json"))
	resetAdoptTombstonesForTest(t)

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", RefreshToken: "ref", Domain: "codebuddy.cn"},
		Account: storedAccount{UID: uid, Nickname: "reimport"},
	}
	// Simulate "the user re-imported": a recorded tombstone + the canonical file
	// already back on disk.
	if err := wbRecordDeletionTombstone(sa, "workbuddy-"+uid+".json"); err != nil {
		t.Fatalf("record tombstone: %v", err)
	}
	if !wbIsAdoptTombstoned(sa) {
		t.Fatal("tombstone not recorded")
	}
	canonicalName := "workbuddy-" + uid + ".json"
	raw, _ := json.Marshal(sa)
	prevList, prevGetPhys, prevPersist := hostAuthListFn, hostAuthGetPhysicalFn, hostAuthPersistFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{ID: "id-c", AuthIndex: "idx-c", Name: canonicalName, Path: filepath.Join(dir, canonicalName), Type: "workbuddy"},
		}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-c", Name: canonicalName, Path: filepath.Join(dir, canonicalName), JSON: raw}, nil
	}
	var writes int32
	hostAuthPersistFn = func(string, string, []byte) error { atomic.AddInt32(&writes, 1); return nil }
	t.Cleanup(func() {
		hostAuthListFn, hostAuthGetPhysicalFn, hostAuthPersistFn = prevList, prevGetPhys, prevPersist
	})

	// No legacy source is present, so adoption is a no-op; the live canonical
	// file must not be touched.
	adoptForeignAuths()
	if got := atomic.LoadInt32(&writes); got != 0 {
		t.Fatalf("adoption wrote %d time(s) while a live canonical file exists", got)
	}
	if !wbIsAdoptTombstoned(sa) {
		t.Fatal("tombstone must remain in place")
	}
}

// TestAdoptTombstonePathSafety: the tombstone artifact must never live on a
// credential path or inside the host auth directory.
func TestAdoptTombstonePathSafety(t *testing.T) {
	dir := t.TempDir()

	t.Setenv(wbAdoptTombstoneEnv, "off")
	if _, ok, _ := wbResolveAdoptTombstonePath(); ok {
		t.Fatal(`"off" must disable persistence`)
	}
	t.Setenv(wbAdoptTombstoneEnv, "relative/tomb.json")
	if _, ok, _ := wbResolveAdoptTombstonePath(); ok {
		t.Fatal("relative path must be rejected")
	}
	t.Setenv(wbAdoptTombstoneEnv, filepath.Join(dir, "workbuddy-someuid.json"))
	if _, ok, _ := wbResolveAdoptTombstonePath(); ok {
		t.Fatal("credential-like file name must be rejected")
	}

	// Inside the host auth dir: peerAuthDir is derived from the host list.
	prevList, prevGetPhys := hostAuthListFn, hostAuthGetPhysicalFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{ID: "id-a", AuthIndex: "idx-a", Name: "workbuddy-uid-a.json", Path: filepath.Join(dir, "workbuddy-uid-a.json")},
		}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) { return &hostAuthPhysical{}, nil }
	t.Cleanup(func() { hostAuthListFn, hostAuthGetPhysicalFn = prevList, prevGetPhys })

	t.Setenv(wbAdoptTombstoneEnv, filepath.Join(dir, "adopt-tombstones.json"))
	if _, ok, _ := wbResolveAdoptTombstonePath(); ok {
		t.Fatal("path inside the host auth directory must be rejected")
	}

	// A plugin-owned sibling path outside the auth dir is accepted.
	stateDir := t.TempDir()
	t.Setenv(wbAdoptTombstoneEnv, filepath.Join(stateDir, "adopt-tombstones.json"))
	if _, ok, _ := wbResolveAdoptTombstonePath(); !ok {
		t.Fatal("plugin-owned state path must be accepted")
	}
}

func containsAll(s string, needles ...string) bool {
	low := strings.ToLower(s)
	for _, n := range needles {
		if !strings.Contains(low, strings.ToLower(n)) {
			return false
		}
	}
	return true
}
