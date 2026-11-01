// routing_pool.go is the plugin-side pure-Go port of workbuddy2api's account
// pool selection semantics (internal/pool/pick.go + entry.go), scoped to what
// the plugin scheduler can observe:
//
//   - three-factor weighted lottery: credits ratio ×10, expiring-credits ratio
//     ×8, idle compensation (0.5/h, capped at 5);
//   - observed cost tiers with a hard filter: 0 = measured free, 1 = no
//     observation (or observation expired), 2 = measured paid;
//   - conditional exploration: under tier-0 monopoly with tier-1 candidates
//     present, one ride-along request per 30m window is switched to the
//     unknown tier (no extra upstream calls), so free windows can be
//     discovered and paid accounts avoided;
//   - Top-5 shortlist + weighted draw + anti-thundering-herd pick gap with an
//     LRU fallback by monotonic sequence.
//
// Everything here is deterministic given the injected clock and random source
// (randInt64N), which is what makes the parity tests reproducible. No CGO, no
// host RPC — this file is unit-testable with CGO_ENABLED=0.
package main

import (
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cost tiers (workbuddy2api internal/pool/pick.go:88-139 semantics).
const (
	wbCostTierFree    = 0 // measured cost_per_1k <= 0 within TTL
	wbCostTierUnknown = 1 // no observation, or observation expired
	wbCostTierPaid    = 2 // measured cost_per_1k > 0
)

const (
	// wbModelCostTTL is how long one (account, model) cost observation stays
	// valid. 6h mirrors the reference: it covers time-of-day free windows but
	// stale prices do not resurrect across restarts.
	wbModelCostTTL = 6 * time.Hour
	// wbModelCostAlpha is the EMA smoothing factor (about 5 observations to
	// converge) so one anomalous sample cannot steer routing.
	wbModelCostAlpha = 0.3
	// wbDefaultCostExploreInterval is the tier-0→tier-1 exploration window.
	wbDefaultCostExploreInterval = 30 * time.Minute
	// wbDefaultMinPickGap fans concurrent requests across accounts; it is the
	// anti-thundering-herd window from the reference (100ms).
	wbDefaultMinPickGap = 100 * time.Millisecond
	// Idle-compensation defaults (reference defaultIdleWeightPerHour/Max).
	wbDefaultIdleWeightPerHour = 0.5
	wbDefaultIdleWeightMax     = 5.0
	// wbTopShortlist is the shortlist size before the weighted draw.
	wbTopShortlist = 5
	// Weight factors (reference weightOf).
	wbCreditsFactor  = 10.0
	wbExpiringFactor = 8.0
	// Fixed-point scale for the weighted draw (reference pickWeighted).
	wbWeightScale = 1_000_000
)

// wbCostObservation is one (account, model) cost ledger row.
type wbCostObservation struct {
	// CostPer1k is the EMA-smoothed credits per 1000 tokens. <= 0 = measured
	// free (tier 0).
	CostPer1k float64
	// LastSeen is when the newest sample was recorded (TTL anchor).
	LastSeen time.Time
	// Samples counts recorded observations (diagnostics).
	Samples int
}

// wbCostModelKey normalizes a requested model into the ledger's canonical key.
// Both the observation side (executor usage) and the lookup side (SchedulerPick)
// MUST derive the key through this one function, otherwise the cost tier
// filter silently degrades to "unknown" (review S1/I6).
//
// Normalization resolves the host oauth-model-alias back to the bare upstream
// id — the same resolution resolveUpstreamModel performs per request.
//
// Bare-only boundary (Issue #6 owner correction): the removed round4 realm
// namespace (cn: / global: / intl:) is NOT normalized away. Such names are
// denied at the chat ingress and can never be observed, and the ledger key
// must stay total and identity-preserving — stripping one would merge two
// providers' distinct client names into a single ledger row.
//
// Case is preserved: upstream model ids are case-sensitive and must never be
// merged by an incidental case difference.
func wbCostModelKey(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return ""
	}
	if resolved := strings.TrimSpace(resolveUpstreamModel(m, nil)); resolved != "" {
		m = resolved
	}
	return strings.TrimSpace(m)
}

// wbCostLedger tracks measured per-request credit cost per (account, model).
// The account key is the host auth record ID (the only identity SchedulerPick
// can address; see wbAccountPool.noteCost for the S1 rationale). The model key
// is always the canonical form from wbCostModelKey. Thread-safe; all time
// reads go through now() so tests can fast-forward.
type wbCostLedger struct {
	mu      sync.Mutex
	entries map[string]map[string]wbCostObservation // accountID -> model -> observation
	now     func() time.Time
	ttl     time.Duration
}

