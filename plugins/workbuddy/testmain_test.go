package main

// testmain_test.go — package-wide test isolation for plugin-owned state files.
//
// Several existing tests call configure(), which (production behavior) resolves
// the default state paths (<user cache>/workbuddy-plugin/*) and attaches them.
// If a later test feeds the global pool a cost observation or deletes an
// account, that would write outside the per-test t.TempDir() fixtures — exactly
// what the architecture ruling forbids.
//
// TestMain therefore points both artifacts at throwaway temp files for the
// whole test binary:
//   - cost state: "off" (persistence tests inject their own stores);
//   - adopt tombstones: a real temp path, because the delete path now fails
//     closed when the tombstone store is unusable (deletion tests must exercise
//     the verified-record path, not the fallback).
//
// Tests that need their own paths t.Setenv + reset the in-memory caches.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	prev, had := os.LookupEnv(wbCostStateEnv)
	_ = os.Setenv(wbCostStateEnv, "off")
	prevTomb, hadTomb := os.LookupEnv(wbAdoptTombstoneEnv)
	tombDir, err := os.MkdirTemp("", "wb-adopt-tomb-*")
	if err != nil {
		// Extremely unlikely; fall back to "off" so tests still start (the
		// deletion tests then exercise the fail-closed path explicitly).
		_ = os.Setenv(wbAdoptTombstoneEnv, "off")
	} else {
		_ = os.Setenv(wbAdoptTombstoneEnv, filepath.Join(tombDir, "adopt-tombstones.json"))
	}

	code := m.Run()

	if had {
		_ = os.Setenv(wbCostStateEnv, prev)
	} else {
		_ = os.Unsetenv(wbCostStateEnv)
	}
	if hadTomb {
		_ = os.Setenv(wbAdoptTombstoneEnv, prevTomb)
	} else {
		_ = os.Unsetenv(wbAdoptTombstoneEnv)
	}
	if tombDir != "" {
		_ = os.RemoveAll(tombDir)
	}
	os.Exit(code)
}
