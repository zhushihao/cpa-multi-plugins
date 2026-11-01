package main

// account_dedup_test.go — I5 review fix + second-audit scope (red → green).
//
// The non-destructive adoption policy preserves legacy files next to the
// canonical copy, so one credential can be listed twice. Both copies then act
// on the same upstream account: the scheduler candidate pool double-counts the
// account and each account pass (travel / check-in / task run) issues its
// upstream calls twice. These tests pin the in-memory dedup by (uid, realm) and
// that no auth file is ever written or deleted by it.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// stubHostAuthFiles installs a multi-entry host auth list/get seam.
func stubHostAuthFiles(t *testing.T, files []pluginapi.HostAuthFileEntry, byIndex map[string]*storedAuth) {
	t.Helper()
	prevList, prevGet := hostAuthListFn, hostAuthGetFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) { return files, nil }
	hostAuthGetFn = func(idx string) (*storedAuth, error) {
		sa, ok := byIndex[idx]
		if !ok {
			return nil, fmt.Errorf("no auth %s", idx)
		}
		return sa, nil
	}
	t.Cleanup(func() { hostAuthListFn, hostAuthGetFn = prevList, prevGet })
}

// countAuthWrites counts auth-file writes so a test can prove non-destructiveness.
func countAuthWrites(t *testing.T) *int32 {
	t.Helper()
	var writes int32
	prevPersist, prevSave := hostAuthPersistFn, hostAuthSaveJSONFn
	hostAuthPersistFn = func(string, string, []byte) error { atomic.AddInt32(&writes, 1); return nil }
	hostAuthSaveJSONFn = func(string, []byte) error { atomic.AddInt32(&writes, 1); return nil }
	t.Cleanup(func() { hostAuthPersistFn, hostAuthSaveJSONFn = prevPersist, prevSave })
	return &writes
}

// wbDedupFixture builds a legacy + canonical pair for one CN account.
func wbDedupFixture() (files []pluginapi.HostAuthFileEntry, byIndex map[string]*storedAuth) {
	legacySA := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok-legacy", RefreshToken: "ref-legacy", Domain: "codebuddy.cn"},
		Account: storedAccount{UID: "uid-dedup-1", Nickname: "one"},
	}
	canonicalSA := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok-canonical", RefreshToken: "ref-canonical", Domain: "codebuddy.cn"},
		Account: storedAccount{UID: "uid-dedup-1", Nickname: "one"},
	}
	files = []pluginapi.HostAuthFileEntry{
		{ID: "id-legacy", AuthIndex: "idx-legacy", Name: "workbuddy.json", Type: "workbuddy"},
		{ID: "id-canonical", AuthIndex: "idx-canonical", Name: "workbuddy-uid-dedup-1.json", Type: "workbuddy"},
	}
	byIndex = map[string]*storedAuth{"idx-legacy": legacySA, "idx-canonical": canonicalSA}
	return files, byIndex
}

// TestDedupCandidates_LegacyAndCanonicalCollapseToCanonical: one account, one
// candidate; the canonical uid-named file is the stable representative.
func TestDedupCandidates_LegacyAndCanonicalCollapseToCanonical(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)
	writes := countAuthWrites(t)

	cands := []pluginapi.SchedulerAuthCandidate{
		{ID: "id-legacy", Provider: providerName},
		{ID: "id-canonical", Provider: providerName},
	}
	got := wbDedupCandidatesByIdentity(cands)
	if len(got) != 1 {
		t.Fatalf("dedup kept %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].ID != "id-canonical" {
		t.Fatalf("representative = %q, want id-canonical (canonical uid file wins)", got[0].ID)
	}
	if w := atomic.LoadInt32(writes); w != 0 {
		t.Fatalf("dedup wrote %d auth file(s); must be non-destructive", w)
	}
}

// TestDedupCandidates_StableAcrossOrder: the representative does not depend on
// host list order.
func TestDedupCandidates_StableAcrossOrder(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)
	reversed := []pluginapi.SchedulerAuthCandidate{
		{ID: "id-canonical", Provider: providerName},
		{ID: "id-legacy", Provider: providerName},
	}
	got := wbDedupCandidatesByIdentity(reversed)
	if len(got) != 1 || got[0].ID != "id-canonical" {
		t.Fatalf("reversed-order dedup = %+v, want the canonical representative only", got)
	}
}

