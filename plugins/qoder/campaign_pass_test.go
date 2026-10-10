// campaign_pass_test.go pins the scheduled campaign pass end to end at the
// HTTP boundary (campaigns list → claim POST → quota readback) and proves the
// honest-state rules: empty campaign lists, upstream errors and uncredited
// claims can never surface as success, and a per-account/campaign/window
// claim is posted at most once per process even under concurrency.
//
// The quota readback is redirected through campaignCreditsReadOverride — the
// production reader (fetchUserResource) hardcodes the regional upstream base
// and so cannot be pointed at httptest by the billingBaseOverride seam.
//
// The bounded credit re-check is armed through campaignRecheckSchedule, which
// tests replace with the manualRechecks driver: no wall-clock timer, no
// goroutine outliving a test, and no post-cleanup upstream call (S2, 2026-09-29
// review). Tests never let a real timer fire.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// quotaProbe serves a deterministic sequence of remaining-credit values (the
// last value sticks) and counts readbacks.
type quotaProbe struct {
	mu      sync.Mutex
	remains []int64
	calls   int
}

func (p *quotaProbe) read(*storedAuth) (*creditsSummary, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.calls
	if i >= len(p.remains) {
		i = len(p.remains) - 1
	}
	p.calls++
	return &creditsSummary{TotalRemain: p.remains[i]}, nil
}

func (p *quotaProbe) callsN() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func withQuotaReadback(t *testing.T, remains ...int64) *quotaProbe {
	t.Helper()
	p := &quotaProbe{remains: remains}
	prev := campaignCreditsReadOverride
	campaignCreditsReadOverride = p.read
	t.Cleanup(func() { campaignCreditsReadOverride = prev })
	return p
}

func withRecheckDelay(t *testing.T, d time.Duration) {
	t.Helper()
	// The first campaign slot carries a random 0–15 min offset; pin it to 0
	// so every slot arithmetic in these tests is deterministic.
	withCampaignJitter(t, 0)
	prev := campaignCreditRecheckDelay
	campaignCreditRecheckDelay = d
	t.Cleanup(func() { campaignCreditRecheckDelay = prev })
}

// armedRecheck is one callback captured by the manual driver.
type armedRecheck struct {
	at  time.Time
	run func(firedAt time.Time)
}

// manualRechecks replaces campaignRecheckSchedule with a recorder: nothing
// fires until the test calls runAll with the timestamp it chooses. This makes
// the bounded credit re-check deterministic — no wall-clock timer and no
// goroutine can outlive the test, and a missed assertion cannot happen because
// a real timer fired early or after cleanup (S2 regression, 2026-09-29 review:
// the old fixed-clock bj(2026,9,29,10,5) tests armed real AfterFunc timers once
// wall-clock time passed the re-check deadline).
type manualRechecks struct {
	mu    sync.Mutex
	armed []armedRecheck
}

func withManualRechecks(t *testing.T) *manualRechecks {
	t.Helper()
	m := &manualRechecks{}
	prev := campaignRecheckSchedule
	campaignRecheckSchedule = func(at time.Time, run func(time.Time)) {
		m.mu.Lock()
		m.armed = append(m.armed, armedRecheck{at: at, run: run})
		m.mu.Unlock()
	}
	t.Cleanup(func() { campaignRecheckSchedule = prev })
	return m
}

func (m *manualRechecks) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.armed)
}

func (m *manualRechecks) first() (armedRecheck, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.armed) == 0 {
		return armedRecheck{}, false
	}
	return m.armed[0], true
}

// runAll fires every armed callback with the given firing time and returns how
// many ran.
func (m *manualRechecks) runAll(firedAt time.Time) int {
	m.mu.Lock()
	list := m.armed
	m.armed = nil
	m.mu.Unlock()
	for _, a := range list {
		a.run(firedAt)
	}
	return len(list)
}

// claimableServer answers the campaigns list with one always-claimable
// campaign (simulating upstream status lag after an earlier claim) and serves
// the claim POST with claimBody. Returns the claim-hit counter.
func claimableServer(t *testing.T, claimBody string) *int32 {
	t.Helper()
	var hits int32
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, campaignList("CLAIMABLE")
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			atomic.AddInt32(&hits, 1)
			return http.StatusOK, claimBody
		},
	})
	return &hits
}

