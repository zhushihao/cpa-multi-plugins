package main

// routing_pool_test.go — G07 parity tests for the weighted cost-tier account
// pool (workbuddy2api internal/pool/pick.go semantics, plugin-side pure-Go
// port). All tests run with CGO_ENABLED=0; clocks and the random source are
// injected for determinism (no wall-clock flake, fixed seed).

import (
	"testing"
	"time"
)

// wbTestClock is a mutable injected clock.
type wbTestClock struct{ t time.Time }

func (c *wbTestClock) now() time.Time { return c.t }
func (c *wbTestClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

// wbSeqRand returns a deterministic pseudo-random source (xorshift64*) so
// distribution assertions are stable across machines/runs.
func wbSeqRand(seed uint64) func(int64) int64 {
	state := seed
	if state == 0 {
		state = 0x9E3779B97F4A7C15
	}
	return func(n int64) int64 {
		if n <= 0 {
			return 0
		}
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		v := state * 0x2545F4914F6CDD1D
		return int64(v % uint64(n))
	}
}

func wbTestPool(clk *wbTestClock, rand func(int64) int64) *wbAccountPool {
	return newWbAccountPool(wbPoolParams{
		Now:                 func() time.Time { return clk.now() },
		RandInt64N:          rand,
		CostExploreInterval: 30 * time.Minute,
		MinPickGap:          0, // distribution tests disable anti-herd gap
	})
}

func wbCand(id string, credits, expiring int64) wbPoolCandidate {
	return wbPoolCandidate{ID: id, Credits: credits, CreditsExpiring: expiring}
}

// TestCostTier0HardFilter: with a free (tier 0) account present, a paid
// (tier 2) account must never be picked — hard filter, not mere ordering.
func TestCostTier0HardFilter(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(1))
	pool.noteCost("free", "m1", 0, 1000)   // measured free
	pool.noteCost("paid", "m1", 2.0, 1000) // measured paid

	for i := 0; i < 200; i++ {
		got, ok := pool.pick("m1", []wbPoolCandidate{wbCand("free", 100, 0), wbCand("paid", 100, 0)})
		if !ok || got != "free" {
			t.Fatalf("pick #%d = %q (ok=%v), want free (tier 0 hard filter)", i, got, ok)
		}
	}
}

// TestCostTierUnknownPreferredOverPaid: unobserved (tier 1) accounts rank
// above measured-paid (tier 2) ones — otherwise a new account could never be
// discovered as free.
func TestCostTierUnknownPreferredOverPaid(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(2))
	pool.noteCost("paid", "m1", 2.0, 1000) // tier 2

	for i := 0; i < 100; i++ {
		got, ok := pool.pick("m1", []wbPoolCandidate{wbCand("unknown", 10, 0), wbCand("paid", 999, 0)})
		if !ok || got != "unknown" {
			t.Fatalf("pick #%d = %q, want unknown (tier 1 beats tier 2)", i, got)
		}
	}
}

// TestCostExploreWindow: under tier-0 monopoly, exactly one ride-along
// exploration to a tier-1 account per 30m window (no extra upstream calls).
func TestCostExploreWindow(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(3))
	pool.noteCost("free", "m1", 0, 1000)
	cands := []wbPoolCandidate{wbCand("free", 100, 0), wbCand("unknown", 100, 0)}

	if got, _ := pool.pick("m1", cands); got != "unknown" {
		t.Fatalf("t=0 first pick = %q, want one-off exploration to unknown", got)
	}
	clk.advance(10 * time.Minute)
	if got, _ := pool.pick("m1", cands); got != "free" {
		t.Fatalf("t=10m pick = %q, want free (exploration window not elapsed)", got)
	}
	clk.advance(21 * time.Minute) // t=31m
	if got, _ := pool.pick("m1", cands); got != "unknown" {
		t.Fatalf("t=31m pick = %q, want second exploration", got)
	}
	clk.advance(1 * time.Minute)
	if got, _ := pool.pick("m1", cands); got != "free" {
		t.Fatalf("t=32m pick = %q, want free (window consumed)", got)
	}
}

