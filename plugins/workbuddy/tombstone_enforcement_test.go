package main

// tombstone_enforcement_test.go — solution-architect finding (round 2):
//
// After a deliberate removal, the canonical file is deleted but the preserved
// legacy copy keeps its bytes (disabled=false). The host may still register it
// as a workbuddy identity, so the plugin's OWN paths must treat a tombstoned
// identity as removed: routing must not pick it, automatic tasks/travel/check-in
// must not act on it, and lifecycle reconcile must not retry its deletion.
// (The host routing a request directly to such a record is outside the plugin's
// control and is recorded as a residual boundary; G07 is not claimed.)

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// tombstonedLegacyEnv builds the post-deletion state: identity tombstoned, only
// the preserved legacy copy left in the host list.
func tombstonedLegacyEnv(t *testing.T) *tombFailEnv {
	t.Helper()
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	legacyName := "codebuddy-cn-" + e.uid + ".json"
	legacyPath := filepath.Join(e.authDir, legacyName)
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(e.canonicalPath)
	e.physName, e.physPath = legacyName, legacyPath
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID: "id-legacy-copy", AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, Type: "codebuddy-cn",
		}}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, JSON: legacyJSON}, nil
	}
	if err := wbRecordDeletionTombstone(e.sa, "workbuddy-"+e.uid+".json"); err != nil {
		t.Fatalf("record tombstone: %v", err)
	}
	resetAuthUIDCacheForTest(t)
	return e
}

func TestTombstonedIdentityIsNotRoutedByScheduler(t *testing.T) {
	tombstonedLegacyEnv(t)
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "wb-tomb-model",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "id-legacy-copy", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := parsePickResponse(t, raw); resp.Handled {
		t.Fatalf("a tombstoned identity must not be routed, got %+v", resp)
	}
}

func TestTombstonedIdentityIsNotAdvancedByTravel(t *testing.T) {
	tombstonedLegacyEnv(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "")
	resetTravelSlotStateForTest(t)

	prevTravel := tasksTravelOnceFn
	calls := 0
	tasksTravelOnceFn = func(*storedAuth, func(string, ...any)) { calls++ }
	t.Cleanup(func() { tasksTravelOnceFn = prevTravel })

	runAutoTravel()
	if calls != 0 {
		t.Fatalf("travel pass advanced a tombstoned account %d time(s), want 0", calls)
	}
}

func TestTombstonedIdentityIsNotCheckedIn(t *testing.T) {
	tombstonedLegacyEnv(t)
	withCheckinTasksConfig(t, true, false, false)

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()
	restoreBase := setBillingBase(srv.URL)
	defer restoreBase()

	runAutoCheckin()
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("check-in tick issued %d upstream request(s) for a tombstoned account, want 0", got)
	}
}

func TestTombstonedIdentityIsNotReconciledOrRetried(t *testing.T) {
	e := tombstonedLegacyEnv(t)
	// Check-in off, lifecycle on: the tick's only remaining work would be a
	// reconcile (which would retry the refused deletion).
	withCheckinTasksConfig(t, false, false, true)

	var persists int32
	prevPersist := hostAuthPersistMigrateFn
	hostAuthPersistMigrateFn = func(string, string, string, []byte) error {
		atomic.AddInt32(&persists, 1)
		return nil
	}
	t.Cleanup(func() { hostAuthPersistMigrateFn = prevPersist })

	runAutoCheckin()
	if got := atomic.LoadInt32(&persists); got != 0 {
		t.Fatalf("lifecycle retried a tombstoned account (%d write(s)), want 0", got)
	}
	_ = e
}

