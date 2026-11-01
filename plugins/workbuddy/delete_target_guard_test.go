package main

// delete_target_guard_test.go — round-2 review fixes (red → green):
//
//  1. deleteAuth recorded the tombstone from the caller's sa while deleting
//     whatever phys.Path pointed at. On cache/concurrency/index drift the two
//     can disagree, so the plugin could delete account B's credential while
//     tombstoning account A (leaving B resurrectable and A spuriously blocked).
//     The physical target's identity must be parsed and verified against the
//     requested credential, or the delete fails closed.
//  2. A preserved legacy codebuddy-cn / codebuddy-intl SOURCE file must never be
//     the delete target (I6: old credential files are read-only rollback
//     material).
//  3. The disable/reenable/delete write target must never be a legacy source:
//     resolveAuthFileTarget used to keep "codebuddy-cn-<uid>.json" as the save
//     name, so a disable could overwrite the preserved source.
//  4. removeLegacyDualName auto-deleted a same-UID workbuddy.json. The I6
//     policy preserves old files; the delete path must leave the dual copy in
//     place.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type delGuardEnv struct {
	authDir     string
	sa          *storedAuth
	phys        *hostAuthPhysical
	persists    int32
	lastName    string
	lastPath    string
	lastLegacy  string
	lastRaw     []byte
	tombWritten bool
}

func delGuardAuth(uid, domain string, token string) *storedAuth {
	return &storedAuth{
		Auth:    storedTokens{AccessToken: token, RefreshToken: "ref-" + token, Domain: domain},
		Account: storedAccount{UID: uid, Nickname: uid},
	}
}

// newDelGuardEnv installs the host seams for one delete/disable call.
func newDelGuardEnv(t *testing.T, sa *storedAuth, phys *hostAuthPhysical, tombEnv string) *delGuardEnv {
	t.Helper()
	e := &delGuardEnv{authDir: t.TempDir(), sa: sa, phys: phys}
	t.Setenv(wbAdoptTombstoneEnv, tombEnv)
	resetAdoptTombstonesForTest(t)
	resetLifecycleDedupeForTest(t)

	prevGet, prevPersist := hostAuthGetPhysicalFn, hostAuthPersistMigrateFn
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) { return e.phys, nil }
	hostAuthPersistMigrateFn = func(name, path, legacyPath string, raw []byte) error {
		atomic.AddInt32(&e.persists, 1)
		e.lastName, e.lastPath, e.lastLegacy = name, path, legacyPath
		e.lastRaw = append([]byte(nil), raw...)
		return nil
	}
	t.Cleanup(func() { hostAuthGetPhysicalFn, hostAuthPersistMigrateFn = prevGet, prevPersist })
	return e
}

func writeAuthFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDeleteAuth_RefusesIdentityMismatch: sa=A but the physical credential is
// B; nothing may be deleted, no tombstone may be recorded.
func TestDeleteAuth_RefusesIdentityMismatch(t *testing.T) {
	saA := delGuardAuth("uid-A", "codebuddy.cn", "tok-A")
	saB := delGuardAuth("uid-B", "codebuddy.cn", "tok-B")
	dir := t.TempDir()
	pathB := filepath.Join(dir, "workbuddy-uid-B.json")
	rawB, _ := json.Marshal(saB)
	writeAuthFile(t, pathB, rawB)

	e := newDelGuardEnv(t, saA, &hostAuthPhysical{
		AuthIndex: "idx-shared", Name: "workbuddy-uid-B.json", Path: pathB, JSON: rawB,
	}, filepath.Join(t.TempDir(), "adopt-tombstones.json"))

	if err := deleteAuth("idx-shared", "aid-shared", saA); err == nil {
		t.Fatal("deleteAuth must fail closed when the physical credential identity differs")
	}
	if got, err := os.ReadFile(pathB); err != nil || string(got) != string(rawB) {
		t.Fatalf("account B's credential was touched (err=%v, bytes=%d)", err, len(got))
	}
	if e.persists != 0 {
		t.Fatalf("identity mismatch must not write anything, got %d persist(s)", e.persists)
	}
	if wbIsAdoptTombstoned(saA) {
		t.Fatal("account A must not be tombstoned when nothing was deleted")
	}
	if wbIsAdoptTombstoned(saB) {
		t.Fatal("account B must not be tombstoned by A's delete")
	}
}

// TestDeleteAuth_RefusesRealmMismatch: same uid, different realm is a different
// account (CN vs Intl).
func TestDeleteAuth_RefusesRealmMismatch(t *testing.T) {
	saCN := delGuardAuth("uid-both", "codebuddy.cn", "tok-cn")
	saIntl := delGuardAuth("uid-both", "www.codebuddy.ai", "tok-intl")
	dir := t.TempDir()
	pathIntl := filepath.Join(dir, "workbuddy-intl-uid-both.json")
	rawIntl, _ := json.Marshal(saIntl)
	writeAuthFile(t, pathIntl, rawIntl)

	newDelGuardEnv(t, saCN, &hostAuthPhysical{
		AuthIndex: "idx-realm", Name: "workbuddy-intl-uid-both.json", Path: pathIntl, JSON: rawIntl,
	}, filepath.Join(t.TempDir(), "adopt-tombstones.json"))

	if err := deleteAuth("idx-realm", "aid-realm", saCN); err == nil {
		t.Fatal("deleteAuth must fail closed on a realm mismatch")
	}
	if got, err := os.ReadFile(pathIntl); err != nil || string(got) != string(rawIntl) {
		t.Fatalf("Intl credential was touched (err=%v)", err)
	}
}