func hitCount(p *int32) int { return int(atomic.LoadInt32(p)) }

// TestCampaignPassEmptyIsNoneNotSuccess: an empty campaign list is a no-op —
// no claim POST, no credit readback, and the result is not a success state.
func TestCampaignPassEmptyIsNoneNotSuccess(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	rechecks := withManualRechecks(t)
	probe := withQuotaReadback(t, 100)
	hits := int32(0)
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, `{"showCampaign":false,"campaigns":[]}`
		},
	})

	res := campaignPassForAccount(cnAuth(), "acct-none", "idx-none", bj(2026, 9, 29, 10, 5))
	if res.State != campaignStateNone {
		t.Fatalf("state = %q, want %q", res.State, campaignStateNone)
	}
	if res.ClaimMade || res.Confirmed {
		t.Fatalf("empty list must not claim or confirm: %+v", res)
	}
	if hits != 0 {
		t.Fatalf("claim POSTs = %d, want 0", hits)
	}
	if probe.callsN() != 0 {
		t.Fatalf("quota readbacks = %d, want 0 (nothing was claimed)", probe.callsN())
	}
	if res.Summary == nil {
		t.Fatal("summary snapshot must be attached for the panel cache")
	}
	if rechecks.count() != 0 {
		t.Fatalf("armed re-checks = %d, want 0 (nothing was claimed)", rechecks.count())
	}
}

// TestCampaignPassStatusErrorIsError: a failed campaigns fetch is an error
// state — no claim, no success, and the message carries the failure.
func TestCampaignPassStatusErrorIsError(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	withManualRechecks(t)
	withQuotaReadback(t, 100)
	hits := int32(0)
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusInternalServerError, `{"err":"boom"}`
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			atomic.AddInt32(&hits, 1)
			return http.StatusOK, `{"status":"CLAIMED"}`
		},
	})

	res := campaignPassForAccount(cnAuth(), "acct-err", "idx-err", bj(2026, 9, 29, 10, 5))
	if res.State != campaignStateError {
		t.Fatalf("state = %q, want %q (%+v)", res.State, campaignStateError, res)
	}
	if res.Error == "" {
		t.Fatal("error state must carry the upstream failure")
	}
	if res.ClaimMade || res.Confirmed {
		t.Fatalf("errored status must not claim or confirm: %+v", res)
	}
	if hits != 0 {
		t.Fatalf("claim POSTs = %d, want 0", hits)
	}
}

// TestCampaignPassClaimConfirmedByCreditDelta: CLAIMED plus the observed +100
// credit delta is the only path that reports a confirmed claim.
func TestCampaignPassClaimConfirmedByCreditDelta(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	rechecks := withManualRechecks(t)
	probe := withQuotaReadback(t, 100, 200)
	hits := claimableServer(t, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)

	res := campaignPassForAccount(cnAuth(), "acct-ok", "idx-ok", bj(2026, 9, 29, 10, 5))
	if res.State != campaignStateClaimed || !res.Confirmed {
		t.Fatalf("state = %q confirmed=%v, want claimed/confirmed (%+v)", res.State, res.Confirmed, res)
	}
	if !res.ClaimMade {
		t.Fatal("fresh CLAIMED must be reported as a claim made by this pass")
	}
	if res.ExpectedCredit != 100 {
		t.Fatalf("expected credit = %d, want 100", res.ExpectedCredit)
	}
	if res.CreditsBefore == nil || res.CreditsBefore.TotalRemain != 100 || res.CreditsAfter == nil || res.CreditsAfter.TotalRemain != 200 {
		t.Fatalf("credit snapshots wrong: before=%v after=%v", res.CreditsBefore, res.CreditsAfter)
	}
	if hitCount(hits) != 1 {
		t.Fatalf("claim POSTs = %d, want 1", hitCount(hits))
	}
	if probe.callsN() != 2 {
		t.Fatalf("quota readbacks = %d, want 2 (before + after)", probe.callsN())
	}
	rec, ok := campaignLookupClaim(campaignClaimKey{AuthID: "acct-ok", CampaignID: "camp-cn-daily", Window: "2026-09-29"})
	if !ok || !rec.Confirmed {
		t.Fatalf("ledger must record the confirmed claim: %+v ok=%v", rec, ok)
	}
	if rechecks.count() != 0 {
		t.Fatalf("armed re-checks = %d, want 0 (already confirmed)", rechecks.count())
	}
}

