// payload_reasoning_placeholder_test.go — placeholder reasoning check.
//
// injectReasoningInPlace folds historical reasoning into <thought> blocks.
// When the host's responses→chat fallback filled a lost turn with the literal
// "[reasoning unavailable]", that token used to be folded into content and the
// model parroted it back (live 2026-09-30). reasoningTextFromMessage must
// treat the placeholder as absent.
package main

import "testing"

func TestReasoningTextFromMessageIgnoresPlaceholder(t *testing.T) {
	msg := map[string]any{"role": "assistant", "reasoning_content": "[reasoning unavailable]"}
	if got := reasoningTextFromMessage(msg); got != "" {
		t.Fatalf("placeholder reasoning surfaced as real text: %q", got)
	}
	msg2 := map[string]any{"role": "assistant", "reasoning": " [reasoning unavailable] "}
	if got := reasoningTextFromMessage(msg2); got != "" {
		t.Fatalf("placeholder in reasoning field surfaced: %q", got)
	}
	msg3 := map[string]any{"role": "assistant", "reasoning_content": "step one"}
	if got := reasoningTextFromMessage(msg3); got != "step one" {
		t.Fatalf("real reasoning lost: %q", got)
	}
}

func TestInjectReasoningSkipsPlaceholderTurn(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "Calling tools.", "reasoning_content": "[reasoning unavailable]"},
	}}
	injectReasoningInPlace(obj)
	msgs := obj["messages"].([]any)
	a := msgs[1].(map[string]any)
	content, _ := a["content"].(string)
	if content != "Calling tools." {
		t.Fatalf("placeholder must not be folded into content; content=%q", content)
	}
}
