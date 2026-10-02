//go:build bearingplus && bearingembed

package main

import _ "embed"

//go:embed adapter.wasm
var embeddedModule []byte

func init() { embedded = embeddedModule }
