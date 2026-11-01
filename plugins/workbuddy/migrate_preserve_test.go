package main

// migrate_preserve_test.go — I6 non-destructive contract for the
// workbuddy.json → workbuddy-<uid>.json migration performed by lifecycle
// note/disabled writes (disableAuth / reenableAuth / persistLifecycleState).
//
// Old behavior deleted the legacy file after the canonical save. That breaks
// rollback when the auth directory is shared with the still-running :3002
// gateway (design ruling: 旧凭据不回写不删除). These tests pin the fixed
// contract: legacy file untouched (byte-identical), token bytes unchanged, and
// the operation stays idempotent.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const (
	wbMigrateTokensJSON = `{"type":"workbuddy","auth":{"accessToken":"tok-M","refreshToken":"ref-M","expiresAt":42},"account":{"uid":"777","nickname":"m"},"note":"old"}`
	wbMigrateLegacyName = "workbuddy.json"
	wbMigrateCanonName  = "workbuddy-777.json"
)

// stubMigratePersist installs a hostAuthPersistFn recorder for one migrate run
// and returns the recorded calls.
func stubMigratePersist(t *testing.T, saveErr error) *[]struct {
	name string
	raw  []byte
} {
	t.Helper()
	prev := hostAuthPersistFn
	calls := &[]struct {
		name string
		raw  []byte
	}{}
	hostAuthPersistFn = func(name, _ string, raw []byte) error {
		*calls = append(*calls, struct {
			name string
			raw  []byte
		}{name: name, raw: append([]byte(nil), raw...)})
		return saveErr
	}
	t.Cleanup(func() { hostAuthPersistFn = prev })
	return calls
}

func writeLegacyFile(t *testing.T) (dir, legacyPath string) {
	t.Helper()
	dir = t.TempDir()
	legacyPath = filepath.Join(dir, wbMigrateLegacyName)
	if err := os.WriteFile(legacyPath, []byte(wbMigrateTokensJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, legacyPath
}

func assertLegacyIntact(t *testing.T, legacyPath string) {
	t.Helper()
	got, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("legacy credential deleted or unreadable: %v", err)
	}
	if string(got) != wbMigrateTokensJSON {
		t.Fatalf("legacy credential rewritten:\n got %s\nwant %s", got, wbMigrateTokensJSON)
	}
}

// buildMigrateRaw mimics the lifecycle callers: the canonical document is
// rebuilt from the parsed credential. Token bytes must survive verbatim.
func buildMigrateRaw(t *testing.T, note string) []byte {
	t.Helper()
	sa, err := parseStored([]byte(wbMigrateTokensJSON))
	if err != nil || sa == nil {
		t.Fatalf("parseStored: %v", err)
	}
	raw, err := buildAuthFileJSON(sa, true, note, nil)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertCanonicalTokens(t *testing.T, raw []byte) {
	t.Helper()
	var got struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("canonical doc unparsable: %v", err)
	}
	if got.Auth.AccessToken != "tok-M" || got.Auth.RefreshToken != "ref-M" {
		t.Fatalf("canonical tokens altered: %+v", got.Auth)
	}
}

// TestHostAuthPersistMigrate_NonDestructive: canonical write happens, legacy
// file survives byte-identically, tokens unchanged.
func TestHostAuthPersistMigrate_NonDestructive(t *testing.T) {
	_, legacyPath := writeLegacyFile(t)
	calls := stubMigratePersist(t, nil)
	raw := buildMigrateRaw(t, "disabled · exhausted")

	if err := hostAuthPersistMigrate(wbMigrateCanonName, legacyPath, legacyPath, raw); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].name != wbMigrateCanonName {
		t.Fatalf("canonical save = %+v, want one save of %s", *calls, wbMigrateCanonName)
	}
	assertCanonicalTokens(t, (*calls)[0].raw)
	assertLegacyIntact(t, legacyPath)
}

// TestHostAuthPersistMigrate_Idempotent: a second run (reconcile repeats on
// every tick) must not delete the legacy file and must produce the same
// canonical document.
func TestHostAuthPersistMigrate_Idempotent(t *testing.T) {
	_, legacyPath := writeLegacyFile(t)
	calls := stubMigratePersist(t, nil)
	raw := buildMigrateRaw(t, "disabled · exhausted")

	for i := 0; i < 3; i++ {
		if err := hostAuthPersistMigrate(wbMigrateCanonName, legacyPath, legacyPath, raw); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		assertLegacyIntact(t, legacyPath)
	}
	if len(*calls) != 3 {
		t.Fatalf("saves = %d, want 3 (idempotent rewrite of the same canonical doc)", len(*calls))
	}
	first := string((*calls)[0].raw)
	for i, c := range *calls {
		if string(c.raw) != first {
			t.Fatalf("run %d produced a different canonical document", i)
		}
	}
}

// TestHostAuthPersistMigrate_SaveErrorKeepsLegacy: a failed canonical save
// must not remove or touch the legacy file either.
func TestHostAuthPersistMigrate_SaveErrorKeepsLegacy(t *testing.T) {
	_, legacyPath := writeLegacyFile(t)
	stubMigratePersist(t, fmt.Errorf("host down"))
	raw := buildMigrateRaw(t, "note")

	if err := hostAuthPersistMigrate(wbMigrateCanonName, legacyPath, legacyPath, raw); err == nil {
		t.Fatal("save failure must surface")
	}
	assertLegacyIntact(t, legacyPath)
}

// TestResolveAuthFileTarget_LegacyStillReportsCanonicalGuard: the resolver may
// still hand callers the canonical name plus the legacy path; the migrate
// contract (not deletion) is what changed.
func TestResolveAuthFileTarget_LegacyStillReportsCanonicalGuard(t *testing.T) {
	sa, err := parseStored([]byte(wbMigrateTokensJSON))
	if err != nil || sa == nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(t.TempDir(), wbMigrateLegacyName)
	name, path, legacyPath := resolveAuthFileTarget(sa, &hostAuthPhysical{Name: wbMigrateLegacyName, Path: legacy})
	if name != wbMigrateCanonName {
		t.Fatalf("name = %q, want %q", name, wbMigrateCanonName)
	}
	if path != legacy {
		t.Fatalf("path = %q, want the legacy physical path %q", path, legacy)
	}
	if legacyPath != legacy {
		t.Fatalf("legacyPath = %q, want %q", legacyPath, legacy)
	}
}