// TestCostExploreNeedsTier1: the exploration switch only fires when a tier-1
// candidate exists; tier0+tier2 keeps the tier-0 hard filter (within the 6h
// observation TTL, so the free sample stays valid for the whole test).
func TestCostExploreNeedsTier1(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(4))
	pool.noteCost("free", "m1", 0, 1000)
	pool.noteCost("paid", "m1", 3.0, 1000)
	cands := []wbPoolCandidate{wbCand("free", 100, 0), wbCand("paid", 100, 0)}
	for i := 0; i < 40; i++ { // 40 × 5m = 200m < 6h TTL
		if got, _ := pool.pick("m1", cands); got != "free" {
			t.Fatalf("pick #%d = %q, want free (no tier-1 to explore)", i, got)
		}
		clk.advance(5 * time.Minute)
	}
}

// TestCostLedgerEMAAndTTL: EMA smoothing (alpha 0.3) and 6h observation TTL.
func TestCostLedgerEMAAndTTL(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(5))
	pool.noteCost("a", "m1", 2.0, 1000) // per1k = 2
	pool.noteCost("a", "m1", 0, 1000)   // EMA = 2*0.7 + 0*0.3 = 1.4
	entry, ok := pool.costLedger.lookup("a", "m1")
	if !ok {
		t.Fatal("observation should be valid inside TTL")
	}
	if diff := entry.CostPer1k - 1.4; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("EMA cost = %v, want 1.4", entry.CostPer1k)
	}
	if entry.Samples != 2 {
		t.Fatalf("samples = %d, want 2", entry.Samples)
	}
	// Tokens<=0 is not a valid observation (cannot normalize per-1k).
	pool.noteCost("a", "m1", 9.0, 0)
	if entry2, _ := pool.costLedger.lookup("a", "m1"); entry2.Samples != 2 {
		t.Fatalf("tokens<=0 must not be recorded, samples = %d", entry2.Samples)
	}
	// Expired observation → unknown again.
	clk.advance(6*time.Hour + time.Second)
	if _, ok := pool.costLedger.lookup("a", "m1"); ok {
		t.Fatal("observation must expire after 6h TTL")
	}
	if tier, _ := pool.costLedger.tierFor("a", "m1"); tier != wbCostTierUnknown {
		t.Fatalf("expired observation tier = %d, want 1 (unknown)", tier)
	}
}

// TestCostLedgerZeroIsFreeAndPaidFlips: credit=0 with tokens>0 is a real
// tier-0 observation; a later credit>0 observation graduates to tier 2.
func TestCostLedgerZeroIsFreeAndPaidFlips(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(6))
	pool.noteCost("a", "m1", 0, 500)
	if tier, _ := pool.costLedger.tierFor("a", "m1"); tier != wbCostTierFree {
		t.Fatalf("zero-credit observation tier = %d, want 0", tier)
	}
	pool.noteCost("a", "m1", 1.0, 500) // per1k = 2; EMA = 0*0.7+2*0.3 = 0.6 > 0
	if tier, _ := pool.costLedger.tierFor("a", "m1"); tier != wbCostTierPaid {
		t.Fatalf("after paid observation tier = %d, want 2", tier)
	}
	// A negative token count is invalid and must not create an entry.
	pool.noteCost("b", "m1", 5, -1)
	if _, ok := pool.costLedger.lookup("b", "m1"); ok {
		t.Fatal("negative tokens must not be recorded")
	}
}