// TestCampaignPassUncreditedClaimStaysPending: CLAIMED with no observed credit
// increase is pending_credit — never confirmed, and one bounded re-check is
// armed at the deterministic min(claimed+delay, next slot) instant. The manual
// driver keeps this independent of wall-clock time (S2).
func TestCampaignPassUncreditedClaimStaysPending(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	rechecks := withManualRechecks(t)
	withQuotaReadback(t, 100, 100)
	hits := claimableServer(t, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)

	now := bj(2026, 9, 29, 10, 5)
	res := campaignPassForAccount(cnAuth(), "acct-pending", "idx-pending", now)
	if res.State != campaignStatePendingCredit || res.Confirmed {
		t.Fatalf("state = %q confirmed=%v, want pending_credit/unconfirmed (%+v)", res.State, res.Confirmed, res)
	}
	if !res.ClaimMade {
		t.Fatal("the claim did reach upstream and must be recorded as attempted/made")
	}
	rec, ok := campaignLookupClaim(campaignClaimKey{AuthID: "acct-pending", CampaignID: "camp-cn-daily", Window: "2026-09-29"})
	if !ok {
		t.Fatal("ledger must record the pending claim")
	}
	if rec.Confirmed || rec.Unconfirmed {
		t.Fatalf("pending claim must not be confirmed nor 待核 yet: %+v", rec)
	}
	if rec.NextCheckAt.IsZero() {
		t.Fatal("a bounded re-check must be scheduled")
	}
	if a, ok := rechecks.first(); !ok || !a.at.Equal(bj(2026, 9, 29, 10, 35)) {
		t.Fatalf("armed re-check = (%v, %v), want exactly one at 10:35 Beijing", a.at, ok)
	}
	if rechecks.count() != 1 {
		t.Fatalf("armed re-checks = %d, want 1", rechecks.count())
	}
	if hitCount(hits) != 1 {
		t.Fatalf("claim POSTs = %d, want 1", hitCount(hits))
	}
}

// TestCampaignPassReplayIsAlready: an idempotent upstream replay is a no-op,
// not a fresh success and not an error.
func TestCampaignPassReplayIsAlready(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	rechecks := withManualRechecks(t)
	withQuotaReadback(t, 100)
	claimableServer(t, `{"status":"CLAIMED","replayed":true}`)

	res := campaignPassForAccount(cnAuth(), "acct-replay", "idx-replay", bj(2026, 9, 29, 10, 5))
	if res.State != campaignStateAlready {
		t.Fatalf("state = %q, want %q (%+v)", res.State, campaignStateAlready, res)
	}
	if res.ClaimMade || res.Confirmed {
		t.Fatalf("a replay makes no new grant: %+v", res)
	}
	if rechecks.count() != 0 {
		t.Fatalf("armed re-checks = %d, want 0 (a replay has no new grant to confirm)", rechecks.count())
	}
}

// TestCampaignPassUpstreamUnconfirmedIsNotClaimed: an HTTP 200 without a
// CLAIMED status is unconfirmed — no ledger entry, so the next slot may retry,
// but this pass never reports success.
func TestCampaignPassUpstreamUnconfirmedIsNotClaimed(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	withManualRechecks(t)
	withQuotaReadback(t, 100)
	hits := claimableServer(t, `{"code":0,"msg":"ok"}`)

	res := campaignPassForAccount(cnAuth(), "acct-unconf", "idx-unconf", bj(2026, 9, 29, 10, 5))
	if res.State != campaignStateUnconfirmed || res.Confirmed || res.ClaimMade {
		t.Fatalf("state = %q confirmed=%v claim_made=%v, want unconfirmed/no-success (%+v)", res.State, res.Confirmed, res.ClaimMade, res)
	}
	if hitCount(hits) != 1 {
		t.Fatalf("claim POSTs = %d, want 1", hitCount(hits))
	}
	if _, ok := campaignLookupClaim(campaignClaimKey{AuthID: "acct-unconf", CampaignID: "camp-cn-daily", Window: "2026-09-29"}); ok {
		t.Fatal("an unconfirmed claim must not be recorded as claimed")
	}
}

