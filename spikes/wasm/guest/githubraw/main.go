// Command githubraw is the lean GitHub adapter module on the raw wazero ABI
// (guest/rawabi) instead of Extism, to separate Extism's memory-copy cost
// from the cost of running in WASM at all.
package main

import (
	"context"
	"encoding/json"
	"net/http"

	"bearing.example/spikes/wasm/guest/capability"
	adapter "bearing.example/spikes/wasm/guest/lean/adapter"
	github "bearing.example/spikes/wasm/guest/lean/githubcopy"
	"bearing.example/spikes/wasm/guest/rawabi"
)

var gh = &github.Adapter{
	HTTP:   &http.Client{Transport: capability.Transport{}},
	Now:    capability.Now,
	Getenv: func(string) string { return "" },
}

func errJSON(err error) error {
	e, ok := err.(*adapter.Error)
	if !ok {
		e = &adapter.Error{Code: adapter.CodeInternal, Message: err.Error()}
	}
	b, _ := json.Marshal(e)
	return &jsonErr{string(b)}
}

type jsonErr struct{ s string }

func (e *jsonErr) Error() string { return e.s }

//go:wasmexport describe
func describe(p, n uint32) uint64 {
	return rawabi.Serve(p, n, func([]byte) ([]byte, error) {
		res, err := gh.Describe(context.Background())
		if err != nil {
			return nil, errJSON(err)
		}
		return json.Marshal(res)
	})
}

//go:wasmexport sync
func sync(p, n uint32) uint64 {
	return rawabi.Serve(p, n, func(in []byte) ([]byte, error) {
		var sp adapter.SyncParams
		if err := json.Unmarshal(in, &sp); err != nil {
			return nil, errJSON(&adapter.Error{Code: adapter.CodeInvalidParams, Message: err.Error()})
		}
		res, err := gh.Sync(context.Background(), sp)
		if err != nil {
			return nil, errJSON(err)
		}
		return json.Marshal(res)
	})
}

func main() {}