func newWbCostLedger(now func() time.Time) *wbCostLedger {
	if now == nil {
		now = time.Now
	}
	return &wbCostLedger{entries: map[string]map[string]wbCostObservation{}, now: now, ttl: wbModelCostTTL}
}

// note records one observation. credit is the upstream usage.credit for the
// request (consumed credits, not balance); tokens is the request's total token
// count. tokens <= 0 or an empty uid/model is not a valid observation and is
// ignored (cannot normalize per-1k without tokens).
func (l *wbCostLedger) note(uid, model string, credit float64, tokens int) {
	if l == nil || uid == "" || model == "" || tokens <= 0 {
		return
	}
	per1k := credit / float64(tokens) * 1000
	if per1k < 0 {
		per1k = 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	byModel, ok := l.entries[uid]
	if !ok {
		byModel = map[string]wbCostObservation{}
		l.entries[uid] = byModel
	}
	prev, seen := byModel[model]
	if !seen {
		byModel[model] = wbCostObservation{CostPer1k: per1k, LastSeen: now, Samples: 1}
		return
	}
	byModel[model] = wbCostObservation{
		CostPer1k: prev.CostPer1k*(1-wbModelCostAlpha) + per1k*wbModelCostAlpha,
		LastSeen:  now,
		Samples:   prev.Samples + 1,
	}
}

// lookup returns the valid observation for (uid, model); expired rows are
// reported as missing (and lazily dropped on the next note for that model).
func (l *wbCostLedger) lookup(uid, model string) (wbCostObservation, bool) {
	if l == nil || uid == "" || model == "" {
		return wbCostObservation{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	byModel, ok := l.entries[uid]
	if !ok {
		return wbCostObservation{}, false
	}
	entry, ok := byModel[model]
	if !ok || entry.LastSeen.IsZero() {
		return wbCostObservation{}, false
	}
	if l.now().Sub(entry.LastSeen) > l.ttl {
		delete(byModel, model)
		return wbCostObservation{}, false
	}
	return entry, true
}

// tierFor maps an account+model to its cost tier and observed per-1k cost.
func (l *wbCostLedger) tierFor(uid, model string) (int, float64) {
	entry, ok := l.lookup(uid, model)
	if !ok {
		return wbCostTierUnknown, 0
	}
	if entry.CostPer1k <= 0 {
		return wbCostTierFree, 0
	}
	return wbCostTierPaid, entry.CostPer1k
}

// snapshot returns a TTL-filtered deep copy of every observation (uid → model
// → entry) and prunes expired rows from the live maps. Used by the state-file
// persister; the reference filters by modelCostTTL on save for the same
// reason (stale time-of-day prices must not be written back).
func (l *wbCostLedger) snapshot(now time.Time) map[string]map[string]wbCostObservation {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]map[string]wbCostObservation, len(l.entries))
	for uid, byModel := range l.entries {
		for model, entry := range byModel {
			if entry.LastSeen.IsZero() || now.Sub(entry.LastSeen) > l.ttl {
				delete(byModel, model)
				continue
			}
			if out[uid] == nil {
				out[uid] = map[string]wbCostObservation{}
			}
			out[uid][model] = entry
		}
		if len(byModel) == 0 {
			delete(l.entries, uid)
		}
	}
	return out
}

// restore inserts one persisted observation, applying the same validity and
// TTL rules as lookup (never resurrect stale or malformed rows).
//
// A reconfigure-time reload must never regress live knowledge: when the
// in-memory row is NEWER than the disk row, the in-memory row wins (minor
// review fix — the file snapshot can lag a just-recorded observation).
func (l *wbCostLedger) restore(uid, model string, entry wbCostObservation, now time.Time) bool {
	if l == nil || uid == "" || model == "" || entry.Samples < 1 || entry.CostPer1k < 0 {
		return false
	}
	if entry.LastSeen.IsZero() || now.Sub(entry.LastSeen) > l.ttl {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	byModel, ok := l.entries[uid]
	if !ok {
		byModel = map[string]wbCostObservation{}
		l.entries[uid] = byModel
	}
	if prev, seen := byModel[model]; seen && prev.LastSeen.After(entry.LastSeen) {
		return false // newer live observation must not be overwritten
	}
	byModel[model] = entry
	return true
}

// count returns the number of (uid, model) rows currently held.
func (l *wbCostLedger) count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, byModel := range l.entries {
		n += len(byModel)
	}
	return n
}

// wbPoolCandidate is the scheduler's view of one routable WorkBuddy account.
type wbPoolCandidate struct {
	ID              string
	Disabled        bool
	Exhausted       bool
	Credits         int64
	CreditsExpiring int64
}

// wbPoolParams are tunables plus injected clock/randomness. Zero values fall
// back to the reference defaults; tests inject Now and RandInt64N for
// reproducibility.
type wbPoolParams struct {
	IdleWeightPerHour   float64
	IdleWeightMax       float64
	CostExploreInterval time.Duration
	MinPickGap          time.Duration
	// Now returns the current time (nil → time.Now).
	Now func() time.Time
	// RandInt64N returns a uniform value in [0,n) (nil → math/rand/v2).
	RandInt64N func(int64) int64
}

// wbAccountPool is the stateful picker: cost ledger + last-used bookkeeping +
// exploration timers. One instance for the plugin process.
type wbAccountPool struct {
	mu   sync.Mutex
	pars wbPoolParams

	costLedger *wbCostLedger

	// costStore persists the ledger to the plugin-owned state file (nil =
	// in-memory only). Loaded on attach, saved on a throttle. Swapped by
	// reconfigure (attach/detach) while requests read it, so access goes
	// through getCostStore/setCostStore.
	costStoreMu sync.RWMutex
	costStore   *wbCostStore

	lastUsed map[string]time.Time
	usedSeq  map[string]int64
	pickSeq  int64

	// exploreLast is keyed by model (the plugin has no realm dimension here).
	exploreLast map[string]time.Time
}

// getCostStore returns the attached state file (nil when persistence is
// disabled or not yet attached).
func (p *wbAccountPool) getCostStore() *wbCostStore {
	if p == nil {
		return nil
	}
	p.costStoreMu.RLock()
	defer p.costStoreMu.RUnlock()
	return p.costStore
}

// setCostStore swaps the attached state file (nil detaches). Reconfigure calls
// this while request paths read the pointer, hence the lock.
func (p *wbAccountPool) setCostStore(store *wbCostStore) {
	if p == nil {
		return
	}
	p.costStoreMu.Lock()
	p.costStore = store
	p.costStoreMu.Unlock()
}

func newWbAccountPool(params wbPoolParams) *wbAccountPool {
	if params.IdleWeightPerHour <= 0 {
		params.IdleWeightPerHour = wbDefaultIdleWeightPerHour
	}
	if params.IdleWeightMax <= 0 {
		params.IdleWeightMax = wbDefaultIdleWeightMax
	}
	if params.CostExploreInterval <= 0 {
		params.CostExploreInterval = wbDefaultCostExploreInterval
	}
	if params.Now == nil {
		params.Now = time.Now
	}
	p := &wbAccountPool{
		pars:        params,
		lastUsed:    map[string]time.Time{},
		usedSeq:     map[string]int64{},
		exploreLast: map[string]time.Time{},
	}
	p.costLedger = newWbCostLedger(params.Now)
	return p
}

// noteCost records one measured cost observation (counterpart of
// workbuddy2api Pool.NoteModelCost; called from usage handling) and schedules
// a throttled state-file save so a restart keeps the knowledge.
//
// id is the host auth record ID the scheduler addresses (NOT the account UID:
// SchedulerPick only ever sees host IDs, so a UID-keyed row would be
// unreachable — review S1). The model is canonicalized here so the same key
// space is used by pickWithSticky's lookup.
func (p *wbAccountPool) noteCost(id, model string, credit float64, tokens int) {
	if p == nil {
		return
	}
	p.costLedger.note(id, wbCostModelKey(model), credit, tokens)
	if store := p.getCostStore(); store != nil {
		store.markDirty()
	}
	p.maybePersistCosts()
}

// attachCostStore wires the plugin-owned state file into this pool: the file
// is loaded (TTL-filtered) and later saves go through it. A load error is
// returned for logging but never fatal — the pool continues in-memory
// (fail-safe degradation).
func (p *wbAccountPool) attachCostStore(store *wbCostStore) (int, error) {
	if p == nil || store == nil {
		return 0, nil
	}
	p.setCostStore(store)
	return store.loadInto(p.costLedger)
}

// detachCostStore removes the state file so no further writes touch it
// (state_file: off on reconfigure — N2 second audit).
func (p *wbAccountPool) detachCostStore() {
	p.setCostStore(nil)
}

// persistCostsNow force-writes the ledger to the state file.
func (p *wbAccountPool) persistCostsNow() error {
	if p == nil {
		return nil
	}
	store := p.getCostStore()
	if store == nil {
		return nil
	}
	return store.saveFrom(p.costLedger)
}

// maybePersistCosts writes only when dirty and past the throttle interval.
func (p *wbAccountPool) maybePersistCosts() error {
	if p == nil {
		return nil
	}
	store := p.getCostStore()
	if store == nil {
		return nil
	}
	return store.maybeSave(p.costLedger)
}

// markUsed records that an account served a request (also used by the sticky
// path so sticky hits participate in LRU ordering and idle compensation).
func (p *wbAccountPool) markUsed(id string) {
	if p == nil || id == "" {
		return
	}
	now := p.pars.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.touchLocked(id, now)
}

func (p *wbAccountPool) touchLocked(id string, now time.Time) {
	p.lastUsed[id] = now
	p.pickSeq++
	p.usedSeq[id] = p.pickSeq
}

// wbWeightedCand is one shortlist entry: candidate + precomputed weight, tier
// and unit cost (computed exactly once per pick — the reference calls this out;
// recomputing inside the sort comparator or draw turns O(n) into O(n log n)).
type wbWeightedCand struct {
	cand   wbPoolCandidate
	w      float64
	tier   int
	cost1k float64
}

// pick selects one account for model from cands. Returns ("", false) when no
// candidate is routable.
func (p *wbAccountPool) pick(model string, cands []wbPoolCandidate) (string, bool) {
	return p.pickWithSticky(model, cands, "")
}

// pickWithSticky is pick plus a session-stickiness preference: when stickyUID
// is a live candidate whose cost tier equals the effective tier this pick
// would use, it is served directly (and marked used). A sticky account in a
// worse tier is refused — the tier-0 hard filter is unconditional, so session
// affinity can never route around it; the caller then re-binds the session to
// the account this pick actually chose.
func (p *wbAccountPool) pickWithSticky(model string, cands []wbPoolCandidate, stickyUID string) (string, bool) {
	if p == nil {
		return "", false
	}
	// Canonical model key (host alias resolved; bare-only — the round4 realm
	// namespace is removed, Issue #6 owner correction) — the same key noteCost
	// writes, so observations are addressable regardless of which spelling the
	// caller uses (review S1/I6).
	model = wbCostModelKey(model)
	now := p.pars.Now()
	live := make([]wbPoolCandidate, 0, len(cands))
	for _, c := range cands {
		if c.Disabled || c.Exhausted || c.ID == "" {
			continue
		}
		live = append(live, c)
	}
	if len(live) == 0 {
		return "", false
	}

	var maxCredits int64
	for _, c := range live {
		if c.Credits > maxCredits {
			maxCredits = c.Credits
		}
	}

	// Everything below runs under the pool lock so concurrent picks see a
	// consistent lastUsed/seq/explore state (the ledger has its own lock and is
	// always acquired inside this one — ordering is pool→ledger, never the
	// reverse, so there is no inversion).
	p.mu.Lock()
	defer p.mu.Unlock()

	ws := make([]wbWeightedCand, 0, len(live))
	bestTier := wbCostTierPaid
	hasTier1 := false
	for _, c := range live {
		tier, cost1k := p.costLedger.tierFor(c.ID, model)
		if tier < bestTier {
			bestTier = tier
		}
		if tier == wbCostTierUnknown {
			hasTier1 = true
		}
		ws = append(ws, wbWeightedCand{cand: c, w: p.weightOf(c, maxCredits, now), tier: tier, cost1k: cost1k})
	}

	// Conditional exploration: tier-0 monopoly + tier-1 candidates present +
	// window elapsed → this single pick targets tier 1 (ride-along). The timer
	// write happens under the pool lock, so concurrent picks cannot double-fire
	// the same window.
	if p.pars.CostExploreInterval > 0 && bestTier == wbCostTierFree && model != "" && hasTier1 {
		last := p.exploreLast[model]
		if last.IsZero() || now.Sub(last) >= p.pars.CostExploreInterval {
			p.exploreLast[model] = now
			bestTier = wbCostTierUnknown
		}
	}

	// Sticky preference inside the effective tier.
	if stickyUID != "" {
		for _, w := range ws {
			if w.cand.ID == stickyUID && w.tier == bestTier {
				p.touchLocked(stickyUID, now)
				return stickyUID, true
			}
		}
	}

	eligibleAll := make([]wbWeightedCand, 0, len(ws))
	for _, w := range ws {
		if w.tier == bestTier {
			eligibleAll = append(eligibleAll, w)
		}
	}
	if len(eligibleAll) == 0 {
		return "", false
	}

	// Shortlist ordering: paid tier sorts by unit cost ascending first; other
	// tiers have cost1k 0 so this degenerates to weight-desc; final tie-break
	// by ID keeps the order deterministic (the reference adds a time-seeded
	// shuffle for equal weights — dropped here on purpose so tests and
	// restarts are reproducible; the ID tie-break still spreads picks).
	sort.SliceStable(eligibleAll, func(i, j int) bool {
		if eligibleAll[i].cost1k != eligibleAll[j].cost1k {
			return eligibleAll[i].cost1k < eligibleAll[j].cost1k
		}
		if eligibleAll[i].w != eligibleAll[j].w {
			return eligibleAll[i].w > eligibleAll[j].w
		}
		return eligibleAll[i].cand.ID < eligibleAll[j].cand.ID
	})

	shortlist := eligibleAll
	if len(shortlist) > wbTopShortlist {
		shortlist = shortlist[:wbTopShortlist]
	}
	// Anti-thundering-herd: skip accounts used within the gap; when the whole
	// shortlist was just used, fall back to the globally least-recently-used
	// account by monotonic sequence (wall-clock precision is not reliable on
	// Windows, hence the sequence).
	var draw []wbWeightedCand
	if p.pars.MinPickGap > 0 {
		for _, w := range shortlist {
			if last, ok := p.lastUsed[w.cand.ID]; !ok || now.Sub(last) >= p.pars.MinPickGap {
				draw = append(draw, w)
			}
		}
	} else {
		draw = shortlist
	}
	var chosen string
	if len(draw) == 0 {
		oldest := eligibleAll[0]
		oldestSet := false
		for _, w := range eligibleAll {
			seq := p.usedSeq[w.cand.ID]
			bestSeq := p.usedSeq[oldest.cand.ID]
			if !oldestSet || seq < bestSeq || (seq == bestSeq && w.cand.ID < oldest.cand.ID) {
				oldest = w
				oldestSet = true
			}
		}
		chosen = oldest.cand.ID
	} else {
		chosen = p.drawWeightedLocked(draw)
	}
	if chosen == "" {
		return "", false
	}
	p.touchLocked(chosen, now)
	return chosen, true
}

// drawWeightedLocked performs the fixed-point weighted draw. The caller must
// hold p.mu.
func (p *wbAccountPool) drawWeightedLocked(draw []wbWeightedCand) string {
	weights := make([]int64, len(draw))
	var total int64
	for i, d := range draw {
		wi := int64(d.w*wbWeightScale + 0.5)
		if wi < 1 {
			wi = 1 // never zero out a candidate entirely
		}
		weights[i] = wi
		total += wi
	}
	rnd := p.pars.RandInt64N
	if rnd == nil {
		rnd = rand.Int64N
	}
	if total <= 0 {
		return draw[0].cand.ID
	}
	r := rnd(total)
	var acc int64
	for i := range draw {
		acc += weights[i]
		if r < acc {
			return draw[i].cand.ID
		}
	}
	return draw[len(draw)-1].cand.ID
}

// weightOf is the three-factor weight: credits ratio ×10, expiring ratio ×8,
// idle compensation (capped). maxCredits is the max over all routed
// candidates so the shortlist ordering and the draw share one scale.
func (p *wbAccountPool) weightOf(c wbPoolCandidate, maxCredits int64, now time.Time) float64 {
	w := 1.0
	if maxCredits > 0 {
		w += float64(c.Credits) / float64(maxCredits) * wbCreditsFactor
	}
	if c.Credits > 0 && c.CreditsExpiring > 0 {
		w += float64(c.CreditsExpiring) / float64(c.Credits) * wbExpiringFactor
	}
	if last, ok := p.lastUsed[c.ID]; !ok || last.IsZero() {
		w += p.pars.IdleWeightMax // never used → full idle credit
	} else {
		hours := now.Sub(last).Hours()
		idle := hours * p.pars.IdleWeightPerHour
		if idle > p.pars.IdleWeightMax {
			idle = p.pars.IdleWeightMax
		}
		if idle < 0 {
			idle = 0 // clock skew
		}
		w += idle
	}
	return w
}
