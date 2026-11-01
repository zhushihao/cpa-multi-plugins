package main

// expiring_credits_test.go — the design's three-factor weight includes a
// "soon-expiring credits" ratio, but the plugin's billing snapshot had no
// expiring bucket, so that factor was inert (always 0). This file pins the
// fix: derive the expiring subset from the SAME get-user-resource packages the
// plugin already fetches (reference: workbuddy2api upstream/client.go
// UserResourceDetailed), with a configurable window, and feed it into the
// weighted pool candidates.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func wbPkg(remain int64, cycleEnd string) packageSummary {
	return packageSummary{Name: "pack", Remain: remain, Size: remain, CycleEnd: cycleEnd}
}

func TestWBExpiringSoonRemain_WindowRules(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, cstZone)
	win := 168 * time.Hour
	cases := []struct {
		name string
		pkgs []packageSummary
		want int64
	}{
		{"window disabled", []packageSummary{wbPkg(100, "2026-09-29 13:00:00")}, 0},
		{"within window", []packageSummary{wbPkg(100, "2026-09-30 12:00:00")}, 100},
		{"outside window", []packageSummary{wbPkg(100, "2026-10-20 12:00:00")}, 0},
		{"exactly at the edge counts", []packageSummary{wbPkg(100, "2026-10-06 12:00:00")}, 100},
		{"already expired counts", []packageSummary{wbPkg(100, "2026-09-01 00:00:00")}, 100},
		{"zero remain ignored", []packageSummary{wbPkg(0, "2026-09-30 12:00:00")}, 0},
		{"empty end date = stable", []packageSummary{wbPkg(100, "")}, 0},
		{"unparsable end date = stable", []packageSummary{wbPkg(100, "not-a-date")}, 0},
		{"mixed sum", []packageSummary{
			wbPkg(30, "2026-09-30 00:00:00"),
			wbPkg(70, "2026-12-31 00:00:00"),
			wbPkg(40, "2026-10-02 00:00:00"),
		}, 70},
	}
	for _, tc := range cases {
		window := win
		if tc.name == "window disabled" {
			window = 0
		}
		if got := wbExpiringSoonRemain(tc.pkgs, now, window); got != tc.want {
			t.Errorf("%s: wbExpiringSoonRemain = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestWBExpiringSoonRemain_CSTWallClock: upstream end times are CST wall-clock
// strings; parsing them as UTC would shift the boundary by 8h.
func TestWBExpiringSoonRemain_CSTWallClock(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, cstZone)
	// End == now + 48h CST exactly → inside a 48h window.
	pkgs := []packageSummary{wbPkg(10, "2026-10-01 00:00:00")}
	if got := wbExpiringSoonRemain(pkgs, now, 48*time.Hour); got != 10 {
		t.Fatalf("CST edge = %d, want 10 (CST parse)", got)
	}
	// One second beyond → outside.
	if got := wbExpiringSoonRemain(pkgs, now, 48*time.Hour-time.Second); got != 0 {
		t.Fatalf("beyond CST edge = %d, want 0", got)
	}
}

// TestClampExpiring: the bucket can never exceed the total remain.
func TestClampExpiring(t *testing.T) {
	if got := wbClampExpiring(500, 300); got != 300 {
		t.Fatalf("clamp = %d, want 300", got)
	}
	if got := wbClampExpiring(100, 300); got != 100 {
		t.Fatalf("clamp = %d, want 100", got)
	}
	if got := wbClampExpiring(100, -5); got != 0 {
		t.Fatalf("clamp = %d, want 0", got)
	}
}

func TestParseExpiringSoonDuration(t *testing.T) {
	if d := wbParseExpiringSoonDuration("168h"); d != 168*time.Hour {
		t.Fatalf("168h → %v", d)
	}
	if d := wbParseExpiringSoonDuration(" 48h "); d != 48*time.Hour {
		t.Fatalf("trimmed → %v", d)
	}
	if d := wbParseExpiringSoonDuration(""); d != defaultExpiringSoonWindow {
		t.Fatalf("empty → %v, want default %v", d, defaultExpiringSoonWindow)
	}
	if d := wbParseExpiringSoonDuration("bogus"); d != defaultExpiringSoonWindow {
		t.Fatalf("invalid → %v, want default", d)
	}
	if d := wbParseExpiringSoonDuration("0"); d != 0 {
		t.Fatalf("explicit zero must disable, got %v", d)
	}
	if d := wbParseExpiringSoonDuration("-1h"); d != 0 {
		t.Fatalf("negative must disable, got %v", d)
	}
}

// TestFetchUserResource_SetsTotalExpiring drives the real aggregation path
// against a local stub gateway: TotalExpiring must be filled from package
// CycleEndTime values inside the window and clamped by TotalRemain.
func TestFetchUserResource_SetsTotalExpiring(t *testing.T) {
	now := time.Now().In(cstZone)
	soon := now.Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	far := now.Add(90 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{
				"Response": map[string]any{
					"Data": map[string]any{
						"TotalCount": 2,
						"Accounts": []map[string]any{
							{"PackageName": "checkin", "CapacityRemain": 40, "CapacityUsed": 0, "CapacitySize": 40,
								"CycleCapacityRemain": 40, "CycleCapacitySize": 40, "CycleEndTime": soon},
							{"PackageName": "monthly", "CapacityRemain": 160, "CapacityUsed": 0, "CapacitySize": 160,
								"CycleCapacityRemain": 160, "CycleCapacitySize": 160, "CycleEndTime": far},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	prevBase := billingBase
	billingBase = srv.URL
	t.Cleanup(func() { billingBase = prevBase })

	prevWindow := expiringSoonWindow
	setExpiringSoonWindow(168 * time.Hour)
	t.Cleanup(func() { setExpiringSoonWindow(prevWindow) })

	sum, err := fetchUserResource(&storedAuth{})
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	if sum.TotalRemain != 200 {
		t.Fatalf("TotalRemain = %d, want 200", sum.TotalRemain)
	}
	if sum.TotalExpiring != 40 {
		t.Fatalf("TotalExpiring = %d, want 40 (only the soon-expiring package)", sum.TotalExpiring)
	}
}

// TestPoolCandidatesCarryExpiring: the scheduler's candidate builder must map
// the cached TotalExpiring into the pool candidate (this is the wiring that
// makes the factor non-inert).
func TestPoolCandidatesCarryExpiring(t *testing.T) {
	accountCache.Store("exp-a", &accountCacheEntry{credits: &creditsSummary{
		TotalRemain:   100,
		TotalExpiring: 60,
	}})
	accountCache.Store("exp-b", &accountCacheEntry{credits: &creditsSummary{
		TotalRemain: 100,
	}})
	t.Cleanup(func() {
		accountCache.Delete("exp-a")
		accountCache.Delete("exp-b")
	})

	got := wbPoolCandidatesForScheduler([]pluginapi.SchedulerAuthCandidate{
		{ID: "exp-a"},
		{ID: "exp-b"},
	})
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2", len(got))
	}
	if got[0].Credits != 100 || got[0].CreditsExpiring != 60 {
		t.Fatalf("exp-a = %+v, want credits=100 expiring=60", got[0])
	}
	if got[1].CreditsExpiring != 0 {
		t.Fatalf("exp-b = %+v, want no expiring bucket", got[1])
	}
}
