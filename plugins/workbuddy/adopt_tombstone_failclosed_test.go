package main

// adopt_tombstone_failclosed_test.go — solution-architect strengthening
// (2026-09-29, round 2):
//
// The tombstone must be durably recorded (atomic write + read-back) BEFORE the
// credential file is deleted; any failure — persistence off, corrupt or
// future-schema file, permission/path/write failure, read-back mismatch —
// fails CLOSED: no deletion (the credential is disabled instead, so it leaves
// routing without becoming resurrectable).
//
// Adoption and record+delete share one mutex so a concurrent adoption cannot
// re-create a canonical file the delete just removed. Automatic adoption is a
// one-time controlled migration (no background rescan).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type tombFailEnv struct {
	authDir       string
	stateDir      string
	uid           string
	canonicalName string
	canonicalPath string
	sa            *storedAuth
	physName      string
	physPath      string
	listCalls     int32
	persistCalls  int32
	lastPersist   []byte
}

// newTombFailEnv builds one CN credential whose physical file lives in a real
// temp dir, and installs the host seams. tombEnvValue is written to
// WORKBUDDY_ADOPT_TOMBSTONE_FILE.
func newTombFailEnv(t *testing.T, tombEnvValue string) *tombFailEnv {
	t.Helper()
	e := &tombFailEnv{
		authDir:  t.TempDir(),
		stateDir: t.TempDir(),
		uid:      "uid-failclosed",
	}
	e.sa = &storedAuth{
		Auth:    storedTokens{AccessToken: "tok-fc", RefreshToken: "ref-fc", Domain: "codebuddy.cn"},
		Account: storedAccount{UID: e.uid, Nickname: "fc"},
	}
	e.canonicalName = "workbuddy-" + e.uid + ".json"
	e.canonicalPath = filepath.Join(e.authDir, e.canonicalName)
	raw, err := json.Marshal(e.sa)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.canonicalPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	e.physName, e.physPath = e.canonicalName, e.canonicalPath

	t.Setenv(wbAdoptTombstoneEnv, tombEnvValue)
	resetAdoptTombstonesForTest(t)
	resetLifecycleDedupeForTest(t)

	prevList, prevGetPhys, prevPersist := hostAuthListFn, hostAuthGetPhysicalFn, hostAuthPersistMigrateFn
	prevGet := hostAuthGetFn
	prevBundle := hostAuthGetBundleFn
	hostAuthGetFn = func(string) (*storedAuth, error) { return e.sa, nil }
	hostAuthGetBundleFn = func(string) (*storedAuth, *hostAuthPhysical, error) {
		raw, _ := json.Marshal(e.sa)
		return e.sa, &hostAuthPhysical{AuthIndex: "idx-fc", Name: e.physName, Path: e.physPath, JSON: raw}, nil
	}
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		atomic.AddInt32(&e.listCalls, 1)
		return []pluginapi.HostAuthFileEntry{{
			ID: "id-fc", AuthIndex: "idx-fc", Name: e.physName, Path: e.physPath, Type: "workbuddy",
		}}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		raw, _ := json.Marshal(e.sa)
		return &hostAuthPhysical{AuthIndex: "idx-fc", Name: e.physName, Path: e.physPath, JSON: raw}, nil
	}
	hostAuthPersistMigrateFn = func(name, path, legacyPath string, raw []byte) error {
		atomic.AddInt32(&e.persistCalls, 1)
		e.lastPersist = append([]byte(nil), raw...)
		return nil
	}
	t.Cleanup(func() {
		hostAuthListFn, hostAuthGetPhysicalFn, hostAuthPersistMigrateFn = prevList, prevGetPhys, prevPersist
		hostAuthGetFn = prevGet
		hostAuthGetBundleFn = prevBundle
	})
	return e
}

func (e *tombFailEnv) tombPath() string {
	v := strings.TrimSpace(os.Getenv(wbAdoptTombstoneEnv))
	if v == "" || strings.EqualFold(v, "off") {
		return ""
	}
	return v
}

func (e *tombFailEnv) canonicalExists() bool {
	_, err := os.Stat(e.canonicalPath)
	return err == nil
}

// assertFailClosed: the credential file is still there and was disabled
// (not deleted without a durable tombstone).
func (e *tombFailEnv) assertFailClosed(t *testing.T) {
	t.Helper()
	if !e.canonicalExists() {
		t.Fatal("credential was deleted without a durable tombstone (fail-open)")
	}
	if e.persistCalls == 0 {
		t.Fatal("fail-closed path must disable the credential (no persist recorded)")
	}
	var doc struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.Unmarshal(e.lastPersist, &doc); err != nil {
		t.Fatalf("last persisted doc unparsable: %v", err)
	}
	if !doc.Disabled {
		t.Fatalf("credential must be disabled when deletion is refused, got %s", e.lastPersist)
	}
}

// TestTombstoneOffFailsClosedBeforeDelete: persistence disabled ⇒ no deletion.
func TestTombstoneOffFailsClosedBeforeDelete(t *testing.T) {
	e := newTombFailEnv(t, "off")
	if err := deleteAuth("idx-fc", "id-fc", e.sa); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	e.assertFailClosed(t)
}

