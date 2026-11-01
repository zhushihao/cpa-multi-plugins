package main

// cost_store_lifecycle_test.go — N2 second-audit fix (red → green) plus the
// reconfigure restore-merge fix.
//
// N2: configuring state_file: off only logged; the previously attached cost
// store stayed wired, so the pool kept writing the old path. Disabling
// persistence must detach the store.
//
// Minor: reconfiguring (attach → load) restored disk rows unconditionally,
// regressing an in-memory observation that was newer than the file's snapshot.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCostStateConfigOffDetachesStore: after the disable configuration the pool
// must not write to the old path again.
func TestCostStateConfigOffDetachesStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cost-ledger.json")
	t.Setenv(wbCostStateEnv, path)
	// Ensure the pool starts unattached for this test.
	wbApplyCostStateConfig("off")
	t.Cleanup(func() { wbApplyCostStateConfig("off") })

	// Attach through the same helper configure() uses.
	wbApplyCostStateConfig("")
	if wbRouterPool.getCostStore() == nil {
		t.Fatal("state file must be attached when enabled")
	}
	wbRouterPool.noteCost("wb-detach-auth", "m", 0.5, 1000)
	if err := wbRouterPool.persistCostsNow(); err != nil {
		t.Fatalf("initial persist: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	// Disable: the store must be detached.
	wbApplyCostStateConfig("off")
	if wbRouterPool.getCostStore() != nil {
		t.Fatal("state_file: off must detach the cost store (N2: stale store kept writing)")
	}
	// New observations must not reach the old path, even on a forced save.
	wbRouterPool.noteCost("wb-detach-auth", "m2", 9, 1000)
	_ = wbRouterPool.persistCostsNow()
	_ = wbRouterPool.maybePersistCosts()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file vanished: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("state file changed after state_file: off\nbefore=%s\nafter=%s", before, after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}

	// Re-enabling attaches a fresh store again (reload from disk).
	wbApplyCostStateConfig("")
	if wbRouterPool.getCostStore() == nil {
		t.Fatal("re-enabling must re-attach the store")
	}
}

// TestCostLedgerRestoreKeepsNewerInMemoryObservation: a reconfigure-time load
// must never regress an observation that is newer than the file snapshot.
func TestCostLedgerRestoreKeepsNewerInMemoryObservation(t *testing.T) {
	cur := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	ledger := newWbCostLedger(func() time.Time { return cur })

	// Live observation at t0.
	ledger.note("wb-auth", "m", 1.0, 1000) // per1k = 1.0
	newer, ok := ledger.lookup("wb-auth", "m")
	if !ok {
		t.Fatal("note must be visible")
	}

	// A disk row from an hour ago (different price) must not overwrite it.
	if ledger.restore("wb-auth", "m", wbCostObservation{CostPer1k: 9.0, LastSeen: cur.Add(-time.Hour), Samples: 3}, cur) {
		t.Fatal("older disk row reported as restored over a newer in-memory observation")
	}
	got, ok := ledger.lookup("wb-auth", "m")
	if !ok || got.CostPer1k != newer.CostPer1k || !got.LastSeen.Equal(newer.LastSeen) {
		t.Fatalf("in-memory observation was regressed: got %+v, want %+v", got, newer)
	}

	// Advance the clock: a disk row newer than the in-memory row (but still
	// within TTL) is the normal restart case and must win.
	cur = cur.Add(30 * time.Minute)
	fresher := wbCostObservation{CostPer1k: 2.5, LastSeen: cur.Add(-10 * time.Minute), Samples: 4}
	if !ledger.restore("wb-auth", "m", fresher, cur) {
		t.Fatal("newer disk row must restore")
	}
	got2, _ := ledger.lookup("wb-auth", "m")
	if got2.CostPer1k != fresher.CostPer1k {
		t.Fatalf("newer disk row not applied: %+v", got2)
	}
}
