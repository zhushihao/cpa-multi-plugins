// cost_persist.go persists the routing pool's UID×model cost observations into
// a plugin-owned, schema-versioned local state file.
//
// Architecture ruling (2026-09-29): G07/G09 do not accept in-memory-only cost
// knowledge; the plugin must keep the 3002 semantics (restart keeps cost
// tiers) without writing host credentials. Reference parity:
// workbuddy2api internal/pool/persist.go (state.json, modelCostTTL filtering on
// load and save, throttled flush, failure log throttling).
//
// Safety contract:
//   - the state file is the plugin's own artifact: it stores cost metadata
//     only (routing account ID / model / per-1k cost / last-seen / samples) —
//     never tokens, never credential documents; the host auth store is never
//     written. The routing account ID is the host auth record ID the scheduler
//     addresses (the pool's key, see routing_pool.go noteCost), not the
//     account UID;
//   - the resolved path must not look like credential data: credential-like
//     file names and any path inside the host auth directory are rejected, and
//     relative paths are rejected (no CWD-dependent writes);
//   - writes are atomic (temp file + rename) so a crash never leaves a torn
//     file; saves happen at most once per throttle interval (plus a periodic
//     janitor flush);
//   - loads are fail-safe: missing file = clean first run; corrupt file =
//     ignore + log (the next save repairs it); newer schema on disk = refuse
//     to load AND refuse to write (never clobber a future version).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// wbCostStateSchemaVersion versions the on-disk document. Bump it when the
	// row meaning changes; older versions must stay loadable or be migrated.
	wbCostStateSchemaVersion = 1
	// wbCostStateFileName is the default artifact name (plugin-scoped).
	wbCostStateFileName = "cost-ledger.json"
	// wbCostSaveInterval throttles writes: observations are frequent, disk
	// sync is not free, and a 1-minute window loses at most one minute of
	// learning on a hard crash.
	wbCostSaveInterval = time.Minute
	// wbCostStateEnv overrides the path (deployment-friendly, like the usage
	// report env knobs).
	wbCostStateEnv = "WORKBUDDY_STATE_FILE"
	// wbCostErrLogInterval throttles save/load failure logs.
	wbCostErrLogInterval = time.Minute
)

// wbCostStateEntry is one persisted (routing account ID, model) observation.
type wbCostStateEntry struct {
	CostPer1k float64 `json:"cost_per_1k"`
	LastSeen  string  `json:"last_seen"`
	Samples   int     `json:"samples"`
}

// wbCostStateDoc is the on-disk document. Accounts is keyed by the host auth
// record ID the scheduler addresses (schema v1; see routing_pool.go wbCostLedger).
type wbCostStateDoc struct {
	SchemaVersion int                                    `json:"schema_version"`
	UpdatedAt     string                                 `json:"updated_at,omitempty"`
	Accounts      map[string]map[string]wbCostStateEntry `json:"accounts"`
}

// wbCostStore owns one state file: path, throttle state, and the refusal flag
// set when a newer-schema file must not be touched.
//
// Concurrency model (review 2026-09-29):
//   - writeMu serializes whole writes (snapshot → marshal → atomic write →
//     bookkeeping). Two concurrent saveFrom calls therefore cannot share a temp
//     path or interleave a rename. Lock order is writeMu → mu → ledger.mu; no
//     path acquires them in reverse (noteCost releases ledger.mu before
//     markDirty), and the tests-only hook runs with no lock held, so a parked
//     write can never block a note.
//   - dirtyRev is bumped by every markDirty. A save clears the dirty flag only
//     when no observation landed since its snapshot (dirtyRev unchanged),
//     otherwise the newer observation stays pending for the janitor/next save.
type wbCostStore struct {
	mu        sync.Mutex
	writeMu   sync.Mutex
	path      string
	saveEvery time.Duration
	now       func() time.Time
	// lastSave is reserved at the START of a write attempt (not on success), so
	// a failing disk is retried at the throttle cadence instead of hammered.
	lastSave   time.Time
	lastErrLog time.Time
	dirty      bool
	dirtyRev   int64
	refuse     bool
	refuseWhy  string
	// afterSnapshotHook, when set (tests only), runs after the ledger snapshot
	// and before the atomic write, with NO store lock held — it exists so
	// concurrency tests can deterministically park a write while a note lands.
	afterSnapshotHook func()
}

