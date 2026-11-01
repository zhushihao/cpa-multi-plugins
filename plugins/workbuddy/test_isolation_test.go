package main

// test_isolation_test.go — per-test reset helpers for process-global state.
//
// Production behavior is correct: the model rate-limit registry and the
// lifecycle note dedupe are deliberately process-wide and survive across
// requests. But tests that use FIXED uid/model/authID keys then become
// order- and repetition-dependent: with `go test -count=N` (or a different
// test order) round 2 sees round 1's still-live window/dedupe entry and fails.
// Reproduced on pristine HEAD for four tests (see the work log); these helpers
// give them a fresh-process starting point without changing any product code.
//
// Usage: call at the start of the affected test.

import (
	"sync"
	"testing"
	"time"
)

// resetModelRateLimitsForTest empties the process-wide model rate-limit
// registry (wbModelRateLimits).
func resetModelRateLimitsForTest(t *testing.T) {
	t.Helper()
	wbModelRateLimits.mu.Lock()
	wbModelRateLimits.entries = map[string]time.Time{}
	wbModelRateLimits.mu.Unlock()
}

// resetLifecycleDedupeForTest clears the lifecycle note dedupe cache so a test
// re-running with the same authID observes the same "changed" state as on a
// fresh process.
func resetLifecycleDedupeForTest(t *testing.T) {
	t.Helper()
	lifecycleState.Range(func(key, _ any) bool {
		lifecycleState.Delete(key)
		return true
	})
}

// resetTravelSlotStateForTest forgets the in-memory "last travel slot ran"
// key so a scheduler-dispatch test starts from a fresh process state.
func resetTravelSlotStateForTest(t *testing.T) {
	t.Helper()
	wbTravelRunMu.Lock()
	wbTravelLastKey = ""
	wbTravelRunMu.Unlock()
}

// resetAuthUIDCacheForTest clears the auth-record→identity resolver cache and
// its scan throttle so each identity test resolves from its own host stub.
func resetAuthUIDCacheForTest(t *testing.T) {
	t.Helper()
	wbAuthIdentityMu.Lock()
	wbAuthIdentitiesByID = map[string]wbAuthIdentity{}
	wbAuthIdentityScanAt = time.Time{}
	wbAuthIdentityMu.Unlock()
}

// resetAdoptTombstonesForTest drops the lazy-loaded tombstone state so a test
// can point WORKBUDDY_ADOPT_TOMBSTONE_FILE at its own temp path (and simulate a
// process restart by re-loading from disk).
func resetAdoptTombstonesForTest(t *testing.T) {
	t.Helper()
	wbAdoptTombMu.Lock()
	wbAdoptTombRows = map[string]wbAdoptTombstoneRow{}
	wbAdoptTombLoaded = false
	wbAdoptTombEnvValue = ""
	wbAdoptTombPath = ""
	wbAdoptTombStoreErr = ""
	wbAdoptTombMu.Unlock()
}

// resetStartAdoptionForTest gives a test a fresh one-time-adoption gate.
func resetStartAdoptionForTest(t *testing.T) {
	t.Helper()
	startAdoptionOnce = sync.Once{}
}
