package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"
)

// grant is what one module may do through bearing_call: ADR 9's manifest
// scope narrowed by a Source. Anything not granted is a permission error.
type grant struct {
	HTTPHosts   []string // allowed host:port values
	HTTPMethods []string
	// TokenEnv names the environment variable whose value the host injects
	// as a bearer token on granted http requests. The guest never sees it.
	TokenEnv string
	Clock    bool
	Log      bool
}

// host serves bearing_call for one module and counts what crosses it.
type host struct {
	grant  grant
	client *http.Client
	now    func() time.Time
	getenv func(string) string
	log    *slog.Logger

	calls, bytesIn, bytesOut atomic.Int64
	denied                   atomic.Int64
	// hostNanos is time spent inside bearing_call, including the Extism
	// memory reads and writes and the capability itself.
	hostNanos atomic.Int64
	// capNanos is time spent in the capability alone (dispatch).
	capNanos atomic.Int64
	lastAuth atomic.Value // Authorization header the guest sent on its last http request
}

// httpRequest mirrors guest/capability's request message.
type httpRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// encodeHTTPResponse frames a response: status and header length (4 bytes
// each, big-endian), headers as JSON, then the raw body.
func encodeHTTPResponse(status int, headers http.Header, body []byte) []byte {
	hdr, _ := json.Marshal(headers)
	out := make([]byte, 8, 8+len(hdr)+len(body))
	binary.BigEndian.PutUint32(out, uint32(status))
	binary.BigEndian.PutUint32(out[4:], uint32(len(hdr)))
	return append(append(out, hdr...), body...)
}

var errDenied = errors.New("permission denied")

func (h *host) dispatch(ctx context.Context, capability, method string, req []byte) ([]byte, error) {
	switch capability + "." + method {
	case "http.request":
		return h.http(ctx, req)
	case "clock.now":
		if !h.grant.Clock {
			return nil, fmt.Errorf("%w: clock", errDenied)
		}
		return []byte(h.now().UTC().Format(time.RFC3339Nano)), nil
	case "log.write":
		if !h.grant.Log {
			return nil, fmt.Errorf("%w: log", errDenied)
		}
		h.log.InfoContext(ctx, "guest log", "record", string(bytes.TrimSpace(req)))
		return nil, nil
	}
	return nil, fmt.Errorf("%w: unknown capability %s.%s", errDenied, capability, method)
}

func (h *host) http(ctx context.Context, raw []byte) ([]byte, error) {
	var r httpRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("http: decode request: %w", err)
	}
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	if !slices.Contains(h.grant.HTTPHosts, u.Host) || !slices.Contains(h.grant.HTTPMethods, r.Method) {
		h.denied.Add(1)
		return nil, fmt.Errorf("%w: http %s %s is not granted", errDenied, r.Method, u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, bytes.NewReader(r.Body))
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	h.lastAuth.Store(http.Header(r.Headers).Get("Authorization"))
	for k, vs := range r.Headers {
		if http.CanonicalHeaderKey(k) == "Authorization" && h.grant.TokenEnv != "" {
			continue // the host owns credentials
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if h.grant.TokenEnv != "" {
		req.Header.Set("Authorization", "Bearer "+h.getenv(h.grant.TokenEnv))
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	return encodeHTTPResponse(resp.StatusCode, resp.Header, body), nil
}

// hostFunction is the single bearing_call import. Arguments and result are
// Extism memory offsets; the result is a status byte (0 ok, 1 error) and
// the payload or error message.
func (h *host) hostFunction() extism.HostFunction {
	f := extism.NewHostFunctionWithStack("bearing_call",
		func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
			h.calls.Add(1)
			start := time.Now()
			defer func() { h.hostNanos.Add(int64(time.Since(start))) }()
			capability, err1 := p.ReadString(stack[0])
			method, err2 := p.ReadString(stack[1])
			req, err3 := p.ReadBytes(stack[2])
			var out []byte
			if err := errors.Join(err1, err2, err3); err != nil {
				out = append([]byte{1}, err.Error()...)
			} else {
				cs := time.Now()
				resp, err := h.dispatch(ctx, capability, method, req)
				h.capNanos.Add(int64(time.Since(cs)))
				if err != nil {
					out = append([]byte{1}, err.Error()...)
				} else {
					out = append([]byte{0}, resp...)
				}
			}
			h.bytesIn.Add(int64(len(req)))
			h.bytesOut.Add(int64(len(out)))
			off, err := p.WriteBytes(out)
			if err != nil {
				panic(err)
			}
			stack[0] = off
		},
		[]extism.ValueType{extism.ValueTypePTR, extism.ValueTypePTR, extism.ValueTypePTR},
		[]extism.ValueType{extism.ValueTypePTR})
	f.SetNamespace("extism:host/user")
	return f
}

// Resource limits (C-ADAPTER-3), off unless -limits: limitPages caps each
// linear memory (64 KiB pages; 0 = wazero's 4 GiB default) and syncDeadline
// bounds each full sync. With limits on, the runtime closes a module when
// its context is done, so a guest stuck in a loop is interrupted.
// limitMode ("both", "mem" or "close") isolates the cost of each limit.
var (
	limitPages   uint32
	syncDeadline time.Duration
	limitMode    = "both"
)

func runtimeConfig(cache wazero.CompilationCache, pages uint32) wazero.RuntimeConfig {
	rc := wazero.NewRuntimeConfig().WithCompilationCache(cache)
	if pages > 0 && limitMode != "close" {
		rc = rc.WithMemoryLimitPages(pages)
	}
	if pages > 0 && limitMode != "mem" {
		rc = rc.WithCloseOnContextDone(true)
	}
	return rc
}

// withDeadline applies syncDeadline, if set, to ctx.
func withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if syncDeadline <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, syncDeadline)
}

