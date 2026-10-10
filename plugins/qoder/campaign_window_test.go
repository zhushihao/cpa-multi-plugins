// campaign_window_test.go pins the Beijing-time campaign window contract:
// slots at 10:05/10:35/21:05 Asia/Shanghai (never host-local hours), the
// bounded per-process startup recovery decision, the bounded credit re-check
// time, the in-memory per-account/campaign/window dedup ledger, the honest
// credit-delta rule, the grace decision and the read-only panel projection.
//
// These tests are pure logic: no HTTP, no host RPC, runnable with
// CGO_ENABLED=0 (see abi_nocgo.go).
package main

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// bj builds a Beijing wall-clock instant. Beijing is UTC+8 year-round (no DST
// since 1991), so the fixed zone the implementation uses is exact.
func bj(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, campaignWindowZone)
}

// withCampaignJitter pins the first-slot jitter seam to a deterministic
// offset for the duration of a test (production draws randomly).
func withCampaignJitter(t *testing.T, minutes int) {
	t.Helper()
	orig := campaignJitterFor
	campaignJitterFor = func() int { return minutes }
	t.Cleanup(func() { campaignJitterFor = orig })
}

func resetCampaignLedger() {
	campaignLedger.mu.Lock()
	campaignLedger.m = make(map[campaignClaimKey]campaignClaimRecord)
	campaignLedger.mu.Unlock()
}

func resetCampaignPassWindow() { markCampaignPassWindow("") }

// TestCampaignWindowIDIsBeijingCalendarDay: the dedup window flips at Beijing
// midnight, not at the host's or UTC's midnight.
func TestCampaignWindowIDIsBeijingCalendarDay(t *testing.T) {
	if got := campaignWindowID(time.Date(2026, 9, 29, 16, 30, 0, 0, time.UTC)); got != "2026-09-30" {
		t.Fatalf("window(2026-09-29 16:30Z) = %q, want 2026-09-30 (Beijing 00:30)", got)
	}
	if got := campaignWindowID(time.Date(2026, 9, 29, 15, 59, 0, 0, time.UTC)); got != "2026-09-29" {
		t.Fatalf("window(2026-09-29 15:59Z) = %q, want 2026-09-29 (Beijing 23:59)", got)
	}
}

