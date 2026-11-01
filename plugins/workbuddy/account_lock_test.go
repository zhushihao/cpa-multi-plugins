package main

// account_lock_test.go — I3 review fix (red → green): travel, manual
// check-in/tasks runs and the scheduled tick must serialize on ONE per-account
// lock (checkinLockFor). The travel pass previously used a private mutex map
// (wbTravelLockFor), so a travel step could race a manual check-in or task run
// against the same upstream account.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// stubHostAuthList installs a single-account host auth list/get seam and
// restores it on cleanup.
func stubHostAuthList(t *testing.T, id, authIndex, name string, sa *storedAuth) {
	t.Helper()
	prevList, prevGet := hostAuthListFn, hostAuthGetFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: id, AuthIndex: authIndex, Name: name}}, nil
	}
	hostAuthGetFn = func(string) (*storedAuth, error) {
		return sa, nil
	}
	t.Cleanup(func() { hostAuthListFn, hostAuthGetFn = prevList, prevGet })
}

// withCheckinTasksConfig pins checkin_auto/tasks_auto/lifecycle_auto for the
// test duration.
func withCheckinTasksConfig(t *testing.T, wantCheckinAuto, wantTasksAuto, wantLifecycleAuto bool) {
	t.Helper()
	checkinAutoMu.Lock()
	prevCheckin := checkinAuto
	checkinAuto = wantCheckinAuto
	checkinAutoMu.Unlock()
	tasksAutoMu.Lock()
	prevTasks := tasksAuto
	tasksAuto = wantTasksAuto
	tasksAutoMu.Unlock()
	lifecycleAutoMu.Lock()
	prevLifecycle := lifecycleAuto
	lifecycleAuto = wantLifecycleAuto
	lifecycleAutoMu.Unlock()
	t.Cleanup(func() {
		checkinAutoMu.Lock()
		checkinAuto = prevCheckin
		checkinAutoMu.Unlock()
		tasksAutoMu.Lock()
		tasksAuto = prevTasks
		tasksAutoMu.Unlock()
		lifecycleAutoMu.Lock()
		lifecycleAuto = prevLifecycle
		lifecycleAutoMu.Unlock()
	})
}

func cnLockTestAuth(uid string) *storedAuth {
	return &storedAuth{
		Auth:    storedTokens{AccessToken: "tok-" + uid, RefreshToken: "ref-" + uid, Domain: "codebuddy.cn"},
		Account: storedAccount{UID: uid, Nickname: uid},
	}
}

// TestAccountLock_IdentityKeyedAcrossCopies: the unified per-account lock must be
// keyed by the account identity (uid × realm), not by auth_index — legacy and
// canonical copies of one credential have different indexes, so an index-keyed
// lock lets two passes (manual check-in vs auto travel/tick) hit the same
// upstream account concurrently (round-2 review).
func TestAccountLock_IdentityKeyedAcrossCopies(t *testing.T) {
	sa := cnLockTestAuth("uid-lock-identity")
	muLegacy := wbAccountLockFor("idx-copy-legacy", sa)
	muCanonical := wbAccountLockFor("idx-copy-canonical", sa)
	muLegacy.Lock()
	defer muLegacy.Unlock()
	if muCanonical.TryLock() {
		muCanonical.Unlock()
		t.Fatal("legacy and canonical copies of one account must share ONE lock")
	}
	// A different account still gets its own lock.
	other := wbAccountLockFor("idx-other", cnLockTestAuth("uid-lock-other"))
	if !other.TryLock() {
		t.Fatal("a different account must not be blocked by this account's lock")
	}
	other.Unlock()
}

// TestTravelPassBlocksBehindCanonicalCopyLock is the cross-pass counter-example:
// a pass holding the lock through the LEGACY copy must block the travel pass
// running on the canonical copy of the same account.
func TestTravelPassBlocksBehindCanonicalCopyLock(t *testing.T) {
	resetTravelSlotStateForTest(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "")

	files, byIndex := wbDedupFixture() // host list: legacy first, canonical second
	stubHostAuthFiles(t, files, byIndex)
	sa := byIndex["idx-canonical"]

	entered := make(chan struct{})
	var once sync.Once
	prevTravel := tasksTravelOnceFn
	tasksTravelOnceFn = func(*storedAuth, func(string, ...any)) { once.Do(func() { close(entered) }) }
	t.Cleanup(func() { tasksTravelOnceFn = prevTravel })

	// Simulate a manual check-in that locked the account through the legacy copy.
	muLegacy := wbAccountLockFor("idx-legacy", sa)
	muLegacy.Lock()
	done := make(chan struct{})
	go func() {
		runAutoTravel()
		close(done)
	}()
	select {
	case <-entered:
		muLegacy.Unlock()
		t.Fatal("travel pass ran while the same account was locked through its legacy copy")
	case <-time.After(150 * time.Millisecond):
	}
	muLegacy.Unlock()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("travel did not run after the account lock was released")
	}
	<-done
}

