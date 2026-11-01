package main

// cost_persist_test.go — architecture ruling (2026-09-29): G07/G09 do not
// accept in-memory-only cost knowledge. The plugin persists its UID×model cost
// observations into a plugin-owned, schema-versioned local state file
// (reference parity: workbuddy2api internal/pool/persist.go state.json +
// modelCostTTL filtering on load/save).
//
// Contract pinned here:
//   - the resolved path must never point at live credential data (test-only
//     dirs via t.TempDir; guards reject credential-like names and any path
//     inside the host auth directory);
//   - atomic write (temp file + rename), startup validation, 6h TTL on load,
//     fail-safe degradation on corrupt/newer-schema/unwritable files;
//   - concurrency-safe under parallel note/save.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func wbTestStorePool(t *testing.T, dir string, clk *wbTestClock) (*wbAccountPool, *wbCostStore) {
	t.Helper()
	pool := newWbAccountPool(wbPoolParams{
		Now:                 func() time.Time { return clk.now() },
		RandInt64N:          wbSeqRand(1),
		CostExploreInterval: 30 * time.Minute,
		MinPickGap:          0,
	})
	store := newWbCostStore(filepath.Join(dir, wbCostStateFileName), time.Minute)
	store.now = func() time.Time { return clk.now() }
	if _, err := pool.attachCostStore(store); err != nil {
		t.Fatalf("attachCostStore: %v", err)
	}
	return pool, store
}

// TestResolveWbCostStatePath_Guards: the resolver must reject anything that
// could be live credential data and provide a non-live default.
func TestResolveWbCostStatePath_Guards(t *testing.T) {
	// Explicit disables.
	for _, off := range []string{"off", "OFF", "none", "-"} {
		if path, enabled, _ := wbResolveCostStatePath(off); enabled || path != "" {
			t.Errorf("%q must disable persistence, got (%q,%v)", off, path, enabled)
		}
	}
	// Credential-like file names are rejected outright.
	for _, bad := range []string{
		`C:\auths\workbuddy.json`,
		`C:\auths\workbuddy-123.json`,
		`C:\auths\codebuddy-cn-1.json`,
	} {
		if _, enabled, reason := wbResolveCostStatePath(bad); enabled {
			t.Errorf("%q must be rejected, got enabled", bad)
		} else if !strings.Contains(reason, "credential") {
			t.Errorf("%q: reason = %q, want credential-path rejection", bad, reason)
		}
	}
	// Relative paths are rejected (CWD-dependent writes are not controlled).
	if _, enabled, _ := wbResolveCostStatePath("state/cost-ledger.json"); enabled {
		t.Error("relative path must be rejected")
	}
	// A path inside the discovered host auth directory is rejected.
	authDir := t.TempDir()
	prevList := hostAuthListFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{Name: "workbuddy-1.json", Path: filepath.Join(authDir, "workbuddy-1.json")}}, nil
	}
	t.Cleanup(func() { hostAuthListFn = prevList })
	if _, enabled, reason := wbResolveCostStatePath(filepath.Join(authDir, "cost-ledger.json")); enabled {
		t.Errorf("path inside the auth dir must be rejected, reason=%q", reason)
	}
	// A normal temp path is accepted.
	okPath := filepath.Join(t.TempDir(), "cost-ledger.json")
	path, enabled, reason := wbResolveCostStatePath(okPath)
	if !enabled || path != okPath {
		t.Fatalf("normal path rejected: (%q,%v,%q)", path, enabled, reason)
	}
	// The default (empty config) is a non-live per-user path. TestMain forces
	// the env override off for the suite, so clear it here explicitly.
	t.Setenv(wbCostStateEnv, "")
	defPath, defEnabled, defReason := wbResolveCostStatePath("")
	if !defEnabled {
		t.Fatalf("default path must be enabled, reason=%q", defReason)
	}
	if isSafeWorkbuddyAuthPath(defPath) {
		t.Fatalf("default path looks like a credential path: %q", defPath)
	}
	if !strings.Contains(strings.ToLower(defPath), "workbuddy") {
		t.Fatalf("default path should be plugin-scoped, got %q", defPath)
	}
}

