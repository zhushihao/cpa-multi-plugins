// quota_provider_coldpath_test.go — round7e (red → green).
//
// The host's management quota.fetch carries NO storage_json
// (plugin_quota.go fetchQuotaForPlugin sends AuthIndex/AuthID/Metadata/
// Attributes only); when it does carry storage it may be a JSON-encoded
// STRING wrapping the object (live 2026-09-30 diagnostic build). The cold
// path must therefore (a) unwrap a string-wrapped storage, and (b) resolve
// the credential through the auth list when storage is absent — while the
// removed cold-debug branches stay removed: any failure lands in the plain
// quota_unavailable envelope, never a fabricated balance.
package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQuotaFetchUnwrapsStringEncodedStorage(t *testing.T) {
	quotaTestUpstreams(t)
	quotaTestNoHostAuth(t)

	fetchCalls := 0
	prevFetch := quotaLiveFetchFn
	quotaLiveFetchFn = func(sa *storedAuth) (*creditsSummary, error) {
		fetchCalls++
		return &creditsSummary{
			TotalRemain: 100, TotalUsed: 0, TotalSize: 100, PackCount: 1,
			FetchedAt: "2026-09-30T10:00:00Z",
			Packages:  []packageSummary{{Name: "测试包", Remain: 100, Used: 0, Size: 100}},
		}, nil
	}
	t.Cleanup(func() { quotaLiveFetchFn = prevFetch })

	objectForm := json.RawMessage(`{"auth":{"accessToken":"dt-cold-fixture","refreshToken":"drt-cold-fixture"},"account":{"uid":"cold-uid"}}`)
	stringForm, _ := json.Marshal(string(objectForm)) // JSON string wrapping the object

	resp, ok, code := quotaFetchViaWireStorage(t, "qoder-cold-string", stringForm)
	if !ok {
		t.Fatalf("string-wrapped storage must still serve quota; code=%q", code)
	}
	if fetchCalls != 1 {
		t.Fatalf("live fetch ran %d time(s), want exactly 1 — proves the wrapper was unwrapped and parsed", fetchCalls)
	}
	_ = resp
}

func TestQuotaFetchResolvesAuthListWhenStorageAbsent(t *testing.T) {
	quotaTestUpstreams(t)

	fetchCalls := 0
	prevFetch := quotaLiveFetchFn
	quotaLiveFetchFn = func(sa *storedAuth) (*creditsSummary, error) {
		fetchCalls++
		return &creditsSummary{
			TotalRemain: 77, TotalUsed: 0, TotalSize: 100, PackCount: 1,
			FetchedAt: "2026-09-30T10:00:00Z",
			Packages:  []packageSummary{{Name: "测试包", Remain: 77, Used: 0, Size: 100}},
		}, nil
	}
	t.Cleanup(func() { quotaLiveFetchFn = prevFetch })

	prevList := hostAuthListFn
	hostAuthListFn = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{
			AuthIndex: "idx-cold-1",
			ID:        "id-cold-1",
			Name:      "qoder-cn-019f99ec.json",
			Disabled:  false,
		}}, nil
	}
	t.Cleanup(func() { hostAuthListFn = prevList })

	prevPhys := hostAuthGetPhysicalFn
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		if authIndex != "idx-cold-1" {
			t.Fatalf("unexpected auth index lookup: %q", authIndex)
		}
		return &hostAuthPhysical{
			JSON: json.RawMessage(`{"auth":{"accessToken":"dt-cold-fixture","refreshToken":"drt-cold-fixture"},"account":{"uid":"cold-uid"}}`),
		}, nil
	}
	t.Cleanup(func() { hostAuthGetPhysicalFn = prevPhys })

	resp, ok, code := quotaFetchViaWireStorage(t, "idx-cold-1", nil)
	if !ok {
		t.Fatalf("storageless quota.fetch must resolve via the auth list; code=%q", code)
	}
	if fetchCalls != 1 {
		t.Fatalf("live fetch ran %d time(s), want exactly 1", fetchCalls)
	}
	if len(resp.Summary) == 0 {
		t.Fatalf("quota response missing summary metrics")
	}
}

func TestQuotaFetchStringStorageParseFailureStaysUnknown(t *testing.T) {
	quotaTestUpstreams(t)
	quotaTestNoHostAuth(t)

	fetchCalls := 0
	prevFetch := quotaLiveFetchFn
	quotaLiveFetchFn = func(sa *storedAuth) (*creditsSummary, error) {
		fetchCalls++
		return nil, nil
	}
	t.Cleanup(func() { quotaLiveFetchFn = prevFetch })

	garbage, _ := json.Marshal("not json at all {{{")
	_, ok, code := quotaFetchViaWireStorage(t, "qoder-cold-garbage", garbage)
	if ok {
		t.Fatalf("unparseable storage must not fabricate a balance")
	}
	if fetchCalls != 0 {
		t.Fatalf("live fetch ran on unparseable storage (%d call(s))", fetchCalls)
	}
	if code != "quota_unavailable" {
		t.Fatalf("code = %q, want quota_unavailable", code)
	}
}