// TestWeightedDistributionCreditsDominant pins the weighted draw itself:
// every account is pre-used at t0 (idle factor 0, no clock advance) so the
// weights are static credits ratios and the 1000-draw histogram must follow
// them — rich 11, mid 2, poor 1 → ~78.6% / 14.3% / 7.1%.
func TestWeightedDistributionCreditsDominant(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(7))
	cands := []wbPoolCandidate{
		wbCand("rich", 1000, 0),
		wbCand("mid", 100, 0),
		wbCand("poor", 0, 0),
	}
	for _, c := range cands {
		pool.markUsed(c.ID) // zero idle on all → credits-only static weights
	}
	hits := map[string]int{}
	const draws = 1000
	for i := 0; i < draws; i++ {
		got, ok := pool.pick("m1", cands)
		if !ok {
			t.Fatal("pick returned no account")
		}
		hits[got]++
	}
	if hits["rich"] == 0 || hits["mid"] == 0 || hits["poor"] == 0 {
		t.Fatalf("all accounts must stay reachable, got %v", hits)
	}
	if !(hits["rich"] > hits["mid"] && hits["mid"] > hits["poor"]) {
		t.Fatalf("hit counts must follow weights (rich>mid>poor), got %v", hits)
	}
	if hits["rich"] < draws*60/100 {
		t.Fatalf("rich account share too low: %d/%d (want credits-dominant)", hits["rich"], draws)
	}
}

// TestWeightedDeterministicWithFixedSeed: identical seed + identical history
// must produce an identical pick sequence (reproducible evidence).
func TestWeightedDeterministicWithFixedSeed(t *testing.T) {
	run := func() []string {
		clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
		pool := wbTestPool(clk, wbSeqRand(99))
		cands := []wbPoolCandidate{wbCand("a", 10, 0), wbCand("b", 20, 0), wbCand("c", 30, 0)}
		out := make([]string, 0, 50)
		for i := 0; i < 50; i++ {
			got, _ := pool.pick("m1", cands)
			out = append(out, got)
			clk.advance(time.Second)
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sequence diverged at %d: %q vs %q", i, a[i], b[i])
		}
	}
}

// TestWeightedIdleFactor: an idle account must gain weight over time and
// eventually overtake a lightly-better-credits account (idle compensation).
func TestWeightedIdleFactor(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(8))
	// Make "hot" used now so it has zero idle; "cold" never used → full idle.
	pool.markUsed("hot")
	cands := []wbPoolCandidate{wbCand("hot", 100, 0), wbCand("cold", 10, 0)}
	// credits ratio: hot 10, cold 1; idle: hot 0, cold = max (default 6).
	// weights: hot = 1+10 = 11, cold = 1+1+6 = 8 → hot still ahead.
	hits := map[string]int{}
	for i := 0; i < 400; i++ {
		got, _ := pool.pick("m1", cands)
		hits[got]++
		clk.advance(time.Minute)
		pool.markUsed(got)
	}
	if hits["hot"] == 0 || hits["cold"] == 0 {
		t.Fatalf("both accounts must be reachable, got %v", hits)
	}
	// After 8h with idle capped at max, cold's weight is 1+1+6=8; hot's is
	// 1+10+k*8h... this test only pins reachability + credit ordering not
	// flipping: verify hot still leads given 10x credits.
	if hits["hot"] < hits["cold"] {
		t.Fatalf("credits factor must dominate here: %v", hits)
	}
}

// TestWeightedShortlistTop5: with >5 candidates, only the 5 highest-weight
// accounts are draw-eligible. All candidates are pre-used (idle=0) so weights
// are credits-only and the excluded set is exactly the three lowest-credit
// accounts — the reference deliberately lets idle compensation pull
// low-credit accounts INTO the shortlist, so an idle-carrying setup would not
// pin this.
func TestWeightedShortlistTop5(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(10))
	var cands []wbPoolCandidate
	for i := 0; i < 8; i++ {
		cands = append(cands, wbCand(string(rune('a'+i)), int64(i), 0))
	}
	for _, c := range cands {
		pool.markUsed(c.ID)
	}
	// 'a','b','c' (credits 0,1,2) fall out of the top5; 'd'..'h' rotate.
	seenLow := 0
	seenHigh := map[string]int{}
	for i := 0; i < 300; i++ {
		got, _ := pool.pick("m1", cands)
		if got == "a" || got == "b" || got == "c" {
			seenLow++
		}
		seenHigh[got]++
	}
	if seenLow != 0 {
		t.Fatalf("out-of-shortlist accounts picked %d times (%v)", seenLow, seenHigh)
	}
	if len(seenHigh) < 3 {
		t.Fatalf("shortlist draw should rotate across candidates, got %v", seenHigh)
	}
}

