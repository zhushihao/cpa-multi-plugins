//go:build !cgo

// hostcall_nocgo.go is the non-cgo build stub for the host RPC bridge. It only
// exists so `CGO_ENABLED=0 go test ./...` compiles the pure-Go logic (pool
// routing, session stickiness, travel schedule, adopt logic) without a C
// toolchain. Any code path that actually needs the host at runtime (auth list,
// streaming push, model discovery) fails fast with a clear error instead of
// silently pretending to work — a non-cgo build is a test artifact, never a
// deployable plugin (the DLL requires CGO).
package main

import "fmt"

func hostCall(method string, request []byte) ([]byte, error) {
	_ = request
	return nil, fmt.Errorf("host RPC %s unavailable: built without cgo (test-only build)", method)
}

// hostCallAvailable mirrors the cgo bridge check; always false without cgo so
// the pure-Go paths fall back exactly as they do when the host predates the
// bridge.
func hostCallAvailable() bool { return false }