func TestTombstonedIdentityIsNotRunByBatchTasks(t *testing.T) {
	tombstonedLegacyEnv(t)
	withCheckinTasksConfig(t, true, true, false)

	prevBonus := tasksDailyBonusFn
	runs := 0
	tasksDailyBonusFn = func(*storedAuth) *tasksBonusResult { runs++; return &tasksBonusResult{} }
	t.Cleanup(func() { tasksDailyBonusFn = prevBonus })

	_ = handleTasksRun(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if runs != 0 {
		t.Fatalf("batch task run processed a tombstoned account %d time(s), want 0", runs)
	}
}

func TestTombstonedIdentityIsNotRunByBatchCheckin(t *testing.T) {
	tombstonedLegacyEnv(t)

	prev := checkinOneAccountFn
	calls := 0
	checkinOneAccountFn = func(f pluginapi.HostAuthFileEntry) map[string]any {
		calls++
		return map[string]any{"auth_index": f.AuthIndex, "success": true}
	}
	t.Cleanup(func() { checkinOneAccountFn = prev })

	_ = handleManualCheckin(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if calls != 0 {
		t.Fatalf("batch check-in processed a tombstoned account %d time(s), want 0", calls)
	}
}

// TestDeliberateReimportClearsTombstone: an explicit import/login must revive the
// account (otherwise the tombstone would permanently block a legitimate
// re-import now that routing filters tombstoned identities).
func TestDeliberateReimportClearsTombstone(t *testing.T) {
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	if err := wbRecordDeletionTombstone(e.sa, "workbuddy-"+e.uid+".json"); err != nil {
		t.Fatalf("record tombstone: %v", err)
	}
	if !wbIsAdoptTombstoned(e.sa) {
		t.Fatal("precondition: identity must be tombstoned")
	}
	if cleared, reason := wbClearTombstoneAfterVerifiedCanonicalWrite(e.sa); !cleared {
		t.Fatalf("deliberate re-import/login must clear the tombstone: %s", reason)
	}
	if wbIsAdoptTombstoned(e.sa) {
		t.Fatal("deliberate re-import/login must clear the tombstone")
	}
	// Restart: the cleared state must be durable.
	resetAdoptTombstonesForTest(t)
	if wbIsAdoptTombstoned(e.sa) {
		t.Fatal("cleared tombstone must survive a restart")
	}
}

// corruptStoreLegacyEnv: a preserved legacy copy is the only credential; the
// tombstone store is CORRUPT (e.g. a damaged file after a crash/restart).
func corruptStoreLegacyEnv(t *testing.T) *tombFailEnv {
	t.Helper()
	e := newTombFailEnv(t, filepath.Join(t.TempDir(), "adopt-tombstones.json"))
	legacyName := "codebuddy-cn-" + e.uid + ".json"
	legacyPath := filepath.Join(e.authDir, legacyName)
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(e.canonicalPath)
	e.physName, e.physPath = legacyName, legacyPath
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID: "id-legacy-copy", AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, Type: "codebuddy-cn",
		}}, nil
	}
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, JSON: legacyJSON}, nil
	}
	// Damage the store and force a reload (simulating a restart on a bad file).
	if err := os.WriteFile(e.tombPath(), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetAdoptTombstonesForTest(t)
	resetAuthUIDCacheForTest(t)
	return e
}

// TestCorruptStoreBlocksRoutingOfPreservedCopy: an untrusted store must fail
// CLOSED for routing — the preserved copy is not provably alive.
func TestCorruptStoreBlocksRoutingOfPreservedCopy(t *testing.T) {
	corruptStoreLegacyEnv(t)
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "wb-corrupt-store",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "id-legacy-copy", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := parsePickResponse(t, raw); resp.Handled {
		t.Fatalf("an untrusted tombstone store must not let routing fall through to a preserved copy, got %+v", resp)
	}
}

