// Package rawabi is the spike's alternative to the Extism ABI: bearing_call
// on plain wazero with direct access to the guest's linear memory, so a
// payload crosses the boundary in one copy instead of Extism's 8 bytes per
// host call. Linking it sets capability.Call.
//
// Host imports (module "bearing"):
//
//	bearing_call(cap_ptr, cap_len, method_ptr, method_len, req_ptr, req_len i32) -> i32
//	    runs the call and returns the response length; the host keeps the
//	    response (status byte + payload) until bearing_read.
//	bearing_read(dst_ptr i32)  copies the pending response into guest memory.
//
// Guest exports: bearing_alloc(len i32) -> ptr i32 and bearing_free(ptr i32)
// for the host to pass inputs, and each entry point takes (ptr, len) and
// returns ptr<<32 | len of a status-prefixed result the host frees.
package rawabi

import (
	"errors"
	"fmt"
	"unsafe"

	"bearing.example/spikes/wasm/guest/capability"
)

func init() { capability.Call = Call }

//go:wasmimport bearing bearing_call
func bearingCall(capPtr, capLen, methodPtr, methodLen, reqPtr, reqLen uint32) uint32

//go:wasmimport bearing bearing_read
func bearingRead(dst uint32)

// pinned keeps buffers handed to the host alive until it frees them.
var pinned = map[uint32][]byte{}

func ptr(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b))))
}

func pin(b []byte) uint32 {
	p := ptr(b)
	if p != 0 {
		pinned[p] = b
	}
	return p
}

//go:wasmexport bearing_alloc
func alloc(n uint32) uint32 { return pin(make([]byte, max(n, 1))) }

//go:wasmexport bearing_free
func free(p uint32) { delete(pinned, p) }

// Call invokes one capability method on the host.
func Call(capability, method string, req []byte) ([]byte, error) {
	c, m := []byte(capability), []byte(method)
	n := bearingCall(ptr(c), uint32(len(c)), ptr(m), uint32(len(m)), ptr(req), uint32(len(req)))
	if n == 0 {
		return nil, errors.New("bearing_call: empty response")
	}
	out := make([]byte, n)
	bearingRead(ptr(out))
	if out[0] != 0 {
		return nil, fmt.Errorf("bearing_call %s.%s: %s", capability, method, out[1:])
	}
	return out[1:], nil
}

// Serve runs an entry point on the input at (p, n), which the host
// allocated with bearing_alloc, and returns the packed result.
func Serve(p, n uint32, fn func([]byte) ([]byte, error)) uint64 {
	in := pinned[p][:n]
	delete(pinned, p)
	out, err := fn(in)
	var res []byte
	if err != nil {
		res = append([]byte{1}, err.Error()...)
	} else {
		res = append([]byte{0}, out...)
	}
	return uint64(pin(res))<<32 | uint64(len(res))
}
