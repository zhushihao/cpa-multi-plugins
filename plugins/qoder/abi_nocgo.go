//go:build !cgo

// abi_nocgo.go supplies the pure-Go stubs used when this package is compiled
// without cgo (go test with CGO_ENABLED=0, i.e. on hosts without a MinGW C
// toolchain). The real implementations live in abi_cgo.go. Host bridge calls
// answer "unavailable" so callers fall back to direct HTTP — the same path
// unit tests use on Windows, see hostHTTPDo.
package main

import "fmt"

// hostCall mimics the cgo host RPC entry point: without the C host table
// there is no host to call.
func hostCall(method string, request []byte) ([]byte, error) {
	return nil, fmt.Errorf("host ABI unavailable (built without cgo)")
}

// hostAPIBridgeReady reports the host bridge is never ready without cgo.
func hostAPIBridgeReady() bool { return false }