// TestPruneCheckinLocksKeepsAllLocks: pruning is deliberately DISABLED this
// round — any reclamation scheme here is racy (a lock freed and then deleted can
// be re-acquired in the window, and the next pass would create a parallel
// mutex). The table keeps one mutex per identity/record for the process
// lifetime; growth is bounded by the auth set. This test pins that no lock is
// ever dropped.
func TestPruneCheckinLocksKeepsAllLocks(t *testing.T) {
	sa := cnLockTestAuth("uid-prune-keep")
	stubHostAuthFiles(t, []pluginapi.HostAuthFileEntry{
		{ID: "id-live", AuthIndex: "idx-live", Name: "workbuddy-uid-prune-keep.json", Type: "workbuddy"},
	}, map[string]*storedAuth{"idx-live": sa})
	resetAuthUIDCacheForTest(t)

	identityLock := wbAccountLockFor("idx-live", sa)
	identityLock.Lock()
	defer identityLock.Unlock()
	staleHeld := wbAccountLockFor("idx-vanished-held", nil)
	staleHeld.Lock()
	defer staleHeld.Unlock()
	staleFree := wbAccountLockFor("idx-vanished-free", nil)

	pruneCheckinLocks()

	if wbAccountLockFor("idx-live", sa) != identityLock {
		t.Fatal("prune dropped a live identity-keyed lock")
	}
	if wbAccountLockFor("idx-vanished-held", nil) != staleHeld {
		t.Fatal("prune dropped a held index-fallback lock")
	}
	if wbAccountLockFor("idx-vanished-free", nil) != staleFree {
		t.Fatal("prune must not drop any lock (reclamation is disabled as racy)")
	}
}

// TestTravelPassHoldsUnifiedAccountLock: while the travel pass advances an
// account, the unified per-account lock (checkinLockFor) must be held, so a
// manual check-in/tasks run cannot interleave. Before the fix the travel pass
// held a private lock and TryLock here succeeded.
func TestTravelPassHoldsUnifiedAccountLock(t *testing.T) {
	resetTravelSlotStateForTest(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "")

	const authIndex = "idx-unified-lock-1"
	stubHostAuthList(t, "id-unified-lock-1", authIndex, "workbuddy-uid-lock-1.json", cnLockTestAuth("uid-lock-1"))

	prevTravel := tasksTravelOnceFn
	lockWasFree := false
	tasksTravelOnceFn = func(sa *storedAuth, _ func(string, ...any)) {
		mu := wbAccountLockFor(authIndex, sa)
		if mu.TryLock() {
			mu.Unlock()
			lockWasFree = true
		}
	}
	t.Cleanup(func() { tasksTravelOnceFn = prevTravel })

	runAutoTravel()
	if lockWasFree {
		t.Fatal("travel pass must hold the unified per-account lock (checkinLockFor) while advancing the account")
	}
}

// TestTravelPassBlocksBehindManualAccountLock: a manual check-in holding the
// per-account lock must block the travel pass for the same account until it is
// released (real mutual exclusion, not just a shared map).
func TestTravelPassBlocksBehindManualAccountLock(t *testing.T) {
	resetTravelSlotStateForTest(t)
	prevAuto, prevRaw := wbTravelConfig()
	t.Cleanup(func() { setWbTravelConfig(prevAuto, prevRaw) })
	setWbTravelConfig(true, "")

	const authIndex = "idx-unified-lock-2"
	stubHostAuthList(t, "id-unified-lock-2", authIndex, "workbuddy-uid-lock-2.json", cnLockTestAuth("uid-lock-2"))

	entered := make(chan struct{})
	var once sync.Once
	prevTravel := tasksTravelOnceFn
	tasksTravelOnceFn = func(*storedAuth, func(string, ...any)) { once.Do(func() { close(entered) }) }
	t.Cleanup(func() { tasksTravelOnceFn = prevTravel })

	mu := wbAccountLockFor(authIndex, cnLockTestAuth("uid-lock-2"))
	mu.Lock()
	done := make(chan struct{})
	go func() {
		runAutoTravel()
		close(done)
	}()
	select {
	case <-entered:
		mu.Unlock()
		t.Fatal("travel ran while a manual check-in held the per-account lock")
	case <-time.After(150 * time.Millisecond):
		// Still blocked — expected.
	}
	mu.Unlock()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("travel did not run after the per-account lock was released")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("travel pass did not finish")
	}
}

// TestAutoCheckinTickHoldsUnifiedAccountLock: the scheduled tick must hold the
// per-account lock while it does upstream work, so it cannot race a manual
// check-in or the travel pass for the same account.
func TestAutoCheckinTickHoldsUnifiedAccountLock(t *testing.T) {
	const authIndex = "idx-unified-lock-3"
	stubHostAuthList(t, "id-unified-lock-3", authIndex, "workbuddy-uid-lock-3.json", cnLockTestAuth("uid-lock-3"))
	withCheckinTasksConfig(t, true, false, false) // check-in only: no tasks/reconcile noise

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()
	restoreBase := setBillingBase(srv.URL)
	defer restoreBase()

	f := pluginapi.HostAuthFileEntry{ID: "id-unified-lock-3", AuthIndex: authIndex, Name: "workbuddy-uid-lock-3.json"}
	done := make(chan struct{})
	go func() {
		processAutoCheckinAccount(f, true, &wbAccountPassGuard{})
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("auto check-in tick never reached the upstream call")
	}
	mu := wbAccountLockFor(authIndex, cnLockTestAuth("uid-lock-3"))
	if mu.TryLock() {
		mu.Unlock()
		close(release)
		<-done
		t.Fatal("auto check-in tick must hold the per-account lock while working upstream")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("auto check-in tick did not finish after upstream released")
	}
}