// TestCorruptStoreBlocksAutomaticPaths: travel, the scheduled tick, and the
// batch endpoints must all fail closed on an untrusted store.
func TestCorruptStoreBlocksAutomaticPaths(t *testing.T) {
	e := corruptStoreLegacyEnv(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "")
	resetTravelSlotStateForTest(t)

	prevTravel := tasksTravelOnceFn
	travelCalls := 0
	tasksTravelOnceFn = func(*storedAuth, func(string, ...any)) { travelCalls++ }
	t.Cleanup(func() { tasksTravelOnceFn = prevTravel })
	runAutoTravel()
	if travelCalls != 0 {
		t.Fatalf("travel advanced an untrusted-store account %d time(s), want 0", travelCalls)
	}

	withCheckinTasksConfig(t, true, false, false)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()
	restoreBase := setBillingBase(srv.URL)
	defer restoreBase()
	runAutoCheckin()
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("check-in tick issued %d request(s) on an untrusted store, want 0", got)
	}

	withCheckinTasksConfig(t, true, true, false)
	prevBonus := tasksDailyBonusFn
	runs := 0
	tasksDailyBonusFn = func(*storedAuth) *tasksBonusResult { runs++; return &tasksBonusResult{} }
	t.Cleanup(func() { tasksDailyBonusFn = prevBonus })
	_ = handleTasksRun(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if runs != 0 {
		t.Fatalf("batch task run processed an untrusted-store account %d time(s), want 0", runs)
	}
	_ = e
}

// TestDisabledStoreBlocksRoutingOfPreservedCopy: architect ruling (2026-09-29)
// — an explicitly disabled tombstone store LOSES the persistent record of
// removed identities, so it must fail closed exactly like a corrupt store. The
// scenario: the canonical file was deleted (identity tombstoned), the legacy
// copy was preserved, and the process restarted with
// WORKBUDDY_ADOPT_TOMBSTONE_FILE=off. SchedulerPick must not select the old
// copy, and the automatic paths must not act on it. Turning the store off is
// therefore an explicit choice that can render automatic WorkBuddy work
// unavailable.
func TestDisabledStoreBlocksRoutingOfPreservedCopy(t *testing.T) {
	e := newRevivalEnv(t) // identity tombstoned while the store was healthy
	legacyName := "codebuddy-cn-" + e.uid + ".json"
	legacyPath := filepath.Join(e.authDir, legacyName)
	legacyJSON, _ := json.Marshal(e.sa)
	if err := os.WriteFile(legacyPath, legacyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(e.authDir, e.canonicalName))
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			ID: "id-legacy-copy", AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, Type: "codebuddy-cn",
		}}, nil
	}
	hostAuthGetFn = func(string) (*storedAuth, error) { return e.sa, nil }
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: "idx-legacy-copy", Name: legacyName, Path: legacyPath, JSON: legacyJSON}, nil
	}
	// Restart with the store explicitly disabled: the persisted record is gone.
	t.Setenv(wbAdoptTombstoneEnv, "off")
	resetAdoptTombstonesForTest(t)
	resetAuthUIDCacheForTest(t)

	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider:   providerName,
		Model:      "wb-off-store",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "id-legacy-copy", Provider: providerName}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp := parsePickResponse(t, raw); resp.Handled {
		t.Fatalf("a disabled tombstone store must fail closed, not route the preserved copy, got %+v", resp)
	}

	// Automatic paths fail closed as well.
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "")
	resetTravelSlotStateForTest(t)
	prevTravel := tasksTravelOnceFn
	travelCalls := 0
	tasksTravelOnceFn = func(*storedAuth, func(string, ...any)) { travelCalls++ }
	t.Cleanup(func() { tasksTravelOnceFn = prevTravel })
	runAutoTravel()
	if travelCalls != 0 {
		t.Fatalf("travel advanced an off-store account %d time(s), want 0", travelCalls)
	}
	withCheckinTasksConfig(t, true, true, false)
	prevBonus := tasksDailyBonusFn
	runs := 0
	tasksDailyBonusFn = func(*storedAuth) *tasksBonusResult { runs++; return &tasksBonusResult{} }
	t.Cleanup(func() { tasksDailyBonusFn = prevBonus })
	_ = handleTasksRun(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if runs != 0 {
		t.Fatalf("batch task run processed an off-store account %d time(s), want 0", runs)
	}
	if usable, reason := wbAdoptTombstonesUsable(); usable {
		t.Fatalf("off store must be unusable (reason=%q)", reason)
	}
}
