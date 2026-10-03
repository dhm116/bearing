// Package bearingcall is the Extism ABI for the spike's single host
// function, bearing_call(capability, method, request) -> response. Linking
// it sets capability.Call.
//
// Arguments and the result are Extism memory offsets. The response is one
// status byte (0 ok, 1 error) followed by the payload or the error message.
package bearingcall

import (
	"errors"
	"fmt"

	"github.com/extism/go-pdk"

	"bearing.example/spikes/wasm/guest/capability"
)

func init() { capability.Call = Call }

// bearingCall takes three Extism memory offsets (capability, method,
// request) and returns the offset of the response.
//
//go:wasmimport extism:host/user bearing_call
func bearingCall(capability, method, request uint64) uint64

// Call invokes one capability method on the host.
func Call(capability, method string, req []byte) ([]byte, error) {
	c := pdk.AllocateString(capability)
	m := pdk.AllocateString(method)
	r := pdk.AllocateBytes(req)
	defer c.Free()
	defer m.Free()
	defer r.Free()
	off := bearingCall(c.Offset(), m.Offset(), r.Offset())
	if off == 0 {
		return nil, errors.New("bearing_call: no response")
	}
	mem := pdk.FindMemory(off)
	out := mem.ReadBytes()
	mem.Free()
	if len(out) == 0 {
		return nil, errors.New("bearing_call: empty response")
	}
	if out[0] != 0 {
		return nil, fmt.Errorf("bearing_call %s.%s: %s", capability, method, out[1:])
	}
	return out[1:], nil
}
