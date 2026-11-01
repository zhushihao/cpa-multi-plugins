// payload_effort_clamp_test.go — round6 item 1 (red → green).
//
// Live failure (2026-09-30): a deepseek-v4.1-flash session routed through a
// rich-protocol ingress carried a reasoning_effort the upstream model rejects
// (upstream code 11150) because the payload path forwarded the client's dial
// verbatim. The per-model supported sets (live upstream roster) are:
//
//	deepseek-v4.1-flash low/high/max      kimi-k3-2         low/high/xhigh
//	glm-5.3             low/high/max      kimi-k2.8-preview low/high/max
//	glm-5.3-flash       low/high/max      hy4-preview       high
//	                                       hy3 / hy3-x       low/high
//
// The clamp maps an unsupported dial to the nearest supported level (ties
// prefer the higher tier: never silently downgrade thinking; low is the
// floor). It runs AFTER forceMaxThinking so the hy3/hy4 semantics stay intact
// (explicit hy3/hy3-x "low" is preserved), and it never invents a value:
// unknown dials and unknown models pass through unchanged.
package main

import (
	"encoding/json"
	"testing"
)

// decodeEffort runs prepareUpstreamBody over one body and returns the
// resulting reasoning_effort ("" when absent).
func decodeEffort(t *testing.T, body []byte, upstreamModel string) (string, map[string]any) {
	t.Helper()
	out := prepareUpstreamBody(body, nil, &storedAuth{}, upstreamModel)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output unparsable: %v (%s)", err, out)
	}
	eff, _ := obj["reasoning_effort"].(string)
	return eff, obj
}

// TestPrepareUpstreamBodyClampsDeepseekEffort is the red test from the live
// 11150 incident: "medium" and "xhigh" are not in deepseek's supported set
// and must be clamped to a supported level.
func TestPrepareUpstreamBodyClampsDeepseekEffort(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Supported levels pass through (never rewritten).
		{"low", "low"},
		{"high", "high"},
		{"max", "max"},
		// Unsupported dials clamp to the nearest supported level.
		// medium sits between low and high — tie resolves upward.
		{"medium", "high"},
		// xhigh sits between high and max — tie resolves upward.
		{"xhigh", "max"},
		// minimal is below low — low is the floor.
		{"minimal", "low"},
	}
	for _, tc := range cases {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + tc.in + `"}`)
		got, obj := decodeEffort(t, body, "deepseek-v4.1-flash")
		if got != tc.want {
			t.Errorf("deepseek-v4.1-flash reasoning_effort %q clamped to %q, want %q", tc.in, got, tc.want)
		}
		if obj["reasoning_summary"] != "auto" {
			t.Errorf("effort %q: reasoning_summary mirror lost: %v", tc.in, obj["reasoning_summary"])
		}
	}
}

// TestPrepareUpstreamBodyEffortClampRespectsForceMaxThinking pins the
// interaction with the v0.9.29 hy3/hy4 semantics: forceMaxThinking runs first
// and the clamp must not fight it.
func TestPrepareUpstreamBodyEffortClampRespectsForceMaxThinking(t *testing.T) {
	hy3Low := []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)
	if got, _ := decodeEffort(t, hy3Low, "hy3"); got != "low" {
		t.Fatalf("hy3 explicit low must be preserved, got %q", got)
	}
	hy3Medium := []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`)
	if got, _ := decodeEffort(t, hy3Medium, "hy3"); got != "high" {
		t.Fatalf("hy3 medium pins to high, got %q", got)
	}
	hy4Low := []byte(`{"model":"hy4-preview","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)
	if got, _ := decodeEffort(t, hy4Low, "hy4-preview"); got != "high" {
		t.Fatalf("hy4-preview low pins to high, got %q", got)
	}
}

// TestPrepareUpstreamBodyEffortClampLeavesUnknownAlone: the clamp never
// invents a value — an unknown model or an unknown dial is forwarded as-is
// (mirrorReasoningEffort's none/off deletion stays the only special case).
func TestPrepareUpstreamBodyEffortClampLeavesUnknownAlone(t *testing.T) {
	unknownModel := []byte(`{"model":"gpt-5.1","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`)
	if got, _ := decodeEffort(t, unknownModel, "gpt-5.1"); got != "medium" {
		t.Fatalf("unknown model: effort must pass through unchanged, got %q", got)
	}
	unknownDial := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"ultra"}`)
	if got, _ := decodeEffort(t, unknownDial, "deepseek-v4.1-flash"); got != "ultra" {
		t.Fatalf("unknown dial must pass through unchanged, got %q", got)
	}
}

// TestClampReasoningEffortForModel pins the full per-model supporting matrix:
// every roster model, every dial rank, including the above-top clamp to the
// set's top tier (owner correction: kimi-k3-2 "max" → "xhigh") and the low
// floor.
func TestClampReasoningEffortForModel(t *testing.T) {
	cases := []struct{ model, in, want string }{
		// low/high/max models: xhigh clamps up to max, medium up to high,
		// minimal down to low.
		{"deepseek-v4.1-flash", "low", "low"},
		{"deepseek-v4.1-flash", "high", "high"},
		{"deepseek-v4.1-flash", "max", "max"},
		{"deepseek-v4.1-flash", "medium", "high"},
		{"deepseek-v4.1-flash", "xhigh", "max"},
		{"deepseek-v4.1-flash", "minimal", "low"},
		{"glm-5.3", "xhigh", "max"},
		{"glm-5.3", "medium", "high"},
		{"glm-5.3-flash", "minimal", "low"},
		{"kimi-k2.8-preview", "xhigh", "max"},
		// kimi-k3-2: low/high/xhigh — a request above the top tier clamps to
		// the set's top (owner correction 2026-09-30: "max" → "xhigh", not
		// "high"), and xhigh itself is an exact supported level.
		{"kimi-k3-2", "xhigh", "xhigh"},
		{"kimi-k3-2", "max", "xhigh"},
		{"kimi-k3-2", "medium", "high"},
		// hy4-preview supports only high — everything clamps there.
		{"hy4-preview", "low", "high"},
		{"hy4-preview", "medium", "high"},
		{"hy4-preview", "xhigh", "high"},
		{"hy4-preview", "max", "high"},
		// hy3 / hy3-x: low is supported (v0.9.29 keep), everything above
		// high clamps down to high.
		{"hy3", "low", "low"},
		{"hy3", "medium", "high"},
		{"hy3", "xhigh", "high"},
		{"hy3", "max", "high"},
		{"hy3-x", "low", "low"},
		{"hy3-x", "xhigh", "high"},
		// Case/space-insensitive model and dial matching.
		{"DeepSeek-V4.1-Flash", "  MEDIUM  ", "high"},
		{"KIMI-K3-2", "MAX", "xhigh"},
		// Unknown model / unknown dial / empty: never invent.
		{"gpt-5.1", "medium", "medium"},
		{"", "medium", "medium"},
		{"deepseek-v4.1-flash", "ultra", "ultra"},
		{"deepseek-v4.1-flash", "", ""},
		{"glm-5.3", "none", "none"},
	}
	for _, tc := range cases {
		if got := clampReasoningEffortForModel(tc.model, tc.in); got != tc.want {
			t.Errorf("clamp(%q, %q) = %q, want %q", tc.model, tc.in, got, tc.want)
		}
	}
}
