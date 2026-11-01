package main

// cost_persist_concurrency_test.go — review-driven tests for the state-file
// writer (user review 2026-09-29):
//
//  1. dirty-flag lost update: a noteCost (markDirty) landing after the snapshot
//     but before the post-write clear must keep the ledger dirty, otherwise the
//     janitor never flushes that observation;
//  2. temp-path collision: concurrent saves with the same clock reading must
//     not share a temp file (the old pid+UnixNano naming collides on Windows'
//     coarse clock);
//  3. explicit Windows rename-over-existing coverage (Go's os.Rename replaces
//     an existing destination via MoveFileEx(MOVEFILE_REPLACE_EXISTING)).
//
// The blocking hook (store.afterSnapshotHook) is read under the store lock and
// invoked with NO store lock held, so a concurrent noteCost can proceed while
// a write is parked — that is exactly the interleaving under review.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCostStore_ConcurrentNoteDuringBlockedWrite pins the dirty-flag contract:
// an observation that arrives while a write is in flight (after its snapshot)
// must survive to the next flush.
func TestCostStore_ConcurrentNoteDuringBlockedWrite(t *testing.T) {
	dir := t.TempDir()
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{
		Now:                 func() time.Time { return clk.now() },
		RandInt64N:          wbSeqRand(41),
		CostExploreInterval: 30 * time.Minute,
		MinPickGap:          0,
	})
	store := newWbCostStore(filepath.Join(dir, wbCostStateFileName), time.Hour)
	store.now = func() time.Time { return clk.now() }
	if _, err := pool.attachCostStore(store); err != nil {
		t.Fatal(err)
	}

	// Seed observation A and persist it (also reserves the throttle window).
	pool.noteCost("uid-a", "m1", 1.0, 1000)
	if err := pool.persistCostsNow(); err != nil {
		t.Fatalf("seed persist: %v", err)
	}

	// Park the next write after its snapshot, then land observation B.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.afterSnapshotHook = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	writeErr := make(chan error, 1)
	go func() { writeErr <- pool.persistCostsNow() }()
	<-entered
	// B is written to the in-memory ledger and marks the store dirty; its own
	// maybeSave is inside the throttle window (lastSave was reserved by the
	// parked write), so it cannot sneak in.
	pool.noteCost("uid-b", "m1", 2.0, 1000)
	close(release)
	if err := <-writeErr; err != nil {
		t.Fatalf("parked write: %v", err)
	}
	store.afterSnapshotHook = nil

	// The parked write's snapshot only contained A. Because B landed after the
	// snapshot, the store must still be dirty so a later flush picks B up.
	clk.advance(2 * time.Hour) // past the throttle window
	if err := pool.maybePersistCosts(); err != nil {
		t.Fatalf("follow-up maybeSave: %v", err)
	}
	doc := readCostStateDoc(t, store.path)
	if _, ok := doc.Accounts["uid-b"]; !ok {
		t.Fatalf("observation landing during a blocked write was lost (dirty flag cleared too early): %v", doc.Accounts)
	}
	if _, ok := doc.Accounts["uid-a"]; !ok {
		t.Fatalf("seed observation missing after follow-up flush: %v", doc.Accounts)
	}
	assertNoTempLitter(t, dir)
}

// TestCostStore_ConcurrentForcedSavesFrozenClock: with the clock frozen, every
// writer reads the same timestamp. Saves must still each succeed (serialized
// writers + unique temp names), leave no temp litter, and produce a valid doc
// that contains every observation.
func TestCostStore_ConcurrentForcedSavesFrozenClock(t *testing.T) {
	dir := t.TempDir()
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{
		Now:                 func() time.Time { return clk.now() },
		RandInt64N:          wbSeqRand(42),
		CostExploreInterval: 30 * time.Minute,
		MinPickGap:          0,
	})
	store := newWbCostStore(filepath.Join(dir, wbCostStateFileName), 0)
	store.now = func() time.Time { return clk.now() } // frozen: identical timestamps
	if _, err := pool.attachCostStore(store); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		pool.noteCost(uidForIndex(i), "m1", float64(i), 1000)
	}
	// Hold every writer inside the write for a moment so the old shared
	// pid+nanosecond temp name would collide.
	store.afterSnapshotHook = func() { time.Sleep(120 * time.Millisecond) }

	const writers = 8
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = pool.persistCostsNow()
		}()
	}
	wg.Wait()
	store.afterSnapshotHook = nil

	for i, err := range errs {
		if err != nil {
			t.Fatalf("save %d failed (temp-name collision or interleaved rename?): %v", i, err)
		}
	}
	doc := readCostStateDoc(t, store.path)
	for i := 0; i < 8; i++ {
		if _, ok := doc.Accounts[uidForIndex(i)]; !ok {
			t.Fatalf("account %s missing after concurrent saves: %v", uidForIndex(i), doc.Accounts)
		}
	}
	assertNoTempLitter(t, dir)
}

// TestCostStore_ReplaceExistingFile: explicit Windows rename-over coverage.
// The replacement must be complete (no trailing bytes from the longer previous
// document) and repeatable.
func TestCostStore_ReplaceExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wbCostStateFileName)
	// A deliberately longer previous document with recognizable junk.
	longOld := strings.Repeat(`{"schema_version":1,"accounts":{}},`, 200)
	if err := os.WriteFile(path, []byte(longOld), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{Now: func() time.Time { return clk.now() }, RandInt64N: wbSeqRand(43), MinPickGap: 0})
	store := newWbCostStore(path, 0)
	store.now = func() time.Time { return clk.now() }
	pool.attachCostStore(store)
	pool.noteCost("uid-r", "m1", 0, 500)

	for run := 0; run < 2; run++ {
		if err := pool.persistCostsNow(); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "schema_version\":1,\"accounts\":{}") {
			t.Fatalf("run %d: previous content survived the rename: %s", run, raw)
		}
		var doc wbCostStateDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("run %d: replaced file is not valid JSON: %v (%s)", run, err, raw)
		}
		if len(doc.Accounts) != 1 || doc.SchemaVersion != wbCostStateSchemaVersion {
			t.Fatalf("run %d: doc = %+v", run, doc)
		}
		assertNoTempLitter(t, dir)
	}
}

// TestCostStore_FailedSaveReservesRetryWindow: a failed save still advances the
// throttle so a full disk is not hammered; the next window retries with the
// data intact.
func TestCostStore_FailedSaveReservesRetryWindow(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{Now: func() time.Time { return clk.now() }, RandInt64N: wbSeqRand(44), MinPickGap: 0})
	store := newWbCostStore(filepath.Join(blocker, wbCostStateFileName), time.Hour)
	store.now = func() time.Time { return clk.now() }
	pool.attachCostStore(store)
	pool.noteCost("uid-f", "m1", 1, 1000)
	if err := pool.persistCostsNow(); err == nil {
		t.Fatal("expected the unwritable target to fail")
	}
	// Inside the reserve window the janitor does not retry...
	if err := pool.maybePersistCosts(); err != nil {
		t.Fatalf("throttled retry should be a no-op: %v", err)
	}
	// ...and the knowledge is still pending after the window (dirty kept).
	if !store.dirtySnapshot() {
		t.Fatal("failed save must keep the store dirty for a later retry")
	}
}

// dirtySnapshot reads the dirty flag under the store lock (test helper).
func (s *wbCostStore) dirtySnapshot() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

func assertNoTempLitter(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}
