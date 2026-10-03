//go:build protovalidate

package main

import (
	"buf.build/go/protovalidate"
	"github.com/extism/go-pdk"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// validate exists only to link protovalidate and cel-go into the module, to
// measure what guest-side validation would cost. Bearing validates on the
// host, so the real modules don't need it.
//
//go:wasmexport validate
func validate() int32 {
	v, err := protovalidate.New()
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	if err := v.Validate(wrapperspb.String(pdk.InputString())); err != nil {
		pdk.SetError(err)
		return 1
	}
	return 0
}