// TestCostStore_RoundTripRestartTTL: observations survive a restart (paid and
// free tiers restored), stale rows (>6h) are dropped on load.
func TestCostStore_RoundTripRestartTTL(t *testing.T) {
	dir := t.TempDir()
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool, _ := wbTestStorePool(t, dir, clk)
	pool.noteCost("uid-free", "m1", 0, 1000)
	pool.noteCost("uid-paid", "m1", 2.0, 1000)
	if err := pool.persistCostsNow(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Restart 5h later → both observations valid.
	clk2 := &wbTestClock{t: clk.now().Add(5 * time.Hour)}
	pool2, _ := wbTestStorePool(t, dir, clk2)
	if tier, _ := pool2.costLedger.tierFor("uid-free", "m1"); tier != wbCostTierFree {
		t.Fatalf("after restart free tier = %d, want 0", tier)
	}
	if tier, _ := pool2.costLedger.tierFor("uid-paid", "m1"); tier != wbCostTierPaid {
		t.Fatalf("after restart paid tier = %d, want 2", tier)
	}
	// The restored ledger decides routing identically: tier-0 hard filter.
	for i := 0; i < 20; i++ {
		got, ok := pool2.pick("m1", []wbPoolCandidate{{ID: "uid-paid", Credits: 10}, {ID: "uid-free", Credits: 10}})
		if !ok || got != "uid-free" {
			t.Fatalf("post-restart pick = (%q,%v), want uid-free", got, ok)
		}
		clk2.advance(time.Minute)
	}

	// Restart 7h after the observation → TTL expired on load.
	clk3 := &wbTestClock{t: clk.now().Add(7 * time.Hour)}
	pool3, store3 := wbTestStorePool(t, dir, clk3)
	if _, ok := pool3.costLedger.lookup("uid-paid", "m1"); ok {
		t.Fatal("stale observation must not resurrect after TTL")
	}
	if tier, _ := pool3.costLedger.tierFor("uid-paid", "m1"); tier != wbCostTierUnknown {
		t.Fatalf("stale tier = %d, want unknown", tier)
	}
	// And the stale rows are gone from the next persisted doc too.
	if err := pool3.persistCostsNow(); err != nil {
		t.Fatal(err)
	}
	doc := readCostStateDoc(t, store3.path)
	if len(doc.Accounts) != 0 {
		t.Fatalf("stale rows must not be re-persisted, got %v", doc.Accounts)
	}
}

// TestCostStore_MissingFileFirstRun: absent file is a clean first run, and the
// save creates missing parent directories.
func TestCostStore_MissingFileFirstRun(t *testing.T) {
	dir := t.TempDir()
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool, store := wbTestStorePool(t, filepath.Join(dir, "nested", "deeper"), clk)
	if n, err := pool.attachCostStore(store); err != nil || n != 0 {
		t.Fatalf("first run load = (%d,%v), want (0,nil)", n, err)
	}
	pool.noteCost("uid1", "m1", 1.0, 1000)
	if err := pool.persistCostsNow(); err != nil {
		t.Fatalf("persist into nested dir: %v", err)
	}
	if _, err := os.Stat(store.path); err != nil {
		t.Fatalf("state file missing: %v", err)
	}
}

// TestCostStore_CorruptFileFailsSafe: garbage on disk never breaks the pool;
// loading reports it and the next save replaces it with valid JSON.
func TestCostStore_CorruptFileFailsSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wbCostStateFileName)
	if err := os.WriteFile(path, []byte("{this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{Now: func() time.Time { return clk.now() }, RandInt64N: wbSeqRand(2), MinPickGap: 0})
	store := newWbCostStore(path, time.Minute)
	store.now = func() time.Time { return clk.now() }
	n, err := pool.attachCostStore(store)
	if err == nil {
		t.Fatal("corrupt file must surface a load error")
	}
	if n != 0 {
		t.Fatalf("corrupt file restored %d rows, want 0", n)
	}
	// Pool still routes (fail-safe) and a save repairs the file.
	pool.noteCost("uid1", "m1", 0, 500)
	if got, ok := pool.pick("m1", []wbPoolCandidate{{ID: "uid1", Credits: 5}}); !ok || got != "uid1" {
		t.Fatalf("pool unusable after corrupt load: (%q,%v)", got, ok)
	}
	if err := pool.persistCostsNow(); err != nil {
		t.Fatalf("repair save: %v", err)
	}
	doc := readCostStateDoc(t, path)
	if doc.SchemaVersion != wbCostStateSchemaVersion || len(doc.Accounts) != 1 {
		t.Fatalf("repaired doc = %+v", doc)
	}
}

// TestCostStore_NewerSchemaRefusedNoClobber: a future schema must be preserved,
// not overwritten — persistence disables itself for this process.
func TestCostStore_NewerSchemaRefusedNoClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, wbCostStateFileName)
	future := `{"schema_version":99,"updated_at":"2030-01-01T00:00:00Z","accounts":{"u":{"m":{"cost_per_1k":1,"last_seen":"2030-01-01T00:00:00Z","samples":1}}}}`
	if err := os.WriteFile(path, []byte(future), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{Now: func() time.Time { return clk.now() }, RandInt64N: wbSeqRand(3), MinPickGap: 0})
	store := newWbCostStore(path, time.Minute)
	store.now = func() time.Time { return clk.now() }
	if _, err := pool.attachCostStore(store); err == nil {
		t.Fatal("newer schema must surface a load error")
	}
	pool.noteCost("uid1", "m1", 1, 1000)
	if err := pool.persistCostsNow(); err == nil {
		t.Fatal("save must refuse to clobber a newer schema file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != future {
		t.Fatalf("newer schema file was modified:\n%s", got)
	}
}

