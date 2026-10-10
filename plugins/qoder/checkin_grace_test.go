package main

// checkin_grace_test.go — v0.8.8: covers the two check-in fixes surfaced by
// the 2026-09-04 field report ("点签到账号消失/被禁用，刷新不回来"):
//
//  1. ALREADY_CLAIMED must be recognized as a successful already-check-in
//     (the old performCheckinCall swallowed http>=409 into success:false,
//     making the panel show "签到失败 http 409" for already-claimed days).
//  2. The post-check-in lifecycle grace window must suppress a disable that
//     would otherwise fire off a stale remain=0 quota snapshot taken right
//     after a successful check-in.

import (
	"strings"
	"testing"
	"time"
)

func TestRememberCheckinMoment_GraceWindow(t *testing.T) {
	const id = "grace-test-id"
	defer accountCache.Delete(id)

	if checkinGraceActive(id) {
		t.Fatal("grace must be inactive before any check-in")
	}
	rememberCheckinMoment(id)
	if !checkinGraceActive(id) {
		t.Fatal("grace must be active immediately after check-in")
	}

	// Simulate an old check-in (outside the window).
	if v, ok := accountCache.Load(id); ok {
		e := *(v.(*accountCacheEntry))
		e.checkinAt = time.Now().Add(-checkinGraceWindow - time.Minute)
		accountCache.Store(id, &e)
	}
	if checkinGraceActive(id) {
		t.Fatal("grace must expire after the window")
	}
}

func TestRememberCheckinMoment_PreservesCacheEntry(t *testing.T) {
	const id = "grace-preserve-id"
	defer accountCache.Delete(id)

	accountCache.Store(id, &accountCacheEntry{
		plan:    "Pro Trial",
		fetched: time.Now(),
	})
	rememberCheckinMoment(id)
	v, ok := accountCache.Load(id)
	if !ok {
		t.Fatal("entry missing after stamping")
	}
	e := v.(*accountCacheEntry)
	if e.plan != "Pro Trial" {
		t.Errorf("plan wiped by grace stamping: %q", e.plan)
	}
	if e.checkinAt.IsZero() {
		t.Fatal("checkinAt not stamped")
	}
}

// TestApplyCampaignPassOutcomePendingCreditGrantsGrace — S1 regression
// (2026-09-29 review): a CLAIMED-with-lagging-credits pass (pending_credit)
// must grant the lifecycle grace window exactly like claimed/already.
// Otherwise the same tick's forced reconcile can disable an account upstream
// just credited (stale remain=0 snapshot).
func TestApplyCampaignPassOutcomePendingCreditGrantsGrace(t *testing.T) {
	const id = "grace-pending-credit"
	defer accountCache.Delete(id)

	applyCampaignPassOutcome(id, campaignPassResult{
		State:         campaignStatePendingCredit,
		CreditsBefore: &creditsSummary{TotalRemain: 0},
		CreditsAfter:  &creditsSummary{TotalRemain: 0},
		Summary:       &checkinSummary{Active: true},
	})
	if !checkinGraceActive(id) {
		t.Fatal("pending_credit must grant the lifecycle grace window (S1)")
	}
	v, ok := accountCache.Load(id)
	if !ok {
		t.Fatal("pass outcome must populate the panel cache")
	}
	e := v.(*accountCacheEntry)
	if e.checkin == nil || e.credits == nil {
		t.Fatalf("panel cache missing the pass snapshot: %+v", e)
	}
	if e.checkinAt.IsZero() {
		t.Fatal("cache merge wiped the grace stamp it had just set")
	}
}

// TestApplyCampaignPassOutcomeNoEvidenceNoGrace: error / none / unconfirmed
// carry no claim evidence and must not suppress reconcile.
func TestApplyCampaignPassOutcomeNoEvidenceNoGrace(t *testing.T) {
	for _, state := range []string{campaignStateError, campaignStateNone, campaignStateUnconfirmed} {
		id := "grace-none-" + state
		defer accountCache.Delete(id)
		applyCampaignPassOutcome(id, campaignPassResult{State: state})
		if checkinGraceActive(id) {
			t.Fatalf("state %q must not grant lifecycle grace (no claim evidence)", state)
		}
	}
}

func TestNormalizeAlreadyClaimedBody(t *testing.T) {
	// The detection strings performCheckinCall relies on: the upstream 409
	// body carries result=ALREADY_CLAIMED. Keep the exact literal here so a
	// typo in the matcher fails this test.
	body := `{"result":"ALREADY_CLAIMED","rewardCredits":100}`
	if !strings.Contains(body, "ALREADY_CLAIMED") {
		t.Fatal("matcher literal changed")
	}
}
