package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

// Client talks to one adapter over the protocol. It is safe for concurrent
// use; calls are serialized because the protocol handles one request at a time.
type Client struct {
	name   string
	mu     sync.Mutex
	enc    *json.Encoder
	sc     *bufio.Scanner
	nextID int
	close  func() error
}

// NewClient speaks the protocol over an existing reader and writer, such as
// in-memory pipes in tests. name labels the adapter in telemetry.
func NewClient(name string, r io.Reader, w io.Writer) *Client {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	return &Client{name: name, enc: json.NewEncoder(w), sc: sc, close: func() error { return nil }}
}

// Start launches an adapter executable and connects to its stdin and stdout.
// The adapter's stderr, where its console telemetry goes, is passed through
// to this process's stderr. The adapter inherits this process's environment,
// including OTEL_* settings.
func Start(ctx context.Context, name string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start adapter %s: %w", name, err)
	}
	telemetry.Logger(pkgName).InfoContext(ctx, "adapter started",
		string(attrAdapter), filepath.Base(name), "pid", cmd.Process.Pid)
	c := NewClient(filepath.Base(name), stdout, stdin)
	c.close = func() error {
		stdin.Close()
		return cmd.Wait()
	}
	return c, nil
}

// Name is the adapter's label in telemetry.
func (c *Client) Name() string { return c.name }

// Close ends the session and, for started adapters, waits for the process.
func (c *Client) Close() error { return c.close() }

func (c *Client) call(ctx context.Context, method string, params, result any) (err error) {
	ctx, span := tracer.Start(ctx, method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.RPCSystemNameJSONRPC, semconv.RPCMethod(method), attrAdapter.String(c.name)))
	start := time.Now()
	defer func() {
		attrs := []attribute.KeyValue{semconv.RPCMethod(method), attrAdapter.String(c.name)}
		var rpcErr *Error
		switch {
		case err == nil:
		case errors.As(err, &rpcErr) && rpcErr.Code == CodeNotSupported:
			attrs = append(attrs, semconv.ErrorTypeKey.String(rpcErr.ErrorType()))
			span.SetAttributes(semconv.RPCResponseStatusCode(rpcErr.ErrorType()))
		default:
			attrs = append(attrs, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		rpcClientDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		span.End()
	}()

	meta := map[string]string{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(meta))

	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := json.RawMessage(strconv.Itoa(c.nextID))
	span.SetAttributes(attribute.String("jsonrpc.request.id", string(id)))
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	if err := c.enc.Encode(request{JSONRPC: "2.0", ID: id, Method: method, Params: raw, Meta: meta}); err != nil {
		return fmt.Errorf("%s: write: %w", method, err)
	}
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return fmt.Errorf("%s: read: %w", method, err)
		}
		return fmt.Errorf("%s: adapter closed the connection", method)
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	if string(resp.ID) != string(id) {
		return fmt.Errorf("%s: response id %s does not match request id %s", method, resp.ID, id)
	}
	if resp.Error != nil {
		return resp.Error
	}
	return json.Unmarshal(resp.Result, result)
}

// Describe calls bearing.describe.
func (c *Client) Describe(ctx context.Context) (DescribeResult, error) {
	var res DescribeResult
	err := c.call(ctx, MethodDescribe, nil, &res)
	return res, err
}

// Sync calls bearing.sync for one page.
func (c *Client) Sync(ctx context.Context, p SyncParams) (SyncResult, error) {
	var res SyncResult
	err := c.call(ctx, MethodSync, p, &res)
	return res, err
}

// Handle calls bearing.handle.
func (c *Client) Handle(ctx context.Context, p HandleParams) (HandleResult, error) {
	var res HandleResult
	err := c.call(ctx, MethodHandle, p, &res)
	return res, err
}

// ErrTooManyPages is returned by SyncAll when an adapter never reports done.
var ErrTooManyPages = errors.New("adapter returned too many pages without finishing")

// Syncer is anything that serves sync pages: a Client, or an Adapter called
// in-process.
type Syncer interface {
	Sync(context.Context, SyncParams) (SyncResult, error)
}

// SyncAll pages through a full sync, validating every observation and passing
// it to emit. maxPages guards against adapters that never finish. The whole
// sync runs in one span, with each page as a child.
func SyncAll(ctx context.Context, a Syncer, config json.RawMessage, maxPages int, emit func(model.Observation) error) (err error) {
	name := "in-process"
	if n, ok := a.(interface{ Name() string }); ok {
		name = n.Name()
	}
	log := telemetry.Logger(pkgName)
	ctx, span := tracer.Start(ctx, "bearing.adapter.sync", trace.WithAttributes(attrAdapter.String(name)))
	start := time.Now()
	pages, accepted := 0, 0
	defer func() {
		span.SetAttributes(attrPages.Int(pages), attrCount.Int(accepted))
		attrs := []attribute.KeyValue{attrAdapter.String(name)}
		if err != nil {
			attrs = append(attrs, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
			telemetry.Fail(ctx, span, log, "adapter sync failed", err, attrAdapter.String(name), attrPages.Int(pages))
		} else {
			log.InfoContext(ctx, "adapter sync finished", string(attrAdapter), name,
				string(attrPages), pages, string(attrCount), accepted, "duration", time.Since(start).String())
		}
		syncDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		span.End()
	}()

	cursor := ""
	for pages < maxPages {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := a.Sync(ctx, SyncParams{Config: config, Cursor: cursor})
		if err != nil {
			return fmt.Errorf("sync page %d: %w", pages+1, err)
		}
		pages++
		syncPages.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(name)))
		for _, o := range res.Observations {
			if err := o.Validate(); err != nil {
				observationsInvalid.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(name)))
				return fmt.Errorf("sync page %d: %w", pages, err)
			}
			observationsReceived.Add(ctx, 1, metric.WithAttributes(attrAdapter.String(name), attrKind.String(string(o.Data.Entity.Kind))))
			accepted++
			if err := emit(o); err != nil {
				return err
			}
		}
		if res.Done {
			return nil
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			return fmt.Errorf("sync page %d: adapter is not done but returned no new cursor", pages)
		}
		cursor = res.NextCursor
	}
	return ErrTooManyPages
}