// TestDedupCandidates_DifferentRealmsNotMerged: the same human uid may hold a
// CN credential and an Intl credential; those are different accounts and must
// never be merged.
func TestDedupCandidates_DifferentRealmsNotMerged(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	cn := &storedAuth{Auth: storedTokens{AccessToken: "t1", Domain: "codebuddy.cn"}, Account: storedAccount{UID: "uid-both"}}
	intl := &storedAuth{Auth: storedTokens{AccessToken: "t2", Domain: "www.codebuddy.ai"}, Account: storedAccount{UID: "uid-both"}}
	files := []pluginapi.HostAuthFileEntry{
		{ID: "id-cn", AuthIndex: "idx-cn", Name: "workbuddy-uid-both.json", Type: "workbuddy"},
		{ID: "id-intl", AuthIndex: "idx-intl", Name: "workbuddy-intl-uid-both.json", Type: "workbuddy-intl"},
	}
	stubHostAuthFiles(t, files, map[string]*storedAuth{"idx-cn": cn, "idx-intl": intl})

	got := wbDedupCandidatesByIdentity([]pluginapi.SchedulerAuthCandidate{
		{ID: "id-cn", Provider: providerName},
		{ID: "id-intl", Provider: providerName},
	})
	if len(got) != 2 {
		t.Fatalf("CN+Intl same-uid candidates were merged into %d; realms are distinct accounts", len(got))
	}
}

// TestDedupCandidates_UnknownIdentityKept: fail-open — an unresolvable
// credential is never merged on a guess.
func TestDedupCandidates_UnknownIdentityKept(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	noUID := &storedAuth{Auth: storedTokens{AccessToken: "t", Domain: "codebuddy.cn"}}
	files := []pluginapi.HostAuthFileEntry{
		{ID: "id-a", AuthIndex: "idx-a", Name: "workbuddy.json", Type: "workbuddy"},
		{ID: "id-b", AuthIndex: "idx-b", Name: "workbuddy-other.json", Type: "workbuddy"},
	}
	stubHostAuthFiles(t, files, map[string]*storedAuth{"idx-a": noUID, "idx-b": noUID})
	got := wbDedupCandidatesByIdentity([]pluginapi.SchedulerAuthCandidate{
		{ID: "id-a", Provider: providerName},
		{ID: "id-b", Provider: providerName},
	})
	if len(got) != 2 {
		t.Fatalf("uid-less candidates were merged into %d, want 2 kept", len(got))
	}
}

// TestSchedulerPick_DedupedCandidatePool: through the SchedulerPick surface,
// the duplicate copy must never be returned as the routing decision.
func TestSchedulerPick_DedupedCandidatePool(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	setModelAliasesForTest(t, nil)
	restore := setSchedulerMode(schedulerModeWeighted)
	t.Cleanup(restore)
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "wb-dedup-model",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "id-legacy", Provider: providerName},
			{ID: "id-canonical", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "id-canonical" {
		t.Fatalf("pick = %+v, want the single canonical candidate", resp)
	}
}

// TestTravelPassDedupesLegacyAndCanonical: the independent travel pass advances
// the upstream account once per slot, not once per file.
func TestTravelPassDedupesLegacyAndCanonical(t *testing.T) {
	resetTravelSlotStateForTest(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "")
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)

	prevTravel := tasksTravelOnceFn
	calls := 0
	tasksTravelOnceFn = func(*storedAuth, func(string, ...any)) { calls++ }
	t.Cleanup(func() { tasksTravelOnceFn = prevTravel })

	runAutoTravel()
	if calls != 1 {
		t.Fatalf("travel pass advanced the account %d time(s), want exactly 1 (legacy+canonical double-count)", calls)
	}
}

// TestAutoCheckinTickDedupesLegacyAndCanonical: the scheduled tick performs one
// account's upstream check-in work, not two.
func TestAutoCheckinTickDedupesLegacyAndCanonical(t *testing.T) {
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)
	withCheckinTasksConfig(t, true, false, false)

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()
	restoreBase := setBillingBase(srv.URL)
	defer restoreBase()

	runAutoCheckin()
	// fetchCheckinStatus probes at most two paths for ONE account; the duplicate
	// copy must not add a second account's worth of calls.
	if got := atomic.LoadInt32(&requests); got == 0 || got > 2 {
		t.Fatalf("check-in tick issued %d upstream request(s), want 1..2 (one account)", got)
	}
}

// TestTasksRunBatchDedupesLegacyAndCanonical: a batch /tasks/run over a legacy
// + canonical pair runs the daily loop once for the account and reports the
// duplicate in the summary.
func TestTasksRunBatchDedupesLegacyAndCanonical(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)
	withCheckinTasksConfig(t, true, true, false)

	prevBonus := tasksDailyBonusFn
	runs := 0
	tasksDailyBonusFn = func(*storedAuth) *tasksBonusResult {
		runs++
		return &tasksBonusResult{Success: true}
	}
	t.Cleanup(func() { tasksDailyBonusFn = prevBonus })

	out := handleTasksRun(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if runs != 1 {
		t.Fatalf("daily loop ran %d time(s) for one account, want exactly 1", runs)
	}
	summary, _ := out["summary"].(map[string]any)
	if n, _ := summary["duplicate"].(int); n != 1 {
		t.Fatalf("summary duplicate = %v, want 1", summary["duplicate"])
	}
}