// TestNextCampaignSlot: strict next slot in Beijing wall-clock, across zones
// and across midnight. The first slot carries a random 0–15 min offset, so
// "want" for first-slot cases is a WINDOW predicate, not an exact minute.
func TestNextCampaignSlot(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"between opening slots", bj(2026, 9, 29, 10, 20), bj(2026, 9, 29, 10, 35)},
		{"after second slot", bj(2026, 9, 29, 10, 35), bj(2026, 9, 29, 21, 5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextCampaignSlot(tc.now); !got.Equal(tc.want) {
				t.Fatalf("nextCampaignSlot(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
	// First-slot cases carry a random 0–15 min offset: assert the WINDOW.
	// "exactly on a slot" (10:05): the jittered slot may still lie ahead.
	t.Run("on the base minute stays in window", func(t *testing.T) {
		got := nextCampaignSlot(bj(2026, 9, 29, 10, 5))
		if got.Before(bj(2026, 9, 29, 10, 5)) || got.After(bj(2026, 9, 29, 10, 35)) {
			t.Fatalf("nextCampaignSlot(10:05) = %s, want inside [10:05, 10:35)", got)
		}
	})
	for _, tc := range []struct{ name string; now time.Time; lo, hi time.Time }{
		{"before first slot", bj(2026, 9, 29, 9, 0), bj(2026, 9, 29, 10, 5), bj(2026, 9, 29, 10, 20)},
		{"after evening slot", bj(2026, 9, 29, 21, 5), bj(2026, 9, 30, 10, 5), bj(2026, 9, 30, 10, 20)},
		{"late night rolls to next day", bj(2026, 9, 29, 23, 59), bj(2026, 9, 30, 10, 5), bj(2026, 9, 30, 10, 20)},
		// 2026-09-29 02:04 UTC is 10:04 Beijing — the slot is decided in
		// Beijing time regardless of the caller's zone.
		{"non-Beijing input zone", time.Date(2026, 9, 29, 2, 4, 0, 0, time.UTC), bj(2026, 9, 29, 10, 5), bj(2026, 9, 29, 10, 20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextCampaignSlot(tc.now)
			if got.Before(tc.lo) || got.After(tc.hi) {
				t.Fatalf("nextCampaignSlot(%s) = %s, want inside %s–%s", tc.now, got, tc.lo.Format("15:04"), tc.hi.Format("15:04"))
			}
		})
	}
}

// TestCampaignRecoveryDue: a (re)start inside the current Beijing window
// catches up once; before the day's first slot nothing is due; a window that
// already had a pass is not replayed. Jitter pinned to 0 so the first slot is
// exactly 10:05.
func TestCampaignRecoveryDue(t *testing.T) {
	withCampaignJitter(t, 0)
	if _, due := campaignRecoveryDue(bj(2026, 9, 29, 9, 59), ""); due {
		t.Fatal("09:59 Beijing is before the 10:05 slot — recovery must not fire")
	}
	if _, due := campaignRecoveryDue(bj(2026, 9, 29, 10, 4), ""); due {
		t.Fatal("10:04 Beijing is before the 10:05 slot — recovery must not fire")
	}
	slot, due := campaignRecoveryDue(bj(2026, 9, 29, 10, 5), "")
	if !due || !slot.Equal(bj(2026, 9, 29, 10, 5)) {
		t.Fatalf("10:05 recovery = (%s, %v), want the 10:05 slot due", slot, due)
	}
	slot, due = campaignRecoveryDue(bj(2026, 9, 29, 10, 36), "")
	if !due || !slot.Equal(bj(2026, 9, 29, 10, 35)) {
		t.Fatalf("10:36 recovery = (%s, %v), want the 10:35 slot due", slot, due)
	}
	slot, due = campaignRecoveryDue(bj(2026, 9, 29, 23, 0), "")
	if !due || !slot.Equal(bj(2026, 9, 29, 21, 5)) {
		t.Fatalf("23:00 recovery = (%s, %v), want the 21:05 slot due", slot, due)
	}
	if _, due := campaignRecoveryDue(bj(2026, 9, 29, 23, 59), "2026-09-29"); due {
		t.Fatal("same window already covered — recovery is bounded to one catch-up per (process, window)")
	}
	if _, due := campaignRecoveryDue(bj(2026, 9, 30, 9, 0), "2026-09-29"); due {
		t.Fatal("before the next day's first slot nothing is due")
	}
	if _, due := campaignRecoveryDue(bj(2026, 9, 30, 10, 6), "2026-09-29"); !due {
		t.Fatal("a new window after its first slot is due again")
	}
}

// TestRunStartupRecoveryOnce: the scheduler glue runs the catch-up at most
// once per (process, window) and only when due. The marker is process memory
// only — cross-process dedup is upstream idempotency, not this state (I1).
func TestRunStartupRecoveryOnce(t *testing.T) {
	withCampaignJitter(t, 0)
	resetCampaignPassWindow()
	ran := 0
	if !runStartupRecoveryOnce(bj(2026, 9, 29, 10, 6), func() { ran++ }) {
		t.Fatal("startup inside the window must run the bounded catch-up")
	}
	if ran != 1 {
		t.Fatalf("catch-up ran %d times, want 1", ran)
	}
	if runStartupRecoveryOnce(bj(2026, 9, 29, 23, 0), func() { ran++ }) {
		t.Fatal("a second catch-up in the same window must be suppressed")
	}
	if ran != 1 {
		t.Fatalf("catch-up ran %d times after the second start, want 1", ran)
	}
	if runStartupRecoveryOnce(bj(2026, 9, 30, 9, 0), func() { ran++ }) {
		t.Fatal("before the next window's first slot nothing may run")
	}
	if !runStartupRecoveryOnce(bj(2026, 9, 30, 10, 5), func() { ran++ }) {
		t.Fatal("the next window must be recoverable again")
	}
	if ran != 2 {
		t.Fatalf("catch-up ran %d times, want 2 across two windows", ran)
	}
}

// TestCampaignCreditRecheckAt: the single bounded re-check lands at
// min(claimed+10m, next slot) — never unbounded retries. Jitter pinned to 0
// so the 10:35 recovery slot is deterministic for the claimed-at-10:26 case.
func TestCampaignCreditRecheckAt(t *testing.T) {
	withCampaignJitter(t, 0)
	if got := campaignCreditRecheckAt(bj(2026, 9, 29, 10, 5)); !got.Equal(bj(2026, 9, 29, 10, 15)) {
		t.Fatalf("recheck(10:05) = %s, want 10:15 (10-minute delay)", got)
	}
	if got := campaignCreditRecheckAt(bj(2026, 9, 29, 10, 30)); !got.Equal(bj(2026, 9, 29, 10, 35)) {
		t.Fatalf("recheck(10:30) = %s, want 10:35 (next slot wins)", got)
	}
	if got := campaignCreditRecheckAt(bj(2026, 9, 29, 22, 0)); !got.Equal(bj(2026, 9, 29, 22, 10)) {
		t.Fatalf("recheck(22:00) = %s, want 22:10", got)
	}
}

// TestCampaignClaimDedupKey: the ledger dedups per account + campaign +
// window, and nothing else.
func TestCampaignClaimDedupKey(t *testing.T) {
	resetCampaignLedger()
	defer resetCampaignLedger()

	key := campaignClaimKey{AuthID: "acct-1", CampaignID: "camp-1", Window: "2026-09-29"}
	if _, ok := campaignLookupClaim(key); ok {
		t.Fatal("empty ledger must miss")
	}
	if _, first := campaignMarkClaimed(key, campaignClaimRecord{ClaimedAt: bj(2026, 9, 29, 10, 5), Expected: 100}); !first {
		t.Fatal("first mark must win")
	}
	if _, first := campaignMarkClaimed(key, campaignClaimRecord{}); first {
		t.Fatal("second mark of the same key must not be first")
	}
	rec, ok := campaignLookupClaim(key)
	if !ok || rec.Expected != 100 {
		t.Fatalf("lookup = (%+v, %v), want the first record kept", rec, ok)
	}
	for _, other := range []campaignClaimKey{
		{AuthID: "acct-2", CampaignID: "camp-1", Window: "2026-09-29"},
		{AuthID: "acct-1", CampaignID: "camp-2", Window: "2026-09-29"},
		{AuthID: "acct-1", CampaignID: "camp-1", Window: "2026-09-30"},
	} {
		if _, ok := campaignLookupClaim(other); ok {
			t.Fatalf("dedup leaked into %+v — key must be account+campaign+window", other)
		}
	}
}

// TestCampaignClaimDedupConcurrentMarks: concurrent marks of one key elect
// exactly one first claim (the scheduler and the manual button can race).
func TestCampaignClaimDedupConcurrentMarks(t *testing.T) {
	resetCampaignLedger()
	defer resetCampaignLedger()
	key := campaignClaimKey{AuthID: "acct-c", CampaignID: "camp-c", Window: "2026-09-29"}

	const n = 32
	var wg sync.WaitGroup
	firsts := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, first := campaignMarkClaimed(key, campaignClaimRecord{Expected: 100}); first {
				firsts <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(firsts)
	if got := len(firsts); got != 1 {
		t.Fatalf("first marks = %d, want exactly 1", got)
	}
}

// TestCampaignCreditConfirmedRules pins the honest delta rule: a credit grant
// is only confirmed when the post-claim readback shows the expected increase
// over the pre-claim snapshot. Unreadable snapshots stay unknown — never
// upgraded to success.
func TestCampaignCreditConfirmedRules(t *testing.T) {
	rec := campaignClaimRecord{Expected: 100, BeforeRemain: 50, HasBefore: true}
	if c, known := campaignCreditConfirmed(rec, &creditsSummary{TotalRemain: 150}); !c || !known {
		t.Fatalf("150 >= 50+100 must confirm (got c=%v known=%v)", c, known)
	}
	if c, known := campaignCreditConfirmed(rec, &creditsSummary{TotalRemain: 149}); c || !known {
		t.Fatalf("149 < 50+100 must stay unconfirmed but known (got c=%v known=%v)", c, known)
	}
	if _, known := campaignCreditConfirmed(rec, nil); known {
		t.Fatal("missing readback must stay unknown")
	}
	if _, known := campaignCreditConfirmed(campaignClaimRecord{Expected: 100}, &creditsSummary{TotalRemain: 999}); known {
		t.Fatal("missing pre-claim snapshot must stay unknown — an absolute balance proves no delta")
	}
	// Non-credit benefits have no delta to observe: the CLAIMED status is the
	// evidence.
	if c, known := campaignCreditConfirmed(campaignClaimRecord{}, &creditsSummary{}); !c || !known {
		t.Fatalf("non-credit claim must be confirmed by evidence (got c=%v known=%v)", c, known)
	}
}

// TestCampaignApplyCreditReadbackBounded: the recheck that still sees no grant
// marks the claim 待核 (Unconfirmed), and a later observed grant may still
// confirm it — but nothing here ever reports success without evidence.
func TestCampaignApplyCreditReadbackBounded(t *testing.T) {
	resetCampaignLedger()
	defer resetCampaignLedger()
	key := campaignClaimKey{AuthID: "acct-r", CampaignID: "camp-r", Window: "2026-09-29"}
	claimedAt := bj(2026, 9, 29, 10, 5)
	campaignMarkClaimed(key, campaignClaimRecord{
		ClaimedAt: claimedAt, Expected: 100, BeforeRemain: 50, HasBefore: true,
		NextCheckAt: campaignCreditRecheckAt(claimedAt),
	})

	// Immediate readback (before the recheck deadline): stays pending.
	rec, confirmed := campaignApplyCreditReadback(key, &creditsSummary{TotalRemain: 50}, nil, claimedAt.Add(time.Second))
	if confirmed || rec.Unconfirmed {
		t.Fatalf("immediate miss must stay pending (confirmed=%v unconfirmed=%v)", confirmed, rec.Unconfirmed)
	}
	// The bounded recheck runs and still misses: 待核, not success.
	rec, confirmed = campaignApplyCreditReadback(key, &creditsSummary{TotalRemain: 50}, nil, rec.NextCheckAt)
	if confirmed || !rec.Unconfirmed {
		t.Fatalf("expired recheck must mark 待核 (confirmed=%v unconfirmed=%v)", confirmed, rec.Unconfirmed)
	}
	// Evidence arriving later still wins (honest, not a fake).
	rec, confirmed = campaignApplyCreditReadback(key, &creditsSummary{TotalRemain: 150}, nil, rec.NextCheckAt.Add(time.Minute))
	if !confirmed || !rec.Confirmed {
		t.Fatal("an observed grant after 待核 must still confirm")
	}
}

// TestCampaignStateGrantsLifecycleGrace: lifecycle grace is granted only on
// claim evidence — including pending_credit (CLAIMED with the credit pool
// still lagging: the S1 regression from the 2026-09-29 review) — and never for
// error / none / unconfirmed.
func TestCampaignStateGrantsLifecycleGrace(t *testing.T) {
	for _, state := range []string{campaignStateClaimed, campaignStateAlready, campaignStatePendingCredit} {
		if !campaignStateGrantsLifecycleGrace(state) {
			t.Fatalf("state %q must grant lifecycle grace (claim evidence)", state)
		}
	}
	for _, state := range []string{campaignStateError, campaignStateNone, campaignStateUnconfirmed} {
		if campaignStateGrantsLifecycleGrace(state) {
			t.Fatalf("state %q must NOT grant lifecycle grace (no claim evidence)", state)
		}
	}
}

// TestCampaignClaimViewProjectsLedgerStates: the read-only panel projection
// exposes the honest evidence states, the confirmed/unconfirmed flags, the
// last readback error and the latest record across windows (I2/m2).
func TestCampaignClaimViewProjectsLedgerStates(t *testing.T) {
	resetCampaignLedger()
	defer resetCampaignLedger()

	// pending_credit: CLAIMED recorded, bounded re-check still pending.
	pendingKey := campaignClaimKey{AuthID: "acct-view", CampaignID: "camp-1", Window: "2026-09-28"}
	campaignMarkClaimed(pendingKey, campaignClaimRecord{
		ClaimedAt:   bj(2026, 9, 28, 10, 5),
		Expected:    100,
		NextCheckAt: bj(2026, 9, 28, 10, 15),
	})
	v := campaignClaimViewFor("acct-view")
	if v == nil || v.State != campaignStatePendingCredit || v.Confirmed || v.Unconfirmed {
		t.Fatalf("pending view = %+v", v)
	}
	if v.Window != "2026-09-28" || v.Expected != 100 || v.NextCheckAt == "" {
		t.Fatalf("pending view lost ledger fields: %+v", v)
	}
	// A newer record (next window, confirmed) wins "latest".
	confirmedKey := campaignClaimKey{AuthID: "acct-view", CampaignID: "camp-1", Window: "2026-09-29"}
	campaignMarkClaimed(confirmedKey, campaignClaimRecord{
		ClaimedAt:    bj(2026, 9, 29, 10, 5),
		Expected:     100,
		BeforeRemain: 50,
		HasBefore:    true,
		Confirmed:    true,
		RecheckedAt:  bj(2026, 9, 29, 10, 5),
	})
	v = campaignClaimViewFor("acct-view")
	if v == nil || v.State != campaignStateClaimed || !v.Confirmed || v.Unconfirmed {
		t.Fatalf("confirmed view = %+v", v)
	}
	if v.Window != "2026-09-29" || v.ClaimedAt == "" {
		t.Fatalf("latest-window selection wrong: %+v", v)
	}

	// 待核: the bounded re-check ran out without credit evidence; the readback
	// failure reason is exposed, never hidden.
	unconfKey := campaignClaimKey{AuthID: "acct-view-unconf", CampaignID: "camp-1", Window: "2026-09-29"}
	campaignMarkClaimed(unconfKey, campaignClaimRecord{
		ClaimedAt:    bj(2026, 9, 29, 10, 5),
		Expected:     100,
		BeforeRemain: 50,
		HasBefore:    true,
		NextCheckAt:  bj(2026, 9, 29, 10, 15),
	})
	campaignApplyCreditReadback(unconfKey, &creditsSummary{TotalRemain: 50}, errors.New("readback boom"), bj(2026, 9, 29, 10, 15))
	v = campaignClaimViewFor("acct-view-unconf")
	if v == nil || v.State != campaignStateUnconfirmed || !v.Unconfirmed || v.Confirmed {
		t.Fatalf("待核 view = %+v", v)
	}
	if v.LastError == "" {
		t.Fatalf("readback failure must be exposed: %+v", v)
	}

	// Replayed -> already (no new grant).
	replayKey := campaignClaimKey{AuthID: "acct-view-replay", CampaignID: "camp-1", Window: "2026-09-29"}
	campaignMarkClaimed(replayKey, campaignClaimRecord{ClaimedAt: bj(2026, 9, 29, 10, 5), Replayed: true})
	if v := campaignClaimViewFor("acct-view-replay"); v == nil || v.State != campaignStateAlready || !v.Replayed {
		t.Fatalf("replay view = %+v", v)
	}

	if v := campaignClaimViewFor("acct-no-record"); v != nil {
		t.Fatalf("account without a record must project nil, got %+v", v)
	}
}

// TestCampaignClaimViewJSONContract pins the panel-visible JSON keys the
// embedded panel / API consumers rely on (I2/m2).
func TestCampaignClaimViewJSONContract(t *testing.T) {
	resetCampaignLedger()
	defer resetCampaignLedger()
	campaignMarkClaimed(
		campaignClaimKey{AuthID: "acct-json", CampaignID: "camp-1", Window: "2026-09-29"},
		campaignClaimRecord{ClaimedAt: bj(2026, 9, 29, 10, 5), Expected: 100,
			NextCheckAt: bj(2026, 9, 29, 10, 15)},
	)
	acct := wbAccount{AuthID: "acct-json", CampaignClaim: campaignClaimViewFor("acct-json")}
	b, err := json.Marshal(acct)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	claim, ok := m["campaign_claim"].(map[string]any)
	if !ok {
		t.Fatalf("campaign_claim missing from the dashboard row: %s", b)
	}
	for _, key := range []string{"window", "state", "confirmed", "unconfirmed", "claimed_at", "next_check_at"} {
		if _, ok := claim[key]; !ok {
			t.Fatalf("campaign_claim.%s missing from the panel contract: %s", key, b)
		}
	}
	if claim["state"] != campaignStatePendingCredit || claim["confirmed"] != false || claim["unconfirmed"] != false {
		t.Fatalf("projection wrong: %s", b)
	}
}