func newWbCostStore(path string, saveEvery time.Duration) *wbCostStore {
	return &wbCostStore{
		path:      path,
		saveEvery: saveEvery,
		now:       time.Now,
	}
}

// markDirty flags that the ledger changed since the last write and bumps the
// revision used to detect observations arriving during a write.
func (s *wbCostStore) markDirty() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.dirty = true
	s.dirtyRev++
	s.mu.Unlock()
}

// loadInto validates and restores the state file into the ledger, applying the
// same 6h TTL rule as live lookups. Missing file = (0, nil).
func (s *wbCostStore) loadInto(ledger *wbCostLedger) (int, error) {
	if s == nil || ledger == nil {
		return 0, nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		wrapped := fmt.Errorf("read cost state %s: %w", s.path, err)
		s.logErr(wrapped)
		return 0, wrapped
	}
	var doc wbCostStateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		wrapped := fmt.Errorf("parse cost state %s: %w", s.path, err)
		s.logErr(wrapped)
		return 0, wrapped
	}
	if doc.SchemaVersion > wbCostStateSchemaVersion {
		wrapped := fmt.Errorf("cost state %s has newer schema v%d (this plugin understands v%d) — leaving it untouched",
			s.path, doc.SchemaVersion, wbCostStateSchemaVersion)
		s.mu.Lock()
		s.refuse = true
		s.refuseWhy = wrapped.Error()
		s.mu.Unlock()
		s.logErr(wrapped)
		return 0, wrapped
	}
	if doc.SchemaVersion != wbCostStateSchemaVersion {
		wrapped := fmt.Errorf("cost state %s has unsupported schema v%d", s.path, doc.SchemaVersion)
		s.logErr(wrapped)
		return 0, wrapped
	}
	now := s.now()
	restored := 0
	for uid, models := range doc.Accounts {
		for model, entry := range models {
			lastSeen, err := time.Parse(time.RFC3339Nano, entry.LastSeen)
			if err != nil {
				continue
			}
			if ledger.restore(uid, model, wbCostObservation{
				CostPer1k: entry.CostPer1k,
				LastSeen:  lastSeen,
				Samples:   entry.Samples,
			}, now) {
				restored++
			}
		}
	}
	return restored, nil
}

// saveFrom snapshots the ledger (TTL-filtered) and writes it atomically.
// Writers are serialized by writeMu; the dirty flag is cleared only for the
// revision this write covered.
func (s *wbCostStore) saveFrom(ledger *wbCostLedger) error {
	if s == nil || ledger == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	if s.refuse {
		why := s.refuseWhy
		s.mu.Unlock()
		return fmt.Errorf("cost state write refused: %s", why)
	}
	rev := s.dirtyRev
	// Reserve the throttle window up front: a failing disk must not turn every
	// noteCost into a write attempt.
	s.lastSave = s.now()
	hook := s.afterSnapshotHook
	s.mu.Unlock()

	now := s.now()
	snapshot := ledger.snapshot(now)
	doc := wbCostStateDoc{
		SchemaVersion: wbCostStateSchemaVersion,
		UpdatedAt:     now.UTC().Format(time.RFC3339Nano),
		Accounts:      make(map[string]map[string]wbCostStateEntry, len(snapshot)),
	}
	for uid, models := range snapshot {
		rows := make(map[string]wbCostStateEntry, len(models))
		for model, entry := range models {
			rows[model] = wbCostStateEntry{
				CostPer1k: entry.CostPer1k,
				LastSeen:  entry.LastSeen.UTC().Format(time.RFC3339Nano),
				Samples:   entry.Samples,
			}
		}
		doc.Accounts[uid] = rows
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		s.logErr(err)
		return err
	}
	if hook != nil {
		hook() // test-only park point: no store lock is held here
	}
	if err := s.writeAtomic(raw); err != nil {
		s.logErr(err)
		return err // dirty stays set: the next window retries with intact data
	}
	s.mu.Lock()
	// Clear dirty only when nothing landed after this write's snapshot. A note
	// that raced the write bumped dirtyRev and must stay pending so the janitor
	// flushes it.
	if s.dirtyRev == rev {
		s.dirty = false
	}
	s.mu.Unlock()
	return nil
}