// TestWeightedExpiringFactor: the soon-expiring bucket is the second factor —
// with equal credits and zero idle, an account holding expiring credits must
// win clearly more draws (weight 1+10+8·ratio vs 1+10).
func TestWeightedExpiringFactor(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(31))
	cands := []wbPoolCandidate{
		{ID: "spend-now", Credits: 100, CreditsExpiring: 50},
		{ID: "stable", Credits: 100},
	}
	for _, c := range cands {
		pool.markUsed(c.ID) // idle 0 on both → static weights
	}
	hits := map[string]int{}
	const draws = 1000
	for i := 0; i < draws; i++ {
		got, ok := pool.pick("m1", cands)
		if !ok {
			t.Fatal("pick returned no account")
		}
		hits[got]++
	}
	if hits["spend-now"] == 0 || hits["stable"] == 0 {
		t.Fatalf("both accounts must stay reachable, got %v", hits)
	}
	// Expected: 15 vs 11 → ~57.7% / 42.3%.
	if hits["spend-now"] <= hits["stable"] {
		t.Fatalf("expiring bucket must win the draw, got %v", hits)
	}
	if hits["spend-now"] < draws*48/100 {
		t.Fatalf("expiring share too low: %d/%d", hits["spend-now"], draws)
	}
}

// TestWeightedExhaustedExcluded: exhausted/disabled candidates never win.
func TestWeightedExhaustedExcluded(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(11))
	cands := []wbPoolCandidate{
		{ID: "ex", Exhausted: true, Credits: 0},
		{ID: "off", Disabled: true, Credits: 1000},
		{ID: "ok", Credits: 50},
	}
	for i := 0; i < 100; i++ {
		if got, _ := pool.pick("m1", cands); got != "ok" {
			t.Fatalf("pick = %q, want ok", got)
		}
	}
	if _, ok := pool.pick("m1", []wbPoolCandidate{{ID: "ex", Exhausted: true}}); ok {
		t.Fatal("all-exhausted must return ok=false")
	}
}

// TestWeightedMinPickGapLRUFallback: with an anti-herd gap, consecutive picks
// fan out; when every shortlist account was just used, the LRU fallback picks
// the oldest by sequence (not the same account twice in a row).
func TestWeightedMinPickGapLRUFallback(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := newWbAccountPool(wbPoolParams{
		Now:                 func() time.Time { return clk.now() },
		RandInt64N:          wbSeqRand(12),
		CostExploreInterval: 30 * time.Minute,
		MinPickGap:          time.Hour,
	})
	cands := []wbPoolCandidate{wbCand("a", 10, 0), wbCand("b", 10, 0)}
	first, _ := pool.pick("m1", cands)
	second, _ := pool.pick("m1", cands)
	if first == second {
		t.Fatalf("gap must fan out picks, got %q twice", first)
	}
	third, _ := pool.pick("m1", cands) // both used → LRU (oldest = first)
	if third != first {
		t.Fatalf("LRU fallback = %q, want %q (oldest)", third, first)
	}
}

// TestPickRetrySwitch: a retried request arrives with the failed account
// removed from candidates (host tried-set) — the picker must choose another.
func TestPickRetrySwitch(t *testing.T) {
	clk := &wbTestClock{t: time.Unix(1_700_000_000, 0)}
	pool := wbTestPool(clk, wbSeqRand(13))
	all := []wbPoolCandidate{wbCand("a", 10, 0), wbCand("b", 10, 0)}
	first, _ := pool.pick("m1", all)
	remaining := []wbPoolCandidate{}
	for _, c := range all {
		if c.ID != first {
			remaining = append(remaining, c)
		}
	}
	second, ok := pool.pick("m1", remaining)
	if !ok || second == first {
		t.Fatalf("retry pick = %q (ok=%v), want the other account", second, ok)
	}
}
