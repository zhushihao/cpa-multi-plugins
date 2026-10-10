// campaign_window.go implements the Beijing-time campaign claim schedule:
// slots at 10:05 / 10:35 / 21:05 Asia/Shanghai, a bounded per-process startup
// catch-up for a missed slot, an in-memory per-process
// account/campaign/window claim ledger, and the honest claim state machine —
// success requires the CLAIMED status AND (for credit benefits) a quota
// readback consistent with the grant. Empty campaign lists, upstream errors
// and uncredited claims never surface as success.
//
// Scope of the dedup guarantee (I1, 2026-09-29 review): the ledger and the
// "window already covered" marker live in process memory only. They dedup
// within one plugin process (scheduler slot vs. a second slot vs. the manual
// button). They do NOT dedup across process restarts, across plugin
// instances, or against external clients (CLI/curl/browser hitting upstream
// directly). The only cross-process guard is the upstream claim status /
// replay idempotency (ALREADY_CLAIMED), which is NOT proven in the real
// environment by any test here — tests use httptest stubs. Do not claim
// duplicate claims are safe beyond this process+window without real evidence.
//
// Scope of the credit-delta evidence (I2, 2026-09-29 review): the readback is
// the aggregate usable total (creditsSummary.TotalRemain), which can move for
// reasons other than this claim — concurrent chat usage, a shared/org pool,
// package expiry, or another account drawing the same pool. `after >= before +
// expected` is therefore evidence consistent with the grant, not a proof of it;
// a miss is likewise not proof the grant failed (that is why a miss stays
// pending/待核 instead of an error). The panel projection exposes
// confirmed/unconfirmed so the operator sees exactly how strong the evidence is.
//
// Why Beijing wall-clock, not host-local hours: the upstream activity opens
// at 10:00 China time. A host in another timezone used to fire the old
// checkinHours=[10,21] at its own local 10:00 — hours off the real window.
// Asia/Shanghai has had no DST since 1991, so a fixed UTC+8 zone is exact and
// avoids time.LoadLocation tzdata problems on Windows.
package main