// maybeSave writes only when dirty and past the throttle interval.
func (s *wbCostStore) maybeSave(ledger *wbCostLedger) error {
	if s == nil || ledger == nil {
		return nil
	}
	s.mu.Lock()
	if s.refuse || !s.dirty {
		s.mu.Unlock()
		return nil
	}
	now := s.now()
	if !s.lastSave.IsZero() && s.saveEvery > 0 && now.Sub(s.lastSave) < s.saveEvery {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	return s.saveFrom(ledger)
}

// writeAtomic writes via a unique same-directory temp file and rename, so
// readers (and a crash) never observe a partially written document.
func (s *wbCostStore) writeAtomic(raw []byte) error {
	return wbAtomicWriteFile(s.path, raw)
}

// wbAtomicWriteFile writes raw to path via a unique same-directory temp file
// and rename, so readers (and a crash) never observe a partially written
// document. Uniqueness comes from os.CreateTemp (O_EXCL + random suffix)
// instead of a pid+timestamp name, which collides when the clock is coarse
// (Windows) or frozen; concurrent writers of the same artifact must serialize
// above this helper. The temp file is always cleaned up on failure.
func wbAtomicWriteFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp state in %s: %w", dir, err)
	}
	tmp := f.Name()
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp state %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync temp state %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp state %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace state %s: %w", path, err)
	}
	return nil
}

// logErr logs state-file failures at most once per interval (a full disk or a
// revoked permission must not flood the host log).
func (s *wbCostStore) logErr(err error) {
	s.mu.Lock()
	now := s.now()
	if !s.lastErrLog.IsZero() && now.Sub(s.lastErrLog) < wbCostErrLogInterval {
		s.mu.Unlock()
		return
	}
	s.lastErrLog = now
	s.mu.Unlock()
	log.Printf("workbuddy: cost state: %v", err)
}

// wbResolveCostStatePath resolves the effective state-file path from config
// ("off"/"none"/"-" disable) or the WORKBUDDY_STATE_FILE env, defaulting to a
// plugin-scoped per-user cache path. It returns enabled=false with a reason
// when the value is disabled or unsafe: credential-like names, paths inside
// the host auth directory, and relative paths are all rejected so the state
// file can never point at live credential data.
func wbResolveCostStatePath(raw string) (path string, enabled bool, reason string) {
	v := strings.TrimSpace(raw)
	if v == "" {
		v = strings.TrimSpace(os.Getenv(wbCostStateEnv))
	}
	if v != "" {
		switch strings.ToLower(v) {
		case "off", "none", "-", "disabled":
			return "", false, "disabled by configuration"
		}
	} else {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return "", false, "no per-user cache dir available: " + err.Error()
		}
		v = filepath.Join(cacheDir, "workbuddy-plugin", wbCostStateFileName)
	}
	if !filepath.IsAbs(v) {
		return "", false, "state path must be absolute"
	}
	if isSafeWorkbuddyAuthPath(v) {
		return "", false, "state path looks like a credential path"
	}
	if dir := peerAuthDir(); dir != "" && isPathUnder(v, dir) {
		return "", false, "state path is inside the host auth directory"
	}
	return v, true, ""
}

// wbApplyCostStateConfig (re)configures cost persistence from the raw
// state_file config value. Enabled → attach a fresh store (loading the
// TTL-filtered snapshot into the pool, newest observation wins per row).
// Disabled/invalid → DETACH the current store so reconfigure cannot leave a
// stale writer pointed at the old path (N2 second audit).
func wbApplyCostStateConfig(raw string) {
	path, enabled, reason := wbResolveCostStatePath(raw)
	if !enabled {
		wbRouterPool.detachCostStore()
		log.Printf("workbuddy: cost persistence disabled — %s", reason)
		return
	}
	if restored, err := wbRouterPool.attachCostStore(newWbCostStore(path, wbCostSaveInterval)); err != nil {
		log.Printf("workbuddy: cost state %s unreadable (%v) — starting with in-memory observations", path, err)
	} else if restored > 0 {
		log.Printf("workbuddy: cost state %s restored %d observation(s)", path, restored)
	} else {
		log.Printf("workbuddy: cost state %s ready (no observations yet)", path)
	}
}

// resolveUsageReport fills usageReportURL/key from config → env → secret files.
// Mirrors community plugins that inject management keys via env/build (e.g.
// codex-auth-importer CODEX_AUTH_IMPORTER_MANAGEMENT_KEY), not plaintext CPA
// remote-management.secret-key (that field is bcrypt-hashed).
