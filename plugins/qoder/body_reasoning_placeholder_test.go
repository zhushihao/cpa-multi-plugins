// body_reasoning_placeholder_test.go — round7c (red → green).
//
// The host's responses→chat request translator fills history turns whose
// reasoning summary was lost with the literal "[reasoning unavailable]"
// (fallbackToolReasoning). Qwen-family upstreams parrot the bracketed token
// back as new thinking and the client stores it verbatim (live 2026-09-30,
// qfmodel). The body builder must swap the placeholder for a neutral sentence
// before the history reaches upstream.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildQoderBodyReplacesReasoningPlaceholder(t *testing.T) {
	history := []map[string]any{
		{"role": "system", "content": "you are helpful"},
		{"role": "user", "content": "37*43? use calc"},
		{"role": "assistant", "content": "Calling tools.", "reasoning_content": "[reasoning unavailable]",
			"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "calc", "arguments": `{"expr":"37*43"}`}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "1591"},
	}
	raw, _ := json.Marshal(map[string]any{"model": "qfmodel", "messages": history})
	req := &openAIRequest{}
	if err := json.Unmarshal(raw, req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	body, err := buildQoderBody(req, "qfmodel", uiUserType(nil))
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	if strings.Contains(string(body), "[reasoning unavailable]") {
		t.Fatalf("upstream body still carries the reasoning placeholder; body=%s", string(body))
	}
	if !strings.Contains(string(body), "Previous reasoning content was not retained") {
		t.Fatalf("upstream body missing the neutral replacement sentence; body=%s", string(body))
	}
}

func TestBuildQoderBodyKeepsRealReasoning(t *testing.T) {
	history := []map[string]any{
		{"role": "system", "content": "you are helpful"},
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "ok", "reasoning_content": "step one: multiply"},
	}
	raw, _ := json.Marshal(map[string]any{"model": "qfmodel", "messages": history})
	req := &openAIRequest{}
	if err := json.Unmarshal(raw, req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	body, err := buildQoderBody(req, "qfmodel", uiUserType(nil))
	if err != nil {
		t.Fatalf("buildQoderBody: %v", err)
	}
	if !strings.Contains(string(body), "step one: multiply") {
		t.Fatalf("real reasoning_content must round-trip verbatim; body=%s", string(body))
	}
}
