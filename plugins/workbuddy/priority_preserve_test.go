// priority_preserve_test.go — round6 item 3 (red → green).
//
// The plugin rewrites every managed credential through its own canonical key
// set (buildAuthFileJSON / import payload) and dropped the top-level
// "priority" written by the CPA panel, CPA-Manager-Plus or a hand edit. The
// host's plugin-auth branch does NOT read the file's top-level priority into
// the routing record (only the non-plugin synthesizer path does), so once the
// key disappeared the credential silently fell back to the default tier
// (shawn-primary/anyu-fallback reverted to balanced pooling).
//
// Two fixes are pinned here:
//  1. the save funnel carries an existing on-disk priority forward on every
//     rewrite (read before overwrite);
//  2. AuthParse surfaces the file's priority into AuthData.Attributes so the
//     host record gets the routing tier (authPriority reads Attributes).
package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const wbPriorityTestUID = "442e36a5-858a-408d-ba25-9c906c7cad28"

// wbPriorityPhysicalDoc is the on-disk fixture shape (fixture JSON only, no
// real credential bytes).
func wbPriorityPhysicalDoc() []byte {
	return []byte(`{"type":"workbuddy","provider":"workbuddy","auth_kind":"oauth","disabled":false,"note":"CN · test","priority":10,"auth":{"accessToken":"tok-fixture","refreshToken":"ref-fixture","domain":"copilot.tencent.com","expiresAt":9999999999},"account":{"uid":"` + wbPriorityTestUID + `","nickname":"fixture"}}`)
}

// TestSaveFunnelPreservesTopLevelPriority is the red test: a typed rebuild
// (the lifecycle note/credits path) drops priority; the save funnel must carry
// the on-disk value forward so the file still contains priority 10 after the
// plugin's save path runs.
func TestSaveFunnelPreservesTopLevelPriority(t *testing.T) {
	phys := wbPriorityPhysicalDoc()
	restore := stubPersistSeams(t, map[string][]byte{"live": phys})
	defer restore()

	sa, err := parseStored(phys)
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	// The real rebuild used by lifecycle/credits writes: fixed key set, no
	// priority (this is the production loss path).
	rebuilt, err := buildAuthFileJSON(sa, false, "CN · test", nil)
	if err != nil {
		t.Fatalf("buildAuthFileJSON: %v", err)
	}
	var rebuiltKeys map[string]json.RawMessage
	_ = json.Unmarshal(rebuilt, &rebuiltKeys)
	if _, has := rebuiltKeys["priority"]; has {
		t.Fatal("precondition: the typed rebuild must not carry priority by itself")
	}

	merged := preservePluginDocKeys("workbuddy-live.json", rebuilt)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatalf("merged doc unreadable: %v", err)
	}
	if got := string(m["priority"]); got != "10" {
		t.Fatalf("priority lost across the plugin save path: priority=%s (doc=%s)", got, merged)
	}
	// The rebuild's own fields still win.
	if got := string(m["note"]); got != `"CN · test"` {
		t.Fatalf("rebuild note must win, got %s", got)
	}

	// A document that already carries model_cache (the old early-return path)
	// must still receive the missing priority.
	withCache := []byte(`{"type":"workbuddy","provider":"workbuddy","model_cache":{"realm":"cn","fetched_at":"2026-09-30T00:00:00Z","models":[]},"auth":{"accessToken":"tok-fixture","region":"cn"},"account":{"uid":"` + wbPriorityTestUID + `"}}`)
	merged2 := preservePluginDocKeys("workbuddy-live.json", withCache)
	var m2 map[string]json.RawMessage
	if err := json.Unmarshal(merged2, &m2); err != nil {
		t.Fatalf("merged2 unreadable: %v", err)
	}
	if got := string(m2["priority"]); got != "10" {
		t.Fatalf("priority must be restored even when model_cache is already present, got %s", got)
	}

	// Caller-supplied priority wins over the on-disk copy.
	withOwn := []byte(`{"type":"workbuddy","provider":"workbuddy","priority":3,"auth":{"accessToken":"tok-fixture","region":"cn"},"account":{"uid":"` + wbPriorityTestUID + `"}}`)
	merged3 := preservePluginDocKeys("workbuddy-live.json", withOwn)
	var m3 map[string]json.RawMessage
	if err := json.Unmarshal(merged3, &m3); err != nil {
		t.Fatalf("merged3 unreadable: %v", err)
	}
	if got := string(m3["priority"]); got != "3" {
		t.Fatalf("caller-supplied priority must win, got %s", got)
	}

	// No physical source → nothing invented.
	bare := preservePluginDocKeys("workbuddy-brand-new.json", rebuilt)
	var b map[string]json.RawMessage
	if err := json.Unmarshal(bare, &b); err != nil {
		t.Fatalf("bare doc unreadable: %v", err)
	}
	if _, has := b["priority"]; has {
		t.Fatal("priority must not be invented without a physical source")
	}
}