// TestTombstoneCorruptFailsClosedBeforeDelete: an unreadable tombstone file
// must not be overwritten (that would drop previously recorded removals).
func TestTombstoneCorruptFailsClosedBeforeDelete(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	if err := os.WriteFile(e.tombPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteAuth("idx-fc", "id-fc", e.sa); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	e.assertFailClosed(t)
	if raw, err := os.ReadFile(e.tombPath()); err != nil || string(raw) != "{not json" {
		t.Fatalf("corrupt tombstone file was modified (err=%v, content=%q)", err, raw)
	}
}

// TestTombstoneFutureSchemaFailsClosedBeforeDelete: never overwrite a future
// schema.
func TestTombstoneFutureSchemaFailsClosedBeforeDelete(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	future := []byte(`{"schema_version":99,"tombstones":[]}`)
	if err := os.WriteFile(e.tombPath(), future, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteAuth("idx-fc", "id-fc", e.sa); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	e.assertFailClosed(t)
	if raw, _ := os.ReadFile(e.tombPath()); string(raw) != string(future) {
		t.Fatalf("future-schema tombstone file was modified: %s", raw)
	}
}

// TestTombstoneWriteFailureFailsClosedBeforeDelete: the path's parent is a
// regular file, so the atomic write cannot succeed.
func TestTombstoneWriteFailureFailsClosedBeforeDelete(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newTombFailEnv(t, filepath.Join(blocker, "adopt-tombstones.json"))
	if err := deleteAuth("idx-fc", "id-fc", e.sa); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	e.assertFailClosed(t)
}

// TestTombstoneReadBackMismatchFailsClosedBeforeDelete: the file is written but
// the read-back cannot prove the row (test hook removes it in between).
func TestTombstoneReadBackMismatchFailsClosedBeforeDelete(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	prevHook := wbAdoptTombstoneAfterWriteHook
	wbAdoptTombstoneAfterWriteHook = func() { _ = os.Remove(e.tombPath()) }
	t.Cleanup(func() { wbAdoptTombstoneAfterWriteHook = prevHook })

	if err := deleteAuth("idx-fc", "id-fc", e.sa); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	e.assertFailClosed(t)
	if wbIsAdoptTombstoned(e.sa) {
		t.Fatal("failed record must be reverted from the in-memory set")
	}
}

// TestTombstoneVerifiedRecordThenDelete: the happy path deletes only after the
// tombstone is durable and verified; the record survives a restart and blocks
// re-adoption of a preserved legacy source.
func TestTombstoneVerifiedRecordThenDelete(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "state", "adopt-tombstones.json"))
	if err := deleteAuth("idx-fc", "id-fc", e.sa); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	if e.canonicalExists() {
		t.Fatal("verified tombstone should allow the deletion")
	}
	raw, err := os.ReadFile(e.tombPath())
	if err != nil {
		t.Fatalf("tombstone file missing: %v", err)
	}
	if !containsAll(string(raw), e.uid, "cn", e.canonicalName) {
		t.Fatalf("tombstone row incomplete: %s", raw)
	}
	// Restart: reload from disk, identity stays tombstoned.
	resetAdoptTombstonesForTest(t)
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("tombstone must survive a restart")
	}
	// A preserved legacy source for the same account is not re-adopted.
	legacyDir := t.TempDir()
	legacyName := "codebuddy-cn-" + e.uid + ".json"
	legacyPath := filepath.Join(legacyDir, legacyName)
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "id-legacy", AuthIndex: "idx-legacy", Name: legacyName, Path: legacyPath, Type: "codebuddy-cn"}}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-legacy", Name: legacyName, Path: legacyPath, JSON: legacyJSON}, nil
	}
	var saves int32
	prevPersist := hostAuthPersistFn
	hostAuthPersistFn = func(string, string, []byte) error { atomic.AddInt32(&saves, 1); return nil }
	t.Cleanup(func() { hostAuthPersistFn = prevPersist })
	adoptForeignAuths()
	if got := atomic.LoadInt32(&saves); got != 0 {
		t.Fatalf("tombstoned account was re-adopted (%d save(s))", got)
	}
	if raw, err := os.ReadFile(legacyPath); err != nil || string(raw) != string(legacyJSON) {
		t.Fatalf("legacy source was modified (err=%v)", err)
	}
}