// TestManualCheckinBatchDedupesLegacyAndCanonical: the batch check-in endpoint
// fans out once for the account and counts the duplicate.
func TestManualCheckinBatchDedupesLegacyAndCanonical(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)

	prev := checkinOneAccountFn
	calls := 0
	checkinOneAccountFn = func(f pluginapi.HostAuthFileEntry) map[string]any {
		calls++
		return map[string]any{"auth_index": f.AuthIndex, "success": true}
	}
	t.Cleanup(func() { checkinOneAccountFn = prev })

	out := handleManualCheckin(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if calls != 1 {
		t.Fatalf("check-in ran %d time(s) for one account, want exactly 1", calls)
	}
	summary, _ := out["summary"].(map[string]any)
	if n, _ := summary["skipped_duplicate"].(int); n != 1 {
		t.Fatalf("summary skipped_duplicate = %v, want 1", summary["skipped_duplicate"])
	}
}

// TestManualCheckinSingleModeNotDeduped: an explicit auth_index names one
// credential; it must run even when it is the duplicate copy.
func TestManualCheckinSingleModeNotDeduped(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)

	prev := checkinOneAccountFn
	calls := 0
	checkinOneAccountFn = func(f pluginapi.HostAuthFileEntry) map[string]any {
		calls++
		return map[string]any{"auth_index": f.AuthIndex, "success": true}
	}
	t.Cleanup(func() { checkinOneAccountFn = prev })

	_ = handleManualCheckin(pluginapi.ManagementRequest{Body: []byte(`{"auth_index":"idx-legacy"}`)})
	if calls != 1 {
		t.Fatalf("single-account check-in ran %d time(s), want exactly 1", calls)
	}
}

// TestDedupedTargets_CanonicalRepresentativeRegardlessOfOrder: the representative
// must be the canonical uid-named copy even when the legacy source is listed
// first — the batch/lifecycle entries must not act on the preserved old file
// (round-2 review).
func TestDedupedTargets_CanonicalRepresentativeRegardlessOfOrder(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	files, byIndex := wbDedupFixture() // [legacy, canonical]
	stubHostAuthFiles(t, files, byIndex)
	// Make the legacy ID sort BEFORE the canonical ID so only the canonical-name
	// preference can pick the canonical copy (an ID tie-break must not be the
	// reason the test passes).
	legacy, canonical := files[0], files[1]
	legacy.ID, canonical.ID = "a-legacy", "z-canonical"

	kept, dup := wbDedupedTargets([]pluginapi.HostAuthFileEntry{legacy, canonical})
	if len(kept) != 1 || dup != 1 {
		t.Fatalf("kept=%d dup=%d, want 1/1", len(kept), dup)
	}
	if kept[0].ID != "z-canonical" {
		t.Fatalf("representative = %q, want z-canonical (legacy-first order must not win)", kept[0].ID)
	}
	// Reversed order must yield the same representative.
	keptRev, _ := wbDedupedTargets([]pluginapi.HostAuthFileEntry{canonical, legacy})
	if len(keptRev) != 1 || keptRev[0].ID != "z-canonical" {
		t.Fatalf("reversed-order representative = %+v, want z-canonical", keptRev)
	}
	// Non-destructive: the function never writes auth files.
	writes := countAuthWrites(t)
	if w := atomic.LoadInt32(writes); w != 0 {
		t.Fatalf("target dedup wrote %d auth file(s)", w)
	}
}

// TestManualCheckinBatchUsesCanonicalRepresentative: legacy-first list order must
// not route the batch entry onto the preserved source.
func TestManualCheckinBatchUsesCanonicalRepresentative(t *testing.T) {
	resetAuthUIDCacheForTest(t)
	files, byIndex := wbDedupFixture()
	stubHostAuthFiles(t, files, byIndex)

	prev := checkinOneAccountFn
	calls := 0
	gotIndex := ""
	checkinOneAccountFn = func(f pluginapi.HostAuthFileEntry) map[string]any {
		calls++
		gotIndex = f.AuthIndex
		return map[string]any{"auth_index": f.AuthIndex, "success": true}
	}
	t.Cleanup(func() { checkinOneAccountFn = prev })

	_ = handleManualCheckin(pluginapi.ManagementRequest{Body: []byte(`{}`)})
	if calls != 1 {
		t.Fatalf("check-in ran %d time(s), want 1", calls)
	}
	if gotIndex != "idx-canonical" {
		t.Fatalf("batch check-in used %q, want the canonical copy (idx-canonical)", gotIndex)
	}
}