// TestCampaignPassDedupsWithinWindowNotAcross: one POST per account+campaign+
// window; the next Beijing day is a new window.
func TestCampaignPassDedupsWithinWindowNotAcross(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	withManualRechecks(t)
	withQuotaReadback(t, 100, 200, 200, 300)
	hits := claimableServer(t, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)

	first := campaignPassForAccount(cnAuth(), "acct-dup", "idx-dup", bj(2026, 9, 29, 10, 5))
	if first.State != campaignStateClaimed {
		t.Fatalf("first pass = %q, want claimed (%+v)", first.State, first)
	}
	second := campaignPassForAccount(cnAuth(), "acct-dup", "idx-dup", bj(2026, 9, 29, 10, 35))
	if second.State != campaignStateAlready {
		t.Fatalf("second pass same window = %q, want already — no re-claim", second.State)
	}
	if hitCount(hits) != 1 {
		t.Fatalf("claim POSTs after two same-window passes = %d, want 1", hitCount(hits))
	}
	third := campaignPassForAccount(cnAuth(), "acct-dup", "idx-dup", bj(2026, 9, 30, 10, 5))
	if third.State != campaignStateClaimed {
		t.Fatalf("next window = %q, want claimed again (%+v)", third.State, third)
	}
	if hitCount(hits) != 2 {
		t.Fatalf("claim POSTs across two windows = %d, want 2", hitCount(hits))
	}
}

