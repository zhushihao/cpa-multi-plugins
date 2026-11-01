package main

// adopt_preserve_test.go — G07 credential-preservation tests. The design bars
// destructive adoption: the legacy credential directory may be shared with the
// still-running :3002 gateway, so adoption must rewrite a canonical copy
// WITHOUT deleting or altering the legacy file and WITHOUT touching token
// bytes. See adopt.go for the policy comment.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const wbLegacyAuthJSON = `{"type":"codebuddy-cn","auth":{"accessToken":"tok-A","refreshToken":"ref-A","expiresAt":123},"account":{"uid":"123","nickname":"n"},"note":"mine"}`

// stubAdoptSeams installs fake host RPC seams for one adopt run.
func stubAdoptSeams(t *testing.T, files []pluginapi.HostAuthFileEntry, phys *hostAuthPhysical) (savedName *string, savedRaw *[]byte) {
	t.Helper()
	prevList, prevGet, prevPersist := hostAuthListFn, hostAuthGetPhysicalFn, hostAuthPersistFn
	t.Cleanup(func() {
		hostAuthListFn, hostAuthGetPhysicalFn, hostAuthPersistFn = prevList, prevGet, prevPersist
	})
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) { return files, nil }
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) { return phys, nil }
	name := ""
	var raw []byte
	hostAuthPersistFn = func(n, _ string, r []byte) error {
		name = n
		raw = append([]byte(nil), r...)
		return nil
	}
	return &name, &raw
}

func TestAdoptForeignAuths_NonDestructive(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "codebuddy-cn-123.json")
	if err := os.WriteFile(legacyPath, []byte(wbLegacyAuthJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	savedName, savedRaw := stubAdoptSeams(t,
		[]pluginapi.HostAuthFileEntry{{ID: "123", AuthIndex: "idx-1", Name: "codebuddy-cn-123.json"}},
		&hostAuthPhysical{AuthIndex: "idx-1", Name: "codebuddy-cn-123.json", Path: legacyPath, JSON: []byte(wbLegacyAuthJSON)},
	)

	adoptForeignAuths()

	if *savedName != "workbuddy-123.json" {
		t.Fatalf("canonical name = %q, want workbuddy-123.json", *savedName)
	}
	// Token bytes must survive verbatim (no re-mint, no normalization).
	var got struct {
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"auth"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(*savedRaw, &got); err != nil {
		t.Fatalf("saved JSON unparsable: %v", err)
	}
	if got.Auth.AccessToken != "tok-A" || got.Auth.RefreshToken != "ref-A" {
		t.Fatalf("tokens altered: %+v", got.Auth)
	}
	// The source's own note is preserved (not overwritten by the default
	// "migrated from ..." note).
	if got.Note != "mine" {
		t.Fatalf("note = %q, want the source note preserved", got.Note)
	}
	// The legacy file must still exist, byte-identical (shared-dir safety).
	onDisk, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("legacy credential was destroyed: %v", err)
	}
	if string(onDisk) != wbLegacyAuthJSON {
		t.Fatalf("legacy credential was rewritten: %s", onDisk)
	}
}

func TestAdoptForeignAuths_DuplicateKeepsLegacyAndSkipsSave(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "codebuddy-cn-123.json")
	if err := os.WriteFile(legacyPath, []byte(wbLegacyAuthJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	savedName, _ := stubAdoptSeams(t,
		[]pluginapi.HostAuthFileEntry{
			{ID: "123", AuthIndex: "idx-1", Name: "codebuddy-cn-123.json"},
			{ID: "canon", AuthIndex: "idx-2", Name: "workbuddy-123.json"},
		},
		&hostAuthPhysical{AuthIndex: "idx-1", Name: "codebuddy-cn-123.json", Path: legacyPath, JSON: []byte(wbLegacyAuthJSON)},
	)

	adoptForeignAuths()

	if *savedName != "" {
		t.Fatalf("duplicate must not re-save, got %q", *savedName)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("duplicate cleanup destroyed the legacy credential: %v", err)
	}
}

func TestAdoptForeignAuths_MissingUIDLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "codebuddy-cn-nouid.json")
	bad := `{"type":"codebuddy-cn","auth":{"accessToken":"tok-B"}}`
	if err := os.WriteFile(legacyPath, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	savedName, _ := stubAdoptSeams(t,
		[]pluginapi.HostAuthFileEntry{{ID: "?", AuthIndex: "idx-9", Name: "codebuddy-cn-nouid.json"}},
		&hostAuthPhysical{AuthIndex: "idx-9", Name: "codebuddy-cn-nouid.json", Path: legacyPath, JSON: []byte(bad)},
	)

	adoptForeignAuths()

	if *savedName != "" {
		t.Fatalf("unparsable/missing-uid file must be left in place, got save %q", *savedName)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("file must remain for manual re-import: %v", err)
	}
}
