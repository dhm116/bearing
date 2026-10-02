package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

// The raw ABI (see guest/rawabi): bearing_call on plain wazero, reading and
// writing the guest's linear memory directly. It exists to separate the cost
// of Extism's memory model from the cost of WASM itself.

type rawPending struct{ resp []byte }

type rawCtxKey struct{}

// rawRuntime is a compiled raw-ABI module plus the host functions.
type rawRuntime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
}

func compileRaw(ctx context.Context, module []byte, cache wazero.CompilationCache, h *host) (*rawRuntime, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	_, err := rt.NewHostModuleBuilder("bearing").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
		h.calls.Add(1)
		start := time.Now()
		defer func() { h.hostNanos.Add(int64(time.Since(start))) }()
		pend := ctx.Value(rawCtxKey{}).(*rawPending)
		mem := m.Memory()
		read := func(p, n uint64) []byte {
			b, _ := mem.Read(uint32(p), uint32(n))
			return b
		}
		capability, method, req := string(read(stack[0], stack[1])), string(read(stack[2], stack[3])), read(stack[4], stack[5])
		cs := time.Now()
		resp, err := h.dispatch(ctx, capability, method, req)
		h.capNanos.Add(int64(time.Since(cs)))
		if err != nil {
			pend.resp = append([]byte{1}, err.Error()...)
		} else {
			pend.resp = append([]byte{0}, resp...)
		}
		h.bytesIn.Add(int64(len(req)))
		h.bytesOut.Add(int64(len(pend.resp)))
		stack[0] = uint64(len(pend.resp))
	}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32}).Export("bearing_call").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
		pend := ctx.Value(rawCtxKey{}).(*rawPending)
		m.Memory().Write(uint32(stack[0]), pend.resp)
		pend.resp = nil
	}), []api.ValueType{api.ValueTypeI32}, nil).Export("bearing_read").
		Instantiate(ctx)
	if err != nil {
		return nil, err
	}
	compiled, err := rt.CompileModule(ctx, module)
	if err != nil {
		return nil, err
	}
	return &rawRuntime{rt: rt, compiled: compiled}, nil
}

func (r *rawRuntime) Close(ctx context.Context) error { return r.rt.Close(ctx) }

type rawInstance struct {
	mod  api.Module
	pend *rawPending
}

func (r *rawRuntime) instance(ctx context.Context) (*rawInstance, error) {
	mod, err := r.rt.InstantiateModule(ctx, r.compiled,
		wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize").WithSysWalltime().WithSysNanotime().
			WithStdout(os.Stderr).WithStderr(os.Stderr))
	if err != nil {
		return nil, err
	}
	return &rawInstance{mod: mod, pend: &rawPending{}}, nil
}

func (i *rawInstance) call(ctx context.Context, name string, in []byte) ([]byte, error) {
	ctx = context.WithValue(ctx, rawCtxKey{}, i.pend)
	res, err := i.mod.ExportedFunction("bearing_alloc").Call(ctx, uint64(len(in)))
	if err != nil {
		return nil, err
	}
	p := res[0]
	i.mod.Memory().Write(uint32(p), in)
	res, err = i.mod.ExportedFunction(name).Call(ctx, p, uint64(len(in)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	op, n := uint32(res[0]>>32), uint32(res[0])
	b, ok := i.mod.Memory().Read(op, n)
	if !ok || n == 0 {
		return nil, errors.New(name + ": bad result")
	}
	out := append([]byte(nil), b...)
	if _, err := i.mod.ExportedFunction("bearing_free").Call(ctx, uint64(op)); err != nil {
		return nil, err
	}
	if out[0] != 0 {
		return nil, fmt.Errorf("%s: %s", name, out[1:])
	}
	return out[1:], nil
}

func syncRaw(ctx context.Context, i *rawInstance, cfg json.RawMessage) ([]model.Observation, error) {
	var all []model.Observation
	cursor := ""
	for page := 1; page <= 100; page++ {
		in, _ := json.Marshal(adapter.SyncParams{Config: cfg, Cursor: cursor})
		out, err := i.call(ctx, "sync", in)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		var res adapter.SyncResult
		if err := json.Unmarshal(out, &res); err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		for _, o := range res.Observations {
			if err := o.Validate(); err != nil {
				return nil, fmt.Errorf("page %d: %w", page, err)
			}
		}
		all = append(all, res.Observations...)
		if res.Done {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return nil, adapter.ErrTooManyPages
}

// benchRaw mirrors benchModule for the raw ABI.
func benchRaw(ctx context.Context, module []byte, cfg json.RawMessage, h *host) (*moduleResult, error) {
	r := &moduleResult{}
	dir, err := os.MkdirTemp("", "wazero-cache-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	rr, err := compileRaw(ctx, module, cache, h)
	if err != nil {
		return nil, err
	}
	r.cold = time.Since(start)
	rr.Close(ctx)
	cache.Close(ctx)

	cache, err = wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, err
	}
	defer cache.Close(ctx)
	start = time.Now()
	rr, err = compileRaw(ctx, module, cache, h)
	if err != nil {
		return nil, err
	}
	r.warm = time.Since(start)
	defer rr.Close(ctx)

	var mt memTracker
	tctx := experimental.WithMemoryAllocator(ctx, &mt)
	start = time.Now()
	inst, err := rr.instance(tctx)
	if err != nil {
		return nil, err
	}
	if _, err := inst.call(tctx, "describe", nil); err != nil {
		return nil, err
	}
	r.first = time.Since(start)
	h.calls.Store(0)
	h.bytesIn.Store(0)
	h.bytesOut.Store(0)
	h.hostNanos.Store(0)
	h.capNanos.Store(0)
	if r.obs, err = syncRaw(tctx, inst, cfg); err != nil {
		return nil, err
	}
	r.memPeak, _ = mt.peaks()
	r.calls, r.bytesIn, r.bytesOut = h.calls.Load(), h.bytesIn.Load(), h.bytesOut.Load()
	r.hostTime, r.capTime = time.Duration(h.hostNanos.Load()), time.Duration(h.capNanos.Load())
	inst.mod.Close(ctx)

	for i := 0; i < *runs; i++ {
		start := time.Now()
		inst, err := rr.instance(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := syncRaw(ctx, inst, cfg); err != nil {
			return nil, err
		}
		r.syncs = append(r.syncs, time.Since(start))
		inst.mod.Close(ctx)
	}

	inst, err = rr.instance(ctx)
	if err != nil {
		return nil, err
	}
	_, err = syncRaw(ctx, inst, json.RawMessage(`{"org":"acme","api_url":"https://api.evil.example"}`))
	r.denied = containsDenied(err)
	inst.mod.Close(ctx)
	return r, nil
}