// TestDeleteAuth_RefusesLegacyNamedTarget: a legacy-named physical target
// (workbuddy.json / codebuddy-cn-* / codebuddy-intl-*) is preserved material and
// is never deleted — the delete refuses and records nothing.
func TestDeleteAuth_RefusesLegacyNamedTarget(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	// Physical target is the legacy single-file name (same credential bytes).
	e.physName = "workbuddy.json"
	e.physPath = filepath.Join(e.authDir, "workbuddy.json")
	raw, _ := json.Marshal(e.sa)
	if err := os.WriteFile(e.physPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteAuth("idx-fc", "id-fc", e.sa); err == nil {
		t.Fatal("deleteAuth must refuse a preserved legacy-named target")
	}
	if got, err := os.ReadFile(e.physPath); err != nil || string(got) != string(raw) {
		t.Fatalf("legacy-named credential was modified/deleted (err=%v)", err)
	}
	if wbIsAdoptTombstoned(e.sa) {
		t.Fatal("no tombstone may be recorded when the delete was refused")
	}
}

// TestAdoptRefusesWhenTombstoneStoreUnusable: adoption itself is fail-closed —
// without a trustworthy tombstone store it must not create canonical files.
func TestAdoptRefusesWhenTombstoneStoreUnusable(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	if err := os.WriteFile(e.tombPath(), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Legacy source present; canonical absent (simulating a removed account).
	legacyName := "codebuddy-cn-" + e.uid + ".json"
	legacyPath := filepath.Join(t.TempDir(), legacyName)
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "id-legacy", AuthIndex: "idx-legacy", Name: legacyName, Path: legacyPath, Type: "codebuddy-cn"}}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-legacy", Name: legacyName, Path: legacyPath, JSON: legacyJSON}, nil
	}
	var saves int32
	prevPersist := hostAuthPersistFn
	hostAuthPersistFn = func(string, string, []byte) error { atomic.AddInt32(&saves, 1); return nil }
	t.Cleanup(func() { hostAuthPersistFn = prevPersist })

	adoptForeignAuths()
	if got := atomic.LoadInt32(&saves); got != 0 {
		t.Fatalf("adoption ran with an unusable tombstone store (%d save(s))", got)
	}
	if raw, err := os.ReadFile(legacyPath); err != nil || string(raw) != string(legacyJSON) {
		t.Fatalf("legacy source was modified (err=%v)", err)
	}
}

// TestAdoptAndDeleteAreSerialized: while an adoption holds the shared
// credential-mutation mutex, a delete must wait (no adopt-after-delete
// resurrection window).
func TestAdoptAndDeleteAreSerialized(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	// The legacy SOURCE is what adoption reads; the delete target is the
	// canonical file (legacy-named targets are refused by design).
	legacyName := "codebuddy-cn-" + e.uid + ".json"
	legacyPath := filepath.Join(e.authDir, legacyName)
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(e.canonicalPath) // canonical absent → adoption will re-create it
	e.physName, e.physPath = e.canonicalName, e.canonicalPath
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "id-fc", AuthIndex: "idx-legacy", Name: legacyName, Path: legacyPath, Type: "codebuddy-cn"}}, nil
	}
	prevGetPhys := hostAuthGetPhysicalFn
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		if authIndex == "idx-legacy" {
			return &hostAuthPhysical{AuthIndex: "idx-legacy", Name: legacyName, Path: legacyPath, JSON: legacyJSON}, nil
		}
		return &hostAuthPhysical{AuthIndex: "idx-fc", Name: e.canonicalName, Path: e.canonicalPath, JSON: legacyJSON}, nil
	}
	t.Cleanup(func() { hostAuthGetPhysicalFn = prevGetPhys })

	entered := make(chan struct{})
	release := make(chan struct{})
	prevPersist := hostAuthPersistFn
	hostAuthPersistFn = func(string, string, []byte) error {
		close(entered)
		<-release
		return nil
	}
	t.Cleanup(func() { hostAuthPersistFn = prevPersist })

	adoptDone := make(chan struct{})
	go func() { adoptForeignAuths(); close(adoptDone) }()
	<-entered

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- deleteAuth("idx-fc", "id-fc", e.sa) }()
	select {
	case err := <-deleteDone:
		close(release)
		<-adoptDone
		t.Fatalf("deleteAuth ran while an adoption held the mutation mutex (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
		// Still blocked — expected.
	}
	close(release)
	<-adoptDone
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("deleteAuth after adoption: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deleteAuth did not proceed after the adoption finished")
	}
	if _, err := os.Stat(e.tombPath()); err != nil {
		t.Fatalf("delete did not record a tombstone: %v", err)
	}
}

// TestStartAdoptionIsOneTimeControlledMigration: repeated configure/register
// calls must not re-scan for legacy credentials; a new legacy file appearing
// later is only picked up after a process restart.
func TestStartAdoptionIsOneTimeControlledMigration(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	resetStartAdoptionForTest(t)
	os.Remove(e.canonicalPath) // adoption has work to do
	e.physName, e.physPath = "codebuddy-cn-"+e.uid+".json", filepath.Join(e.authDir, "codebuddy-cn-"+e.uid+".json")
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(e.physPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	var saves int32
	prevPersist := hostAuthPersistFn
	hostAuthPersistFn = func(string, string, []byte) error { atomic.AddInt32(&saves, 1); return nil }
	t.Cleanup(func() { hostAuthPersistFn = prevPersist })

	startAdoption()
	waitFor(t, func() bool { return atomic.LoadInt32(&saves) == 1 }, "first adoption")
	startAdoption()
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&saves); got != 1 {
		t.Fatalf("adoption ran %d time(s), want 1 (one-time controlled migration)", got)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