import (
	"log"
	"math/rand"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Beijing wall-clock schedule
// -----------------------------------------------------------------------------

const campaignWindowZoneOffset = 8 * 3600 // UTC+8

var campaignWindowZone = time.FixedZone("CST", campaignWindowZoneOffset)

// campaignSlot is a Beijing wall-clock claim slot.
type campaignSlot struct{ Hour, Min int }

// campaignSlots are the daily claim triggers, in order:
//
//	10:05 + random 0–15 min — first attempt after the upstream activity opens
//	    at 10:00 Beijing (owner schedule ruling 2026-09-30, refined: window
//	    10:05–10:20);
//	10:35 — bounded recovery slot for a failed/missed first attempt;
//	21:05 — evening companion (upstream retry / late-day window).
//
// The scheduler additionally runs one startup catch-up when the plugin
// (re)starts inside the current window — see campaignRecoveryDue.
var campaignSlots = []campaignSlot{{Hour: 10, Min: 5}, {Hour: 10, Min: 35}, {Hour: 21, Min: 5}}

// campaignFirstSlotJitterMinutes bounds the randomized offset of the FIRST
// daily slot: base 10:05 plus 0–15 minutes → window 10:05–10:20 CST. The
// recovery slot (10:35) sits 15 minutes past the window end, so a jittered
// first attempt can never collide with it.
const campaignFirstSlotJitterMinutes = 16

// campaignJitterFor draws the first slot's random offset (0–15 min). Tests
// replace this seam with a deterministic value so slot arithmetic stays
// exact; production keeps the live draw.
var campaignJitterFor = func() int {
	return rand.Intn(campaignFirstSlotJitterMinutes)
}

// campaignSlotTime returns slot s on day (interpreted in Beijing time).
// The first slot (index 0) carries the random 0–15 min offset.
func campaignSlotTime(day time.Time, s campaignSlot) time.Time {
	d := day.In(campaignWindowZone)
	minute := s.Min
	if s == campaignSlots[0] {
		minute = s.Min + campaignJitterFor()
	}
	return time.Date(d.Year(), d.Month(), d.Day(), s.Hour, minute, 0, 0, campaignWindowZone)
}

// nextCampaignSlot returns the earliest Beijing slot strictly after now.
func nextCampaignSlot(now time.Time) time.Time {
	bj := now.In(campaignWindowZone)
	for _, day := range []time.Time{bj, bj.AddDate(0, 0, 1)} {
		for _, s := range campaignSlots {
			if t := campaignSlotTime(day, s); t.After(now) {
				return t
			}
		}
	}
	// Unreachable: tomorrow always contains a slot after now.
	return campaignSlotTime(bj.AddDate(0, 0, 1), campaignSlots[0])
}

// latestCampaignSlot returns the latest Beijing slot at or before now today.
func latestCampaignSlot(now time.Time) (time.Time, bool) {
	var latest time.Time
	for _, s := range campaignSlots {
		if t := campaignSlotTime(now, s); !t.After(now) {
			latest = t
		}
	}
	return latest, !latest.IsZero()
}

// campaignWindowID is the dedup window: the Beijing calendar day. A claim is
// deduped per account + campaign + this day.
func campaignWindowID(t time.Time) string {
	return t.In(campaignWindowZone).Format("2006-01-02")
}

// campaignRecoveryDue decides the bounded startup catch-up: when the process
// (re)starts at/after the current day's first slot (10:05 Beijing) and no pass
// has covered this window yet, run once and report the missed slot. Before the
// first slot nothing is due (the 10:05 timer will fire); after a pass covered
// the window nothing is replayed (bounded to one catch-up per window).
func campaignRecoveryDue(now time.Time, lastWindow string) (time.Time, bool) {
	slot, ok := latestCampaignSlot(now)
	if !ok {
		return time.Time{}, false
	}
	if campaignWindowID(now) == lastWindow {
		return time.Time{}, false
	}
	return slot, true
}

// campaignPassState tracks the last Beijing window a scheduled pass covered so
// the startup catch-up runs at most once per (process, window). This is
// process memory only: a host restart forgets it and the catch-up may run once
// more in the same window. That replay is bounded to one extra POST attempt;
// the upstream CLAIMED/replay status is the only cross-process guard and is
// NOT proven in the real environment (I1).
var campaignPassState struct {
	mu     sync.Mutex
	window string
}

func markCampaignPassWindow(w string) {
	campaignPassState.mu.Lock()
	campaignPassState.window = w
	campaignPassState.mu.Unlock()
}

func campaignPassWindow() string {
	campaignPassState.mu.Lock()
	defer campaignPassState.mu.Unlock()
	return campaignPassState.window
}

// runStartupRecoveryOnce runs the bounded catch-up at most once per
// (process, window): in-memory only, so a restart inside the same window may
// run it once more (bounded replay; upstream idempotency is the only
// cross-process guard). Returns whether run was invoked.
func runStartupRecoveryOnce(now time.Time, run func()) bool {
	slot, due := campaignRecoveryDue(now, campaignPassWindow())
	if !due {
		return false
	}
	markCampaignPassWindow(campaignWindowID(now))
	log.Printf("qoder campaign: startup catch-up window=%s missed_slot=%s",
		campaignWindowID(now), slot.In(campaignWindowZone).Format("15:04"))
	if run != nil {
		run()
	}
	return true
}

// -----------------------------------------------------------------------------
// Claim ledger: per account + campaign + Beijing window (in-memory, per-process)
// -----------------------------------------------------------------------------

// campaignClaimKey is the dedup identity of one daily claim. The ledger is
// process memory only — see the scope note in the package header.
type campaignClaimKey struct {
	AuthID     string
	CampaignID string
	Window     string // Beijing calendar day, see campaignWindowID
}

// campaignClaimRecord is one ledger entry. Values are copied in and out under
// campaignLedger.mu — never shared by pointer.
type campaignClaimRecord struct {
	ClaimedAt    time.Time
	Window       string
	CampaignID   string
	Replayed     bool  // upstream answered ALREADY_CLAIMED (idempotent replay)
	Expected     int64 // expected credit grant (0 = non-credit benefit)
	BeforeRemain int64 // pre-claim remaining credits when readable
	HasBefore    bool
	Confirmed    bool // grant observed (or non-credit claim evidence)
	Unconfirmed  bool // bounded re-check ran out without credit evidence (待核)
	NextCheckAt  time.Time
	RecheckedAt  time.Time
	LastReadback *int64
	LastError    string
}

var campaignLedger = struct {
	mu sync.Mutex
	m  map[campaignClaimKey]campaignClaimRecord
}{m: make(map[campaignClaimKey]campaignClaimRecord)}

// campaignMarkClaimed records a claim; first reports whether this call
// created the entry. Existing entries are never overwritten (the first
// evidence wins; later readbacks update via campaignApplyCreditReadback).
func campaignMarkClaimed(key campaignClaimKey, rec campaignClaimRecord) (campaignClaimRecord, bool) {
	campaignLedger.mu.Lock()
	defer campaignLedger.mu.Unlock()
	if existing, ok := campaignLedger.m[key]; ok {
		return existing, false
	}
	rec.Window = key.Window
	rec.CampaignID = key.CampaignID
	campaignLedger.m[key] = rec
	return rec, true
}

func campaignLookupClaim(key campaignClaimKey) (campaignClaimRecord, bool) {
	campaignLedger.mu.Lock()
	defer campaignLedger.mu.Unlock()
	rec, ok := campaignLedger.m[key]
	return rec, ok
}

// campaignLatestClaim returns the most recent ledger record for authID across
// all windows (read-only copy). Used by the panel projection; the ledger is
// bounded by accounts × campaigns × days in process lifetime, so a linear scan
// is fine.
func campaignLatestClaim(authID string) (campaignClaimRecord, bool) {
	campaignLedger.mu.Lock()
	defer campaignLedger.mu.Unlock()
	var best campaignClaimRecord
	found := false
	for k, rec := range campaignLedger.m {
		if k.AuthID != authID {
			continue
		}
		if !found || rec.ClaimedAt.After(best.ClaimedAt) {
			best = rec
			found = true
		}
	}
	return best, found
}

// campaignCreditConfirmed reports whether the readback is consistent with the
// expected grant over the pre-claim snapshot. known=false is the honest
// "cannot prove the delta" state: a missing readback or a missing pre-claim
// snapshot is never upgraded to success. A non-credit benefit (Expected<=0) is
// confirmed by the CLAIMED status alone — there is no credit delta to observe.
//
// Honesty limits (I2): TotalRemain is an aggregate, so it can be confounded by
// concurrent usage, shared/org pools and package expiry. `confirmed` therefore
// means "observed a delta at least as large as expected", not "proved this
// specific grant landed"; and a miss only means "not observed" (hence pending
// or 待核, never a failure claim).
func campaignCreditConfirmed(rec campaignClaimRecord, after *creditsSummary) (confirmed, known bool) {
	if rec.Expected <= 0 {
		return true, true
	}
	if after == nil || !rec.HasBefore {
		return false, false
	}
	return after.TotalRemain >= rec.BeforeRemain+rec.Expected, true
}

// campaignApplyCreditReadback folds one quota readback attempt into the ledger
// record and reports the confirmed state. A miss before NextCheckAt keeps the
// claim pending; a miss at/after NextCheckAt marks it 待核 (Unconfirmed) — the
// bounded re-check ran out, and nothing is reported as success. Evidence
// arriving later still confirms.
func campaignApplyCreditReadback(key campaignClaimKey, after *creditsSummary, readErr error, now time.Time) (campaignClaimRecord, bool) {
	campaignLedger.mu.Lock()
	defer campaignLedger.mu.Unlock()
	rec, ok := campaignLedger.m[key]
	if !ok {
		return campaignClaimRecord{}, false
	}
	if rec.Confirmed {
		return rec, true
	}
	if after != nil {
		v := after.TotalRemain
		rec.LastReadback = &v
	}
	if confirmed, known := campaignCreditConfirmed(rec, after); known && confirmed {
		rec.Confirmed = true
		rec.Unconfirmed = false
		rec.RecheckedAt = now
		rec.LastError = ""
		campaignLedger.m[key] = rec
		return rec, true
	}
	if readErr != nil {
		rec.LastError = truncateRedacted(readErr.Error(), 200)
	}
	rec.RecheckedAt = now
	if !rec.NextCheckAt.IsZero() && !now.Before(rec.NextCheckAt) {
		rec.Unconfirmed = true
	}
	campaignLedger.m[key] = rec
	return rec, false
}

// campaignCreditRecheckDelay bounds the credit re-check: at most one delayed
// readback per claim (var so tests can shrink it).
var campaignCreditRecheckDelay = 10 * time.Minute

// campaignCreditRecheckAt picks the single bounded re-check time:
// min(claimed + 10m, next Beijing slot).
func campaignCreditRecheckAt(claimedAt time.Time) time.Time {
	t := claimedAt.Add(campaignCreditRecheckDelay)
	if slot := nextCampaignSlot(claimedAt); slot.Before(t) {
		return slot
	}
	return t
}

// campaignCreditsReadOverride is nil in production; tests point the quota
// readback at a deterministic probe. fetchUserResource hardcodes the regional
// upstream base, so the billingBaseOverride seam cannot redirect it.
//
// scheduleCampaignCreditRecheck resolves the reader ONCE at schedule time, so
// a delayed callback can never reach fetchUserResource after a test restored
// this override (which would be a real network call from a test process).
var campaignCreditsReadOverride func(*storedAuth) (*creditsSummary, error)

func readCampaignCredits(sa *storedAuth) (*creditsSummary, error) {
	if campaignCreditsReadOverride != nil {
		return campaignCreditsReadOverride(sa)
	}
	return fetchUserResource(sa)
}

// campaignRecheckSchedule is the timer seam for the single bounded credit
// readback. Production arms a real time.AfterFunc; tests substitute a manual
// driver so no wall-clock timer (and no goroutine) can outlive a test or fire
// after cleanup — the S2 regression from the 2026-09-29 review. The callback
// receives the actual firing time so ledger state transitions stay
// deterministic under the manual driver.
var campaignRecheckSchedule = func(at time.Time, run func(firedAt time.Time)) {
	delay := time.Until(at)
	if delay < 0 {
		delay = 0
	}
	time.AfterFunc(delay, func() { run(time.Now()) })
}

// scheduleCampaignCreditRecheck runs the one bounded credit readback and
// finalizes the record (confirmed / 待核). Never re-claims. authIndex lets the
// delayed readback prefer a freshly loaded credential (tokens may rotate
// during the delay); the captured snapshot is the fallback.
func scheduleCampaignCreditRecheck(sa *storedAuth, authIndex string, key campaignClaimKey, claimedAt time.Time) {
	at := campaignCreditRecheckAt(claimedAt)
	// Resolve the reader now (see campaignCreditsReadOverride): the callback
	// must never pick up a different reader than the one armed with it.
	read := campaignCreditsReadOverride
	if read == nil {
		read = fetchUserResource
	}
	campaignRecheckSchedule(at, func(firedAt time.Time) {
		if authIndex != "" {
			if fresh, err := hostAuthGet(authIndex); err == nil && fresh != nil {
				sa = fresh
			}
		}
		after, err := read(sa)
		// A confirmed late grant must reach the panel without its own upstream
		// call: fold the snapshot into the account cache (I2).
		if after != nil {
			storeCreditsSnapshot(key.AuthID, after)
		}
		rec, confirmed := campaignApplyCreditReadback(key, after, err, firedAt)
		switch {
		case confirmed:
			log.Printf("qoder campaign: credit confirmed on re-check window=%s", key.Window)
		case rec.Unconfirmed:
			log.Printf("qoder campaign: claim not credited after bounded re-check (待核) window=%s", key.Window)
		}
	})
}

// -----------------------------------------------------------------------------
// The scheduled pass
// -----------------------------------------------------------------------------

// Pass states. Only campaignStateClaimed with Confirmed=true means "credits
// observed"; campaignStateAlready means "nothing new was granted". None of
// these states is a cross-process guarantee (I1).
const (
	campaignStateClaimed       = "claimed"        // CLAIMED + credit delta observed (or non-credit evidence)
	campaignStateAlready       = "already"        // replayed / already recorded — no new grant
	campaignStateNone          = "none"           // no claimable campaign in the window
	campaignStateError         = "error"          // status fetch or claim call failed (HTTP >=400 / transport) — retry next slot
	campaignStateUnconfirmed   = "unconfirmed"    // upstream answered 200 but did not confirm CLAIMED
	campaignStatePendingCredit = "pending_credit" // CLAIMED, credit not yet observed (bounded re-check armed)
)

// campaignStateGrantsLifecycleGrace reports whether a pass outcome is evidence
// of an upstream claim and therefore justifies the lifecycle grace window.
// claimed / already / pending_credit are all CLAIMED-backed evidence: credits
// (and the quota endpoint they are read from) can lag behind the claim, so the
// just-claimed account must not be disabled by a stale remain=0 snapshot in
// the same tick. error / none / unconfirmed carry no claim evidence and must
// NOT suppress reconcile (they stay retryable).
func campaignStateGrantsLifecycleGrace(state string) bool {
	switch state {
	case campaignStateClaimed, campaignStateAlready, campaignStatePendingCredit:
		return true
	}
	return false
}

// campaignPassResult is one account's scheduled-pass outcome. JSON tags are
// intentionally omitted: this is internal scheduling state, not an API shape.
type campaignPassResult struct {
	AuthID         string
	Nickname       string
	Window         string
	State          string
	CampaignID     string
	CampaignKey    string
	ExpectedCredit int64
	CreditsBefore  *creditsSummary
	CreditsAfter   *creditsSummary
	Confirmed      bool
	ClaimMade      bool
	Message        string
	Error          string
	Summary        *checkinSummary
}

// campaignPassForAccount runs the scheduled claim pass for one account.
// authID is the stable auth-file id (dedup / cache key); authIndex serializes
// against the manual check-in path via the shared per-account mutex.
func campaignPassForAccount(sa *storedAuth, authID, authIndex string, now time.Time) campaignPassResult {
	res := campaignPassResult{AuthID: authID, Window: campaignWindowID(now)}
	defer func() {
		log.Printf("qoder campaign pass: auth=%s state=%s window=%s campaign=%s confirmed=%v",
			res.AuthID, res.State, res.Window, res.CampaignKey, res.Confirmed)
	}()
	if sa == nil {
		res.State = campaignStateError
		res.Error = "no credential"
		return res
	}
	res.Nickname = sa.Account.Nickname

	lock := checkinLockFor(authIndex)
	lock.Lock()
	defer lock.Unlock()

	status, err := fetchCampaignStatus(sa)
	if err != nil {
		res.State = campaignStateError
		res.Error = "status: " + err.Error()
		return res
	}
	summary := campaignCheckinSummary(status)
	mergeLegacyCheckinStats(sa, summary)
	res.Summary = summary

	if c := claimableCampaign(status); c != nil {
		return campaignClaimPass(sa, authID, authIndex, res, c, now)
	}
	if claimed := claimedCampaign(status); claimed != nil {
		// The activity row itself says this account already claimed — record it
		// so a later pass in the same window skips even if upstream flickers.
		campaignMarkClaimed(campaignClaimKey{AuthID: authID, CampaignID: claimed.CampaignID, Window: res.Window},
			campaignClaimRecord{ClaimedAt: now, Replayed: true})
		res.CampaignID = claimed.CampaignID
		res.CampaignKey = claimed.CampaignKey
		res.State = campaignStateAlready
		res.Message = "今日已领取（活动记录）"
		return res
	}
	res.State = campaignStateNone
	res.Message = "当前没有可领取的活动"
	return res
}

// campaignClaimPass claims the selected campaign for one account and folds
// the outcome into res. Callers hold the per-account lock and have already
// built res.Summary.
func campaignClaimPass(sa *storedAuth, authID, authIndex string, res campaignPassResult, c *campaign, now time.Time) campaignPassResult {
	res.CampaignID = c.CampaignID
	res.CampaignKey = c.CampaignKey
	res.ExpectedCredit = campaignCredit(c)
	key := campaignClaimKey{AuthID: authID, CampaignID: c.CampaignID, Window: res.Window}

	if rec, ok := campaignLookupClaim(key); ok {
		res.State = campaignStateAlready
		res.Confirmed = rec.Confirmed
		res.Message = "本窗口已由本插件领取，去重跳过"
		return res
	}

	// Pre-claim snapshot (best-effort): without it a later delta cannot be
	// proven, which only ever downgrades the result to 待核 — never to success.
	before, _ := readCampaignCredits(sa)
	res.CreditsBefore = before

	// performCampaignCheckin re-reads the campaign list (its own selection) and
	// normalizes the claim POST the same way the manual button does.
	claimed, err := performCampaignCheckin(sa)
	if err != nil {
		res.State = campaignStateError
		res.Error = "claim: " + err.Error()
		return res
	}
	if result, _ := claimed["result"].(string); result == "ALREADY_CLAIMED" {
		campaignMarkClaimed(key, campaignClaimRecord{ClaimedAt: now, Replayed: true})
		res.State = campaignStateAlready
		res.Message = "上游幂等回放：本窗口已领取"
		return res
	}
	if claimErr, _ := claimed["claim_error"].(bool); claimErr {
		// The claim call itself failed (HTTP >=400 or transport error): upstream
		// never confirmed anything, so this is an error state for the next slot,
		// NOT "unconfirmed" (m1). No ledger entry.
		msg, _ := claimed["message"].(string)
		res.State = campaignStateError
		res.Error = "claim: " + msg
		res.Message = msg
		return res
	}
	if success, _ := claimed["success"].(bool); !success {
		// Upstream answered but did not confirm CLAIMED: no ledger entry, so a
		// later slot may retry — this pass reports no success.
		res.State = campaignStateUnconfirmed
		res.Message = summarizeCheckinClaimRes(claimed)
		return res
	}

	// Fresh CLAIMED. Prefer the id/key/credits the claim response itself
	// carries (the selected campaign may differ from the earlier snapshot).
	if id, _ := claimed["campaign_id"].(string); id != "" {
		key.CampaignID = id
		res.CampaignID = id
	}
	if k, _ := claimed["campaign_key"].(string); k != "" {
		res.CampaignKey = k
	}
	if rc, ok := claimed["rewardCredits"].(float64); ok && int64(rc) > 0 {
		res.ExpectedCredit = int64(rc)
	}
	res.ClaimMade = true

	rec := campaignClaimRecord{
		ClaimedAt:   now,
		Expected:    res.ExpectedCredit,
		NextCheckAt: campaignCreditRecheckAt(now),
	}
	if before != nil {
		rec.BeforeRemain = before.TotalRemain
		rec.HasBefore = true
	}
	campaignMarkClaimed(key, rec)

	if res.ExpectedCredit <= 0 {
		res.State = campaignStateClaimed
		res.Confirmed = true
		res.Message = "签到成功（非积分权益，以领取状态为准）"
		return res
	}

	after, afterErr := readCampaignCredits(sa)
	res.CreditsAfter = after
	rec, confirmed := campaignApplyCreditReadback(key, after, afterErr, now)
	res.Confirmed = confirmed
	if confirmed {
		res.State = campaignStateClaimed
		res.Message = "签到成功，额度已回读确认"
		return res
	}
	// Credits not visible yet: arm the single bounded re-check. The claim is
	// recorded (no re-POST) but NOT reported as a confirmed success.
	res.State = campaignStatePendingCredit
	res.Message = "上游已确认领取，额度尚未到账（有限复核中）"
	scheduleCampaignCreditRecheck(sa, authIndex, key, now)
	return res
}

// rememberCampaignClaim records a manual (panel-button) claim in the window
// ledger so a scheduled pass in the same process+window does not re-POST
// while the upstream status still shows CLAIMABLE. Cross-process dedup rests
// on upstream idempotency only (I1).
func rememberCampaignClaim(authID string, res map[string]any, now time.Time) {
	id, _ := res["campaign_id"].(string)
	if authID == "" || id == "" {
		return
	}
	expected := int64(0)
	if rc, ok := res["rewardCredits"].(float64); ok {
		expected = int64(rc)
	}
	campaignMarkClaimed(
		campaignClaimKey{AuthID: authID, CampaignID: id, Window: campaignWindowID(now)},
		campaignClaimRecord{ClaimedAt: now, Expected: expected},
	)
}

// -----------------------------------------------------------------------------
// Read-only panel projection (I2)
// -----------------------------------------------------------------------------

// campaignClaimView is the panel-facing, read-only projection of the local
// claim ledger for one account. It exposes the evidence strength instead of a
// bare success flag: Confirmed=true means a credit delta (or non-credit
// evidence) was observed; Confirmed=false + Unconfirmed=true means 待核 (the
// bounded re-check ran out without credit evidence — possibly a readback
// failure, see LastError); Confirmed=false + Unconfirmed=false means the
// bounded credit re-check is still pending. None of these denies the upstream
// CLAIMED status.
type campaignClaimView struct {
	Window      string `json:"window"` // Beijing calendar day of the claim
	CampaignID  string `json:"campaign_id,omitempty"`
	State       string `json:"state"` // claimed | already | pending_credit | unconfirmed
	Confirmed   bool   `json:"confirmed"`
	Unconfirmed bool   `json:"unconfirmed"`
	Replayed    bool   `json:"replayed,omitempty"`
	Expected    int64  `json:"expected_credit,omitempty"`
	ClaimedAt   string `json:"claimed_at,omitempty"`    // RFC3339, Beijing offset
	NextCheckAt string `json:"next_check_at,omitempty"` // bounded re-check deadline
	RecheckedAt string `json:"rechecked_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

func formatCampaignViewTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(campaignWindowZone).Format(time.RFC3339)
}

// campaignClaimViewFor returns the latest local claim record for authID as a
// panel view (nil when the account has no record in this process). Derivation
// is explicit and total: replayed → already; confirmed → claimed;
// unconfirmed → unconfirmed (待核); else pending_credit.
func campaignClaimViewFor(authID string) *campaignClaimView {
	rec, ok := campaignLatestClaim(authID)
	if !ok {
		return nil
	}
	v := &campaignClaimView{
		Window:      rec.Window,
		CampaignID:  rec.CampaignID,
		Confirmed:   rec.Confirmed,
		Unconfirmed: rec.Unconfirmed,
		Replayed:    rec.Replayed,
		Expected:    rec.Expected,
		ClaimedAt:   formatCampaignViewTime(rec.ClaimedAt),
		NextCheckAt: formatCampaignViewTime(rec.NextCheckAt),
		RecheckedAt: formatCampaignViewTime(rec.RecheckedAt),
		LastError:   rec.LastError,
	}
	switch {
	case rec.Replayed:
		v.State = campaignStateAlready
	case rec.Confirmed:
		v.State = campaignStateClaimed
	case rec.Unconfirmed:
		v.State = campaignStateUnconfirmed
	default:
		v.State = campaignStatePendingCredit
	}
	return v
}