// TestDeleteAuth_RefusesUnparsablePhysical: without a verifiable physical
// credential the target identity is unknown → fail closed.
func TestDeleteAuth_RefusesUnparsablePhysical(t *testing.T) {
	sa := delGuardAuth("uid-unparsable", "codebuddy.cn", "tok-u")
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-uid-unparsable.json")
	writeAuthFile(t, path, []byte("{not a credential"))

	newDelGuardEnv(t, sa, &hostAuthPhysical{
		AuthIndex: "idx-u", Name: "workbuddy-uid-unparsable.json", Path: path, JSON: []byte("{not a credential"),
	}, filepath.Join(t.TempDir(), "adopt-tombstones.json"))

	if err := deleteAuth("idx-u", "aid-u", sa); err == nil {
		t.Fatal("deleteAuth must fail closed when the physical credential is unparsable")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file must be preserved (err=%v)", err)
	}
}

// TestDeleteAuth_RefusesLegacySourceTarget: the host-reported physical target is
// a preserved codebuddy-cn source for the same identity — never delete it.
func TestDeleteAuth_RefusesLegacySourceTarget(t *testing.T) {
	sa := delGuardAuth("uid-legacy-src", "codebuddy.cn", "tok-ls")
	dir := t.TempDir()
	src := filepath.Join(dir, "codebuddy-cn-uid-legacy-src.json")
	raw, _ := json.Marshal(sa)
	writeAuthFile(t, src, raw)

	newDelGuardEnv(t, sa, &hostAuthPhysical{
		AuthIndex: "idx-ls", Name: "codebuddy-cn-uid-legacy-src.json", Path: src, JSON: raw,
	}, filepath.Join(t.TempDir(), "adopt-tombstones.json"))

	if err := deleteAuth("idx-ls", "aid-ls", sa); err == nil {
		t.Fatal("deleteAuth must refuse to delete a preserved legacy codebuddy source")
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != string(raw) {
		t.Fatalf("legacy source was modified/deleted (err=%v)", err)
	}
	if wbIsAdoptTombstoned(sa) {
		t.Fatal("no tombstone may be recorded when the delete was refused")
	}
}

// TestDisableAuth_NeverOverwritesLegacySource: the disable write must target the
// canonical name, never the preserved source file.
func TestDisableAuth_NeverOverwritesLegacySource(t *testing.T) {
	sa := delGuardAuth("uid-disable-src", "codebuddy.cn", "tok-ds")
	dir := t.TempDir()
	src := filepath.Join(dir, "codebuddy-cn-uid-disable-src.json")
	raw, _ := json.Marshal(sa)
	writeAuthFile(t, src, raw)

	e := newDelGuardEnv(t, sa, &hostAuthPhysical{
		AuthIndex: "idx-ds", Name: "codebuddy-cn-uid-disable-src.json", Path: src, JSON: raw,
	}, filepath.Join(t.TempDir(), "adopt-tombstones.json"))

	if err := disableAuth("idx-ds", "aid-ds", sa, nil, "exhausted"); err != nil {
		t.Fatalf("disableAuth: %v", err)
	}
	if e.persists != 1 {
		t.Fatalf("disable persisted %d time(s), want 1", e.persists)
	}
	if e.lastName != "workbuddy-uid-disable-src.json" {
		t.Fatalf("disable wrote name %q, want the canonical workbuddy-uid-disable-src.json (legacy source must never be the target)", e.lastName)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != string(raw) {
		t.Fatalf("legacy source was modified (err=%v)", err)
	}
}

// TestDeleteAuth_PreservesSameUIDDualName: the delete path removes the canonical
// file but must PRESERVE the dual-named legacy copy (preservation-first).
func TestDeleteAuth_PreservesSameUIDDualName(t *testing.T) {
	sa := delGuardAuth("uid-dual", "codebuddy.cn", "tok-dual")
	dir := t.TempDir()
	canonical := filepath.Join(dir, "workbuddy-uid-dual.json")
	dual := filepath.Join(dir, authFileName) // workbuddy.json
	raw, _ := json.Marshal(sa)
	writeAuthFile(t, canonical, raw)
	writeAuthFile(t, dual, raw)

	newDelGuardEnv(t, sa, &hostAuthPhysical{
		AuthIndex: "idx-dual", Name: "workbuddy-uid-dual.json", Path: canonical, JSON: raw,
	}, filepath.Join(t.TempDir(), "adopt-tombstones.json"))

	if err := deleteAuth("idx-dual", "aid-dual", sa); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("canonical file should be deleted (err=%v)", err)
	}
	got, err := os.ReadFile(dual)
	if err != nil {
		t.Fatalf("dual-named legacy copy must be preserved: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("dual-named legacy copy was modified")
	}
}

// TestResolveAuthFileTarget_MismatchedIdentityUsesCanonicalName: even without
// the delete path, the write target for a drifted physical record is the
// caller's canonical name, never the other account's file.
func TestResolveAuthFileTarget_MismatchedIdentityUsesCanonicalName(t *testing.T) {
	saA := delGuardAuth("uid-name-A", "codebuddy.cn", "tok-a")
	saB := delGuardAuth("uid-name-B", "codebuddy.cn", "tok-b")
	rawB, _ := json.Marshal(saB)
	name, _, _ := resolveAuthFileTarget(saA, &hostAuthPhysical{
		Name: "workbuddy-uid-name-B.json", Path: filepath.Join(t.TempDir(), "workbuddy-uid-name-B.json"), JSON: rawB,
	})
	if name != "workbuddy-uid-name-A.json" {
		t.Fatalf("write target = %q, want the caller's canonical name", name)
	}
}

var _ = pluginapi.HostAuthFileEntry{} // keep the import used if assertions change
