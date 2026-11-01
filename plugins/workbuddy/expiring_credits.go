// expiring_credits.go implements the "soon-expiring credits" bucket of the
// weighted pool's three-factor weight.
//
// Why: the reference weightOf() is credits-ratio ×10 + expiring-ratio ×8 +
// idle compensation, and the plugin port had the mechanism but no data source
// — creditsSummary carried only totals, so the expiring factor was inert
// (always 0). The same get-user-resource response the plugin already fetches
// carries per-package CycleEndTime, which is exactly what the reference uses
// (workbuddy2api internal/upstream/client.go UserResourceDetailed): packages
// whose cycle ends within the window are the soon-expiring subset. Deriving it
// here adds no upstream calls and no new credential data surface.
package main

import (
	"strings"
	"sync"
	"time"
)

// defaultExpiringSoonWindow mirrors the reference default ("168h" = 7 days):
// official bonus credits typically expire within two weeks.
const defaultExpiringSoonWindow = 168 * time.Hour

// wbPackageEndLayout is the upstream wall-clock format for CycleEndTime
// (e.g. "2026-10-01 00:00:00"), interpreted in CST (billing.go cstZone).
const wbPackageEndLayout = "2006-01-02 15:04:05"

var (
	expiringSoonWindowMu sync.RWMutex
	expiringSoonWindow   = defaultExpiringSoonWindow
)

// setExpiringSoonWindow installs the configured window (0 disables the bucket).
func setExpiringSoonWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	expiringSoonWindowMu.Lock()
	expiringSoonWindow = d
	expiringSoonWindowMu.Unlock()
}

// loadedExpiringSoonWindow returns the effective window (0 = bucket disabled).
func loadedExpiringSoonWindow() time.Duration {
	expiringSoonWindowMu.RLock()
	defer expiringSoonWindowMu.RUnlock()
	return expiringSoonWindow
}

// wbParseExpiringSoonDuration parses the plugin config value: a Go duration
// ("168h", "48h", also "30m"); empty/invalid falls back to the default;
// explicit zero and negatives disable the bucket (reference semantics).
func wbParseExpiringSoonDuration(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultExpiringSoonWindow
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return defaultExpiringSoonWindow
	}
	if d < 0 {
		return 0
	}
	return d
}

// wbExpiringSoonRemain sums the currently-spendable balance of packages whose
// cycle end is at or before now+window. Packages with no usable Remain, an
// empty or unparsable end time, or window<=0 count as stable (never guessed
// into the expiring bucket).
func wbExpiringSoonRemain(packages []packageSummary, now time.Time, window time.Duration) int64 {
	if window <= 0 {
		return 0
	}
	deadline := now.Add(window)
	var expiring int64
	for _, p := range packages {
		if p.Remain <= 0 {
			continue
		}
		end := strings.TrimSpace(p.CycleEnd)
		if end == "" {
			continue
		}
		t, err := time.ParseInLocation(wbPackageEndLayout, end, cstZone)
		if err != nil {
			continue
		}
		if !t.After(deadline) {
			expiring += p.Remain
		}
	}
	return expiring
}

// wbClampExpiring keeps the bucket inside [0, remain].
func wbClampExpiring(remain, expiring int64) int64 {
	if expiring < 0 {
		return 0
	}
	if remain < 0 {
		remain = 0
	}
	if expiring > remain {
		return remain
	}
	return expiring
}
