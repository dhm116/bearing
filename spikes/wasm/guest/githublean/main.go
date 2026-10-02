// Command githublean is the GitHub adapter module built from a generated copy
// of adapters/github (guest/lean/githubcopy, see the Makefile's lean target)
// that uses an API-only telemetry shim instead of pkg/telemetry. The adapter
// logic is byte-for-byte the same; only the telemetry import differs.
package main

import (
	"net/http"

	_ "bearing.example/spikes/wasm/guest/bearingcall"
	"bearing.example/spikes/wasm/guest/capability"
	exports "bearing.example/spikes/wasm/guest/lean/exportscopy"
	github "bearing.example/spikes/wasm/guest/lean/githubcopy"
)

var gh = &github.Adapter{
	HTTP:   &http.Client{Transport: capability.Transport{}},
	Now:    capability.Now,
	Getenv: func(string) string { return "" },
}

//go:wasmexport describe
func describe() int32 { return exports.Describe(gh) }

//go:wasmexport sync
func sync() int32 { return exports.Sync(gh) }

//go:wasmexport handle
func handle() int32 { return exports.Handle(gh) }

func main() {}
