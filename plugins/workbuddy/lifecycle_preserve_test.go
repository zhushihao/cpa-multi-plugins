package main

// lifecycle_preserve_test.go — I6 credential-preservation tests for the
// exhausted-Global delete path (deleteAuth). The feature deletes the account's
// OWN canonical credential on purpose; every OLD credential file is preserved
// (round-2 review: preservation-first — the dual-named workbuddy.json is no
// longer auto-deleted, and a legacy codebuddy-cn/intl source is never a delete
// target).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	wbDeleteCanonJSON     = `{"type":"workbuddy","auth":{"accessToken":"tok-canon","refreshToken":"ref-canon"},"account":{"uid":"777","nickname":"c"}}`
	wbDeleteLegacySameUID = `{"type":"workbuddy","auth":{"accessToken":"tok-legacy","refreshToken":"ref-legacy"},"account":{"uid":"777","nickname":"c"}}`
	wbDeleteLegacyOther   = `{"type":"workbuddy","auth":{"accessToken":"tok-other","refreshToken":"ref-other"},"account":{"uid":"888","nickname":"other"}}`
	wbDeleteLegacyNoUID   = `{"type":"workbuddy","auth":{"accessToken":"tok-x","refreshToken":"ref-x"},"account":{"nickname":"n"}}`
)

func mustParseStored(t *testing.T, raw string) *storedAuth {
	t.Helper()
	sa, err := parseStored([]byte(raw))
	if err != nil || sa == nil {
		t.Fatalf("parseStored(%s): %v", raw, err)
	}
	return sa
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPreserveLegacyDualName_SameUIDKept: the dual-named copy of the SAME
// credential is preserved byte-for-byte (round-2 review: the old auto-delete
// contradicted the I6 non-destructive policy).
func TestPreserveLegacyDualName_SameUIDKept(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, authFileName)
	writeFile(t, legacy, wbDeleteLegacySameUID)

	preserveLegacyDualName(dir, mustParseStored(t, wbDeleteCanonJSON))

	got, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("same-UID legacy copy was deleted: %v", err)
	}
	if string(got) != wbDeleteLegacySameUID {
		t.Fatalf("same-UID legacy copy was rewritten")
	}
}

// TestRemoveLegacyDualName_OtherUIDPreserved: a legacy file belonging to a
// DIFFERENT account must never be deleted.
func TestRemoveLegacyDualName_OtherUIDPreserved(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, authFileName)
	writeFile(t, legacy, wbDeleteLegacyOther)

	preserveLegacyDualName(dir, mustParseStored(t, wbDeleteCanonJSON))

	got, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("legacy file of a different account was deleted: %v", err)
	}
	if string(got) != wbDeleteLegacyOther {
		t.Fatalf("legacy file was rewritten: %s", got)
	}
}

// TestRemoveLegacyDualName_UnknownUIDPreserved: unparsable or UID-less legacy
// content is preserved (never guess an identity to delete).
func TestRemoveLegacyDualName_UnknownUIDPreserved(t *testing.T) {
	for name, content := range map[string]string{
		"unparsable": "{not json",
		"no uid":     wbDeleteLegacyNoUID,
		"empty":      "",
	} {
		dir := t.TempDir()
		legacy := filepath.Join(dir, authFileName)
		writeFile(t, legacy, content)

		preserveLegacyDualName(dir, mustParseStored(t, wbDeleteCanonJSON))

		got, err := os.ReadFile(legacy)
		if err != nil {
			t.Fatalf("%s: legacy file deleted: %v", name, err)
		}
		if string(got) != content {
			t.Fatalf("%s: legacy file rewritten", name)
		}
	}
}

// TestRemoveLegacyDualName_NoLegacyFile: absent legacy file is a no-op, and
// sibling account files are never touched.
func TestRemoveLegacyDualName_NoLegacyFile(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "workbuddy-999.json")
	writeFile(t, sibling, wbDeleteLegacyOther)

	preserveLegacyDualName(dir, mustParseStored(t, wbDeleteCanonJSON))

	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling account file touched: %v", err)
	}
}

// TestDeleteAuth_PreservesUnrelatedLegacy: end-to-end through the exhausted-
// Global delete: the account's canonical file is removed, an unrelated legacy
// workbuddy.json survives byte-identically.
func TestDeleteAuth_PreservesUnrelatedLegacy(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "workbuddy-777.json")
	legacy := filepath.Join(dir, authFileName)
	writeFile(t, canonical, wbDeleteCanonJSON)
	writeFile(t, legacy, wbDeleteLegacyOther)

	prev := hostAuthGetPhysicalFn
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{
			Name: "workbuddy-777.json",
			Path: canonical,
			JSON: []byte(wbDeleteCanonJSON),
		}, nil
	}
	t.Cleanup(func() { hostAuthGetPhysicalFn = prev })

	if err := deleteAuth("idx-777", "aid-777", mustParseStored(t, wbDeleteCanonJSON)); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("canonical file should be deleted, stat err = %v", err)
	}
	got, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("unrelated legacy file deleted: %v", err)
	}
	if string(got) != wbDeleteLegacyOther {
		t.Fatalf("unrelated legacy file rewritten")
	}
}

// TestDeleteAuth_PreservesSameUIDLegacy: end-to-end, the canonical file is
// deleted while the dual-named legacy copy of the SAME account is preserved
// byte-for-byte (round-2 review).
func TestDeleteAuth_PreservesSameUIDLegacy(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "workbuddy-777.json")
	legacy := filepath.Join(dir, authFileName)
	writeFile(t, canonical, wbDeleteCanonJSON)
	writeFile(t, legacy, wbDeleteLegacySameUID)

	prev := hostAuthGetPhysicalFn
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{
			Name: "workbuddy-777.json",
			Path: canonical,
			JSON: []byte(wbDeleteCanonJSON),
		}, nil
	}
	t.Cleanup(func() { hostAuthGetPhysicalFn = prev })

	if err := deleteAuth("idx-777", "aid-777", mustParseStored(t, wbDeleteCanonJSON)); err != nil {
		t.Fatalf("deleteAuth: %v", err)
	}
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("canonical file should be deleted, stat err = %v", err)
	}
	got, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("dual-named legacy copy must be preserved: %v", err)
	}
	if string(got) != wbDeleteLegacySameUID {
		t.Fatalf("dual-named legacy copy was modified")
	}
}

// sanity: the fixtures parse and stay distinguishable.
func TestMigrateFixturesSanity(t *testing.T) {
	var a, b struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal([]byte(wbDeleteCanonJSON), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wbDeleteLegacyOther), &b); err != nil {
		t.Fatal(err)
	}
	if a.Auth.AccessToken == b.Auth.AccessToken {
		t.Fatal("fixtures must differ")
	}
}
