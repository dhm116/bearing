//go:build bearingplus

// Package main is a size probe: the harness copies cmd/bearing/main.go here
// and builds it with this file, which links the Extism/wazero host and adds a
// "wasm-describe" command, so the binary size difference is the cost of the
// host (and, with the bearingembed tag, of an embedded adapter module).
package main

import (
	"context"
	"fmt"
	"os"

	extism "github.com/extism/go-sdk"
)

// embedded is set by embed.go under the bearingembed tag.
var embedded []byte

func init() {
	if len(os.Args) < 2 || os.Args[1] != "wasm-describe" {
		return
	}
	module := embedded
	if len(os.Args) > 2 {
		b, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		module = b
	}
	ctx := context.Background()
	p, err := extism.NewPlugin(ctx, extism.Manifest{Wasm: []extism.Wasm{extism.WasmData{Data: module}}},
		extism.PluginConfig{EnableWasi: true}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_, out, err := p.Call("describe", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(append(out, '\n'))
	os.Exit(0)
}