// compile builds a CompiledPlugin for module with the given compilation cache.
func compile(ctx context.Context, module []byte, cache wazero.CompilationCache, h *host) (*extism.CompiledPlugin, error) {
	return compileLimited(ctx, module, cache, h, limitPages)
}

func compileLimited(ctx context.Context, module []byte, cache wazero.CompilationCache, h *host, pages uint32) (*extism.CompiledPlugin, error) {
	manifest := extism.Manifest{Wasm: []extism.Wasm{extism.WasmData{Data: module}}}
	cfg := extism.PluginConfig{
		EnableWasi:    true,
		RuntimeConfig: runtimeConfig(cache, pages),
	}
	return extism.NewCompiledPlugin(ctx, manifest, cfg, []extism.HostFunction{h.hostFunction()})
}

// call runs one export and turns an Extism error into a Go error.
func call(ctx context.Context, p *extism.Plugin, name string, in []byte) ([]byte, error) {
	rc, out, err := p.CallWithContext(ctx, name, in)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if rc != 0 {
		return nil, fmt.Errorf("%s: exit %d: %s", name, rc, p.GetError())
	}
	return out, nil
}

// memTracker is a wazero memory allocator that records the largest size
// each linear memory reaches. Wasm memories never shrink, so the final
// sizes are the peaks.
type memTracker struct {
	mu   sync.Mutex
	mems []*trackedMem
}

type trackedMem struct {
	buf  []byte
	peak uint64
}

func (t *memTracker) Allocate(capacity, _ uint64) experimental.LinearMemory {
	m := &trackedMem{buf: make([]byte, 0, capacity)}
	t.mu.Lock()
	t.mems = append(t.mems, m)
	t.mu.Unlock()
	return m
}

func (m *trackedMem) Reallocate(size uint64) []byte {
	if size > uint64(cap(m.buf)) {
		nb := make([]byte, size, size+size/4)
		copy(nb, m.buf)
		m.buf = nb
	} else {
		m.buf = m.buf[:size]
	}
	m.peak = max(m.peak, size)
	return m.buf
}

func (m *trackedMem) Free() {}

// peaks returns the largest memory's peak (the guest's heap; the Extism
// kernel's memory holding inputs and outputs is the other one) and the sum.
func (t *memTracker) peaks() (largest, total uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range t.mems {
		largest = max(largest, m.peak)
		total += m.peak
	}
	return largest, total
}