// TestHandleParseAuthCarriesTopLevelPriority: the host rebuilds the in-memory
// auth record from this parse result, and its routing tier comes from
// Attributes — the file's priority must be surfaced here or a restart loses
// the tier even when the file still has it.
func TestHandleParseAuthCarriesTopLevelPriority(t *testing.T) {
	path := "/auths/workbuddy-" + wbPriorityTestUID + ".json"
	fileName := "workbuddy-" + wbPriorityTestUID + ".json"

	body, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: providerName,
		Path:     path,
		FileName: fileName,
		RawJSON:  wbPriorityPhysicalDoc(),
	})
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatalf("handleParseAuth: %v", err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Handled {
		t.Fatal("fixture must be handled")
	}
	if got := resp.Auth.Attributes["priority"]; got != "10" {
		t.Fatalf("file priority 10 not surfaced to the host record: attributes=%v", resp.Auth.Attributes)
	}
	// The value must ALSO ride in Metadata: the host's own auth-save
	// serializes Metadata keys back into the file, so without it every
	// host-memory-driven re-save strips the tier again (the live clobber).
	if got, ok := resp.Auth.Metadata["priority"]; !ok || fmt.Sprint(got) != "10" {
		t.Fatalf("file priority 10 not mirrored into Metadata: metadata=%v", resp.Auth.Metadata)
	}

	// A file without priority must not gain a priority attribute.
	noPriority := []byte(`{"type":"workbuddy","provider":"workbuddy","auth":{"accessToken":"tok-fixture","domain":"copilot.tencent.com"},"account":{"uid":"` + wbPriorityTestUID + `","nickname":"fixture"}}`)
	body2, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: providerName,
		Path:     path,
		FileName: fileName,
		RawJSON:  noPriority,
	})
	out2, err := handleParseAuth(body2)
	if err != nil {
		t.Fatalf("handleParseAuth(no priority): %v", err)
	}
	resp2 := decodeParseAuth(t, out2)
	if _, has := resp2.Auth.Attributes["priority"]; has {
		t.Fatalf("priority attribute invented without a file value: %v", resp2.Auth.Attributes)
	}
	if _, has := resp2.Auth.Metadata["priority"]; has {
		t.Fatalf("priority metadata invented without a file value: %v", resp2.Auth.Metadata)
	}
}

// TestImportSurfacePriority covers the management-surface hook: an explicit
// priority in the imported payload is persisted, and a re-import of an
// existing credential (payload without priority) keeps the on-disk value.
func TestImportSurfacePriority(t *testing.T) {
	sa, err := parseStored(wbPriorityPhysicalDoc())
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}

	// Payload carries priority (number and numeric string both accepted).
	for _, payload := range []string{
		`{"type":"workbuddy","priority":10,"auth":{"accessToken":"tok-fixture","region":"cn"},"account":{"uid":"` + wbPriorityTestUID + `"}}`,
		`{"type":"workbuddy","priority":"10","auth":{"accessToken":"tok-fixture","region":"cn"},"account":{"uid":"` + wbPriorityTestUID + `"}}`,
	} {
		doc, err := buildImportAuthFileJSON(sa, []byte(payload))
		if err != nil {
			t.Fatalf("buildImportAuthFileJSON: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(doc, &m); err != nil {
			t.Fatalf("doc unreadable: %v", err)
		}
		if got := string(m["priority"]); got != "10" {
			t.Fatalf("import payload priority not persisted: priority=%s", got)
		}
	}

	// Payload without priority → no priority invented, and a re-import keeps
	// the on-disk value via the same preserve step handleImportAuth runs.
	payload := []byte(`{"type":"workbuddy","auth":{"accessToken":"tok-fixture","region":"cn"},"account":{"uid":"` + wbPriorityTestUID + `"}}`)
	doc, err := buildImportAuthFileJSON(sa, payload)
	if err != nil {
		t.Fatalf("buildImportAuthFileJSON: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatalf("doc unreadable: %v", err)
	}
	if _, has := m["priority"]; has {
		t.Fatal("import must not invent a priority")
	}
	restore := stubPersistSeams(t, map[string][]byte{"live": wbPriorityPhysicalDoc()})
	defer restore()
	merged := preservePluginDocKeys("workbuddy-live.json", doc)
	var mm map[string]json.RawMessage
	if err := json.Unmarshal(merged, &mm); err != nil {
		t.Fatalf("merged unreadable: %v", err)
	}
	if got := string(mm["priority"]); got != "10" {
		t.Fatalf("re-import must keep the on-disk priority, got %s", got)
	}
}

// TestPriorityFromJSON rejects non-numeric shapes instead of guessing.
func TestPriorityFromJSON(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
		ok   bool
	}{
		{`{"priority":10}`, 10, true},
		{`{"priority":"10"}`, 10, true},
		{`{"priority":" 10 "}`, 10, true},
		{`{"priority":0}`, 0, true},
		{`{"priority":null}`, 0, false},
		{`{"priority":"x"}`, 0, false},
		{`{"priority":{}}`, 0, false},
		{`{}`, 0, false},
		{``, 0, false},
	} {
		got, ok := priorityFromJSON([]byte(tc.raw))
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("priorityFromJSON(%q) = (%d, %v), want (%d, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}