// TestCampaignPassConcurrentSameAccountClaimsOnce: the scheduler and a
// recovery/catch-up run can race — the per-account mutex plus the ledger must
// still produce exactly one claim POST.
func TestCampaignPassConcurrentSameAccountClaimsOnce(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	withManualRechecks(t)
	withQuotaReadback(t, 100, 200)
	var hits int32
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, campaignList("CLAIMABLE")
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			atomic.AddInt32(&hits, 1)
			time.Sleep(60 * time.Millisecond) // widen the race window
			return http.StatusOK, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`
		},
	})

	now := bj(2026, 9, 29, 10, 5)
	results := make([]campaignPassResult, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = campaignPassForAccount(cnAuth(), "acct-race", "idx-race", now)
		}(i)
	}
	wg.Wait()

	if hitCount(&hits) != 1 {
		t.Fatalf("claim POSTs = %d, want exactly 1 under concurrency", hitCount(&hits))
	}
	claimed := 0
	for _, res := range results {
		if res.State != campaignStateClaimed && res.State != campaignStateAlready {
			t.Fatalf("unexpected concurrent state %q (%+v)", res.State, res)
		}
		if res.ClaimMade {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("passes reporting a made claim = %d, want 1", claimed)
	}
}

// TestCampaignCreditRecheckConfirmsWhenGrantLands: after the bounded re-check
// fires (driven deterministically, test-chosen firing time) the delayed grant
// is observed and the claim is confirmed.
func TestCampaignCreditRecheckConfirmsWhenGrantLands(t *testing.T) {
	resetCampaignLedger()
	defer resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	rechecks := withManualRechecks(t)
	withQuotaReadback(t, 100, 100, 200) // before, immediate (lagging), re-check
	hits := claimableServer(t, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)

	now := bj(2026, 9, 29, 10, 5)
	res := campaignPassForAccount(cnAuth(), "acct-late", "idx-late", now)
	if res.State != campaignStatePendingCredit {
		t.Fatalf("state = %q, want pending_credit before the grant lands (%+v)", res.State, res)
	}
	a, ok := rechecks.first()
	if !ok || !a.at.Equal(bj(2026, 9, 29, 10, 35)) {
		t.Fatalf("armed re-check = (%v, %v), want 10:35 Beijing", a.at, ok)
	}
	if n := rechecks.runAll(a.at); n != 1 {
		t.Fatalf("fired re-checks = %d, want 1", n)
	}
	key := campaignClaimKey{AuthID: "acct-late", CampaignID: "camp-cn-daily", Window: campaignWindowID(now)}
	rec, ok := campaignLookupClaim(key)
	if !ok || !rec.Confirmed {
		t.Fatalf("late grant never confirmed the claim: %+v ok=%v", rec, ok)
	}
	if hitCount(hits) != 1 {
		t.Fatalf("claim POSTs = %d, want 1 (the re-check must not re-claim)", hitCount(hits))
	}
}

// TestCampaignCreditRecheckMarksUnconfirmedWhenGrantNeverLands: after the
// bounded re-check the claim is 待核, never a fake success.
func TestCampaignCreditRecheckMarksUnconfirmedWhenGrantNeverLands(t *testing.T) {
	resetCampaignLedger()
	defer resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	rechecks := withManualRechecks(t)
	withQuotaReadback(t, 100) // always 100 — the grant never shows
	hits := claimableServer(t, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)

	now := bj(2026, 9, 29, 10, 5)
	res := campaignPassForAccount(cnAuth(), "acct-nocredit", "idx-nocredit", now)
	if res.State != campaignStatePendingCredit || res.Confirmed {
		t.Fatalf("state = %q confirmed=%v, want pending_credit/unconfirmed (%+v)", res.State, res.Confirmed, res)
	}
	a, ok := rechecks.first()
	if !ok {
		t.Fatal("no bounded re-check armed")
	}
	if n := rechecks.runAll(a.at); n != 1 {
		t.Fatalf("fired re-checks = %d, want 1", n)
	}
	key := campaignClaimKey{AuthID: "acct-nocredit", CampaignID: "camp-cn-daily", Window: campaignWindowID(now)}
	rec, ok := campaignLookupClaim(key)
	if !ok || !rec.Unconfirmed {
		t.Fatalf("bounded re-check never marked the claim 待核: %+v ok=%v", rec, ok)
	}
	if rec.Confirmed {
		t.Fatal("待核 must never be confirmed without evidence")
	}
	if hitCount(hits) != 1 {
		t.Fatalf("claim POSTs = %d, want 1 (待核 must not re-claim)", hitCount(hits))
	}
}

// TestCampaignCreditRecheckUpdatesPanelCache: the bounded re-check folds the
// late-observed snapshot into the account cache without a second upstream call
// and without dropping plan / checkin / the grace stamp (I2).
func TestCampaignCreditRecheckUpdatesPanelCache(t *testing.T) {
	resetCampaignLedger()
	defer resetCampaignLedger()
	const id = "acct-cache-recheck"
	defer accountCache.Delete(id)
	accountCache.Store(id, &accountCacheEntry{
		plan:      "Pro Trial",
		checkin:   &checkinSummary{Active: true},
		checkinAt: time.Now(),
	})
	withRecheckDelay(t, time.Hour)
	rechecks := withManualRechecks(t)
	withQuotaReadback(t, 100, 100, 200)
	claimableServer(t, `{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}`)

	now := bj(2026, 9, 29, 10, 5)
	res := campaignPassForAccount(cnAuth(), id, "idx-cache-recheck", now)
	if res.State != campaignStatePendingCredit {
		t.Fatalf("state = %q, want pending_credit (%+v)", res.State, res)
	}
	a, _ := rechecks.first()
	rechecks.runAll(a.at)

	v, ok := accountCache.Load(id)
	if !ok {
		t.Fatal("cache entry disappeared")
	}
	e := v.(*accountCacheEntry)
	if e.credits == nil || e.credits.TotalRemain != 200 {
		t.Fatalf("late grant not folded into the panel cache: %+v", e.credits)
	}
	if e.credits.FetchedAt == "" {
		t.Fatal("refreshed snapshot must carry FetchedAt")
	}
	if e.plan != "Pro Trial" || e.checkin == nil || e.checkinAt.IsZero() {
		t.Fatalf("cache merge dropped plan/checkin/grace: %+v", e)
	}
}

// TestCampaignPassHTTPErrorIsErrorState: a claim POST rejected with HTTP >=400
// is an error state for the next slot — never a fake success and never
// "unconfirmed" (m1: the call failed, upstream did not answer a 200).
func TestCampaignPassHTTPErrorIsErrorState(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	withManualRechecks(t)
	withQuotaReadback(t, 100)
	hits := int32(0)
	newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
		"/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
			return http.StatusOK, campaignList("CLAIMABLE")
		},
		"/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
			atomic.AddInt32(&hits, 1)
			return http.StatusInternalServerError, `{"err":"boom"}`
		},
	})

	res := campaignPassForAccount(cnAuth(), "acct-http500", "idx-http500", bj(2026, 9, 29, 10, 5))
	if res.State != campaignStateError {
		t.Fatalf("state = %q, want %q (%+v)", res.State, campaignStateError, res)
	}
	if res.Error == "" || res.Message == "" {
		t.Fatalf("error state must carry the upstream failure: %+v", res)
	}
	if res.ClaimMade || res.Confirmed {
		t.Fatalf("a failed claim call must not claim or confirm: %+v", res)
	}
	if _, ok := campaignLookupClaim(campaignClaimKey{AuthID: "acct-http500", CampaignID: "camp-cn-daily", Window: "2026-09-29"}); ok {
		t.Fatal("a failed claim call must not be recorded as claimed")
	}
	if hitCount(&hits) != 1 {
		t.Fatalf("claim POSTs = %d, want 1", hitCount(&hits))
	}
}

// TestCampaignPassTransportErrorIsErrorState: a transport-level failure (the
// connection dies before a response) is an error state, not "unconfirmed".
// The server hijacks and closes the claim connection — entirely local, never a
// real external call (m1).
func TestCampaignPassTransportErrorIsErrorState(t *testing.T) {
	resetCampaignLedger()
	withRecheckDelay(t, time.Hour)
	withManualRechecks(t)
	withQuotaReadback(t, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/claim"):
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("test server does not support hijacking")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close() // client sees EOF -> transport error
		case strings.HasSuffix(r.URL.Path, "/me/campaigns"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(campaignList("CLAIMABLE")))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	prev := billingBaseOverride
	billingBaseOverride = func(string) string { return srv.URL }
	t.Cleanup(func() { billingBaseOverride = prev })

	res := campaignPassForAccount(cnAuth(), "acct-drop", "idx-drop", bj(2026, 9, 29, 10, 5))
	if res.State != campaignStateError {
		t.Fatalf("state = %q, want %q (%+v)", res.State, campaignStateError, res)
	}
	if res.Error == "" {
		t.Fatalf("transport error must be surfaced: %+v", res)
	}
	if res.ClaimMade || res.Confirmed {
		t.Fatalf("a dropped connection must not claim or confirm: %+v", res)
	}
	if _, ok := campaignLookupClaim(campaignClaimKey{AuthID: "acct-drop", CampaignID: "camp-cn-daily", Window: "2026-09-29"}); ok {
		t.Fatal("a transport failure must not be recorded as claimed")
	}
}

// TestNextCheckinTimeUsesBeijingCampaignSlots: on the deployment host (+08:00)
// the next tick after 10:30 Beijing is the 10:35 slot, not a host-hour tick.
func TestNextCheckinTimeUsesBeijingCampaignSlots(t *testing.T) {
	withCampaignJitter(t, 0)
	if _, off := time.Now().Zone(); off != 8*3600 {
		t.Skip("host zone is not UTC+8; the host-local keepalive cadence may legitimately win here")
	}
	now := time.Date(2026, 9, 29, 10, 30, 0, 0, campaignWindowZone)
	next := nextCheckinTime(now).In(campaignWindowZone)
	if got := next.Format("15:04"); got != "10:35" {
		t.Fatalf("next tick after 10:30 Beijing = %s, want 10:35", got)
	}
	if next.Day() != 29 {
		t.Fatalf("next tick rolled to the wrong day: %s", next)
	}
}
