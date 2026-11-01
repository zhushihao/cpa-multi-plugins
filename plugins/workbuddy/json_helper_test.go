package main

import (
	"testing"
	"time"
)

func TestJsonBool(t *testing.T) {
	m := map[string]any{"a": true, "b": float64(1), "c": "true", "d": "false", "e": "1", "f": float64(0)}
	if !jsonBool(m, "a") {
		t.Error("a")
	}
	if !jsonBool(m, "b") {
		t.Error("b")
	}
	if !jsonBool(m, "c") {
		t.Error("c")
	}
	if !jsonBool(m, "e") {
		t.Error("e")
	}
	if jsonBool(m, "d") {
		t.Error("d")
	}
	if jsonBool(m, "f") {
		t.Error("f")
	}
	if jsonBool(m, "missing") {
		t.Error("missing")
	}
	if !jsonBool(m, "missing", "a") {
		t.Error("fallback a")
	}
}
func TestJsonI64(t *testing.T) {
	m := map[string]any{"a": float64(42), "b": int64(7), "c": "99", "d": "abc"}
	if jsonI64(m, "a") != 42 {
		t.Error("a")
	}
	if jsonI64(m, "b") != 7 {
		t.Error("b")
	}
	if jsonI64(m, "c") != 99 {
		t.Error("c")
	}
	if jsonI64(m, "d") != 0 {
		t.Error("d")
	}
	if jsonI64(m, "missing") != 0 {
		t.Error("missing")
	}
}
func TestJsonStr(t *testing.T) {
	m := map[string]any{"a": "hello", "b": 42, "c": true}
	if jsonStr(m, "a") != "hello" {
		t.Error("a")
	}
	if jsonStr(m, "b") != "" {
		t.Error("b")
	}
	if jsonStr(m, "c") != "" {
		t.Error("c")
	}
	if jsonStr(m, "missing") != "" {
		t.Error("missing")
	}
	if jsonStr(m, "missing", "a") != "hello" {
		t.Error("fallback")
	}
}
func TestNextCheckinTime(t *testing.T) {
	// I6 schedule ruling (2026-09-30): single midnight check-in slot 00:00 +
	// random 5–15 min → window 00:05–00:15 CST. The window is asserted on
	// wbNextCheckinInstant DIRECTLY (the raw check-in planner); nextCheckinTime
	// is the full scheduler wake (check-in ∪ keepalive  travel) and is
	// asserted separately (TestNextCheckinTimeWakesAtTravelSlot).
	withAutoTravelDisabled(t)
	// production default: midnight slot, jitter live
	late := time.Date(2026, 7, 24, 22, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		j := wbNextCheckinInstant(late, 0)
		if j.Day() != 25 || j.Hour() != 0 || j.Minute() < 5 || j.Minute() > 14 {
			t.Fatalf("want 25th 00:05–00:14, got %v", j)
		}
	}
	// window passed (00:20) → tomorrow's window
	passed := time.Date(2026, 7, 24, 0, 20, 0, 0, time.UTC)
	j := wbNextCheckinInstant(passed, 0)
	if j.Day() != 25 || j.Hour() != 0 || j.Minute() < 5 || j.Minute() > 14 {
		t.Fatalf("want 25th 00:05–00:14, got %v", j)
	}
	// pinned jitter → exact instants; legacy hour-table pinning still works
	withCheckinHours(t, 9)
	morning := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	if got := wbNextCheckinInstant(morning, 9); got.Hour() != 9 || got.Minute() != 0 {
		t.Fatalf("want exact 09:00, got %v", got)
	}
}