// TestCostStore_WriteFailureFailsSafe: an unwritable target degrades to
// in-memory only — no partial files, no temp litter, pool still routes.
func TestCostStore_WriteFailureFailsSafe(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker") // a FILE used as a parent dir
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{Now: func() time.Time { return clk.now() }, RandInt64N: wbSeqRand(4), MinPickGap: 0})
	store := newWbCostStore(filepath.Join(blocker, wbCostStateFileName), time.Minute)
	store.now = func() time.Time { return clk.now() }
	pool.attachCostStore(store)
	pool.noteCost("uid1", "m1", 0, 500)
	if err := pool.persistCostsNow(); err == nil {
		t.Fatal("unwritable target must error")
	}
	// In-memory knowledge still works.
	if tier, _ := pool.costLedger.tierFor("uid1", "m1"); tier != wbCostTierFree {
		t.Fatalf("in-memory tier = %d, want 0", tier)
	}
	if got, ok := pool.pick("m1", []wbPoolCandidate{{ID: "uid1", Credits: 1}}); !ok || got != "uid1" {
		t.Fatalf("pool unusable after write failure: (%q,%v)", got, ok)
	}
	// No temp files anywhere near the target.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// TestCostStore_ConcurrentNoteAndSave: parallel observations and saves stay
// consistent and the final file parses with every observation present.
func TestCostStore_ConcurrentNoteAndSave(t *testing.T) {
	dir := t.TempDir()
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{Now: func() time.Time { return clk.now() }, RandInt64N: wbSeqRand(5), MinPickGap: 0})
	store := newWbCostStore(filepath.Join(dir, wbCostStateFileName), 0) // every save allowed
	store.now = func() time.Time { return clk.now() }
	pool.attachCostStore(store)

	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.noteCost(uidForIndex(i), "m1", float64(i), 1000)
			if i%3 == 0 {
				_ = pool.persistCostsNow()
			}
		}()
	}
	wg.Wait()
	if err := pool.persistCostsNow(); err != nil {
		t.Fatalf("final save: %v", err)
	}
	doc := readCostStateDoc(t, store.path)
	if len(doc.Accounts) != n {
		t.Fatalf("persisted accounts = %d, want %d", len(doc.Accounts), n)
	}
	for i := 0; i < n; i++ {
		if _, ok := doc.Accounts[uidForIndex(i)]; !ok {
			t.Fatalf("account %s missing from persisted doc", uidForIndex(i))
		}
	}
	// A fresh load sees the same set.
	clk2 := &wbTestClock{t: clk.now()}
	pool2 := newWbAccountPool(wbPoolParams{Now: func() time.Time { return clk2.now() }, RandInt64N: wbSeqRand(6), MinPickGap: 0})
	store2 := newWbCostStore(store.path, 0)
	store2.now = func() time.Time { return clk2.now() }
	pool2.attachCostStore(store2)
	if got := pool2.costLedger.count(); got != n {
		t.Fatalf("reloaded rows = %d, want %d", got, n)
	}
}

// TestCostStore_SaveThrottledAndForced: maybeSave honors the interval; a forced
// save writes immediately.
func TestCostStore_SaveThrottledAndForced(t *testing.T) {
	dir := t.TempDir()
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool, store := wbTestStorePool(t, dir, clk) // saveEvery = 1m
	store.saveEvery = time.Hour
	pool.noteCost("uid1", "m1", 1, 1000)
	if err := pool.maybePersistCosts(); err != nil {
		t.Fatalf("first maybeSave: %v", err)
	}
	first := readCostStateDoc(t, store.path)
	clk.advance(10 * time.Minute)
	pool.noteCost("uid2", "m1", 2, 1000)
	if err := pool.maybePersistCosts(); err != nil {
		t.Fatalf("throttled maybeSave: %v", err)
	}
	if len(readCostStateDoc(t, store.path).Accounts) != 1 {
		t.Fatal("throttled save must not rewrite the file")
	}
	_ = first
	if err := pool.persistCostsNow(); err != nil {
		t.Fatal(err)
	}
	if len(readCostStateDoc(t, store.path).Accounts) != 2 {
		t.Fatal("forced save must include the new observation")
	}
}

func uidForIndex(i int) string {
	return "uid-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/10))
}

type wbCostStateDocTest = wbCostStateDoc

func readCostStateDoc(t *testing.T, path string) wbCostStateDocTest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var doc wbCostStateDocTest
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("state file is not valid JSON: %v\n%s", err, raw)
	}
	return doc
}
