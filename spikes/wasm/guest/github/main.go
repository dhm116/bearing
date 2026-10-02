// Command github is the spike's GitHub adapter as an Extism WASM module. It
// imports bearing.example/adapters/github unchanged and swaps only its
// injected dependencies: HTTP goes through the host's "http" capability,
// time through "clock", and Getenv returns nothing because the host injects
// the token.
package main

import (
	"net/http"

	"bearing.example/adapters/github"
	_ "bearing.example/spikes/wasm/guest/bearingcall"
	"bearing.example/spikes/wasm/guest/capability"
	"bearing.example/spikes/wasm/guest/exports"
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
